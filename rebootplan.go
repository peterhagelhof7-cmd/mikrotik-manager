package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Neustart-Routine: Zu einer festen Zeit an gewählten Wochentagen startet der Manager die
// gewählten Geräte eines Bereichs (Cloud: Standort, lokal: das ganze Netz) neu. Die Regel
// lautet „nie zwei gleichzeitig offline“. Dafür reicht es nicht, nacheinander zu arbeiten:
//
//   - Vor jedem Neustart müssen alle überwachten Geräte per API antworten (Tor). Fehlt eines,
//     wird gewartet und am Ende abgebrochen. Der Stand kommt vom Gerät, nicht vom letzten Poll.
//   - Die Reihenfolge läuft von unten nach oben durch den Netzwerk-Baum. Ein Switch nimmt
//     beim Neustart die Geräte dahinter mit, das lässt sich nicht vermeiden; die Oberfläche
//     zeigt es deshalb an. Bleibt ein Access Point weg, ist der Switch noch unberührt.
//   - Nach dem ersten Fehler passiert nichts mehr. Ein Gerät, das nicht zurückkommt, ist
//     schon das eine Gerät, das offline sein darf.
//
// Der Neustart läuft als normaler Job. Jobs laufen nacheinander (Jobs.serial), das Tor sitzt
// im Job: So kann auch ein Update oder eine zweite Routine nicht dazwischenkommen.

var (
	rebootSettle      = time.Minute     // Pause nach einem Gerät, WLAN und Tunnel brauchen länger als die API
	rebootGateTimeout = 5 * time.Minute // so lange darf das Tor auf Geräte warten
	rebootGateEvery   = 15 * time.Second
	rebootBusyTimeout = 10 * time.Minute // andere Aktion auf dem Gerät abwarten
)

// rebootArea ist der Bereich einer Routine. Der lokale Manager kennt nur den leeren Bereich.
type rebootArea struct{ Tenant, Site string }

// RebootPlan ist die Einstellung eines Bereichs.
type RebootPlan struct {
	Enabled bool     `json:"enabled"`
	Time    string   `json:"time"`    // hh:mm Ortszeit
	Days    []int    `json:"days"`    // 0 = Sonntag … 6 = Samstag
	Devices []string `json:"devices"` // leer = keines; ein Neustart braucht eine bewusste Wahl

	LastRun     time.Time    `json:"lastRun,omitempty"`
	LastReason  string       `json:"lastReason,omitempty"`
	LastNote    string       `json:"lastNote,omitempty"`
	LastResults []AutoResult `json:"lastResults,omitempty"`
}

// RebootStep ist ein Schritt der Reihenfolge.
type RebootStep struct {
	ID       string   `json:"id"`
	Identity string   `json:"identity"`
	Behind   []string `json:"behind,omitempty"` // gehen beim Neustart mit offline
}

// RebootPlanView ergänzt die Einstellung für die Oberfläche.
type RebootPlanView struct {
	RebootPlan
	Next    time.Time    `json:"next,omitempty"`
	Running bool         `json:"running"`
	Order   []RebootStep `json:"order"`
}

// rebootRuns merkt sich die laufenden Routinen.
type rebootRuns struct {
	mu      sync.Mutex
	running map[rebootArea]bool
}

func (r *rebootRuns) begin(a rebootArea) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[a] {
		return false
	}
	if r.running == nil {
		r.running = map[rebootArea]bool{}
	}
	r.running[a] = true
	return true
}

func (r *rebootRuns) end(a rebootArea) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, a)
}

func (r *rebootRuns) is(a rebootArea) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[a]
}

var rebootMu sync.Mutex // Datei der Einstellung

func (p *Poller) RebootPlanCfg(a rebootArea) RebootPlan {
	rebootMu.Lock()
	defer rebootMu.Unlock()
	return p.readRebootPlan(a)
}

func (p *Poller) readRebootPlan(a rebootArea) RebootPlan {
	c := RebootPlan{Time: "04:00", Days: []int{0}}
	if b, err := os.ReadFile(p.rebootPath(a)); err == nil {
		json.Unmarshal(b, &c)
	}
	// Ein Gerät, das den Bereich verlassen hat, gehört nicht mehr dazu.
	in := map[string]bool{}
	for _, d := range p.areaDevices(a) {
		in[d.ID] = true
	}
	var keep []string
	for _, id := range c.Devices {
		if in[id] {
			keep = append(keep, id)
		}
	}
	c.Devices = keep
	return c
}

