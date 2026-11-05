package main

import (
	"strings"
	"testing"
)

func pvState() portVLANState {
	return portVLANState{Port: "ether5", Bridge: "bridge", PVID: 1, Main: "bridge",
		Bridges: map[string]bool{"bridge": true}, VLANIfs: map[string]string{}, Members: map[string]int{"bridge": 4}}
}

func TestSiteVLANs(t *testing.T) {
	got := siteVLANs(RouterCfg{Nets: []RouterNet{{Address: "10.0.0.1/24", VLAN: 1}, {Address: "10.0.20.1/24", VLAN: 20}},
		Ports: []RouterPort{{Name: "e1", Role: "access", VLAN: "30"}}})
	if len(got) != 2 || got[0].Label != "VLAN 20 (10.0.20.1/24)" || got[1].Label != "VLAN 30" {
		t.Fatalf("%+v", got)
	}
	if l := siteVLANs(RouterCfg{}); len(l) != 0 {
		t.Fatalf("%+v", l)
	}
}

func TestPortVLANMovesOutOfBridge(t *testing.T) {
	p := buildPortVLANPlan(pvState(), 20)
	if p.Noop {
		t.Fatalf("%+v", p)
	}
	for _, w := range []string{
		`/interface bridge add name="bridge-vlan20"`,
		`/interface vlan add name="vlan20" interface="bridge" vlan-id=20`,
		`/interface bridge port add bridge="bridge-vlan20" interface="vlan20"`,
		`/interface bridge port remove [find interface="ether5"]`,
		`/interface bridge port add bridge="bridge-vlan20" interface="ether5"`,
	} {
		if !strings.Contains(p.Apply, w) {
			t.Errorf("fehlt %s in\n%s", w, p.Apply)
		}
	}
	if strings.Contains(p.Apply, "vlan-filtering") {
		t.Error("der VLAN-Filter bleibt unberührt")
	}
	// Erst aus der alten Bridge, dann in die neue
	if strings.Index(p.Apply, `port remove [find interface="ether5"]`) > strings.Index(p.Apply, `interface="ether5" comment`) {
		t.Error(p.Apply)
	}
	for _, w := range []string{`/interface bridge port add bridge="bridge" interface="ether5"`, `/interface vlan remove [find name="vlan20"]`, `/interface bridge remove [find name="bridge-vlan20"]`} {
		if !strings.Contains(p.Undo, w) {
			t.Errorf("Rückweg ohne %s:\n%s", w, p.Undo)
		}
	}
}

func TestPortVLANNoop(t *testing.T) {
	// Die Prüfung nach dem Ausführen muss „nichts zu tun“ ergeben, sonst rollt doChange zurück.
	st := pvState()
	st.Bridge = "bridge-vlan20"
	if p := buildPortVLANPlan(st, 20); !p.Noop {
		t.Fatalf("%+v", p)
	}
	if p := buildPortVLANPlan(pvState(), 0); !p.Noop {
		t.Fatalf("%+v", p)
	}
}

func TestPortVLANReusesExisting(t *testing.T) {
	// Zweiter Port im selben VLAN: Bridge und Schnittstelle sind schon da.
	st := pvState()
	st.Bridges["bridge-vlan20"] = true
	st.VLANIfs["vlan20"] = "bridge"
	p := buildPortVLANPlan(st, 20)
	if strings.Contains(p.Apply, "/interface bridge add") || strings.Contains(p.Apply, "/interface vlan add") {
		t.Fatal(p.Apply)
	}
	// Der Rückweg darf sie dann auch nicht entfernen.
	if strings.Contains(p.Undo, `/interface vlan remove`) {
		t.Fatal(p.Undo)
	}
}

func TestPortVLANBackToBridge(t *testing.T) {
	st := pvState()
	st.Bridge = "bridge-vlan20"
	st.Bridges["bridge-vlan20"] = true
	st.VLANIfs["vlan20"] = "bridge"
	st.Members["bridge-vlan20"] = 1
	p := buildPortVLANPlan(st, 0)
	for _, w := range []string{`/interface bridge port add bridge="bridge" interface="ether5"`, `/interface vlan remove [find name="vlan20"]`, `/interface bridge remove [find name="bridge-vlan20"]`} {
		if !strings.Contains(p.Apply, w) {
			t.Errorf("fehlt %s in\n%s", w, p.Apply)
		}
	}
	if !strings.Contains(p.Undo, `/interface bridge add name="bridge-vlan20"`) || !strings.Contains(p.Undo, `/interface bridge port add bridge="bridge-vlan20" interface="ether5"`) {
		t.Fatal(p.Undo)
	}
	// Hängt noch ein Port daran, bleibt die Bridge.
	st.Members["bridge-vlan20"] = 2
	if p := buildPortVLANPlan(st, 0); strings.Contains(p.Apply, "/interface bridge remove") {
		t.Fatal(p.Apply)
	}
}

func TestPortVLANOf(t *testing.T) {
	for b, want := range map[string]int{"bridge": 0, "bridge-vlan20": 20, "": 0, "bridge1": 0} {
		if got := portVLANOf(b); got != want {
			t.Errorf("%s: %d", b, got)
		}
	}
}
