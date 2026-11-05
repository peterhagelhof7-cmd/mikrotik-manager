package main

import (
	"strings"
	"testing"
)

func staticReq() ChangeReq {
	return ChangeReq{IPMode: "static", IPAddress: "192.168.1.20", IPMask: "255.255.255.0", IPGateway: "192.168.1.1", IPDNS: "192.168.1.1, 9.9.9.9"}
}

func TestParseIPReq(t *testing.T) {
	r, err := parseIPReq(staticReq())
	if err != nil || r.addr.String() != "192.168.1.20/24" || r.gw.String() != "192.168.1.1" || strings.Join(r.dns, ",") != "192.168.1.1,9.9.9.9" {
		t.Fatal(r, err)
	}
	for _, m := range []string{"24", "/24"} {
		c := staticReq()
		c.IPMask = m
		if r, err := parseIPReq(c); err != nil || r.addr.Bits() != 24 {
			t.Errorf("%s: %v", m, err)
		}
	}
	for name, f := range map[string]func(*ChangeReq){
		"IP":           func(c *ChangeReq) { c.IPAddress = "192.168.1" },
		"IPv6":         func(c *ChangeReq) { c.IPAddress = "fe80::1" },
		"Netz":         func(c *ChangeReq) { c.IPAddress = "192.168.1.0" },
		"Broadcast":    func(c *ChangeReq) { c.IPAddress = "192.168.1.255" },
		"Maske":        func(c *ChangeReq) { c.IPMask = "255.0.255.0" },
		"Maske leer":   func(c *ChangeReq) { c.IPMask = "" },
		"zu klein":     func(c *ChangeReq) { c.IPMask = "31" },
		"Gateway fern": func(c *ChangeReq) { c.IPGateway = "10.0.0.1" },
		"Gateway=IP":   func(c *ChangeReq) { c.IPGateway = "192.168.1.20" },
		"DNS":          func(c *ChangeReq) { c.IPDNS = "dns.example" },
		"DNS leer":     func(c *ChangeReq) { c.IPDNS = " " },
	} {
		c := staticReq()
		f(&c)
		if _, err := parseIPReq(c); err == nil {
			t.Errorf("%s angenommen", name)
		}
	}
}

func TestChooseIPIface(t *testing.T) {
	types := map[string]string{"bridge": "bridge", "ether1": "ether", "wg-mm": "wg", "lte1": "lte"}
	a := func(i string) map[string]string { return map[string]string{"interface": i} }
	// Cloud: Tunnel-Adresse und LTE zählen nicht.
	if n, why := chooseIPIface([]map[string]string{a("bridge"), a("wg-mm"), a("lte1")}, nil, types); n != "bridge" || why != "" {
		t.Fatal(n, why)
	}
	if n, why := chooseIPIface(nil, []map[string]string{a("bridge")}, types); n != "bridge" || why != "" {
		t.Fatal(n, why)
	}
	// Router: WAN per DHCP, LAN fest.
	if n, why := chooseIPIface([]map[string]string{a("bridge")}, []map[string]string{a("ether1")}, types); n != "" || why == "" {
		t.Fatal(n, why)
	}
	if n, why := chooseIPIface([]map[string]string{a("wg-mm")}, nil, types); n != "" || why == "" {
		t.Fatal(n, why)
	}
	// Ausgeschalteter Rückfall-Client ist keine zweite Schnittstelle.
	if n, _ := chooseIPIface([]map[string]string{a("bridge")}, []map[string]string{{"interface": "ether1", "disabled": "true"}}, types); n != "bridge" {
		t.Fatal(n)
	}
}

func dhcpState() ipState {
	return ipState{Iface: "bridge", CurOnIface: true, DNS: "", DHCP: map[string]string{"interface": "bridge", "disabled": "false"}}
}

func staticPlan(t *testing.T, st ipState) Plan {
	t.Helper()
	r, err := parseIPReq(staticReq())
	if err != nil {
		t.Fatal(err)
	}
	return buildIPStaticPlan(st, r)
}

