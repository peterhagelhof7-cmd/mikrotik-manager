package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Automatische Updates: zu einer festen Zeit an gewählten Wochentagen werden
// die ausgewählten Geräte nacheinander aktualisiert (RouterOS, optional die
// RouterBOARD-Firmware). Geräte ohne Update bleiben unberührt (kein Backup,
// kein Neustart). Läuft zur geplanten Zeit noch ein Job (z. B. das nächtliche
// Backup), wird gewartet.

// AutoUpdate ist die Einstellung (gespeichert in DATA_DIR/auto-update.json).
type AutoUpdate struct {
	Enabled  bool     `json:"enabled"`
	Time     string   `json:"time"`     // hh:mm Ortszeit
	Days     []int    `json:"days"`     // 0 = Sonntag … 6 = Samstag
	Firmware bool     `json:"firmware"` // RouterBOARD-Firmware mit aktualisieren
	Devices  []string `json:"devices"`  // leer = alle Geräte

	LastRun     time.Time    `json:"lastRun,omitempty"`
	LastReason  string       `json:"lastReason,omitempty"`
	LastResults []AutoResult `json:"lastResults,omitempty"`
}

// AutoResult ist das Ergebnis eines Geräts beim letzten Lauf.
type AutoResult struct {
	ID       string `json:"id"`
	Identity string `json:"identity"`
	OK       bool   `json:"ok"`
	Msg      string `json:"msg"`
}

// AutoUpdateView ergänzt die Einstellung für die Oberfläche.
type AutoUpdateView struct {
	AutoUpdate
	Next    time.Time `json:"next,omitempty"`
	Running bool      `json:"running"`
}

type autoUpdater struct {
	mu      sync.Mutex
	running bool
}

var autoMu sync.Mutex

func defaultAutoUpdate() AutoUpdate {
	return AutoUpdate{Time: "03:00", Days: []int{0}} // sonntags 03:00, aus
}

func (p *Poller) autoPath() string { return filepath.Join(p.cfg.DataDir, "auto-update.json") }

func (p *Poller) AutoUpdateCfg() AutoUpdate {
	autoMu.Lock()
	defer autoMu.Unlock()
	a := defaultAutoUpdate()
	if b, err := os.ReadFile(p.autoPath()); err == nil {
		json.Unmarshal(b, &a)
	}
	return a
}

func (p *Poller) writeAuto(a AutoUpdate) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	autoMu.Lock()
	defer autoMu.Unlock()
	return writeFileAtomic(p.autoPath(), b, 0o600)
}

// SaveAutoUpdate übernimmt die Einstellung aus der Oberfläche (Ergebnisse bleiben).
func (p *Poller) SaveAutoUpdate(in AutoUpdate) error {
	if _, ok := nextBackupAt(time.Now(), in.Time); !ok || in.Time == "off" {
		return fmt.Errorf("Uhrzeit %q ungültig (hh:mm)", in.Time)
	}
	days := map[int]bool{}
	for _, d := range in.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("Wochentag %d ungültig", d)
		}
		days[d] = true
	}
	if len(days) == 0 {
		return fmt.Errorf("mindestens einen Wochentag wählen")
	}
	a := p.AutoUpdateCfg()
	a.Enabled, a.Time, a.Firmware = in.Enabled, in.Time, in.Firmware
	a.Days = a.Days[:0]
	for d := range days {
		a.Days = append(a.Days, d)
	}
	sort.Ints(a.Days)
	a.Devices = nil
	for _, id := range in.Devices {
		if _, ok := p.st.Get(id); ok && !contains(a.Devices, id) {
			a.Devices = append(a.Devices, id)
		}
	}
	return p.writeAuto(a)
}

// nextAutoAt: nächster Termin hh:mm an einem der Wochentage nach now (DST-fest).
func nextAutoAt(now time.Time, hhmm string, days []int) (time.Time, bool) {
	first, ok := nextBackupAt(now, hhmm)
	if !ok || len(days) == 0 {
		return time.Time{}, false
	}
	for i := 0; i < 8; i++ {
		t := first.AddDate(0, 0, i)
		for _, d := range days {
			if int(t.Weekday()) == d {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func (p *Poller) AutoUpdateView() AutoUpdateView {
	a := p.AutoUpdateCfg()
	v := AutoUpdateView{AutoUpdate: a}
	if a.Enabled {
		v.Next, _ = nextAutoAt(time.Now(), a.Time, a.Days)
	}
	p.auto.mu.Lock()
	v.Running = p.auto.running
	p.auto.mu.Unlock()
	return v
}

// RunAutoUpdate startet einen Lauf im Hintergrund (geplant oder „Jetzt ausführen“).
func (p *Poller) RunAutoUpdate(reason string) error {
	p.auto.mu.Lock()
	if p.auto.running {
		p.auto.mu.Unlock()
		return fmt.Errorf("ein Update-Lauf ist bereits aktiv")
	}
	p.auto.running = true
	p.auto.mu.Unlock()
	go func() {
		defer func() {
			p.auto.mu.Lock()
			p.auto.running = false
			p.auto.mu.Unlock()
		}()
		p.autoRun(p.ctx, reason)
	}()
	return nil
}

func (p *Poller) autoRun(ctx context.Context, reason string) {
	a := p.AutoUpdateCfg()
	ids := a.Devices
	if len(ids) == 0 {
		for _, d := range p.st.List() {
			ids = append(ids, d.ID)
		}
	}
	// Erst warten, bis z. B. das nächtliche Backup durch ist (höchstens 2 h).
	for wait := 0; p.jobs.AnyRunning() && wait < 240; wait++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
	log.Printf("automatische Updates (%s): %d Geräte, Firmware %v", reason, len(ids), a.Firmware)
	fw := a.Firmware
	res := p.startJobs(ctx, ids, "update", func(ctx context.Context, id string, x *Job) error {
		return p.updateDevice(ctx, id, x, fw)
	})
	// Warten, bis alle gestarteten Jobs durch sind (laufen nacheinander)
	var results []AutoResult
	for _, r := range res {
		ar := AutoResult{ID: r.ID, Identity: r.Identity, OK: r.OK, Msg: r.Msg}
		if r.OK {
			for {
				j, ok := p.jobs.Get(r.ID)
				if !ok || !j.Running {
					if ok {
						ar.OK, ar.Msg = j.OK, j.Step
					}
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}
		results = append(results, ar)
	}
	a = p.AutoUpdateCfg()
	a.LastRun, a.LastReason, a.LastResults = time.Now(), reason, results
	if err := p.writeAuto(a); err != nil {
		log.Printf("automatische Updates: Ergebnis speichern: %v", err)
	}
	var bad []string
	for _, r := range results {
		if !r.OK {
			bad = append(bad, r.Identity)
		}
	}
	log.Printf("automatische Updates fertig: %d Geräte, Fehler: %s", len(results), strings.Join(bad, ", "))
}

// runAutoUpdates prüft jede halbe Minute: Lag seit dem letzten Blick ein Termin?
// So gilt eine geänderte Einstellung sofort, ohne Neuplanung.
func (p *Poller) runAutoUpdates(ctx context.Context) {
	last := time.Now()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		if a := p.AutoUpdateCfg(); a.Enabled {
			if due, ok := nextAutoAt(last, a.Time, a.Days); ok && !due.After(now) {
				if err := p.RunAutoUpdate("geplant"); err != nil {
					log.Printf("automatische Updates: %v", err)
				}
			}
		}
		last = now
	}
}
