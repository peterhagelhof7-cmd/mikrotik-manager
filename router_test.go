package main

import (
	"strings"
	"testing"
)

func routerSample() RouterCfg {
	return RouterCfg{Device: "R",
		Ports: []RouterPort{
			{Name: "ether1", Role: "wan", Type: "dhcp"},
			{Name: "ether2", Role: "backup", Type: "pppoe", VLAN: "7", User: "u", SecretSet: true},
			{Name: "ether3", Role: "access"},
			{Name: "ether4", Role: "access", VLAN: "10"},
			{Name: "ether5", Role: "access", VLAN: "10,20"},
			{Name: "ether6", Role: "access", VLAN: "1,20"},
			{Name: "ether7", Role: "off"},
		},
		Nets: []RouterNet{
			{Address: "192.168.55.1/24", VLAN: 1, DHCP: true, First: "192.168.55.100", Last: "192.168.55.200"},
			{Address: "192.168.56.1/24", VLAN: 10, DHCP: true, First: "192.168.56.100", Last: "192.168.56.200", DNS: "9.9.9.9"},
		}}
}

func TestValidRouter(t *testing.T) {
	c := routerSample()
	if err := validRouter(&c); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*RouterCfg){
		"ohne Gerät":       func(c *RouterCfg) { c.Device = "" },
		"zwei WAN":         func(c *RouterCfg) { c.Ports[1].Role = "wan" },
		"kein WAN":         func(c *RouterCfg) { c.Ports[0].Role = "off" },
		"zwei Backup":      func(c *RouterCfg) { c.Ports[0].Role = "backup" },
		"kein Access":      func(c *RouterCfg) { c.Ports = c.Ports[:2] },
		"Rolle fehlt":      func(c *RouterCfg) { c.Ports[2].Role = "" },
		"Typ fehlt":        func(c *RouterCfg) { c.Ports[0].Type = "" },
		"PPPoE ohne Pw":    func(c *RouterCfg) { c.Ports[1].SecretSet = false },
		"IPoE unvollst.":   func(c *RouterCfg) { c.Ports[0].Type = "ipoe" },
		"WAN zwei VLANs":   func(c *RouterCfg) { c.Ports[1].VLAN = "7,8" },
		"WAN VLAN 1":       func(c *RouterCfg) { c.Ports[1].VLAN = "1" },
		"WAN-VLAN Port":    func(c *RouterCfg) { c.Ports[3].VLAN = "7" },
		"WAN-VLAN Netz":    func(c *RouterCfg) { c.Nets[1].VLAN = 7 },
		"WAN-VLAN doppelt": func(c *RouterCfg) { c.Ports[0].VLAN = "7" },
		"WWAN mit VLAN":    func(c *RouterCfg) { c.Ports[0].Type, c.Ports[0].VLAN = "wwan", "9" },
		"VLAN Text":        func(c *RouterCfg) { c.Ports[3].VLAN = "abc" },
		"VLAN zu groß":     func(c *RouterCfg) { c.Ports[3].VLAN = "5000" },
		"VLAN doppelt":     func(c *RouterCfg) { c.Ports[4].VLAN = "20,20" },
		"Netz doppelt":     func(c *RouterCfg) { c.Nets[1].VLAN = 1 },
		"Netz überlappt":   func(c *RouterCfg) { c.Nets[1].Address = "192.168.55.2/25" },
		"Netzadresse":      func(c *RouterCfg) { c.Nets[1].Address = "192.168.56.0/24" },
		"kein CIDR":        func(c *RouterCfg) { c.Nets[1].Address = "192.168.56.1" },
		"DHCP außerhalb":   func(c *RouterCfg) { c.Nets[1].First = "192.168.57.1" },
		"DHCP verdreht":    func(c *RouterCfg) { c.Nets[1].First, c.Nets[1].Last = "192.168.56.200", "192.168.56.100" },
		"DHCP mit Router":  func(c *RouterCfg) { c.Nets[1].First = "192.168.56.1" },
		"DNS ungültig":     func(c *RouterCfg) { c.Nets[1].DNS = "dns" },
	} {
		c := routerSample()
		f(&c)
		if err := validRouter(&c); err == nil {
			t.Errorf("%s angenommen", name)
		}
	}
	// Das erste Netz ist immer VLAN 1.
	c = routerSample()
	c.Nets[0].VLAN = 99
	if validRouter(&c); c.Nets[0].VLAN != 1 {
		t.Fatal(c.Nets[0].VLAN)
	}
}

