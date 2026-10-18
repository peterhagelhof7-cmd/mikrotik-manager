package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Selbst-Update über GitHub-Releases.
//
//   - Prüfen: alle 6 Stunden (und auf Knopfdruck) das neueste Release abfragen.
//   - systemd-Installation (Programm in einem für den Dienst beschreibbaren
//     Verzeichnis, siehe deploy/install.sh): passendes Programm laden, gegen
//     SHA256SUMS des Releases prüfen, Probestart mit "version", altes Programm als
//     .old behalten, neues an seine Stelle und per exec neu starten.
//   - Docker: ein Container ersetzt sich nicht selbst – die Oberfläche zeigt das
//     neue Release und die Befehle zum Aktualisieren des Images.
//   - Läuft ein Job (Update, Ausrollen …), wird nicht aktualisiert.

const defaultUpdateRepo = "peterhagelhof7-cmd/mikrotik-manager"

const updateCheckEvery = 6 * time.Hour

// UpdateInfo ist der Stand für die Weboberfläche.
type UpdateInfo struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`
	URL       string    `json:"url,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	Mode      string    `json:"mode"` // binary | docker | manual | off
	Hint      string    `json:"hint,omitempty"`
	Auto      bool      `json:"auto"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
	Error     string    `json:"error,omitempty"`
	Busy      bool      `json:"busy"`
	Status    string    `json:"status,omitempty"`
}

type selfUpdater struct {
	mu     sync.Mutex
	info   UpdateInfo
	assets map[string]string // Dateiname → Download-URL des neuesten Releases
	repo   string
	api    string // https://api.github.com (Tests: eigener Server)
	jobs   *Jobs
	save   func() // vor dem Neustart (Geräteliste sichern)
	client *http.Client
}

func newSelfUpdater(cfg Config, jobs *Jobs, save func()) *selfUpdater {
	u := &selfUpdater{repo: cfg.UpdateRepo, api: "https://api.github.com", jobs: jobs, save: save, client: &http.Client{Timeout: 2 * time.Minute}}
	u.info = UpdateInfo{Current: strings.TrimPrefix(version, "v"), Auto: cfg.AutoUpdate}
	u.info.Mode, u.info.Hint = updateMode(cfg.UpdateRepo)
	return u
}

// updateMode: kann sich dieses Programm selbst ersetzen?
func updateMode(repo string) (string, string) {
	if repo == "" || repo == "off" {
		return "off", "Update-Prüfung ist abgeschaltet (UPDATE_REPO=off)."
	}
	if os.Getenv("MM_DOCKER") == "1" {
		return "docker", "Im Docker-Container: Image aktualisieren mit „docker compose pull && docker compose up -d“ (Synology: im Container Manager Image neu laden und Container neu erstellen)."
	}
	exe, err := selfPath()
	if err != nil {
		return "manual", "Programmpfad unbekannt: " + err.Error()
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".mm-write-test-*")
	if err != nil {
		return "manual", fmt.Sprintf("Das Programmverzeichnis %s ist für den Dienst nicht beschreibbar – einmal mit deploy/install.sh neu installieren, danach geht das Selbst-Update.", filepath.Dir(exe))
	}
	f.Close()
	os.Remove(f.Name())
	return "binary", ""
}

func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// assetName ist der Dateiname des Programms für diese Plattform im Release.
func assetName() string {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	return "mikrotik-manager-linux-" + arch
}

// parseVer: "v1.2.3" / "1.2.3-dev" → [1 2 3], Vorabversion?
func parseVer(v string) ([3]int, bool, bool) {
	var n [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return n, false, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil {
			return n, false, false
		}
		n[i] = x
	}
	return n, pre != "", true
}

// newerVersion: ist latest neuer als current? Unlesbare current-Versionen
// (Entwicklungsstand „dev“, „main-<commit>“) gelten als älter.
func newerVersion(latest, current string) bool {
	l, lpre, ok := parseVer(latest)
	if !ok || lpre {
		return false
	}
	c, cpre, ok := parseVer(current)
	if !ok {
		return true
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return cpre // gleiche Nummer: Release ist neuer als dessen Vorabversion
}

func (u *selfUpdater) Info() UpdateInfo {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.info
}

func (u *selfUpdater) get(ctx context.Context, url string, limit int64, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mikrotik-manager/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("%s: größer als %d Byte", url, limit)
	}
	return err
}

// Check fragt das neueste Release ab.
func (u *selfUpdater) Check(ctx context.Context) {
	u.mu.Lock()
	mode := u.info.Mode
	u.mu.Unlock()
	if mode == "off" {
		return
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Body   string `json:"body"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	var buf strings.Builder
	err := u.get(ctx, u.api+"/repos/"+u.repo+"/releases/latest", 1<<20, &buf)
	if err == nil {
		err = json.Unmarshal([]byte(buf.String()), &rel)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.info.CheckedAt = time.Now()
	if err != nil {
		u.info.Error = "Update-Prüfung: " + err.Error()
		return
	}
	u.info.Error = ""
	u.info.Latest = strings.TrimPrefix(rel.Tag, "v")
	u.info.URL = rel.URL
	notes := rel.Body
	if len(notes) > 4000 {
		notes = notes[:4000] + " …"
	}
	u.info.Notes = notes
	u.info.Available = newerVersion(rel.Tag, version)
	u.assets = map[string]string{}
	for _, a := range rel.Assets {
		u.assets[a.Name] = a.URL
	}
}

