package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Verlauf je Gerät: was über den Manager an diesem Gerät gemacht wurde, von
// wem und mit welchem Ergebnis. Datei DATA_DIR/history/<Geräte-ID>.jsonl,
// eine Zeile je Eintrag; ab 1000 Einträgen werden die ältesten verworfen.

type HistEntry struct {
	Time   time.Time `json:"time"`
	User   string    `json:"user"` // Web-Benutzer oder „Manager“ (automatisch)
	Action string    `json:"action"`
	Detail string    `json:"detail,omitempty"`
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
}

const histKeep = 1000

type devHistory struct {
	mu  sync.Mutex
	dir string
}

func newDevHistory(dataDir string) *devHistory {
	return &devHistory{dir: filepath.Join(dataDir, "history")}
}

func (h *devHistory) path(id string) string {
	return filepath.Join(h.dir, safeName(id)+".jsonl")
}

// Add hängt einen Eintrag an (Fehler beim Schreiben werden nur ignoriert –
// der Verlauf darf keine Aktion verhindern).
func (h *devHistory) Add(id, user, action, detail string, err error) {
	if h == nil || id == "" {
		return
	}
	e := HistEntry{Time: time.Now(), User: user, Action: action, Detail: detail, OK: err == nil}
	if err != nil {
		e.Error = err.Error()
	}
	b, _ := json.Marshal(e)
	h.mu.Lock()
	defer h.mu.Unlock()
	os.MkdirAll(h.dir, 0o700)
	f, ferr := os.OpenFile(h.path(id), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	f.Write(append(b, '\n'))
	fi, _ := f.Stat()
	f.Close()
	// Begrenzen: ab histKeep×600 Byte auf die neuesten Einträge kürzen – höchstens
	// histKeep und höchstens halb so viele Byte, damit nicht jeder weitere Eintrag
	// gleich wieder kürzt.
	if fi != nil && fi.Size() > histKeep*600 {
		l := h.readLocked(id)
		var lines [][]byte
		size := 0
		for i := len(l) - 1; i >= 0 && len(lines) < histKeep; i-- {
			bb, _ := json.Marshal(l[i])
			if size+len(bb)+1 > histKeep*300 {
				break
			}
			lines = append(lines, bb)
			size += len(bb) + 1
		}
		var buf bytes.Buffer
		for i := len(lines) - 1; i >= 0; i-- {
			buf.Write(append(lines[i], '\n'))
		}
		writeFileAtomic(h.path(id), buf.Bytes(), 0o600)
	}
}

// List liefert die Einträge eines Geräts, neueste zuerst (höchstens max).
func (h *devHistory) List(id string, max int) []HistEntry {
	h.mu.Lock()
	l := h.readLocked(id)
	h.mu.Unlock()
	out := make([]HistEntry, 0, len(l))
	for i := len(l) - 1; i >= 0 && (max <= 0 || len(out) < max); i-- {
		out = append(out, l[i])
	}
	return out
}

func (h *devHistory) readLocked(id string) []HistEntry {
	f, err := os.Open(h.path(id))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []HistEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e HistEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// Forget löscht den Verlauf eines entfernten Geräts.
func (h *devHistory) Forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	os.Remove(h.path(id))
}

// safeName: Geräte-IDs (Software-ID, MAC, „ip:…“) als Dateiname, nichts Pfadartiges.
func safeName(id string) string {
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_", ":", "_").Replace(id)
}
