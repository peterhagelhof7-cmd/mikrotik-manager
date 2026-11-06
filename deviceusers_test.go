package main

import "testing"

func TestParseDeviceUsers(t *testing.T) {
	got := parseDeviceUsers([]map[string]string{
		{"name": "admin", "group": "full", "last-logged-in": "2026-11-06 10:00:00"},
		{"name": "technik", "group": "write", "disabled": "true"},
		{"name": "mikrotik-manager", "group": "full"},
		{"name": "mikrotik-manager-cloud", "group": "full"},
		{"name": "service", "group": "full", "comment": "mikrotik-manager-cloud (nicht loeschen)"},
		{"name": "login", "group": "full"},
	}, "login")
	want := map[string]bool{"admin": false, "technik": false, "mikrotik-manager": true, "mikrotik-manager-cloud": true, "service": true, "login": true}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for _, u := range got {
		if u.Managed != want[u.Name] {
			t.Errorf("%s: %v", u.Name, u.Managed)
		}
	}
	if !got[1].Disabled || got[0].LastLogin == "" || got[0].Group != "full" {
		t.Fatalf("%+v", got)
	}
}