func (p *Poller) writeRebootPlan(a rebootArea, c RebootPlan) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p.rebootPath(a), b, 0o600)
}

// updateRebootPlan ändert die gespeicherte Einstellung unter der Sperre.
func (p *Poller) updateRebootPlan(a rebootArea, f func(*RebootPlan)) error {
	rebootMu.Lock()
	defer rebootMu.Unlock()
	c := p.readRebootPlan(a)
	f(&c)
	return p.writeRebootPlan(a, c)
}

// SaveRebootPlan übernimmt die Einstellung aus der Oberfläche (Ergebnisse bleiben).
func (p *Poller) SaveRebootPlan(a rebootArea, in RebootPlan) error {
	if _, ok := nextBackupAt(time.Now(), in.Time); !ok || in.Time == "off" {
		return fmt.Errorf("Uhrzeit %q ungültig (hh:mm)", in.Time)
	}
	seen := map[int]bool{}
	var days []int
	for _, d := range in.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("Wochentag %d ungültig", d)
		}
		if !seen[d] {
			seen[d] = true
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		return fmt.Errorf("mindestens einen Wochentag wählen")
	}
	sort.Ints(days)
	area := map[string]bool{}
	for _, d := range p.areaDevices(a) {
		area[d.ID] = true
	}
	var ids []string
	for _, id := range in.Devices {
		if _, known := p.st.Get(id); known && !area[id] {
			return fmt.Errorf("Gerät %s gehört nicht zu diesem Bereich", id)
		}
		if area[id] && !contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if in.Enabled && len(ids) == 0 {
		return fmt.Errorf("mindestens ein Gerät wählen")
	}
	return p.updateRebootPlan(a, func(c *RebootPlan) {
		c.Enabled, c.Time, c.Days, c.Devices = in.Enabled, in.Time, days, ids
	})
}

// rebootOrder: unten im Baum zuerst, die Wurzel zuletzt; gleich tief nach Name.
func rebootOrder(ids []string, parent map[string]string, name func(string) string) []string {
	depth := func(id string) int {
		n, seen := 0, map[string]bool{id: true}
		for {
			up, ok := parent[id]
			if !ok || up == "" || seen[up] {
				return n
			}
			seen[up] = true
			id = up
			n++
		}
	}
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := depth(out[i]), depth(out[j]); a != b {
			return a > b
		}
		return strings.ToLower(name(out[i])) < strings.ToLower(name(out[j]))
	})
	return out
}

// rebootBehind: alle Geräte, deren Weg nach oben über id führt.
func rebootBehind(id string, all []string, parent map[string]string) []string {
	var out []string
	for _, x := range all {
		seen := map[string]bool{x: true}
		for c := x; ; {
			up, ok := parent[c]
			if !ok || up == "" || seen[up] {
				break
			}
			if up == id {
				out = append(out, x)
				break
			}
			seen[up] = true
			c = up
		}
	}
	return out
}

// rebootParents liefert den Elternteil je Gerät. live liest den Baum neu und speichert ihn am
// Gerät; ohne Baum (oder ohne live) gilt der gespeicherte Stand.
func (p *Poller) rebootParents(ctx context.Context, devs []Device, live bool) map[string]string {
	out := map[string]string{}
	if live && len(devs) > 0 {
		ids, root := p.topoScope(devs[0])
		parentMu.Lock()
		t := p.Topology(ctx, ids, root)
		found := parentsFrom(t, time.Now())
		for x, par := range found {
			par := par
			p.st.Update(x, func(c *Device) { c.Parent = &par })
		}
		parentMu.Unlock()
		if t.Root != nil {
			for x, par := range found {
				out[x] = par.ID
			}
			return out
		}
	}
	for _, d := range devs {
		if cur, ok := p.st.Get(d.ID); ok && cur.Parent != nil {
			out[d.ID] = cur.Parent.ID
		}
	}
	return out
}

