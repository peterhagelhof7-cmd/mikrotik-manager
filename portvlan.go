package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-routeros/routeros/v3"
)

// Untagged VLAN je Ethernet-Port auf der Geräteseite: Bridge oder ein VLAN des Routers.
//
// Praktisch alle Ports hängen in einer Bridge ohne VLAN-Filter, dort wirkt ein Port-VLAN nicht.
// Bei einem VLAN wandert der Port deshalb aus der Hauptbridge in eine eigene Bridge
// bridge-vlan<N>, die über die VLAN-Schnittstelle vlan<N> an der Hauptbridge hängt: Am Port
// liegt das VLAN ungetaggt an, zum Uplink läuft es getaggt. Dasselbe Muster nutzt die
// WLAN-Umstellung, und es braucht keinen VLAN-Filter. Bei Bridge wandert der Port zurück,
// eine leere VLAN-Bridge wird aufgeräumt. Der Verkehr dieser Ports läuft über die CPU.
// Der Uplink bleibt gesperrt, ebenso der Router, den der Router-Tab einstellt.

const portVLANComment = "mm-portvlan"

var portVLANBridgeRe = regexp.MustCompile(`^bridge-vlan(\d+)$`)

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

// siteVLANs nennt die VLANs ohne das Standard-LAN: Das ist die Hauptbridge.
func siteVLANs(c RouterCfg) []SiteVLAN {
	out := []SiteVLAN{}
	seen := map[int]bool{}
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

// portVLANState ist der Ist-Zustand rund um den Port.
type portVLANState struct {
	Port      string
	Bridge    string            // aktuelle Bridge des Ports
	PVID      int               // aktuelles Port-VLAN
	Main      string            // Hauptbridge
	Bridges   map[string]bool   // vorhandene Bridges
	VLANIfs   map[string]string // VLAN-Schnittstelle → Elternschnittstelle
	Members   map[string]int    // Bridge → Anzahl Ports ohne die VLAN-Schnittstelle
	Filtering bool              // Hauptbridge mit VLAN-Filter
}

func portVLANOf(bridge string) int {
	if m := portVLANBridgeRe.FindStringSubmatch(bridge); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v
	}
	return 0
}

// buildPortVLANPlan: v = 0 heißt Hauptbridge. Der erneute Plan nach dem Ausführen muss
// „nichts zu tun“ ergeben, sonst rollt doChange zurück.
func buildPortVLANPlan(st portVLANState, v int) Plan {
	p := Plan{Action: "portvlan"}
	cm := q(portVLANComment)
	cur := portVLANOf(st.Bridge)
	if cur == v {
		p.Noop = true
		if v == 0 {
			p.Steps = []string{st.Port + " hängt bereits an der Bridge – nichts zu tun"}
		} else {
			p.Steps = []string{fmt.Sprintf("%s liegt bereits in VLAN %d – nichts zu tun", st.Port, v)}
		}
		return p
	}
	var apply, undo strings.Builder
	port := q(st.Port)
	target := st.Main
	if v > 0 {
		target = vlanBridgeName(strconv.Itoa(v))
		vi, br := vlanIfName(strconv.Itoa(v)), target
		if !st.Bridges[br] {
			p.Steps = append(p.Steps, "Bridge "+br+" anlegen")
			apply.WriteString("/interface bridge add name=" + q(br) + " comment=" + cm + "\n")
		}
		if _, ok := st.VLANIfs[vi]; !ok {
			p.Steps = append(p.Steps, fmt.Sprintf("VLAN-Schnittstelle %s auf %s anlegen", vi, st.Main))
			fmt.Fprintf(&apply, "/interface vlan add name=%s interface=%s vlan-id=%d comment=%s\n", q(vi), q(st.Main), v, cm)
			fmt.Fprintf(&apply, "/interface bridge port add bridge=%s interface=%s comment=%s\n", q(br), q(vi), cm)
		}
		if st.Filtering {
			p.Warn = append(p.Warn, fmt.Sprintf("Die Hauptbridge hat einen VLAN-Filter: VLAN %d muss dort für die Bridge und den Uplink getaggt erlaubt sein.", v))
		}
		p.Warn = append(p.Warn, "Der Verkehr dieses Ports läuft über die CPU, nicht über den Switch-Chip.")
		p.Steps = append(p.Steps, fmt.Sprintf("%s aus %s nehmen und in %s legen: VLAN %d liegt dort ungetaggt an", st.Port, orNone(st.Bridge), br, v))
	} else {
		p.Steps = append(p.Steps, fmt.Sprintf("%s aus %s zurück in die Bridge %s", st.Port, st.Bridge, st.Main))
	}
	// Erst raus, dann rein: Ein Port kann nur an einer Bridge hängen.
	apply.WriteString(try("/interface bridge port remove [find interface=" + port + "]"))
	apply.WriteString("/interface bridge port add bridge=" + q(target) + " interface=" + port + " comment=" + cm + "\n")

	// Alte VLAN-Bridge aufräumen, wenn der Port der letzte war.
	if cur > 0 && st.Members[st.Bridge] <= 1 {
		p.Steps = append(p.Steps, "leere Bridge "+st.Bridge+" entfernen")
		apply.WriteString(routerRemoveVLANBridge(cur))
	}

	undo.WriteString(try("/interface bridge port remove [find interface=" + port + "]"))
	if cur > 0 && st.Members[st.Bridge] <= 1 {
		restoreVLANBridge(&undo, cur, st.Main)
	}
	if st.Bridge != "" {
		pv := ""
		if st.PVID > 1 {
			pv = " pvid=" + strconv.Itoa(st.PVID)
		}
		undo.WriteString(try("/interface bridge port add bridge=" + q(st.Bridge) + " interface=" + port + pv))
	}
	if v > 0 {
		vs := strconv.Itoa(v)
		if _, ok := st.VLANIfs[vlanIfName(vs)]; !ok {
			undo.WriteString(routerRemoveVLANBridge(v))
		}
	}
	p.Apply, p.Undo = apply.String(), undo.String()
	return p
}

