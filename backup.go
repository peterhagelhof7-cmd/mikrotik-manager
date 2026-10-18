package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-routeros/routeros/v3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Backups liegen auf dem Manager unter DATA_DIR/backups/<Geräte-ID>/:
//
//	<zeit>-<anlass>.backup  Binär-Backup (/system backup, unverschlüsselt)
//	<zeit>-<anlass>.rsc     Text-Export (/export show-sensitive), lesbar/diffbar
//
// Beide enthalten Geheimnisse (WLAN-Schlüssel, Benutzer) → Dateien 0600.
const keepBackups = 20 // je Gerät, ältere werden gelöscht

var backupNameRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[a-z-]+\.(backup|rsc)$`)

// BackupFile ist eine auf dem Manager gespeicherte Sicherung.
type BackupFile struct {
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
}

func (p *Poller) backupDir(id string) string {
	// Software-IDs sind z. B. "ABCD-1234"; trotzdem nichts Pfadartiges zulassen.
	return filepath.Join(p.cfg.DataDir, "backups", strings.NewReplacer("/", "_", "\\", "_", "..", "_", ":", "_").Replace(id))
}

// ListBackups liefert die Sicherungen eines Geräts, neueste zuerst.
func (p *Poller) ListBackups(id string) []BackupFile {
	ents, _ := os.ReadDir(p.backupDir(id))
	var out []BackupFile
	for _, e := range ents {
		if !backupNameRe.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupFile{Name: e.Name(), Size: fi.Size(), Time: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// BackupPath liefert den Pfad einer Sicherung oder "" bei ungültigem Namen.
func (p *Poller) BackupPath(id, name string) string {
	if !backupNameRe.MatchString(name) {
		return ""
	}
	return filepath.Join(p.backupDir(id), name)
}

// --- SSH mit Trust-on-first-use: Der Hostkey jedes Geräts wird beim ersten
// Kontakt gespeichert; ändert er sich später, wird die Verbindung verweigert.

type hostKeys struct {
	mu   sync.Mutex
	path string
}

func (h *hostKeys) callback(id string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		m := map[string]string{}
		if b, err := os.ReadFile(h.path); err == nil {
			json.Unmarshal(b, &m)
		}
		got := key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
		if want, ok := m[id]; ok {
			if want != got {
				return fmt.Errorf("SSH-Hostkey von %s hat sich geändert (%s) – Verbindung verweigert; nach Neuinstallation Eintrag in ssh_known_hosts.json löschen", id, ssh.FingerprintSHA256(key))
			}
			return nil
		}
		m[id] = got
		b, _ := json.MarshalIndent(m, "", "  ")
		return writeFileAtomic(h.path, b, 0o600)
	}
}

func (p *Poller) sshDial(ctx context.Context, id string) (*ssh.Client, error) {
	d, ok := p.st.Get(id)
	if !ok || d.IP == "" {
		return nil, fmt.Errorf("Gerät/IP unbekannt")
	}
	c, _ := p.vault.For(id)
	cfg := &ssh.ClientConfig{
		User: c.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(c.Password),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				a := make([]string, len(qs))
				for i := range a {
					a[i] = c.Password
				}
				return a, nil
			}),
		},
		HostKeyCallback: p.hostKeys.callback(id),
		Timeout:         10 * time.Second,
	}
	var nd net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := nd.DialContext(dctx, "tcp", net.JoinHostPort(d.IP, "22"))
	if err != nil {
		return nil, fmt.Errorf("SSH (Port 22): %w", err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, conn.RemoteAddr().String(), cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SSH-Anmeldung: %w", err)
	}
	return ssh.NewClient(sc, chans, reqs), nil
}

// BackupToManager erzeugt auf dem Gerät ein Backup, lädt es per SFTP auf den
// Manager, prüft die Größe und löscht dann die Kopie auf dem Gerät (der Flash
// kleiner Geräte ist knapp). Zusätzlich wird der Text-Export gespeichert.
// Liefert die Beschreibung der gespeicherten Dateien.
func (p *Poller) BackupToManager(ctx context.Context, id string, cl *routeros.Client, reason string) (string, error) {
	desc, _, err := p.backupToManager(ctx, id, cl, reason)
	return desc, err
}

// backupToManager liefert zusätzlich den Pfad der .backup-Datei auf dem Manager
// (für ein späteres Zurückspielen).
func (p *Poller) backupToManager(ctx context.Context, id string, cl *routeros.Client, reason string) (string, string, error) {
	stamp := time.Now().Format("20060102-150405")
	remote, err := saveBackup(ctx, cl, "mm-"+stamp)
	if err != nil {
		return "", "", err
	}
	// Gerätekopie in jedem Fall wieder entfernen – auch wenn der Download scheitert.
	defer removeDeviceFile(ctx, cl, remote)

	sc, err := p.sshDial(ctx, id)
	if err != nil {
		return "", "", err
	}
	defer sc.Close()

	dir := p.backupDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	base := filepath.Join(dir, stamp+"-"+reason)

	n, err := sftpDownload(sc, remote, base+".backup")
	if err != nil {
		os.Remove(base + ".backup")
		return "", "", fmt.Errorf("Download %s: %w", remote, err)
	}

	desc := fmt.Sprintf("%s.backup (%d Byte)", filepath.Base(base), n)
	d, _ := p.st.Get(id)
	if exp, err := sshExport(sc, d.Major()); err != nil {
		desc += ", Text-Export fehlgeschlagen: " + err.Error()
	} else if err := os.WriteFile(base+".rsc", exp, 0o600); err == nil {
		desc += fmt.Sprintf(" + .rsc (%d Byte)", len(exp))
	}
	p.pruneBackups(id)
	return desc, base + ".backup", nil
}

func sftpDownload(sc *ssh.Client, remote, local string) (int64, error) {
	fc, err := sftp.NewClient(sc)
	if err != nil {
		return 0, fmt.Errorf("SFTP: %w", err)
	}
	defer fc.Close()
	st, err := fc.Stat(remote)
	if err != nil {
		return 0, err
	}
	src, err := fc.Open(remote)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	dst, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if n != st.Size() || n == 0 {
		return n, fmt.Errorf("Größe %d statt %d Byte", n, st.Size())
	}
	return n, nil
}

// sftpUpload legt eine Datei vom Manager auf das Gerät (z. B. ein Backup zum
// Zurückspielen) und prüft die Größe.
func sftpUpload(sc *ssh.Client, local, remote string) error {
	b, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	fc, err := sftp.NewClient(sc)
	if err != nil {
		return fmt.Errorf("SFTP: %w", err)
	}
	defer fc.Close()
	f, err := fc.Create(remote)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	st, err := fc.Stat(remote)
	if err != nil {
		return err
	}
	if st.Size() != int64(len(b)) {
		return fmt.Errorf("Größe %d statt %d Byte", st.Size(), len(b))
	}
	return nil
}

// sshExport holt die Konfiguration als Text. v7 blendet Geheimnisse sonst aus,
// v6 kennt "show-sensitive" nicht (zeigt sie dort standardmäßig).
func sshExport(sc *ssh.Client, major string) ([]byte, error) {
	s, err := sc.NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	cmd := "/export show-sensitive"
	if major == "6" {
		cmd = "/export"
	}
	var out, errb bytes.Buffer
	s.Stdout, s.Stderr = &out, &errb
	done := make(chan error, 1)
	go func() { done <- s.Run(cmd) }()
	select {
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("%v %s", err, errb.String())
		}
	case <-time.After(90 * time.Second):
		return nil, fmt.Errorf("Zeitüberschreitung")
	}
	b := bytes.ReplaceAll(out.Bytes(), []byte("\r\n"), []byte("\n"))
	if !bytes.Contains(b, []byte("/")) {
		return nil, fmt.Errorf("leere Ausgabe")
	}
	return b, nil
}

func removeDeviceFile(ctx context.Context, cl *routeros.Client, name string) {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cl.RunContext(rctx, "/file/remove", "=numbers="+name)
}

func (p *Poller) pruneBackups(id string) {
	list := p.ListBackups(id)
	stamps := map[string]bool{}
	for _, b := range list {
		stamp := b.Name[:15] // JJJJMMTT-hhmmss
		if !stamps[stamp] && len(stamps) >= keepBackups {
			os.Remove(filepath.Join(p.backupDir(id), b.Name))
			continue
		}
		stamps[stamp] = true
	}
}

// backupJob liefert den Job „Backup“; reason landet im Dateinamen.
func (p *Poller) backupJob(reason string) func(context.Context, string, *Job) error {
	return func(ctx context.Context, id string, x *Job) error {
		d, _ := p.st.Get(id)
		cl, err := p.session(ctx, id)
		if err != nil {
			return err
		}
		defer cl.Close()
		p.jobs.step(x, d.Identity, "erstelle Backup und lade es auf den Manager …")
		desc, err := p.BackupToManager(ctx, id, cl, reason)
		if err != nil {
			return err
		}
		p.jobs.step(x, d.Identity, "gespeichert auf dem Manager: %s", desc)
		return nil
	}
}

func (p *Poller) StartBackup(ids []string) []Result {
	return p.startJobs(p.ctx, ids, "backup", p.backupJob("manuell"))
}

// nextBackupAt liefert den nächsten Termin für "hh:mm" (Ortszeit) nach now.
func nextBackupAt(now time.Time, hhmm string) (time.Time, bool) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return time.Time{}, false
	}
	n := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !n.After(now) {
		n = n.AddDate(0, 0, 1) // AddDate statt +24h: stimmt auch bei Zeitumstellung
	}
	return n, true
}

// runNightlyBackup sichert täglich zur eingestellten Zeit alle Geräte mit
// funktionierender API nacheinander auf den Manager.
func (p *Poller) runNightlyBackup(ctx context.Context) {
	if _, ok := nextBackupAt(time.Now(), p.cfg.BackupTime); !ok {
		log.Printf("nächtliches Backup aus (BACKUP_TIME=%q)", p.cfg.BackupTime)
		return
	}
	for {
		next, _ := nextBackupAt(time.Now(), p.cfg.BackupTime)
		p.mu.Lock()
		p.nextBackup = next
		p.mu.Unlock()
		log.Printf("nächtliches Backup: nächster Lauf %s", next.Format("02.01.2006 15:04"))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		var ids []string
		for _, d := range p.st.List() {
			if d.APIStatus == "ok" {
				ids = append(ids, d.ID)
			}
		}
		log.Printf("nächtliches Backup startet für %d Geräte", len(ids))
		p.startJobs(ctx, ids, "backup", p.backupJob("nachts"))
	}
}

func (p *Poller) NextBackup() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nextBackup
}
