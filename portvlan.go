package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/go-routeros/routeros/v3"
)

// Untagged VLAN je Ethernet-Port auf der Geräteseite. Es gilt dasselbe wie an Access-Ports
// im Router-Tab: VLAN 1 = ungetaggt VLAN 1, alle anderen getaggt. Anderes VLAN = nur dieses
// ungetaggt. Zur Auswahl stehen die VLANs, die der Router des Standorts kennt.
//
// Ohne VLAN-Filter der Bridge wirkt ein Port-VLAN nicht. Der Plan schaltet ihn dann ein und
// trägt die VLANs des Standorts auf allen übrigen offenen Ethernet-Ports getaggt ein, sonst
// bricht Verkehr ab, der bisher durchlief. Der Uplink bleibt gesperrt, ebenso das
// Router-Gerät selbst, das der Router-Tab einstellt.

const portVLANComment = "mm-portvlan"

// SiteVLAN ist ein Eintrag der Auswahl.
type SiteVLAN struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

// SiteVLANs liefert die VLANs des Standorts eines Geräts und ob es selbst der Router ist.
func (p *Poller) SiteVLANs(id string) ([]SiteVLAN, bool, error) {
	d, ok := p.st.Get(id)
	if !ok {
		return nil, false, fmt.Errorf("Gerät unbekannt")
	}
	c := p.Router(routerKeyOf(d))
	return siteVLANs(c), c.Device == id, nil
}

