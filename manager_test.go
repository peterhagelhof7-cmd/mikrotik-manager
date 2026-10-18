package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tlv(typ uint16, v []byte) []byte {
	b := make([]byte, 4, 4+len(v))
	binary.BigEndian.PutUint16(b[0:2], typ)
	binary.BigEndian.PutUint16(b[2:4], uint16(len(v)))
	return append(b, v...)
}

func TestParseMNDP(t *testing.T) {
	up := make([]byte, 4)
	binary.LittleEndian.PutUint32(up, 93784)
	pkt := []byte{0, 0, 0, 7}
	pkt = append(pkt, tlv(tlvMAC, []byte{0x48, 0x8f, 0x5a, 0x01, 0x02, 0x03})...)
	pkt = append(pkt, tlv(tlvIdentity, []byte("hap-flur"))...)
	pkt = append(pkt, tlv(tlvVersion, []byte("6.48.6 (long-term)"))...)
	pkt = append(pkt, tlv(tlvPlatform, []byte("MikroTik"))...)
	pkt = append(pkt, tlv(tlvUptime, up)...)
	pkt = append(pkt, tlv(tlvSoftwareID, []byte("ABCD-1234"))...)
	pkt = append(pkt, tlv(tlvBoard, []byte("RB952Ui-5ac2nD"))...)
	pkt = append(pkt, tlv(tlvInterface, []byte("bridge"))...)
	pkt = append(pkt, tlv(tlvIPv4, []byte{10, 0, 0, 20})...)

	d, ok := parseMNDP(pkt)
	if !ok {
		t.Fatal("nicht erkannt")
	}
	if d.MAC != "48:8F:5A:01:02:03" || d.Identity != "hap-flur" || d.SoftwareID != "ABCD-1234" ||
		d.IP != "10.0.0.20" || d.Uptime != 93784 || d.Board != "RB952Ui-5ac2nD" || d.Interface != "bridge" {
		t.Fatalf("falsch geparst: %+v", d)
	}
	if d.Major() != "6" {
		t.Fatalf("Major = %q", d.Major())
	}
	// eigene Anfrage (4 Nullbytes) und abgeschnittene Pakete ignorieren
	if _, ok := parseMNDP([]byte{0, 0, 0, 0}); ok {
		t.Fatal("Anfrage als Gerät erkannt")
	}
	if _, ok := parseMNDP(pkt[:10]); ok {
		t.Fatal("abgeschnittenes Paket ohne MAC als Gerät erkannt")
	}
}

func TestParseROSDuration(t *testing.T) {
	for in, want := range map[string]int64{
		"1w2d3h4m5s":   604800 + 2*86400 + 3*3600 + 4*60 + 5,
		"5m10s":        310,
		"1w2d03:04:05": 604800 + 2*86400 + 3*3600 + 4*60 + 5,
		"01:00:00":     3600,
		"":             0,
	} {
		if got := parseROSDuration(in); got != want {
			t.Errorf("%q: %d, erwartet %d", in, got, want)
		}
	}
}

func TestBroadcastAddr(t *testing.T) {
	if b := broadcastAddr(netip.MustParsePrefix("10.0.0.3/24")); b.String() != "10.0.0.255" {
		t.Fatal(b)
	}
}

// Ein Gerät, das zuerst nur per Scan (IP) bekannt ist, wird beim ersten
// API-Kontakt auf die Software-ID umgeschlüsselt; eine spätere MNDP-Sichtung
// mit derselben Software-ID landet im selben Eintrag.
func TestStoreRekey(t *testing.T) {
	st := newStore(t.TempDir())
	id, isNew := st.Observe(Device{IP: "10.0.0.5"}, "scan")
	if !isNew || id != "ip:10.0.0.5" {
		t.Fatalf("id=%s new=%v", id, isNew)
	}
	id2, _ := st.Observe(Device{SoftwareID: "SW1", IP: "10.0.0.5"}, "api")
	if id2 != "SW1" {
		t.Fatalf("nicht umgeschlüsselt: %s", id2)
	}
	id3, isNew := st.Observe(Device{SoftwareID: "SW1", MAC: "aa:bb:cc:dd:ee:ff", IP: "10.0.0.9"}, "mndp")
	if id3 != "SW1" || isNew {
		t.Fatalf("MNDP nicht zugeordnet: %s %v", id3, isNew)
	}
	if l := st.List(); len(l) != 1 || l[0].IP != "10.0.0.9" || l[0].MAC != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("Store: %+v", l)
	}
}