func TestRouterVLANs(t *testing.T) {
	c := routerSample()
	got := routerVLANs(c)
	if len(got) != 2 || got[0].ID != 10 || got[1].ID != 20 {
		t.Fatalf("%+v", got)
	}
	// VLAN 10: Netz am Router, offener Port getaggt, einzeln ungetaggt, Trunk getaggt
	if j := strings.Join(got[0].Tagged, ","); j != "BRIDGE,ether3,ether5" {
		t.Fatal(j)
	}
	if j := strings.Join(got[0].Untagged, ","); j != "ether4" {
		t.Fatal(j)
	}
	// VLAN 20: nur an Ports, ohne Netz, also nicht am Router
	if j := strings.Join(got[1].Tagged, ","); j != "ether3,ether5,ether6" {
		t.Fatal(j)
	}
	for _, x := range []struct {
		vlan, frames string
		pvid         int
	}{{"", "admit-all", 1}, {"10", "admit-only-untagged-and-priority-tagged", 10}, {"10,20", "admit-only-vlan-tagged", 1}, {"1,20", "admit-all", 1}} {
		if p, f := routerBridgePort(RouterPort{VLAN: x.vlan}); p != x.pvid || f != x.frames {
			t.Errorf("%q: %d %s", x.vlan, p, f)
		}
	}
}

func TestRouterScript(t *testing.T) {
	c := routerSample()
	if err := validRouter(&c); err != nil {
		t.Fatal(err)
	}
	s := routerScript(c, map[string]string{"ether2": "geh$eim"}, "bridge")
	for _, w := range []string{
		`/ip dhcp-client add interface="ether1" add-default-route=yes default-route-distance=1`,
		`/interface vlan add name="mm-wan7" interface="ether2" vlan-id=7`,
		`/interface pppoe-client add name="mm-pppoe-ether2" interface="mm-wan7" user="u" password="geh\$eim" add-default-route=yes default-route-distance=2`,
		`/interface bridge port add bridge="bridge" interface="ether4" pvid=10 frame-types=admit-only-untagged-and-priority-tagged`,
		`/interface vlan add name="mm-vlan10" interface="bridge" vlan-id=10`,
		`/ip address add address="192.168.56.1/24" interface="mm-vlan10"`,
		`/ip dhcp-server network add address="192.168.56.0/24" gateway="192.168.56.1" dns-server="9.9.9.9"`,
		`/ip dhcp-server network add address="192.168.55.0/24" gateway="192.168.55.1" dns-server="192.168.55.1"`,
		`/interface bridge vlan add bridge="bridge" vlan-ids=10 tagged="bridge,ether3,ether5" untagged="ether4"`,
		`/ip firewall nat add chain=srcnat out-interface-list="mm-wan" action=masquerade`,
		`/ip firewall filter remove [find where dynamic=no and in-interface!="wg-mm"]`,
		`/ip address remove [find where dynamic=no and interface!="wg-mm"]`,
		`/system scheduler add name="mm-router-wan"`,
		`/interface ethernet set [find name="ether7"] disabled=yes`,
	} {
		if !strings.Contains(s, w) {
			t.Errorf("fehlt %s\n", w)
		}
	}
	// VLAN-Filterung geht erst an, wenn alles steht.
	if a, b := strings.LastIndex(s, "vlan-filtering=yes"), strings.LastIndex(s, "/interface bridge vlan add"); a < b {
		t.Error("Reihenfolge VLAN-Filter")
	}
	// Eine Firewall-Regel ohne Rückweg zum Tunnel würde den Manager aussperren.
	if strings.Contains(s, `/interface wireguard`) || strings.Contains(s, `/user`) {
		t.Error("Tunnel oder Benutzer angefasst")
	}
	// Ohne Backup WAN kein Zeitplan.
	c.Ports = append(c.Ports[:1], c.Ports[2:]...)
	if s := routerScript(c, nil, "bridge"); strings.Contains(s, "scheduler add") {
		t.Error(s)
	}
}

func TestRouterFailoverScript(t *testing.T) {
	s := routerFailoverScript(RouterPort{Name: "ether1", Type: "dhcp"}, RouterPort{Name: "lte1", Type: "wwan"})
	for _, w := range []string{`[/ip dhcp-client get [find interface="ether1"] gateway]`, `:set bk [:tostr "lte1"]`, `dst-address=0.0.0.0/1 gateway=$bk`, `dst-address=128.0.0.0/1 gateway=$bk`,
		`($mmWanFail >= 6)`, `($mmWanOk >= 6)`, `/ping 8.8.4.4 count=2`, `/ping 1.0.0.1 count=2`} {
		if !strings.Contains(s, w) {
			t.Errorf("fehlt %s\n%s", w, s)
		}
	}
	// Die Prüfadresse darf nicht in den DHCP-DNS-Vorgaben landen.
	if s := routerFailoverScript(RouterPort{Type: "ipoe", Gateway: "1.2.3.4"}, RouterPort{Name: "x", Type: "pppoe"}); !strings.Contains(s, `"mm-pppoe-x"`) || !strings.Contains(s, `"1.2.3.4"`) {
		t.Error(s)
	}
}

