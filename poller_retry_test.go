package main

import (
	"testing"
	"time"
)

// Nicht erreichbare Geräte werden vor dem nächsten Intervall erneut versucht,
// aber höchstens einmal je retryAfter; erreichbare nie.
func TestRetryUnreachable(t *testing.T) {
	now := time.Now()
	st := newStore(t.TempDir())
	p := &Poller{st: st, queue: make(chan string, 16), busy: map[string]bool{}}
	add := func(sid, status string, last time.Time) string {
		id, _ := st.Observe(Device{SoftwareID: sid, IP: "192.0.2." + sid, Identity: sid}, "api")
		st.Update(id, func(d *Device) { d.APIStatus, d.LastPoll = status, last })
		return id
	}
	fresh := add("10", "unreachable", now.Add(-20*time.Second))
	stale := add("11", "unreachable", now.Add(-2*time.Minute))
	ok := add("12", "ok", now.Add(-2*time.Minute))
	p.retryUnreachable(now)
	got := map[string]bool{}
	for len(p.queue) > 0 {
		got[<-p.queue] = true
	}
	for id, want := range map[string]bool{fresh: false, stale: true, ok: false} {
		if got[id] != want {
			t.Errorf("%s: erneut versucht = %v, erwartet %v", id, got[id], want)
		}
	}
}
