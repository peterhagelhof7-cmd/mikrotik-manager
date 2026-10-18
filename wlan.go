package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-routeros/routeros/v3"
)

// WLAN: SSID, Passwort und An/Aus je Schnittstelle – für das alte
// wireless-Paket und das neue wifi-Paket (wifi-qcom-ac, wifi-qcom).
//
// Wo Werte aus Profilen kommen (wireless: Sicherheitsprofil, wifi:
// Konfigurations-/Sicherheitsprofil), wird das Profil geändert, wenn ALLE
// Schnittstellen, die es benutzen, denselben neuen Wert bekommen. Sonst wird
// nur die einzelne Schnittstelle geändert (wireless: eigenes Profil mm-N,
// wifi: Wert direkt an der Schnittstelle), damit nichts ungewollt mitwandert.

// WlanIf ist eine WLAN-Schnittstelle, wie sie die Weboberfläche zeigt.
type WlanIf struct {
	Name       string `json:"name"`
	Master     string `json:"master,omitempty"` // gesetzt = virtuelles WLAN auf diesem Funkteil
	Band       string `json:"band"`
	Chip       string `json:"chip,omitempty"`
	SSID       string `json:"ssid"`
	Auth       string `json:"auth"` // z. B. "wpa2-psk,wpa3-psk"; leer = offen
	Passphrase string `json:"passphrase"`
	Profile    string `json:"profile,omitempty"` // Sicherheitsprofil
	Disabled   bool   `json:"disabled"`
	Running    bool   `json:"running"`
	Clients    int    `json:"clients"`
	Bridge     string `json:"bridge,omitempty"`
	Hidden     bool   `json:"hidden,omitempty"` // SSID wird nicht ausgestrahlt
	// Radio: Funkwerte, nur bei physischen Schnittstellen (Funkteilen)
	Radio *RadioCfg `json:"radio,omitempty"`
}

// RadioCfg sind die Funkwerte eines Funkteils samt erlaubter Auswahl.
type RadioCfg struct {
	Band     string   `json:"band"`  // RouterOS-Wert, z. B. "2ghz-n"; "" = Standard
	Bands    []string `json:"bands"` // wählbar
	Width    string   `json:"width"` // 20 | 40 | 80 | 160; "" = Standard
	Widths   []string `json:"widths"`
	Freq     string   `json:"freq"` // "auto" oder MHz
	Channels []int    `json:"channels"`
	SkipDFS  string   `json:"skipDfs"`           // disabled | 10min-cac | all; "" = Standard
	Profile  string   `json:"profile,omitempty"` // wifi: Kanalprofil (dann nur dort änderbar)
	Current  string   `json:"current,omitempty"` // aktueller Kanal laut Gerät
}

// WlanInfo ist der WLAN-Stand eines Geräts.
type WlanInfo struct {
	Pkg        string   `json:"pkg"`     // wireless | wifi | "" (kein WLAN)
	Country    string   `json:"country"` // klein geschrieben wie bei wireless, z. B. "germany"
	Auths      []string `json:"auths"`   // wählbare Verschlüsselung
	Interfaces []WlanIf `json:"interfaces"`
}

// RadioEdit ändert Funkwerte eines Funkteils; nil = unverändert.
type RadioEdit struct {
	Band    *string `json:"band,omitempty"`
	Width   *string `json:"width,omitempty"`
	Freq    *string `json:"freq,omitempty"`
	SkipDFS *string `json:"skipDfs,omitempty"`
}

// WlanEdit ist eine gewünschte Änderung. Name wählt eine Schnittstelle,
// MatchSSID alle Schnittstellen mit dieser SSID (für „im ganzen Netz“).
// Nil-Felder bleiben unverändert.
type WlanEdit struct {
	Name       string     `json:"name,omitempty"`
	MatchSSID  string     `json:"matchSsid,omitempty"`
	SSID       *string    `json:"ssid,omitempty"`
	Passphrase *string    `json:"passphrase,omitempty"`
	Disabled   *bool      `json:"disabled,omitempty"`
	Auth       *string    `json:"auth,omitempty"` // Verschlüsselung, z. B. "wpa2-psk,wpa3-psk"
	Radio      *RadioEdit `json:"radio,omitempty"`
}

// Wählbare Verschlüsselung. Das alte wireless-Paket kann kein WPA3.
var authOptions = map[string][]string{
	"wifi":     {"wpa2-psk,wpa3-psk", "wpa3-psk", "wpa2-psk"},
	"wireless": {"wpa2-psk", "wpa-psk,wpa2-psk"},
}

// normAuth sortiert die Liste, damit "wpa3-psk,wpa2-psk" == "wpa2-psk,wpa3-psk".
func normAuth(a string) string {
	var l []string
	for _, x := range strings.Split(a, ",") {
		if x = strings.TrimSpace(x); x != "" {
			l = append(l, x)
		}
	}
	sort.Strings(l)
	return strings.Join(l, ",")
}