func siteVLANs(c RouterCfg) []SiteVLAN {
	out := []SiteVLAN{{1, "VLAN 1 (Standard-LAN)"}}
	seen := map[int]bool{1: true}
	for _, n := range c.Nets {
		if n.VLAN > 1 && !seen[n.VLAN] {
			seen[n.VLAN] = true
			out = append(out, SiteVLAN{n.VLAN, fmt.Sprintf("VLAN %d (%s)", n.VLAN, n.Address)})
		}
	}
	for _, v := range routerVLANs(c) {
		if !seen[v.ID] {
			seen[v.ID] = true
			out = append(out, SiteVLAN{v.ID, fmt.Sprintf("VLAN %d", v.ID)})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// pvBridgeVLAN ist eine Zeile der VLAN-Tabelle einer Bridge.
type pvBridgeVLAN struct {
	ID               string // .id, leer = neu
	VLAN             int
	Tagged, Untagged []string
}

// portVLANState ist der Ist-Zustand der Bridge, an der der Port hängt.
type portVLANState struct {
	Port, Bridge, Frames string
	PVID                 int
	Filtering            bool
	Ethers               []string // Ethernet-Ports dieser Bridge
	Uplinks              []string
	Frame                map[string]string // Port → frame-types
	PVIDs                map[string]int
	Rows                 map[int]*pvBridgeVLAN
	Site                 []int // VLANs des Standorts ohne VLAN 1
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func listEqual(a, b []string) bool {
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, ",") == strings.Join(y, ",")
}

func listWithout(l []string, x string) []string {
	var out []string
	for _, v := range l {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

func listWith(l []string, x string) []string {
	if contains(l, x) {
		return l
	}
	return append(append([]string{}, l...), x)
}

// buildPortVLANPlan ist der rechnende Teil. Der erneute Plan nach dem Ausführen muss
// „nichts zu tun“ ergeben: Er setzt denselben Zustand voraus, den das Skript herstellt.
func buildPortVLANPlan(st portVLANState, v int) Plan {
	p := Plan{Action: "portvlan"}
	frames := "admit-all"
	if v != 1 {
		frames = "admit-only-untagged-and-priority-tagged"
	}
	ids := map[int]bool{}
	for _, x := range st.Site {
		ids[x] = true
	}
	if v != 1 {
		ids[v] = true
	}
	var want []int
	for x := range ids {
		want = append(want, x)
	}
	sort.Ints(want)

	var apply, undo strings.Builder
	var changed []string
	if st.PVID != v || st.Frames != frames {
		changed = append(changed, fmt.Sprintf("%s: VLAN %d ungetaggt", st.Port, v))
		find := "[find interface=" + q(st.Port) + "]"
		fmt.Fprintf(&apply, "/interface bridge port set %s pvid=%d frame-types=%s\n", find, v, frames)
		undo.WriteString(try(fmt.Sprintf("/interface bridge port set %s pvid=%d frame-types=%s", find, st.PVID, st.Frames)))
	}
	if st.Filtering || v != 1 {
		// Offene Ports tragen alle VLANs getaggt; sobald der Filter an ist, gilt das nur noch per Tabelle.
		var open []string
		for _, e := range st.Ethers {
			if e == st.Port {
				continue
			}
			if st.PVIDs[e] == 1 && (st.Frame[e] == "admit-all" || st.Frame[e] == "") || contains(st.Uplinks, e) {
				open = append(open, e)
			}
		}
		for _, id := range want {
			r := st.Rows[id]
			if r == nil {
				r = &pvBridgeVLAN{VLAN: id}
			}
			tagged, untagged := r.Tagged, listWithout(r.Untagged, st.Port)
			tagged = listWithout(tagged, st.Port)
			for _, e := range open {
				if !contains(untagged, e) {
					tagged = listWith(tagged, e)
				}
			}
			if id == v {
				untagged = listWith(untagged, st.Port)
			} else if v == 1 {
				tagged = listWith(tagged, st.Port)
			}
			if r.ID != "" && listEqual(tagged, r.Tagged) && listEqual(untagged, r.Untagged) {
				continue
			}
			ts, us := strings.Join(tagged, ","), strings.Join(untagged, ",")
			if r.ID == "" {
				fmt.Fprintf(&apply, "/interface bridge vlan add bridge=%s vlan-ids=%d tagged=%s untagged=%s comment=%s\n", q(st.Bridge), id, q(ts), q(us), q(portVLANComment))
				undo.WriteString(try(fmt.Sprintf("/interface bridge vlan remove [find where bridge=%s and vlan-ids=%d and comment=%s]", q(st.Bridge), id, q(portVLANComment))))
			} else {
				fmt.Fprintf(&apply, "/interface bridge vlan set numbers=%s tagged=%s untagged=%s\n", r.ID, q(ts), q(us))
				undo.WriteString(try(fmt.Sprintf("/interface bridge vlan set numbers=%s tagged=%s untagged=%s", r.ID, q(strings.Join(r.Tagged, ",")), q(strings.Join(r.Untagged, ",")))))
			}
			changed = append(changed, fmt.Sprintf("VLAN-Tabelle VLAN %d: getaggt %s, ungetaggt %s", id, orNone(ts), orNone(us)))
		}
	}
	if !st.Filtering && v != 1 {
		changed = append(changed, "VLAN-Filter der Bridge "+st.Bridge+" einschalten")
		apply.WriteString("/interface bridge set [find name=" + q(st.Bridge) + "] vlan-filtering=yes\n")
		// Beim Rückweg zuerst, damit die Tabelle nicht mitten im Umbau greift
		s := try("/interface bridge set [find name=" + q(st.Bridge) + "] vlan-filtering=no")
		u := undo.String()
		undo.Reset()
		undo.WriteString(s + u)
		p.Warn = append(p.Warn, "Der VLAN-Filter ist auf dieser Bridge noch aus und wird eingeschaltet. VLANs, die der Router nicht kennt, laufen danach nicht mehr durch.")
	}
	if len(changed) == 0 {
		p.Noop = true
		p.Steps = []string{fmt.Sprintf("%s ist bereits VLAN %d – nichts zu tun", st.Port, v)}
		return p
	}
	p.Steps, p.Apply, p.Undo = changed, apply.String(), undo.String()
	return p
}

func orNone(s string) string {
	if s == "" {
		return "keine"
	}
	return s
}

func (p *Poller) planPortVLAN(ctx context.Context, cl *routeros.Client, id, port string, v int) (Plan, error) {
	pl := Plan{Action: "portvlan"}
	sv, isRouter, err := p.SiteVLANs(id)
	if err != nil {
		return pl, err
	}
	if isRouter {
		pl.Blocked = "Die Ports des Routers werden im Tab Router eingestellt."
		return pl, nil
	}
	st := portVLANState{Port: port, Frame: map[string]string{}, PVIDs: map[string]int{}, Rows: map[int]*pvBridgeVLAN{}}
	known := false
	for _, x := range sv {
		known = known || x.ID == v
		if x.ID > 1 {
			st.Site = append(st.Site, x.ID)
		}
	}
	if !known {
		pl.Blocked = fmt.Sprintf("VLAN %d ist am Router dieses Standorts nicht eingerichtet.", v)
		return pl, nil
	}
	bp, err := printAll(ctx, cl, "/interface/bridge/port/print", "=.proplist=interface,bridge,pvid,frame-types,dynamic")
	if err != nil {
		return pl, err
	}
	for _, x := range bp {
		if x["interface"] == port {
			st.Bridge, st.Frames = x["bridge"], x["frame-types"]
			st.PVID, _ = strconv.Atoi(x["pvid"])
		}
	}
	if st.Bridge == "" {
		pl.Blocked = port + " hängt an keiner Bridge."
		return pl, nil
	}
	raw := readTopo(ctx, cl)
	up, ok := uplinkPorts(raw)
	if !ok {
		pl.Blocked = "Der Uplink dieses Geräts ist nicht eindeutig erkennbar. Ohne ihn wäre die Änderung nicht sicher."
		return pl, nil
	}
	if contains(up, port) {
		pl.Blocked = port + " ist der Uplink dieses Geräts. Eine Änderung würde es vom Netz trennen."
		return pl, nil
	}
	st.Uplinks = up
	eth := map[string]bool{}
	for _, x := range raw.Ports {
		if x["type"] == "ether" {
			eth[x["name"]] = true
		}
	}
	for _, x := range bp {
		if x["bridge"] == st.Bridge && eth[x["interface"]] {
			st.Ethers = append(st.Ethers, x["interface"])
			st.Frame[x["interface"]] = x["frame-types"]
			st.PVIDs[x["interface"]], _ = strconv.Atoi(x["pvid"])
		}
	}
	sort.Strings(st.Ethers)
	br, err := printAll(ctx, cl, "/interface/bridge/print", "=.proplist=name,vlan-filtering")
	if err != nil {
		return pl, err
	}
	for _, x := range br {
		if x["name"] == st.Bridge {
			st.Filtering = x["vlan-filtering"] == "true"
		}
	}
	rows, err := printAll(ctx, cl, "/interface/bridge/vlan/print", "=.proplist=.id,bridge,vlan-ids,tagged,untagged,dynamic")
	if err != nil {
		return pl, err
	}
	for _, x := range rows {
		n, e := strconv.Atoi(x["vlan-ids"])
		if x["bridge"] == st.Bridge && x["dynamic"] != "true" && e == nil {
			st.Rows[n] = &pvBridgeVLAN{ID: x[".id"], VLAN: n, Tagged: splitList(x["tagged"]), Untagged: splitList(x["untagged"])}
		}
	}
	out := buildPortVLANPlan(st, v)
	if d, ok := p.st.Get(id); ok && !st.Filtering && v != 1 && (strings.HasPrefix(d.Model, "CRS1") || strings.HasPrefix(d.Model, "CRS2") || strings.HasPrefix(d.Board, "CRS1") || strings.HasPrefix(d.Board, "CRS2")) {
		out.Warn = append(out.Warn, "Dieser Switch kann den VLAN-Filter der Bridge nicht im Switch-Chip: Der Verkehr läuft danach über die CPU und ist deutlich langsamer.")
	}
	return out, nil
}
