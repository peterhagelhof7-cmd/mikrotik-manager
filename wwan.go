package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// WWAN / LTE-Router (z. B. Chateau LTE7 ax): Das Gerät routet ins Mobilfunknetz.
// Je Gerät einstellbar: SIM-PIN je LTE-Schnittstelle, APN (optional) und das
// LAN als /24 – das Gerät bekommt immer .254, DHCP verteilt .10–.200, DNS
// 1.1.1.1 und 9.9.9.9, NTP an, NAT über LTE, Grund-Firewall gegen Zugriffe aus
// dem Mobilfunknetz. Ausrollen wie die übrigen Änderungen: Vorschau, Backup,
// Rückroll-Wächter (Commit-Confirm), Gegenprobe.
//
// Volumen: Der Manager zählt bei jeder Abfrage die Byte-Zähler der LTE-
// Schnittstellen hoch (Neustart = Zähler beginnen bei 0) und bucht sie auf den
// Kalendermonat (Stichtag 1. des Monats, Ortszeit des Managers).
//
// Am echten Gerät NOCH NICHT getestet (kein Testgerät) – siehe docs/entscheidungen.md.

const (
	wwanComment = "mm-wwan"
	wwanHost    = 254 // letzte nutzbare Adresse im /24
	wwanPoolLo  = 10
	wwanPoolHi  = 200
)

var wwanDNS = []string{"1.1.1.1", "9.9.9.9"}

// WAN über Kabel: ether4/ether5 (soweit vorhanden) in einer eigenen Bridge mit
// DHCP-Client. Ist darüber das Internet erreichbar (Ping an wanCheck über das
// WAN-Gateway, rekursive Route mit check-gateway), ist das die Standardroute
// (Distanz 1); LTE ist Ersatz (Distanz lteDistance). wanCheck ist bewusst KEIN
// DNS-Server der Clients – die Prüfadresse läuft immer über das WAN.
const (
	wanBridge   = "mm-wan"
	wanCheck    = "8.8.4.4"
	lteDistance = "2"
)

var wanPortNames = []string{"ether4", "ether5"}

// wanDhcpScript läuft im DHCP-Client von mm-wan bei Lease/Ablauf (nur ASCII).
const wanDhcpScript = `:local gw $"gateway-address"
/ip route remove [find comment="mm-wan"]
:if ($bound = 1) do={
  /ip route add dst-address=8.8.4.4/32 gateway=$gw scope=10 comment="mm-wan"
  /ip route add dst-address=0.0.0.0/0 gateway=8.8.4.4 check-gateway=ping distance=1 target-scope=11 comment="mm-wan"
}
`

const wwanNTP = "de.pool.ntp.org"

// WwanCfg ist die WWAN-Einstellung eines Geräts (PINs liegen verschlüsselt im Tresor).
type WwanCfg struct {
	Prefix  string            `json:"prefix"`        // erste drei Oktette, z. B. "192.168.50"
	APN     string            `json:"apn,omitempty"` // leer = automatisch
	PinSet  map[string]bool   `json:"pinSet"`        // LTE-Schnittstelle → PIN hinterlegt (nur Anzeige)
	PinHash map[string]string `json:"-"`             // zuletzt ausgerollte PIN (Hash) – RouterOS zeigt die PIN nicht
	Updated time.Time         `json:"updated,omitempty"`
}

var prefixRe = regexp.MustCompile(`^(\d{1,3})\.(\d{1,3})\.(\d{1,3})$`)

func validPrefix(p string) error {
	m := prefixRe.FindStringSubmatch(strings.TrimSpace(p))
	if m == nil {
		return fmt.Errorf("Netz bitte als die ersten drei Oktette angeben, z. B. 192.168.50")
	}
	var o [3]int
	for i := 0; i < 3; i++ {
		o[i], _ = strconv.Atoi(m[i+1])
		if o[i] > 255 {
			return fmt.Errorf("Oktett %d ist größer als 255", o[i])
		}
	}
	switch {
	case o[0] == 0 || o[0] == 127 || o[0] >= 224:
		return fmt.Errorf("%s.0/24 ist als LAN nicht nutzbar", p)
	case o[0] == 10 && o[1] == 250:
		return fmt.Errorf("10.250.x.x ist das Tunnelnetz des Managers")
	case o[0] == 169 && o[1] == 254:
		return fmt.Errorf("169.254.x.x ist link-local")
	}
	return nil
}

