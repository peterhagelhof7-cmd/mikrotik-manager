package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Infofläche der Geräteseite: „verbunden mit <parent>“. Der Elternteil kommt aus dem
// Netzwerk-Baum. Er bleibt am Gerät gespeichert, denn ein Gerät, das offline ist, sagt
// nichts mehr: Genau dann zählt „zuletzt verbunden mit <parent>“.

// DeviceParent ist das zuletzt bekannte Gerät oberhalb.
type DeviceParent struct {
	ID       string    `json:"id,omitempty"` // leer = Fremdgerät
	Identity string    `json:"identity"`
	Port     string    `json:"port,omitempty"`   // Port am Elternteil
	Uplink   string    `json:"uplink,omitempty"` // eigener Port nach oben
	Mesh     bool      `json:"mesh,omitempty"`   // über Meshlink
	At       time.Time `json:"at"`
}

// parentInfo ist die Antwort für die Geräteseite.
type parentInfo struct {
	Connected bool          `json:"connected"`
	Root      bool          `json:"root,omitempty"` // das Gerät ist die Wurzel des Baums
	Parent    *DeviceParent `json:"parent,omitempty"`
}

// parentsFrom liest die Elternteile aus dem Baum.
func parentsFrom(t Topology, now time.Time) map[string]DeviceParent {
	out := map[string]DeviceParent{}
	var walk func(n *TopoNode)
	walk = func(n *TopoNode) {
		for _, p := range n.Ports {
			for _, c := range p.Children {
				if c.ID != "" {
					out[c.ID] = DeviceParent{ID: n.ID, Identity: n.Identity, Port: p.Name, Uplink: c.Uplink,
						Mesh: isMesh(c.Uplink) || isMesh(p.Name), At: now}
				}
				walk(c)
			}
		}
	}
	if t.Root != nil {
		walk(t.Root)
	}
	return out
}

var parentMu sync.Mutex

// DeviceParentInfo liest den Baum des Bereichs und speichert die Elternteile aller Geräte. Die
// Geräteseite fragt das beim Öffnen und danach jede Minute ab.
func (p *Poller) DeviceParentInfo(ctx context.Context, id string) (parentInfo, error) {
	parentMu.Lock()
	defer parentMu.Unlock()
	d, ok := p.st.Get(id)
	if !ok {
		return parentInfo{}, fmt.Errorf("Gerät unbekannt")
	}
	ids, root := p.topoScope(d)
	t := p.Topology(ctx, ids, root)
	now := time.Now()
	found := parentsFrom(t, now)
	for x, par := range found {
		par := par
		p.st.Update(x, func(c *Device) { c.Parent = &par })
	}
	d, _ = p.st.Get(id)
	if _, here := found[id]; here {
		return parentInfo{Connected: true, Parent: d.Parent}, nil
	}
	if t.Root != nil && t.Root.ID == id {
		return parentInfo{Connected: true, Root: true}, nil
	}
	return parentInfo{Parent: d.Parent}, nil
}

func (w *web) deviceParent(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	info, err := w.pl.DeviceParentInfo(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, info)
}
