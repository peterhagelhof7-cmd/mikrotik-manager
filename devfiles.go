package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Dateien auf dem Gerät löschen. Geschützt ist nur, was gerade gebraucht wird:
// Datenträger und Ordner, der Rückweg einer unbestätigten Änderung und die
// Dateien einer laufenden Aktion. Alte mm-Dateien sind Reste und dürfen weg.

const maxFileDelete = 100

// DeviceFileDelete ist das Ergebnis je Datei.
type DeviceFileDelete struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Msg  string `json:"msg,omitempty"`
}

// classifyFile setzt den Schutz einer Datei. marker sind die Skripte mm-undo-<tag>
// auf dem Gerät: Solange eines steht, wartet ein Wächter auf seine Bestätigung.
func classifyFile(f *DeviceFile, marker map[string]bool, jobRunning bool) {
	base := path.Base(f.Name)
	own := strings.HasPrefix(base, "mm-")
	switch {
	case f.Type == "disk" || f.Type == "directory":
		f.Protected, f.Why = true, "Datenträger oder Ordner"
	case own && marker[strings.TrimSuffix(base, ".backup")]:
		f.Protected, f.Why = true, "Rückweg einer unbestätigten Änderung"
	case own && jobRunning:
		f.Protected, f.Why = true, "gehört zur laufenden Aktion"
	case own:
		f.Leftover = true
	}
}

func (p *Poller) readDeviceFiles(ctx context.Context, cl *routeros.Client, id string) ([]DeviceFile, error) {
	l, err := printAll(ctx, cl, "/file/print", "=.proplist=.id,name,type,size,creation-time,last-modified")
	if err != nil {
		return nil, err
	}
	scripts, err := printAll(ctx, cl, "/system/script/print", "=.proplist=name")
	if err != nil {
		return nil, err
	}
	marker := map[string]bool{}
	for _, s := range scripts {
		if strings.HasPrefix(s["name"], "mm-undo") {
			marker[s["name"]] = true
		}
	}
	j, _ := p.jobs.Get(id)
	out := make([]DeviceFile, 0, len(l))
	for _, m := range l {
		f := DeviceFile{Name: m["name"], Type: m["type"], Time: m["last-modified"], rosID: m[".id"]}
		if f.Time == "" {
			f.Time = m["creation-time"]
		}
		// v6 liefert "12.5KiB", v7 Bytes
		f.Size = parseSize(m["size"])
		classifyFile(&f, marker, j.Running)
		out = append(out, f)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// DeleteDeviceFiles liest die Liste neu, löscht nur Ungeschütztes und prüft danach.
func (p *Poller) DeleteDeviceFiles(ctx context.Context, id string, names []string) ([]DeviceFileDelete, error) {
	if d, ok := p.st.Get(id); ok {
		if msg := cloudBlock(d); msg != "" {
			return nil, errors.New(msg)
		}
	}
	cl, err := p.session(ctx, id)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	files, err := p.readDeviceFiles(ctx, cl, id)
	if err != nil {
		return nil, err
	}
	byName := map[string]DeviceFile{}
	for _, f := range files {
		byName[f.Name] = f
	}
	out := make([]DeviceFileDelete, 0, len(names))
	tried := false
	for _, n := range names {
		r := DeviceFileDelete{Name: n}
		f, ok := byName[n]
		switch {
		case !ok:
			r.OK, r.Msg = true, "war schon weg"
		case f.Protected:
			r.Msg = "geschützt: " + f.Why
		default:
			if _, err := cl.RunContext(ctx, "/file/remove", "=numbers="+f.rosID); err != nil {
				r.Msg = err.Error()
			} else {
				tried = true
				r.OK = true
			}
		}
		out = append(out, r)
	}
	if !tried {
		return out, nil
	}
	left, err := p.readDeviceFiles(ctx, cl, id)
	if err != nil {
		return nil, fmt.Errorf("Löschen ausgeführt, Prüfung fehlgeschlagen: %w", err)
	}
	still := map[string]bool{}
	for _, f := range left {
		still[f.Name] = true
	}
	for i := range out {
		if _, had := byName[out[i].Name]; had && out[i].OK && still[out[i].Name] {
			out[i].OK, out[i].Msg = false, "liegt noch auf dem Gerät"
		}
	}
	return out, nil
}

func (w *web) deleteDeviceFiles(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&req); err != nil || len(req.Names) == 0 {
		http.Error(rw, "Dateien nötig", http.StatusBadRequest)
		return
	}
	if len(req.Names) > maxFileDelete {
		http.Error(rw, fmt.Sprintf("höchstens %d Dateien auf einmal", maxFileDelete), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := w.pl.DeleteDeviceFiles(ctx, r.PathValue("id"), req.Names)
	if err != nil {
		http.Error(rw, "Dateien löschen: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, map[string]any{"results": res})
}
