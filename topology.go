package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Netzwerk-Baum (MAC-Liste je Standort): Wurzel ist der Router oder der zentrale
// Switch; darunter je Port die dort gelernten MAC-Adressen und die weiteren
// verwalteten Geräte mit ihren Ports. Nur lesend:
//   - /interface/bridge/host  → welche MAC an welchem Port (inkl. WLAN-Clients)
//   - /ip/neighbor (MNDP/LLDP) → welches verwaltete Gerät an welchem Port hängt
//   - /interface              → eigene MACs der Geräte (Zuordnung MAC → Gerät)
//   - /ip/arp, DHCP-Leases     → IP und Hostname zu einer MAC

// TopoForeign als Wurzel: ein Fremdgerät (Router/Switch eines anderen Herstellers)
// steht oben, alle verwalteten Geräte eine Stufe darunter.
const TopoForeign = "@fremd"

// topoRaw sind die gelesenen Daten eines Geräts.
type topoRaw struct {
	Gateways            []string // aktive Standardrouten (Gateway-IP) – für die Richtung „nach oben“
	ID, Identity, Model string
	Err                 string
	OwnMACs             []string
	Hosts               []map[string]string // mac-address, on-interface, vid, local
	Neighbors           []map[string]string // interface, mac-address, identity
	Ports               []map[string]string // name, type, running, mac-address
	ARP                 []map[string]string // address, mac-address
	Leases              []map[string]string // address, mac-address, host-name
}

// TopoMAC ist ein Eintrag am Port.
type TopoMAC struct {
	MAC      string `json:"mac"`
	IP       string `json:"ip,omitempty"`
	Host     string `json:"host,omitempty"`
	VLAN     string `json:"vlan,omitempty"`
	Wireless bool   `json:"wireless,omitempty"`
}

// TopoPort ist ein Port eines Geräts im Baum.
type TopoPort struct {
	Name     string      `json:"name"`
	Type     string      `json:"type,omitempty"` // ether, wifi, …
	Link     bool        `json:"link"`
	MACs     []TopoMAC   `json:"macs"`
	Children []*TopoNode `json:"children,omitempty"`
}

// TopoNode ist ein verwaltetes Gerät im Baum.
type TopoNode struct {
	ID       string      `json:"id"`
	Identity string      `json:"identity"`
	Model    string      `json:"model,omitempty"`
	Uplink   string      `json:"uplink,omitempty"` // Port Richtung Wurzel
	Ports    []*TopoPort `json:"ports"`
	Err      string      `json:"error,omitempty"`
}

// Topology ist das Ergebnis für einen Standort.
type Topology struct {
	Root       *TopoNode   `json:"root"`
	RootAuto   bool        `json:"rootAuto"` // Wurzel automatisch gewählt
	Unattached []*TopoNode `json:"unattached,omitempty"`
	Unreached  []TopoNode  `json:"unreached,omitempty"` // nicht lesbar
	At         time.Time   `json:"at"`
}

func normMAC(m string) string { return strings.ToUpper(strings.TrimSpace(m)) }

// physPort: Nachbar-Schnittstelle „ether2,bridge“ o. ä. → physischer Port.
func physPort(iface string, ports map[string]bool) string {
	for _, p := range strings.Split(iface, ",") {
		p = strings.TrimSpace(p)
		if i := strings.LastIndex(p, "/"); i >= 0 {
			p = p[i+1:]
		}
		if ports[p] {
			return p
		}
	}
	return ""
}