func TestVault(t *testing.T) {
	dir := t.TempDir()
	v, err := openVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c, src := v.For("x"); src != "factory" || c.User != "admin" {
		t.Fatal(src)
	}
	v.Set(defaultCredKey, Cred{User: "mtm", Password: "geheim"})
	v.Set("dev:x", Cred{User: "admin", Password: "pw2"})
	v2, _ := openVault(dir)
	if c, src := v2.For("x"); src != "device" || c.Password != "pw2" {
		t.Fatal(src, c)
	}
	if c, src := v2.For("y"); src != "default" || c.Password != "geheim" {
		t.Fatal(src, c)
	}
}

func TestVerNum(t *testing.T) {
	for in, want := range map[string]string{
		"7.24.5 (stable) 2026-09-29 14:21:27": "7.24.5",
		"7.24.5":                              "7.24.5",
		"":                                    "",
		"   ":                                 "",
	} {
		if got := verNum(in); got != want {
			t.Errorf("verNum(%q) = %q, erwartet %q", in, got, want)
		}
	}
}

func TestBackupNamesAndPrune(t *testing.T) {
	dir := t.TempDir()
	p := &Poller{cfg: Config{DataDir: dir}}
	for _, bad := range []string{"../secret.key", "x.backup", "20261018-155700-manuell.exe", "20261018-155700-vor-update.backup/../x"} {
		if p.BackupPath("A", bad) != "" {
			t.Errorf("%q zugelassen", bad)
		}
	}
	if p.BackupPath("A", "20261018-155700-vor-update.backup") == "" {
		t.Fatal("gültiger Name abgelehnt")
	}
	bd := p.backupDir("../../etc")
	if filepath.Dir(bd) != filepath.Join(dir, "backups") {
		t.Fatalf("Pfad entkommt: %s", bd)
	}
	// 25 Sicherungen (je .backup + .rsc) → die 20 neuesten bleiben
	os.MkdirAll(p.backupDir("A"), 0o700)
	for i := 0; i < 25; i++ {
		for _, ext := range []string{".backup", ".rsc"} {
			os.WriteFile(filepath.Join(p.backupDir("A"), fmt.Sprintf("20261018-%06d-manuell%s", i, ext)), []byte("x"), 0o600)
		}
	}
	p.pruneBackups("A")
	l := p.ListBackups("A")
	if len(l) != 40 || l[0].Name != "20261018-000024-manuell.rsc" || l[39].Name != "20261018-000005-manuell.backup" {
		t.Fatalf("%d Dateien, erste %s, letzte %s", len(l), l[0].Name, l[len(l)-1].Name)
	}
}

func TestNextBackupAt(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	at := func(s string) time.Time {
		v, _ := time.ParseInLocation("2006-01-02 15:04", s, loc)
		return v
	}
	for _, c := range []struct{ now, want string }{
		{"2026-10-18 16:00", "2026-10-19 04:00"},
		{"2026-10-18 03:59", "2026-10-18 04:00"},
		{"2026-10-18 04:00", "2026-10-19 04:00"},
		{"2026-10-24 12:00", "2026-10-25 04:00"}, // Nacht der Zeitumstellung
	} {
		got, ok := nextBackupAt(at(c.now), "04:00")
		if !ok || !got.Equal(at(c.want)) {
			t.Errorf("%s: %v, erwartet %s", c.now, got, c.want)
		}
	}
	if _, ok := nextBackupAt(time.Now(), "off"); ok {
		t.Error("off nicht erkannt")
	}
}

func TestScriptQuoting(t *testing.T) {
	if got := q(`a"b$c\d`); got != `"a\"b\$c\\d"` {
		t.Fatal(got)
	}
	for _, bad := range []string{"", "Büro-Küche", "a\nb", strings.Repeat("x", 65)} {
		if validIdentity(bad) == nil {
			t.Errorf("%q zugelassen", bad)
		}
	}
	if validIdentity("Keller AP-2") != nil {
		t.Error("gültiger Name abgelehnt")
	}
}

