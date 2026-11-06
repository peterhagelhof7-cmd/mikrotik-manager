package main

import "testing"

// In die Cloud umgezogene Geräte startet der lokale Manager nicht neu.
func TestRebootPlanSkipsMoved(t *testing.T) {
	p, ids := rebootPoller(t, "ap", "weg")
	p.st.Update(ids[1], func(d *Device) { d.CloudURL = "https://cloud.example" })
	if d := p.areaDevices(rebootArea{}); len(d) != 1 || d[0].ID != ids[0] {
		t.Fatalf("%+v", d)
	}
}