func validPIN(pin string) error {
	if len(pin) < 4 || len(pin) > 8 {
		return fmt.Errorf("PIN muss 4–8 Ziffern haben")
	}
	for _, r := range pin {
		if r < '0' || r > '9' {
			return fmt.Errorf("PIN darf nur Ziffern enthalten")
		}
	}
	return nil
}

var lteNameRe = regexp.MustCompile(`^lte\d{1,2}$`)

// --- Speicher: wwan.json (ohne PINs) + Tresor-Einträge "pin:<gerät>:<lte>" ---

type wwanStore struct {
	mu   sync.Mutex
	path string
}

func (w *wwanStore) load() map[string]WwanCfg {
	m := map[string]WwanCfg{}
	if b, err := os.ReadFile(w.path); err == nil {
		var raw map[string]struct {
			WwanCfg
			PinHash map[string]string `json:"pinHash"`
		}
		if json.Unmarshal(b, &raw) == nil {
			for k, v := range raw {
				v.WwanCfg.PinHash = v.PinHash
				m[k] = v.WwanCfg
			}
		}
	}
	return m
}

func (w *wwanStore) save(m map[string]WwanCfg) error {
	type rec struct {
		WwanCfg
		PinHash map[string]string `json:"pinHash,omitempty"`
	}
	out := map[string]rec{}
	for k, v := range m {
		out[k] = rec{v, v.PinHash}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return writeFileAtomic(w.path, b, 0o600)
}

func (p *Poller) wwan() *wwanStore {
	return &wwanStore{path: filepath.Join(p.cfg.DataDir, "wwan.json")}
}

var wwanMu sync.Mutex

// WwanConfig liefert die Einstellung eines Geräts (ok=false: noch nicht eingerichtet).
func (p *Poller) WwanConfig(id string) (WwanCfg, bool) {
	wwanMu.Lock()
	defer wwanMu.Unlock()
	c, ok := p.wwan().load()[id]
	if c.PinSet == nil {
		c.PinSet = map[string]bool{}
	}
	return c, ok
}

func wwanPinKey(id, lte string) string { return "pin:" + id + ":" + lte }

// SaveWwan speichert Netz/APN und – nur wenn angegeben – PINs. pins: LTE → PIN ("" = unverändert, "-" = löschen).
func (p *Poller) SaveWwan(id string, prefix, apn string, pins map[string]string) (WwanCfg, error) {
	prefix = strings.TrimSpace(prefix)
	if err := validPrefix(prefix); err != nil {
		return WwanCfg{}, err
	}
	apn = strings.TrimSpace(apn)
	if len(apn) > 64 || strings.ContainsAny(apn, "\"\\$ ") {
		return WwanCfg{}, fmt.Errorf("APN ungültig")
	}
	for lte, pin := range pins {
		if !lteNameRe.MatchString(lte) {
			return WwanCfg{}, fmt.Errorf("unbekannte LTE-Schnittstelle %q", lte)
		}
		if pin != "" && pin != "-" {
			if err := validPIN(pin); err != nil {
				return WwanCfg{}, fmt.Errorf("%s: %w", lte, err)
			}
		}
	}
	wwanMu.Lock()
	defer wwanMu.Unlock()
	st := p.wwan()
	all := st.load()
	c := all[id]
	c.Prefix, c.APN, c.Updated = prefix, apn, time.Now()
	if c.PinSet == nil {
		c.PinSet = map[string]bool{}
	}
	for lte, pin := range pins {
		switch pin {
		case "":
		case "-":
			p.vault.Delete(wwanPinKey(id, lte))
			delete(c.PinSet, lte)
		default:
			if err := p.vault.Set(wwanPinKey(id, lte), Cred{User: lte, Password: pin}); err != nil {
				return c, err
			}
			c.PinSet[lte] = true
		}
	}
	all[id] = c
	return c, st.save(all)
}

// markPinsApplied merkt sich nach erfolgreichem Ausrollen, welche PIN gesetzt wurde.
func (p *Poller) markPinsApplied(id string, hashes map[string]string) {
	wwanMu.Lock()
	defer wwanMu.Unlock()
	st := p.wwan()
	all := st.load()
	c := all[id]
	c.PinHash = hashes
	all[id] = c
	st.save(all)
}

func (p *Poller) forgetWwan(id string) {
	wwanMu.Lock()
	all := p.wwan().load()
	c := all[id]
	delete(all, id)
	p.wwan().save(all)
	wwanMu.Unlock()
	for lte := range c.PinSet {
		p.vault.Delete(wwanPinKey(id, lte))
	}
	os.Remove(p.wwanUsagePath(id))
}

func pinHash(id, pin string) string {
	// nur zum Wiedererkennen, kein Geheimnis-Schutz nötig (PIN liegt im Tresor)
	h := uint64(1469598103934665603)
	for _, b := range []byte(id + "\x00" + pin) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 36)
}

