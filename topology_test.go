package main

import (
	"strings"
	"testing"
)

// Standort: Router R (ether3: PC1), Switch S an R:ether2 (ether3: PC2, ether4: unverwalteter
// Switch mit PC3+PC4), AP A an S:ether2 (wifi1: Handy1, Handy2; ether2: PC5).
func topoSite(withAPNeighbor bool) []topoRaw {
	h := func(port string, macs ...string) []map[string]string {
		var out []map[string]string
		for _, m := range macs {
			out = append(out, map[string]string{"mac-address": m, "on-interface": port, "vid": "1"})
		}
		return out
	}
	cat := func(l ...[]map[string]string) []map[string]string {
		var out []map[string]string
		for _, x := range l {
			out = append(out, x...)
		}
		return out
	}
	port := func(name, typ string) map[string]string {
		return map[string]string{"name": name, "type": typ, "running": "true"}
	}
	const (
		R, S, A                  = "RR:00:00:00:00:01", "SS:00:00:00:00:01", "AA:00:00:00:00:01"
		PC1, PC2, PC3, PC4, PC5  = "C1:00:00:00:00:01", "C2:00:00:00:00:01", "C3:00:00:00:00:01", "C4:00:00:00:00:01", "C5:00:00:00:00:01"
		Handy1, Handy2, Internet = "H1:00:00:00:00:01", "H2:00:00:00:00:01", "FF:00:00:00:00:01"
	)
	r := topoRaw{ID: "R", Identity: "Router", OwnMACs: []string{R},
		Ports:     []map[string]string{port("ether1", "ether"), port("ether2", "ether"), port("ether3", "ether"), port("bridge", "bridge")},
		Hosts:     cat(h("ether1", Internet), h("ether2", S, A, PC2, PC3, PC4, PC5, Handy1, Handy2), h("ether3", PC1)),
		Neighbors: []map[string]string{{"interface": "ether2,bridge", "mac-address": S, "identity": "Switch"}},
		Leases:    []map[string]string{{"address": "192.168.50.11", "mac-address": PC1, "host-name": "buero-pc"}, {"address": "192.168.50.31", "mac-address": Handy1, "host-name": "handy-anna"}},
		ARP:       []map[string]string{{"address": "192.168.50.12", "mac-address": PC2}}}
	s := topoRaw{ID: "S", Identity: "Switch", OwnMACs: []string{S},
		Ports:     []map[string]string{port("ether1", "ether"), port("ether2", "ether"), port("ether3", "ether"), port("ether4", "ether"), port("ether5", "ether")},
		Hosts:     cat(h("ether1", R, PC1, Internet), h("ether2", A, PC5, Handy1, Handy2), h("ether3", PC2), h("ether4", PC3, PC4)),
		Neighbors: []map[string]string{{"interface": "ether1", "mac-address": R, "identity": "Router"}}}
	if withAPNeighbor {
		s.Neighbors = append(s.Neighbors, map[string]string{"interface": "ether2", "mac-address": A, "identity": "AP"})
	}
	a := topoRaw{ID: "A", Identity: "AP", OwnMACs: []string{A},
		Ports: []map[string]string{port("ether1", "ether"), port("ether2", "ether"), port("wifi1", "wifi")},
		Hosts: cat(h("ether1", R, S, PC1, PC2, PC3, PC4, Internet), h("wifi1", Handy1, Handy2), h("ether2", PC5))}
	if withAPNeighbor {
		a.Neighbors = []map[string]string{{"interface": "ether1", "mac-address": S, "identity": "Switch"}}
	}
	return []topoRaw{r, s, a}
}

