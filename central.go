package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Zentrale WLAN-Verwaltung: eine WLAN-Konfiguration auf dem Manager, die auf
// alle ausgewählten Geräte gleich ausgerollt wird. Ausrollen = vorhandene
// WLAN-Konfiguration des Geräts zurücksetzen (virtuelle WLANs, Profile, Werte
// an den Funkteilen) und neu aufbauen:
//   - je Band trägt das Funkteil das erste Netz dieses Bands, weitere Netze
//     werden virtuelle WLANs auf dem Funkteil
//   - alle WLANs an die erste Bridge des Geräts
//   - 2,4-GHz-Kanalplan 1/6/11 reihum über die ausgewählten Geräte (nach Name)
//
// Geprüft wird durch Neuplanung: Entspricht das Gerät der Konfiguration, ist
// nichts zu tun. Rückweg ist ein Backup auf dem Gerät selbst (backup load).

// CentralNet ist ein WLAN der zentralen Konfiguration.
type CentralNet struct {
	SSID       string `json:"ssid"`
	Passphrase string `json:"passphrase"`
	Auth       string `json:"auth"`  // wpa2-psk,wpa3-psk | wpa3-psk | wpa2-psk
	Bands      string `json:"bands"` // both | 2 | 5
	Hidden     bool   `json:"hidden"`
	VLAN       int    `json:"vlan,omitempty"` // 0 = keins; sonst Clients in diesem VLAN (Bridge-PVID)
}

// CentralRadio sind die Funkwerte eines Bands.
type CentralRadio struct {
	Enabled bool   `json:"enabled"`
	Width   string `json:"width"`   // 20 | 40 | 80
	Channel string `json:"channel"` // auto | plan (nur 2,4 GHz: 1/6/11 reihum) | MHz
	SkipDFS string `json:"skipDfs"` // nur 5 GHz: disabled | 10min-cac | all
}

// CentralWLAN ist die zentrale WLAN-Konfiguration.
type CentralWLAN struct {
	Enabled  bool         `json:"enabled"`
	Devices  []string     `json:"devices"`
	Country  string       `json:"country"`
	Networks []CentralNet `json:"networks"`
	Radio2   CentralRadio `json:"radio2"`
	Radio5   CentralRadio `json:"radio5"`
	Updated  time.Time    `json:"updated"`
}

// defaultCentral entspricht der Empfehlung im WLAN-Dialog.
func defaultCentral() CentralWLAN {
	return CentralWLAN{Country: "germany",
		Radio2: CentralRadio{Enabled: true, Width: "20", Channel: "plan"},
		Radio5: CentralRadio{Enabled: true, Width: "80", Channel: "auto", SkipDFS: "10min-cac"}}
}

var centralMu sync.Mutex

func (p *Poller) centralPath() string { return filepath.Join(p.cfg.DataDir, "wlan-central.json") }