func TestWifiTranslate(t *testing.T) {
	for in, want := range map[string]string{"2ghz-b/g/n": "2ghz-n", "2ghz-g/n": "2ghz-n", "2ghz-b/g": "2ghz-g", "5ghz-n/ac": "5ghz-ac", "5ghz-a/n": "5ghz-n", "5ghz-a": "5ghz-a"} {
		if got := wifiBand(in); got != want {
			t.Errorf("wifiBand(%q) = %q, erwartet %q", in, got, want)
		}
	}
	if wifiWidth("20/40/80mhz-XXXX") != "20/40/80mhz" || wifiWidth("20/40mhz-XX") != "20/40mhz" || wifiWidth("") != "" {
		t.Error("wifiWidth")
	}
	if wifiCountry("germany") != "Germany" || wifiCountry("united states") != "United States" || wifiCountry("no_country_set") != "" {
		t.Error("wifiCountry")
	}
	for _, bad := range []string{"", "Küche", strings.Repeat("x", 33)} {
		if validSSID(bad) == nil {
			t.Errorf("SSID %q zugelassen", bad)
		}
	}
	for _, bad := range []string{"kurz", "Passwört123", strings.Repeat("x", 64)} {
		if validPSK(bad) == nil {
			t.Errorf("Passwort %q zugelassen", bad)
		}
	}
}

