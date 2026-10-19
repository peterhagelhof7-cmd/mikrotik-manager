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
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Umstellung vom alten wireless-Paket auf das neue wifi-Paket (wifi-qcom-ac).
//
// Ablauf (zwei Neustarts, ca. 5 Minuten ohne WLAN):
//  1. WLAN-Konfiguration lesen und übersetzen, Backup auf den Manager
//  2. wireless deinstallieren → Neustart (macht Platz im 16-MB-Flash)
//  3. wifi-qcom-ac aus dem Internet installieren → Neustart
//     scheitert das: wireless wieder installieren und das Backup einspielen
//  4. übersetzte Konfiguration einspielen (auch als .rsc auf dem Manager
//     abgelegt), Bridge-Ports/Schnittstellenlisten übernehmen, prüfen
//
// wifi-qcom-ac unterstützt nur 802.11ac-Wave-2-Funkchips; ältere (z. B.
// AR9888/AR9300 im hAP ac lite) bleiben auf wireless.

const wifiPkg = "wifi-qcom-ac"

var wifiChips = map[string]bool{"IPQ4018": true, "IPQ4019": true, "QCA9984": true, "QCA9888": true}

// flashReserve bleibt nach der Umstellung mindestens frei.
const flashReserve = 64 << 10

// legacyIf ist eine wireless-Schnittstelle in übersetzbarer Form.
type legacyIf struct {
	Name, MAC, Master string
	SSID, Auth, PSK   string
	Band, Freq, Width string // wifi-Werte, leer = Standard
	Country, Install  string
	Hide, Disabled    bool
	Bridge, PVID      string
	Lists             []string
	// wireless vlan-mode=use-tag: wifi-qcom-ac kann nicht selbst taggen. Das
	// VLAN läuft danach über eine VLAN-Schnittstelle auf VLANParent (der bisherigen
	// Bridge) und eine eigene Bridge (Bridge = vlanBridgeName(VLAN)).
	VLAN, VLANParent string `json:",omitempty"`
}

func vlanIfName(vid string) string     { return "vlan" + vid }
func vlanBridgeName(vid string) string { return "bridge-vlan" + vid }

// checkUseTag prüft, ob sich vlan-mode=use-tag der Schnittstelle n sicher
// übersetzen lässt; Ergebnis "" = ja, sonst der Sperrgrund.
// bp ist ihr Bridge-Port, bridges/vlans die Bridges bzw. VLAN-Schnittstellen
// des Geräts, names alle Schnittstellennamen.
func checkUseTag(n, vid string, bp map[string]string, bridges map[string]map[string]string,
	vlans []map[string]string, names map[string]bool) string {
	v, err := strconv.Atoi(vid)
	if err != nil || v < 2 || v > 4094 {
		return fmt.Sprintf("%s: VLAN-ID %q", n, vid)
	}
	if bp == nil || bp["bridge"] == "" {
		return n + ": VLAN-Modus use-tag ohne Bridge"
	}
	parent := bp["bridge"]
	if bridges[parent]["vlan-filtering"] == "true" {
		return fmt.Sprintf("%s: VLAN-Modus use-tag an Bridge %s mit VLAN-Filter", n, parent)
	}
	for _, m := range vlans {
		if m["vlan-id"] == vid {
			return fmt.Sprintf("%s: VLAN %s gibt es schon als Schnittstelle %s", n, vid, m["name"])
		}
	}
	for _, x := range []string{vlanIfName(vid), vlanBridgeName(vid)} {
		if names[x] {
			return fmt.Sprintf("%s: Name %s ist schon vergeben", n, x)
		}
	}
	return ""
}

type migration struct {
	ifs    []legacyIf
	resume string // "" = neu | install | config | restore
	backup string // .backup-Datei auf dem Manager (Rückweg)
}

// migState ist der Fortschritt einer Umstellung, gespeichert auf dem Manager.
// Bricht die Umstellung ab (z. B. Neustart des Managers, fehlender Download),
// kann sie damit fortgesetzt werden – die wireless-Konfiguration gibt es auf
// dem Gerät dann nicht mehr. Enthält WLAN-Passwörter → 0600.
type migState struct {
	Ifs     []legacyIf `json:"ifs"`
	Backup  string     `json:"backup"`
	Started time.Time  `json:"started"`
}

func (p *Poller) migStatePath(id string) string {
	return filepath.Join(p.cfg.DataDir, "migrations", filepath.Base(p.backupDir(id))+".json")
}

func (p *Poller) loadMigState(id string) *migState {
	b, err := os.ReadFile(p.migStatePath(id))
	if err != nil {
		return nil
	}
	var s migState
	if json.Unmarshal(b, &s) != nil || len(s.Ifs) == 0 {
		return nil
	}
	return &s
}

func (p *Poller) saveMigState(id string, s migState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(p.migStatePath(id)), 0o700)
	return writeFileAtomic(p.migStatePath(id), b, 0o600)
}

func (p *Poller) clearMigState(id string) { os.Remove(p.migStatePath(id)) }

// lastMigBackup ist das neueste Backup „vor-wifi“ auf dem Manager.
func (p *Poller) lastMigBackup(id string) string {
	for _, b := range p.ListBackups(id) { // neueste zuerst
		if strings.HasSuffix(b.Name, "-vor-wifi.backup") {
			return filepath.Join(p.backupDir(id), b.Name)
		}
	}
	return ""
}

