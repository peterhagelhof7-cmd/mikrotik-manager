package main

import (
	"strings"
	"testing"
)

func meshCentral() CentralWLAN {
	c := defaultCentral()
	c.Enabled = true
	c.Networks = []CentralNet{
		{SSID: "Heim", Passphrase: "geheim123", Auth: "wpa2-psk,wpa3-psk", Bands: "both"},
		{SSID: "Heim-5", Passphrase: "geheim123", Auth: "wpa2-psk,wpa3-psk", Bands: "both"},
	}
	c.Radio5.Channel = "5180"
	c.Meshlink = CentralMeshlink{Enabled: true, SSID: "mm-mesh-abc123", Passphrase: "Meshpasswort-12345678"}
	return c
}

// meshState5180: Gerät wie centralTestState, 5 GHz fest auf 5180, optional mit fertigem Meshlink.
func meshState5180(withMesh, staOn bool) *wlanState {
	s := centralTestState("20", "2437")
	s.find("wifi2").Radio.Freq = "5180"
	s.mesh = meshState{read: true, hasUplink: true, uplinkLink: !staOn}
	if withMesh {
		mk := func(name, mode string, disabled, hidden bool) *wlanIfc {
			return &wlanIfc{WlanIf: WlanIf{Name: name, Master: "wifi2", Band: "5 GHz", SSID: "mm-mesh-abc123", Auth: "wpa2-psk,wpa3-psk",
				Passphrase: "Meshpasswort-12345678", Bridge: "bridge1", Disabled: disabled, Hidden: hidden}, country: "germany",
				raw: map[string]string{"configuration.mode": mode, "name": name}}
		}
		s.ifs = append(s.ifs, mk(meshAP, "ap", staOn, true), mk(meshSTA, "station-bridge", !staOn, false))
		s.mesh.script, s.mesh.scheduler = meshWatch, true
	}
	return s
}

func TestMeshlinkPlan(t *testing.T) {
	c := meshCentral()

	// Ohne festen 5-GHz-Kanal: gesperrt
	ca := c
	ca.Radio5.Channel = "auto"
	if p, _ := planCentralState(meshState5180(false, false), testBridge(), ca, "2437"); !strings.Contains(p.Blocked, "festen 5-GHz-Kanal") {
		t.Fatalf("auto-Kanal: %+v", p)
	}

	// Noch nicht eingerichtet: Abweichungen + Skript legt alles an
	p, err := planCentralState(meshState5180(false, false), testBridge(), c, "2437")
	if err != nil || p.Noop || p.Blocked != "" {
		t.Fatalf("%v %+v", err, p)
	}
	all := strings.Join(p.Steps, "\n")
	for _, want := range []string{"Meshlink: mm-mesh-ap fehlt", "Meshlink: mm-mesh-sta fehlt", "Meshlink: Wächter-Skript fehlt", "Meshlink auf wifi2 (Kanal"} {
		if !strings.Contains(all, want) {
			t.Errorf("fehlt %q in\n%s", want, all)
		}
	}
	for _, want := range []string{
		`/interface wifi add name="mm-mesh-ap" master-interface="wifi2" configuration.ssid="mm-mesh-abc123"`,
		`configuration.mode=ap configuration.hide-ssid=yes disabled=no`,
		`configuration.mode=station-bridge disabled=yes`,
		`:if ([/interface get [/interface find name="ether1"] running]) do={`,
		`/interface bridge port add bridge="bridge1" interface="mm-mesh-sta" comment="mm-meshlink"`,
		`/system scheduler add name="mm-meshlink" interval=30s policy=read,write,test`,
	} {
		if !strings.Contains(p.Apply, want) {
			t.Errorf("fehlt im Skript: %s\n--- Skript:\n%s", want, p.Apply)
		}
	}
	// Skript-Quelltext einzeilig maskiert: keine echten Zeilenumbrüche, $ maskiert
	line := p.Apply[strings.Index(p.Apply, "/system script add name=\"mm-meshlink\""):]
	line = line[:strings.Index(line, "\n")]
	if !strings.Contains(line, `\n:local sta`) || !strings.Contains(line, `\$sta`) {
		t.Errorf("Skript-Quelltext nicht maskiert: %s", line[:200])
	}
	// Meshlink-Schnittstellen in der Reihenfolge: nach dem WLAN-Zurücksetzen, vor Bridge/VLAN
	if strings.Index(p.Apply, "# Meshlink") < strings.Index(p.Apply, "# neu") || strings.Index(p.Apply, "# Meshlink") > strings.Index(p.Apply, "# Bridge/VLAN") {
		t.Errorf("Reihenfolge im Skript falsch:\n%s", p.Apply)
	}

	// Fertig eingerichtet, Kabel da → nichts zu tun
	if p, err := planCentralState(meshState5180(true, false), testBridge(), c, "2437"); err != nil || !p.Noop {
		t.Fatalf("Noop erwartet: %v %+v", err, p.Steps)
	}
	// Fertig eingerichtet, Knoten hängt gerade am Meshlink → trotzdem nichts zu tun (Laufzeit-Zustand)
	if p, err := planCentralState(meshState5180(true, true), testBridge(), c, "2437"); err != nil || !p.Noop {
		t.Fatalf("Noop am Meshlink erwartet: %v %+v", err, p.Steps)
	}
	// Veraltetes Wächter-Skript → Abweichung
	s := meshState5180(true, false)
	s.mesh.script = "alt"
	if p, _ := planCentralState(s, testBridge(), c, "2437"); p.Noop || !strings.Contains(strings.Join(p.Steps, "\n"), "Wächter-Skript fehlt oder ist veraltet") {
		t.Fatalf("veraltetes Skript: %+v", p.Steps)
	}
	// Andere Meshlink-SSID (z. B. anderer Standort) → Abweichung
	c2 := c
	c2.Meshlink.SSID = "mm-mesh-anders"
	if p, _ := planCentralState(meshState5180(true, false), testBridge(), c2, "2437"); p.Noop {
		t.Fatal("andere SSID muss abweichen")
	}
	// Mit VLAN: Meshlink-Schnittstellen tragen die VLANs getaggt
	cv := c
	cv.Networks = append([]CentralNet{}, c.Networks...)
	cv.Networks[1].VLAN = 30
	p, _ = planCentralState(meshState5180(true, false), testBridge(), cv, "2437")
	if !strings.Contains(p.Apply, `vlan-ids=30 tagged=ether1,ether2,mm-mesh-ap,mm-mesh-sta`) {
		t.Errorf("VLAN nicht über Meshlink getaggt:\n%s", p.Apply)
	}

	// Kein ether1 → gesperrt
	s = meshState5180(false, false)
	s.mesh.hasUplink = false
	if p, _ := planCentralState(s, testBridge(), c, "2437"); !strings.Contains(p.Blocked, "ether1") {
		t.Fatalf("ohne ether1: %+v", p)
	}
	// Altes wireless-Paket → nur Hinweis, kein Meshlink im Skript
	s = meshState5180(false, false)
	s.pkg = "wireless"
	if p, _ := planCentralState(s, testBridge(), c, "2437"); strings.Contains(p.Apply, "mm-mesh-ap") || !strings.Contains(strings.Join(p.Warn, " "), "wifi-Paket") {
		t.Fatalf("wireless: %+v", p.Warn)
	}
}