// --- Planen und Ausrollen ---

// wwanState: was auf dem Gerät zu WWAN gehört.
type wwanState struct {
	ltes     []string // LTE-Schnittstellen
	bridge   string   // erste Bridge (LAN)
	addrs    []map[string]string
	pools    []map[string]string
	servers  []map[string]string
	networks []map[string]string
	nat      []map[string]string
	filter   []map[string]string
	dhcpCl   []map[string]string
	dns      string // /ip dns servers
	ntpOn    bool
	ntpSrv   string
	apn      string // APN des Standardprofils
	apnDist  string // default-route-distance des Standardprofils
	ethers   []string
	ports    []map[string]string // Bridge-Ports
	bridges  []string
}

// wanPorts: ether4/ether5, soweit das Gerät sie hat.
func (s *wwanState) wanPorts() []string {
	var out []string
	for _, n := range wanPortNames {
		if contains(s.ethers, n) {
			out = append(out, n)
		}
	}
	return out
}

// wanIfs: alle Schnittstellen ins Internet (NAT, Firewall).
func (s *wwanState) wanIfs() []string {
	out := append([]string{}, s.ltes...)
	if len(s.wanPorts()) > 0 {
		out = append(out, wanBridge)
	}
	return out
}

func readWwan(ctx context.Context, cl *routeros.Client) (*wwanState, error) {
	s := &wwanState{}
	l, err := printAll(ctx, cl, "/interface/lte/print", "=.proplist=name")
	if err != nil {
		return nil, fmt.Errorf("LTE lesen: %w", err)
	}
	for _, m := range l {
		s.ltes = append(s.ltes, m["name"])
	}
	sort.Strings(s.ltes)
	if b, err := printAll(ctx, cl, "/interface/bridge/print", "=.proplist=name"); err == nil {
		for _, m := range b {
			s.bridges = append(s.bridges, m["name"])
			if s.bridge == "" && m["name"] != wanBridge {
				s.bridge = m["name"] // LAN = erste Bridge, die nicht die WAN-Bridge ist
			}
		}
	}
	if e, err := printAll(ctx, cl, "/interface/ethernet/print", "=.proplist=name"); err == nil {
		for _, m := range e {
			s.ethers = append(s.ethers, m["name"])
		}
	}
	s.ports, _ = printAll(ctx, cl, "/interface/bridge/port/print", "=.proplist=.id,interface,bridge,comment")
	s.addrs, _ = printAll(ctx, cl, "/ip/address/print", "=.proplist=.id,address,interface,comment,dynamic")
	s.pools, _ = printAll(ctx, cl, "/ip/pool/print", "=.proplist=.id,name,ranges,comment")
	s.servers, _ = printAll(ctx, cl, "/ip/dhcp-server/print", "=.proplist=.id,name,interface,address-pool,disabled,comment")
	s.networks, _ = printAll(ctx, cl, "/ip/dhcp-server/network/print", "=.proplist=.id,address,gateway,dns-server,comment")
	s.nat, _ = printAll(ctx, cl, "/ip/firewall/nat/print", "=.proplist=.id,chain,action,out-interface,comment,disabled")
	s.filter, _ = printAll(ctx, cl, "/ip/firewall/filter/print", "=.proplist=.id,comment")
	s.dhcpCl, _ = printAll(ctx, cl, "/ip/dhcp-client/print", "=.proplist=.id,interface,comment,add-default-route,use-peer-dns,script,disabled")
	if m, err := printAll(ctx, cl, "/ip/dns/print"); err == nil && len(m) > 0 {
		s.dns = m[0]["servers"]
	}
	if m, err := printAll(ctx, cl, "/system/ntp/client/print"); err == nil && len(m) > 0 {
		s.ntpOn, s.ntpSrv = m[0]["enabled"] == "true", m[0]["servers"]
	}
	if m, err := printAll(ctx, cl, "/interface/lte/apn/print", "=.proplist=name,apn,default-route-distance"); err == nil {
		for _, a := range m {
			if a["name"] == "default" {
				s.apn, s.apnDist = a["apn"], a["default-route-distance"]
			}
		}
	}
	return s, nil
}

