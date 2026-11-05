package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// PoE-Cycle: Port aus, 10 s warten, Port an, das Gerät dahinter startet neu. Ein Neustart
// des Switches reicht dafür nicht, der PoE-Ausgang bleibt dabei an (test-CRS328 gemessen).
// Alle Ports laufen nacheinander. Das Skript läuft auf dem Gerät, damit ein Port auch dann
// wieder angeht, wenn die Verbindung zum Manager abreißt. Der Uplink ist immer ausgenommen.

const poeOff = 10 * time.Second

func poeScript(port, mode string) string {
	n := q(port)
	return fmt.Sprintf("/interface ethernet poe set [find name=%s] poe-out=off\n:delay %ds\n/interface ethernet poe set [find name=%s] poe-out=%s\n", n, int(poeOff.Seconds()), n, mode)
}

// poeTarget ist ein Port mit seiner PoE-Einstellung vor dem Cycle.
type poeTarget struct{ Port, Mode string }

// poeTargets: Einzelner Port, wenn er PoE liefern kann. Alle = nur Ports, die gerade ein Gerät
// versorgen; leere Ports brauchen sonst je 10 s für nichts.
func poeTargets(poe, mon []map[string]string, uplinks []string, only string) ([]poeTarget, error) {
	state := map[string]string{}
	for _, m := range mon {
		state[m["name"]] = m["poe-out-status"]
	}
	var out []poeTarget
	for _, x := range poe {
		n, mode := x["name"], x["poe-out"]
		if only != "" && n != only {
			continue
		}
		if contains(uplinks, n) {
			if only != "" {
				return nil, fmt.Errorf("%s ist der Uplink dieses Geräts", n)
			}
			continue
		}
		if mode == "" || mode == "off" {
			if only != "" {
				return nil, fmt.Errorf("%s liefert kein PoE", n)
			}
			continue
		}
		if only == "" && state[n] != "powered-on" {
			continue
		}
		out = append(out, poeTarget{n, mode})
	}
	if len(out) == 0 {
		if only != "" {
			return nil, fmt.Errorf("%s kann kein PoE", only)
		}
		return nil, errors.New("Kein Port versorgt gerade ein Gerät")
	}
	return out, nil
}

// poeModeOf liest die Einstellung eines Ports.
func poeModeOf(ctx context.Context, cl *routeros.Client, port string) (string, error) {
	l, err := printAll(ctx, cl, "/interface/ethernet/poe/print", "?name="+port, "=.proplist=poe-out")
	if err != nil {
		return "", err
	}
	if len(l) == 0 {
		return "", fmt.Errorf("%s nicht gefunden", port)
	}
	return l[0]["poe-out"], nil
}

func (p *Poller) doPoECycle(ctx context.Context, id, only string, x *Job) error {
	d, _ := p.st.Get(id)
	name := d.Identity
	cl, err := p.session(ctx, id)
	if err != nil {
		return err
	}
	defer func() { cl.Close() }()
	rctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	poe, err := printAll(rctx, cl, "/interface/ethernet/poe/print", "=.proplist=name,poe-out")
	if err != nil {
		return fmt.Errorf("PoE lesen: %w", err)
	}
	if len(poe) == 0 {
		return errors.New("Das Gerät hat keinen PoE-Ausgang")
	}
	up, ok := uplinkPorts(readTopo(rctx, cl))
	if !ok && only == "" {
		return errors.New("Der Uplink ist nicht eindeutig erkennbar. Bitte die Ports einzeln wählen.")
	}
	var names []string
	for _, x := range poe {
		names = append(names, x["name"])
	}
	mon, _ := printAll(rctx, cl, "/interface/ethernet/poe/monitor", "=numbers="+joinComma(names), "=once=")
	list, err := poeTargets(poe, mon, up, only)
	if err != nil {
		return err
	}
	for i, t := range list {
		p.jobs.step(x, name, "%s (%d von %d): PoE aus, %d s warten, wieder an …", t.Port, i+1, len(list), int(poeOff.Seconds()))
		ectx, c := context.WithTimeout(ctx, 15*time.Second)
		_, err := cl.RunContext(ectx, "/execute", "=script="+poeScript(t.Port, t.Mode))
		c()
		if err != nil {
			return fmt.Errorf("%s: %w", t.Port, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poeOff + 3*time.Second):
		}
		// Prüfen, notfalls über eine neue Verbindung: Das Gerät hat das Skript selbst zu Ende geführt.
		var mode string
		for try := 0; try < 4; try++ {
			vctx, c := context.WithTimeout(ctx, 10*time.Second)
			mode, err = poeModeOf(vctx, cl, t.Port)
			c()
			if err == nil && mode == t.Mode {
				break
			}
			if err != nil {
				cl.Close()
				if n, e := p.session(ctx, id); e == nil {
					cl = n
				}
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			return fmt.Errorf("%s: Stand nicht lesbar: %w", t.Port, err)
		}
		if mode != t.Mode {
			return fmt.Errorf("%s: PoE steht auf %s statt %s", t.Port, mode, t.Mode)
		}
		p.jobs.step(x, name, "%s: PoE wieder an", t.Port)
	}
	p.jobs.step(x, name, "fertig: %d Port(s) neu versorgt", len(list))
	return nil
}

func joinComma(l []string) string {
	s := ""
	for i, x := range l {
		if i > 0 {
			s += ","
		}
		s += x
	}
	return s
}

// StartPoECycle startet den Cycle; only "" = alle versorgenden Ports.
func (p *Poller) StartPoECycle(id, only string) []Result {
	return p.startJobs(p.ctx, []string{id}, "poecycle", func(ctx context.Context, id string, x *Job) error {
		return p.doPoECycle(ctx, id, only, x)
	})
}

func (w *web) poeCycle(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Port string `json:"port"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(rw, "ungültige Anfrage", http.StatusBadRequest)
		return
	}
	res := w.pl.StartPoECycle(r.PathValue("id"), req.Port)
	if len(res) == 0 {
		res = []Result{{ID: r.PathValue("id"), Msg: "Gerät unbekannt"}}
	}
	writeJSON(rw, res[0])
}
