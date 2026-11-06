package main

import "testing"

func TestDeviceAddresses(t *testing.T) {
	got := deviceAddresses(
		[]map[string]string{
			{"address": "10.250.0.3/16", "interface": "wg-mm"},
			{"address": "192.168.55.196/24", "interface": "bridge", "dynamic": "true"},
			{"address": "10.9.9.9/24", "interface": "ether2", "disabled": "true", "invalid": "true"},
		},
		[]map[string]string{{"address": "fe80::1/64", "interface": "bridge", "dynamic": "true"}},
	)
	want := []DeviceAddress{
		{Address: "192.168.55.196/24", Interface: "bridge", Dynamic: true},
		{Address: "10.9.9.9/24", Interface: "ether2", Disabled: true, Invalid: true},
		{Address: "10.250.0.3/16", Interface: "wg-mm"},
		{Address: "fe80::1/64", Interface: "bridge", Dynamic: true},
	}
	if len(got) != len(want) {
		t.Fatalf("%d Adressen, erwartet %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v, erwartet %+v", i, got[i], want[i])
		}
	}
}