// pickAuth prüft die gewünschte Verschlüsselung. Beim wireless-Paket wird
// WPA2/WPA3 zu WPA2 (mit Hinweis), damit „im ganzen Netz“ überall funktioniert.
func pickAuth(pkg, want string) (string, string, error) {
	want = normAuth(want)
	if pkg == "wireless" && want == "wpa2-psk,wpa3-psk" {
		return "wpa2-psk", "Das alte wireless-Paket kann kein WPA3 – hier wird WPA2 verwendet.", nil
	}
	for _, o := range authOptions[pkg] {
		if o == want {
			return want, "", nil
		}
	}
	if pkg == "wireless" && strings.Contains(want, "wpa3") {
		return "", "", fmt.Errorf("das alte wireless-Paket kann kein WPA3 – erst auf das wifi-Paket umstellen (sofern der Funkchip es kann)")
	}
	return "", "", fmt.Errorf("Verschlüsselung %q nicht wählbar", want)
}

func authName(a string) string {
	switch normAuth(a) {
	case "":
		return "offen"
	case "wpa2-psk,wpa3-psk":
		return "WPA2/WPA3"
	case "wpa3-psk":
		return "WPA3"
	case "wpa2-psk":
		return "WPA2"
	case "wpa-psk,wpa2-psk":
		return "WPA/WPA2"
	}
	return strings.ToUpper(a)
}

type wlanIfc struct {
	WlanIf
	cfg      string            // wifi: Konfigurationsprofil
	rawWidth string            // Breite, wie RouterOS sie speichert
	raw      map[string]string // alle Werte der Schnittstelle (zum Zurücksetzen)
	country  string            // wirksames Land, klein geschrieben
}

type wlanState struct {
	pkg  string
	ifs  []*wlanIfc
	secs map[string]map[string]string // Sicherheitsprofile nach Name
	cfgs map[string]map[string]string // wifi: Konfigurationsprofile nach Name
}

// --- Funkwerte ---

// widthUI: RouterOS-Kanalbreite → 20/40/80/160 („20/40mhz-XX“ → "40").
func widthUI(w string) string {
	switch {
	case w == "":
		return ""
	case strings.HasPrefix(w, "20/40/80/160"):
		return "160"
	case strings.HasPrefix(w, "20/40/80"):
		return "80"
	case strings.HasPrefix(w, "20/40"), strings.HasPrefix(w, "40"):
		return "40"
	case strings.HasPrefix(w, "20"):
		return "20"
	}
	return w
}

var legacyWidths = map[string]string{"20": "20mhz", "40": "20/40mhz-XX", "80": "20/40/80mhz-XXXX", "160": "20/40/80/160mhz-XXXXXXXX"}
var wifiWidths = map[string]string{"20": "20mhz", "40": "20/40mhz", "80": "20/40/80mhz", "160": "20/40/80/160mhz"}

// Kanäle in Deutschland/EU (wireless kennt keine Liste per API).
var euChannels2 = []int{2412, 2417, 2422, 2427, 2432, 2437, 2442, 2447, 2452, 2457, 2462, 2467, 2472}
var euChannels5 = []int{5180, 5200, 5220, 5240, 5260, 5280, 5300, 5320, 5500, 5520, 5540, 5560, 5580, 5600, 5620, 5640, 5660, 5680, 5700}

// legacyBands: wählbare wireless-Bänder; ac nur bei ac-fähigem Chip.
func legacyBands(cur, chip string) ([]string, []string, []int) {
	if strings.HasPrefix(cur, "2ghz") {
		return []string{"2ghz-b/g/n", "2ghz-g/n", "2ghz-onlyn", "2ghz-b/g"}, []string{"20", "40"}, euChannels2
	}
	if strings.Contains(cur, "ac") || wifiChips[chip] || strings.Contains(chip, "AR9888") {
		return []string{"5ghz-a/n/ac", "5ghz-n/ac", "5ghz-onlyac", "5ghz-a/n", "5ghz-onlyn"}, []string{"20", "40", "80"}, euChannels5
	}
	return []string{"5ghz-a/n", "5ghz-onlyn", "5ghz-a"}, []string{"20", "40"}, euChannels5
}

// parseBands: "2ghz-g:20mhz,2ghz-n:20mhz,20/40mhz" → {2ghz-g:[20], 2ghz-n:[20 40]}.
func parseBands(s string) map[string][]string {
	out := map[string][]string{}
	cur := ""
	for _, tok := range strings.Split(s, ",") {
		if i := strings.Index(tok, ":"); i >= 0 {
			cur, tok = tok[:i], tok[i+1:]
			out[cur] = out[cur]
		}
		if cur != "" && tok != "" {
			out[cur] = append(out[cur], widthUI(tok))
		}
	}
	return out
}

