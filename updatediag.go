package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Diagnose, wenn ein Gerät bei der Update-Suche keine neueste Version findet
// (z. B. „ERROR: broken NEWESTa7.stable file“): Uhrzeit, NTP, DNS und ein
// Probeabruf der Versionsdatei vom Gerät aus. Nur lesend – /tool fetch mit
// output=user schreibt keine Datei, /ping löst nur den Namen auf.
// Anlass: Update-Suche scheiterte an zwei neuen cAP ax (20.11.2026).

const updHost = "upgrade.mikrotik.com"

type updDiag struct {
	Channel               string
	Clock                 time.Time // Gerätezeit in UTC; Zero = nicht lesbar
	ClockRaw              string    // wie das Gerät sie meldet
	OffsetKnown           bool      // gmt-offset gelesen (sonst nur grober Vergleich)
	NTPRead, NTPOn        bool
	NTPStatus, NTPServers string
	DNS                   string
	Resolved, ResolveErr  string
	Fetch                 []updFetch // HTTPS und HTTP: ein Proxy/Web-Filter fängt oft nur HTTP ab
}

// updFetch: Probeabruf der Versionsdatei über eine URL.
type updFetch struct {
	URL, Data, Err, Status string
}

// first: erste Zeile der Antwort; ok: sieht wie eine RouterOS-Versionszeile aus.
func (f updFetch) first() string {
	s, _, _ := strings.Cut(strings.TrimSpace(f.Data), "\n")
	return s
}
func (f updFetch) ok() bool { return f.Err == "" && rosVersionLine.MatchString(f.first()) }

var rosVersionLine = regexp.MustCompile(`^\d+\.\d+(\.\d+)?(beta\d+|rc\d+)?\b`)

// readUpdDiag liest alles für die Diagnose; einzelne Fehler landen im Ergebnis.
func readUpdDiag(ctx context.Context, cl *routeros.Client) updDiag {
	var g updDiag
	run := func(timeout time.Duration, cmd ...string) (*routeros.Reply, error) {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return cl.RunContext(rctx, cmd...)
	}
	g.Channel = "stable"
	if r, err := run(15*time.Second, "/system/package/update/print"); err == nil && len(r.Re) > 0 && r.Re[0].Map["channel"] != "" {
		g.Channel = r.Re[0].Map["channel"]
	}
	if r, err := run(15*time.Second, "/system/clock/print"); err == nil && len(r.Re) > 0 {
		m := r.Re[0].Map
		g.ClockRaw = strings.TrimSpace(m["date"] + " " + m["time"])
		g.Clock, g.OffsetKnown = parseROSClock(m["date"], m["time"], m["gmt-offset"])
	}
	if r, err := run(15*time.Second, "/system/ntp/client/print"); err == nil && len(r.Re) > 0 {
		m := r.Re[0].Map
		g.NTPRead, g.NTPOn = true, m["enabled"] == "true"
		g.NTPStatus = m["status"]
		g.NTPServers = strings.TrimSpace(strings.Join([]string{m["servers"], m["server-dns-names"]}, " "))
	}
	if r, err := run(15*time.Second, "/ip/dns/print"); err == nil && len(r.Re) > 0 {
		m := r.Re[0].Map
		var l []string
		for _, s := range []string{m["servers"], m["dynamic-servers"]} {
			if s = strings.TrimSpace(s); s != "" {
				l = append(l, s)
			}
		}
		g.DNS = strings.Join(l, ",")
	}
	if r, err := run(15*time.Second, "/ping", "=address="+updHost, "=count=1"); err != nil {
		g.ResolveErr = err.Error()
	} else {
		for _, re := range r.Re {
			if h := re.Map["host"]; h != "" {
				g.Resolved = h
			}
		}
		if g.Resolved == "" {
			g.ResolveErr = "keine Adresse"
		}
	}
	for _, scheme := range []string{"https", "http"} {
		f := updFetch{URL: scheme + "://" + updHost + "/routeros/NEWESTa7." + g.Channel}
		if r, err := run(30*time.Second, "/tool/fetch", "=url="+f.URL, "=output=user"); err != nil {
			f.Err = err.Error()
		} else {
			for _, re := range r.Re {
				if s := re.Map["status"]; s != "" {
					f.Status = s
				}
				if d := re.Map["data"]; d != "" {
					f.Data = d
				}
			}
		}
		g.Fetch = append(g.Fetch, f)
	}
	return g
}

