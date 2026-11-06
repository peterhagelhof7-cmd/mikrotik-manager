package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// rollNet spielt ein Netz: Ein Neustart nimmt das Gerät und alles dahinter offline.
type rollNet struct {
	mu      sync.Mutex
	parent  map[string]string
	down    map[string]bool
	reboots []string
	stuck   string // kommt nicht zurück
	offline []string
}

func (n *rollNet) ops() rollOps {
	return rollOps{
		name:   func(s string) string { return s },
		settle: time.Millisecond, gateFor: 5 * time.Millisecond, gateEvery: time.Millisecond,
		probe: func(_ context.Context, id string) error {
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.down[id] || contains(n.offline, id) {
				return errors.New("offline")
			}
			return nil
		},
		reboot: func(ctx context.Context, id string, gate func(context.Context) error) error {
			if err := gate(ctx); err != nil {
				return err
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			n.reboots = append(n.reboots, id)
			if id == n.stuck {
				n.down[id] = true
				return errors.New("nicht zurück")
			}
			return nil
		},
	}
}

func TestRebootOrderBottomUp(t *testing.T) {
	parent := map[string]string{"sw2": "sw1", "sw1": "rt", "ap1": "sw2", "ap2": "sw1", "fremd": ""}
	got := rebootOrder([]string{"rt", "sw1", "ap2", "sw2", "ap1", "fremd"}, parent, func(s string) string { return s })
	want := []string{"ap1", "ap2", "sw2", "sw1", "fremd", "rt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	// Eine Schleife in alten Daten darf nicht hängen.
	if got := rebootOrder([]string{"a", "b"}, map[string]string{"a": "b", "b": "a"}, func(s string) string { return s }); len(got) != 2 {
		t.Fatal(got)
	}
}

func TestRebootBehind(t *testing.T) {
	parent := map[string]string{"sw2": "sw1", "sw1": "rt", "ap1": "sw2", "ap2": "sw1"}
	all := []string{"rt", "sw1", "sw2", "ap1", "ap2"}
	if got := rebootBehind("sw1", all, parent); !reflect.DeepEqual(got, []string{"sw2", "ap1", "ap2"}) {
		t.Fatal(got)
	}
	if got := rebootBehind("ap1", all, parent); got != nil {
		t.Fatal(got)
	}
}

func TestRollingRebootAllOK(t *testing.T) {
	n := &rollNet{down: map[string]bool{}}
	res := rollingReboot(context.Background(), []string{"a", "b", "c"}, []string{"a", "b", "c", "x"}, n.ops())
	if !reflect.DeepEqual(n.reboots, []string{"a", "b", "c"}) || len(res) != 3 {
		t.Fatalf("%v %+v", n.reboots, res)
	}
	for _, r := range res {
		if !r.OK {
			t.Fatalf("%+v", r)
		}
	}
}

func TestRollingRebootStopsAfterFailure(t *testing.T) {
	n := &rollNet{down: map[string]bool{}, stuck: "b"}
	res := rollingReboot(context.Background(), []string{"a", "b", "c", "d"}, []string{"a", "b", "c", "d"}, n.ops())
	if !reflect.DeepEqual(n.reboots, []string{"a", "b"}) {
		t.Fatalf("nach dem Fehler neu gestartet: %v", n.reboots)
	}
	if len(res) != 4 || !res[0].OK || res[1].OK || res[2].OK || res[3].OK || !strings.Contains(res[3].Msg, "Fehler bei b") {
		t.Fatalf("%+v", res)
	}
}

// Fehlt ein überwachtes Gerät, wird keines angefasst, auch wenn es selbst nicht gewählt ist.
func TestRollingRebootGateBlocks(t *testing.T) {
	n := &rollNet{down: map[string]bool{}, offline: []string{"nebenan"}}
	res := rollingReboot(context.Background(), []string{"a", "b"}, []string{"a", "b", "nebenan"}, n.ops())
	if len(n.reboots) != 0 || res[0].OK || !strings.Contains(res[0].Msg, "nebenan") {
		t.Fatalf("%v %+v", n.reboots, res)
	}
}

// Ein Gerät, das nach dem Switch verzögert zurückkommt, wird abgewartet.
func TestRollingRebootGateWaits(t *testing.T) {
	n := &rollNet{down: map[string]bool{"b": true}}
	o := n.ops()
	o.gateFor = time.Second
	go func() {
		time.Sleep(10 * time.Millisecond)
		n.mu.Lock()
		delete(n.down, "b")
		n.mu.Unlock()
	}()
	res := rollingReboot(context.Background(), []string{"a"}, []string{"a", "b"}, o)
	if len(n.reboots) != 1 || !res[0].OK {
		t.Fatalf("%v %+v", n.reboots, res)
	}
}

func TestRollingRebootCancelled(t *testing.T) {
	n := &rollNet{down: map[string]bool{}}
	o := n.ops()
	o.settle = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	res := rollingReboot(ctx, []string{"a", "b"}, []string{"a", "b"}, o)
	if len(n.reboots) != 1 || !res[0].OK || res[1].OK {
		t.Fatalf("%v %+v", n.reboots, res)
	}
}

func TestRebootRunsOnlyOncePerArea(t *testing.T) {
	var r rebootRuns
	a, b := rebootArea{Site: "1"}, rebootArea{Site: "2"}
	if !r.begin(a) || r.begin(a) || !r.begin(b) || !r.is(a) {
		t.Fatal("Sperre je Bereich")
	}
	r.end(a)
	if r.is(a) || !r.begin(a) {
		t.Fatal("wieder frei")
	}
}

func rebootPoller(t *testing.T, names ...string) (*Poller, []string) {
	t.Helper()
	dir := t.TempDir()
	st := newStore(dir)
	p := newPoller(Config{DataDir: dir}, st, nil)
	var ids []string
	for _, n := range names {
		id, _ := st.Observe(Device{SoftwareID: "SW-" + n, IP: "10.0.0.9", Identity: n}, "test")
		st.Update(id, func(d *Device) { d.IP = "" }) // ohne IP scheitert jede Sitzung sofort
		ids = append(ids, id)
	}
	return p, ids
}

func TestRebootPlanSave(t *testing.T) {
	p, ids := rebootPoller(t, "ap", "sw")
	a := rebootArea{}
	for in, want := range map[*RebootPlan]string{
		{Time: "25:00", Days: []int{1}}:                "Uhrzeit",
		{Time: "off", Days: []int{1}}:                  "Uhrzeit",
		{Time: "04:00"}:                                "Wochentag",
		{Time: "04:00", Days: []int{7}}:                "Wochentag",
		{Enabled: true, Time: "04:00", Days: []int{1}}: "Gerät",
		{Enabled: true, Time: "04:00", Days: []int{1}, Devices: []string{"gibt-es-nicht"}}: "Gerät",
	} {
		if err := p.SaveRebootPlan(a, *in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v", *in, err)
		}
	}
	if err := p.SaveRebootPlan(a, RebootPlan{Enabled: true, Time: "03:30", Days: []int{5, 1, 5}, Devices: []string{ids[1], ids[1], ids[0]}}); err != nil {
		t.Fatal(err)
	}
	c := p.RebootPlanCfg(a)
	if !c.Enabled || c.Time != "03:30" || !reflect.DeepEqual(c.Days, []int{1, 5}) || !reflect.DeepEqual(c.Devices, []string{ids[1], ids[0]}) {
		t.Fatalf("%+v", c)
	}
	// Das Ergebnis des letzten Laufs überlebt das Speichern.
	p.updateRebootPlan(a, func(s *RebootPlan) { s.LastReason = "geplant" })
	p.SaveRebootPlan(a, RebootPlan{Time: "03:30", Days: []int{1}})
	if c := p.RebootPlanCfg(a); c.LastReason != "geplant" || c.Enabled || len(c.Devices) != 0 {
		t.Fatalf("%+v", c)
	}
}

// Die Oberfläche zeigt die Reihenfolge und wer beim Neustart mit offline geht.
func TestRebootPlanViewOrder(t *testing.T) {
	p, ids := rebootPoller(t, "ap", "sw", "rt")
	p.st.Update(ids[0], func(d *Device) { d.Parent = &DeviceParent{ID: ids[1]} })
	p.st.Update(ids[1], func(d *Device) { d.Parent = &DeviceParent{ID: ids[2]} })
	if err := p.SaveRebootPlan(rebootArea{}, RebootPlan{Enabled: true, Time: "04:00", Days: []int{0}, Devices: []string{ids[2], ids[0], ids[1]}}); err != nil {
		t.Fatal(err)
	}
	v := p.RebootPlanView(context.Background(), rebootArea{}, false)
	if len(v.Order) != 3 || v.Order[0].Identity != "ap" || v.Order[1].Identity != "sw" || v.Order[2].Identity != "rt" {
		t.Fatalf("%+v", v.Order)
	}
	if !reflect.DeepEqual(v.Order[1].Behind, []string{"ap"}) || !reflect.DeepEqual(v.Order[2].Behind, []string{"ap", "sw"}) || v.Order[0].Behind != nil || v.Next.IsZero() {
		t.Fatalf("%+v", v)
	}
}

// Sperrt das Tor, läuft der Job als Fehler aus und das Gerät bekommt keinen Neustart.
func TestRebootStepGateStopsJob(t *testing.T) {
	p, ids := rebootPoller(t, "ap")
	want := errors.New("offline: nebenan")
	if err := p.rebootStep(context.Background(), ids[0], func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	x, ok := p.jobs.Get(ids[0])
	if !ok || x.Running || x.OK || x.Kind != "reboot" || strings.Contains(strings.Join(x.Log, " "), "Laufzeit bisher") {
		t.Fatalf("%+v", x)
	}
}

// Gesamter Lauf mit Geräten, die nicht antworten: nichts wird angefasst, das Ergebnis bleibt stehen.
func TestRunRebootPlanNobodyAnswers(t *testing.T) {
	old := rebootGateTimeout
	rebootGateTimeout = 0
	defer func() { rebootGateTimeout = old }()
	p, ids := rebootPoller(t, "ap", "sw", "nicht-gewaehlt")
	a := rebootArea{}
	if err := p.RunRebootPlan(a, "von Hand"); err == nil {
		t.Fatal("ohne Gerät darf nichts laufen")
	}
	if err := p.SaveRebootPlan(a, RebootPlan{Time: "04:00", Days: []int{0}, Devices: ids[:2]}); err != nil {
		t.Fatal(err)
	}
	if err := p.RunRebootPlan(a, "von Hand"); err != nil {
		t.Fatal(err)
	}
	var c RebootPlan
	for i := 0; i < 500 && (p.reboots.is(a) || i == 0); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	c = p.RebootPlanCfg(a)
	if len(c.LastResults) != 2 || c.LastResults[0].OK || c.LastResults[1].OK || c.LastReason != "von Hand" {
		t.Fatalf("%+v", c)
	}
	if !strings.Contains(c.LastResults[0].Msg, "offline") || !strings.Contains(c.LastResults[1].Msg, "abgebrochen") || !strings.Contains(c.LastNote, "nicht-gewaehlt") {
		t.Fatalf("%+v", c)
	}
}
