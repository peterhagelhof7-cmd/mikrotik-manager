package main

import (
	"context"
	"sort"
	"strings"

	"github.com/go-routeros/routeros/v3"
)

// Router und Access Point im selben Gerät (z. B. Chateau LTE): Router-Tab und zentrale
// WLAN-Verwaltung schreiben dann in dieselbe Bridge-VLAN-Tabelle. Seit RouterOS 7.17 lehnt die
// Bridge zwei statische Einträge mit derselben VLAN-ID ab („disallow duplicate static VLAN
// entries“). Deshalb gilt je VLAN genau ein Eintrag:
//
//   - Hat der Router-Tab das VLAN (Netz, Port-VLAN oder Bewohner-Bereich), gehört der Eintrag dem
//     Router (Kommentar mm-router). Bridge und Ethernet-Ports bestimmt der Router, die
//     WLAN-Schnittstellen (alles andere) die WLAN-Verwaltung – sie setzt nur diese im
//     Router-Eintrag und legt für das VLAN keinen eigenen an.
//   - Beim Ausrollen räumt der Router nur fremde Einträge weg, nicht die der WLAN-Verwaltung,
//     und übernimmt deren WLAN-Schnittstellen, wenn er einen Eintrag mit derselben VLAN-ID anlegt.
//
// So nimmt keine Seite der anderen etwas weg, und es entsteht nie ein doppelter Eintrag.

// bridgeEntry ist ein statischer Eintrag der Bridge-VLAN-Tabelle.
type bridgeEntry struct {
	ID       string // .id
	IDs      string // vlan-ids, z. B. "1999" oder "2000-3999"
	Comment  string
	Tagged   []string
	Untagged []string
}

// bridgeLive: statische VLAN-Einträge einer Bridge und die Ethernet-Ports des Geräts.
type bridgeLive struct {
	eth     map[string]bool
	entries []bridgeEntry
}

func splitPorts(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func readBridgeLive(ctx context.Context, cl *routeros.Client, bridge string) (bridgeLive, error) {
	b := bridgeLive{eth: map[string]bool{}}
	el, err := printAll(ctx, cl, "/interface/ethernet/print", "=.proplist=name")
	if err != nil {
		return b, err
	}
	for _, m := range el {
		b.eth[m["name"]] = true
	}
	vl, err := printAll(ctx, cl, "/interface/bridge/vlan/print", "=.proplist=.id,bridge,vlan-ids,tagged,untagged,comment,dynamic")
	if err != nil {
		return b, err
	}
	for _, m := range vl {
		if m["bridge"] != bridge || m["dynamic"] == "true" {
			continue
		}
		b.entries = append(b.entries, bridgeEntry{ID: m[".id"], IDs: m["vlan-ids"], Comment: m["comment"],
			Tagged: splitPorts(m["tagged"]), Untagged: splitPorts(m["untagged"])})
	}
	return b, nil
}

// wlanPorts: die Ports, die der WLAN-Verwaltung gehören – weder Bridge noch Ethernet.
func wlanPorts(ports []string, eth map[string]bool, bridge string) []string {
	var out []string
	for _, p := range ports {
		if p != bridge && !eth[p] {
			out = append(out, p)
		}
	}
	return out
}

// routerOwnPorts: die Ports eines Router-Eintrags, die dem Router gehören (Bridge und Ethernet).
func routerOwnPorts(ports []string, eth map[string]bool, bridge string) []string {
	var out []string
	for _, p := range ports {
		if p == bridge || eth[p] {
			out = append(out, p)
		}
	}
	return out
}

// unionPorts: sortiert, ohne Doppelte.
func unionPorts(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, p := range l {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

func portsLabel(l []string) string {
	if len(l) == 0 {
		return "keine"
	}
	return strings.Join(unionPorts(l), ",")
}

func samePorts(a, b []string) bool {
	return strings.Join(unionPorts(a), ",") == strings.Join(unionPorts(b), ",")
}