func findPort(n *TopoNode, name string) *TopoPort {
	for _, p := range n.Ports {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func macsOf(p *TopoPort) string {
	var l []string
	for _, m := range p.MACs {
		l = append(l, m.MAC[:2])
	}
	return strings.Join(l, ",")
}

func TestTopologyTree(t *testing.T) {
	for _, withNeighbor := range []bool{true, false} {
		tp := BuildTopology(topoSite(withNeighbor), "R")
		r := tp.Root
		if r == nil || r.ID != "R" || tp.RootAuto {
			t.Fatalf("Wurzel: %+v", tp)
		}
		// Router: ether3 = PC1 mit IP/Hostname aus der Lease; ether2 = Switch, keine MACs doppelt
		if p := findPort(r, "ether3"); p == nil || macsOf(p) != "C1" || p.MACs[0].IP != "192.168.50.11" || p.MACs[0].Host != "buero-pc" {
			t.Fatalf("R ether3: %+v", p)
		}
		p2 := findPort(r, "ether2")
		if p2 == nil || len(p2.Children) != 1 || p2.Children[0].ID != "S" || len(p2.MACs) != 0 {
			t.Fatalf("R ether2 (neighbor=%v): %+v", withNeighbor, p2)
		}
		if findPort(r, "bridge") != nil {
			t.Fatal("Bridge ist kein Port im Baum")
		}
		s := p2.Children[0]
		if s.Uplink != "ether1" || findPort(s, "ether1") != nil {
			t.Fatalf("S Uplink: %+v", s)
		}
		if p := findPort(s, "ether3"); macsOf(p) != "C2" || p.MACs[0].IP != "192.168.50.12" {
			t.Fatalf("S ether3: %+v", p)
		}
		if p := findPort(s, "ether4"); macsOf(p) != "C3,C4" {
			t.Fatalf("S ether4 (unverwalteter Switch): %+v", p)
		}
		if p := findPort(s, "ether5"); p == nil || len(p.MACs) != 0 {
			t.Fatalf("S ether5 (leer) fehlt: %+v", p)
		}
		pa := findPort(s, "ether2")
		if pa == nil || len(pa.Children) != 1 || pa.Children[0].ID != "A" || len(pa.MACs) != 0 {
			t.Fatalf("S ether2 → AP (neighbor=%v): %+v", withNeighbor, pa)
		}
		a := pa.Children[0]
		if w := findPort(a, "wifi1"); macsOf(w) != "H1,H2" || !w.MACs[0].Wireless || w.MACs[0].Host != "handy-anna" {
			t.Fatalf("AP wifi1: %+v", w)
		}
		if p := findPort(a, "ether2"); macsOf(p) != "C5" {
			t.Fatalf("AP ether2: %+v", p)
		}
		if len(tp.Unattached) != 0 {
			t.Fatalf("nicht zugeordnet: %+v", tp.Unattached)
		}
	}
}

func TestTopologyRootAndErrors(t *testing.T) {
	// automatisch: Switch hat die meisten Nachbar-Kanten
	tp := BuildTopology(topoSite(true), "")
	if !tp.RootAuto || tp.Root.ID != "S" {
		t.Fatalf("Wurzel automatisch: %+v", tp.Root)
	}
	// Switch als Wurzel: Router hängt an S:ether1 und zeigt PC1 an seinem ether3
	r := findPort(tp.Root, "ether1")
	if r == nil || len(r.Children) != 1 || r.Children[0].ID != "R" || macsOf(findPort(r.Children[0], "ether3")) != "C1" {
		t.Fatalf("Router unter Switch: %+v", r)
	}
	// nicht lesbares Gerät
	raws := topoSite(true)
	raws[2] = topoRaw{ID: "A", Identity: "AP", Err: "nicht erreichbar"}
	tp = BuildTopology(raws, "R")
	if len(tp.Unreached) != 1 || tp.Unreached[0].ID != "A" {
		t.Fatalf("nicht lesbar: %+v", tp.Unreached)
	}
	// Gerät ohne jede Verbindung zum Baum
	raws = topoSite(true)
	raws = append(raws, topoRaw{ID: "X", Identity: "Insel", OwnMACs: []string{"XX:00:00:00:00:01"}})
	tp = BuildTopology(raws, "R")
	if len(tp.Unattached) != 1 || tp.Unattached[0].ID != "X" {
		t.Fatalf("nicht zugeordnet: %+v", tp.Unattached)
	}
	if physPort("ether2,bridge", map[string]bool{"ether2": true}) != "ether2" || physPort("bridge1/ether7", map[string]bool{"ether7": true}) != "ether7" {
		t.Fatal("physPort")
	}
}

// Echter Stand 11-05: Router R (sfp1) → CRS328 C (sfp4 oben) → CRS112 K (ether1 oben,
// ether4) → wAP W; dazu cAP P an C:ether8. MNDP läuft durch die Bridges: Jedes Gerät
// meldet alle anderen als Nachbarn, nur der Port verrät die Richtung.
func chainSite() []topoRaw {
	const R, C, K, W, P = "RR:01", "CC:01", "KK:01", "WW:01", "PP:01"
	port := func(names ...string) []map[string]string {
		var out []map[string]string
		for _, n := range names {
			out = append(out, map[string]string{"name": n, "type": "ether", "running": "true"})
		}
		return out
	}
	dev := func(id, mac string, ports []map[string]string, m map[string]string) topoRaw {
		r := topoRaw{ID: id, Identity: id, OwnMACs: []string{mac}, Ports: ports}
		for other, p := range m {
			r.Hosts = append(r.Hosts, map[string]string{"mac-address": other, "on-interface": p})
			r.Neighbors = append(r.Neighbors, map[string]string{"mac-address": other, "interface": p + ",bridge"})
		}
		return r
	}
	return []topoRaw{
		dev("R", R, port("ether1", "sfp1"), map[string]string{C: "sfp1", K: "sfp1", W: "sfp1", P: "sfp1"}),
		dev("C", C, port("ether1", "ether8", "sfp4"), map[string]string{R: "sfp4", K: "ether1", W: "ether1", P: "ether8"}),
		dev("K", K, port("ether1", "ether4"), map[string]string{R: "ether1", C: "ether1", P: "ether1", W: "ether4"}),
		dev("W", W, port("ether1", "ether2"), map[string]string{R: "ether1", C: "ether1", K: "ether1", P: "ether1"}),
		dev("P", P, port("ether1", "ether2"), map[string]string{R: "ether1", C: "ether1", K: "ether1", W: "ether1"}),
	}
}

// topoShape schreibt den Baum als „Gerät[Uplink] Port>(Kind) …“.
func topoShape(n *TopoNode) string {
	s := n.ID + "[" + n.Uplink + "]"
	for _, p := range n.Ports {
		for _, c := range p.Children {
			s += " " + p.Name + ">(" + topoShape(c) + ")"
		}
	}
	return s
}

func TestTopologyNeighborsThroughBridges(t *testing.T) {
	want := map[string]string{
		"R": "R[] sfp1>(C[sfp4] ether1>(K[ether1] ether4>(W[ether1])) ether8>(P[ether1]))",
		"C": "C[] ether1>(K[ether1] ether4>(W[ether1])) ether8>(P[ether1]) sfp4>(R[sfp1])",
		"K": "K[] ether1>(C[ether1] ether8>(P[ether1]) sfp4>(R[sfp1])) ether4>(W[ether1])",
	}
	for root, w := range want {
		if got := topoShape(BuildTopology(chainSite(), root).Root); got != w {
			t.Errorf("Wurzel %s:\n %s\nerwartet\n %s", root, got, w)
		}
	}
	// Ohne MAC-Tabelle bleibt der Nachbar: Sein Port zeigt dieselbe Richtung.
	raws := chainSite()
	for i := range raws {
		raws[i].Hosts = nil
	}
	if got := topoShape(BuildTopology(raws, "R").Root); got != want["R"] {
		t.Errorf("nur Nachbarn: %s", got)
	}
}

func TestTopologyVLANs(t *testing.T) {
	if got := strings.Join(topoVLANIDs("10, 20-22,x"), ","); got != "10,20,21,22" {
		t.Fatal(got)
	}
	r := topoRaw{
		BridgePorts: []map[string]string{
			{"interface": "ether1", "pvid": "1"},
			{"interface": "ether2", "pvid": "10", "frame-types": "admit-only-untagged-and-priority-tagged"},
			{"interface": "ether3", "pvid": "1", "frame-types": "admit-only-vlan-tagged"},
		},
		BridgeVLANs: []map[string]string{
			{"vlan-ids": "10", "current-tagged": "bridge,ether1,ether3", "current-untagged": "ether2"},
			{"vlan-ids": "20-21", "tagged": "ether3"},
		},
	}
	got := topoPortVLANs(&r)
	for p, want := range map[string]string{"ether1": "1,10", "ether2": "10", "ether3": "10,20,21"} {
		if j := strings.Join(got[p], ","); j != want {
			t.Errorf("%s: %s, erwartet %s", p, j, want)
		}
	}
	raws := chainSite()
	for i := range raws {
		if raws[i].ID == "C" {
			raws[i].BridgeVLANs = []map[string]string{{"vlan-ids": "30", "tagged": "ether8"}}
		}
		if raws[i].ID == "K" {
			raws[i].Hosts = append(raws[i].Hosts, map[string]string{"mac-address": "AA:AA:AA:00:00:01", "on-interface": "ether4", "vid": "40"})
		}
	}
	top := BuildTopology(raws, "R")
	if j := strings.Join(top.VLANs, ","); j != "30,40" {
		t.Fatal(j)
	}
	var port *TopoPort
	var walk func(n *TopoNode)
	walk = func(n *TopoNode) {
		for _, p := range n.Ports {
			if n.ID == "C" && p.Name == "ether8" {
				port = p
			}
			for _, c := range p.Children {
				walk(c)
			}
		}
	}
	walk(top.Root)
	if port == nil || strings.Join(port.VLANs, ",") != "30" {
		t.Fatalf("%+v", port)
	}
}

func TestManagedTreeOddData(t *testing.T) {
	ok := []*topoRaw{{ID: "R"}, {ID: "X"}, {ID: "Y"}, {ID: "Z"}, {ID: "L"}}
	// X und Y sehen einander „unten“ (Schleife), Z sieht die Wurzel nicht.
	parent, port, _ := managedTree(ok, "R", map[string]map[string]string{
		"R": {"X": "e1", "Y": "e2", "Z": "e3"},
		"X": {"R": "e1", "Y": "e2"},
		"Y": {"R": "e1", "X": "e2"},
		"Z": {"L": "e2"},
	})
	if parent["X"] != "R" || parent["Y"] != "X" || port["X"] != "e1" || port["Y"] != "e2" { // Kreis aufgebrochen
		t.Fatalf("Schleife: %v %v", parent, port)
	}
	if parent["Z"] != "R" {
		t.Fatalf("Z: %v", parent)
	}
	if _, in := parent["L"]; in {
		t.Fatalf("Z kennt die Richtung nicht und darf L nicht aufnehmen: %v", parent)
	}
}

// Fremd-Router oben (nicht verwaltet, Gateway 192.168.1.1 = GW): daran Switch S (ether1)
// mit AP A an S:ether2, AP B direkt am Router, PC9 direkt am Router.
func foreignSite(withGateway bool) []topoRaw {
	h := func(port string, macs ...string) []map[string]string {
		var out []map[string]string
		for _, m := range macs {
			out = append(out, map[string]string{"mac-address": m, "on-interface": port})
		}
		return out
	}
	cat := func(l ...[]map[string]string) []map[string]string {
		var out []map[string]string
		for _, x := range l {
			out = append(out, x...)
		}
		return out
	}
	port := func(n string) map[string]string {
		return map[string]string{"name": n, "type": "ether", "running": "true"}
	}
	const S, A, B, GW, PC9, PC2, Handy = "SS:01", "AA:01", "BB:01", "GW:01", "C9:01", "C2:01", "H1:01"
	arp := []map[string]string{{"address": "192.168.1.1", "mac-address": GW}}
	gws := []string{"192.168.1.1"}
	if !withGateway {
		arp, gws = nil, nil
	}
	s := topoRaw{ID: "S", Identity: "Switch", OwnMACs: []string{S}, Gateways: gws, ARP: arp,
		Ports: []map[string]string{port("ether1"), port("ether2"), port("ether3")},
		Hosts: cat(h("ether1", GW, B, PC9), h("ether2", A, Handy), h("ether3", PC2))}
	a := topoRaw{ID: "A", Identity: "AP-OG", OwnMACs: []string{A}, Gateways: gws, ARP: arp,
		Ports: []map[string]string{port("ether1"), port("ether2")},
		Hosts: cat(h("ether1", GW, S, B, PC9, PC2), h("ether2", Handy))}
	b := topoRaw{ID: "B", Identity: "AP-Garten", OwnMACs: []string{B}, Gateways: gws, ARP: arp,
		Ports: []map[string]string{port("ether1")},
		Hosts: h("ether1", GW, S, A, PC9, PC2, Handy)}
	return []topoRaw{s, a, b}
}

func TestTopologyForeignRoot(t *testing.T) {
	for _, withGW := range []bool{true, false} {
		tp := BuildTopology(foreignSite(withGW), TopoForeign)
		r := tp.Root
		if r == nil || r.ID != "" || r.Identity != "Fremdgerät" || tp.RootAuto || len(r.Ports) != 1 {
			t.Fatalf("Wurzel (gw=%v): %+v", withGW, r)
		}
		top := r.Ports[0]
		var names []string
		for _, c := range top.Children {
			names = append(names, c.Identity)
		}
		if withGW {
			// alle Geräte ohne verwaltetes Gerät darüber eine Stufe unter dem Fremdgerät
			if strings.Join(names, ",") != "AP-Garten,Switch" {
				t.Fatalf("direkt am Fremdgerät: %v", names)
			}
			// am Fremdgerät: Gateway und PC9 (nur über die Uplinks gesehen)
			if macsOf(top) != "C9,GW" || top.MACs[1].IP != "192.168.1.1" {
				t.Fatalf("MACs am Fremdgerät: %+v", top.MACs)
			}
			var sw *TopoNode
			for _, c := range top.Children {
				if c.ID == "S" {
					sw = c
				}
			}
			if sw.Uplink != "ether1" {
				t.Fatalf("Uplink Switch: %+v", sw)
			}
			p := findPort(sw, "ether2")
			if p == nil || len(p.Children) != 1 || p.Children[0].ID != "A" || macsOf(findPort(p.Children[0], "ether2")) != "H1" {
				t.Fatalf("AP unter Switch: %+v", p)
			}
			if macsOf(findPort(sw, "ether3")) != "C2" {
				t.Fatalf("PC2 am Switch: %+v", findPort(sw, "ether3"))
			}
		}
		// ohne Gateway-Information: kein Absturz, keine Schleife, alle Geräte im Baum
		count := 0
		var walk func(n *TopoNode)
		walk = func(n *TopoNode) {
			count++
			for _, p := range n.Ports {
				for _, c := range p.Children {
					walk(c)
				}
			}
		}
		walk(r)
		if count != 4 || len(tp.Unattached) != 0 {
			t.Fatalf("gw=%v: %d Knoten, nicht zugeordnet %+v", withGW, count, tp.Unattached)
		}
	}
}
