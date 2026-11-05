package main

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/go-routeros/routeros/v3"
)

// Ports eines Geräts für die Detailseite: alle Ethernet-/SFP-Schnittstellen mit
// Link, Geschwindigkeit, PoE-Ausgang und Bridge. Nur lesend (print/monitor once).

type PortInfo struct {
	Name     string `json:"name"`
	Comment  string `json:"comment,omitempty"`
	Disabled bool   `json:"disabled"`
	SFP      bool   `json:"sfp"`
	Link     bool   `json:"link"`
	Rate     string `json:"rate,omitempty"`   // z. B. 1Gbps
	Duplex   string `json:"duplex,omitempty"` // full | half
	Bridge   string `json:"bridge,omitempty"`
	PVID     int    `json:"pvid,omitempty"` // ungetaggtes VLAN am Bridge-Port
	MAC      string `json:"mac,omitempty"`
	// PoE-Ausgang ("" = Port kann kein PoE-out)
	PoE      string `json:"poe,omitempty"`      // auto-on | forced-on | off
	PoEState string `json:"poeState,omitempty"` // powered-on | waiting-for-load | short-circuit | …
	PoEWatt  string `json:"poeWatt,omitempty"`
	PoEUsed  bool   `json:"poeUsed"` // versorgt gerade ein Gerät
}

func (p *Poller) Ports(ctx context.Context, id string) ([]PortInfo, error) {
	cl, err := p.session(ctx, id)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	return readPorts(ctx, cl)
}

func readPorts(ctx context.Context, cl *routeros.Client) ([]PortInfo, error) {
	eth, err := printAll(ctx, cl, "/interface/ethernet/print", "=.proplist=name,default-name,comment,disabled,running,mac-address")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range eth {
		names = append(names, m["name"])
	}
	var mon, poe, poeMon, br []map[string]string
	if len(names) > 0 {
		// Liefert je Port status/rate/full-duplex; scheitert es, reicht „running“.
		mon, _ = printAll(ctx, cl, "/interface/ethernet/monitor", "=numbers="+strings.Join(names, ","), "=once=")
	}
	// Nur Geräte mit PoE-Ausgang kennen das Menü; sonst Fehler → kein PoE.
	if poe, err = printAll(ctx, cl, "/interface/ethernet/poe/print", "=.proplist=name,poe-out"); err == nil && len(poe) > 0 {
		var pn []string
		for _, m := range poe {
			pn = append(pn, m["name"])
		}
		poeMon, _ = printAll(ctx, cl, "/interface/ethernet/poe/monitor", "=numbers="+strings.Join(pn, ","), "=once=")
	}
	br, _ = printAll(ctx, cl, "/interface/bridge/port/print", "=.proplist=interface,bridge,pvid")
	return buildPorts(eth, mon, poe, poeMon, br), nil
}

// buildPorts setzt die Antworten zusammen (getrennt, damit testbar).
func buildPorts(eth, mon, poe, poeMon, br []map[string]string) []PortInfo {
	byName := func(l []map[string]string) map[string]map[string]string {
		out := map[string]map[string]string{}
		for _, m := range l {
			out[m["name"]] = m
		}
		return out
	}
	monBy, poeBy, poeMonBy := byName(mon), byName(poe), byName(poeMon)
	bridge := map[string]string{}
	pvid := map[string]int{}
	for _, m := range br {
		bridge[m["interface"]] = m["bridge"]
		pvid[m["interface"]], _ = strconv.Atoi(m["pvid"])
	}
	var out []PortInfo
	for _, m := range eth {
		x := PortInfo{Name: m["name"], Comment: m["comment"], Disabled: m["disabled"] == "true",
			Link: m["running"] == "true", Bridge: bridge[m["name"]], PVID: pvid[m["name"]], MAC: m["mac-address"]}
		x.SFP = strings.HasPrefix(m["default-name"], "sfp") || strings.HasPrefix(m["name"], "sfp")
		if mm, ok := monBy[x.Name]; ok {
			if s := mm["status"]; s != "" {
				x.Link = s == "link-ok"
			}
			if x.Link {
				x.Rate = mm["rate"]
				if mm["full-duplex"] == "true" {
					x.Duplex = "full"
				} else if mm["full-duplex"] == "false" {
					x.Duplex = "half"
				}
			}
		}
		if x.Disabled {
			x.Link = false
		}
		if pm, ok := poeBy[x.Name]; ok {
			x.PoE = pm["poe-out"]
			if pmon, ok := poeMonBy[x.Name]; ok {
				x.PoEState = pmon["poe-out-status"]
				x.PoEWatt = pmon["poe-out-power"]
			}
			x.PoEUsed = x.PoEState == "powered-on"
		}
		out = append(out, x)
	}
	sort.SliceStable(out, func(a, b int) bool { return natLess(out[a].Name, out[b].Name) })
	return out
}

// natLess: ether2 vor ether10, Kupfer vor SFP bei gleichem Präfix egal.
func natLess(a, b string) bool {
	pa, na := splitNum(a)
	pb, nb := splitNum(b)
	if pa != pb {
		return pa < pb
	}
	return na < nb
}

func splitNum(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(s[i:])
	return s[:i], n
}
