package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Job ist eine laufende oder abgeschlossene Aktion auf einem Gerät
// (Update, Reboot). Läuft im Hintergrund, die Weboberfläche fragt den Stand ab.
type Job struct {
	Kind    string    `json:"kind"` // update | reboot
	Running bool      `json:"running"`
	OK      bool      `json:"ok"`
	Step    string    `json:"step"`
	Log     []string  `json:"log"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitempty"`
}

type Jobs struct {
	mu   sync.Mutex
	jobs map[string]*Job
	// Updates laufen nacheinander: fällt ein Gerät aus, bleibt der Rest heil.
	serial sync.Mutex
}

func newJobs() *Jobs { return &Jobs{jobs: map[string]*Job{}} }

func (j *Jobs) Get(id string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	x, ok := j.jobs[id]
	if !ok {
		return Job{}, false
	}
	c := *x
	c.Log = append([]string(nil), x.Log...)
	return c, true
}

// AnyRunning: läuft irgendwo ein Job? (Dann kein Selbst-Update.)
func (j *Jobs) AnyRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, x := range j.jobs {
		if x.Running {
			return true
		}
	}
	return false
}

// start legt einen Job an; false, wenn auf dem Gerät schon einer läuft.
func (j *Jobs) start(id, kind string) (*Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if x, ok := j.jobs[id]; ok && x.Running {
		return nil, false
	}
	x := &Job{Kind: kind, Running: true, Step: "wartet", Started: time.Now()}
	j.jobs[id] = x
	return x, true
}

func (j *Jobs) step(x *Job, name, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	j.mu.Lock()
	x.Step = msg
	x.Log = append(x.Log, time.Now().Format("15:04:05")+" "+msg)
	j.mu.Unlock()
	log.Printf("%s: %s", name, msg)
}

func (j *Jobs) finish(x *Job, ok bool) {
	j.mu.Lock()
	x.Running, x.OK, x.Ended = false, ok, time.Now()
	j.mu.Unlock()
}

// devState ist, was wir zum Prüfen vor/nach einem Neustart brauchen.
type devState struct {
	Version   string // nur die Nummer, z. B. "7.24.5"
	Uptime    int64
	CurrentFW string
	UpgradeFW string
	RB        bool
	Latest    string
	Status    string
}

// verNum liefert nur die Versionsnummer ("7.24.5 (stable) …" → "7.24.5"); leer bleibt leer.
func verNum(v string) string {
	f := strings.Fields(v)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func readState(ctx context.Context, cl *routeros.Client) (devState, error) {
	var s devState
	r, err := cl.RunContext(ctx, "/system/resource/print")
	if err != nil {
		return s, err
	}
	if len(r.Re) > 0 {
		s.Version = verNum(r.Re[0].Map["version"])
		s.Uptime = parseROSDuration(r.Re[0].Map["uptime"])
	}
	if r, err := cl.RunContext(ctx, "/system/routerboard/print"); err == nil && len(r.Re) > 0 {
		m := r.Re[0].Map
		s.RB = m["routerboard"] == "true"
		s.CurrentFW, s.UpgradeFW = m["current-firmware"], m["upgrade-firmware"]
	}
	if r, err := cl.RunContext(ctx, "/system/package/update/print"); err == nil && len(r.Re) > 0 {
		s.Latest = verNum(r.Re[0].Map["latest-version"])
		s.Status = r.Re[0].Map["status"]
	}
	return s, nil
}

// session öffnet eine API-Verbindung zum Gerät mit der aktuellen IP aus dem
// Store (kann sich nach einem Neustart per DHCP geändert haben).
func (p *Poller) session(ctx context.Context, id string) (*routeros.Client, error) {
	d, ok := p.st.Get(id)
	if !ok || d.IP == "" {
		return nil, fmt.Errorf("Gerät/IP unbekannt")
	}
	c, _ := p.vault.For(id)
	return p.dial(ctx, d.IP, c)
}

// waitReboot wartet, bis das Gerät nach einem Neustart wieder per API
// erreichbar ist (Uptime kleiner als vorher), und liefert den neuen Stand.
func (p *Poller) waitReboot(ctx context.Context, id string, before int64, timeout time.Duration, x *Job, name string) (devState, error) {
	deadline := time.Now().Add(timeout)
	wentDown := false
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return devState{}, ctx.Err()
		case <-time.After(10 * time.Second):
		}
		cl, err := p.session(ctx, id)
		if err != nil {
			if !wentDown {
				wentDown = true
				p.jobs.step(x, name, "Gerät startet neu …")
			}
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		s, err := readState(rctx, cl)
		cancel()
		cl.Close()
		if err != nil {
			continue
		}
		if s.Uptime < before || (wentDown && s.Uptime < 600) {
			return s, nil
		}
	}
	return devState{}, fmt.Errorf("Gerät ist nach %s nicht neu gestartet zurück", timeout)
}