// wwanFilterRules sind die Firewall-Regeln (in dieser Reihenfolge), Kennung im Kommentar.
func wwanFilterRules(ltes []string) [][2]string {
	var out [][2]string
	out = append(out,
		[2]string{"mm-wwan in-est", "chain=input connection-state=established,related action=accept"},
		[2]string{"mm-wwan in-inv", "chain=input connection-state=invalid action=drop"})
	for _, l := range ltes {
		out = append(out, [2]string{"mm-wwan in-" + l, "chain=input in-interface=" + l + " action=drop"})
	}
	out = append(out,
		[2]string{"mm-wwan fw-est", "chain=forward connection-state=established,related action=accept"},
		[2]string{"mm-wwan fw-inv", "chain=forward connection-state=invalid action=drop"})
	for _, l := range ltes {
		out = append(out, [2]string{"mm-wwan fw-" + l, "chain=forward in-interface=" + l + " connection-state=new action=drop"})
	}
	return out
}

func hasComment(l []map[string]string, c string) map[string]string {
	for _, m := range l {
		if m["comment"] == c {
			return m
		}
	}
	return nil
}

// planWwanState plant die Router-Konfiguration (getrennt vom Lesen, damit testbar).
// pins: LTE → PIN aus dem Tresor (nur hinterlegte), applied: zuletzt ausgerollte PIN-Hashes.
func planWwanState(s *wwanState, id string, c WwanCfg, pins map[string]string, applied map[string]string) Plan {
	p := Plan{Action: "wwan"}
	if len(s.ltes) == 0 {
		p.Blocked = "Das Gerät hat keine LTE-Schnittstelle."
		return p
	}
	if s.bridge == "" {
		p.Blocked = "Keine Bridge auf dem Gerät – das LAN braucht eine Bridge."
		return p
	}
	if err := validPrefix(c.Prefix); err != nil {
		p.Blocked = "Netz fehlt oder ist ungültig: " + err.Error()
		return p
	}
	gw := fmt.Sprintf("%s.%d", c.Prefix, wwanHost)
	addr := gw + "/24"
	net := c.Prefix + ".0/24"
	rng := fmt.Sprintf("%s.%d-%s.%d", c.Prefix, wwanPoolLo, c.Prefix, wwanPoolHi)
	dns := strings.Join(wwanDNS, ",")
	br := s.bridge

	var d []string
	if a := hasComment(s.addrs, wwanComment); a == nil || a["address"] != addr || a["interface"] != br {
		d = append(d, "Adresse "+addr+" auf "+br)
	}
	for _, a := range s.addrs {
		if a["comment"] != wwanComment && a["dynamic"] != "true" && strings.HasPrefix(a["address"], c.Prefix+".") && a["interface"] == br {
			d = append(d, "andere feste Adresse im selben Netz ("+a["address"]+") wird entfernt")
		}
	}
	if x := hasComment(s.pools, wwanComment); x == nil || x["ranges"] != rng {
		d = append(d, "Adressbereich "+rng)
	}
	if x := hasComment(s.servers, wwanComment); x == nil || x["interface"] != br || x["disabled"] == "true" {
		d = append(d, "DHCP-Server auf "+br)
	}
	if x := hasComment(s.networks, wwanComment); x == nil || x["address"] != net || x["gateway"] != gw || x["dns-server"] != dns {
		d = append(d, fmt.Sprintf("DHCP-Netz %s, Gateway %s, DNS %s", net, gw, dns))
	}
	for _, l := range s.wanIfs() {
		found := false
		for _, n := range s.nat {
			found = found || (n["comment"] == wwanComment+" "+l && n["out-interface"] == l && n["action"] == "masquerade" && n["disabled"] != "true")
		}
		if !found {
			d = append(d, "NAT über "+l)
		}
	}
	for _, r := range wwanFilterRules(s.wanIfs()) {
		if hasComment(s.filter, r[0]) == nil {
			d = append(d, "Firewall-Regel „"+r[0]+"“ fehlt")
			break
		}
	}
	for _, x := range s.dhcpCl {
		if x["interface"] == br {
			d = append(d, "DHCP-Client auf "+br+" wird entfernt (das Gerät ist jetzt selbst Router)")
		}
	}
	if s.dns != dns {
		d = append(d, "DNS-Server "+dns+" statt "+orStd(s.dns))
	}
	if !s.ntpOn || s.ntpSrv != wwanNTP {
		d = append(d, "NTP-Client an ("+wwanNTP+")")
	}
	if s.apn != c.APN && !(c.APN == "" && s.apn == "") {
		d = append(d, "APN „"+orStd(c.APN)+"“")
	}
	if s.apnDist != lteDistance {
		d = append(d, "LTE-Standardroute Distanz "+lteDistance+" (Ersatz)")
	}
	wan := s.wanPorts()
	if len(wan) > 0 {
		if !contains(s.bridges, wanBridge) {
			d = append(d, "Bridge "+wanBridge+" fehlt")
		}
		for _, n := range wan {
			ok := false
			for _, pt := range s.ports {
				ok = ok || (pt["interface"] == n && pt["bridge"] == wanBridge)
			}
			if !ok {
				d = append(d, n+" gehört nicht zu "+wanBridge)
			}
		}
		x := hasComment(s.dhcpCl, wwanComment)
		if x == nil || x["interface"] != wanBridge || x["add-default-route"] != "no" || x["disabled"] == "true" ||
			strings.TrimSpace(x["script"]) != strings.TrimSpace(wanDhcpScript) {
			d = append(d, "DHCP-Client auf "+wanBridge+" (mit Internet-Prüfung)")
		}
	}
	hashes := map[string]string{}
	for _, l := range s.ltes {
		if pin, ok := pins[l]; ok {
			hashes[l] = pinHash(id, pin)
			if applied[l] != hashes[l] {
				d = append(d, "SIM-PIN für "+l+" setzen")
			}
		}
	}
	for l := range pins {
		if !contains(s.ltes, l) {
			p.Warn = append(p.Warn, "PIN für "+l+" hinterlegt, das Gerät hat diese Schnittstelle aber nicht.")
		}
	}
	if len(d) == 0 {
		p.Noop = true
		p.Steps = []string{"entspricht der WWAN-Einstellung – nichts zu tun"}
		return p
	}
	sort.Strings(d)
	p.Steps = append([]string{"Abweichungen:"}, prefixAll("  ", d)...)
	p.Steps = append(p.Steps,
		"Backup auf den Manager, Rückroll-Wächter 5 min",
		fmt.Sprintf("LAN %s: Gerät %s, DHCP %s, DNS %s", net, gw, rng, dns),
		"NAT über "+strings.Join(s.wanIfs(), ", ")+"; Firewall: aus dem Internet nur Antworten (established/related)",
		"NTP "+wwanNTP)
	if len(wan) > 0 {
		p.Steps = append(p.Steps, fmt.Sprintf("WAN: %s in eigener Bridge %s mit DHCP-Client; ist über das WAN das Internet erreichbar (Ping %s), ist das die Standardroute – LTE springt als Ersatz ein (Distanz %s)",
			strings.Join(wan, ", "), wanBridge, wanCheck, lteDistance))
		p.Warn = append(p.Warn, strings.Join(wan, " und ")+" gehören danach nicht mehr zum LAN, sondern sind der WAN-Anschluss (Kabel zum Internet-Router/Modem).")
	}
	p.Warn = append(p.Warn,
		"Das Gerät wird Router: Clients im LAN bekommen neue Adressen aus "+net+" – Geräte mit fester Adresse im alten Netz verlieren den Zugang.",
		"Läuft die Verwaltung bisher über das LAN, geht sie danach über LTE (WireGuard-Tunnel zum Manager).")
	p.Apply = wwanScript(s, c, pins)
	p.wwanPins = hashes
	return p
}