func TestMeshlinkDisable(t *testing.T) {
	c := meshCentral()
	c.Meshlink.Enabled = false
	// Eingerichtet, Kabel da → wird entfernt
	p, _ := planCentralState(meshState5180(true, false), testBridge(), c, "2437")
	if p.Noop || p.Blocked != "" || !strings.Contains(strings.Join(p.Steps, "\n"), "Meshlink ist noch eingerichtet und wird entfernt") ||
		!strings.Contains(p.Apply, `/system script remove [find name="mm-meshlink"]`) || strings.Contains(p.Apply, "/interface wifi add name=\"mm-mesh") {
		t.Fatalf("entfernen: %+v\n%s", p.Steps, p.Apply)
	}
	// Knoten hängt nur am Meshlink → Abschalten gesperrt
	if p, _ := planCentralState(meshState5180(true, true), testBridge(), c, "2437"); !strings.Contains(p.Blocked, "nur über Meshlink") {
		t.Fatalf("Abschalten am Meshlink muss gesperrt sein: %+v", p)
	}
	// Nie eingerichtet → nichts zu tun
	if p, _ := planCentralState(meshState5180(false, false), testBridge(), c, "2437"); !p.Noop {
		t.Fatalf("ohne Meshlink Noop erwartet: %+v", p.Steps)
	}
}

func TestMeshlinkFillAndScript(t *testing.T) {
	m := CentralMeshlink{Enabled: true}
	fillMeshlink(&m)
	if !strings.HasPrefix(m.SSID, "mm-mesh-") || len(m.SSID) != 14 || len(m.Passphrase) != 32 {
		t.Fatalf("erzeugt: %+v", m)
	}
	old := m
	fillMeshlink(&m)
	if m != old {
		t.Fatal("vorhandene Zugangsdaten dürfen nicht neu erzeugt werden")
	}
	off := CentralMeshlink{}
	fillMeshlink(&off)
	if off.SSID != "" {
		t.Fatal("ausgeschaltet: nichts erzeugen")
	}
	for i, r := range meshWatch {
		if r > 127 {
			t.Fatalf("Wächter-Skript nicht ASCII an Stelle %d (%q)", i, r)
		}
	}
	if err := validSSID(m.SSID); err != nil {
		t.Fatal(err)
	}
	if err := validPSK(m.Passphrase); err != nil {
		t.Fatal(err)
	}
}
