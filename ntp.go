package main

import (
	"context"
	"fmt"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// NTP für die Neustart-Routine (Haken „NTP an“): Viele Geräte haben keine batteriegepufferte
// Uhr und starten nach einem Neustart mit falscher Zeit – ohne NTP scheitern dann Update-Suche
// und Zertifikatsprüfung. Vor dem Neustart schaltet der Manager den NTP-Client ein, nach dem
// Neustart wartet er, bis die Zeit synchron ist.

// ntpDefault: Server, der eingetragen wird, wenn am Gerät noch keiner steht.
const ntpDefault = "de.pool.ntp.org"

// ntpSyncWait: so lange wartet die Routine nach dem Neustart auf die Synchronisation.
var ntpSyncWait = 2 * time.Minute

// ensureNTP schaltet den NTP-Client ein, wenn er aus ist. Eingetragene Server bleiben; nur ohne
// Server kommt ntpDefault hinein. Liefert, was getan wurde (leer = war schon an).
func ensureNTP(ctx context.Context, cl *routeros.Client) (string, error) {
	r, err := cl.RunContext(ctx, "/system/ntp/client/print")
	if err != nil {
		return "", fmt.Errorf("NTP-Client lesen: %w", err)
	}
	if len(r.Re) == 0 {
		return "", fmt.Errorf("NTP-Client nicht gefunden")
	}
	m := r.Re[0].Map
	if m["enabled"] == "true" {
		return "", nil
	}
	args := []string{"/system/ntp/client/set", "=enabled=yes"}
	done := "NTP-Client eingeschaltet"
	if m["servers"] == "" && m["server-dns-names"] == "" {
		args = append(args, "=servers="+ntpDefault)
		done += " (Server " + ntpDefault + ")"
	}
	if _, err := cl.RunContext(ctx, args...); err != nil {
		return "", fmt.Errorf("NTP-Client einschalten: %w", err)
	}
	return done, nil
}

// ntpSynced: RouterOS 7 meldet „synchronized“, sobald die Uhr gestellt ist.
func ntpSynced(status string) bool { return status == "synchronized" }

// waitNTPSync fragt den Zustand ab, bis er synchron ist oder die Zeit um ist.
func waitNTPSync(ctx context.Context, cl *routeros.Client, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	status := ""
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		r, err := cl.RunContext(rctx, "/system/ntp/client/print")
		cancel()
		if err == nil && len(r.Re) > 0 {
			status = r.Re[0].Map["status"]
			if ntpSynced(status) {
				return status, true
			}
		}
		if !time.Now().Before(deadline) {
			return status, false
		}
		if err := rollWait(ctx, 5*time.Second); err != nil {
			return status, false
		}
	}
}

// rebootNTPBefore: vor dem Neustart NTP einschalten (Protokoll + Audit). Fehler brechen den
// Neustart nicht ab – die Routine soll trotzdem laufen.
func (p *Poller) rebootNTPBefore(ctx context.Context, id string, x *Job, name string) {
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cl, err := p.session(rctx, id)
	if err != nil {
		p.jobs.step(x, name, "NTP: Gerät nicht erreichbar – %v", err)
		return
	}
	defer cl.Close()
	done, err := ensureNTP(rctx, cl)
	switch {
	case err != nil:
		p.jobs.step(x, name, "NTP: %v", err)
	case done == "":
		p.jobs.step(x, name, "NTP-Client ist an")
	default:
		p.jobs.step(x, name, "%s", done)
		p.noteNTP(id, name, done)
	}
}

// rebootNTPAfter: nach dem Neustart auf die Synchronisation warten und das Ergebnis protokollieren.
func (p *Poller) rebootNTPAfter(ctx context.Context, id string, x *Job, name string) {
	cl, err := p.session(ctx, id)
	if err != nil {
		p.jobs.step(x, name, "NTP: Gerät nicht erreichbar – %v", err)
		return
	}
	defer cl.Close()
	p.jobs.step(x, name, "warte auf NTP-Synchronisation …")
	if status, ok := waitNTPSync(ctx, cl, ntpSyncWait); ok {
		p.jobs.step(x, name, "Zeit per NTP synchron")
	} else {
		if status == "" {
			status = "unbekannt"
		}
		p.jobs.step(x, name, "⚠ NTP nach %s noch nicht synchron (Zustand: %s) – Zeitserver bzw. UDP 123 am Standort prüfen", ntpSyncWait, status)
	}
}