// cAP ac mit wireless-Paket: zwei Funkteile mit gemeinsamem Profil, dazu ein
// virtuelles Gästenetz ohne Passwort. MACs passen absichtlich nicht → Zuordnung per Band.
func TestWifiScript(t *testing.T) {
	mig := &migration{ifs: []legacyIf{
		{Name: "wlan1", MAC: "AA:00:00:00:00:01", SSID: "Haus", Auth: "wpa2-psk", PSK: `ge"heim$1`, Band: "2ghz-n", Width: "20/40mhz", Country: "Germany", Install: "indoor", Bridge: "bridge1", Lists: []string{"LAN"}},
		{Name: "wlan2", MAC: "AA:00:00:00:00:02", SSID: "Haus-5", Auth: "wpa2-psk", PSK: `ge"heim$1`, Band: "5ghz-ac", Width: "20/40/80mhz", Freq: "5180", Country: "Germany", Install: "indoor", Bridge: "bridge1"},
		{Name: "wifi1", Master: "wlan1", SSID: "Gast", Band: "2ghz-n", Country: "Germany", Install: "indoor", Disabled: true},
	}}
	radios := []wifiRadio{
		{Name: "wifi1", MAC: "48:00:00:00:00:01", Bands: "5ghz-a:20mhz,5ghz-ac:20mhz"},
		{Name: "wifi2", MAC: "48:00:00:00:00:02", Bands: "2ghz-g:20mhz,2ghz-n:20mhz"},
	}
	script, names, err := wifiScript(mig, radios)
	if err != nil {
		t.Fatal(err)
	}
	if names["wlan1"] != "wifi2" || names["wlan2"] != "wifi1" || names["wifi1"] != "wifi1-v" {
		t.Fatalf("Zuordnung %v", names)
	}
	for _, want := range []string{
		`add name=sec1 authentication-types=wpa2-psk passphrase="ge\"heim\$1"`,
		`add name=cfg1 mode=ap ssid="Haus" security=sec1 country="Germany" installation=indoor`,
		`add name=cfg3 mode=ap ssid="Gast" country="Germany" installation=indoor`,
		`set [find name="wifi1"] configuration=cfg2 disabled=no channel.band=5ghz-ac channel.width=20/40/80mhz channel.frequency=5180`,
		`add name="wifi1-v" master-interface="wifi2" configuration=cfg3 disabled=yes`,
		`add bridge="bridge1" interface="wifi2"`,
		`add list="LAN" interface="wifi2"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("fehlt: %s\n--- Skript:\n%s", want, script)
		}
	}
	if strings.Count(script, "add name=sec") != 1 {
		t.Errorf("gemeinsames Passwort sollte ein Sicherheitsprofil ergeben:\n%s", script)
	}
}

func TestMigState(t *testing.T) {
	p := &Poller{cfg: Config{DataDir: t.TempDir()}}
	if p.loadMigState("A") != nil || p.lastMigBackup("A") != "" {
		t.Fatal("leerer Stand erwartet")
	}
	if err := p.saveMigState("A", migState{Ifs: []legacyIf{{Name: "wlan1", PSK: "x"}}, Backup: "b"}); err != nil {
		t.Fatal(err)
	}
	if s := p.loadMigState("A"); s == nil || s.Ifs[0].Name != "wlan1" || s.Ifs[0].PSK != "x" {
		t.Fatalf("%+v", s)
	}
	p.clearMigState("A")
	if p.loadMigState("A") != nil {
		t.Fatal("nicht gelöscht")
	}
	os.MkdirAll(p.backupDir("A"), 0o700)
	for _, n := range []string{"20261018-170000-vor-wifi.backup", "20261018-175440-vor-wifi.backup", "20261018-180000-nachts.backup"} {
		os.WriteFile(filepath.Join(p.backupDir("A"), n), []byte("x"), 0o600)
	}
	if b := filepath.Base(p.lastMigBackup("A")); b != "20261018-175440-vor-wifi.backup" {
		t.Fatal(b)
	}
}

func TestRadioPlan(t *testing.T) {
	b := parseBands("2ghz-g:20mhz,2ghz-n:20mhz,20/40mhz")
	if len(b["2ghz-n"]) != 2 || b["2ghz-n"][1] != "40" || len(b["2ghz-g"]) != 1 {
		t.Fatalf("parseBands %v", b)
	}
	if widthUI("20/40/80mhz-XXXX") != "80" || widthUI("20/40mhz-Ce") != "40" || widthUI("20mhz") != "20" {
		t.Error("widthUI")
	}
	str := func(s string) *string { return &s }

	// wireless: alles in einer set-Zeile, Rückweg mit Originalwerten
	bands, widths, chans := legacyBands("2ghz-b/g/n", "IPQ4019")
	lx := &wlanIfc{WlanIf: WlanIf{Name: "wlan1", Radio: &RadioCfg{Band: "2ghz-b/g/n", Bands: bands, Width: "40", Widths: widths, Freq: "auto", Channels: chans, SkipDFS: "disabled"}}, rawWidth: "20/40mhz-XX"}
	ls := &wlanState{pkg: "wireless", ifs: []*wlanIfc{lx}}
	c, _, err := planRadio(ls, lx, &RadioEdit{Width: str("20"), Freq: str("2437")})
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if c.apply != `/interface wireless set [find name="wlan1"] channel-width="20mhz" frequency="2437"`+"\n" ||
		!strings.Contains(c.undo, `channel-width="20/40mhz-XX" frequency="auto"`) {
		t.Fatalf("wireless:\n%s\n%s", c.apply, c.undo)
	}
	if _, _, err := planRadio(ls, lx, &RadioEdit{Freq: str("5180")}); err == nil {
		t.Error("5-GHz-Kanal am 2,4-GHz-Funkteil zugelassen")
	}

	// wifi: "auto" und leer = unset, Rückweg setzt den alten Wert
	wx := &wlanIfc{WlanIf: WlanIf{Name: "wifi2", Radio: &RadioCfg{Band: "", Bands: []string{"5ghz-ac", "5ghz-n"}, Width: "", Widths: []string{"20", "40", "80"}, Freq: "5320", Channels: euChannels5}}}
	ws := &wlanState{pkg: "wifi", ifs: []*wlanIfc{wx}}
	c, warn, err := planRadio(ws, wx, &RadioEdit{Band: str("5ghz-ac"), Width: str("80"), Freq: str("auto"), SkipDFS: str("10min-cac")})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`/interface wifi set [find name="wifi2"] channel.band="5ghz-ac"`,
		`/interface wifi set [find name="wifi2"] channel.width="20/40/80mhz"`,
		`/interface wifi unset [find name="wifi2"] value-name=channel.frequency`,
		`/interface wifi set [find name="wifi2"] channel.skip-dfs-channels="10min-cac"`,
	} {
		if !strings.Contains(c.apply, want) {
			t.Errorf("fehlt %s in\n%s", want, c.apply)
		}
	}
	if !strings.Contains(c.undo, `channel.frequency="5320"`) || !strings.Contains(c.undo, `value-name=channel.band`) || len(warn) != 0 {
		t.Errorf("Rückweg:\n%s", c.undo)
	}
	if _, warn, _ := planRadio(ws, wx, &RadioEdit{Freq: str("5620")}); len(warn) != 1 || !strings.Contains(warn[0], "10 Minuten") {
		t.Errorf("DFS-Hinweis fehlt: %v", warn)
	}
	wx.Radio.Profile = "channel1"
	if _, _, err := planRadio(ws, wx, &RadioEdit{Freq: str("auto")}); err == nil {
		t.Error("Kanalprofil nicht beachtet")
	}

	// Land: wifi über das Konfigurationsprofil, einmal je Profil
	cs := &wlanState{pkg: "wifi", cfgs: map[string]map[string]string{"cfg1": {"country": "Austria"}}, ifs: []*wlanIfc{
		{WlanIf: WlanIf{Name: "wifi1"}, cfg: "cfg1", country: "austria"},
		{WlanIf: WlanIf{Name: "wifi2"}, cfg: "cfg1", country: "austria"},
	}}
	cc := planCountry(cs, "germany")
	if len(cc) != 1 || !strings.Contains(cc[0].apply, `configuration set [find name="cfg1"] country="Germany"`) {
		t.Fatalf("%+v", cc)
	}
	if !validCountry("germany") || !validCountry("united states") || validCountry("Germany") || validCountry("de") {
		t.Error("validCountry")
	}
}

// Typischer Access Point: zwei Funkteile mit Heim, virtuell Heim-5.
func centralTestState(width2, freq2 string) *wlanState {
	r2 := &RadioCfg{Width: width2, Widths: []string{"20", "40"}, Freq: freq2, Channels: euChannels2}
	r5 := &RadioCfg{Width: "80", Widths: []string{"20", "40", "80"}, Freq: "auto", Channels: euChannels5, SkipDFS: "10min-cac"}
	mk := func(name, master, band, ssid string, r *RadioCfg) *wlanIfc {
		return &wlanIfc{WlanIf: WlanIf{Name: name, Master: master, Band: band, SSID: ssid, Auth: "wpa3-psk,wpa2-psk",
			Passphrase: "geheim123", Bridge: "bridge1", Radio: r}, country: "germany",
			raw: map[string]string{"configuration": "cfg1", "configuration.ssid": ssid, "name": name}}
	}
	return &wlanState{pkg: "wifi", ifs: []*wlanIfc{
		mk("wifi1", "", "2,4 GHz", "Heim", r2), mk("wifi2", "", "5 GHz", "Heim", r5),
		mk("wifi3", "wifi1", "2,4 GHz", "Heim-5", nil), mk("wifi4", "wifi2", "5 GHz", "Heim-5", nil),
	}}
}

// Bridge wie auf den Geräten: alle WLANs ungetaggt (PVID 1), ether1+ether2 Uplink.
func testBridge() bridgeInfo {
	return bridgeInfo{name: "bridge1", ethers: []string{"ether1", "ether2"}, mm: map[int]string{},
		pvid: map[string]int{"ether1": 1, "ether2": 1, "wifi1": 1, "wifi2": 1, "wifi3": 1, "wifi4": 1}}
}

func TestCentralVLAN(t *testing.T) {
	c := defaultCentral()
	c.Networks = []CentralNet{
		{SSID: "Heim", Passphrase: "geheim123", Auth: "wpa2-psk,wpa3-psk", Bands: "both"},
		{SSID: "Heim-5", Passphrase: "geheim123", Auth: "wpa2-psk,wpa3-psk", Bands: "both", VLAN: 20},
	}
	s := centralTestState("20", "2437")
	p, err := planCentralState(s, testBridge(), c, "2437")
	if err != nil || p.Noop {
		t.Fatalf("%v %+v", err, p)
	}
	all := strings.Join(p.Steps, "\n")
	for _, want := range []string{`virtuelles WLAN "Heim-5" fehlt`, "Bridge bridge1: VLAN 20 fehlt, VLAN-Filter aus", `"Heim-5" WPA2/WPA3 VLAN 20`} {
		if !strings.Contains(all, want) {
			t.Errorf("fehlt %q in\n%s", want, all)
		}
	}
	for _, want := range []string{
		`/interface bridge port set [find interface="wifi1-v1"] pvid=20`,
		`/interface bridge vlan add bridge="bridge1" vlan-ids=20 tagged=ether1,ether2 comment="mikrotik-manager"`,
		`/interface bridge set [find name="bridge1"] vlan-filtering=yes`,
	} {
		if !strings.Contains(p.Apply, want) {
			t.Errorf("fehlt: %s\n--- Skript:\n%s", want, p.Apply)
		}
	}
	// VLAN-Filter erst ganz am Ende einschalten
	if strings.Index(p.Apply, "vlan-filtering=yes") < strings.Index(p.Apply, "bridge vlan add") {
		t.Error("VLAN-Filter vor der VLAN-Tabelle eingeschaltet")
	}
	if len(p.Warn) == 0 || !strings.Contains(strings.Join(p.Warn, " "), "getaggt durchreichen") {
		t.Errorf("Switch-Hinweis fehlt: %v", p.Warn)
	}

	// Gerät entspricht (virtuelle in VLAN 20, Tabelle + Filter da) → nichts zu tun
	bi := testBridge()
	bi.filtering, bi.mm[20], bi.pvid["wifi3"], bi.pvid["wifi4"] = true, "ether1,ether2", 20, 20
	if p, err := planCentralState(s, bi, c, "2437"); err != nil || !p.Noop {
		t.Fatalf("Noop erwartet: %v %v", err, p.Steps)
	}
	// VLAN aus der Vorgabe entfernt → Tabelle aufräumen, Ports zurück auf PVID 1
	c.Networks[1].VLAN = 0
	p, _ = planCentralState(s, bi, c, "2437")
	if !strings.Contains(strings.Join(p.Steps, "\n"), "VLAN 20 wird nicht mehr gebraucht") ||
		!strings.Contains(p.Apply, `/interface bridge vlan remove [find comment="mikrotik-manager"]`) ||
		!strings.Contains(p.Apply, `/interface bridge port set [find interface="wifi1-v1"] pvid=1`) {
		t.Errorf("Aufräumen:\n%s\n%s", strings.Join(p.Steps, "\n"), p.Apply)
	}
	// ohne Ethernet-Port in der Bridge kein VLAN
	c.Networks[1].VLAN = 30
	bi.ethers = nil
	if p, _ := planCentralState(s, bi, c, "2437"); p.Blocked == "" {
		t.Error("VLAN ohne Ethernet-Uplink zugelassen")
	}
}

func TestCentral(t *testing.T) {
	c := defaultCentral()
	c.Enabled = true
	c.Networks = []CentralNet{
		{SSID: "Heim", Passphrase: "geheim123", Auth: "wpa2-psk,wpa3-psk", Bands: "both"},
		{SSID: "Heim-5", Passphrase: "geheim123", Auth: "wpa2-psk,wpa3-psk", Bands: "both"},
	}
	// entspricht → nichts zu tun
	if p, err := planCentralState(centralTestState("20", "2437"), testBridge(), c, "2437"); err != nil || !p.Noop {
		t.Fatalf("Noop erwartet: %v %+v", err, p.Steps)
	}
	// 40 MHz/auto auf 2,4 GHz → Abweichung, Skript setzt zurück und baut neu auf
	p, err := planCentralState(centralTestState("40", "auto"), testBridge(), c, "2437")
	if err != nil || p.Noop || p.Blocked != "" {
		t.Fatalf("%v %+v", err, p)
	}
	if !strings.Contains(strings.Join(p.Steps, "\n"), "Breite 40 statt 20 MHz") {
		t.Errorf("Abweichung fehlt:\n%s", strings.Join(p.Steps, "\n"))
	}
	for _, want := range []string{
		`/interface wifi remove [find name="wifi3"]`,
		`/interface wifi unset [find name="wifi1"] value-name=configuration.ssid`,
		`/interface wifi security remove [find]`,
		`/interface wifi security add name="mm-sec1" authentication-types=wpa2-psk,wpa3-psk passphrase="geheim123"`,
		`/interface wifi configuration add name="mm-cfg2" mode=ap ssid="Heim-5" country="Germany" security="mm-sec2" hide-ssid=no`,
		`/interface wifi set [find name="wifi1"] configuration="mm-cfg1" channel.width=20mhz channel.frequency=2437 disabled=no`,
		`/interface wifi set [find name="wifi2"] configuration="mm-cfg1" channel.width=20/40/80mhz channel.skip-dfs-channels=10min-cac disabled=no`,
		`/interface wifi add name="wifi1-v1" master-interface="wifi1" configuration="mm-cfg2" disabled=no`,
		`/interface bridge port add bridge="bridge1" interface="wifi2-v1"`,
	} {
		if !strings.Contains(p.Apply, want) {
			t.Errorf("fehlt: %s\n--- Skript:\n%s", want, p.Apply)
		}
	}
	if strings.Count(p.Apply, "mm-sec1\" authentication") != 1 {
		t.Error("Profil doppelt angelegt")
	}
	// 5 GHz aus → Funkteil aus, keine virtuellen WLANs darauf
	c.Radio5.Enabled = false
	p, _ = planCentralState(centralTestState("20", "2437"), testBridge(), c, "2437")
	if !strings.Contains(p.Apply, `/interface wifi set [find name="wifi2"] disabled=yes`) || strings.Contains(p.Apply, "wifi2-v1") {
		t.Errorf("5 GHz aus:\n%s", p.Apply)
	}

	// wireless-Paket (z. B. hAP ac lite): WPA2/WPA3 → WPA2 mit Hinweis
	c.Radio5.Enabled = true
	s := centralTestState("20", "2437")
	s.pkg = "wireless"
	p, err = planCentralState(s, testBridge(), c, "2437")
	if err != nil || len(p.Warn) == 0 || !strings.Contains(p.Warn[0], "kein WPA3") {
		t.Fatalf("%v %v", err, p.Warn)
	}
	for _, want := range []string{
		`/interface wireless security-profiles remove [find default=no]`,
		`/interface wireless security-profiles add name="mm-sec1" mode=dynamic-keys authentication-types=wpa2-psk wpa2-pre-shared-key="geheim123"`,
		`/interface wireless set [find name="wifi1"] mode=ap-bridge ssid="Heim" security-profile="mm-sec1" hide-ssid=no channel-width="20mhz" frequency=2437 country="germany" disabled=no`,
		`/interface wireless add name="wifi2-v1" master-interface="wifi2" ssid="Heim-5" security-profile="mm-sec2" hide-ssid=no disabled=no`,
	} {
		if !strings.Contains(p.Apply, want) {
			t.Errorf("fehlt: %s\n--- Skript:\n%s", want, p.Apply)
		}
	}
}

func TestCentralValidate(t *testing.T) {
	p := &Poller{cfg: Config{DataDir: t.TempDir()}, st: newStore(t.TempDir())}
	c := defaultCentral()
	if p.validCentral(&c) == nil {
		t.Error("ohne WLAN zugelassen")
	}
	c.Networks = []CentralNet{{SSID: "A", Passphrase: "geheim123", Auth: "wpa3-psk,wpa2-psk", Bands: "both"}}
	c.Devices = []string{"unbekannt"}
	if err := p.validCentral(&c); err != nil || c.Networks[0].Auth != "wpa2-psk,wpa3-psk" || len(c.Devices) != 0 {
		t.Fatal(err, c)
	}
	c.Radio5.Channel = "plan"
	if p.validCentral(&c) == nil {
		t.Error("Kanalplan auf 5 GHz zugelassen")
	}
	c.Radio5.Channel = "auto"
	c.Networks = append(c.Networks, CentralNet{SSID: "A", Passphrase: "geheim123", Auth: "wpa2-psk", Bands: "2"})
	if p.validCentral(&c) == nil {
		t.Error("doppelte SSID zugelassen")
	}
	if err := p.SaveCentral(CentralWLAN{Country: "germany", Networks: c.Networks[:1], Radio2: c.Radio2, Radio5: c.Radio5}); err != nil {
		t.Fatal(err)
	}
	if got := p.Central(); len(got.Networks) != 1 || got.Updated.IsZero() {
		t.Fatalf("%+v", got)
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.12.0", "0.11.0-dev", true},
		{"v0.12.0", "v0.12.0", false},
		{"v0.12.0", "0.12.0-dev", true}, // Release ist neuer als seine Vorabversion
		{"v0.11.9", "v0.12.0", false},
		{"v1.0.0", "0.99.99", true},
		{"v0.12.0", "main-abc123", true}, // Entwicklungsstand gilt als älter
		{"v0.13.0-rc1", "0.12.0", false}, // Vorabversionen werden nicht angeboten
		{"kaputt", "0.12.0", false},
	} {
		if got := newerVersion(c.latest, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}

func TestSelfUpdateCheckAndChecksum(t *testing.T) {
	bin := []byte("neues Programm")
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v99.0.0","html_url":"https://example/rel","body":"Neu: alles","assets":[
			{"name":%q,"browser_download_url":"%s/bin"},{"name":"SHA256SUMS","browser_download_url":"%s/sums"}]}`, assetName(), srv.URL, srv.URL)
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(bin) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%064x  %s\n", 1, assetName()) // falsche Prüfsumme
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	u := &selfUpdater{repo: "o/r", api: srv.URL, jobs: newJobs(), client: srv.Client(), info: UpdateInfo{Current: "1.0.0", Mode: "binary"}}
	u.Check(context.Background())
	i := u.Info()
	if !i.Available || i.Latest != "99.0.0" || i.Notes != "Neu: alles" || i.Error != "" {
		t.Fatalf("%+v", i)
	}
	err := u.apply(i.Latest, u.assets)
	if err == nil || !strings.Contains(err.Error(), "Prüfsumme stimmt nicht") {
		t.Fatalf("falsche Prüfsumme nicht erkannt: %v", err)
	}
	exe, _ := selfPath()
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Error(".new nach Fehler nicht aufgeräumt")
	}
	// Während ein Job läuft, wird nicht aktualisiert
	u.jobs.start("x", "update")
	if err := u.Start(); err == nil {
		t.Error("Update trotz laufendem Job gestartet")
	}
}