// BuildTopology baut den Baum (rein rechnend, testbar). root "" = automatisch.
func BuildTopology(raws []topoRaw, root string) Topology {
	t := Topology{At: time.Now()}
	byID := map[string]*topoRaw{}
	owner := map[string]string{} // MAC → Gerät
	var ok []*topoRaw
	for i := range raws {
		r := &raws[i]
		if r.Err != "" {
			t.Unreached = append(t.Unreached, TopoNode{ID: r.ID, Identity: r.Identity, Model: r.Model, Err: r.Err})
			continue
		}
		byID[r.ID] = r
		ok = append(ok, r)
		for _, m := range r.OwnMACs {
			owner[normMAC(m)] = r.ID
		}
	}
	if len(ok) == 0 {
		return t
	}
	isPort := map[string]map[string]bool{}
	for _, r := range ok {
		isPort[r.ID] = map[string]bool{}
		for _, p := range r.Ports {
			isPort[r.ID][p["name"]] = true
		}
	}
	// Kanten aus den Nachbarn: (A, Port) → B
	type edge struct{ from, port, to string }
	var edges []edge
	for _, r := range ok {
		for _, n := range r.Neighbors {
			b := owner[normMAC(n["mac-address"])]
			if b == "" {
				for _, o := range ok {
					if o.Identity != "" && o.Identity == n["identity"] {
						b = o.ID
					}
				}
			}
			if b == "" || b == r.ID {
				continue
			}
			if p := physPort(n["interface"], isPort[r.ID]); p != "" {
				edges = append(edges, edge{r.ID, p, b})
			}
		}
	}
	foreign := root == TopoForeign
	// Wurzel: vorgegeben, sonst das Gerät mit den meisten Nachbar-Kanten
	if _, found := byID[root]; !found && !foreign {
		t.RootAuto = true
		cnt := map[string]int{}
		for _, e := range edges {
			cnt[e.from]++
		}
		ids := make([]string, 0, len(ok))
		for _, r := range ok {
			ids = append(ids, r.ID)
		}
		sort.SliceStable(ids, func(a, b int) bool {
			if cnt[ids[a]] != cnt[ids[b]] {
				return cnt[ids[a]] > cnt[ids[b]]
			}
			return byID[ids[a]].Identity < byID[ids[b]].Identity
		})
		root = ids[0]
	}
	// IP/Hostname zu MACs (aus allen Geräten)
	ipOf, hostOf := map[string]string{}, map[string]string{}
	for _, r := range ok {
		for _, a := range r.ARP {
			if m := normMAC(a["mac-address"]); m != "" && a["address"] != "" {
				ipOf[m] = a["address"]
			}
		}
	}
	for _, r := range ok {
		for _, l := range r.Leases {
			m := normMAC(l["mac-address"])
			if l["address"] != "" {
				ipOf[m] = l["address"]
			}
			if l["host-name"] != "" {
				hostOf[m] = l["host-name"]
			}
		}
	}
	var parent, parentPort, uplink map[string]string
	if foreign {
		parent, parentPort, uplink = foreignTree(ok, owner)
	} else {
		// Breitensuche: Geräte an ihren Eltern-Port hängen
		parent = map[string]string{root: ""}
		parentPort = map[string]string{}
		queue := []string{root}
		for len(queue) > 0 {
			a := queue[0]
			queue = queue[1:]
			for _, e := range edges {
				if e.from == a {
					if _, seen := parent[e.to]; !seen {
						parent[e.to], parentPort[e.to] = a, e.port
						queue = append(queue, e.to)
					}
				}
			}
		}
		// Uplink eines Geräts: Port, an dem es den Eltern-Knoten sieht (Nachbar, sonst MAC-Tabelle)
		upl := func(id string) string {
			par := parent[id]
			if par == "" {
				return ""
			}
			for _, e := range edges {
				if e.from == id && e.to == par {
					return e.port
				}
			}
			for _, h := range byID[id].Hosts {
				if owner[normMAC(h["mac-address"])] == par {
					return h["on-interface"]
				}
			}
			return ""
		}
		depth := func(id string) int {
			n := 0
			for p := parent[id]; p != ""; p = parent[p] {
				n++
			}
			return n
		}
		// Ersatz ohne Nachbar-Eintrag: dort einhängen, wo die MAC des Geräts am tiefsten
		// gelernt wird – aber nie über den Uplink eines Geräts (dort steht alles von oben)
		for changed := true; changed; {
			changed = false
			for _, r := range ok {
				if _, in := parent[r.ID]; in {
					continue
				}
				best, bestPort, bestDepth := "", "", -1
				for _, x := range ok {
					if _, in := parent[x.ID]; !in || x.ID == r.ID {
						continue
					}
					ux := upl(x.ID)
					for _, h := range x.Hosts {
						if owner[normMAC(h["mac-address"])] == r.ID && h["on-interface"] != ux && depth(x.ID) > bestDepth {
							best, bestPort, bestDepth = x.ID, h["on-interface"], depth(x.ID)
						}
					}
				}
				if best != "" {
					parent[r.ID], parentPort[r.ID] = best, bestPort
					changed = true
				}
			}
		}
		uplink = map[string]string{}
		for id := range parent {
			uplink[id] = upl(id)
		}
	}
	children := map[string][]string{}
	for id, par := range parent {
		if par != "" {
			children[par] = append(children[par], id)
		}
	}
	// MACs, die ein Gerät selbst „unten“ sieht (nicht über seinen Uplink) – rekursiv
	below := map[string]map[string]bool{}
	var collect func(id string) map[string]bool
	collect = func(id string) map[string]bool {
		if b, done := below[id]; done {
			return b
		}
		b := map[string]bool{}
		for _, m := range byID[id].OwnMACs {
			b[normMAC(m)] = true
		}
		for _, h := range byID[id].Hosts {
			if h["on-interface"] != uplink[id] && h["local"] != "true" {
				b[normMAC(h["mac-address"])] = true
			}
		}
		for _, c := range children[id] {
			for m := range collect(c) {
				b[m] = true
			}
		}
		below[id] = b
		return b
	}
	var build func(id string) *TopoNode
	build = func(id string) *TopoNode {
		r := byID[id]
		n := &TopoNode{ID: id, Identity: r.Identity, Model: r.Model, Uplink: uplink[id]}
		ports := map[string]*TopoPort{}
		var order []string
		get := func(name string) *TopoPort {
			if p, ok := ports[name]; ok {
				return p
			}
			p := &TopoPort{Name: name, MACs: []TopoMAC{}}
			for _, x := range r.Ports {
				if x["name"] == name {
					p.Type, p.Link = x["type"], x["running"] == "true"
				}
			}
			ports[name] = p
			order = append(order, name)
			return p
		}
		for _, x := range r.Ports {
			if x["type"] == "ether" || x["type"] == "wifi" || x["type"] == "wlan" {
				get(x["name"])
			}
		}
		// Kinder an ihren Port, und was sie (und darunter) selbst sehen
		hidden := map[string]map[string]bool{} // Port → MACs, die ein Kind dahinter selbst zeigt
		for _, c := range children[id] {
			pp := parentPort[c]
			if hidden[pp] == nil {
				hidden[pp] = map[string]bool{}
			}
			for m := range collect(c) {
				hidden[pp][m] = true
			}
		}
		sort.Strings(children[id])
		for _, c := range children[id] {
			p := get(parentPort[c])
			p.Children = append(p.Children, build(c))
		}
		for _, h := range r.Hosts {
			port := h["on-interface"]
			m := normMAC(h["mac-address"])
			if h["local"] == "true" || port == uplink[id] || hidden[port][m] || owner[m] != "" {
				continue
			}
			p := get(port)
			p.MACs = append(p.MACs, TopoMAC{MAC: m, IP: ipOf[m], Host: hostOf[m], VLAN: h["vid"], Wireless: p.Type == "wifi" || p.Type == "wlan"})
		}
		sort.Strings(order)
		for _, name := range order {
			if name == uplink[id] {
				continue
			}
			p := ports[name]
			sort.Slice(p.MACs, func(a, b int) bool { return p.MACs[a].MAC < p.MACs[b].MAC })
			n.Ports = append(n.Ports, p)
		}
		sort.SliceStable(n.Ports, func(a, b int) bool { return natLess(n.Ports[a].Name, n.Ports[b].Name) })
		return n
	}
	if foreign {
		// Virtuelle Wurzel: alle Geräte ohne verwaltetes Gerät darüber hängen direkt am
		// Fremdgerät; dazu die MACs, die nur über deren Uplinks gesehen werden.
		vr := &TopoNode{Identity: "Fremdgerät", Model: "Router/Switch eines anderen Herstellers"}
		port := &TopoPort{Link: true, MACs: []TopoMAC{}}
		top := append([]string{}, children[TopoForeign]...)
		sort.Strings(top)
		seen := map[string]bool{}
		for _, c := range top {
			port.Children = append(port.Children, build(c))
			for m := range collect(c) {
				seen[m] = true
			}
		}
		for _, c := range top {
			for _, h := range byID[c].Hosts {
				m := normMAC(h["mac-address"])
				if h["local"] == "true" || h["on-interface"] != uplink[c] || seen[m] || owner[m] != "" {
					continue
				}
				seen[m] = true
				port.MACs = append(port.MACs, TopoMAC{MAC: m, IP: ipOf[m], Host: hostOf[m], VLAN: h["vid"]})
			}
		}
		sort.Slice(port.MACs, func(a, b int) bool { return port.MACs[a].MAC < port.MACs[b].MAC })
		vr.Ports = []*TopoPort{port}
		t.Root = vr
	} else {
		t.Root = build(root)
	}
	for _, r := range ok {
		if _, in := parent[r.ID]; !in {
			t.Unattached = append(t.Unattached, &TopoNode{ID: r.ID, Identity: r.Identity, Model: r.Model})
		}
	}
	sort.Slice(t.Unattached, func(a, b int) bool { return t.Unattached[a].Identity < t.Unattached[b].Identity })
	return t
}