// run führt cmd aus; Abbrüche der Verbindung durch den Neustart sind erwartet.
func runIgnoringDisconnect(ctx context.Context, cl *routeros.Client, timeout time.Duration, cmd ...string) error {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := cl.RunContext(rctx, cmd...)
	if err == nil {
		return nil
	}
	s := err.Error()
	if strings.Contains(s, "EOF") || strings.Contains(s, "reset by peer") || strings.Contains(s, "closed") ||
		strings.Contains(s, "deadline") || strings.Contains(s, "broken pipe") {
		return nil
	}
	return err
}

// saveBackup legt ein Systembackup an und prüft, dass die Datei existiert.
// Geräte mit kleinem Flash (16 MB, z. B. RB952/hAP lite) haben ein Verzeichnis
// "flash/"; alles außerhalb davon liegt im RAM und ist nach dem Neustart weg.
func saveBackup(ctx context.Context, cl *routeros.Client, base string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	r, err := cl.RunContext(rctx, "/file/print", "?name=flash", "=.proplist=name,type")
	if err != nil {
		return "", err
	}
	if len(r.Re) > 0 {
		base = "flash/" + base
	}
	if _, err := cl.RunContext(rctx, "/system/backup/save", "=name="+base, "=dont-encrypt=yes"); err != nil {
		return "", err
	}
	file := base + ".backup"
	// Das Schreiben kann der API-Antwort kurz hinterherlaufen.
	for i := 0; i < 10; i++ {
		r, err := cl.RunContext(rctx, "/file/print", "?name="+file, "=.proplist=name,size")
		if err != nil {
			return "", err
		}
		if len(r.Re) > 0 {
			return file, nil
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("%s nach dem Speichern nicht vorhanden", file)
}

// StartUpdate startet Updates (RouterOS, dann Firmware) für die Geräte,
// nacheinander im Hintergrund.
// Die Jobs laufen im Dienst-Kontext weiter, auch wenn der Browser die Anfrage beendet.
func (p *Poller) StartUpdate(ids []string) []Result {
	return p.startJobs(p.ctx, ids, "update", p.doUpdate)
}

func (p *Poller) StartReboot(ids []string) []Result {
	return p.startJobs(p.ctx, ids, "reboot", p.doReboot)
}

func (p *Poller) startJobs(ctx context.Context, ids []string, kind string, fn func(context.Context, string, *Job) error) []Result {
	var out []Result
	for _, id := range ids {
		d, ok := p.st.Get(id)
		if !ok {
			continue
		}
		x, ok := p.jobs.start(id, kind)
		if !ok {
			out = append(out, Result{ID: id, Identity: d.Identity, Msg: "es läuft bereits eine Aktion"})
			continue
		}
		out = append(out, Result{ID: id, Identity: d.Identity, OK: true, Msg: "eingeplant"})
		go func() {
			p.jobs.serial.Lock()
			defer p.jobs.serial.Unlock()
			name := d.Identity
			var err error
			func() {
				// Ein Programmfehler in einem Job darf nicht den ganzen Dienst beenden.
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("interner Fehler: %v", r)
					}
				}()
				err = fn(ctx, id, x)
			}()
			if err != nil {
				p.jobs.step(x, name, "FEHLER: %v", err)
			}
			p.jobs.finish(x, err == nil)
			p.Trigger(id)
		}()
	}
	return out
}

