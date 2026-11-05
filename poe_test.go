package main

import (
	"strings"
	"testing"
)

func TestPoEScript(t *testing.T) {
	s := poeScript("ether8", "auto-on")
	a, b, c := strings.Index(s, `poe-out=off`), strings.Index(s, ":delay 10s"), strings.Index(s, `poe-out=auto-on`)
	if a < 0 || b < a || c < b {
		t.Fatal(s)
	}
}

func TestPoETargets(t *testing.T) {
	poe := []map[string]string{{"name": "ether1", "poe-out": "auto-on"}, {"name": "ether2", "poe-out": "forced-on"}, {"name": "ether3", "poe-out": "off"}, {"name": "ether4", "poe-out": "auto-on"}, {"name": "ether5", "poe-out": "auto-on"}}
	mon := []map[string]string{{"name": "ether1", "poe-out-status": "powered-on"}, {"name": "ether2", "poe-out-status": "powered-on"}, {"name": "ether4", "poe-out-status": "waiting-for-load"}, {"name": "ether5", "poe-out-status": "powered-on"}}
	got, err := poeTargets(poe, mon, []string{"ether1"}, "")
	if err != nil || len(got) != 2 || got[0] != (poeTarget{"ether2", "forced-on"}) || got[1].Port != "ether5" {
		t.Fatalf("%+v %v", got, err)
	}
	// Einzeln geht auch ein Port, der gerade nichts versorgt.
	if got, err := poeTargets(poe, mon, nil, "ether4"); err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	for port, bad := range map[string]string{"ether1": "Uplink", "ether3": "kein PoE", "ether9": "kein PoE"} {
		if _, err := poeTargets(poe, mon, []string{"ether1"}, port); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%s: %v", port, err)
		}
	}
	if _, err := poeTargets(poe[2:3], nil, nil, ""); err == nil {
		t.Fatal("nichts zu tun")
	}
}