func routerRemoveVLANBridge(v int) string {
	vs := strconv.Itoa(v)
	vi, br := q(vlanIfName(vs)), q(vlanBridgeName(vs))
	return try("/interface bridge port remove [find interface="+vi+"]") + try("/interface vlan remove [find name="+vi+"]") + try("/interface bridge remove [find name="+br+"]")
}

func restoreVLANBridge(b *strings.Builder, v int, main string) {
	vs := strconv.Itoa(v)
	vi, br := q(vlanIfName(vs)), q(vlanBridgeName(vs))
	cm := q(portVLANComment)
	b.WriteString(try("/interface bridge add name=" + br + " comment=" + cm))
	b.WriteString(try(fmt.Sprintf("/interface vlan add name=%s interface=%s vlan-id=%d comment=%s", vi, q(main), v, cm)))
	b.WriteString(try("/interface bridge port add bridge=" + br + " interface=" + vi + " comment=" + cm))
}

func orNone(s string) string {
	if s == "" {
		return "keiner Bridge"
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
	if v != 0 {
		known := false
		for _, x := range sv {
			known = known || x.ID == v
		}
		if !known {
			pl.Blocked = fmt.Sprintf("VLAN %d ist am Router dieses Standorts nicht eingerichtet.", v)
			return pl, nil
		}
	}
	st := portVLANState{Port: port, Bridges: map[string]bool{}, VLANIfs: map[string]string{}, Members: map[string]int{}}
	br, err := printAll(ctx, cl, "/interface/bridge/print", "=.proplist=name,vlan-filtering")
	if err != nil {
		return pl, err
	}
	filter := map[string]bool{}
	for _, x := range br {
		st.Bridges[x["name"]] = true
		filter[x["name"]] = x["vlan-filtering"] == "true"
		if st.Main == "" && portVLANOf(x["name"]) == 0 {
			st.Main = x["name"]
		}
	}
	if st.Main == "" {
		pl.Blocked = "Das Gerät hat keine Bridge."
		return pl, nil
	}
	st.Filtering = filter[st.Main]
	vl, err := printAll(ctx, cl, "/interface/vlan/print", "=.proplist=name,interface")
	if err != nil {
		return pl, err
	}
	for _, x := range vl {
		st.VLANIfs[x["name"]] = x["interface"]
	}
	bp, err := printAll(ctx, cl, "/interface/bridge/port/print", "=.proplist=interface,bridge,pvid")
	if err != nil {
		return pl, err
	}
	for _, x := range bp {
		if x["interface"] == port {
			st.Bridge = x["bridge"]
			st.PVID, _ = strconv.Atoi(x["pvid"])
		}
		if _, isVLAN := st.VLANIfs[x["interface"]]; !isVLAN {
			st.Members[x["bridge"]]++
		}
	}
	up, ok := uplinkPorts(readTopo(ctx, cl))
	if !ok {
		pl.Blocked = "Der Uplink dieses Geräts ist nicht eindeutig erkennbar. Ohne ihn wäre die Änderung nicht sicher."
		return pl, nil
	}
	if contains(up, port) {
		pl.Blocked = port + " ist der Uplink dieses Geräts. Eine Änderung würde es vom Netz trennen."
		return pl, nil
	}
	return buildPortVLANPlan(st, v), nil
}