func (p *Poller) rebootSteps(ctx context.Context, a rebootArea, ids []string, live bool) []RebootStep {
	devs := p.areaDevices(a)
	names, all := map[string]string{}, make([]string, 0, len(devs))
	for _, d := range devs {
		names[d.ID] = d.Identity
		all = append(all, d.ID)
	}
	parent := p.rebootParents(ctx, devs, live)
	out := []RebootStep{}
	for _, id := range rebootOrder(ids, parent, func(s string) string { return names[s] }) {
		s := RebootStep{ID: id, Identity: names[id]}
		for _, b := range rebootBehind(id, all, parent) {
			s.Behind = append(s.Behind, names[b])
		}
		sort.Strings(s.Behind)
		out = append(out, s)
	}
	return out
}

func (p *Poller) RebootPlanView(ctx context.Context, a rebootArea, live bool) RebootPlanView {
	c := p.RebootPlanCfg(a)
	v := RebootPlanView{RebootPlan: c, Running: p.reboots.is(a), Order: p.rebootSteps(ctx, a, c.Devices, live)}
	if c.Enabled {
		v.Next, _ = nextAutoAt(time.Now(), c.Time, c.Days)
	}
	return v
}

// rollOps sind die Schritte eines Laufs; die Tests setzen eigene ein.
type rollOps struct {
	probe     func(ctx context.Context, id string) error
	reboot    func(ctx context.Context, id string, gate func(context.Context) error) error
	name      func(id string) string
	settle    time.Duration
	gateFor   time.Duration
	gateEvery time.Duration
}

func rollWait(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// rollGate wartet, bis alle überwachten Geräte antworten.
func (o rollOps) rollGate(ctx context.Context, watch []string) error {
	deadline := time.Now().Add(o.gateFor)
	for {
		var down []string
		for _, id := range watch {
			if err := o.probe(ctx, id); err != nil {
				down = append(down, o.name(id))
			}
		}
		if len(down) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("offline: %s", strings.Join(down, ", "))
		}
		if err := rollWait(ctx, o.gateEvery); err != nil {
			return err
		}
	}
}

// rollingReboot startet order nacheinander neu und hört beim ersten Fehler auf.
func rollingReboot(ctx context.Context, order, watch []string, o rollOps) []AutoResult {
	out := make([]AutoResult, 0, len(order))
	for i, id := range order {
		if err := o.reboot(ctx, id, func(c context.Context) error { return o.rollGate(c, watch) }); err != nil {
			out = append(out, AutoResult{ID: id, Identity: o.name(id), Msg: err.Error()})
			return append(out, rollSkipped(order[i+1:], o, "nicht neu gestartet, Lauf nach Fehler bei "+o.name(id)+" abgebrochen")...)
		}
		out = append(out, AutoResult{ID: id, Identity: o.name(id), OK: true, Msg: "neu gestartet"})
		if i < len(order)-1 {
			if err := rollWait(ctx, o.settle); err != nil {
				return append(out, rollSkipped(order[i+1:], o, "Lauf abgebrochen: "+err.Error())...)
			}
		}
	}
	return out
}

func rollSkipped(ids []string, o rollOps, msg string) []AutoResult {
	var out []AutoResult
	for _, id := range ids {
		out = append(out, AutoResult{ID: id, Identity: o.name(id), Msg: msg})
	}
	return out
}

// probeDevice fragt das Gerät selbst; der Store kennt nur die letzte Abfrage.
func (p *Poller) probeDevice(ctx context.Context, id string) error {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cl, err := p.session(rctx, id)
	if err != nil {
		return err
	}
	defer cl.Close()
	_, err = cl.RunContext(rctx, "/system/resource/print")
	return err
}

// rebootStep läuft als Job. Das Tor sitzt darin, also hinter Jobs.serial.
func (p *Poller) rebootStep(ctx context.Context, id string, gate func(context.Context) error) error {
	d, ok := p.st.Get(id)
	if !ok {
		return fmt.Errorf("Gerät unbekannt")
	}
	var x *Job
	for deadline := time.Now().Add(rebootBusyTimeout); ; {
		if x, ok = p.jobs.start(id, "reboot"); ok {
			break
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("auf dem Gerät läuft noch eine andere Aktion")
		}
		if err := rollWait(ctx, 5*time.Second); err != nil {
			return err
		}
	}
	return p.runJob(ctx, id, d.Identity, x, "reboot", func(ctx context.Context, id string, x *Job) error {
		p.jobs.step(x, d.Identity, "Neustart-Routine: prüft, ob alle Geräte erreichbar sind")
		if err := gate(ctx); err != nil {
			return err
		}
		return p.doReboot(ctx, id, x)
	})
}

