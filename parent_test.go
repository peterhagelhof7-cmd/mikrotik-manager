package main

import (
	"strings"
	"testing"
	"time"
)

func TestParentsFrom(t *testing.T) {
	now := time.Now()
	ap := &TopoNode{ID: "AP", Identity: "AP", Uplink: "ether1"}
	mesh := &TopoNode{ID: "M", Identity: "Mesh", Uplink: "mm-mesh-sta"}
	sw := &TopoNode{ID: "SW", Identity: "Switch", Uplink: "sfp1", Ports: []*TopoPort{{Name: "ether8", Children: []*TopoNode{ap}}}}
	apNode := &TopoNode{ID: "W", Identity: "wAP", Uplink: "ether1", Ports: []*TopoPort{{Name: "mm-mesh-ap", Children: []*TopoNode{mesh}}}}
	root := &TopoNode{Identity: "Fremdgerät", Ports: []*TopoPort{{Children: []*TopoNode{sw, apNode}}}}
	got := parentsFrom(Topology{Root: root}, now)
	if a := got["AP"]; a.ID != "SW" || a.Port != "ether8" || a.Uplink != "ether1" || a.Mesh {
		t.Fatalf("%+v", a)
	}
	if m := got["M"]; m.ID != "W" || !m.Mesh {
		t.Fatalf("%+v", m)
	}
	if s := got["SW"]; s.ID != "" || s.Identity != "Fremdgerät" {
		t.Fatalf("%+v", s)
	}
	if len(parentsFrom(Topology{}, now)) != 0 {
		t.Fatal("ohne Wurzel")
	}
}

func TestMeshWatchLinkBackAtOnce(t *testing.T) {
	// Loop Protect sperrt den Port nach Sekunden. Wartet der Wächter auf stabilen Link, kommt er
	// nie aus dem Meshlink heraus (Schleife am CRS328 nach einem Neustart, 11-06).
	back, lost := strings.Index(meshWatch, "Etherlink wieder da"), strings.Index(meshWatch, "kein Etherlink")
	delay := strings.Index(meshWatch, ":delay")
	if back < 0 || lost < 0 || delay < back || lost < delay {
		t.Fatal(meshWatch)
	}
}
