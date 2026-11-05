package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Router je Bereich (lokal: das ganze Netz, Cloud: je Standort): Ein Gerät wird als Router
// gewählt, je Port gibt es Rolle (WAN, Backup WAN, Access, Aus), Typ und VLAN, dazu die
// Netzwerke. Ausrollen ersetzt die Router-Konfiguration ganz: Bridge-VLANs, Adressen,
// DHCP, NAT und Firewall. Es läuft mit Backup und Wächter wie die zentrale WLAN-Verwaltung;
// der Rückweg ist ein Backup auf dem Gerät. Erhalten bleibt der Tunnel zum Manager (wg-mm).
//
// Grundregeln: Netzwerke sind untereinander gesperrt und dürfen ins Internet, nur das
// Standard-LAN darf in die anderen. Von außen ist alles zu. Der Router selbst ist nur aus
// dem Standard-LAN und über den Tunnel verwaltbar.

const (
	routerComment  = "mm-router"
	routerState    = "mm-router-cfg" // Skript, dessen Text den Stand der Konfiguration trägt
	routerFailover = "mm-router-wan"
	routerCheckAll = 6 // Runden je 10 s, also 60 s bis zum Umschalten und zurück
	routerWanList  = "mm-wan"
	routerLanList  = "mm-lan"
	routerNetList  = "mm-net"
	routerTunnel   = "wg-mm"
)

var routerChecks = []string{"8.8.4.4", "1.0.0.1"} // bewusst keine DNS-Server der Clients