// RunRebootPlan startet den Lauf im Hintergrund.
func (p *Poller) RunRebootPlan(a rebootArea, reason string) error {
	c := p.RebootPlanCfg(a)
	if len(c.Devices) == 0 {
		return fmt.Errorf("kein Gerät gewählt")
	}
	if !p.reboots.begin(a) {
		return fmt.Errorf("die Neustart-Routine läuft bereits")
	}
	if err := p.updateRebootPlan(a, func(s *RebootPlan) {
		s.LastRun, s.LastReason, s.LastNote, s.LastResults = time.Now(), reason, "", nil
	}); err != nil {
		p.reboots.end(a)
		return err
	}
	go func() {
		defer p.reboots.end(a)
		p.runRebootPlan(p.ctx, a, c.Devices)
	}()
	return nil
}

func (p *Poller) runRebootPlan(ctx context.Context, a rebootArea, selected []string) {
	devs := p.areaDevices(a)
	names := map[string]string{}
	for _, d := range devs {
		names[d.ID] = d.Identity
	}
	// Ein nicht gewähltes Gerät, das schon vor dem Lauf fehlt, bleibt außen vor: Sonst blockiert
	// ein längst abgebautes Gerät die Routine für immer. Es steht aber im Ergebnis.
	var watch, gone []string
	for _, d := range devs {
		if contains(selected, d.ID) || p.probeDevice(ctx, d.ID) == nil {
			watch = append(watch, d.ID)
		} else {
			gone = append(gone, d.Identity)
		}
	}
	lctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	parent := p.rebootParents(lctx, devs, true)
	cancel()
	order := rebootOrder(selected, parent, func(s string) string { return names[s] })
	res := rollingReboot(ctx, order, watch, rollOps{probe: p.probeDevice, reboot: p.rebootStep,
		name: func(s string) string { return names[s] }, settle: rebootSettle, gateFor: rebootGateTimeout, gateEvery: rebootGateEvery})
	note := ""
	if len(gone) > 0 {
		sort.Strings(gone)
		note = "Schon vor dem Lauf offline und nicht gewählt: " + strings.Join(gone, ", ")
	}
	if err := p.updateRebootPlan(a, func(s *RebootPlan) { s.LastNote, s.LastResults = note, res }); err != nil {
		log.Printf("Neustart-Routine: Ergebnis speichern: %v", err)
	}
	var bad []string
	for _, r := range res {
		if !r.OK {
			bad = append(bad, r.Identity)
		}
	}
	log.Printf("Neustart-Routine fertig (%q/%q): %d Geräte, nicht ok: %s", a.Tenant, a.Site, len(res), strings.Join(bad, ", "))
}

// runRebootPlans prüft jede halbe Minute: Lag seit dem letzten Blick ein Termin?
func (p *Poller) runRebootPlans(ctx context.Context) {
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
		for _, a := range p.rebootAreas() {
			if c := p.RebootPlanCfg(a); c.Enabled {
				if due, ok := nextAutoAt(last, c.Time, c.Days); ok && !due.After(now) {
					if err := p.RunRebootPlan(a, "geplant"); err != nil {
						log.Printf("Neustart-Routine (%q/%q): %v", a.Tenant, a.Site, err)
					}
				}
			}
		}
		last = now
	}
}

func (w *web) rebootGet(rw http.ResponseWriter, r *http.Request, a rebootArea) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	writeJSON(rw, w.pl.RebootPlanView(ctx, a, r.URL.Query().Get("live") != ""))
}

func (w *web) rebootSave(rw http.ResponseWriter, r *http.Request, a rebootArea) {
	var in RebootPlan
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&in); err != nil {
		http.Error(rw, "ungültige Daten", http.StatusBadRequest)
		return
	}
	if err := w.pl.SaveRebootPlan(a, in); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(rw, w.pl.RebootPlanView(r.Context(), a, false))
}

func (w *web) rebootRun(rw http.ResponseWriter, r *http.Request, a rebootArea) {
	if err := w.pl.RunRebootPlan(a, "von Hand"); err != nil {
		http.Error(rw, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(rw, w.pl.RebootPlanView(r.Context(), a, false))
}