func parseInts(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		var n int
		if _, err := fmt.Sscanf(f, "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func monitorChannel(ctx context.Context, cl *routeros.Client, menu, name string) string {
	m, err := printAll(ctx, cl, menu+"/monitor", "=numbers="+name, "=once=")
	if err != nil || len(m) == 0 {
		return ""
	}
	c := m[0]["channel"]
	if st := m[0]["state"]; st != "" && st != "running" {
		c += " (" + st + ")"
	}
	return c
}

// wlanCountries: wählbare Länder (RouterOS-Namen, wie wireless sie schreibt).
// Gleiche Liste wie in der Weboberfläche; Tippfehler lehnte sonst erst das Gerät ab.
var wlanCountries = map[string]bool{"germany": true, "austria": true, "switzerland": true, "netherlands": true,
	"belgium": true, "luxembourg": true, "france": true, "italy": true, "spain": true, "poland": true,
	"czech republic": true, "denmark": true, "united kingdom": true, "united states": true}

func validCountry(c string) bool { return wlanCountries[c] }

func dfsLabel(v string) string {
	switch v {
	case "disabled":
		return "alle erlaubt"
	case "10min-cac":
		return "ohne 10-min-Radarkanäle"
	case "all":
		return "keine DFS-Kanäle"
	}
	return orStd(v)
}

func (s *wlanState) find(name string) *wlanIfc {
	for _, x := range s.ifs {
		if x.Name == name {
			return x
		}
	}
	return nil
}

func bandName(b string) string {
	switch {
	case strings.Contains(b, "5ghz"):
		return "5 GHz"
	case strings.Contains(b, "2ghz"):
		return "2,4 GHz"
	}
	return b
}

func masterOf(m map[string]string) string {
	if mi := m["master-interface"]; mi != "" && mi != "none" {
		return mi
	}
	return ""
}

func countClients(ctx context.Context, cl *routeros.Client, cmd string) map[string]int {
	n := map[string]int{}
	if l, err := printAll(ctx, cl, cmd, "=.proplist=interface"); err == nil {
		for _, m := range l {
			n[m["interface"]]++
		}
	}
	return n
}

// readWLAN liest alle WLAN-Schnittstellen mit ihren wirksamen Werten.
func readWLAN(ctx context.Context, cl *routeros.Client) (*wlanState, error) {
	s := &wlanState{secs: map[string]map[string]string{}}
	ports := map[string]string{}
	if l, err := printAll(ctx, cl, "/interface/bridge/port/print", "=.proplist=interface,bridge"); err == nil {
		for _, m := range l {
			ports[m["interface"]] = m["bridge"]
		}
	}

	if l, err := printAll(ctx, cl, "/interface/wireless/print"); err == nil && len(l) > 0 {
		s.pkg = "wireless"
		profs, err := printAll(ctx, cl, "/interface/wireless/security-profiles/print")
		if err != nil {
			return nil, err
		}
		for _, m := range profs {
			s.secs[m["name"]] = m
		}
		clients := countClients(ctx, cl, "/interface/wireless/registration-table/print")
		band := map[string]string{}
		for _, m := range l {
			band[m["name"]] = m["band"]
		}
		for _, m := range l {
			x := &wlanIfc{WlanIf: WlanIf{
				Name: m["name"], Master: masterOf(m), SSID: m["ssid"], Profile: m["security-profile"],
				Disabled: m["disabled"] == "true", Running: m["running"] == "true",
				Clients: clients[m["name"]], Bridge: ports[m["name"]], Chip: m["interface-type"],
			}}
			x.raw, x.Hidden = m, m["hide-ssid"] == "true"
			b := m["band"]
			if x.Master != "" {
				b = band[x.Master]
			} else {
				bands, widths, chans := legacyBands(b, x.Chip)
				x.rawWidth, x.country = m["channel-width"], m["country"]
				x.Radio = &RadioCfg{Band: b, Bands: bands, Width: widthUI(x.rawWidth), Widths: widths,
					Freq: m["frequency"], Channels: chans, SkipDFS: m["skip-dfs-channels"]}
				if !x.Disabled {
					x.Radio.Current = monitorChannel(ctx, cl, "/interface/wireless", x.Name)
				}
			}
			x.Band = bandName(b)
			x.Auth, x.Passphrase = legacyAuth(s.secs[x.Profile])
			s.ifs = append(s.ifs, x)
		}
		for _, x := range s.ifs {
			if x.Master != "" {
				if m := s.find(x.Master); m != nil {
					x.country = m.country
				}
			}
		}
		return s, nil
	}

	l, err := printAll(ctx, cl, "/interface/wifi/print")
	if err != nil || len(l) == 0 {
		return s, nil // kein WLAN auf dem Gerät
	}
	s.pkg = "wifi"
	cfgs := map[string]map[string]string{}
	s.cfgs = cfgs
	if c, err := printAll(ctx, cl, "/interface/wifi/configuration/print"); err == nil {
		for _, m := range c {
			cfgs[m["name"]] = m
		}
	}
	if c, err := printAll(ctx, cl, "/interface/wifi/security/print"); err == nil {
		for _, m := range c {
			s.secs[m["name"]] = m
		}
	}
	radio := map[string]map[string]string{}
	if c, err := printAll(ctx, cl, "/interface/wifi/radio/print"); err == nil {
		for _, m := range c {
			radio[m["interface"]] = m
		}
	}
	clients := countClients(ctx, cl, "/interface/wifi/registration-table/print")
	for _, m := range l {
		x := &wlanIfc{cfg: m["configuration"], WlanIf: WlanIf{
			Name: m["name"], Master: masterOf(m), SSID: m["configuration.ssid"],
			Auth: m["security.authentication-types"], Passphrase: m["security.passphrase"],
			Profile:  m["security"],
			Disabled: m["disabled"] == "true", Running: m["running"] == "true",
			Clients: clients[m["name"]], Bridge: ports[m["name"]],
		}}
		x.raw, x.Hidden = m, m["configuration.hide-ssid"] == "true"
		if x.Profile == "" {
			x.Profile = cfgs[x.cfg]["security"]
		}
		r := radio[x.Name]
		if x.Master != "" {
			r = radio[x.Master]
		}
		x.Band, x.Chip = bandName(r["bands"]), r["hw-type"]
		x.country = strings.ToLower(m["configuration.country"])
		if x.Master == "" {
			bands := parseBands(r["bands"])
			rc := &RadioCfg{Band: m["channel.band"], Width: widthUI(m["channel.width"]), Freq: m["channel.frequency"],
				SkipDFS: m["channel.skip-dfs-channels"], Profile: m["channel"]}
			x.rawWidth = m["channel.width"]
			for b := range bands {
				rc.Bands = append(rc.Bands, b)
			}
			sort.Strings(rc.Bands)
			ws := map[string]bool{}
			for _, l := range bands {
				for _, w := range l {
					ws[w] = true
				}
			}
			for _, w := range []string{"20", "40", "80", "160"} {
				if ws[w] {
					rc.Widths = append(rc.Widths, w)
				}
			}
			if rc.Freq == "" {
				rc.Freq = "auto"
			}
			if strings.Contains(r["bands"], "5ghz") {
				rc.Channels = parseInts(r["5g-channels"])
			} else {
				rc.Channels = parseInts(r["2g-channels"])
			}
			if !x.Disabled {
				rc.Current = monitorChannel(ctx, cl, "/interface/wifi", x.Name)
			}
			x.Radio = rc
		}
		s.ifs = append(s.ifs, x)
	}
	return s, nil
}

// legacyAuth liefert Authentifizierung und Schlüssel eines wireless-Sicherheitsprofils.
func legacyAuth(p map[string]string) (auth, psk string) {
	switch p["mode"] {
	case "dynamic-keys":
		psk = p["wpa2-pre-shared-key"]
		if psk == "" {
			psk = p["wpa-pre-shared-key"]
		}
		return p["authentication-types"], psk
	case "static-keys-required", "static-keys-optional":
		return "wep", ""
	}
	return "", ""
}

// ReadWLAN liefert den WLAN-Stand für die Weboberfläche.
func (p *Poller) ReadWLAN(ctx context.Context, id string) (WlanInfo, error) {
	d, ok := p.st.Get(id)
	if !ok {
		return WlanInfo{}, fmt.Errorf("Gerät unbekannt")
	}
	var info WlanInfo
	err := p.withClient(ctx, d, func(ctx context.Context, cl *routeros.Client) error {
		s, err := readWLAN(ctx, cl)
		if err != nil {
			return err
		}
		info.Pkg = s.pkg
		info.Auths = authOptions[s.pkg]
		info.Interfaces = []WlanIf{}
		for _, x := range s.ifs {
			info.Interfaces = append(info.Interfaces, x.WlanIf)
			if info.Country == "" && x.Master == "" {
				info.Country = x.country
			}
		}
		return nil
	})
	return info, err
}

func printableASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func validSSID(s string) error {
	if s == "" || len(s) > 32 || !printableASCII(s) {
		return fmt.Errorf("SSID muss 1–32 Zeichen lang sein, nur ASCII (keine Umlaute)")
	}
	return nil
}

func validPSK(s string) error {
	if len(s) < 8 || len(s) > 63 || !printableASCII(s) {
		return fmt.Errorf("WLAN-Passwort muss 8–63 Zeichen lang sein, nur ASCII (keine Umlaute)")
	}
	return nil
}

func onOff(dis bool) string {
	if dis {
		return "ausschalten"
	}
	return "einschalten"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

type wlanChange struct{ step, apply, undo string }

func chanLabel(f string) string {
	n := 0
	fmt.Sscanf(f, "%d", &n)
	switch {
	case f == "" || f == "auto":
		return "automatisch"
	case n >= 2412 && n <= 2484:
		return fmt.Sprintf("%d (%s MHz)", (n-2407)/5, f)
	case n >= 5000:
		return fmt.Sprintf("%d (%s MHz)", (n-5000)/5, f)
	}
	return f
}

func orStd(v string) string {
	if v == "" {
		return "Standard"
	}
	return v
}

// planRadio: Band, Breite, Kanal, DFS eines Funkteils.
func planRadio(s *wlanState, x *wlanIfc, r *RadioEdit) (*wlanChange, []string, error) {
	rc := x.Radio
	if rc == nil {
		return nil, nil, fmt.Errorf("%s ist ein virtuelles WLAN – Funkwerte gelten für das Funkteil %s", x.Name, x.Master)
	}
	in := func(v string, l []string) bool {
		for _, s := range l {
			if s == v {
				return true
			}
		}
		return false
	}
	type prop struct{ key, old, new, label string }
	var props []prop
	if r.Band != nil && *r.Band != rc.Band {
		if !(in(*r.Band, rc.Bands) || (*r.Band == "" && s.pkg == "wifi")) {
			return nil, nil, fmt.Errorf("%s: Band %q nicht wählbar", x.Name, *r.Band)
		}
		props = append(props, prop{"band", rc.Band, *r.Band, fmt.Sprintf("Band %s → %s", orStd(rc.Band), orStd(*r.Band))})
	}
	if r.Width != nil && *r.Width != rc.Width {
		if !(in(*r.Width, rc.Widths) || (*r.Width == "" && s.pkg == "wifi")) {
			return nil, nil, fmt.Errorf("%s: Kanalbreite %q nicht wählbar", x.Name, *r.Width)
		}
		props = append(props, prop{"width", x.rawWidth, *r.Width, fmt.Sprintf("Breite %s → %s MHz", orStd(rc.Width), orStd(*r.Width))})
	}
	if r.Freq != nil && *r.Freq != rc.Freq {
		ok := *r.Freq == "auto"
		for _, c := range rc.Channels {
			ok = ok || fmt.Sprint(c) == *r.Freq
		}
		if !ok {
			return nil, nil, fmt.Errorf("%s: Kanal %q nicht erlaubt", x.Name, *r.Freq)
		}
		props = append(props, prop{"freq", rc.Freq, *r.Freq, fmt.Sprintf("Kanal %s → %s", chanLabel(rc.Freq), chanLabel(*r.Freq))})
	}
	if r.SkipDFS != nil && *r.SkipDFS != rc.SkipDFS {
		if !(in(*r.SkipDFS, []string{"disabled", "10min-cac", "all"}) || (*r.SkipDFS == "" && s.pkg == "wifi")) {
			return nil, nil, fmt.Errorf("%s: DFS-Einstellung %q unbekannt", x.Name, *r.SkipDFS)
		}
		props = append(props, prop{"dfs", rc.SkipDFS, *r.SkipDFS, fmt.Sprintf("DFS-Kanäle %s → %s", dfsLabel(rc.SkipDFS), dfsLabel(*r.SkipDFS))})
	}
	if len(props) == 0 {
		return nil, nil, nil
	}
	if s.pkg == "wifi" && rc.Profile != "" {
		return nil, nil, fmt.Errorf("%s: Funkwerte kommen aus dem Kanalprofil %q – bitte dort ändern (WinBox: WiFi → Channels)", x.Name, rc.Profile)
	}

	var labels []string
	var apply, undo strings.Builder
	if s.pkg == "wireless" {
		key := map[string]string{"band": "band", "width": "channel-width", "freq": "frequency", "dfs": "skip-dfs-channels"}
		var set, back []string
		for _, pr := range props {
			v := pr.new
			if pr.key == "width" {
				v = legacyWidths[v]
			}
			set = append(set, key[pr.key]+"="+q(v))
			back = append(back, key[pr.key]+"="+q(pr.old))
			labels = append(labels, pr.label)
		}
		apply.WriteString("/interface wireless set [find name=" + q(x.Name) + "] " + strings.Join(set, " ") + "\n")
		undo.WriteString(try("/interface wireless set [find name=" + q(x.Name) + "] " + strings.Join(back, " ")))
	} else {
		key := map[string]string{"band": "channel.band", "width": "channel.width", "freq": "channel.frequency", "dfs": "channel.skip-dfs-channels"}
		// leer bzw. "auto" = Wert entfernen, das Gerät nimmt dann seinen Standard
		line := func(k, v string) string {
			if v == "" || (k == "freq" && v == "auto") {
				return "/interface wifi unset [find name=" + q(x.Name) + "] value-name=" + key[k]
			}
			if k == "width" && wifiWidths[v] != "" {
				v = wifiWidths[v]
			}
			return "/interface wifi set [find name=" + q(x.Name) + "] " + key[k] + "=" + q(v)
		}
		for _, pr := range props {
			apply.WriteString(line(pr.key, pr.new) + "\n")
			undo.WriteString(try(line(pr.key, pr.old)))
			labels = append(labels, pr.label)
		}
	}
	var warn []string
	if r.Freq != nil {
		var n int
		fmt.Sscanf(*r.Freq, "%d", &n)
		if n >= 5260 && n <= 5720 {
			wait := "1 Minute"
			if n >= 5600 && n <= 5650 {
				wait = "10 Minuten"
			}
			warn = append(warn, fmt.Sprintf("%s: Kanal %s ist ein DFS-Kanal – das Funkteil prüft vor dem Senden %s auf Radar und wechselt bei Radar den Kanal.", x.Name, chanLabel(*r.Freq), wait))
		}
	}
	return &wlanChange{step: x.Name + ": " + strings.Join(labels, ", "), apply: apply.String(), undo: undo.String()}, warn, nil
}

// planCountry setzt das Land (bestimmt erlaubte Kanäle und Sendeleistung) für alle Funkteile.
func planCountry(s *wlanState, country string) []wlanChange {
	var out []wlanChange
	if s.pkg == "wireless" {
		for _, x := range s.ifs {
			if x.Master == "" && x.country != country {
				out = append(out, wlanChange{
					step:  fmt.Sprintf("%s: Land %s → %s", x.Name, orStd(x.country), country),
					apply: "/interface wireless set [find name=" + q(x.Name) + "] country=" + q(country) + "\n",
					undo:  try("/interface wireless set [find name=" + q(x.Name) + "] country=" + q(x.country)),
				})
			}
		}
		return out
	}
	v := wifiCountry(country)
	done := map[string]bool{}
	for _, x := range s.ifs {
		if x.cfg != "" {
			if done[x.cfg] {
				continue
			}
			done[x.cfg] = true
			old := s.cfgs[x.cfg]["country"]
			if strings.ToLower(old) == country {
				continue
			}
			back := "/interface wifi configuration set [find name=" + q(x.cfg) + "] country=" + q(old)
			if old == "" {
				back = "/interface wifi configuration unset [find name=" + q(x.cfg) + "] value-name=country"
			}
			out = append(out, wlanChange{
				step:  fmt.Sprintf("Konfigurationsprofil %q: Land %s → %s", x.cfg, orStd(old), v),
				apply: "/interface wifi configuration set [find name=" + q(x.cfg) + "] country=" + q(v) + "\n",
				undo:  try(back),
			})
			continue
		}
		if x.country != country {
			old := wifiCountry(x.country)
			back := "/interface wifi set [find name=" + q(x.Name) + "] configuration.country=" + q(old)
			if old == "" {
				back = "/interface wifi unset [find name=" + q(x.Name) + "] value-name=configuration.country"
			}
			out = append(out, wlanChange{
				step:  fmt.Sprintf("%s: Land %s → %s", x.Name, orStd(old), v),
				apply: "/interface wifi set [find name=" + q(x.Name) + "] configuration.country=" + q(v) + "\n",
				undo:  try(back),
			})
		}
	}
	return out
}

// planWLAN berechnet die Änderungen für die gewünschten Werte.
func planWLAN(ctx context.Context, cl *routeros.Client, edits []WlanEdit, country string) (Plan, error) {
	p := Plan{Action: "wlan"}
	s, err := readWLAN(ctx, cl)
	if err != nil {
		return p, err
	}
	if s.pkg == "" {
		p.Blocked = "Das Gerät hat keine WLAN-Schnittstellen."
		return p, nil
	}

	// Wunschwerte je Schnittstelle; Auswahl per SSID wird hier auf Namen
	// festgelegt, damit die Prüfung nach dem Ändern dieselben Schnittstellen meint.
	type want struct {
		ssid, pass, auth *string
		dis              *bool
		radio            *RadioEdit
	}
	if country != "" && !validCountry(country) {
		return p, fmt.Errorf("Land %q nicht in der Auswahlliste", country)
	}
	wants := map[string]*want{}
	var unmatched []string
	for _, e := range edits {
		if e.SSID != nil {
			if err := validSSID(*e.SSID); err != nil {
				return p, err
			}
		}
		if e.Passphrase != nil {
			if err := validPSK(*e.Passphrase); err != nil {
				return p, err
			}
		}
		hit := false
		for _, x := range s.ifs {
			if (e.Name != "" && x.Name == e.Name) || (e.Name == "" && e.MatchSSID != "" && x.SSID == e.MatchSSID) {
				hit = true
				w := wants[x.Name]
				if w == nil {
					w = &want{}
					wants[x.Name] = w
				}
				if e.SSID != nil {
					w.ssid = e.SSID
				}
				if e.Passphrase != nil {
					w.pass = e.Passphrase
				}
				if e.Disabled != nil {
					w.dis = e.Disabled
				}
				if e.Radio != nil {
					w.radio = e.Radio
				}
				if e.Auth != nil {
					w.auth = e.Auth
				}
				p.wlan = append(p.wlan, WlanEdit{Name: x.Name, SSID: e.SSID, Passphrase: e.Passphrase, Disabled: e.Disabled, Auth: e.Auth, Radio: e.Radio})
			}
		}
		if !hit {
			if e.Name != "" {
				return p, fmt.Errorf("Schnittstelle %s gibt es auf dem Gerät nicht", e.Name)
			}
			unmatched = append(unmatched, e.MatchSSID)
		}
	}

	// Abweichungen bestimmen
	// newSec: Verschlüsselung und Passwort als Paar "auth\x00passwort"
	newSSID, newSec := map[string]string{}, map[string]string{}
	var setDis []string
	authWarn := ""
	for _, x := range s.ifs {
		w := wants[x.Name]
		if w == nil {
			continue
		}
		if w.ssid != nil && *w.ssid != x.SSID {
			newSSID[x.Name] = *w.ssid
		}
		if w.pass != nil || w.auth != nil {
			auth, pass := normAuth(x.Auth), x.Passphrase
			if w.auth != nil {
				a, warn, err := pickAuth(s.pkg, *w.auth)
				if err != nil {
					return p, fmt.Errorf("%s: %w", x.Name, err)
				}
				if warn != "" {
					authWarn = warn
				}
				auth = a
			}
			if w.pass != nil {
				pass = *w.pass
			}
			if auth == "" {
				auth = authOptions[s.pkg][0] // offenes Netz bekommt ein Passwort → Standard-Verschlüsselung
			}
			if pass == "" {
				return p, fmt.Errorf("%s: für %s ist ein Passwort nötig", x.Name, authName(auth))
			}
			if auth != normAuth(x.Auth) || pass != x.Passphrase {
				newSec[x.Name] = auth + "\x00" + pass
			}
		}
		if w.dis != nil && *w.dis != x.Disabled {
			setDis = append(setDis, x.Name)
		}
	}
	// Funkwerte und Land
	var extra []wlanChange
	radioTouched := map[string]bool{}
	for _, x := range s.ifs {
		w := wants[x.Name]
		if w == nil || w.radio == nil {
			continue
		}
		c, warn, err := planRadio(s, x, w.radio)
		if err != nil {
			return p, err
		}
		if c != nil {
			extra = append(extra, *c)
			p.Warn = append(p.Warn, warn...)
			radioTouched[x.Name] = true
		}
	}
	if country != "" {
		cc := planCountry(s, country)
		if len(cc) > 0 {
			extra = append(extra, cc...)
			for _, x := range s.ifs {
				if x.Master == "" {
					radioTouched[x.Name] = true
				}
			}
			p.Warn = append(p.Warn, "Das Land muss dem tatsächlichen Standort entsprechen – es bestimmt erlaubte Kanäle und Sendeleistung.")
		}
	}

	if authWarn != "" {
		p.Warn = append(p.Warn, authWarn)
	}
	if len(newSSID)+len(newSec)+len(setDis)+len(extra) == 0 {
		p.Noop = true
		if len(wants) == 0 && country == "" {
			p.Steps = []string{fmt.Sprintf("SSID %s kommt auf diesem Gerät nicht vor – nichts zu tun", strings.Join(unmatched, ", "))}
		} else {
			p.Steps = []string{"WLAN hat bereits die gewünschten Werte – nichts zu tun"}
		}
		return p, nil
	}

	var apply, undo, undoLast strings.Builder
	touched := map[string]bool{}
	// allSame: Benutzen genau diese Schnittstellen ein Profil und bekommen alle
	// denselben neuen Wert, wird das Profil geändert.
	allSame := func(users []string, vals map[string]string) (string, bool) {
		v, ok := "", len(users) > 0
		for i, u := range users {
			nv, has := vals[u]
			if !has || (i > 0 && nv != v) {
				return "", false
			}
			v = nv
		}
		return v, ok
	}
	usersOf := func(key func(*wlanIfc) string, k string) []string {
		var u []string
		for _, x := range s.ifs {
			if key(x) == k {
				u = append(u, x.Name)
			}
		}
		sort.Strings(u)
		return u
	}
	names := func(m map[string]string) []string {
		var l []string
		for k := range m {
			l = append(l, k)
		}
		sort.Strings(l)
		return l
	}
	// secDesc beschreibt eine Sicherheitsänderung, ohne das Passwort zu nennen.
	secDesc := func(x *wlanIfc, key string) string {
		auth, pass, _ := strings.Cut(key, "\x00")
		var d []string
		if auth != normAuth(x.Auth) {
			d = append(d, fmt.Sprintf("Verschlüsselung %s → %s", authName(x.Auth), authName(auth)))
		}
		if pass != x.Passphrase {
			d = append(d, "neues Passwort")
		}
		return strings.Join(d, ", ")
	}

	if s.pkg == "wireless" {
		for _, n := range names(newSSID) {
			x := s.find(n)
			p.Steps = append(p.Steps, fmt.Sprintf("%s: SSID %q → %q", n, x.SSID, newSSID[n]))
			apply.WriteString("/interface wireless set [find name=" + q(n) + "] ssid=" + q(newSSID[n]) + "\n")
			undo.WriteString(try("/interface wireless set [find name=" + q(n) + "] ssid=" + q(x.SSID)))
			touched[n] = true
		}
		// Sicherheit: Profil ändern oder eigenes Profil mm-N je (altes Profil, neue Werte)
		done := map[string]bool{}
		next := 1
		for _, n := range names(newSec) {
			if done[n] {
				continue
			}
			x := s.find(n)
			prof := s.secs[x.Profile]
			users := usersOf(func(y *wlanIfc) string { return y.Profile }, x.Profile)
			if v, ok := allSame(users, newSec); ok {
				for _, u := range users {
					done[u] = true
					touched[u] = true
				}
				auth, pass, _ := strings.Cut(v, "\x00")
				p.Steps = append(p.Steps, fmt.Sprintf("Sicherheitsprofil %q: %s (gilt für %s)", x.Profile, secDesc(x, v), strings.Join(users, ", ")))
				apply.WriteString("/interface wireless security-profiles set [find name=" + q(x.Profile) + "] " + legacySecArgs(auth, pass) + "\n")
				undo.WriteString(try("/interface wireless security-profiles set [find name=" + q(x.Profile) + "] mode=" + q(prof["mode"]) +
					" authentication-types=" + q(prof["authentication-types"]) + " wpa-pre-shared-key=" + q(prof["wpa-pre-shared-key"]) +
					" wpa2-pre-shared-key=" + q(prof["wpa2-pre-shared-key"])))
				continue
			}
			// eigenes Profil für alle Schnittstellen dieses Profils mit denselben neuen Werten
			v := newSec[n]
			auth, pass, _ := strings.Cut(v, "\x00")
			var grp []string
			for _, u := range users {
				if newSec[u] == v && !done[u] {
					grp = append(grp, u)
				}
			}
			name := ""
			for ; name == ""; next++ {
				if _, used := s.secs[fmt.Sprintf("mm-%d", next)]; !used {
					name = fmt.Sprintf("mm-%d", next)
				}
			}
			p.Steps = append(p.Steps, fmt.Sprintf("eigenes Sicherheitsprofil %s für %s: %s (Profil %q wird auch von anderen benutzt und bleibt)", name, strings.Join(grp, ", "), secDesc(x, v), x.Profile))
			apply.WriteString("/interface wireless security-profiles add name=" + q(name) + " " + legacySecArgs(auth, pass) + "\n")
			undoLast.WriteString(try("/interface wireless security-profiles remove [find name=" + q(name) + "]"))
			for _, u := range grp {
				done[u] = true
				touched[u] = true
				apply.WriteString("/interface wireless set [find name=" + q(u) + "] security-profile=" + q(name) + "\n")
				undo.WriteString(try("/interface wireless set [find name=" + q(u) + "] security-profile=" + q(x.Profile)))
			}
		}
		for _, n := range setDis {
			x := s.find(n)
			p.Steps = append(p.Steps, fmt.Sprintf("%s (%q) %s", n, x.SSID, onOff(!x.Disabled)))
			apply.WriteString("/interface wireless set [find name=" + q(n) + "] disabled=" + yesNo(!x.Disabled) + "\n")
			undo.WriteString(try("/interface wireless set [find name=" + q(n) + "] disabled=" + yesNo(x.Disabled)))
			touched[n] = true
		}
	} else {
		done := map[string]bool{}
		for _, n := range names(newSSID) {
			if done[n] {
				continue
			}
			x := s.find(n)
			if x.cfg != "" {
				users := usersOf(func(y *wlanIfc) string { return y.cfg }, x.cfg)
				if v, ok := allSame(users, newSSID); ok {
					for _, u := range users {
						done[u] = true
						touched[u] = true
					}
					p.Steps = append(p.Steps, fmt.Sprintf("SSID %q → %q im Konfigurationsprofil %q (gilt für %s)", x.SSID, v, x.cfg, strings.Join(users, ", ")))
					apply.WriteString("/interface wifi configuration set [find name=" + q(x.cfg) + "] ssid=" + q(v) + "\n")
					undo.WriteString(try("/interface wifi configuration set [find name=" + q(x.cfg) + "] ssid=" + q(x.SSID)))
					continue
				}
			}
			done[n] = true
			touched[n] = true
			p.Steps = append(p.Steps, fmt.Sprintf("%s: SSID %q → %q (direkt an der Schnittstelle)", n, x.SSID, newSSID[n]))
			apply.WriteString("/interface wifi set [find name=" + q(n) + "] configuration.ssid=" + q(newSSID[n]) + "\n")
			undo.WriteString(try("/interface wifi set [find name=" + q(n) + "] configuration.ssid=" + q(x.SSID)))
		}
		done = map[string]bool{}
		for _, n := range names(newSec) {
			if done[n] {
				continue
			}
			x := s.find(n)
			if x.Profile != "" {
				users := usersOf(func(y *wlanIfc) string { return y.Profile }, x.Profile)
				if v, ok := allSame(users, newSec); ok {
					for _, u := range users {
						done[u] = true
						touched[u] = true
					}
					sec := s.secs[x.Profile]
					auth, pass, _ := strings.Cut(v, "\x00")
					p.Steps = append(p.Steps, fmt.Sprintf("Sicherheitsprofil %q: %s (gilt für %s)", x.Profile, secDesc(x, v), strings.Join(users, ", ")))
					apply.WriteString("/interface wifi security set [find name=" + q(x.Profile) + "] authentication-types=" + auth + " passphrase=" + q(pass) + "\n")
					undo.WriteString(try("/interface wifi security set [find name=" + q(x.Profile) + "] passphrase=" + q(sec["passphrase"]) +
						" authentication-types=" + q(sec["authentication-types"])))
					continue
				}
			}
			done[n] = true
			touched[n] = true
			auth, pass, _ := strings.Cut(newSec[n], "\x00")
			p.Steps = append(p.Steps, fmt.Sprintf("%s: %s (direkt an der Schnittstelle)", n, secDesc(x, newSec[n])))
			apply.WriteString("/interface wifi set [find name=" + q(n) + "] security.authentication-types=" + auth + " security.passphrase=" + q(pass) + "\n")
			undo.WriteString(try("/interface wifi set [find name=" + q(n) + "] security.passphrase=" + q(x.Passphrase) +
				" security.authentication-types=" + q(x.Auth)))
		}
		for _, n := range setDis {
			x := s.find(n)
			p.Steps = append(p.Steps, fmt.Sprintf("%s (%q) %s", n, x.SSID, onOff(!x.Disabled)))
			apply.WriteString("/interface wifi set [find name=" + q(n) + "] disabled=" + yesNo(!x.Disabled) + "\n")
			undo.WriteString(try("/interface wifi set [find name=" + q(n) + "] disabled=" + yesNo(x.Disabled)))
			touched[n] = true
		}
	}

	for _, c := range extra {
		p.Steps = append(p.Steps, c.step)
		apply.WriteString(c.apply)
		undo.WriteString(c.undo)
	}
	// Funkteil geändert → auch seine virtuellen WLANs starten neu
	for _, x := range s.ifs {
		if radioTouched[x.Name] || (x.Master != "" && radioTouched[x.Master]) {
			touched[x.Name] = true
		}
	}

	clients := 0
	for _, x := range s.ifs {
		if touched[x.Name] {
			clients += x.Clients
		}
		if wants[x.Name] == nil && touched[x.Name] && !radioTouched[x.Name] && !radioTouched[x.Master] {
			p.Warn = append(p.Warn, fmt.Sprintf("%s ist über das gemeinsame Profil mitbetroffen", x.Name))
		}
	}
	if clients > 0 {
		p.Warn = append(p.Warn, fmt.Sprintf("%d verbundene WLAN-Geräte werden kurz getrennt und verbinden sich neu (bei neuem Passwort/SSID nur mit den neuen Daten).", clients))
	}
	p.Apply, p.Undo = apply.String(), undo.String()+undoLast.String()
	return p, nil
}

// legacySecArgs: Verschlüsselung + Passwort für ein wireless-Sicherheitsprofil.
func legacySecArgs(auth, psk string) string {
	args := "mode=dynamic-keys authentication-types=" + auth + " wpa2-pre-shared-key=" + q(psk)
	if strings.Contains(","+auth, ",wpa-psk") {
		args += " wpa-pre-shared-key=" + q(psk)
	}
	return args
}