func TestIPStaticFromDHCP(t *testing.T) {
	p := staticPlan(t, dhcpState())
	if p.Noop || p.Blocked != "" || p.NewAddr != "192.168.1.20" || p.NewIP {
		t.Fatalf("%+v", p)
	}
	for _, w := range []string{
		`/ip dhcp-client disable [find interface="bridge"]`,
		`/ip address add address="192.168.1.20/24" interface="bridge"`,
		`/ip route add dst-address=0.0.0.0/0 gateway="192.168.1.1"`,
		`/ip dns set servers="192.168.1.1,9.9.9.9"`,
		`/system scheduler add name="mm-ip-fallback"`,
	} {
		if !strings.Contains(p.Apply, w) {
			t.Errorf("fehlt %s in\n%s", w, p.Apply)
		}
	}
	// Eine feste Adresse gleich der DHCP-Adresse geht erst, wenn der Client aus ist.
	if strings.Index(p.Apply, "dhcp-client disable") > strings.Index(p.Apply, "/ip address add") {
		t.Error("Reihenfolge")
	}
	for _, w := range []string{`/ip dhcp-client enable [find interface="bridge"]`, `/ip address remove [find address="192.168.1.20/24"`,
		`/ip route remove [find dst-address=0.0.0.0/0 gateway="192.168.1.1"]`, `/ip dns set servers=""`, `/system scheduler remove`} {
		if !strings.Contains(p.Undo, w) {
			t.Errorf("Rückweg ohne %s:\n%s", w, p.Undo)
		}
	}
}

func TestIPStaticReplacesOld(t *testing.T) {
	st := dhcpState()
	st.DHCP = nil
	st.DNS = "1.1.1.1"
	st.Statics = []map[string]string{{".id": "*3", "address": "192.168.88.1/24", "comment": "defconf"}}
	st.Routes = []map[string]string{{".id": "*4", "gateway": "192.168.88.254", "distance": "2"}}
	p := staticPlan(t, st)
	for _, w := range []string{"/ip address remove numbers=*3", "/ip route remove numbers=*4", `/ip dhcp-client add interface="bridge" disabled=yes`} {
		if !strings.Contains(p.Apply, w) {
			t.Errorf("fehlt %s in\n%s", w, p.Apply)
		}
	}
	for _, w := range []string{`/ip address add address="192.168.88.1/24" interface="bridge" comment="defconf"`,
		`/ip route add dst-address=0.0.0.0/0 gateway="192.168.88.254" distance=2`, `/ip dns set servers="1.1.1.1"`, `/ip dhcp-client remove`} {
		if !strings.Contains(p.Undo, w) {
			t.Errorf("Rückweg ohne %s:\n%s", w, p.Undo)
		}
	}
}

func TestIPStaticNoop(t *testing.T) {
	// Die Prüfung nach dem Ausführen muss „nichts zu tun“ ergeben, sonst rollt doChange zurück.
	st := ipState{Iface: "bridge", CurOnIface: true, DNS: "192.168.1.1,9.9.9.9",
		DHCP:        map[string]string{"interface": "bridge", "disabled": "true"},
		Statics:     []map[string]string{{"address": "192.168.1.20/24"}},
		Routes:      []map[string]string{{"gateway": "192.168.1.1"}},
		FallbackSrc: ipFallbackSource("bridge", "192.168.1.1")}
	if p := staticPlan(t, st); !p.Noop {
		t.Fatalf("%+v", p)
	}
	// Zeitplan da, Client fehlt: Der Rückweg muss den Zeitplan behalten.
	old := st.DHCP
	st.DHCP = nil
	if p := staticPlan(t, st); p.Noop || !strings.Contains(p.Undo, "system script add") {
		t.Fatalf("%+v\n%s", p, p.Undo)
	}
	st.DHCP = old
	// Device-Mode home: Einen vorhandenen Zeitplan fasst der Plan nicht an.
	st.NoScheduler, st.DNS = true, "1.1.1.1"
	if p := staticPlan(t, st); strings.Contains(p.Undo, "scheduler remove") {
		t.Fatal(p.Undo)
	}
	st.NoScheduler, st.DNS = false, "192.168.1.1,9.9.9.9"
	// Anderes Gateway im Skript zählt als Abweichung.
	st.FallbackSrc = ipFallbackSource("bridge", "192.168.1.2")
	p := staticPlan(t, st)
	if p.Noop || !strings.Contains(p.Undo, "mm-ip-fallback") {
		t.Fatalf("%+v", p)
	}
}

func TestIPStaticHomeMode(t *testing.T) {
	st := dhcpState()
	st.NoScheduler = true
	p := staticPlan(t, st)
	if strings.Contains(p.Apply, "scheduler add") || len(p.Warn) == 0 || !strings.Contains(p.Warn[0], "Device-Mode") {
		t.Fatalf("%+v\n%s", p, p.Apply)
	}
}

func TestIPTunnelDeviceKeepsAddress(t *testing.T) {
	// Cloud: Der Manager erreicht das Gerät über den Tunnel, nicht über diese Schnittstelle.
	st := dhcpState()
	st.CurOnIface = false
	if p := staticPlan(t, st); p.NewAddr != "" || p.NewIP {
		t.Fatalf("%+v", p)
	}
}