func TestRouterHash(t *testing.T) {
	a := routerHash(routerSample(), map[string]string{"ether2": "a"})
	if a != routerHash(routerSample(), map[string]string{"ether2": "a"}) || a == routerHash(routerSample(), map[string]string{"ether2": "b"}) {
		t.Fatal("Passwort zählt nicht")
	}
	c := routerSample()
	c.Nets[1].VLAN = 11
	if a == routerHash(c, map[string]string{"ether2": "a"}) {
		t.Fatal("Konfiguration zählt nicht")
	}
}

func TestSaveRouterKeepsSecret(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), APIPort: 1}
	st := newStore(cfg.DataDir)
	v, err := openVault(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoller(cfg, st, v)
	c := routerSample()
	c.Ports[1].SecretSet = false
	if _, err := p.SaveRouter("k", c); err == nil {
		t.Fatal("PPPoE ohne Passwort gespeichert")
	}
	c.Ports[1].Secret = "pw"
	got, err := p.SaveRouter("k", c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ports[1].Secret != "" || p.Router("k").Ports[1].Secret != "" {
		t.Fatal("Geheimnis in der Datei")
	}
	if s, ok := v.Get(routerSecretKey("k", "ether2")); !ok || s.Password != "pw" {
		t.Fatal(s, ok)
	}
	// Leer lassen behält das Passwort.
	c.Ports[1].Secret = ""
	if _, err := p.SaveRouter("k", c); err != nil {
		t.Fatal(err)
	}
	if p.Router("leer").Nets[0].VLAN != 1 {
		t.Fatal("Standard-LAN fehlt")
	}
}

func TestBuildRouterWan(t *testing.T) {
	c := RouterCfg{Device: "R", Ports: []RouterPort{
		{Name: "ether1", Role: "wan", Type: "dhcp"},
		{Name: "ether2", Role: "backup", Type: "pppoe", VLAN: "7"},
		{Name: "ether3", Role: "access"},
	}}
	addrs := []map[string]string{{"address": "192.168.178.50/24", "interface": "ether1"}, {"address": "100.64.1.2/32", "interface": "mm-pppoe-ether2", "network": "100.64.0.1"},
		{"address": "10.0.0.1/24", "interface": "bridge"}}
	dcs := []map[string]string{{"interface": "ether1", "status": "bound", "gateway": "192.168.178.254", "primary-dns": "192.168.178.254", "expires-after": "4h42m"}}
	ifs := []map[string]string{{"name": "ether1", "running": "true", "last-link-up-time": "2026-11-05 23:37:50"}, {"name": "ether2", "running": "false", "last-link-up-time": "2026-11-01 10:00:00"},
		{"name": "mm-pppoe-ether2", "running": "true", "last-link-up-time": "2026-11-05 23:38:10"}}
	env := []map[string]string{{"name": "mmLeaseWan", "value": "2026-11-05 23:38:00"}}
	got := buildRouterWan(c, addrs, dcs, ifs, env, false)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	w, b := got[0], got[1]
	if w.Since != "2026-11-05 23:38:00" || w.Expires != "4h42m" || w.Gateway != "192.168.178.254" || w.LinkUp != "2026-11-05 23:37:50" || !w.Active || w.Addresses[0] != "192.168.178.50/24" {
		t.Fatalf("%+v", w)
	}
	if b.Since != "2026-11-05 23:38:10" || b.Gateway != "100.64.0.1" || b.Link || b.LinkUp != "" || b.Active {
		t.Fatalf("%+v", b)
	}
	if got := buildRouterWan(c, addrs, dcs, ifs, env, true); got[0].Active || !got[1].Active {
		t.Fatalf("%+v", got)
	}
	// Ohne gebundenen Lease gibt es kein „bezogen am“.
	dcs[0]["status"] = "searching..."
	if got := buildRouterWan(c, addrs, dcs, ifs, env, false); got[0].Since != "" {
		t.Fatalf("%+v", got[0])
	}
	if s := routerScript(routerSampleValid(t), map[string]string{"ether2": "x"}, "bridge"); !strings.Contains(s, `:global mmLeaseWan`) {
		t.Fatal(s)
	}
}

func routerSampleValid(t *testing.T) RouterCfg {
	t.Helper()
	c := routerSample()
	if err := validRouter(&c); err != nil {
		t.Fatal(err)
	}
	return c
}