// readTopo liest die Daten eines Geräts (nur print).
func readTopo(ctx context.Context, cl *routeros.Client) topoRaw {
	var r topoRaw
	r.Ports, _ = printAll(ctx, cl, "/interface/print", "=.proplist=name,type,running,mac-address")
	// wifi-Paket: „running“ heißt dort „Client verbunden“ – ob gesendet wird, sagt monitor
	var wifi []string
	for _, p := range r.Ports {
		if p["type"] == "wifi" {
			wifi = append(wifi, p["name"])
		}
	}
	if len(wifi) > 0 {
		if mon, err := printAll(ctx, cl, "/interface/wifi/monitor", "=numbers="+strings.Join(wifi, ","), "=once="); err == nil {
			state := map[string]string{}
			for i, m := range mon {
				n := m["name"]
				if n == "" && i < len(wifi) {
					n = wifi[i]
				}
				state[n] = strings.ToLower(m["state"])
			}
			for _, p := range r.Ports {
				if st, ok := state[p["name"]]; ok && p["type"] == "wifi" {
					on := strings.HasPrefix(st, "running") || st == "authorized" || strings.HasPrefix(st, "connected")
					p["running"] = map[bool]string{true: "true", false: "false"}[on]
				}
			}
		}
	}
	if rt, err := printAll(ctx, cl, "/ip/route/print", "?dst-address=0.0.0.0/0", "=.proplist=gateway,active"); err == nil {
		for _, x := range rt {
			if x["active"] == "true" && x["gateway"] != "" {
				r.Gateways = append(r.Gateways, x["gateway"])
			}
		}
	}
	for _, p := range r.Ports {
		if m := p["mac-address"]; m != "" {
			r.OwnMACs = append(r.OwnMACs, m)
		}
	}
	r.Hosts, _ = printAll(ctx, cl, "/interface/bridge/host/print", "=.proplist=mac-address,on-interface,vid,local")
	r.Neighbors, _ = printAll(ctx, cl, "/ip/neighbor/print", "=.proplist=interface,mac-address,identity")
	r.ARP, _ = printAll(ctx, cl, "/ip/arp/print", "=.proplist=address,mac-address")
	r.Leases, _ = printAll(ctx, cl, "/ip/dhcp-server/lease/print", "=.proplist=address,mac-address,host-name")
	return r
}

