package main

import (
	"strings"
	"testing"
)

func pvState() portVLANState {
	return portVLANState{Port: "ether5", Bridge: "bridge", Frames: "admit-all", PVID: 1,
		Ethers: []string{"ether1", "ether5", "ether6", "ether7"}, Uplinks: []string{"ether1"},
		Frame: map[string]string{"ether1": "admit-all", "ether5": "admit-all", "ether6": "admit-all", "ether7": "admit-only-untagged-and-priority-tagged"},
		PVIDs: map[string]int{"ether1": 1, "ether5": 1, "ether6": 1, "ether7": 30}, Rows: map[int]*pvBridgeVLAN{}, Site: []int{20, 30}}
}

func TestSiteVLANs(t *testing.T) {
	got := siteVLANs(RouterCfg{Nets: []RouterNet{{Address: "10.0.0.1/24", VLAN: 1}, {Address: "10.0.20.1/24", VLAN: 20}},
		Ports: []RouterPort{{Name: "e1", Role: "access", VLAN: "30"}}})
	if len(got) != 3 || got[0].ID != 1 || got[1].Label != "VLAN 20 (10.0.20.1/24)" || got[2].Label != "VLAN 30" {
		t.Fatalf("%+v", got)
	}
	if l := siteVLANs(RouterCfg{}); len(l) != 1 || l[0].ID != 1 {
		t.Fatalf("%+v", l)
	}
}

func TestPortVLANEnablesFilter(t *testing.T) {
	p := buildPortVLANPlan(pvState(), 20)
	if p.Noop || len(p.Warn) == 0 {
		t.Fatalf("%+v", p)
	}
	for _, w := range []string{
		`/interface bridge port set [find interface="ether5"] pvid=20 frame-types=admit-only-untagged-and-priority-tagged`,
		`/interface bridge vlan add bridge="bridge" vlan-ids=20 tagged="ether1,ether6" untagged="ether5"`,
		`/interface bridge vlan add bridge="bridge" vlan-ids=30 tagged="ether1,ether6" untagged=""`,
		`/interface bridge set [find name="bridge"] vlan-filtering=yes`,
	} {
		if !strings.Contains(p.Apply, w) {
			t.Errorf("fehlt %s in\n%s", w, p.Apply)
		}
	}
	// Der Filter geht zuletzt an und beim Rückweg zuerst aus.
	if strings.Index(p.Apply, "vlan-filtering=yes") < strings.Index(p.Apply, "vlan-ids=30") {
		t.Error(p.Apply)
	}
	if !strings.HasPrefix(p.Undo, `:do { /interface bridge set [find name="bridge"] vlan-filtering=no`) ||
		!strings.Contains(p.Undo, `pvid=1 frame-types=admit-all`) || !strings.Contains(p.Undo, `vlan remove [find where bridge="bridge" and vlan-ids=20`) {
		t.Error(p.Undo)
	}
}

func TestPortVLANNoopAfterApply(t *testing.T) {
	// Die Prüfung nach dem Ausführen muss „nichts zu tun“ ergeben, sonst rollt doChange zurück.
	st := pvState()
	st.PVID, st.Frames, st.Filtering = 20, "admit-only-untagged-and-priority-tagged", true
	st.PVIDs["ether5"], st.Frame["ether5"] = 20, st.Frames
	st.Rows[20] = &pvBridgeVLAN{ID: "*1", VLAN: 20, Tagged: []string{"ether6", "ether1"}, Untagged: []string{"ether5"}}
	st.Rows[30] = &pvBridgeVLAN{ID: "*2", VLAN: 30, Tagged: []string{"ether1", "ether6"}}
	if p := buildPortVLANPlan(st, 20); !p.Noop {
		t.Fatalf("%+v\n%s", p, p.Apply)
	}
}

func TestPortVLANBackToOne(t *testing.T) {
	st := pvState()
	st.PVID, st.Frames, st.Filtering = 20, "admit-only-untagged-and-priority-tagged", true
	st.Rows[20] = &pvBridgeVLAN{ID: "*1", VLAN: 20, Tagged: []string{"ether1", "ether6"}, Untagged: []string{"ether5"}}
	st.Rows[30] = &pvBridgeVLAN{ID: "*2", VLAN: 30, Tagged: []string{"ether1", "ether6"}}
	p := buildPortVLANPlan(st, 1)
	for _, w := range []string{`pvid=1 frame-types=admit-all`, `numbers=*1 tagged="ether1,ether6,ether5" untagged=""`, `numbers=*2 tagged="ether1,ether6,ether5"`} {
		if !strings.Contains(p.Apply, w) {
			t.Errorf("fehlt %s in\n%s", w, p.Apply)
		}
	}
	if strings.Contains(p.Apply, "vlan-filtering") {
		t.Error(p.Apply)
	}
}

func TestPortVLANOneWithoutFilterIsNoop(t *testing.T) {
	if p := buildPortVLANPlan(pvState(), 1); !p.Noop {
		t.Fatalf("%+v", p)
	}
}

func TestPortVLANKeepsForeignRowMembers(t *testing.T) {
	// Einträge des zentralen WLANs (wifi1) dürfen nicht verloren gehen.
	st := pvState()
	st.Filtering = true
	st.Rows[20] = &pvBridgeVLAN{ID: "*1", VLAN: 20, Tagged: []string{"bridge", "wifi1"}}
	p := buildPortVLANPlan(st, 20)
	if !strings.Contains(p.Apply, `numbers=*1 tagged="bridge,wifi1,ether1,ether6" untagged="ether5"`) {
		t.Fatal(p.Apply)
	}
}
