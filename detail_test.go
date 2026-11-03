package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestBuildPorts(t *testing.T) {
	eth := []map[string]string{
		{"name": "ether10", "default-name": "ether10", "running": "false", "disabled": "false"},
		{"name": "ether2", "default-name": "ether2", "running": "true", "disabled": "false", "comment": "AP Flur"},
		{"name": "uplink", "default-name": "ether1", "running": "true", "disabled": "false"},
		{"name": "sfp1", "default-name": "sfp1", "running": "false", "disabled": "true"},
		{"name": "ether3", "default-name": "ether3", "running": "false", "disabled": "false"},
	}
	mon := []map[string]string{
		{"name": "ether2", "status": "link-ok", "rate": "1Gbps", "full-duplex": "true"},
		{"name": "uplink", "status": "link-ok", "rate": "100Mbps", "full-duplex": "false"},
		{"name": "ether3", "status": "no-link"},
	}
	poe := []map[string]string{{"name": "ether2", "poe-out": "auto-on"}, {"name": "ether3", "poe-out": "forced-on"}, {"name": "ether10", "poe-out": "off"}}
	poeMon := []map[string]string{
		{"name": "ether2", "poe-out-status": "powered-on", "poe-out-power": "4.2"},
		{"name": "ether3", "poe-out-status": "powered-on", "poe-out-power": "1.1"},
		{"name": "ether10", "poe-out-status": "disabled"},
	}
	br := []map[string]string{{"interface": "ether2", "bridge": "bridge1"}}
	l := buildPorts(eth, mon, poe, poeMon, br)
	var names []string
	for _, p := range l {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "ether2,ether3,ether10,sfp1,uplink" {
		t.Fatalf("Reihenfolge: %v", names)
	}
	by := map[string]PortInfo{}
	for _, p := range l {
		by[p.Name] = p
	}
	if p := by["ether2"]; !p.Link || p.Rate != "1Gbps" || p.Duplex != "full" || !p.PoEUsed || p.PoEWatt != "4.2" || p.Bridge != "bridge1" || p.Comment != "AP Flur" {
		t.Fatalf("ether2: %+v", p)
	}
	// PoE liefert Strom, aber kein Link (rot blinkend in der Oberfläche)
	if p := by["ether3"]; p.Link || !p.PoEUsed {
		t.Fatalf("ether3: %+v", p)
	}
	if p := by["ether10"]; p.PoE != "off" || p.PoEUsed {
		t.Fatalf("ether10: %+v", p)
	}
	if p := by["sfp1"]; !p.SFP || !p.Disabled || p.Link || p.PoE != "" {
		t.Fatalf("sfp1: %+v", p)
	}
	if p := by["uplink"]; p.SFP || p.Duplex != "half" || p.PoE != "" {
		t.Fatalf("uplink: %+v", p)
	}
}

func TestConfigDiff(t *testing.T) {
	a := rscLines("# 2026-11-01 04:00:00 by RouterOS 7.25\n# software id = AAAA-1111\n/system identity\nset name=AP1\n" +
		"/interface wifi\nset [ find default-name=wifi1 ] configuration.ssid=Heim \\\n    security.passphrase=geheim123\n/ip dns\nset servers=1.1.1.1\n")
	b := rscLines("# 2026-11-02 04:00:00 by RouterOS 7.25\n# software id = AAAA-1111\n/system identity\nset name=AP1\n" +
		"/interface wifi\nset [ find default-name=wifi1 ] configuration.ssid=Heim \\\n    security.passphrase=anders456\n/ip dns\nset servers=9.9.9.9\n")
	for _, l := range append(a, b...) {
		if strings.Contains(l, "geheim123") || strings.Contains(l, "anders456") {
			t.Fatalf("Passwort nicht maskiert: %q", l)
		}
		if strings.HasPrefix(l, "#") {
			t.Fatalf("Kopfkommentar nicht entfernt: %q", l)
		}
	}
	if !strings.Contains(strings.Join(a, "\n"), "configuration.ssid=Heim security.passphrase=•••") {
		t.Fatalf("Fortsetzungszeile nicht zusammengefügt: %q", a)
	}
	// Nur das Passwort geändert → maskiert gleich, nur DNS zählt als Änderung.
	c, changed := diffConfig(a, b)
	if !changed || c.Added != 1 || c.Removed != 1 {
		t.Fatalf("diff: %+v", c)
	}
	if _, changed := diffConfig(a, a); changed {
		t.Fatal("gleiche Exporte als geändert gemeldet")
	}
	if c.Lines[len(c.Lines)-1].Text != "set servers=9.9.9.9" {
		t.Fatalf("Zeilen: %+v", c.Lines)
	}
}

func TestConfigChangesFromBackups(t *testing.T) {
	dir := t.TempDir()
	p := newPoller(Config{DataDir: dir}, newStore(dir), nil)
	bd := p.backupDir("AAAA-1111")
	os.MkdirAll(bd, 0o700)
	write := func(name, body string) {
		os.WriteFile(filepath.Join(bd, name), []byte(body), 0o600)
	}
	write("20261101-040000-nacht.rsc", "/ip dns\nset servers=1.1.1.1\n")
	write("20261102-040000-nacht.rsc", "/ip dns\nset servers=1.1.1.1\n") // unverändert
	write("20261103-040000-nacht.rsc", "/ip dns\nset servers=9.9.9.9\n")
	write("20261104-040000-nacht.rsc", "/ip dns\nset servers=9.9.9.9\n/system identity\nset name=X\n")
	l := p.ConfigChanges("AAAA-1111", 0)
	if len(l) != 2 || l[0].To != "20261104-040000-nacht.rsc" || l[1].From != "20261102-040000-nacht.rsc" {
		t.Fatalf("Änderungen: %+v", l)
	}
	if len(p.ConfigChanges("AAAA-1111", 1)) != 1 {
		t.Fatal("max wirkt nicht")
	}
}

func TestDevHistoryAndRec(t *testing.T) {
	dir := t.TempDir()
	st := newStore(dir)
	p := newPoller(Config{DataDir: dir}, st, nil)
	id, _ := st.Observe(Device{SoftwareID: "AAAA-1111", IP: "10.0.0.9", Identity: "AP1"}, "test")
	w := &web{st: st, pl: p, user: "admin"}

	p.hist.Add(id, "Manager", "Update beendet", "fertig", nil)
	p.hist.Add(id, "Manager", "Neustart beendet", "", fmt.Errorf("nicht erreichbar"))
	l := p.hist.List(id, 0)
	if len(l) != 2 || l[0].Action != "Neustart beendet" || l[0].OK || l[1].Error != "" {
		t.Fatalf("Verlauf: %+v", l)
	}

	// rec: Gerät aus dem Pfad, Ergebnis aus {"ok":false}
	mux := http.NewServeMux()
	mux.Handle("POST /api/devices/{id}/identity", w.rec("Gerät umbenannt", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, Result{ID: r.PathValue("id"), OK: false, Msg: "Name muss 1–64 Zeichen lang sein"})
	})))
	mux.Handle("POST /api/update", w.rec("Update gestartet", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, map[string]any{"results": []Result{{ID: id, OK: true, Msg: "eingeplant"}}})
	})))
	mux.Handle("POST /api/devices/{id}/location", w.rec("Standort geändert", http.HandlerFunc(w.setLocation)))
	call := func(url string, body any) {
		b, _ := json.Marshal(body)
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", url, bytes.NewReader(b)))
	}
	call("/api/devices/"+id+"/identity", map[string]string{"name": ""})
	call("/api/update", map[string]any{"ids": []string{id}})
	call("/api/devices/"+id+"/location", map[string]string{"location": "  Technikraum EG "})
	l = p.hist.List(id, 3)
	if len(l) != 3 || l[0].Action != "Standort geändert" || !l[0].OK || l[0].User != "admin" ||
		l[1].Action != "Update gestartet" || !l[1].OK || l[2].Action != "Gerät umbenannt" || l[2].OK || !strings.Contains(l[2].Error, "1–64") {
		t.Fatalf("rec: %+v", l)
	}
	if d, _ := st.Get(id); d.Location != "Technikraum EG" {
		t.Fatalf("Standort: %q", d.Location)
	}

	// Begrenzung: ältere Einträge fallen weg
	for i := 0; i < histKeep+50; i++ {
		p.hist.Add(id, "x", "Eintrag "+fmt.Sprint(i), strings.Repeat("y", 700), nil)
	}
	if l := p.hist.List(id, 0); len(l) > histKeep+5 || l[0].Action != fmt.Sprintf("Eintrag %d", histKeep+49) {
		t.Fatalf("Begrenzung: %d Einträge, neuester %q", len(l), l[0].Action)
	}
	p.hist.Forget(id)
	if len(p.hist.List(id, 0)) != 0 {
		t.Fatal("Forget")
	}
	_ = time.Now
}

// Doppelte id-Attribute: $('#x') findet nur das erste Element (siehe Cloud-Fehler „sName“).
func TestUIUniqueIDs(t *testing.T) {
	b, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`\sid="([^"$]+)"`).FindAllStringSubmatch(string(b), -1) {
		if seen[m[1]] {
			t.Errorf("id %q kommt mehrfach vor", m[1])
		}
		seen[m[1]] = true
	}
}