// Central liefert die gespeicherte Konfiguration (oder die Empfehlung).
// Enthält WLAN-Passwörter → Datei 0600.
func (p *Poller) Central() CentralWLAN {
	centralMu.Lock()
	defer centralMu.Unlock()
	c := defaultCentral()
	if b, err := os.ReadFile(p.centralPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func (p *Poller) SaveCentral(c CentralWLAN) error {
	if err := p.validCentral(&c); err != nil {
		return err
	}
	c.Updated = time.Now()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	centralMu.Lock()
	defer centralMu.Unlock()
	return writeFileAtomic(p.centralPath(), b, 0o600)
}

// Managed: wird das Gerät zentral verwaltet? (Dann keine Einzeländerungen am WLAN.)
func (p *Poller) Managed(id string) bool {
	c := p.Central()
	return c.Enabled && contains(c.Devices, id)
}

func (p *Poller) validCentral(c *CentralWLAN) error {
	if !validCountry(c.Country) {
		return fmt.Errorf("Land %q nicht in der Auswahlliste", c.Country)
	}
	if len(c.Networks) == 0 || len(c.Networks) > 8 {
		return fmt.Errorf("1 bis 8 WLANs angeben")
	}
	seen := map[string]bool{}
	for i := range c.Networks {
		n := &c.Networks[i]
		n.SSID = strings.TrimSpace(n.SSID)
		if err := validSSID(n.SSID); err != nil {
			return fmt.Errorf("WLAN %d: %w", i+1, err)
		}
		if seen[n.SSID] {
			return fmt.Errorf("SSID %q doppelt", n.SSID)
		}
		seen[n.SSID] = true
		if err := validPSK(n.Passphrase); err != nil {
			return fmt.Errorf("%s: %w", n.SSID, err)
		}
		if _, _, err := pickAuth("wifi", n.Auth); err != nil {
			return fmt.Errorf("%s: %w", n.SSID, err)
		}
		n.Auth = normAuth(n.Auth)
		if n.Bands != "both" && n.Bands != "2" && n.Bands != "5" {
			return fmt.Errorf("%s: Band %q unbekannt", n.SSID, n.Bands)
		}
		// VLAN 1 ist das ungetaggte Netz der Bridge (Verwaltung) – also „kein VLAN“
		if n.VLAN == 1 {
			n.VLAN = 0
		}
		if n.VLAN < 0 || n.VLAN > 4094 {
			return fmt.Errorf("%s: VLAN %d ungültig (2–4094, leer = keins)", n.SSID, n.VLAN)
		}
	}
	chk := func(name string, r CentralRadio, is5 bool) error {
		widths := map[string]bool{"20": true, "40": true, "80": is5}
		if !widths[r.Width] {
			return fmt.Errorf("%s: Kanalbreite %q nicht wählbar", name, r.Width)
		}
		ok := r.Channel == "auto" || (!is5 && r.Channel == "plan")
		list := euChannels2
		if is5 {
			list = euChannels5
		}
		for _, ch := range list {
			ok = ok || fmt.Sprint(ch) == r.Channel
		}
		if !ok {
			return fmt.Errorf("%s: Kanal %q nicht erlaubt", name, r.Channel)
		}
		if is5 && r.SkipDFS != "disabled" && r.SkipDFS != "10min-cac" && r.SkipDFS != "all" {
			return fmt.Errorf("%s: DFS-Einstellung %q unbekannt", name, r.SkipDFS)
		}
		return nil
	}
	if err := chk("2,4 GHz", c.Radio2, false); err != nil {
		return err
	}
	if err := chk("5 GHz", c.Radio5, true); err != nil {
		return err
	}
	c.Radio2.SkipDFS = ""
	var ids []string
	for _, id := range c.Devices {
		if _, ok := p.st.Get(id); ok && !contains(ids, id) {
			ids = append(ids, id)
		}
	}
	c.Devices = ids
	return nil
}

// plan2G liefert den 2,4-GHz-Kanal eines Geräts: 1/6/11 reihum über die
// ausgewählten Geräte, sortiert nach Name (wie die Empfehlung im WLAN-Dialog).
func (p *Poller) plan2G(c CentralWLAN, id string) string {
	ids := append([]string{}, c.Devices...)
	name := func(i string) string { d, _ := p.st.Get(i); return strings.ToLower(d.Identity) }
	sort.SliceStable(ids, func(a, b int) bool { return name(ids[a]) < name(ids[b]) })
	for i, x := range ids {
		if x == id {
			return fmt.Sprint([]int{2412, 2437, 2462}[i%3])
		}
	}
	return "2412"
}

// centralRadio ist der Sollzustand eines Funkteils.
type centralRadio struct {
	x    *wlanIfc
	is5  bool
	off  bool
	nets []CentralNet // [0] am Funkteil, weitere virtuell
	auth []string     // je Netz, ans Paket angepasst
	r    CentralRadio
	freq string
}

func netKey(ssid, auth, pass string, hidden bool, vlan int) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%v\x00%d", ssid, normAuth(auth), pass, hidden, vlan)
}

// bridgeInfo: was die zentrale Verwaltung von der Bridge braucht (VLANs).
type bridgeInfo struct {
	name      string
	filtering bool           // vlan-filtering an
	pvid      map[string]int // Port → PVID
	ethers    []string       // Ethernet-Ports der Bridge (tragen die VLANs getaggt)
	mm        map[int]string // vom Manager angelegte Bridge-VLANs: VLAN → tagged
}

// vlanOf: VLAN eines Bridge-Ports (PVID 1 = ungetaggt = kein VLAN).
func (b bridgeInfo) vlanOf(port string) int {
	if v := b.pvid[port]; v > 1 {
		return v
	}
	return 0
}

func readBridge(ctx context.Context, cl *routeros.Client) bridgeInfo {
	b := bridgeInfo{pvid: map[string]int{}, mm: map[int]string{}}
	l, err := printAll(ctx, cl, "/interface/bridge/print", "=.proplist=name,vlan-filtering")
	if err != nil || len(l) == 0 {
		return b
	}
	b.name, b.filtering = l[0]["name"], l[0]["vlan-filtering"] == "true"
	eth := map[string]bool{}
	if el, err := printAll(ctx, cl, "/interface/ethernet/print", "=.proplist=name"); err == nil {
		for _, m := range el {
			eth[m["name"]] = true
		}
	}
	if pl, err := printAll(ctx, cl, "/interface/bridge/port/print", "=.proplist=interface,bridge,pvid"); err == nil {
		for _, m := range pl {
			if m["bridge"] != b.name {
				continue
			}
			v, _ := strconv.Atoi(m["pvid"])
			b.pvid[m["interface"]] = v
			if eth[m["interface"]] {
				b.ethers = append(b.ethers, m["interface"])
			}
		}
	}
	sort.Strings(b.ethers)
	if vl, err := printAll(ctx, cl, "/interface/bridge/vlan/print", "=.proplist=bridge,vlan-ids,tagged,comment"); err == nil {
		for _, m := range vl {
			if m["bridge"] == b.name && m["comment"] == mmComment {
				v, _ := strconv.Atoi(m["vlan-ids"])
				t := strings.Split(m["tagged"], ",")
				sort.Strings(t)
				b.mm[v] = strings.Join(t, ",")
			}
		}
	}
	return b
}

// planCentral vergleicht das Gerät mit der zentralen Konfiguration und baut
// bei Abweichung das Skript „zurücksetzen und neu aufbauen“.
func planCentral(ctx context.Context, cl *routeros.Client, c CentralWLAN, chan2 string) (Plan, error) {
	p := Plan{Action: "central"}
	s, err := readWLAN(ctx, cl)
	if err != nil {
		return p, err
	}
	if s.pkg == "" {
		p.Blocked = "Das Gerät hat keine WLAN-Schnittstellen."
		return p, nil
	}
	return planCentralState(s, readBridge(ctx, cl), c, chan2)
}

// planCentralState ist der reine Vergleich + Skriptbau (ohne Gerätezugriff, testbar).
func planCentralState(s *wlanState, bi bridgeInfo, c CentralWLAN, chan2 string) (Plan, error) {
	p := Plan{Action: "central"}
	br := bi.name
	if br == "" {
		p.Warn = append(p.Warn, "Keine Bridge auf dem Gerät – virtuelle WLANs hängen dann an keiner Bridge.")
	}

	// Sollzustand je Funkteil
	var radios []*centralRadio
	warned := map[string]bool{}
	for _, x := range s.ifs {
		if x.Master != "" || x.Radio == nil {
			continue
		}
		cr := &centralRadio{x: x, is5: x.Band == "5 GHz", r: c.Radio2}
		if cr.is5 {
			cr.r = c.Radio5
		}
		for _, n := range c.Networks {
			if n.Bands == "both" || (n.Bands == "5") == cr.is5 {
				a, warn, err := pickAuth(s.pkg, n.Auth)
				if err != nil {
					return p, fmt.Errorf("%s: %w", n.SSID, err)
				}
				if warn != "" && !warned[warn] {
					warned[warn] = true
					p.Warn = append(p.Warn, warn)
				}
				cr.nets = append(cr.nets, n)
				cr.auth = append(cr.auth, a)
			}
		}
		cr.off = !cr.r.Enabled || len(cr.nets) == 0
		switch cr.r.Channel {
		case "plan":
			cr.freq = chan2
		default:
			cr.freq = cr.r.Channel
		}
		if !cr.off && cr.freq != "auto" {
			ok := false
			for _, ch := range x.Radio.Channels {
				ok = ok || fmt.Sprint(ch) == cr.freq
			}
			if !ok {
				p.Blocked = fmt.Sprintf("%s: Kanal %s ist auf diesem Funkteil nicht erlaubt", x.Name, chanLabel(cr.freq))
				return p, nil
			}
		}
		if !cr.off && !contains(x.Radio.Widths, cr.r.Width) {
			p.Blocked = fmt.Sprintf("%s: Kanalbreite %s MHz kann dieses Funkteil nicht", x.Name, cr.r.Width)
			return p, nil
		}
		radios = append(radios, cr)
	}

	// Abweichungen
	var diffs []string
	for _, cr := range radios {
		x := cr.x
		var d []string
		if x.Disabled != cr.off {
			d = append(d, map[bool]string{true: "sollte aus sein", false: "sollte an sein"}[cr.off])
		}
		if !cr.off {
			n := cr.nets[0]
			if x.SSID != n.SSID {
				d = append(d, fmt.Sprintf("SSID %q statt %q", x.SSID, n.SSID))
			}
			if normAuth(x.Auth) != cr.auth[0] {
				d = append(d, fmt.Sprintf("Verschlüsselung %s statt %s", authName(x.Auth), authName(cr.auth[0])))
			}
			if x.Passphrase != n.Passphrase {
				d = append(d, "anderes Passwort")
			}
			if x.Hidden != n.Hidden {
				d = append(d, "SSID-Sichtbarkeit")
			}
			if x.Radio.Width != cr.r.Width {
				d = append(d, fmt.Sprintf("Breite %s statt %s MHz", orStd(x.Radio.Width), cr.r.Width))
			}
			if x.Radio.Freq != cr.freq {
				d = append(d, fmt.Sprintf("Kanal %s statt %s", chanLabel(x.Radio.Freq), chanLabel(cr.freq)))
			}
			if cr.is5 && x.Radio.SkipDFS != cr.r.SkipDFS {
				d = append(d, fmt.Sprintf("DFS %s statt %s", dfsLabel(x.Radio.SkipDFS), dfsLabel(cr.r.SkipDFS)))
			}
			if x.country != c.Country {
				d = append(d, fmt.Sprintf("Land %s statt %s", orStd(x.country), c.Country))
			}
			if br != "" && x.Bridge == "" {
				d = append(d, "an keiner Bridge")
			} else if br != "" && bi.vlanOf(x.Name) != n.VLAN {
				d = append(d, fmt.Sprintf("VLAN %s statt %s", vlanLabel(bi.vlanOf(x.Name)), vlanLabel(n.VLAN)))
			}
		}
		// virtuelle WLANs: genau die übrigen Netze, eingeschaltet, an der Bridge, im richtigen VLAN
		want := map[string]int{}
		if !cr.off {
			for i, n := range cr.nets[1:] {
				want[netKey(n.SSID, cr.auth[i+1], n.Passphrase, n.Hidden, n.VLAN)]++
			}
		}
		for _, y := range s.ifs {
			if y.Master != x.Name {
				continue
			}
			k := netKey(y.SSID, y.Auth, y.Passphrase, y.Hidden, bi.vlanOf(y.Name))
			if want[k] > 0 && !y.Disabled && (br == "" || y.Bridge == br) {
				want[k]--
				continue
			}
			d = append(d, fmt.Sprintf("virtuelles WLAN %s (%q) gehört nicht zur Konfiguration", y.Name, y.SSID))
		}
		for k, n := range want {
			if n > 0 {
				ssid, _, _ := strings.Cut(k, "\x00")
				d = append(d, fmt.Sprintf("virtuelles WLAN %q fehlt", ssid))
			}
		}
		sort.Strings(d)
		if len(d) > 0 {
			diffs = append(diffs, fmt.Sprintf("%s (%s): %s", x.Name, x.Band, strings.Join(d, ", ")))
		}
	}
	for _, y := range s.ifs {
		if y.Master != "" && s.find(y.Master) == nil {
			diffs = append(diffs, fmt.Sprintf("virtuelles WLAN %s ohne bekanntes Funkteil", y.Name))
		}
	}

	// VLANs an der Bridge: getaggt auf allen Ethernet-Ports, VLAN-Filter an
	vlans := centralVLANs(radios)
	if len(vlans) > 0 {
		if br == "" {
			p.Blocked = "WLANs mit VLAN brauchen eine Bridge auf dem Gerät."
			return p, nil
		}
		if len(bi.ethers) == 0 {
			p.Blocked = "Keine Ethernet-Ports in " + br + " – über die die VLANs getaggt laufen könnten."
			return p, nil
		}
	}
	tagged := strings.Join(bi.ethers, ",")
	var bd []string
	if len(vlans) > 0 && !bi.filtering {
		bd = append(bd, "VLAN-Filter aus")
	}
	for _, v := range vlans {
		if t, ok := bi.mm[v]; !ok {
			bd = append(bd, fmt.Sprintf("VLAN %d fehlt", v))
		} else if t != tagged {
			bd = append(bd, fmt.Sprintf("VLAN %d getaggt auf %s statt %s", v, t, tagged))
		}
	}
	for v := range bi.mm {
		if !containsInt(vlans, v) {
			bd = append(bd, fmt.Sprintf("VLAN %d wird nicht mehr gebraucht", v))
		}
	}
	if len(bd) > 0 {
		sort.Strings(bd)
		diffs = append(diffs, fmt.Sprintf("Bridge %s: %s", br, strings.Join(bd, ", ")))
	}

	if len(diffs) == 0 {
		p.Noop = true
		p.Steps = []string{"entspricht der zentralen WLAN-Konfiguration – nichts zu tun"}
		return p, nil
	}

	p.Steps = append(p.Steps, "Abweichungen:")
	for _, d := range diffs {
		p.Steps = append(p.Steps, "  "+d)
	}
	nVirt := 0
	for _, y := range s.ifs {
		if y.Master != "" {
			nVirt++
		}
	}
	p.Steps = append(p.Steps, "Backup auf den Manager und als Rückweg auf das Gerät",
		fmt.Sprintf("WLAN-Konfiguration zurücksetzen: %d virtuelle WLANs und alle WLAN-Profile entfernen, Werte an den Funkteilen löschen", nVirt))
	clients := 0
	for _, x := range s.ifs {
		clients += x.Clients
	}
	for _, cr := range radios {
		if cr.off {
			p.Steps = append(p.Steps, fmt.Sprintf("neu %s (%s): aus", cr.x.Name, cr.x.Band))
			continue
		}
		var nets []string
		for i, n := range cr.nets {
			v := ""
			if n.VLAN > 0 {
				v = fmt.Sprintf(" VLAN %d", n.VLAN)
			}
			nets = append(nets, fmt.Sprintf("%q %s%s", n.SSID, authName(cr.auth[i]), v))
		}
		line := fmt.Sprintf("neu %s (%s): %s MHz, Kanal %s", cr.x.Name, cr.x.Band, cr.r.Width, chanLabel(cr.freq))
		if cr.is5 {
			line += ", DFS " + dfsLabel(cr.r.SkipDFS)
		}
		p.Steps = append(p.Steps, line+" – "+strings.Join(nets, ", ")+" (weitere Netze als virtuelle WLANs)")
	}
	if len(vlans) > 0 {
		var vs []string
		for _, v := range vlans {
			vs = append(vs, fmt.Sprint(v))
		}
		p.Steps = append(p.Steps, fmt.Sprintf("Bridge %s: VLAN %s getaggt auf %s, VLAN-Filter an; Verwaltung des Geräts bleibt ungetaggt", br, strings.Join(vs, ", "), tagged))
		p.Warn = append(p.Warn,
			fmt.Sprintf("Der Switch-Port zu diesem Gerät muss VLAN %s getaggt durchreichen – sonst haben die Clients dieser WLANs kein Netz (die Prüfung des Managers merkt das nicht).", strings.Join(vs, ", ")))
		if !bi.filtering {
			p.Warn = append(p.Warn, "Der VLAN-Filter der Bridge kann auf manchen Geräten die Hardware-Beschleunigung abschalten (mehr CPU-Last bei viel Verkehr).")
		}
	}
	p.Warn = append(p.Warn, fmt.Sprintf("Das WLAN des Geräts ist beim Ausrollen kurz weg – %d verbundene WLAN-Geräte werden getrennt.", clients))
	if s.pkg == "wireless" {
		p.Apply = centralScriptLegacy(s, radios, c, br)
	} else {
		p.Apply = centralScriptWifi(s, radios, c, br)
	}
	p.Apply += centralBridgeScript(bi, radios, vlans)
	// p.Undo setzt doChange: Backup auf dem Gerät laden.
	return p, nil
}

func vlanLabel(v int) string {
	if v == 0 {
		return "keins"
	}
	return fmt.Sprint(v)
}

func containsInt(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// centralVLANs: alle VLANs, die auf eingeschalteten Funkteilen gebraucht werden.
func centralVLANs(radios []*centralRadio) []int {
	var out []int
	for _, cr := range radios {
		if cr.off {
			continue
		}
		for _, n := range cr.nets {
			if n.VLAN > 0 && !containsInt(out, n.VLAN) {
				out = append(out, n.VLAN)
			}
		}
	}
	sort.Ints(out)
	return out
}

// centralBridgeScript setzt die PVID der WLAN-Ports und die Bridge-VLANs.
// Reihenfolge: erst Ports und VLAN-Tabelle, zuletzt den VLAN-Filter einschalten.
// Eigene (nicht vom Manager angelegte) Bridge-VLANs bleiben unangetastet.
func centralBridgeScript(bi bridgeInfo, radios []*centralRadio, vlans []int) string {
	if bi.name == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Bridge/VLAN\n")
	pv := func(v int) int {
		if v == 0 {
			return 1
		}
		return v
	}
	for _, cr := range radios {
		v := 0
		if !cr.off {
			v = cr.nets[0].VLAN
		}
		b.WriteString(try("/interface bridge port set [find interface=" + q(cr.x.Name) + "] pvid=" + fmt.Sprint(pv(v))))
		if cr.off {
			continue
		}
		for i, n := range cr.nets[1:] {
			name := fmt.Sprintf("%s-v%d", cr.x.Name, i+1)
			b.WriteString("/interface bridge port set [find interface=" + q(name) + "] pvid=" + fmt.Sprint(pv(n.VLAN)) + "\n")
		}
	}
	if len(bi.mm) > 0 {
		b.WriteString(try("/interface bridge vlan remove [find comment=" + q(mmComment) + "]"))
	}
	for _, v := range vlans {
		b.WriteString(fmt.Sprintf("/interface bridge vlan add bridge=%s vlan-ids=%d tagged=%s comment=%s\n", q(bi.name), v, strings.Join(bi.ethers, ","), q(mmComment)))
	}
	if len(vlans) > 0 && !bi.filtering {
		b.WriteString("/interface bridge set [find name=" + q(bi.name) + "] vlan-filtering=yes\n")
	}
	return b.String()
}

func centralScriptWifi(s *wlanState, radios []*centralRadio, c CentralWLAN, br string) string {
	var b strings.Builder
	b.WriteString("# zuruecksetzen\n")
	for _, y := range s.ifs {
		if y.Master != "" {
			b.WriteString(try("/interface wifi remove [find name=" + q(y.Name) + "]"))
		}
	}
	for _, cr := range radios {
		var keys []string
		for k := range cr.x.raw {
			if k == "configuration" || k == "security" || k == "channel" ||
				strings.HasPrefix(k, "configuration.") || strings.HasPrefix(k, "security.") || strings.HasPrefix(k, "channel.") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(try("/interface wifi unset [find name=" + q(cr.x.Name) + "] value-name=" + k))
		}
	}
	for _, menu := range []string{"configuration", "security", "channel"} {
		b.WriteString(try("/interface wifi " + menu + " remove [find]"))
	}

	b.WriteString("# neu\n")
	idx := map[string]int{} // SSID → Profilnummer
	for i, n := range c.Networks {
		idx[n.SSID] = i + 1
	}
	made := map[int]bool{}
	profile := func(n CentralNet, auth string) string {
		i := idx[n.SSID]
		if !made[i] {
			made[i] = true
			b.WriteString(fmt.Sprintf("/interface wifi security add name=\"mm-sec%d\" authentication-types=%s passphrase=%s\n", i, auth, q(n.Passphrase)))
			b.WriteString(fmt.Sprintf("/interface wifi configuration add name=\"mm-cfg%d\" mode=ap ssid=%s country=%s security=\"mm-sec%d\" hide-ssid=%s\n",
				i, q(n.SSID), q(wifiCountry(c.Country)), i, yesNo(n.Hidden)))
		}
		return fmt.Sprintf("\"mm-cfg%d\"", i)
	}
	for _, cr := range radios {
		name := q(cr.x.Name)
		if cr.off {
			b.WriteString("/interface wifi set [find name=" + name + "] disabled=yes\n")
			continue
		}
		line := "/interface wifi set [find name=" + name + "] configuration=" + profile(cr.nets[0], cr.auth[0]) +
			" channel.width=" + wifiWidths[cr.r.Width]
		if cr.freq != "auto" {
			line += " channel.frequency=" + cr.freq
		}
		if cr.is5 {
			line += " channel.skip-dfs-channels=" + cr.r.SkipDFS
		}
		b.WriteString(line + " disabled=no\n")
		if br != "" && cr.x.Bridge == "" {
			b.WriteString(try("/interface bridge port add bridge=" + q(br) + " interface=" + name))
		}
		for i, n := range cr.nets[1:] {
			v := fmt.Sprintf("%s-v%d", cr.x.Name, i+1)
			b.WriteString("/interface wifi add name=" + q(v) + " master-interface=" + name + " configuration=" + profile(n, cr.auth[i+1]) + " disabled=no\n")
			if br != "" {
				b.WriteString("/interface bridge port add bridge=" + q(br) + " interface=" + q(v) + "\n")
			}
		}
	}
	return b.String()
}

func centralScriptLegacy(s *wlanState, radios []*centralRadio, c CentralWLAN, br string) string {
	var b strings.Builder
	b.WriteString("# zuruecksetzen\n")
	for _, y := range s.ifs {
		if y.Master != "" {
			b.WriteString(try("/interface wireless remove [find name=" + q(y.Name) + "]"))
		}
	}
	for _, cr := range radios {
		b.WriteString("/interface wireless set [find name=" + q(cr.x.Name) + "] security-profile=default\n")
	}
	b.WriteString(try("/interface wireless security-profiles remove [find default=no]"))

	b.WriteString("# neu\n")
	idx := map[string]int{}
	for i, n := range c.Networks {
		idx[n.SSID] = i + 1
	}
	made := map[int]bool{}
	profile := func(n CentralNet, auth string) string {
		i := idx[n.SSID]
		if !made[i] {
			made[i] = true
			b.WriteString(fmt.Sprintf("/interface wireless security-profiles add name=\"mm-sec%d\" %s\n", i, legacySecArgs(auth, n.Passphrase)))
		}
		return fmt.Sprintf("\"mm-sec%d\"", i)
	}
	for _, cr := range radios {
		name := q(cr.x.Name)
		if cr.off {
			b.WriteString("/interface wireless set [find name=" + name + "] disabled=yes\n")
			continue
		}
		n := cr.nets[0]
		line := "/interface wireless set [find name=" + name + "] mode=ap-bridge ssid=" + q(n.SSID) +
			" security-profile=" + profile(n, cr.auth[0]) + " hide-ssid=" + yesNo(n.Hidden) +
			" channel-width=" + q(legacyWidths[cr.r.Width]) + " frequency=" + cr.freq + " country=" + q(c.Country)
		if cr.is5 {
			line += " skip-dfs-channels=" + cr.r.SkipDFS
		}
		b.WriteString(line + " disabled=no\n")
		if br != "" && cr.x.Bridge == "" {
			b.WriteString(try("/interface bridge port add bridge=" + q(br) + " interface=" + name))
		}
		for i, vn := range cr.nets[1:] {
			v := fmt.Sprintf("%s-v%d", cr.x.Name, i+1)
			b.WriteString("/interface wireless add name=" + q(v) + " master-interface=" + name + " ssid=" + q(vn.SSID) +
				" security-profile=" + profile(vn, cr.auth[i+1]) + " hide-ssid=" + yesNo(vn.Hidden) + " disabled=no\n")
			if br != "" {
				b.WriteString("/interface bridge port add bridge=" + q(br) + " interface=" + q(v) + "\n")
			}
		}
	}
	return b.String()
}

// CentralPreview plant für jedes Gerät (parallel, ändert nichts).
func (p *Poller) CentralPreview(ctx context.Context, ids []string) []DevicePlan {
	out := make([]DevicePlan, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		d, _ := p.st.Get(id)
		out[i] = DevicePlan{ID: id, Identity: d.Identity}
		wg.Add(1)
		go func() {
			defer wg.Done()
			pl, err := p.Preview(ctx, id, ChangeReq{Action: "central"})
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			out[i].Plan = &pl
		}()
	}
	wg.Wait()
	return out
}

// DevicePlan ist das Vorschau-Ergebnis eines Geräts.
type DevicePlan struct {
	ID       string `json:"id"`
	Identity string `json:"identity"`
	Plan     *Plan  `json:"plan,omitempty"`
	Error    string `json:"error,omitempty"`
}
