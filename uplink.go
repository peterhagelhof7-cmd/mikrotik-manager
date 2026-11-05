package main

import "strings"

// uplinkPorts sucht die Ethernet-Ports, über die das Standard-Gateway erreicht
// wird. ok = false: nicht eindeutig feststellbar, dann darf nichts geplant werden.
func uplinkPorts(r topoRaw) (ports []string, ok bool) {
	typ := map[string]string{}
	for _, p := range r.Ports {
		typ[p["name"]] = p["type"]
	}
	// use wertet eine Schnittstelle aus: Ethernet ist Uplink; WLAN und LTE sind
	// kein Ethernet-Uplink, die Ports sind dann alle Zugangsports.
	use := func(n string) bool {
		switch typ[n] {
		case "ether":
			ports = append(ports, n)
			return true
		case "wifi", "wlan", "wifiwave2", "lte":
			return true
		}
		return false
	}
	if len(r.Gateways) == 0 {
		return nil, false
	}
	for _, g := range r.Gateways {
		addr, via, _ := strings.Cut(g, "%")
		if use(addr) || (via != "" && use(via)) {
			continue
		}
		mac := ""
		for _, a := range r.ARP {
			if a["address"] == addr {
				mac = normMAC(a["mac-address"])
				if use(a["interface"]) {
					mac = "-" // direkt erledigt
				}
				break
			}
		}
		if mac == "-" {
			continue
		}
		if mac == "" {
			return nil, false
		}
		hit := false
		for _, h := range r.Hosts {
			if normMAC(h["mac-address"]) == mac && use(h["on-interface"]) {
				hit = true
				break
			}
		}
		if !hit {
			return nil, false
		}
	}
	return ports, true
}