func prefixAll(pre string, l []string) []string {
	out := make([]string, len(l))
	for i, s := range l {
		out[i] = pre + s
	}
	return out
}

func wwanScript(s *wwanState, c WwanCfg, pins map[string]string) string {
	gw := fmt.Sprintf("%s.%d", c.Prefix, wwanHost)
	br := q(s.bridge)
	cm := q(wwanComment)
	var b strings.Builder
	b.WriteString("# WWAN / LTE-Router\n")
	// LTE: PIN und APN zuerst – ohne Mobilfunk kein Weg zurück zum Manager
	for _, l := range s.ltes {
		if pin, ok := pins[l]; ok {
			b.WriteString(try("/interface lte set [find name=" + q(l) + "] pin=" + q(pin)))
		}
	}
	if c.APN != "" {
		b.WriteString(try("/interface lte apn set [find name=\"default\"] apn=" + q(c.APN)))
	} else {
		b.WriteString(try("/interface lte apn unset [find name=\"default\"] value-name=apn"))
	}
	b.WriteString(try("/interface lte apn set [find name=\"default\"] add-default-route=yes default-route-distance=" + lteDistance))
	// WAN über Kabel (ether4/ether5)
	if wan := s.wanPorts(); len(wan) > 0 {
		wb := q(wanBridge)
		b.WriteString(":if ([:len [/interface bridge find name=" + wb + "]] = 0) do={ /interface bridge add name=" + wb + " comment=" + cm + " }\n")
		for _, n := range wan {
			b.WriteString(try("/interface bridge port remove [find interface=" + q(n) + "]"))
			b.WriteString("/interface bridge port add bridge=" + wb + " interface=" + q(n) + " comment=" + cm + "\n")
		}
		b.WriteString(try("/ip dhcp-client remove [find comment=" + cm + "]"))
		b.WriteString(try("/ip route remove [find comment=\"mm-wan\"]"))
		b.WriteString("/ip dhcp-client add interface=" + wb + " add-default-route=no use-peer-dns=no use-peer-ntp=no comment=" + cm +
			" script=" + qml(wanDhcpScript) + " disabled=no\n")
	}
	// LAN
	b.WriteString(try("/ip dhcp-client remove [find interface=" + br + "]"))
	b.WriteString(try("/ip address remove [find comment=" + cm + "]"))
	b.WriteString(try("/ip address remove [find interface=" + br + " and dynamic=no and address~\"^" + strings.ReplaceAll(c.Prefix, ".", "\\\\.") + "\\\\.\"]"))
	b.WriteString("/ip address add address=" + q(gw+"/24") + " interface=" + br + " comment=" + cm + "\n")
	b.WriteString(try("/ip dhcp-server remove [find comment=" + cm + "]"))
	b.WriteString(try("/ip dhcp-server network remove [find comment=" + cm + "]"))
	b.WriteString(try("/ip pool remove [find comment=" + cm + "]"))
	b.WriteString(fmt.Sprintf("/ip pool add name=%s ranges=%s comment=%s\n", q("mm-wwan"), q(fmt.Sprintf("%s.%d-%s.%d", c.Prefix, wwanPoolLo, c.Prefix, wwanPoolHi)), cm))
	b.WriteString("/ip dhcp-server add name=\"mm-wwan\" interface=" + br + " address-pool=\"mm-wwan\" lease-time=1h comment=" + cm + " disabled=no\n")
	b.WriteString("/ip dhcp-server network add address=" + q(c.Prefix+".0/24") + " gateway=" + q(gw) + " dns-server=" + q(strings.Join(wwanDNS, ",")) + " comment=" + cm + "\n")
	// DNS, NTP
	b.WriteString("/ip dns set servers=" + q(strings.Join(wwanDNS, ",")) + " allow-remote-requests=no\n")
	b.WriteString("/system ntp client set enabled=yes servers=" + q(wwanNTP) + "\n")
	// NAT + Firewall
	b.WriteString(try("/ip firewall nat remove [find comment~\"^mm-wwan\"]"))
	for _, l := range s.wanIfs() {
		b.WriteString("/ip firewall nat add chain=srcnat out-interface=" + q(l) + " action=masquerade comment=" + q(wwanComment+" "+l) + "\n")
	}
	b.WriteString(try("/ip firewall filter remove [find comment~\"^mm-wwan\"]"))
	for _, r := range wwanFilterRules(s.wanIfs()) {
		b.WriteString("/ip firewall filter add " + r[1] + " comment=" + q(r[0]) + "\n")
	}
	return b.String()
}

