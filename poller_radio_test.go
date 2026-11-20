package main

import "testing"

func TestHasRadioIf(t *testing.T) {
	if !hasRadioIf([]map[string]string{{"type": "ether"}, {"type": "wlan"}}) || !hasRadioIf([]map[string]string{{"type": "wifi"}}) {
		t.Error("WLAN-Schnittstelle nicht erkannt")
	}
	if hasRadioIf([]map[string]string{{"type": "ether"}, {"type": "bridge"}, {"type": "vlan"}}) {
		t.Error("Switch ohne Funk als WLAN-Gerät erkannt")
	}
}