// parseROSClock: RouterOS ab 7.10 „2026-11-20“, davor „nov/20/2026“; gmt-offset „+01:00“.
func parseROSClock(date, clock, offset string) (time.Time, bool) {
	var d time.Time
	var err error
	if strings.Contains(date, "/") {
		d, err = time.Parse("Jan/02/2006 15:04:05", strings.ToUpper(date[:1])+date[1:]+" "+clock)
	} else {
		d, err = time.Parse("2006-01-02 15:04:05", date+" "+clock)
	}
	if err != nil {
		return time.Time{}, false
	}
	if len(offset) == 6 && (offset[0] == '+' || offset[0] == '-') {
		if o, err := time.Parse("15:04", offset[1:]); err == nil {
			sec := o.Hour()*3600 + o.Minute()*60
			if offset[0] == '-' {
				sec = -sec
			}
			return d.Add(-time.Duration(sec) * time.Second), true
		}
	}
	return d, false
}

// report: Zeilen fürs Job-Protokoll und der wahrscheinlichste Grund.
func (g updDiag) report(now time.Time) (lines []string, hint string) {
	// Uhrzeit
	clockBad := false
	switch {
	case g.Clock.IsZero():
		lines = append(lines, "Diagnose Uhrzeit: nicht lesbar")
	default:
		diff := now.UTC().Sub(g.Clock)
		if diff < 0 {
			diff = -diff
		}
		tol := time.Hour
		if !g.OffsetKnown {
			tol = 26 * time.Hour // ohne Zeitzone nur grob
		}
		clockBad = diff > tol
		state := "stimmt"
		if clockBad {
			state = fmt.Sprintf("FALSCH (Abweichung %s)", diffLabel(diff))
		}
		lines = append(lines, fmt.Sprintf("Diagnose Uhrzeit: %s – %s", g.ClockRaw, state))
	}
	ntp := "nicht lesbar"
	if g.NTPRead {
		ntp = "aus"
		if g.NTPOn {
			ntp = "an"
			if g.NTPStatus != "" {
				ntp += ", " + g.NTPStatus
			}
		}
		if g.NTPServers != "" {
			ntp += " (" + g.NTPServers + ")"
		}
	}
	lines = append(lines, "Diagnose NTP-Client: "+ntp)
	// DNS
	dns := g.DNS
	if dns == "" {
		dns = "keine DNS-Server"
	}
	res := g.Resolved
	if res == "" {
		res = "nicht aufgelöst (" + g.ResolveErr + ")"
	}
	lines = append(lines, fmt.Sprintf("Diagnose DNS: %s – %s → %s", dns, updHost, res))
	// Probeabrufe (HTTPS, HTTP)
	var httpsF, httpF updFetch
	for _, f := range g.Fetch {
		switch {
		case f.Err != "":
			lines = append(lines, fmt.Sprintf("Diagnose Abruf %s: Fehler – %s", f.URL, f.Err))
		case f.ok():
			lines = append(lines, fmt.Sprintf("Diagnose Abruf %s: ok – %s", f.URL, f.first()))
		default:
			lines = append(lines, fmt.Sprintf("Diagnose Abruf %s: keine Versionsdatei (%s) – %q", f.URL, f.Status, clip(f.first(), 80)))
		}
		if strings.HasPrefix(f.URL, "https:") {
			httpsF = f
		} else {
			httpF = f
		}
	}
	filter := " – Web-Filter, Proxy oder Firewall am Standort prüfen (" + updHost + " und download.mikrotik.com freigeben)"

	switch {
	case clockBad:
		hint = "Uhrzeit des Geräts falsch – NTP-Client einschalten (Tab „Neustarts“: Haken „NTP an“ und Gerät in der Routine, oder am Gerät /system ntp client set enabled=yes), dann Update erneut starten"
	case g.Resolved == "":
		hint = "DNS am Gerät löst " + updHost + " nicht auf – DNS-Server am Gerät bzw. am Standort prüfen"
	case httpsF.ok() && !httpF.ok():
		hint = "Abruf über HTTPS klappt, über HTTP (Port 80) nicht bzw. mit fremdem Inhalt – am Standort fängt ein Proxy oder Web-Filter HTTP ab" + filter
	case !httpsF.ok() && !httpF.ok():
		hint = "Abruf der Versionsdatei scheitert über HTTPS und HTTP" + filter
	case !httpsF.ok():
		hint = "Abruf über HTTP klappt, über HTTPS nicht" + filter
	default:
		hint = "Das Gerät bekommt die Versionsdatei über HTTPS und HTTP (" + httpsF.first() + ") – am Netz liegt es nicht; die Update-Suche von RouterOS selbst scheitert (Gerät neu starten, sonst RouterOS von Hand aktualisieren)"
	}
	return lines, hint
}

func diffLabel(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d Tage", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// updateDiagnose schreibt die Diagnose ins Job-Protokoll und liefert den Hinweis.
func (p *Poller) updateDiagnose(ctx context.Context, cl *routeros.Client, x *Job, name string) string {
	p.jobs.step(x, name, "Update-Server liefert keine Version – Diagnose (nur lesend) …")
	lines, hint := readUpdDiag(ctx, cl).report(time.Now())
	for _, l := range lines {
		p.jobs.step(x, name, "%s", l)
	}
	return hint
}