// planWwan liest das Gerät und plant (Einstieg aus planFor).
func (p *Poller) planWwan(ctx context.Context, cl *routeros.Client, id string) (Plan, error) {
	if d, ok := p.st.Get(id); ok && p.IsRouter(d) {
		return Plan{Action: "wwan", Blocked: "Dieses Gerät wird im Tab Router konfiguriert."}, nil
	}
	c, ok := p.WwanConfig(id)
	if !ok {
		return Plan{Action: "wwan", Blocked: "Für dieses Gerät ist noch kein WWAN-Netz eingetragen (Detailseite → WWAN)."}, nil
	}
	s, err := readWwan(ctx, cl)
	if err != nil {
		return Plan{Action: "wwan"}, err
	}
	pins := map[string]string{}
	for l := range c.PinSet {
		if cr, ok := p.vault.Get(wwanPinKey(id, l)); ok {
			pins[l] = cr.Password
		}
	}
	pl := planWwanState(s, id, c, pins, c.PinHash)
	if !pl.Noop && pl.Blocked == "" {
		pl.NewAddr = fmt.Sprintf("%s.%d", c.Prefix, wwanHost)
	}
	return pl, nil
}

// --- LTE-Zustand (Detailseite) ---

type LteStatus struct {
	Name     string `json:"name"`
	Status   string `json:"status,omitempty"`   // registration-status
	Operator string `json:"operator,omitempty"` // current-operator
	Tech     string `json:"tech,omitempty"`     // access-technology
	RSSI     string `json:"rssi,omitempty"`
	RSRP     string `json:"rsrp,omitempty"`
	RSRQ     string `json:"rsrq,omitempty"`
	SINR     string `json:"sinr,omitempty"`
	SIM      string `json:"sim,omitempty"` // sim-status / pin-status
	IMEI     string `json:"imei,omitempty"`
	Running  bool   `json:"running"`
	RxBytes  int64  `json:"rxBytes"`
	TxBytes  int64  `json:"txBytes"`
}

