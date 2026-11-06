package main

import "testing"

func TestClassifyFile(t *testing.T) {
	marker := map[string]bool{"mm-undo-111": true}
	for _, c := range []struct {
		name, typ string
		job       bool
		protected bool
		leftover  bool
	}{
		{"flash", "disk", false, true, false},
		{"flash/skins", "directory", false, true, false},
		{"flash/mm-undo-111.backup", "backup", false, true, false},
		{"mm-undo-111.backup", "backup", false, true, false},
		{"flash/mm-undo-222.backup", "backup", false, false, true},
		{"flash/mm-reset.rsc", "script", false, false, true},
		{"flash/mm-reset.rsc", "script", true, true, false},
		{"flash/kunde.txt", ".txt file", true, false, false},
		{"routeros-7.25.npk", "package", false, false, false},
		{"xmm-undo-111.backup", "backup", false, false, false},
	} {
		f := DeviceFile{Name: c.name, Type: c.typ}
		classifyFile(&f, marker, c.job)
		if f.Protected != c.protected || f.Leftover != c.leftover {
			t.Errorf("%s (Aktion %v): geschützt %v, Rest %v", c.name, c.job, f.Protected, f.Leftover)
		}
		if f.Protected && f.Why == "" {
			t.Errorf("%s: Grund fehlt", c.name)
		}
	}
}