// Start installiert das neueste Release im Hintergrund.
func (u *selfUpdater) Start() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.info.Mode != "binary":
		return fmt.Errorf("Selbst-Update hier nicht möglich: %s", u.info.Hint)
	case !u.info.Available:
		return fmt.Errorf("kein neueres Release vorhanden")
	case u.info.Busy:
		return fmt.Errorf("Update läuft bereits")
	case u.jobs.AnyRunning():
		return fmt.Errorf("auf einem Gerät läuft gerade eine Aktion – danach erneut versuchen")
	}
	u.info.Busy, u.info.Status, u.info.Error = true, "lade "+assetName()+" …", ""
	latest, assets := u.info.Latest, u.assets
	go func() {
		err := u.apply(latest, assets)
		u.mu.Lock()
		u.info.Busy = false
		if err != nil {
			u.info.Error, u.info.Status = "Selbst-Update: "+err.Error(), ""
			log.Printf("Selbst-Update auf %s fehlgeschlagen: %v", latest, err)
		}
		u.mu.Unlock()
	}()
	return nil
}

func (u *selfUpdater) status(format string, a ...any) {
	u.mu.Lock()
	u.info.Status = fmt.Sprintf(format, a...)
	u.mu.Unlock()
	log.Printf("Selbst-Update: %s", fmt.Sprintf(format, a...))
}

func (u *selfUpdater) apply(latest string, assets map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := assetName()
	binURL, sumURL := assets[name], assets["SHA256SUMS"]
	if binURL == "" || sumURL == "" {
		return fmt.Errorf("Release %s enthält %s oder SHA256SUMS nicht", latest, name)
	}
	var sums strings.Builder
	if err := u.get(ctx, sumURL, 1<<16, &sums); err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(sums.String()))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return fmt.Errorf("keine Prüfsumme für %s in SHA256SUMS", name)
	}

	exe, err := selfPath()
	if err != nil {
		return err
	}
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	err = u.get(ctx, binURL, 128<<20, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return fmt.Errorf("Prüfsumme stimmt nicht (%s statt %s)", got[:12], want[:12])
	}

	// Probestart: läuft das neue Programm hier überhaupt, und ist es die erwartete Version?
	u.status("Prüfsumme ok, Probestart …")
	pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
	out, err := exec.CommandContext(pctx, tmp, "version").Output()
	pcancel()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("Probestart des neuen Programms: %v", err)
	}
	if got := strings.TrimPrefix(strings.TrimSpace(string(out)), "v"); got != latest {
		os.Remove(tmp)
		return fmt.Errorf("neues Programm meldet Version %q statt %q", got, latest)
	}

	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(old, exe)
		return err
	}
	u.status("Version %s installiert – starte neu …", latest)
	if u.save != nil {
		u.save()
	}
	time.Sleep(time.Second) // Statusabfrage der Oberfläche noch beantworten
	if err := execSelf(exe); err != nil {
		os.Rename(exe, tmp)
		os.Rename(old, exe)
		return fmt.Errorf("Neustart: %w (alte Version bleibt)", err)
	}
	return nil // nicht erreicht: exec ersetzt den Prozess
}

// run prüft regelmäßig; mit AUTO_UPDATE wird ein neues Release gleich installiert.
func (u *selfUpdater) run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	t := time.NewTicker(updateCheckEvery)
	defer t.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		u.Check(cctx)
		cancel()
		i := u.Info()
		if i.Available {
			log.Printf("neue Version %s verfügbar (läuft: %s)", i.Latest, i.Current)
			if i.Auto && i.Mode == "binary" {
				if err := u.Start(); err != nil {
					log.Printf("automatisches Update verschoben: %v", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
