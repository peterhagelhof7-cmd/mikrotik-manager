package main

import (
	"strings"
	"testing"
)

// Echttest 20.11.2026 (hAP RB951Ui-2nD): mit frequency-mode=manual-txpower lehnt RouterOS
// country=germany ab („only regulatory-domain mode allowed for this country“).
func TestPlanCountryRegDomain(t *testing.T) {
	s := &wlanState{pkg: "wireless", ifs: []*wlanIfc{
		{WlanIf: WlanIf{Name: "wlan1"}, country: "no_country_set", freqMode: "manual-txpower"},
		{WlanIf: WlanIf{Name: "wlan2"}, country: "austria", freqMode: "regulatory-domain"},
	}}
	cc := planCountry(s, "germany")
	if len(cc) != 2 {
		t.Fatalf("%+v", cc)
	}
	if a := cc[0].apply; !strings.Contains(a, `country="germany" frequency-mode=regulatory-domain`) {
		t.Errorf("apply: %s", a)
	}
	if u := cc[0].undo; !strings.Contains(u, `country="no_country_set" frequency-mode="manual-txpower"`) {
		t.Errorf("undo: %s", u)
	}
	if !strings.Contains(cc[0].step, "Frequenzmodus manual-txpower → regulatory-domain") {
		t.Errorf("step: %s", cc[0].step)
	}
	if strings.Contains(cc[1].apply, "frequency-mode") {
		t.Errorf("schon regulatory-domain: %s", cc[1].apply)
	}
	for _, c := range []struct {
		country, mode string
		want          bool
	}{{"germany", "manual-txpower", true}, {"germany", "superchannel", true}, {"germany", "regulatory-domain", false},
		{"no_country_set", "manual-txpower", false}, {"germany", "", false}} {
		if needRegDomain(c.country, c.mode) != c.want {
			t.Errorf("needRegDomain(%q, %q)", c.country, c.mode)
		}
	}
}
