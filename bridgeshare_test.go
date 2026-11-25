package main

import (
	"strings"
	"testing"
)

// Router und AP im selben Gerät: Router-Tab und WLAN-Verwaltung nehmen sich keine Bridge-VLANs
// weg und legen nie zwei Einträge mit derselben VLAN-ID an (RouterOS ≥ 7.17 lehnt das ab).

func TestRouterKeepsWlanBridgeEntries(t *testing.T) {
	c := RouterCfg{
		Ports: []RouterPort{{Name: "ether1", Role: "wan", Type: "dhcp"}, {Name: "ether2", Role: "access"}},
		Nets:  []RouterNet{{Address: "192.168.55.1/24", VLAN: 1}, {Address: "192.168.20.1/24", VLAN: 20}},
	}
	live := bridgeLive{eth: map[string]bool{"ether1": true, "ether2": true}, entries: []bridgeEntry{
		{ID: "*A", IDs: "20", Comment: mmComment, Tagged: []string{"ether2", "mm-mesh-ap"}},
		{ID: "*C", IDs: "30", Comment: mmComment, Tagged: []string{"ether2"}}, // nur WLAN
	}}
	sc := routerScriptLive(c, nil, "bridge", live)
	for _, want := range []string{
		`/interface bridge vlan remove [find where bridge="bridge" and dynamic=no and comment!="mikrotik-manager"]`,
		`/interface bridge vlan remove *A`,
		`/interface bridge vlan add bridge="bridge" vlan-ids=20 tagged="bridge,ether2,mm-mesh-ap" comment="mm-router"`,
	} {
		if !strings.Contains(sc, want) {
			t.Errorf("fehlt: %s\n--- Skript:\n%s", want, sc)
		}
	}
	if strings.Contains(sc, "remove *C") {
		t.Error("WLAN-Eintrag ohne Router-VLAN entfernt")
	}
}

func TestCentralUsesRouterBridgeEntries(t *testing.T) {
	c := defaultCentral()
	c.Networks = []CentralNet{
		{SSID: "Heim", Passphrase: "geheim123", Auth: "wpa2-psk,wpa3-psk", Bands: "both"},
		{SSID: "Heim-5", Passphrase: "geheim123", Auth: "wpa2-psk,wpa3-psk", Bands: "both", VLAN: 20},
	}
	s := centralTestState("20", "2437")
	bi := testBridge()
	bi.eth = map[string]bool{"ether1": true, "ether2": true}
	bi.rt = map[string]bridgeEntry{"20": {ID: "*5", IDs: "20", Comment: routerComment, Tagged: []string{"bridge1", "ether1", "ether2", "wifi9"}}}
	p, err := planCentralState(s, bi, c, "2437")
	if err != nil || p.Blocked != "" {
		t.Fatalf("%v %+v", err, p)
	}
	if strings.Contains(p.Apply, "vlan-ids=20 ") || strings.Contains(strings.Join(p.Steps, "\n"), "VLAN 20 fehlt") {
		t.Errorf("eigener Eintrag neben dem Router-Eintrag:\n%s", p.Apply)
	}
	// veraltete WLAN-Schnittstelle im Router-Eintrag → raus, Router-Ports bleiben
	if !strings.Contains(p.Apply, `/interface bridge vlan set *5 tagged="bridge1,ether1,ether2"`) {
		t.Errorf("Router-Eintrag:\n%s", p.Apply)
	}
	e := bi.rt["20"]
	e.Tagged = []string{"bridge1", "ether1", "ether2"}
	bi.rt["20"] = e
	p, _ = planCentralState(s, bi, c, "2437")
	if strings.Contains(p.Apply, "vlan set *") || strings.Contains(strings.Join(p.Steps, "\n"), "Eintrag des Routers") {
		t.Errorf("Abweichung trotz passendem Router-Eintrag: %v", p.Steps)
	}
}
