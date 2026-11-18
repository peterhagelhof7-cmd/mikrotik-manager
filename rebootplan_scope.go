package main

import (
	"context"
	"path/filepath"
)

// Der lokale Manager hat einen Bereich: das ganze Netz. In die Cloud umgezogene Geräte
// verwaltet die Cloud, sie gehören nicht in die Routine.
func (p *Poller) rebootPath(rebootArea) string {
	return filepath.Join(p.cfg.DataDir, "reboot-plan.json")
}

func (p *Poller) rebootAreas() []rebootArea { return []rebootArea{{}} }

func (p *Poller) areaDevices(rebootArea) []Device {
	var out []Device
	for _, d := range p.st.List() {
		if !d.Moved() {
			out = append(out, d)
		}
	}
	return out
}

// rebootHygiene: Aufräumen vor dem geplanten Lauf – nur in der Cloud (Bewohner).
func (p *Poller) rebootHygiene(context.Context, rebootArea, string) string { return "" }