func (p *Poller) doUpdate(ctx context.Context, id string, x *Job) error {
	d, _ := p.st.Get(id)
	name := d.Identity
	cl, err := p.session(ctx, id)
	if err != nil {
		return err
	}
	defer func() { cl.Close() }()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	s, err := readState(rctx, cl)
	cancel()
	if err != nil {
		return err
	}
	p.jobs.step(x, name, "Stand: RouterOS %s, Firmware %s → %s", s.Version, s.CurrentFW, s.UpgradeFW)

	// 1. Backup auf den Manager (Rückfallebene, falls nach dem Update etwas fehlt)
	p.jobs.step(x, name, "erstelle Backup und lade es auf den Manager …")
	bk, err := p.BackupToManager(ctx, id, cl, "vor-update")
	if err != nil {
		return fmt.Errorf("Backup: %w – Update abgebrochen, nichts geändert", err)
	}
	p.jobs.step(x, name, "Backup auf dem Manager gespeichert: %s", bk)

	// 2. RouterOS
	p.jobs.step(x, name, "suche RouterOS-Update …")
	if err := runIgnoringDisconnect(ctx, cl, 90*time.Second, "/system/package/update/check-for-updates"); err != nil {
		return fmt.Errorf("Update-Prüfung: %w", err)
	}
	rctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	s, err = readState(rctx, cl)
	cancel()
	if err != nil {
		return err
	}
	if s.Latest == "" {
		return fmt.Errorf("Update-Server nicht erreichbar (%s)", s.Status)
	}
	if s.Latest != s.Version {
		p.jobs.step(x, name, "installiere RouterOS %s (bisher %s) – Download + Neustart …", s.Latest, s.Version)
		target := s.Latest
		if err := runIgnoringDisconnect(ctx, cl, 10*time.Minute, "/system/package/update/install"); err != nil {
			return fmt.Errorf("Installation: %w", err)
		}
		cl.Close()
		ns, err := p.waitReboot(ctx, id, s.Uptime, 15*time.Minute, x, name)
		if err != nil {
			return err
		}
		if ns.Version != target {
			return fmt.Errorf("nach Neustart RouterOS %s statt %s", ns.Version, target)
		}
		p.jobs.step(x, name, "RouterOS %s läuft", ns.Version)
		s = ns
		if cl, err = p.session(ctx, id); err != nil {
			return err
		}
		// Firmware-Stand neu lesen: nach RouterOS-Update gibt es meist eine neue
		rctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		s, err = readState(rctx, cl)
		cancel()
		if err != nil {
			return err
		}
	} else {
		p.jobs.step(x, name, "RouterOS %s ist aktuell", s.Version)
	}

	// 3. RouterBOARD-Firmware
	if !s.RB {
		p.jobs.step(x, name, "kein RouterBOARD – keine Firmware")
		return nil
	}
	if s.CurrentFW == s.UpgradeFW {
		p.jobs.step(x, name, "Firmware %s ist aktuell – fertig", s.CurrentFW)
		return nil
	}
	target := s.UpgradeFW
	p.jobs.step(x, name, "Firmware-Upgrade %s → %s …", s.CurrentFW, target)
	if err := runIgnoringDisconnect(ctx, cl, 2*time.Minute, "/system/routerboard/upgrade"); err != nil {
		return fmt.Errorf("Firmware-Upgrade: %w", err)
	}
	// Firmware wird beim Neustart übernommen. Kurz warten, bis sie geschrieben ist.
	time.Sleep(5 * time.Second)
	p.jobs.step(x, name, "Neustart zur Übernahme der Firmware …")
	runIgnoringDisconnect(ctx, cl, 10*time.Second, "/system/reboot")
	cl.Close()
	ns, err := p.waitReboot(ctx, id, s.Uptime, 10*time.Minute, x, name)
	if err != nil {
		return err
	}
	if ns.CurrentFW != target {
		return fmt.Errorf("Firmware nach Neustart %s statt %s", ns.CurrentFW, target)
	}
	p.jobs.step(x, name, "fertig: RouterOS %s, Firmware %s", ns.Version, ns.CurrentFW)
	return nil
}

func (p *Poller) doReboot(ctx context.Context, id string, x *Job) error {
	d, _ := p.st.Get(id)
	name := d.Identity
	cl, err := p.session(ctx, id)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	s, err := readState(rctx, cl)
	cancel()
	if err != nil {
		cl.Close()
		return err
	}
	p.jobs.step(x, name, "Neustart (Laufzeit bisher %s) …", strconv.FormatInt(s.Uptime/60, 10)+" min")
	runIgnoringDisconnect(ctx, cl, 10*time.Second, "/system/reboot")
	cl.Close()
	ns, err := p.waitReboot(ctx, id, s.Uptime, 10*time.Minute, x, name)
	if err != nil {
		return err
	}
	p.jobs.step(x, name, "wieder da (RouterOS %s)", ns.Version)
	return nil
}
