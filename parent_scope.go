package main

// topoScope: Der lokale Manager kennt einen Baum für alle Geräte.
func (p *Poller) topoScope(Device) ([]string, string) {
	var ids []string
	for _, d := range p.st.List() {
		ids = append(ids, d.ID)
	}
	return ids, p.TopoRoot("")
}