func migIfSteps(ifs []legacyIf) []string {
	var out []string
	for _, x := range ifs {
		where := "Funkteil " + bandName(x.Band)
		if x.Master != "" {
			where = "virtuell an " + x.Master
		}
		br := "an keiner Bridge"
		if x.VLAN != "" {
			br = fmt.Sprintf("an %s (VLAN %s getaggt über %s)", x.Bridge, x.VLAN, x.VLANParent)
		} else if x.Bridge != "" {
			br = "an " + x.Bridge
		}
		state := ""
		if x.Disabled {
			state = ", ausgeschaltet"
		}
		out = append(out, fmt.Sprintf("%s (%s) → SSID %q, %s, %s%s", x.Name, where, x.SSID, authLabel(x.Auth), br, state))
	}
	return out
}

// planMigrate erkennt auch eine unterbrochene Umstellung und plant ihre Fortsetzung.
func (p *Poller) planMigrate(ctx context.Context, cl *routeros.Client, id string, checkUpdates bool) (Plan, error) {
	pk, err := readPackages(ctx, cl)
	if err != nil {
		return Plan{Action: "wifimigrate"}, err
	}
	wifiOn := pkgInstalled(pk[wifiPkg]) || pkgInstalled(pk["wifi-qcom"])
	st := p.loadMigState(id)
	plan := Plan{Action: "wifimigrate"}
	warn := "Unterbrochene Umstellung gefunden (begonnen %s)."
	switch {
	case wifiOn && st != nil:
		plan.mig = &migration{ifs: st.Ifs, resume: "config", backup: st.Backup}
		plan.Warn = []string{fmt.Sprintf(warn, st.Started.Format("02.01. 15:04"))}
		plan.Steps = append([]string{wifiPkg + " ist bereits installiert"}, migIfSteps(st.Ifs)...)
		plan.Steps = append(plan.Steps, "Konfiguration einspielen und prüfen")
		return plan, nil
	case !wifiOn && !pkgInstalled(pk["wireless"]) && st != nil:
		plan.mig = &migration{ifs: st.Ifs, resume: "install", backup: st.Backup}
		plan.Warn = []string{fmt.Sprintf(warn, st.Started.Format("02.01. 15:04")) + " Das Gerät hat gerade kein WLAN-Paket."}
		plan.Steps = append([]string{fmt.Sprintf("Paket %s aus dem Internet installieren – Neustart", wifiPkg)}, migIfSteps(st.Ifs)...)
		plan.Steps = append(plan.Steps, "Konfiguration einspielen und prüfen",
			"scheitert die Paket-Installation, wird wireless wieder installiert und das Backup eingespielt")
		return plan, nil
	case !wifiOn && !pkgInstalled(pk["wireless"]):
		if b := p.lastMigBackup(id); b != "" {
			plan.mig = &migration{resume: "restore", backup: b}
			plan.Warn = []string{"Das Gerät hat gerade kein WLAN-Paket – eine frühere Umstellung wurde unterbrochen, ihr Fortschritt ist nicht gespeichert."}
			plan.Steps = []string{
				"Paket wireless wieder installieren – Neustart",
				fmt.Sprintf("Backup %s einspielen (Zustand vor der Umstellung) – Neustart", filepath.Base(b)),
				"danach die Umstellung neu planen und durchführen (Backup, wireless entfernen, wifi-qcom-ac installieren, Konfiguration einspielen – zwei weitere Neustarts)",
			}
			return plan, nil
		}
	}
	return planMigrateFresh(ctx, cl, checkUpdates)
}

func readPackages(ctx context.Context, cl *routeros.Client) (map[string]map[string]string, error) {
	l, err := printAll(ctx, cl, "/system/package/print")
	if err != nil {
		return nil, err
	}
	m := map[string]map[string]string{}
	for _, x := range l {
		m[x["name"]] = x
	}
	return m, nil
}

func pkgInstalled(m map[string]string) bool {
	return m != nil && m["available"] != "true" && m["disabled"] != "true"
}

// wifiBand übersetzt das wireless-Band ("2ghz-b/g/n", "5ghz-a/n/ac") ins wifi-Paket.
func wifiBand(b string) string {
	switch {
	case strings.HasPrefix(b, "2ghz") && strings.Contains(b, "n"):
		return "2ghz-n"
	case strings.HasPrefix(b, "2ghz") && strings.Contains(b, "g"):
		return "2ghz-g"
	case strings.HasPrefix(b, "5ghz") && strings.Contains(b, "ac"):
		return "5ghz-ac"
	case strings.HasPrefix(b, "5ghz") && strings.Contains(b, "n"):
		return "5ghz-n"
	case strings.HasPrefix(b, "5ghz"):
		return "5ghz-a"
	}
	return ""
}

// wifiWidth: "20/40mhz-XX" → "20/40mhz", "20/40/80mhz-XXXX" → "20/40/80mhz".
func wifiWidth(w string) string {
	if i := strings.Index(w, "mhz"); i > 0 {
		return w[:i+3]
	}
	return ""
}

