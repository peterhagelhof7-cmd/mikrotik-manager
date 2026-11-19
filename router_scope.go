package main

// Der lokale Manager hat einen Bereich: das ganze Netz.
func routerKeyOf(Device) string { return "" }

// routerAuto: automatisch angelegte Netze gibt es nur in der Cloud (Bewohner).
func (p *Poller) routerAuto(string) []RouterNet { return nil }

// routerAutoVLANs: VLAN-Bereiche des Managers gibt es nur in der Cloud (Bewohner im Einzel-VLAN).
func (p *Poller) routerAutoVLANs(string) []string { return nil }

func (p *Poller) routerCentral(string) CentralWLAN { return p.Central() }