// RouterPort ist eine Zeile der Port-Liste. Secret (PPPoE-Passwort oder SIM-PIN) kommt nur
// von der Oberfläche und liegt danach im Tresor.
type RouterPort struct {
	Name      string `json:"name"`
	Role      string `json:"role"`           // wan | backup | access | off
	Type      string `json:"type,omitempty"` // ipoe | dhcp | pppoe | wwan
	VLAN      string `json:"vlan,omitempty"`
	Address   string `json:"address,omitempty"`
	Mask      string `json:"mask,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	DNS       string `json:"dns,omitempty"`
	User      string `json:"user,omitempty"`
	APN       string `json:"apn,omitempty"` // leer = automatisch
	Secret    string `json:"secret,omitempty"`
	SecretSet bool   `json:"secretSet,omitempty"`
}

// RouterNet ist ein Netzwerk; das erste ist das Standard-LAN mit VLAN 1.
type RouterNet struct {
	Address string `json:"address"` // Adresse des Routers als CIDR
	VLAN    int    `json:"vlan"`
	DHCP    bool   `json:"dhcp"`
	First   string `json:"first,omitempty"`
	Last    string `json:"last,omitempty"`
	DNS     string `json:"dns,omitempty"` // leer = Router
}

type RouterCfg struct {
	Device  string       `json:"device"`
	Ports   []RouterPort `json:"ports"`
	Nets    []RouterNet  `json:"nets"`
	Updated time.Time    `json:"updated,omitempty"`
}

var routerMu sync.Mutex

func (p *Poller) routerPath() string { return filepath.Join(p.cfg.DataDir, "router.json") }

func (p *Poller) routerAll() map[string]RouterCfg {
	m := map[string]RouterCfg{}
	if b, err := os.ReadFile(p.routerPath()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// Router liefert die Konfiguration eines Bereichs, ohne Geheimnisse.
func (p *Poller) Router(key string) RouterCfg {
	routerMu.Lock()
	defer routerMu.Unlock()
	c, ok := p.routerAll()[key]
	if !ok || len(c.Nets) == 0 {
		c.Nets = []RouterNet{{Address: "192.168.88.1/24", VLAN: 1}}
	}
	return c
}

// IsRouter: Ist das Gerät in seinem Bereich als Router gewählt?
func (p *Poller) IsRouter(d Device) bool {
	return d.ID != "" && p.Router(routerKeyOf(d)).Device == d.ID
}

func routerSecretKey(key, port string) string { return "router:" + key + ":" + port }

// SaveRouter prüft und speichert. Ein leeres Secret lässt das hinterlegte stehen.
func (p *Poller) SaveRouter(key string, c RouterCfg) (RouterCfg, error) {
	routerMu.Lock()
	defer routerMu.Unlock()
	for i := range c.Ports {
		x := &c.Ports[i]
		_, has := p.vault.Get(routerSecretKey(key, x.Name))
		x.SecretSet = has || x.Secret != ""
	}
	if err := validRouter(&c); err != nil {
		return c, err
	}
	for i := range c.Ports {
		x := &c.Ports[i]
		if x.Secret != "" {
			if err := p.vault.Set(routerSecretKey(key, x.Name), Cred{User: x.User, Password: x.Secret}); err != nil {
				return c, err
			}
			x.Secret = ""
		}
	}
	c.Updated = time.Now()
	m := p.routerAll()
	m[key] = c
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return c, err
	}
	return c, writeFileAtomic(p.routerPath(), b, 0o600)
}

// --- Prüfung ---

var vlanListRe = regexp.MustCompile(`^\d+(\s*,\s*\d+)*$`)

func parseVLANs(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !vlanListRe.MatchString(s) {
		return nil, fmt.Errorf("VLAN %q: Zahlen, getrennt durch Komma", s)
	}
	seen := map[int]bool{}
	var out []int
	for _, x := range strings.Split(s, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil || v < 1 || v > 4094 {
			return nil, fmt.Errorf("VLAN %q: 1 bis 4094", strings.TrimSpace(x))
		}
		if seen[v] {
			return nil, fmt.Errorf("VLAN %d doppelt", v)
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, nil
}

func validRouter(c *RouterCfg) error {
	if c.Device == "" {
		return fmt.Errorf("bitte ein Gerät als Router wählen")
	}
	var wan, backup, access int
	used := map[int]string{} // VLAN → Port
	wanV := map[int]string{}
	for i := range c.Ports {
		x := &c.Ports[i]
		vs, err := parseVLANs(x.VLAN)
		if err != nil {
			return fmt.Errorf("%s: %w", x.Name, err)
		}
		switch x.Role {
		case "off":
			x.Type, x.VLAN = "", ""
		case "access":
			access++
			x.Type = ""
			for _, v := range vs {
				used[v] = x.Name
			}
		case "wan", "backup":
			if x.Role == "wan" {
				wan++
			} else {
				backup++
			}
			if len(vs) > 1 {
				return fmt.Errorf("%s: Am WAN ist nur ein VLAN möglich", x.Name)
			}
			if len(vs) == 1 {
				if vs[0] == 1 {
					return fmt.Errorf("%s: VLAN 1 ist das Standard-LAN", x.Name)
				}
				if o, dup := wanV[vs[0]]; dup {
					return fmt.Errorf("VLAN %d liegt an %s und %s, ein WAN-VLAN gilt nur einmal", vs[0], o, x.Name)
				}
				wanV[vs[0]] = x.Name
			}
			switch x.Type {
			case "ipoe":
				r, err := parseIPReq(ChangeReq{IPAddress: x.Address, IPMask: x.Mask, IPGateway: x.Gateway, IPDNS: x.DNS})
				if err != nil {
					return fmt.Errorf("%s: %w", x.Name, err)
				}
				x.Address, x.Mask, x.Gateway, x.DNS = r.addr.Addr().String(), strconv.Itoa(r.addr.Bits()), r.gw.String(), strings.Join(r.dns, ",")
			case "dhcp":
			case "pppoe":
				if strings.TrimSpace(x.User) == "" || !x.SecretSet {
					return fmt.Errorf("%s: PPPoE braucht Benutzername und Passwort", x.Name)
				}
			case "wwan":
				if len(vs) > 0 {
					return fmt.Errorf("%s: WWAN kann kein VLAN", x.Name)
				}
				if x.Secret != "" {
					if err := validPIN(x.Secret); err != nil {
						return fmt.Errorf("%s: %w", x.Name, err)
					}
				}
			default:
				return fmt.Errorf("%s: Typ wählen", x.Name)
			}
		default:
			return fmt.Errorf("%s: Rolle wählen", x.Name)
		}
	}
	if wan != 1 {
		return fmt.Errorf("genau ein Port muss WAN sein")
	}
	if backup > 1 {
		return fmt.Errorf("höchstens ein Backup WAN")
	}
	if access == 0 {
		return fmt.Errorf("mindestens ein Port muss Access sein, sonst ist das LAN nicht erreichbar")
	}
	if len(c.Nets) == 0 {
		return fmt.Errorf("das Standard-LAN fehlt")
	}
	c.Nets[0].VLAN = 1
	var nets []netip.Prefix
	seenV := map[int]bool{}
	for i := range c.Nets {
		n := &c.Nets[i]
		pfx, err := netip.ParsePrefix(strings.TrimSpace(n.Address))
		if err != nil || !pfx.Addr().Is4() || pfx.Bits() < 8 || pfx.Bits() > 30 {
			return fmt.Errorf("Netzwerk %q: Adresse als CIDR angeben, z. B. 192.168.10.1/24", n.Address)
		}
		if pfx.Addr() == pfx.Masked().Addr() {
			return fmt.Errorf("Netzwerk %s: Das ist die Netzadresse, hier gehört die Adresse des Routers hin", pfx)
		}
		n.Address = pfx.String()
		if i > 0 && (n.VLAN < 2 || n.VLAN > 4094) {
			return fmt.Errorf("Netzwerk %s: VLAN 2 bis 4094", pfx)
		}
		if seenV[n.VLAN] {
			return fmt.Errorf("VLAN %d ist doppelt vergeben", n.VLAN)
		}
		seenV[n.VLAN] = true
		if o, bad := wanV[n.VLAN]; bad {
			return fmt.Errorf("VLAN %d liegt am WAN (%s) und darf nicht für ein Netzwerk benutzt werden", n.VLAN, o)
		}
		for _, o := range nets {
			if o.Overlaps(pfx) {
				return fmt.Errorf("Netzwerke %s und %s überschneiden sich", o, pfx)
			}
		}
		nets = append(nets, pfx)
		if n.DHCP {
			f, e1 := netip.ParseAddr(strings.TrimSpace(n.First))
			l, e2 := netip.ParseAddr(strings.TrimSpace(n.Last))
			if e1 != nil || e2 != nil || !pfx.Masked().Contains(f) || !pfx.Masked().Contains(l) || l.Less(f) {
				return fmt.Errorf("Netzwerk %s: DHCP-Bereich liegt nicht im Netz", pfx)
			}
			if !pfx.Addr().Less(f) && !l.Less(pfx.Addr()) {
				return fmt.Errorf("Netzwerk %s: Der DHCP-Bereich enthält die Adresse des Routers", pfx)
			}
			n.First, n.Last = f.String(), l.String()
			if d := strings.TrimSpace(n.DNS); d != "" {
				a, err := netip.ParseAddr(d)
				if err != nil || !a.Is4() {
					return fmt.Errorf("Netzwerk %s: DNS ungültig", pfx)
				}
				n.DNS = a.String()
			}
		} else {
			n.First, n.Last, n.DNS = "", "", ""
		}
	}
	for v, o := range wanV {
		if p, bad := used[v]; bad {
			return fmt.Errorf("VLAN %d liegt am WAN (%s) und darf nicht an %s benutzt werden", v, o, p)
		}
	}
	return nil
}

// --- Plan ---

// routerVLAN ist eine Zeile der Bridge-VLAN-Tabelle.
type routerVLAN struct {
	ID       int
	Tagged   []string
	Untagged []string
}

// routerVLANs: Port ohne Eintrag = VLAN 1 ungetaggt, alle anderen getaggt. Ein VLAN = nur dieses
// ungetaggt. Mehrere = nur diese getaggt (VLAN 1 darin läuft ungetaggt).
func routerVLANs(c RouterCfg) []routerVLAN {
	ids := map[int]bool{}
	routed := map[int]bool{}
	for _, n := range c.Nets {
		if n.VLAN > 1 {
			ids[n.VLAN], routed[n.VLAN] = true, true
		}
	}
	var open []string
	type one struct {
		name string
		vs   []int
	}
	var ports []one
	for _, x := range c.Ports {
		if x.Role != "access" {
			continue
		}
		vs, _ := parseVLANs(x.VLAN)
		if len(vs) == 0 {
			open = append(open, x.Name)
			continue
		}
		ports = append(ports, one{x.Name, vs})
		for _, v := range vs {
			if v > 1 {
				ids[v] = true
			}
		}
	}
	var out []routerVLAN
	for id := range ids {
		r := routerVLAN{ID: id}
		if routed[id] {
			r.Tagged = append(r.Tagged, "BRIDGE")
		}
		r.Tagged = append(r.Tagged, open...)
		for _, x := range ports {
			if !containsInt(x.vs, id) {
				continue
			}
			if len(x.vs) == 1 {
				r.Untagged = append(r.Untagged, x.name)
			} else {
				r.Tagged = append(r.Tagged, x.name)
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func routerBridgePort(x RouterPort) (pvid int, frames string) {
	vs, _ := parseVLANs(x.VLAN)
	switch {
	case len(vs) == 0:
		return 1, "admit-all"
	case len(vs) == 1:
		return vs[0], "admit-only-untagged-and-priority-tagged"
	case containsInt(vs, 1):
		return 1, "admit-all"
	}
	return 1, "admit-only-vlan-tagged"
}

// routerL3 liefert die Schnittstelle, auf der die WAN-Verbindung läuft.
func routerL3(x RouterPort) string {
	switch x.Type {
	case "wwan":
		return x.Name
	case "pppoe":
		return "mm-pppoe-" + x.Name
	}
	if vs, _ := parseVLANs(x.VLAN); len(vs) == 1 {
		return fmt.Sprintf("mm-wan%d", vs[0])
	}
	return x.Name
}

func routerNetIf(n RouterNet, bridge string) string {
	if n.VLAN <= 1 {
		return bridge
	}
	return fmt.Sprintf("mm-vlan%d", n.VLAN)
}

// routerGW ist der RouterOS-Ausdruck für das Gateway einer WAN-Verbindung.
func routerGW(x RouterPort) string {
	switch x.Type {
	case "ipoe":
		return q(x.Gateway)
	case "dhcp":
		return "[/ip dhcp-client get [find interface=" + q(routerL3(x)) + "] gateway]"
	}
	return q(routerL3(x))
}

// routerFailoverScript läuft alle 10 s. Die Prüfadressen gehen fest über das WAN; antworten
// beide 60 s lang nicht, übernehmen zwei /1-Routen über das Backup WAN, ohne dass die
// Verbindung am WAN angefasst wird. Sind die Adressen 60 s lang wieder da, fallen sie weg.
func routerFailoverScript(wan, backup RouterPort) string {
	var b strings.Builder
	cm := q(routerComment + "-check")
	bk := q(routerComment + "-backup")
	b.WriteString(":global mmWanFail\n:global mmWanOk\n:if ([:typeof $mmWanFail] != \"num\") do={ :set mmWanFail 0; :set mmWanOk 0 }\n")
	b.WriteString(":local gw \"\"\n:do { :set gw [:tostr " + routerGW(wan) + "] } on-error={}\n")
	b.WriteString(":local bk \"\"\n:do { :set bk [:tostr " + routerGW(backup) + "] } on-error={}\n")
	b.WriteString(":if ([:len $gw] > 0) do={\n  :foreach r in=[/ip route find comment=" + cm + "] do={ :if ([:tostr [/ip route get $r gateway]] != $gw) do={ /ip route remove $r } }\n")
	for _, a := range routerChecks {
		b.WriteString("  :if ([:len [/ip route find comment=" + cm + " dst-address=" + q(a+"/32") + "]] = 0) do={ /ip route add dst-address=" + q(a+"/32") + " gateway=$gw comment=" + cm + " }\n")
	}
	b.WriteString("}\n:local ok 0\n")
	for _, a := range routerChecks {
		b.WriteString(":do { :set ok ($ok + [/ping " + a + " count=2 interval=500ms]) } on-error={}\n")
	}
	b.WriteString(":if ($ok > 0) do={ :set mmWanFail 0; :set mmWanOk ($mmWanOk + 1) } else={ :set mmWanOk 0; :set mmWanFail ($mmWanFail + 1) }\n")
	b.WriteString(":local on [:len [/ip route find comment=" + bk + "]]\n")
	fmt.Fprintf(&b, ":if (($on = 0) && ($mmWanFail >= %d) && ([:len $bk] > 0)) do={\n", routerCheckAll)
	b.WriteString("  /ip route add dst-address=0.0.0.0/1 gateway=$bk comment=" + bk + "\n  /ip route add dst-address=128.0.0.0/1 gateway=$bk comment=" + bk + "\n  :log warning \"mikrotik-manager: WAN ohne Antwort, Backup WAN aktiv\"\n}\n")
	fmt.Fprintf(&b, ":if (($on > 0) && ($mmWanOk >= %d)) do={\n", routerCheckAll)
	b.WriteString("  /ip route remove [find comment=" + bk + "]\n  :log info \"mikrotik-manager: WAN antwortet wieder, Backup WAN aus\"\n}\n")
	return b.String()
}

// routerWanScript richtet eine WAN-Verbindung ein. dist 1 = WAN, 2 = Backup WAN.
func routerWanScript(b *strings.Builder, x RouterPort, secret string, dist int) {
	cm := q(routerComment)
	base := q(x.Name)
	b.WriteString(try("/interface bridge port remove [find interface=" + base + "]"))
	if x.Type != "wwan" {
		if vs, _ := parseVLANs(x.VLAN); len(vs) == 1 {
			fmt.Fprintf(b, "/interface vlan add name=%s interface=%s vlan-id=%d comment=%s\n", q(fmt.Sprintf("mm-wan%d", vs[0])), base, vs[0], cm)
			base = q(fmt.Sprintf("mm-wan%d", vs[0]))
		}
	}
	switch x.Type {
	case "ipoe":
		fmt.Fprintf(b, "/ip address add address=%s interface=%s comment=%s\n", q(x.Address+"/"+x.Mask), base, cm)
		fmt.Fprintf(b, "/ip route add dst-address=0.0.0.0/0 gateway=%s distance=%d comment=%s\n", q(x.Gateway), dist, cm)
	case "dhcp":
		fmt.Fprintf(b, "/ip dhcp-client add interface=%s add-default-route=yes default-route-distance=%d use-peer-dns=yes use-peer-ntp=no comment=%s disabled=no\n", base, dist, cm)
	case "pppoe":
		fmt.Fprintf(b, "/interface pppoe-client add name=%s interface=%s user=%s password=%s add-default-route=yes default-route-distance=%d use-peer-dns=yes comment=%s disabled=no\n",
			q(routerL3(x)), base, q(x.User), q(secret), dist, cm)
		b.WriteString("/ip firewall mangle add chain=forward action=change-mss new-mss=clamp-to-pmtu protocol=tcp tcp-flags=syn out-interface=" + q(routerL3(x)) + " comment=" + cm + "\n")
	case "wwan":
		if secret != "" {
			b.WriteString(try("/interface lte set [find name=" + base + "] pin=" + q(secret)))
		}
		if x.APN != "" {
			b.WriteString(try("/interface lte apn set [find name=\"default\"] apn=" + q(x.APN)))
		} else {
			b.WriteString(try("/interface lte apn unset [find name=\"default\"] value-name=apn"))
		}
		fmt.Fprintf(b, "/interface lte apn set [find name=\"default\"] add-default-route=yes default-route-distance=%d use-peer-dns=yes\n", dist)
	}
	b.WriteString("/interface list member add list=" + q(routerWanList) + " interface=" + q(routerL3(x)) + " comment=" + cm + "\n")
}

// routerHash ist der Stand von Konfiguration und Geheimnissen.
func routerHash(c RouterCfg, secrets map[string]string) string {
	c.Updated = time.Time{}
	b, _ := json.Marshal(c)
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write(b)
	for _, k := range keys {
		h.Write([]byte("\x00" + k + "\x00" + secrets[k]))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// routerScript baut die ganze Konfiguration. Die Reihenfolge räumt erst auf, damit nichts an
// alten Namen oder Adressen hängt; das Anmachen der VLAN-Filterung kommt zuletzt.
func routerScript(c RouterCfg, secrets map[string]string, bridge string) string {
	var b strings.Builder
	cm := q(routerComment)
	br := q(bridge)
	tun := q(routerTunnel)
	b.WriteString("# aufraeumen\n")
	for _, l := range []string{
		"/system scheduler remove [find name=" + q(routerFailover) + "]",
		"/system script remove [find name=" + q(routerFailover) + "]",
		"/system script remove [find name=" + q(routerState) + "]",
		"/ip route remove [find comment~\"^" + routerComment + "\"]",
		"/ip route remove [find where dynamic=no and dst-address=\"0.0.0.0/0\"]",
		"/ip firewall nat remove [find dynamic=no]",
		"/ip firewall mangle remove [find comment=" + cm + "]",
		"/ip firewall filter remove [find where dynamic=no and in-interface!=" + tun + "]",
		"/ip dhcp-server remove [find]",
		"/ip dhcp-server network remove [find]",
		"/ip pool remove [find]",
		"/ip dhcp-client remove [find]",
		"/interface pppoe-client remove [find]",
		"/ip address remove [find where dynamic=no and interface!=" + tun + "]",
		"/interface list member remove [find comment=" + cm + "]",
		"/interface list remove [find comment=" + cm + "]",
		"/interface bridge vlan remove [find where bridge=" + br + " and dynamic=no]",
		"/interface vlan remove [find comment=" + cm + "]",
	} {
		b.WriteString(try(l))
	}
	b.WriteString("# bridge\n")
	b.WriteString(":if ([:len [/interface bridge find name=" + br + "]] = 0) do={ /interface bridge add name=" + br + " comment=" + cm + " }\n")
	b.WriteString("/interface bridge set [find name=" + br + "] vlan-filtering=no pvid=1\n")
	for _, n := range []string{routerWanList, routerLanList, routerNetList} {
		b.WriteString("/interface list add name=" + q(n) + " comment=" + cm + "\n")
	}
	for _, x := range c.Ports {
		if _, isLTE := map[string]bool{"wwan": true}[x.Type]; isLTE {
			continue
		}
		if x.Role == "off" {
			b.WriteString(try("/interface bridge port remove [find interface=" + q(x.Name) + "]"))
			b.WriteString(try("/interface ethernet set [find name=" + q(x.Name) + "] disabled=yes"))
			continue
		}
		b.WriteString(try("/interface ethernet set [find name=" + q(x.Name) + "] disabled=no"))
		if x.Role == "access" {
			pvid, frames := routerBridgePort(x)
			b.WriteString(try("/interface bridge port remove [find interface=" + q(x.Name) + "]"))
			fmt.Fprintf(&b, "/interface bridge port add bridge=%s interface=%s pvid=%d frame-types=%s ingress-filtering=yes comment=%s\n", br, q(x.Name), pvid, frames, cm)
		}
	}
	b.WriteString("# wan\n")
	var wan, backup *RouterPort
	for i := range c.Ports {
		switch c.Ports[i].Role {
		case "wan":
			wan = &c.Ports[i]
		case "backup":
			backup = &c.Ports[i]
		}
	}
	routerWanScript(&b, *wan, secrets[wan.Name], 1)
	if backup != nil {
		routerWanScript(&b, *backup, secrets[backup.Name], 2)
	}
	var dns []string
	for _, x := range []*RouterPort{wan, backup} {
		if x != nil && x.Type == "ipoe" {
			dns = append(dns, x.DNS)
		}
	}
	b.WriteString("/ip dns set allow-remote-requests=yes servers=" + q(strings.Join(dns, ",")) + "\n")
	b.WriteString("# netzwerke\n")
	for _, n := range c.Nets {
		ifc := routerNetIf(n, bridge)
		if n.VLAN > 1 {
			fmt.Fprintf(&b, "/interface vlan add name=%s interface=%s vlan-id=%d comment=%s\n", q(ifc), br, n.VLAN, cm)
			b.WriteString("/interface list member add list=" + q(routerNetList) + " interface=" + q(ifc) + " comment=" + cm + "\n")
		} else {
			b.WriteString("/interface list member add list=" + q(routerLanList) + " interface=" + q(ifc) + " comment=" + cm + "\n")
		}
		fmt.Fprintf(&b, "/ip address add address=%s interface=%s comment=%s\n", q(n.Address), q(ifc), cm)
		if n.DHCP {
			pfx := netip.MustParsePrefix(n.Address)
			pool := fmt.Sprintf("mm-pool%d", n.VLAN)
			dnsSrv := n.DNS
			if dnsSrv == "" {
				dnsSrv = pfx.Addr().String()
			}
			fmt.Fprintf(&b, "/ip pool add name=%s ranges=%s comment=%s\n", q(pool), q(n.First+"-"+n.Last), cm)
			fmt.Fprintf(&b, "/ip dhcp-server add name=%s interface=%s address-pool=%s lease-time=1h comment=%s disabled=no\n", q(fmt.Sprintf("mm-dhcp%d", n.VLAN)), q(ifc), q(pool), cm)
			fmt.Fprintf(&b, "/ip dhcp-server network add address=%s gateway=%s dns-server=%s comment=%s\n", q(pfx.Masked().String()), q(pfx.Addr().String()), q(dnsSrv), cm)
		}
	}
	b.WriteString("# firewall\n")
	wl, ll, nl := q(routerWanList), q(routerLanList), q(routerNetList)
	b.WriteString("/ip firewall nat add chain=srcnat out-interface-list=" + wl + " action=masquerade comment=" + cm + "\n")
	for _, r := range []string{
		"chain=input connection-state=established,related,untracked action=accept",
		"chain=input connection-state=invalid action=drop",
		"chain=input protocol=icmp action=accept",
		"chain=input in-interface-list=" + ll + " action=accept",
		"chain=input in-interface-list=" + nl + " protocol=udp dst-port=53,67 action=accept",
		"chain=input in-interface-list=" + nl + " protocol=tcp dst-port=53 action=accept",
		"chain=input action=drop",
		"chain=forward connection-state=established,related,untracked action=accept",
		"chain=forward connection-state=invalid action=drop",
		"chain=forward in-interface-list=" + ll + " action=accept",
		"chain=forward in-interface-list=" + nl + " out-interface-list=" + wl + " action=accept",
		"chain=forward action=drop",
	} {
		b.WriteString("/ip firewall filter add " + r + " comment=" + cm + "\n")
	}
	if backup != nil {
		pol := "read,write,test"
		n := q(routerFailover)
		b.WriteString("# rueckfall\n/system script add name=" + n + " policy=" + pol + " comment=" + cm + " source=" + rsrc(routerFailoverScript(*wan, *backup)) + "\n")
		b.WriteString("/system scheduler add name=" + n + " policy=" + pol + " comment=" + cm + " interval=10s on-event=" + n + "\n")
	}
	b.WriteString("# vlan\n")
	for _, v := range routerVLANs(c) {
		tagged := make([]string, 0, len(v.Tagged))
		for _, t := range v.Tagged {
			if t == "BRIDGE" {
				t = bridge
			}
			tagged = append(tagged, t)
		}
		fmt.Fprintf(&b, "/interface bridge vlan add bridge=%s vlan-ids=%d", br, v.ID)
		if len(tagged) > 0 {
			b.WriteString(" tagged=" + q(strings.Join(tagged, ",")))
		}
		if len(v.Untagged) > 0 {
			b.WriteString(" untagged=" + q(strings.Join(v.Untagged, ",")))
		}
		b.WriteString(" comment=" + cm + "\n")
	}
	b.WriteString("/interface bridge set [find name=" + br + "] vlan-filtering=yes\n")
	return b.String()
}

// routerLive liest die Ports des Geräts für die Liste im Tab.
type routerLive struct {
	Name string `json:"name"`
	LTE  bool   `json:"lte,omitempty"`
}

func readRouterPorts(ctx context.Context, cl *routeros.Client) ([]routerLive, error) {
	l, err := printAll(ctx, cl, "/interface/ethernet/print", "=.proplist=name")
	if err != nil {
		return nil, err
	}
	var out []routerLive
	for _, x := range l {
		out = append(out, routerLive{Name: x["name"]})
	}
	if lte, err := printAll(ctx, cl, "/interface/lte/print", "=.proplist=name"); err == nil {
		for _, x := range lte {
			out = append(out, routerLive{Name: x["name"], LTE: true})
		}
	}
	return out, nil
}

// RouterPorts liefert die Ports eines Geräts.
func (p *Poller) RouterPorts(ctx context.Context, id string) ([]routerLive, error) {
	d, ok := p.st.Get(id)
	if !ok {
		return nil, fmt.Errorf("Gerät unbekannt")
	}
	var out []routerLive
	err := p.withClient(ctx, d, func(ctx context.Context, cl *routeros.Client) (err error) {
		out, err = readRouterPorts(ctx, cl)
		return err
	})
	return out, err
}

// routerSteps fasst den Plan für die Vorschau zusammen.
func routerSteps(c RouterCfg) []string {
	var out []string
	label := map[string]string{"wan": "WAN", "backup": "Backup WAN"}
	typ := map[string]string{"ipoe": "feste IP", "dhcp": "DHCP", "pppoe": "PPPoE", "wwan": "WWAN"}
	var acc, off []string
	for _, x := range c.Ports {
		switch x.Role {
		case "wan", "backup":
			s := fmt.Sprintf("%s: %s, %s", label[x.Role], x.Name, typ[x.Type])
			if x.VLAN != "" {
				s += ", VLAN " + x.VLAN
			}
			out = append(out, s)
		case "access":
			if x.VLAN != "" {
				acc = append(acc, x.Name+" (VLAN "+x.VLAN+")")
			} else {
				acc = append(acc, x.Name)
			}
		default:
			off = append(off, x.Name)
		}
	}
	out = append(out, "Access: "+strings.Join(acc, ", "))
	if len(off) > 0 {
		out = append(out, "Aus: "+strings.Join(off, ", "))
	}
	for _, n := range c.Nets {
		s := "Netzwerk " + n.Address
		if n.VLAN > 1 {
			s += fmt.Sprintf(", VLAN %d", n.VLAN)
		} else {
			s += ", Standard-LAN"
		}
		if n.DHCP {
			s += ", DHCP " + n.First + " bis " + n.Last
		}
		out = append(out, s)
	}
	out = append(out, "NAT ins Internet, von außen alles gesperrt, Netzwerke untereinander gesperrt (Standard-LAN darf in die anderen)")
	for _, x := range c.Ports {
		if x.Role == "backup" {
			out = append(out, "Rückfallprüfung: 60 s ohne Antwort über das WAN schaltet auf das Backup WAN, 60 s stabil wieder zurück")
		}
	}
	return out
}

func (p *Poller) planRouter(ctx context.Context, cl *routeros.Client, id string) (Plan, error) {
	pl := Plan{Action: "router"}
	d, _ := p.st.Get(id)
	key := routerKeyOf(d)
	c := p.Router(key)
	if c.Device != id {
		pl.Blocked = "Dieses Gerät ist hier nicht als Router gewählt."
		return pl, nil
	}
	if err := validRouter(&c); err != nil {
		pl.Blocked = err.Error()
		return pl, nil
	}
	live, err := readRouterPorts(ctx, cl)
	if err != nil {
		return pl, err
	}
	have := map[string]routerLive{}
	for _, x := range live {
		have[x.Name] = x
	}
	secrets := map[string]string{}
	seen := map[string]bool{}
	var backup bool
	for _, x := range c.Ports {
		seen[x.Name] = true
		l, ok := have[x.Name]
		if !ok {
			pl.Blocked = "Port " + x.Name + " gibt es auf dem Gerät nicht."
			return pl, nil
		}
		if (x.Type == "wwan") != l.LTE && x.Role != "off" && (x.Role == "wan" || x.Role == "backup") {
			pl.Blocked = x.Name + ": WWAN geht nur an LTE-Schnittstellen und LTE nur mit WWAN."
			return pl, nil
		}
		if l.LTE && x.Role == "access" {
			pl.Blocked = x.Name + ": LTE kann nicht Access sein."
			return pl, nil
		}
		if s, ok := p.vault.Get(routerSecretKey(key, x.Name)); ok && (x.Type == "pppoe" || x.Type == "wwan") {
			secrets[x.Name] = s.Password
		}
		backup = backup || x.Role == "backup"
	}
	for n := range have {
		if !seen[n] {
			pl.Blocked = "Port " + n + " fehlt in der Liste. Bitte die Liste neu laden und speichern."
			return pl, nil
		}
	}
	if backup {
		if l, err := printAll(ctx, cl, "/system/device-mode/print"); err == nil && len(l) > 0 {
			s := l[0]["scheduler"]
			if s == "no" || s == "false" || (s == "" && l[0]["mode"] == "home") {
				pl.Blocked = "Device-Mode home sperrt Zeitpläne, ohne sie gibt es keine Rückfallprüfung. Erst den Modus ändern."
				return pl, nil
			}
		}
	}
	hash := routerHash(c, secrets)
	addrs, err := printAll(ctx, cl, "/ip/address/print", "=.proplist=address,interface,dynamic")
	if err != nil {
		return pl, err
	}
	onTunnel := false
	cur := map[string]bool{}
	for _, a := range addrs {
		cur[a["address"]] = true
		if ip, _, _ := strings.Cut(a["address"], "/"); ip == d.IP && a["interface"] == routerTunnel {
			onTunnel = true
		}
	}
	missing := false
	for _, n := range c.Nets {
		missing = missing || !cur[n.Address]
	}
	st, err := printAll(ctx, cl, "/system/script/print", "?name="+routerState, "=.proplist=source")
	if err != nil {
		return pl, err
	}
	if len(st) > 0 && strings.Contains(st[0]["source"], hash) && !missing {
		pl.Noop = true
		pl.Steps = []string{"Das Gerät entspricht der Router-Konfiguration – nichts zu tun"}
		return pl, nil
	}
	bridge := "bridge"
	if l, err := printAll(ctx, cl, "/interface/bridge/print", "=.proplist=name"); err == nil && len(l) > 0 {
		bridge = l[0]["name"]
	}
	count := func(cmd string) int {
		l, err := printAll(ctx, cl, cmd, "=.proplist=.id,dynamic")
		n := 0
		if err == nil {
			for _, x := range l {
				if x["dynamic"] != "true" {
					n++
				}
			}
		}
		return n
	}
	pl.Steps = append(routerSteps(c), fmt.Sprintf("Vorher ein Backup auf den Manager und als Rückweg auf das Gerät. Ersetzt werden: %d Adressen, %d DHCP-Server, %d NAT-Regeln, %d Firewall-Regeln",
		count("/ip/address/print"), count("/ip/dhcp-server/print"), count("/ip/firewall/nat/print"), count("/ip/firewall/filter/print")))
	pl.Warn = append(pl.Warn, "Die Verbindung zum Router und das Netz dahinter sind beim Ausrollen kurz weg.",
		"Geräte hinter dem Router behalten ihre alte Adresse, bis ihre DHCP-Lease erneuert wird. Ein kurzes Ziehen des Kabels oder ein Neustart beschleunigt das.")
	for _, n := range p.routerCentral(id).Networks {
		if n.VLAN > 1 && !routerHasVLAN(c, n.VLAN) {
			pl.Warn = append(pl.Warn, fmt.Sprintf("Die SSID %q nutzt VLAN %d, das in den Netzwerken fehlt.", n.SSID, n.VLAN))
		}
	}
	if !onTunnel {
		lan, _, _ := strings.Cut(c.Nets[0].Address, "/")
		pl.NewAddr = lan
		pl.Warn = append(pl.Warn, "Der Manager verbindet sich danach mit "+lan+" neu. Er muss dieses Netz erreichen, sonst rollt das Gerät nach 5 Minuten zurück.")
	}
	pl.Apply = routerScript(c, secrets, bridge) + "/system script add name=" + q(routerState) + " comment=" + q(routerComment) + " source=" + q("# "+hash) + "\n"
	return pl, nil
}

func routerHasVLAN(c RouterCfg, v int) bool {
	for _, n := range c.Nets {
		if n.VLAN == v {
			return true
		}
	}
	return false
}