// Topology liest die Geräte (parallel) und baut den Baum.
func (p *Poller) Topology(ctx context.Context, ids []string, root string) Topology {
	raws := make([]topoRaw, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		d, _ := p.st.Get(id)
		raws[i] = topoRaw{ID: id, Identity: d.Identity, Model: firstNonEmpty(d.Model, d.Board)}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl, err := p.session(ctx, id)
			if err != nil {
				raws[i].Err = err.Error()
				return
			}
			defer cl.Close()
			x := readTopo(ctx, cl)
			x.ID, x.Identity, x.Model = raws[i].ID, raws[i].Identity, raws[i].Model
			raws[i] = x
		}()
	}
	wg.Wait()
	return BuildTopology(raws, root)
}

// --- gewählte Wurzel je Bereich (mm: "", Cloud: Kunde/Standort) ---

var topoRootMu sync.Mutex

func (p *Poller) topoRootPath() string { return filepath.Join(p.cfg.DataDir, "topology-root.json") }

func (p *Poller) TopoRoot(key string) string {
	topoRootMu.Lock()
	defer topoRootMu.Unlock()
	m := map[string]string{}
	if b, err := os.ReadFile(p.topoRootPath()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m[key]
}

func (p *Poller) SetTopoRoot(key, id string) error {
	topoRootMu.Lock()
	defer topoRootMu.Unlock()
	m := map[string]string{}
	if b, err := os.ReadFile(p.topoRootPath()); err == nil {
		json.Unmarshal(b, &m)
	}
	if id == "" {
		delete(m, key)
	} else {
		m[key] = id
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return writeFileAtomic(p.topoRootPath(), b, 0o600)
}

// foreignTree bestimmt Eltern und Uplinks ohne verwaltete Wurzel (Fremdgerät oben).
// Uplink = Port, an dem die MAC des Standard-Gateways gelernt wird. Vorfahren eines
// Geräts X = Geräte, die X „unten“ sehen (nicht über ihren Uplink); Elternteil = der
// tiefste Vorfahr (der selbst die meisten Vorfahren hat). Ohne Vorfahren: direkt am
// Fremdgerät. Schleifen (fehlende Gateway-Information) werden aufgelöst.
func foreignTree(ok []*topoRaw, owner map[string]string) (parent, parentPort, uplink map[string]string) {
	gw := map[string]bool{}
	for _, r := range ok {
		for _, g := range r.Gateways {
			for _, a := range r.ARP {
				if a["address"] == g {
					gw[normMAC(a["mac-address"])] = true
				}
			}
		}
	}
	up0 := map[string]string{}
	for _, r := range ok {
		for _, h := range r.Hosts {
			if gw[normMAC(h["mac-address"])] {
				up0[r.ID] = h["on-interface"]
				break
			}
		}
	}
	anc := map[string]map[string]string{} // X → Vorfahr Y → Port bei Y
	for _, y := range ok {
		for _, h := range y.Hosts {
			x := owner[normMAC(h["mac-address"])]
			if x == "" || x == y.ID || h["local"] == "true" || (up0[y.ID] != "" && h["on-interface"] == up0[y.ID]) {
				continue
			}
			if anc[x] == nil {
				anc[x] = map[string]string{}
			}
			anc[x][y.ID] = h["on-interface"]
		}
	}
	parent, parentPort, uplink = map[string]string{}, map[string]string{}, map[string]string{}
	for _, x := range ok {
		best, bestN := "", -1
		for y := range anc[x.ID] {
			if n := len(anc[y]); n > bestN || (n == bestN && y < best) {
				best, bestN = y, n
			}
		}
		if best == "" {
			parent[x.ID] = TopoForeign
		} else {
			parent[x.ID], parentPort[x.ID] = best, anc[x.ID][best]
		}
	}
	// Schleifen auflösen: wer nicht in höchstens len(ok) Schritten oben ankommt, hängt am Fremdgerät
	for _, x := range ok {
		cur := x.ID
		for i := 0; i <= len(ok) && cur != TopoForeign; i++ {
			cur = parent[cur]
		}
		if cur != TopoForeign {
			parent[x.ID] = TopoForeign
			delete(parentPort, x.ID)
		}
	}
	for _, x := range ok {
		u := up0[x.ID]
		if u == "" && parent[x.ID] != TopoForeign {
			for _, h := range x.Hosts {
				if owner[normMAC(h["mac-address"])] == parent[x.ID] {
					u = h["on-interface"]
					break
				}
			}
		}
		uplink[x.ID] = u
	}
	return parent, parentPort, uplink
}