func TestIPFallbackScript(t *testing.T) {
	s := ipFallbackSource("bridge", "192.168.1.1")
	for _, w := range []string{`/ip dhcp-client find interface="bridge"`, "/ping 192.168.1.1 count=3", "($mmGwFail >= 5)", "dhcp-client enable $c", "dhcp-client disable $c"} {
		if !strings.Contains(s, w) {
			t.Errorf("fehlt %s in\n%s", w, s)
		}
	}
	// Im Apply-Skript steht das Skript als Zeichenkette: $ und " dürfen dort nichts auslösen.
	a := ipFallbackAdd(s)
	if !strings.Contains(a, `\$mmGwFail`) || !strings.Contains(a, `interface=\"bridge\"`) || strings.Count(a, "\n") != 2 {
		t.Fatal(a)
	}
}

func TestIPBackToDHCP(t *testing.T) {
	st := ipState{Iface: "bridge", CurOnIface: true, DNS: "9.9.9.9",
		DHCP:        map[string]string{"interface": "bridge", "disabled": "true"},
		Statics:     []map[string]string{{".id": "*3", "address": "192.168.1.20/24"}},
		Routes:      []map[string]string{{".id": "*4", "gateway": "192.168.1.1"}},
		FallbackSrc: "alt"}
	p := buildIPDhcpPlan(st)
	if !p.NewIP || p.Bridge != "bridge" {
		t.Fatalf("%+v", p)
	}
	// Der Zeitplan schaltet den Client sonst wieder aus, solange das Gateway antwortet.
	if a, b := strings.Index(p.Apply, "scheduler remove"), strings.Index(p.Apply, "dhcp-client enable"); a < 0 || a > b {
		t.Fatal(p.Apply)
	}
	// Bricht das Skript vorher ab, darf der Rückweg nichts doppelt anlegen.
	if !strings.Contains(p.Undo, `:if ([:len [/ip route find dst-address=0.0.0.0/0 gateway="192.168.1.1" dynamic=no]] = 0) do={`) ||
		!strings.Contains(p.Undo, `:if ([:len [/ip address find address="192.168.1.20/24" interface="bridge"]] = 0) do={`) {
		t.Fatal(p.Undo)
	}
	// Erst Lease, dann entfernen: ohne Lease bleibt die feste IP.
	if a, b := strings.Index(p.Apply, "kein DHCP-Lease"), strings.Index(p.Apply, "/ip address remove"); a < 0 || a > b {
		t.Fatal(p.Apply)
	}
	for _, w := range []string{`/ip dhcp-client disable`, `/ip address add address="192.168.1.20/24"`, `gateway="192.168.1.1"`, "mm-ip-fallback"} {
		if !strings.Contains(p.Undo, w) {
			t.Errorf("Rückweg ohne %s:\n%s", w, p.Undo)
		}
	}
	if p := buildIPDhcpPlan(dhcpState()); !p.Noop {
		t.Fatalf("%+v", p)
	}
}

func TestSNMPPlan(t *testing.T) {
	pub := func(r, w, s string) []map[string]string {
		return []map[string]string{{"name": "public", "read-access": r, "write-access": w, "security": s}}
	}
	// Werkszustand: public da, SNMP aus
	p := buildSNMPPlan(false, pub("true", "false", "none"), true)
	if p.Noop || p.Apply != "/snmp set enabled=yes\n" || !strings.Contains(p.Undo, "enabled=no") {
		t.Fatalf("%+v", p)
	}
	if p := buildSNMPPlan(true, pub("true", "false", "none"), true); !p.Noop {
		t.Fatalf("%+v", p)
	}
	p = buildSNMPPlan(true, pub("true", "true", "authorized"), true)
	if !strings.Contains(p.Apply, "write-access=no security=none") || !strings.Contains(p.Undo, "write-access=yes security=authorized") {
		t.Fatalf("%+v", p)
	}
	p = buildSNMPPlan(false, []map[string]string{{"name": "intern"}}, true)
	if !strings.Contains(p.Apply, `/snmp community add name="public" read-access=yes write-access=no`) || !strings.Contains(p.Undo, "community remove") {
		t.Fatalf("%+v", p)
	}
	if p := buildSNMPPlan(true, nil, false); p.Apply != "/snmp set enabled=no\n" || p.Noop {
		t.Fatalf("%+v", p)
	}
	if p := buildSNMPPlan(false, nil, false); !p.Noop {
		t.Fatalf("%+v", p)
	}
}
