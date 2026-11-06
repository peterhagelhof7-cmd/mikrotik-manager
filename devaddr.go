package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DeviceAddress ist eine IP-Adresse des Geräts samt Schnittstelle.
type DeviceAddress struct {
	Address   string `json:"address"`
	Interface string `json:"interface"`
	Dynamic   bool   `json:"dynamic,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	Invalid   bool   `json:"invalid,omitempty"`
}

func deviceAddresses(v4, v6 []map[string]string) []DeviceAddress {
	out := make([]DeviceAddress, 0, len(v4)+len(v6))
	for _, l := range [][]map[string]string{v4, v6} {
		for _, m := range l {
			out = append(out, DeviceAddress{Address: m["address"], Interface: m["interface"],
				Dynamic: m["dynamic"] == "true", Disabled: m["disabled"] == "true", Invalid: m["invalid"] == "true"})
		}
	}
	isV6 := func(a DeviceAddress) bool { return strings.Contains(a.Address, ":") }
	sort.SliceStable(out, func(a, b int) bool {
		if x, y := isV6(out[a]), isV6(out[b]); x != y {
			return y
		}
		return out[a].Interface < out[b].Interface
	})
	return out
}

// DeviceAddresses liest IPv4 und IPv6. Ohne IPv6 auf dem Gerät bleibt es bei IPv4.
func (p *Poller) DeviceAddresses(ctx context.Context, id string) ([]DeviceAddress, error) {
	cl, err := p.session(ctx, id)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	const props = "=.proplist=address,interface,dynamic,disabled,invalid"
	v4, err := printAll(ctx, cl, "/ip/address/print", props)
	if err != nil {
		return nil, err
	}
	v6, _ := printAll(ctx, cl, "/ipv6/address/print", props)
	return deviceAddresses(v4, v6), nil
}

func (w *web) deviceAddresses(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	l, err := w.pl.DeviceAddresses(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(rw, "Adressen lesen: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, l)
}