func (p *Poller) LteStatus(ctx context.Context, id string) ([]LteStatus, error) {
	cl, err := p.session(ctx, id)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	l, err := printAll(ctx, cl, "/interface/print", "?type=lte", "=.proplist=name,running,rx-byte,tx-byte")
	if err != nil {
		return nil, err
	}
	var out []LteStatus
	for _, m := range l {
		x := LteStatus{Name: m["name"], Running: m["running"] == "true"}
		x.RxBytes, _ = strconv.ParseInt(m["rx-byte"], 10, 64)
		x.TxBytes, _ = strconv.ParseInt(m["tx-byte"], 10, 64)
		if mon, err := printAll(ctx, cl, "/interface/lte/monitor", "=numbers="+x.Name, "=once="); err == nil && len(mon) > 0 {
			v := mon[0]
			x.Status, x.Operator, x.Tech = v["registration-status"], v["current-operator"], v["access-technology"]
			x.RSSI, x.RSRP, x.RSRQ, x.SINR = v["rssi"], v["rsrp"], v["rsrq"], v["sinr"]
			x.SIM, x.IMEI = firstNonEmpty(v["sim-status"], v["pin-status"]), v["imei"]
		}
		out = append(out, x)
	}
	return out, nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// --- Volumen je Monat ---

// WwanMonth ist das Volumen eines Kalendermonats (alle LTE-Schnittstellen zusammen).
type WwanMonth struct {
	Month string `json:"month"` // 2026-11
	Rx    int64  `json:"rx"`
	Tx    int64  `json:"tx"`
}

type wwanUsage struct {
	LastRx     map[string]int64 `json:"lastRx"`
	LastTx     map[string]int64 `json:"lastTx"`
	LastUptime int64            `json:"lastUptime"`
	LastAt     time.Time        `json:"lastAt"`
	Months     []WwanMonth      `json:"months"` // neuester zuerst
}

var wwanUsageMu sync.Mutex

func (p *Poller) wwanUsagePath(id string) string {
	return filepath.Join(p.cfg.DataDir, "wwan-usage", safeName(id)+".json")
}

func (p *Poller) loadUsage(id string) wwanUsage {
	var u wwanUsage
	if b, err := os.ReadFile(p.wwanUsagePath(id)); err == nil {
		json.Unmarshal(b, &u)
	}
	return u
}

// WwanUsage liefert die Monatswerte eines Geräts (neuester zuerst).
func (p *Poller) WwanUsage(id string) []WwanMonth {
	wwanUsageMu.Lock()
	defer wwanUsageMu.Unlock()
	m := p.loadUsage(id).Months
	if m == nil {
		m = []WwanMonth{}
	}
	return m
}

// addUsage bucht einen neuen Zählerstand (rein rechnend, testbar). Erste Messung
// zählt nichts (Ausgangswert); kleinerer Zählerstand oder kleinere Laufzeit =
// Neustart → der neue Stand zählt komplett. Gebucht wird auf den Monat von now.
func addUsage(u wwanUsage, rx, tx map[string]int64, uptime int64, now time.Time) wwanUsage {
	first := u.LastRx == nil
	reboot := !first && uptime > 0 && uptime < u.LastUptime
	var dRx, dTx int64
	for k, v := range rx {
		if first {
			continue
		}
		last, ok := u.LastRx[k]
		switch {
		case !ok:
			dRx += v
		case reboot || v < last:
			dRx += v
		default:
			dRx += v - last
		}
	}
	for k, v := range tx {
		if first {
			continue
		}
		last, ok := u.LastTx[k]
		switch {
		case !ok:
			dTx += v
		case reboot || v < last:
			dTx += v
		default:
			dTx += v - last
		}
	}
	u.LastRx, u.LastTx, u.LastUptime, u.LastAt = rx, tx, uptime, now
	if dRx == 0 && dTx == 0 && !first {
		return u
	}
	month := now.Format("2006-01")
	if len(u.Months) == 0 || u.Months[0].Month != month {
		u.Months = append([]WwanMonth{{Month: month}}, u.Months...)
	}
	u.Months[0].Rx += dRx
	u.Months[0].Tx += dTx
	if len(u.Months) > 60 {
		u.Months = u.Months[:60]
	}
	return u
}

// sampleWwan wird nach jeder erfolgreichen Abfrage eines LTE-Geräts aufgerufen.
func (p *Poller) sampleWwan(id string, rx, tx map[string]int64, uptime int64) {
	if len(rx) == 0 && len(tx) == 0 {
		return
	}
	wwanUsageMu.Lock()
	defer wwanUsageMu.Unlock()
	u := addUsage(p.loadUsage(id), rx, tx, uptime, time.Now())
	b, _ := json.MarshalIndent(u, "", "  ")
	os.MkdirAll(filepath.Dir(p.wwanUsagePath(id)), 0o700)
	writeFileAtomic(p.wwanUsagePath(id), b, 0o600)
}
