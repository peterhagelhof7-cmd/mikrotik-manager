package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestWwanPrefix(t *testing.T) {
	for _, ok := range []string{"192.168.50", "10.20.30", "172.16.0"} {
		if err := validPrefix(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "192.168", "192.168.50.0", "192.168.256", "127.0.0", "10.250.1", "169.254.1", "224.1.1", "a.b.c"} {
		if validPrefix(bad) == nil {
			t.Errorf("%q angenommen", bad)
		}
	}
	for _, bad := range []string{"12", "123456789", "12a4"} {
		if validPIN(bad) == nil {
			t.Errorf("PIN %q angenommen", bad)
		}
	}
}

// wwanDevice: Chateau nach der Setup-Station – Bridge mit DHCP-Client, LTE ohne Router-Konfiguration.
func wwanDevice() *wwanState {
	return &wwanState{ltes: []string{"lte1"}, bridge: "bridge",
		dhcpCl: []map[string]string{{".id": "*1", "interface": "bridge"}},
		dns:    "", ntpOn: false}
}

func TestWwanPlanFresh(t *testing.T) {
	c := WwanCfg{Prefix: "192.168.50", APN: "internet.telekom"}
	p := planWwanState(wwanDevice(), "DEV1", c, map[string]string{"lte1": "1234"}, nil)
	if p.Noop || p.Blocked != "" {
		t.Fatalf("%+v", p)
	}
	all := strings.Join(p.Steps, "\n")
	for _, want := range []string{"Adresse 192.168.50.254/24 auf bridge", "Adressbereich 192.168.50.10-192.168.50.200", "NAT über lte1",
		"DHCP-Client auf bridge wird entfernt", "DNS-Server 1.1.1.1,9.9.9.9", "NTP-Client an", "SIM-PIN für lte1 setzen", "APN „internet.telekom“"} {
		if !strings.Contains(all, want) {
			t.Errorf("fehlt %q in\n%s", want, all)
		}
	}
	for _, want := range []string{
		`/interface lte set [find name="lte1"] pin="1234"`,
		`/interface lte apn set [find name="default"] apn="internet.telekom"`,
		`/ip address add address="192.168.50.254/24" interface="bridge" comment="mm-wwan"`,
		`/ip pool add name="mm-wwan" ranges="192.168.50.10-192.168.50.200" comment="mm-wwan"`,
		`/ip dhcp-server network add address="192.168.50.0/24" gateway="192.168.50.254" dns-server="1.1.1.1,9.9.9.9" comment="mm-wwan"`,
		`/ip dns set servers="1.1.1.1,9.9.9.9" allow-remote-requests=no`,
		`/system ntp client set enabled=yes servers="de.pool.ntp.org"`,
		`/ip firewall nat add chain=srcnat out-interface="lte1" action=masquerade comment="mm-wwan lte1"`,
		`/ip firewall filter add chain=input in-interface=lte1 action=drop comment="mm-wwan in-lte1"`,
		`/ip firewall filter add chain=forward in-interface=lte1 connection-state=new action=drop comment="mm-wwan fw-lte1"`,
		`/ip dhcp-client remove [find interface="bridge"]`,
	} {
		if !strings.Contains(p.Apply, want) {
			t.Errorf("fehlt im Skript: %s\n%s", want, p.Apply)
		}
	}
	// PIN/APN vor dem LAN-Umbau (ohne Mobilfunk kein Weg zurück)
	if strings.Index(p.Apply, "pin=") > strings.Index(p.Apply, "/ip dhcp-client remove") {
		t.Error("PIN muss vor dem Entfernen des DHCP-Clients gesetzt werden")
	}
	if p.wwanPins["lte1"] == "" {
		t.Error("PIN-Hash fehlt")
	}
}

// wwanDone: Gerät entspricht der Einstellung 192.168.50 mit PIN 1234.
func wwanDone() *wwanState {
	s := &wwanState{ltes: []string{"lte1"}, bridge: "bridge", dns: "1.1.1.1,9.9.9.9", ntpOn: true, ntpSrv: "de.pool.ntp.org", apnDist: "2"}
	s.addrs = []map[string]string{{"address": "192.168.50.254/24", "interface": "bridge", "comment": "mm-wwan"}}
	s.pools = []map[string]string{{"ranges": "192.168.50.10-192.168.50.200", "comment": "mm-wwan"}}
	s.servers = []map[string]string{{"interface": "bridge", "comment": "mm-wwan", "disabled": "false"}}
	s.networks = []map[string]string{{"address": "192.168.50.0/24", "gateway": "192.168.50.254", "dns-server": "1.1.1.1,9.9.9.9", "comment": "mm-wwan"}}
	s.nat = []map[string]string{{"comment": "mm-wwan lte1", "out-interface": "lte1", "action": "masquerade"}}
	for _, r := range wwanFilterRules(s.ltes) {
		s.filter = append(s.filter, map[string]string{"comment": r[0]})
	}
	return s
}

func TestWwanPlanNoopAndChanges(t *testing.T) {
	c := WwanCfg{Prefix: "192.168.50"}
	pins := map[string]string{"lte1": "1234"}
	applied := map[string]string{"lte1": pinHash("DEV1", "1234")}
	if p := planWwanState(wwanDone(), "DEV1", c, pins, applied); !p.Noop {
		t.Fatalf("Noop erwartet: %+v", p.Steps)
	}
	// neue PIN → Abweichung
	if p := planWwanState(wwanDone(), "DEV1", c, map[string]string{"lte1": "9999"}, applied); p.Noop || !strings.Contains(strings.Join(p.Steps, "\n"), "SIM-PIN für lte1 setzen") {
		t.Fatalf("neue PIN: %+v", p.Steps)
	}
	// anderes Netz → Abweichung
	if p := planWwanState(wwanDone(), "DEV1", WwanCfg{Prefix: "10.1.2"}, pins, applied); p.Noop {
		t.Fatal("anderes Netz muss abweichen")
	}
	// fehlende Firewall-Regel → Abweichung
	s := wwanDone()
	s.filter = s.filter[1:]
	if p := planWwanState(s, "DEV1", c, pins, applied); p.Noop {
		t.Fatal("fehlende Firewall-Regel muss abweichen")
	}
	// gesperrt: keine LTE-Schnittstelle / keine Bridge / kein Netz
	s = wwanDone()
	s.ltes = nil
	if p := planWwanState(s, "DEV1", c, pins, applied); p.Blocked == "" {
		t.Fatal("ohne LTE gesperrt erwartet")
	}
	s = wwanDone()
	s.bridge = ""
	if p := planWwanState(s, "DEV1", c, pins, applied); p.Blocked == "" {
		t.Fatal("ohne Bridge gesperrt erwartet")
	}
	if p := planWwanState(wwanDone(), "DEV1", WwanCfg{}, pins, applied); p.Blocked == "" {
		t.Fatal("ohne Netz gesperrt erwartet")
	}
}

func TestWwanUsage(t *testing.T) {
	at := func(s string) time.Time { x, _ := time.ParseInLocation("2006-01-02 15:04", s, time.Local); return x }
	m := func(v int64) map[string]int64 { return map[string]int64{"lte1": v} }
	var u wwanUsage
	u = addUsage(u, m(1000), m(500), 3600, at("2026-11-30 23:50")) // Ausgangswert, zählt nicht
	if len(u.Months) != 1 || u.Months[0].Rx != 0 {
		t.Fatalf("erste Messung: %+v", u.Months)
	}
	u = addUsage(u, m(3000), m(800), 3900, at("2026-11-30 23:55"))
	u = addUsage(u, m(5000), m(900), 4200, at("2026-12-01 00:00")) // Stichtag 1. des Monats
	if len(u.Months) != 2 || u.Months[0].Month != "2026-12" || u.Months[0].Rx != 2000 || u.Months[1].Month != "2026-11" || u.Months[1].Rx != 2000 || u.Months[1].Tx != 300 {
		t.Fatalf("Monatswechsel: %+v", u.Months)
	}
	// Neustart: Laufzeit kleiner → neuer Zählerstand zählt komplett
	u = addUsage(u, m(7000), m(100), 60, at("2026-12-01 01:00"))
	if u.Months[0].Rx != 9000 || u.Months[0].Tx != 200 {
		t.Fatalf("Neustart: %+v", u.Months[0])
	}
	// Zähler kleiner bei gleicher Laufzeit-Richtung (Modem neu) → ebenfalls komplett
	u = addUsage(u, m(10), m(5), 120, at("2026-12-01 01:05"))
	if u.Months[0].Rx != 9010 {
		t.Fatalf("Zähler-Reset: %+v", u.Months[0])
	}
	// neue LTE-Schnittstelle zählt ab ihrem Stand
	u = addUsage(u, map[string]int64{"lte1": 20, "lte2": 50}, map[string]int64{"lte1": 5, "lte2": 0}, 180, at("2026-12-01 01:10"))
	if u.Months[0].Rx != 9070 {
		t.Fatalf("zweite LTE: %+v", u.Months[0])
	}
}

func TestWwanSaveKeepsPinsInVault(t *testing.T) {
	dir := t.TempDir()
	v, err := openVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoller(Config{DataDir: dir}, newStore(dir), v)
	if _, err := p.SaveWwan("DEV1", "192.168.50", "", map[string]string{"lte1": "12a4"}); err == nil {
		t.Fatal("ungültige PIN angenommen")
	}
	c, err := p.SaveWwan("DEV1", "192.168.50", "", map[string]string{"lte1": "4321"})
	if err != nil || !c.PinSet["lte1"] {
		t.Fatalf("%+v %v", c, err)
	}
	b, _ := os.ReadFile(dir + "/wwan.json")
	if strings.Contains(string(b), "4321") {
		t.Fatal("PIN steht im Klartext in wwan.json")
	}
	if cr, ok := p.vault.Get(wwanPinKey("DEV1", "lte1")); !ok || cr.Password != "4321" {
		t.Fatal("PIN nicht im Tresor")
	}
	// leer = unverändert, "-" = löschen
	if c, _ = p.SaveWwan("DEV1", "192.168.51", "", map[string]string{"lte1": ""}); !c.PinSet["lte1"] || c.Prefix != "192.168.51" {
		t.Fatalf("unverändert: %+v", c)
	}
	p.markPinsApplied("DEV1", map[string]string{"lte1": "x"})
	if got, _ := p.WwanConfig("DEV1"); got.PinHash["lte1"] != "x" {
		t.Fatalf("PIN-Hash nicht gespeichert: %+v", got)
	}
	if c, _ = p.SaveWwan("DEV1", "192.168.51", "", map[string]string{"lte1": "-"}); c.PinSet["lte1"] {
		t.Fatal("PIN nicht gelöscht")
	}
	p.forgetWwan("DEV1")
	if _, ok := p.WwanConfig("DEV1"); ok {
		t.Fatal("Einstellung nach Vergessen noch da")
	}
}

// Chateau mit 5 Ports, alle im LAN: ether4+ether5 werden WAN-Bridge mit DHCP-Client.
func TestWwanWanBridge(t *testing.T) {
	s := wwanDevice()
	s.ethers = []string{"ether1", "ether2", "ether3", "ether4", "ether5"}
	s.bridges = []string{"bridge"}
	for _, e := range s.ethers {
		s.ports = append(s.ports, map[string]string{"interface": e, "bridge": "bridge"})
	}
	p := planWwanState(s, "DEV1", WwanCfg{Prefix: "192.168.50"}, nil, nil)
	all := strings.Join(p.Steps, "\n")
	for _, want := range []string{"Bridge mm-wan fehlt", "ether4 gehört nicht zu mm-wan", "ether5 gehört nicht zu mm-wan",
		"DHCP-Client auf mm-wan", "LTE-Standardroute Distanz 2", "NAT über mm-wan", "WAN: ether4, ether5 in eigener Bridge mm-wan"} {
		if !strings.Contains(all, want) {
			t.Errorf("fehlt %q in\n%s", want, all)
		}
	}
	for _, want := range []string{
		`/interface lte apn set [find name="default"] add-default-route=yes default-route-distance=2`,
		`/interface bridge add name="mm-wan" comment="mm-wwan"`,
		`/interface bridge port remove [find interface="ether4"]`,
		`/interface bridge port add bridge="mm-wan" interface="ether5" comment="mm-wwan"`,
		`/ip dhcp-client add interface="mm-wan" add-default-route=no use-peer-dns=no use-peer-ntp=no comment="mm-wwan" script=`,
		`check-gateway=ping distance=1 target-scope=11`,
		`/ip firewall nat add chain=srcnat out-interface="mm-wan" action=masquerade comment="mm-wwan mm-wan"`,
		`/ip firewall filter add chain=input in-interface=mm-wan action=drop comment="mm-wwan in-mm-wan"`,
	} {
		if !strings.Contains(p.Apply, want) {
			t.Errorf("fehlt im Skript: %s", want)
		}
	}
	// DHCP-Skript einzeilig maskiert ($ und Zeilenumbrüche)
	if !strings.Contains(p.Apply, `\$\"gateway-address\"`) || !strings.Contains(p.Apply, `\n/ip route remove`) {
		t.Errorf("DHCP-Skript nicht maskiert:\n%s", p.Apply)
	}
	// WAN-Bridge vor dem LAN-Umbau, nach PIN/APN
	if !(strings.Index(p.Apply, "default-route-distance") < strings.Index(p.Apply, "mm-wan") && strings.Index(p.Apply, `/interface bridge add name="mm-wan"`) < strings.Index(p.Apply, `/ip dhcp-client remove [find interface="bridge"]`)) {
		t.Errorf("Reihenfolge:\n%s", p.Apply)
	}

	// fertig: WAN-Bridge steht → nichts zu tun
	d := wwanDone()
	d.ethers, d.bridges = s.ethers, []string{"bridge", "mm-wan"}
	for _, e := range []string{"ether1", "ether2", "ether3"} {
		d.ports = append(d.ports, map[string]string{"interface": e, "bridge": "bridge"})
	}
	d.ports = append(d.ports, map[string]string{"interface": "ether4", "bridge": "mm-wan"}, map[string]string{"interface": "ether5", "bridge": "mm-wan"})
	d.dhcpCl = []map[string]string{{"interface": "mm-wan", "comment": "mm-wwan", "add-default-route": "no", "script": wanDhcpScript, "disabled": "false"}}
	d.nat = append(d.nat, map[string]string{"comment": "mm-wwan mm-wan", "out-interface": "mm-wan", "action": "masquerade"})
	d.filter = nil
	for _, r := range wwanFilterRules(d.wanIfs()) {
		d.filter = append(d.filter, map[string]string{"comment": r[0]})
	}
	if p := planWwanState(d, "DEV1", WwanCfg{Prefix: "192.168.50"}, nil, nil); !p.Noop {
		t.Fatalf("Noop erwartet: %+v", p.Steps)
	}
	// Gerät mit nur 4 Ports: nur ether4
	s4 := wwanDevice()
	s4.ethers = []string{"ether1", "ether2", "ether3", "ether4"}
	if got := s4.wanPorts(); len(got) != 1 || got[0] != "ether4" {
		t.Fatalf("4 Ports: %v", got)
	}
	// ohne ether4/5: keine WAN-Bridge
	s3 := wwanDevice()
	s3.ethers = []string{"ether1", "ether2"}
	if p := planWwanState(s3, "DEV1", WwanCfg{Prefix: "192.168.50"}, nil, nil); strings.Contains(p.Apply, "mm-wan") {
		t.Fatal("ohne ether4/5 keine WAN-Bridge")
	}
	for i, r := range wanDhcpScript {
		if r > 127 {
			t.Fatalf("DHCP-Skript nicht ASCII an Stelle %d", i)
		}
	}
}
