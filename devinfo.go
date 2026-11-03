package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Detailseite eines Geräts: Verlauf vom Gerät (RouterOS „/system history“:
// wer hat was geändert, auch per Winbox/WebFig), Dateien auf dem Gerät und
// Konfigurationsunterschiede zwischen den gespeicherten Text-Exports (.rsc).
// Alles nur lesend.

// DeviceHistItem ist ein Eintrag aus /system history (RouterOS hält ihn im
// Speicher, nach einem Neustart beginnt die Liste neu).
type DeviceHistItem struct {
	Time   string `json:"time"` // wie vom Gerät geliefert
	By     string `json:"by"`
	Action string `json:"action"`
	Policy string `json:"policy,omitempty"`
}

// DeviceFile ist eine Datei auf dem Gerät.
type DeviceFile struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	Time string `json:"time,omitempty"`
}

func (p *Poller) DeviceHistory(ctx context.Context, id string) ([]DeviceHistItem, error) {
	cl, err := p.session(ctx, id)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	l, err := printAll(ctx, cl, "/system/history/print")
	if err != nil {
		return nil, err
	}
	out := make([]DeviceHistItem, 0, len(l))
	for _, m := range l {
		out = append(out, DeviceHistItem{Time: m["time"], By: m["by"], Action: m["action"], Policy: m["policy"]})
	}
	// RouterOS liefert die älteste Änderung zuerst – neueste nach oben.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (p *Poller) DeviceFiles(ctx context.Context, id string) ([]DeviceFile, error) {
	cl, err := p.session(ctx, id)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	l, err := printAll(ctx, cl, "/file/print", "=.proplist=name,type,size,creation-time,last-modified")
	if err != nil {
		return nil, err
	}
	out := make([]DeviceFile, 0, len(l))
	for _, m := range l {
		f := DeviceFile{Name: m["name"], Type: m["type"], Time: m["last-modified"]}
		if f.Time == "" {
			f.Time = m["creation-time"]
		}
		// v6 liefert "12.5KiB", v7 Bytes
		f.Size = parseSize(m["size"])
		out = append(out, f)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

func parseSize(s string) int64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, " ", ""))
	mul := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}} {
		if strings.HasSuffix(s, u.suf) {
			s, mul = strings.TrimSuffix(s, u.suf), u.m
			break
		}
	}
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f * float64(mul))
}

// --- Konfigurationsunterschiede zwischen gespeicherten .rsc-Exports ---

// DiffLine ist eine Zeile im Unterschied: Op '+' hinzu, '-' weg, ' ' Kontext, '…' ausgelassen.
type DiffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// ConfigChange ist der Unterschied zwischen zwei aufeinanderfolgenden Exports.
type ConfigChange struct {
	From    string     `json:"from"` // Name des älteren .rsc
	To      string     `json:"to"`
	Time    time.Time  `json:"time"` // Zeit des neueren Exports
	Added   int        `json:"added"`
	Removed int        `json:"removed"`
	Lines   []DiffLine `json:"lines"`
	TooBig  bool       `json:"tooBig,omitempty"` // zu groß für einen Zeilenvergleich
}

// ConfigChanges vergleicht die gespeicherten Exports eines Geräts paarweise
// (alt → neu) und liefert nur echte Änderungen, neueste zuerst.
func (p *Poller) ConfigChanges(id string, max int) []ConfigChange {
	var rsc []BackupFile
	for _, b := range p.ListBackups(id) {
		if strings.HasSuffix(b.Name, ".rsc") {
			rsc = append(rsc, b)
		}
	}
	sort.Slice(rsc, func(a, b int) bool { return rsc[a].Name < rsc[b].Name }) // Name beginnt mit der Zeit
	var out []ConfigChange
	var prev []string
	prevName := ""
	for _, f := range rsc {
		b, err := os.ReadFile(filepath.Join(p.backupDir(id), f.Name))
		if err != nil {
			continue
		}
		cur := rscLines(string(b))
		if prevName != "" {
			if c, changed := diffConfig(prev, cur); changed {
				c.From, c.To, c.Time = prevName, f.Name, f.Time
				out = append(out, c)
			}
		}
		prev, prevName = cur, f.Name
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// Geheimnisse in Exports: wireless (wpa2-pre-shared-key), wifi (security.passphrase),
// WireGuard (private-key, preshared-key), Benutzer/PPP (password), IPsec (secret) …
var secretRe = regexp.MustCompile(`(^|[\s.])((?:wpa2?-)?pre-shared-key|preshared-key|passphrase|password|secret|private-key|authentication-key)=("(?:[^"\\]|\\.)*"|\S+)`)

// rscLines: Zeilen eines Exports ohne Kopfkommentare (Zeitstempel, Software-ID),
// Fortsetzungszeilen („\“ am Ende) zusammengefügt, Geheimnisse maskiert.
func rscLines(s string) []string {
	var out []string
	cur := ""
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if cur == "" && strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		part := l
		if cur != "" {
			part = strings.TrimLeft(l, " ") // Fortsetzung ist eingerückt
		}
		if strings.HasSuffix(part, "\\") {
			cur += strings.TrimSuffix(part, "\\")
			continue
		}
		full := cur + part
		cur = ""
		if strings.TrimSpace(full) == "" {
			continue
		}
		out = append(out, secretRe.ReplaceAllString(full, "${1}${2}=•••"))
	}
	if cur != "" {
		out = append(out, secretRe.ReplaceAllString(cur, "${1}${2}=•••"))
	}
	return out
}

// diffConfig: Zeilenvergleich (LCS) mit 2 Zeilen Kontext.
func diffConfig(a, b []string) (ConfigChange, bool) {
	var c ConfigChange
	if len(a)*len(b) > 9_000_000 {
		// Sehr große Exporte: nur zählen, was fehlt/neu ist.
		in := func(l []string) map[string]int {
			m := map[string]int{}
			for _, x := range l {
				m[x]++
			}
			return m
		}
		ma, mb := in(a), in(b)
		for k, n := range mb {
			if d := n - ma[k]; d > 0 {
				c.Added += d
			}
		}
		for k, n := range ma {
			if d := n - mb[k]; d > 0 {
				c.Removed += d
			}
		}
		c.TooBig = true
		return c, c.Added+c.Removed > 0
	}
	n, m := len(a), len(b)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var all []DiffLine
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			all = append(all, DiffLine{" ", a[i]})
			i, j = i+1, j+1
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			// erst die alte Zeile, dann die neue (übliche Reihenfolge)
			all = append(all, DiffLine{"-", a[i]})
			c.Removed++
			i++
		default:
			all = append(all, DiffLine{"+", b[j]})
			c.Added++
			j++
		}
	}
	if c.Added+c.Removed == 0 {
		return c, false
	}
	// Nur Änderungen mit 2 Zeilen Kontext; Lücken als „…“.
	keep := make([]bool, len(all))
	for k, l := range all {
		if l.Op != " " {
			for x := k - 2; x <= k+2; x++ {
				if x >= 0 && x < len(all) {
					keep[x] = true
				}
			}
		}
	}
	gap := false
	for k, l := range all {
		if keep[k] {
			c.Lines = append(c.Lines, l)
			gap = false
		} else if !gap {
			c.Lines = append(c.Lines, DiffLine{"…", ""})
			gap = true
		}
	}
	if len(c.Lines) > 400 {
		c.Lines = append(c.Lines[:400], DiffLine{"…", "(gekürzt)"})
	}
	return c, true
}
