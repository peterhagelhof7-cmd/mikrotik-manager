package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseROSClock(t *testing.T) {
	want := time.Date(2026, 11, 20, 14, 55, 37, 0, time.UTC)
	if got, ok := parseROSClock("2026-11-20", "15:55:37", "+01:00"); !ok || !got.Equal(want) {
		t.Errorf("neu: %v %v", got, ok)
	}
	if got, ok := parseROSClock("nov/20/2026", "15:55:37", "+01:00"); !ok || !got.Equal(want) {
		t.Errorf("alt: %v %v", got, ok)
	}
	if got, ok := parseROSClock("2026-11-20", "09:55:37", "-05:00"); !ok || !got.Equal(want) {
		t.Errorf("negativ: %v %v", got, ok)
	}
	if got, ok := parseROSClock("2026-11-20", "15:55:37", ""); ok || got.IsZero() {
		t.Errorf("ohne Offset: %v %v", got, ok)
	}
	if got, _ := parseROSClock("kaputt", "x", ""); !got.IsZero() {
		t.Errorf("kaputt: %v", got)
	}
}

func TestUpdDiagReport(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 55, 0, 0, time.UTC)
	ok := updDiag{Clock: now, ClockRaw: "2026-11-20 15:55:00", OffsetKnown: true, NTPRead: true, NTPOn: true, NTPStatus: "synchronized",
		DNS: "192.168.1.1", Resolved: "159.148.147.251"}
	good := updFetch{Status: "finished", Data: "7.25.1 1763638000\n"}
	blocked := updFetch{Status: "finished", Data: "<html><body>Zugriff gesperrt</body></html>"}
	withFetch := func(g updDiag, https, http updFetch) updDiag {
		https.URL, http.URL = "https://upgrade.mikrotik.com/routeros/NEWESTa7.stable", "http://upgrade.mikrotik.com/routeros/NEWESTa7.stable"
		g.Fetch = []updFetch{https, http}
		return g
	}
	ok = withFetch(ok, good, good)

	lines, hint := ok.report(now)
	if len(lines) != 5 || !strings.Contains(hint, "am Netz liegt es nicht") || strings.Contains(hint, "vorübergehend") {
		t.Fatalf("alles ok: %v / %s", lines, hint)
	}

	g := ok
	g.Clock, g.ClockRaw = time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC), "1970-01-02 01:00:00"
	g.NTPOn = false
	lines, hint = g.report(now)
	if !strings.Contains(hint, "Uhrzeit des Geräts falsch") || !strings.Contains(strings.Join(lines, "\n"), "FALSCH") || !strings.Contains(strings.Join(lines, "\n"), "NTP-Client: aus") {
		t.Errorf("Uhr: %v / %s", lines, hint)
	}

	g = ok
	g.Resolved, g.ResolveErr = "", "could not resolve dns name"
	if _, hint = g.report(now); !strings.Contains(hint, "DNS am Gerät") {
		t.Errorf("DNS: %s", hint)
	}

	// Echttest 20.11.2026: HTTPS ok, HTTP abgefangen
	g = withFetch(ok, good, blocked)
	if lines, hint = g.report(now); !strings.Contains(hint, "HTTP (Port 80)") || !strings.Contains(lines[3], ": ok") || !strings.Contains(lines[4], "keine Versionsdatei") {
		t.Errorf("nur HTTP gesperrt: %v / %s", lines, hint)
	}

	g = withFetch(ok, updFetch{Err: "failure: SSL: handshake timed out"}, blocked)
	if lines, hint = g.report(now); !strings.Contains(hint, "HTTPS und HTTP") || !strings.Contains(lines[3], "Fehler") {
		t.Errorf("beides gestört: %v / %s", lines, hint)
	}

	g = withFetch(ok, blocked, good)
	if _, hint = g.report(now); !strings.Contains(hint, "über HTTPS nicht") {
		t.Errorf("nur HTTPS gestört: %s", hint)
	}

	// ohne Zeitzone: 3 h Abweichung gelten noch als „stimmt“ (nur grober Vergleich)
	g = ok
	g.Clock, g.OffsetKnown = now.Add(3*time.Hour), false
	if _, hint = g.report(now); strings.Contains(hint, "Uhrzeit") {
		t.Errorf("grob: %s", hint)
	}
}