func TestFmtRate(t *testing.T) {
	for in, want := range map[string]string{"19000000": "19 Mbit/s", "866700000": "866.7 Mbit/s", "43.3Mbps-20MHz/1S/SGI": "43.3 Mbit/s", "48Mbps": "48 Mbit/s", "": ""} {
		if got := fmtRate(in); got != want {
			t.Errorf("%q: %q, erwartet %q", in, got, want)
		}
	}
}

func TestLeadingInt(t *testing.T) {
	for in, want := range map[string]int{"-62@6Mbps": -62, "-55": -55, "-71dBm": -71, "": 0, "x": 0} {
		if got := leadingInt(in); got != want {
			t.Errorf("%q: %d, erwartet %d", in, got, want)
		}
	}
}

func TestSecurity(t *testing.T) {
	if a := legacySecArgs("wpa2-psk", "neuesPasswort"); a != `mode=dynamic-keys authentication-types=wpa2-psk wpa2-pre-shared-key="neuesPasswort"` {
		t.Error(a)
	}
	both := legacySecArgs("wpa-psk,wpa2-psk", "neuesPasswort")
	if !strings.Contains(both, `wpa-pre-shared-key="neuesPasswort"`) || !strings.Contains(both, `wpa2-pre-shared-key="neuesPasswort"`) {
		t.Error(both)
	}
	if normAuth("wpa3-psk, wpa2-psk") != "wpa2-psk,wpa3-psk" {
		t.Error("normAuth")
	}
	// wifi: alle drei wählbar; wireless: WPA2/WPA3 wird WPA2 mit Hinweis, nur WPA3 abgelehnt
	if a, w, err := pickAuth("wifi", "wpa3-psk,wpa2-psk"); err != nil || a != "wpa2-psk,wpa3-psk" || w != "" {
		t.Error(a, w, err)
	}
	if a, w, err := pickAuth("wireless", "wpa2-psk,wpa3-psk"); err != nil || a != "wpa2-psk" || w == "" {
		t.Error(a, w, err)
	}
	if _, _, err := pickAuth("wireless", "wpa3-psk"); err == nil {
		t.Error("WPA3 auf wireless zugelassen")
	}
	if _, _, err := pickAuth("wifi", "wep"); err == nil {
		t.Error("wep zugelassen")
	}
	if authName("wpa2-psk,wpa3-psk") != "WPA2/WPA3" || authName("") != "offen" {
		t.Error("authName")
	}
}