// wifiCountry: wireless schreibt Länder klein ("germany"), wifi groß ("Germany").
func wifiCountry(c string) string {
	if c == "" || c == "no_country_set" || c == "etsi" {
		return ""
	}
	f := strings.Fields(c)
	for i, w := range f {
		f[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(f, " ")
}

func authLabel(a string) string {
	if a == "" {
		return "offen (ohne Passwort)"
	}
	return strings.ToUpper(a)
}

// planMigrateFresh prüft, ob und wie das Gerät umgestellt werden kann.
// checkUpdates: Paketliste notfalls vom MikroTik-Server holen (dauert einige Sekunden).
func planMigrateFresh(ctx context.Context, cl *routeros.Client, checkUpdates bool) (Plan, error) {
	p := Plan{Action: "wifimigrate"}
	pk, err := readPackages(ctx, cl)
	if err != nil {
		return p, err
	}
	if pkgInstalled(pk[wifiPkg]) || pkgInstalled(pk["wifi-qcom"]) {
		p.Noop = true
		p.Steps = []string{"Das Gerät verwendet bereits das neue wifi-Paket – nichts zu tun"}
		return p, nil
	}
	if !pkgInstalled(pk["wireless"]) {
		p.Noop = true
		p.Steps = []string{"Das Gerät hat kein wireless-Paket – nichts zu tun"}
		return p, nil
	}

	l, err := printAll(ctx, cl, "/interface/wireless/print")
	if err != nil {
		return p, err
	}
	var bad []string
	for _, m := range l {
		if masterOf(m) == "" && !wifiChips[m["interface-type"]] {
			bad = append(bad, m["name"]+": "+m["interface-type"])
		}
	}
	if len(l) == 0 {
		p.Blocked = "Keine WLAN-Schnittstellen gefunden."
		return p, nil
	}
	if len(bad) > 0 {
		p.Blocked = "Funkchip wird vom neuen wifi-Paket nicht unterstützt (" + strings.Join(bad, ", ") + ") – das Gerät bleibt auf wireless."
		return p, nil
	}
	if c, err := printAll(ctx, cl, "/interface/wireless/cap/print"); err == nil && len(c) > 0 && c[0]["enabled"] == "true" {
		p.Blocked = "Das WLAN wird per CAPsMAN verwaltet – Umstellung dort vornehmen."
		return p, nil
	}

	if pk[wifiPkg] == nil && checkUpdates {
		runIgnoringDisconnect(ctx, cl, 60*time.Second, "/system/package/update/check-for-updates")
		if pk, err = readPackages(ctx, cl); err != nil {
			return p, err
		}
	}
	if pk[wifiPkg] == nil {
		p.Blocked = "Das Paket " + wifiPkg + " wird für dieses Gerät nicht angeboten (keine Internetverbindung zum MikroTik-Server?)."
		return p, nil
	}

	// Platz im Flash: wireless geht, wifi-qcom-ac kommt.
	res, err := printAll(ctx, cl, "/system/resource/print", "=.proplist=free-hdd-space")
	if err != nil || len(res) == 0 {
		return p, fmt.Errorf("Systemressourcen lesen: %v", err)
	}
	free, _ := strconv.ParseInt(res[0]["free-hdd-space"], 10, 64)
	oldSize, _ := strconv.ParseInt(pk["wireless"]["size"], 10, 64)
	newSize, _ := strconv.ParseInt(pk[wifiPkg]["size"], 10, 64)
	left := free + oldSize - newSize
	if newSize == 0 || left < flashReserve {
		p.Blocked = fmt.Sprintf("Zu wenig Flash: frei %d KB + wireless %d KB, %s braucht %d KB (mind. %d KB müssen frei bleiben). Alte Dateien auf dem Gerät löschen.",
			free>>10, oldSize>>10, wifiPkg, newSize>>10, flashReserve>>10)
		return p, nil
	}

	mig, warn, blocked, err := readLegacy(ctx, cl, l)
	if err != nil {
		return p, err
	}
	p.mig = mig
	p.Warn = warn
	if blocked != "" {
		p.Blocked = blocked
		return p, nil
	}

	p.Steps = append(p.Steps,
		"Backup auf den Manager",
		"Paket wireless deinstallieren – Neustart",
		fmt.Sprintf("Paket %s aus dem Internet installieren (%d KB) – Neustart", wifiPkg, newSize>>10))
	p.Steps = append(p.Steps, migIfSteps(mig.ifs)...)
	p.Steps = append(p.Steps,
		"Konfiguration einspielen und prüfen (das Skript liegt danach auch als .rsc bei den Backups)",
		"scheitert die Paket-Installation, wird wireless wieder installiert und das Backup eingespielt")

	clients := 0
	if c, err := printAll(ctx, cl, "/interface/wireless/registration-table/print", "=.proplist=interface"); err == nil {
		clients = len(c)
	}
	p.Warn = append(p.Warn,
		fmt.Sprintf("Das WLAN dieses Geräts ist ca. 5 Minuten aus (2 Neustarts). %d verbundene WLAN-Geräte werden getrennt und wechseln, wenn möglich, zu einem anderen Access Point mit derselben SSID.", clients),
		"Das Gerät braucht Internetzugang zum Herunterladen des Pakets.",
		fmt.Sprintf("Danach sind noch ca. %d KB Flash frei.", left>>10))
	return p, nil
}

// readLegacy übersetzt die wireless-Konfiguration; blocked nennt Einstellungen,
// die sich nicht sicher automatisch übernehmen lassen.
func readLegacy(ctx context.Context, cl *routeros.Client, l []map[string]string) (*migration, []string, string, error) {
	profs, err := printAll(ctx, cl, "/interface/wireless/security-profiles/print")
	if err != nil {
		return nil, nil, "", err
	}
	sec := map[string]map[string]string{}
	for _, m := range profs {
		sec[m["name"]] = m
	}
	ports := map[string]map[string]string{}
	if pl, err := printAll(ctx, cl, "/interface/bridge/port/print"); err == nil {
		for _, m := range pl {
			ports[m["interface"]] = m
		}
	}
	bridges := map[string]map[string]string{}
	if bl, err := printAll(ctx, cl, "/interface/bridge/print", "=.proplist=name,vlan-filtering"); err == nil {
		for _, m := range bl {
			bridges[m["name"]] = m
		}
	}
	vlans, _ := printAll(ctx, cl, "/interface/vlan/print", "=.proplist=name,vlan-id,interface")
	names := map[string]bool{}
	if il, err := printAll(ctx, cl, "/interface/print", "=.proplist=name"); err == nil {
		for _, m := range il {
			names[m["name"]] = true
		}
	}
	lists := map[string][]string{}
	if ml, err := printAll(ctx, cl, "/interface/list/member/print"); err == nil {
		for _, m := range ml {
			lists[m["interface"]] = append(lists[m["interface"]], m["list"])
		}
	}
	var warn, blocked []string
	for _, menu := range []string{"access-list", "connect-list"} {
		if c, err := printAll(ctx, cl, "/interface/wireless/"+menu+"/print"); err == nil && len(c) > 0 {
			warn = append(warn, fmt.Sprintf("%d Einträge in der wireless %s werden NICHT übernommen.", len(c), menu))
		}
	}

	mig := &migration{}
	byName := map[string]map[string]string{}
	for _, m := range l {
		byName[m["name"]] = m
	}
	for _, m := range l {
		n := m["name"]
		x := legacyIf{Name: n, MAC: strings.ToUpper(m["mac-address"]), Master: masterOf(m), SSID: m["ssid"],
			Hide: m["hide-ssid"] == "true", Disabled: m["disabled"] == "true"}
		if mode := m["mode"]; mode != "ap-bridge" && mode != "bridge" {
			blocked = append(blocked, fmt.Sprintf("%s: Modus %q (nur Access-Point wird übernommen)", n, mode))
		}
		if w := m["wds-mode"]; w != "" && w != "disabled" {
			blocked = append(blocked, fmt.Sprintf("%s: WDS (%s)", n, w))
		}
		switch v := m["vlan-mode"]; v {
		case "", "no-tag":
		case "use-tag":
			if b := checkUseTag(n, m["vlan-id"], ports[n], bridges, vlans, names); b != "" {
				blocked = append(blocked, b)
			} else {
				x.VLAN, x.VLANParent = m["vlan-id"], ports[n]["bridge"]
			}
		default:
			blocked = append(blocked, fmt.Sprintf("%s: VLAN-Modus %s", n, v))
		}
		p := sec[m["security-profile"]]
		switch p["mode"] {
		case "none":
		case "dynamic-keys":
			for _, a := range strings.Split(p["authentication-types"], ",") {
				if a != "" && a != "wpa-psk" && a != "wpa2-psk" {
					blocked = append(blocked, fmt.Sprintf("%s: Anmeldung %s (EAP/RADIUS)", n, a))
				}
			}
			if p["radius-mac-authentication"] == "true" {
				blocked = append(blocked, n+": RADIUS-MAC-Anmeldung")
			}
		default:
			blocked = append(blocked, fmt.Sprintf("%s: Sicherheitsmodus %q (WEP)", n, p["mode"]))
		}
		x.Auth, x.PSK = legacyAuth(p)
		if x.Auth != "" && x.PSK == "" {
			blocked = append(blocked, n+": WPA ohne Schlüssel")
		}
		// Funkwerte vom Funkteil (bei virtuellen vom Master)
		r := m
		if x.Master != "" {
			r = byName[x.Master]
		}
		x.Band, x.Width = wifiBand(r["band"]), wifiWidth(r["channel-width"])
		if f := r["frequency"]; f != "" && f != "auto" {
			x.Freq = f
		}
		x.Country, x.Install = wifiCountry(r["country"]), r["installation"]
		if x.Install == "any" {
			x.Install = ""
		}
		if bp := ports[n]; x.VLAN != "" {
			x.Bridge = vlanBridgeName(x.VLAN)
		} else if bp != nil {
			x.Bridge = bp["bridge"]
			if bp["pvid"] != "" && bp["pvid"] != "1" {
				x.PVID = bp["pvid"]
			}
		}
		x.Lists = lists[n]
		mig.ifs = append(mig.ifs, x)
	}
	// Ein VLAN darf nur an einer bisherigen Bridge hängen (Namen vlanN/bridge-vlanN).
	parentOf := map[string]string{}
	for _, x := range mig.ifs {
		if x.VLAN == "" {
			continue
		}
		if p, ok := parentOf[x.VLAN]; ok && p != x.VLANParent {
			blocked = append(blocked, fmt.Sprintf("VLAN %s hängt an zwei Bridges (%s, %s)", x.VLAN, p, x.VLANParent))
		} else if !ok {
			parentOf[x.VLAN] = x.VLANParent
			warn = append(warn, fmt.Sprintf("VLAN %s: %s kann nicht an der WLAN-Schnittstelle taggen. Der Manager legt die VLAN-Schnittstelle %s auf %s und die Bridge %s an, die WLANs mit VLAN %s kommen dorthin. %s selbst bleibt unverändert (kein VLAN-Filter).",
				x.VLAN, wifiPkg, vlanIfName(x.VLAN), x.VLANParent, vlanBridgeName(x.VLAN), x.VLAN, x.VLANParent))
		}
	}
	// Funkteile vor virtuellen anlegen
	sort.SliceStable(mig.ifs, func(i, j int) bool { return mig.ifs[i].Master == "" && mig.ifs[j].Master != "" })
	return mig, warn, strings.Join(blocked, "; "), nil
}

// wifiRadio ist eine Schnittstelle des neuen wifi-Pakets nach der Installation.
type wifiRadio struct {
	Name, MAC, Bands string
}

// wifiScript erzeugt die Konfiguration für das wifi-Paket. radios sind die
// physischen wifi-Schnittstellen; Zuordnung per MAC, sonst per Band.
func wifiScript(mig *migration, radios []wifiRadio) (string, map[string]string, error) {
	newName := map[string]string{} // wireless-Name → wifi-Name
	used := map[string]bool{}
	for _, x := range mig.ifs {
		if x.Master != "" {
			continue
		}
		for _, r := range radios {
			if !used[r.Name] && x.MAC != "" && strings.EqualFold(r.MAC, x.MAC) {
				newName[x.Name], used[r.Name] = r.Name, true
			}
		}
	}
	for _, x := range mig.ifs {
		if x.Master != "" || newName[x.Name] != "" {
			continue
		}
		if len(x.Band) < 4 {
			return "", nil, fmt.Errorf("Band von %s unbekannt", x.Name)
		}
		want := x.Band[:4] // "2ghz" / "5ghz"
		for _, r := range radios {
			if !used[r.Name] && strings.Contains(r.Bands, want) {
				newName[x.Name], used[r.Name] = r.Name, true
				break
			}
		}
		if newName[x.Name] == "" {
			return "", nil, fmt.Errorf("kein passendes neues Funkteil für %s (%s)", x.Name, x.Band)
		}
	}

	var b strings.Builder
	b.WriteString("# mikrotik-manager: WLAN-Konfiguration nach Umstellung wireless -> wifi\n")
	// Sicherheitsprofile je (Anmeldung, Passwort), Konfigurationsprofile je Netz
	secName := map[string]string{}
	cfgName := map[string]string{}
	var secs, cfgs strings.Builder
	for _, x := range mig.ifs {
		sk := x.Auth + "\x00" + x.PSK
		if x.Auth != "" && secName[sk] == "" {
			secName[sk] = fmt.Sprintf("sec%d", len(secName)+1)
			secs.WriteString("add name=" + secName[sk] + " authentication-types=" + x.Auth + " passphrase=" + q(x.PSK) + "\n")
		}
		ck := strings.Join([]string{x.SSID, secName[sk], x.Country, x.Install, strconv.FormatBool(x.Hide)}, "\x00")
		if cfgName[ck] == "" {
			cfgName[ck] = fmt.Sprintf("cfg%d", len(cfgName)+1)
			line := "add name=" + cfgName[ck] + " mode=ap ssid=" + q(x.SSID)
			if s := secName[sk]; s != "" {
				line += " security=" + s
			}
			if x.Country != "" {
				line += " country=" + q(x.Country)
			}
			if x.Install != "" {
				line += " installation=" + x.Install
			}
			if x.Hide {
				line += " hide-ssid=yes"
			}
			cfgs.WriteString(line + "\n")
		}
	}
	if secs.Len() > 0 {
		b.WriteString("/interface wifi security\n" + secs.String())
	}
	b.WriteString("/interface wifi configuration\n" + cfgs.String())

	b.WriteString("/interface wifi\n")
	taken := map[string]bool{}
	for _, r := range radios {
		taken[r.Name] = true
	}
	for _, x := range mig.ifs {
		cfg := cfgName[strings.Join([]string{x.SSID, secName[x.Auth+"\x00"+x.PSK], x.Country, x.Install, strconv.FormatBool(x.Hide)}, "\x00")]
		if x.Master == "" {
			line := "set [find name=" + q(newName[x.Name]) + "] configuration=" + cfg + " disabled=" + yesNo(x.Disabled)
			if x.Band != "" {
				line += " channel.band=" + x.Band
			}
			if x.Width != "" {
				line += " channel.width=" + x.Width
			}
			if x.Freq != "" {
				line += " channel.frequency=" + x.Freq
			}
			b.WriteString(line + "\n")
			continue
		}
		n := x.Name
		for taken[n] {
			n += "-v"
		}
		taken[n] = true
		newName[x.Name] = n
		b.WriteString("add name=" + q(n) + " master-interface=" + q(newName[x.Master]) + " configuration=" + cfg + " disabled=" + yesNo(x.Disabled) + "\n")
	}

	// VLAN-Ersatz für use-tag: VLAN-Schnittstelle auf der bisherigen Bridge und
	// eine eigene Bridge. Mit :if, damit erneutes Einspielen nichts doppelt anlegt.
	done := map[string]bool{}
	for _, x := range mig.ifs {
		if x.VLAN == "" || done[x.VLAN] {
			continue
		}
		done[x.VLAN] = true
		vi, vb := q(vlanIfName(x.VLAN)), q(vlanBridgeName(x.VLAN))
		b.WriteString(":if ([:len [/interface bridge find name=" + vb + "]] = 0) do={ /interface bridge add name=" + vb + " comment=" + q(mmComment) + " }\n")
		b.WriteString(":if ([:len [/interface vlan find name=" + vi + "]] = 0) do={ /interface vlan add name=" + vi + " interface=" + q(x.VLANParent) + " vlan-id=" + x.VLAN + " comment=" + q(mmComment) + " }\n")
		b.WriteString(":if ([:len [/interface bridge port find interface=" + vi + "]] = 0) do={ /interface bridge port add bridge=" + vb + " interface=" + vi + " }\n")
	}

	var ports, members strings.Builder
	for _, x := range mig.ifs {
		if x.Bridge != "" {
			line := "add bridge=" + q(x.Bridge) + " interface=" + q(newName[x.Name])
			if x.PVID != "" {
				line += " pvid=" + x.PVID
			}
			ports.WriteString(line + "\n")
		}
		for _, l := range x.Lists {
			members.WriteString("add list=" + q(l) + " interface=" + q(newName[x.Name]) + "\n")
		}
	}
	if ports.Len() > 0 {
		b.WriteString("/interface bridge port\n" + ports.String())
	}
	if members.Len() > 0 {
		b.WriteString("/interface list member\n" + members.String())
	}
	return b.String(), newName, nil
}

// --- Ausführung ---

func (p *Poller) doMigrate(ctx context.Context, id string, x *Job) error {
	d, _ := p.st.Get(id)
	name := d.Identity
	cl, err := p.session(ctx, id)
	if err != nil {
		return err
	}
	defer func() {
		if cl != nil {
			cl.Close()
		}
	}()

	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	plan, err := p.planMigrate(rctx, cl, id, true)
	cancel()
	if err != nil {
		return err
	}
	if plan.Blocked != "" {
		return fmt.Errorf("nicht ausgeführt: %s", plan.Blocked)
	}
	if plan.Noop {
		p.jobs.step(x, name, "%s", strings.Join(plan.Steps, "; "))
		return nil
	}
	mig := plan.mig

	// Unterbrochene Umstellung ohne gespeicherten Fortschritt: erst den Zustand
	// vor der Umstellung herstellen, dann neu beginnen.
	if mig.resume == "restore" {
		p.jobs.step(x, name, "unterbrochene Umstellung – stelle zuerst den Zustand davor her (Backup %s) …", filepath.Base(mig.backup))
		cl.Close()
		cl = nil
		if err := p.migrateFallback(ctx, id, x, name, mig.backup); err != nil {
			return fmt.Errorf("Wiederherstellung: %w – Backup %s liegt auf dem Manager", err, filepath.Base(mig.backup))
		}
		if cl, err = p.session(ctx, id); err != nil {
			return err
		}
		rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		plan, err = p.planMigrate(rctx, cl, id, true)
		cancel()
		if err != nil {
			return err
		}
		if plan.Blocked != "" || plan.Noop || plan.mig.resume != "" {
			return fmt.Errorf("nach der Wiederherstellung kein Neustart der Umstellung möglich: %s%s", plan.Blocked, strings.Join(plan.Steps, "; "))
		}
		mig = plan.mig
		p.jobs.step(x, name, "starte die Umstellung neu")
	}
	if mig.resume != "" {
		p.jobs.step(x, name, "setze unterbrochene Umstellung fort (%s)", mig.resume)
	}
	backupFile := mig.backup

	if mig.resume == "" {
		p.jobs.step(x, name, "erstelle Backup und lade es auf den Manager …")
		bk, f, err := p.backupToManager(ctx, id, cl, "vor-wifi")
		if err != nil {
			return fmt.Errorf("Backup: %w – nichts geändert", err)
		}
		backupFile = f
		p.jobs.step(x, name, "Backup gespeichert: %s", bk)
		// Ab hier gibt es die wireless-Konfiguration bald nur noch auf dem Manager.
		if err := p.saveMigState(id, migState{Ifs: mig.ifs, Backup: backupFile, Started: time.Now()}); err != nil {
			return fmt.Errorf("Fortschritt speichern: %w – nichts geändert", err)
		}

		// Stufe 1: wireless deinstallieren
		p.jobs.step(x, name, "deinstalliere wireless – Neustart (WLAN ist jetzt aus) …")
		if cl, err = p.pkgChange(ctx, id, cl, x, name, "uninstall", "wireless", 3*time.Minute); err != nil {
			p.clearMigState(id)
			return err
		}
		pk, err := readPackagesT(ctx, cl)
		if err != nil {
			return err
		}
		if pkgInstalled(pk["wireless"]) {
			p.clearMigState(id)
			return fmt.Errorf("wireless ist nach dem Neustart noch installiert – sonst nichts geändert, WLAN läuft weiter")
		}
		p.jobs.step(x, name, "wireless entfernt")
	}

	if mig.resume == "" || mig.resume == "install" {
		// Stufe 2: wifi-qcom-ac installieren
		p.jobs.step(x, name, "installiere %s (Download aus dem Internet) – Neustart …", wifiPkg)
		ncl, err := p.pkgChange(ctx, id, cl, x, name, "enable", wifiPkg, 10*time.Minute)
		if err == nil {
			cl = ncl
			var pk map[string]map[string]string
			pk, err = readPackagesT(ctx, cl)
			if err == nil && !pkgInstalled(pk[wifiPkg]) {
				err = fmt.Errorf("%s ist nach dem Neustart nicht installiert", wifiPkg)
			}
		}
		if err != nil {
			p.jobs.step(x, name, "Installation fehlgeschlagen (%v) – stelle alten Zustand wieder her …", err)
			if cl != nil {
				cl.Close()
				cl = nil
			}
			if ferr := p.migrateFallback(ctx, id, x, name, backupFile); ferr != nil {
				return fmt.Errorf("Umstellung fehlgeschlagen (%v) und Wiederherstellung fehlgeschlagen (%v) – Fortschritt gespeichert, erneut ausführen setzt fort; Backup %s liegt auf dem Manager", err, ferr, filepath.Base(backupFile))
			}
			p.clearMigState(id)
			return fmt.Errorf("Umstellung fehlgeschlagen (%v) – alter Zustand mit wireless wiederhergestellt", err)
		}
		p.jobs.step(x, name, "%s installiert", wifiPkg)
	}

	// Stufe 3: Konfiguration
	if err := p.applyWifiConfig(ctx, id, cl, x, name, mig); err != nil {
		return fmt.Errorf("%w – das Gerät läuft mit %s; erneut ausführen spielt die Konfiguration nochmals ein, Skript liegt auch als …-wifi-config.rsc bei den Backups", err, wifiPkg)
	}
	p.clearMigState(id)
	p.Trigger(id)
	return nil
}

func readPackagesT(ctx context.Context, cl *routeros.Client) (map[string]map[string]string, error) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return readPackages(rctx, cl)
}

// pkgChange markiert ein Paket (uninstall/enable), wendet die Änderung an
// (Neustart, ggf. Download) und liefert eine neue Verbindung nach dem Neustart.
func (p *Poller) pkgChange(ctx context.Context, id string, cl *routeros.Client, x *Job, name, op, pkg string, timeout time.Duration) (*routeros.Client, error) {
	pk, err := readPackagesT(ctx, cl)
	if err == nil && pk[pkg] == nil && op == "enable" {
		// Nach einem Neustart kennt RouterOS die installierbaren Pakete erst
		// wieder nach einer Update-Prüfung; die Liste füllt sich danach verzögert.
		p.jobs.step(x, name, "%s fehlt in der Paketliste – frage den MikroTik-Server …", pkg)
		runIgnoringDisconnect(ctx, cl, 90*time.Second, "/system/package/update/check-for-updates")
		for i := 0; i < 15 && err == nil && pk[pkg] == nil; i++ {
			time.Sleep(2 * time.Second)
			pk, err = readPackagesT(ctx, cl)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	var s devState
	if err == nil {
		s, err = readState(rctx, cl)
	}
	ref := pkg
	if err == nil {
		if pk[pkg] == nil {
			err = fmt.Errorf("Paket nicht in der Paketliste (Internetverbindung zum MikroTik-Server?)")
		} else if id := pk[pkg][".id"]; id != "" {
			ref = id
		}
	}
	if err == nil {
		_, err = cl.RunContext(rctx, "/system/package/"+op, "=numbers="+ref)
	}
	cancel()
	if err != nil {
		return cl, fmt.Errorf("%s %s: %w", op, pkg, err)
	}
	if err := runIgnoringDisconnect(ctx, cl, timeout, "/system/package/apply-changes"); err != nil {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cl.RunContext(rctx, "/system/package/unschedule", "=numbers="+ref)
		cancel()
		return cl, fmt.Errorf("Änderung anwenden: %w", err)
	}
	cl.Close()
	if _, err := p.waitReboot(ctx, id, s.Uptime, timeout+5*time.Minute, x, name); err != nil {
		return nil, err
	}
	return p.session(ctx, id)
}

// migrateFallback: wireless wieder installieren und das Backup einspielen.
func (p *Poller) migrateFallback(ctx context.Context, id string, x *Job, name, backupFile string) error {
	cl, err := p.session(ctx, id)
	if err != nil {
		return err
	}
	pk, err := readPackagesT(ctx, cl)
	if err != nil {
		cl.Close()
		return err
	}
	if !pkgInstalled(pk["wireless"]) {
		p.jobs.step(x, name, "installiere wireless wieder – Neustart …")
		if cl, err = p.pkgChange(ctx, id, cl, x, name, "enable", "wireless", 10*time.Minute); err != nil {
			return err
		}
	}
	defer func() { cl.Close() }()

	p.jobs.step(x, name, "spiele Backup %s ein – Neustart …", filepath.Base(backupFile))
	remote := "mm-restore.backup"
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if r, err := cl.RunContext(rctx, "/file/print", "?name=flash", "=.proplist=name"); err == nil && len(r.Re) > 0 {
		remote = "flash/" + remote
	}
	s, err := readState(rctx, cl)
	cancel()
	if err != nil {
		return err
	}
	sc, err := p.sshDial(ctx, id)
	if err != nil {
		return err
	}
	err = sftpUpload(sc, backupFile, remote)
	sc.Close()
	if err != nil {
		return fmt.Errorf("Backup hochladen: %w", err)
	}
	if err := runIgnoringDisconnect(ctx, cl, 30*time.Second, "/system/backup/load", "=name="+remote, "=password="); err != nil {
		return fmt.Errorf("Backup laden: %w", err)
	}
	cl.Close()
	if _, err := p.waitReboot(ctx, id, s.Uptime, 10*time.Minute, x, name); err != nil {
		return err
	}
	if cl, err = p.session(ctx, id); err != nil {
		return err
	}
	removeDeviceFile(ctx, cl, remote)
	p.jobs.step(x, name, "alter Zustand (wireless + Backup) wiederhergestellt")
	return nil
}

// applyWifiConfig spielt die übersetzte Konfiguration ein und prüft sie.
func (p *Poller) applyWifiConfig(ctx context.Context, id string, cl *routeros.Client, x *Job, name string, mig *migration) error {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	l, err := printAll(rctx, cl, "/interface/wifi/print", "=.proplist=name,mac-address,master-interface")
	if err != nil {
		return fmt.Errorf("wifi-Schnittstellen lesen: %w", err)
	}
	bands := map[string]string{}
	if rl, err := printAll(rctx, cl, "/interface/wifi/radio/print", "=.proplist=interface,bands,radio-mac"); err == nil {
		for _, r := range rl {
			bands[r["interface"]] = r["bands"]
		}
	}
	var radios []wifiRadio
	for _, m := range l {
		if masterOf(m) == "" {
			radios = append(radios, wifiRadio{Name: m["name"], MAC: m["mac-address"], Bands: bands[m["name"]]})
		}
	}
	script, newName, err := wifiScript(mig, radios)
	if err != nil {
		return err
	}
	// Skript auf dem Manager ablegen (zum Nachlesen bzw. manuellen Einspielen)
	dir := p.backupDir(id)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, time.Now().Format("20060102-150405")+"-wifi-config.rsc"), []byte(script), 0o600)

	// Verwaiste Bridge-Ports/Listeneinträge der alten wlan-Schnittstellen entfernen
	names := map[string]bool{"all": true, "none": true}
	if il, err := printAll(rctx, cl, "/interface/print", "=.proplist=name"); err == nil {
		for _, m := range il {
			names[m["name"]] = true
		}
	}
	if il, err := printAll(rctx, cl, "/interface/list/print", "=.proplist=name"); err == nil {
		for _, m := range il {
			names[m["name"]] = true
		}
	}
	old := map[string]bool{}
	for _, li := range mig.ifs {
		old[li.Name] = true
	}
	removed := 0
	for _, menu := range []string{"/interface/bridge/port", "/interface/list/member"} {
		pl, err := printAll(rctx, cl, menu+"/print", "=.proplist=.id,interface")
		if err != nil {
			continue
		}
		for _, m := range pl {
			ifc := m["interface"]
			if !names[ifc] && (old[ifc] || strings.HasPrefix(ifc, "*")) {
				if _, err := cl.RunContext(rctx, menu+"/remove", "=.id="+m[".id"]); err == nil {
					removed++
				}
			}
		}
	}
	if removed > 0 {
		p.jobs.step(x, name, "%d verwaiste Einträge der alten WLAN-Schnittstellen entfernt", removed)
	}

	p.jobs.step(x, name, "spiele WLAN-Konfiguration ein …")
	removeScriptNamed(rctx, cl, "mm-wifi")
	if _, err := cl.RunContext(rctx, "/system/script/add", "=name=mm-wifi", "=source="+script, "=comment="+mmComment,
		"=policy=read,write,policy,test,sensitive"); err != nil {
		return fmt.Errorf("Skript anlegen: %w", err)
	}
	_, runErr := cl.RunContext(rctx, "/system/script/run", "=number=mm-wifi")
	removeScriptNamed(rctx, cl, "mm-wifi")
	if runErr != nil {
		return fmt.Errorf("Konfiguration einspielen: %w", runErr)
	}

	// Prüfen
	time.Sleep(3 * time.Second)
	s, err := readWLAN(rctx, cl)
	if err != nil {
		return err
	}
	var bad []string
	for _, li := range mig.ifs {
		w := s.find(newName[li.Name])
		switch {
		case w == nil:
			bad = append(bad, newName[li.Name]+" fehlt")
		case w.SSID != li.SSID || w.Passphrase != li.PSK || w.Disabled != li.Disabled:
			bad = append(bad, newName[li.Name]+" hat andere Werte")
		case li.Bridge != "" && w.Bridge != li.Bridge:
			bad = append(bad, newName[li.Name]+" nicht an "+li.Bridge)
		}
	}
	if vl, err := printAll(rctx, cl, "/interface/vlan/print", "=.proplist=name,vlan-id,interface"); err == nil {
		for _, li := range mig.ifs {
			ok := li.VLAN == ""
			for _, m := range vl {
				ok = ok || m["name"] == vlanIfName(li.VLAN) && m["vlan-id"] == li.VLAN && m["interface"] == li.VLANParent
			}
			if !ok {
				bad = append(bad, fmt.Sprintf("VLAN-Schnittstelle %s (VLAN %s auf %s) fehlt", vlanIfName(li.VLAN), li.VLAN, li.VLANParent))
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("Prüfung: %s", strings.Join(bad, ", "))
	}
	var sum []string
	for _, li := range mig.ifs {
		sum = append(sum, li.Name+"→"+newName[li.Name])
	}
	p.jobs.step(x, name, "fertig: %s mit allen SSIDs/Passwörtern übernommen (%s)", wifiPkg, strings.Join(sum, ", "))
	return nil
}

func removeScriptNamed(ctx context.Context, cl *routeros.Client, n string) {
	if l, err := printAll(ctx, cl, "/system/script/print", "=.proplist=.id,name"); err == nil {
		for _, s := range l {
			if s["name"] == n {
				cl.RunContext(ctx, "/system/script/remove", "=.id="+s[".id"])
			}
		}
	}
}
