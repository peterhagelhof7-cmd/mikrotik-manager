package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Rollback: eine auf dem Manager gespeicherte .backup-Datei wieder einspielen.
//
// Ablauf: Prüfung → Sicherheits-Backup des aktuellen Stands („vor-rollback“,
// damit sich der Rollback selbst zurücknehmen lässt) → Datei per SFTP aufs
// Gerät → /system backup load (Neustart) → warten, bis das Gerät wieder per
// API erreichbar ist.
//
// Ein Backup enthält nur die Konfiguration, keine Pakete. Braucht es ein
// anderes WLAN-Paket als installiert (z. B. Backup „vor-wifi“ auf einem Gerät,
// das schon wifi-qcom-ac hat), wird es abgelehnt – das WLAN wäre danach weg.

// RestoreCheck ist das Ergebnis der Prüfung vor einem Rollback.
type RestoreCheck struct {
	Blocked string   `json:"blocked,omitempty"`
	Warn    []string `json:"warn,omitempty"`
}

var (
	rscWireless = regexp.MustCompile(`(?m)^/interface wireless( |$)`)
	rscWifi     = regexp.MustCompile(`(?m)^/interface wifi( |$)`)
)

// restorePkgCheck vergleicht das WLAN-Paket, das der Text-Export des Backups
// verwendet, mit den installierten Paketen; "" = passt.
func restorePkgCheck(rsc string, pk map[string]map[string]string) string {
	hasWireless := pkgInstalled(pk["wireless"])
	hasWifi := pkgInstalled(pk[wifiPkg]) || pkgInstalled(pk["wifi-qcom"])
	switch {
	case rscWireless.MatchString(rsc) && !hasWireless:
		return "Das Backup stammt aus der Zeit mit dem alten wireless-Paket, das Gerät hat jetzt das neue wifi-Paket – nach dem Zurückspielen wäre das WLAN aus. Nicht ausgeführt."
	case rscWifi.MatchString(rsc) && !hasWifi:
		return "Das Backup verwendet das neue wifi-Paket, das auf dem Gerät nicht installiert ist – nach dem Zurückspielen wäre das WLAN aus. Nicht ausgeführt."
	}
	return ""
}

// restoreRsc liest den Text-Export, der zu einer .backup-Datei gehört ("" wenn keiner).
func (p *Poller) restoreRsc(id, name string) string {
	b, err := os.ReadFile(strings.TrimSuffix(p.BackupPath(id, name), ".backup") + ".rsc")
	if err != nil {
		return ""
	}
	return string(b)
}

// CheckRestore prüft, ob sich das Backup name auf das Gerät zurückspielen lässt.
func (p *Poller) CheckRestore(ctx context.Context, id, name string) RestoreCheck {
	var c RestoreCheck
	path := p.BackupPath(id, name)
	if path == "" || !strings.HasSuffix(name, ".backup") {
		c.Blocked = "Nur .backup-Dateien lassen sich zurückspielen."
		return c
	}
	if _, err := os.Stat(path); err != nil {
		c.Blocked = "Backup nicht gefunden."
		return c
	}
	if j, ok := p.jobs.Get(id); ok && j.Running {
		c.Blocked = "Auf dem Gerät läuft bereits eine Aktion."
		return c
	}
	rsc := p.restoreRsc(id, name)
	if rsc == "" {
		c.Warn = append(c.Warn, "Zu diesem Backup gibt es keinen Text-Export – das WLAN-Paket kann vorab nicht geprüft werden.")
	}
	cl, err := p.session(ctx, id)
	if err != nil {
		c.Blocked = "Gerät nicht erreichbar: " + err.Error()
		return c
	}
	defer cl.Close()
	pk, err := readPackagesT(ctx, cl)
	if err != nil {
		c.Blocked = "Pakete lesen: " + err.Error()
		return c
	}
	if rsc != "" {
		if b := restorePkgCheck(rsc, pk); b != "" {
			c.Blocked = b
			return c
		}
	}
	cred, _ := p.vault.For(id)
	c.Warn = append(c.Warn,
		"Die gesamte Konfiguration (auch IP-Adressen, Benutzer, Passwörter, WLAN) wird auf den Stand des Backups gesetzt. Änderungen danach gehen verloren.",
		fmt.Sprintf("Der Manager meldet sich danach als %s an. Gab es diesen Benutzer zur Zeit des Backups nicht oder mit anderem Passwort, kommt er nicht mehr aufs Gerät – dann die Zugangsdaten im Manager anpassen.", cred.User),
		"Vorher wird der aktuelle Stand als Backup „vor-rollback“ gespeichert – damit lässt sich der Rollback zurücknehmen.",
		"Das Gerät startet neu (ca. 1–2 Minuten offline).")
	return c
}

// StartRestore startet den Rollback auf das Backup name im Hintergrund.
func (p *Poller) StartRestore(id, name string) []Result {
	return p.startJobs(p.ctx, []string{id}, "restore", func(ctx context.Context, id string, x *Job) error {
		return p.doRestore(ctx, id, name, x)
	})
}

func (p *Poller) doRestore(ctx context.Context, id, name string, x *Job) error {
	d, _ := p.st.Get(id)
	dev := d.Identity
	path := p.BackupPath(id, name)
	if path == "" || !strings.HasSuffix(name, ".backup") {
		return fmt.Errorf("ungültiges Backup %q", name)
	}
	// Kopie ziehen: das Sicherheits-Backup kann ältere Dateien wegräumen (20 je Gerät).
	tmp, err := copyToTemp(path)
	if err != nil {
		return fmt.Errorf("Backup lesen: %w", err)
	}
	defer os.Remove(tmp)

	cl, err := p.session(ctx, id)
	if err != nil {
		return err
	}
	pk, err := readPackagesT(ctx, cl)
	if err == nil {
		if rsc := p.restoreRsc(id, name); rsc != "" {
			if b := restorePkgCheck(rsc, pk); b != "" {
				err = fmt.Errorf("%s", b)
			}
		}
	}
	if err != nil {
		cl.Close()
		return err
	}

	p.jobs.step(x, dev, "sichere den aktuellen Stand auf den Manager …")
	desc, err := p.BackupToManager(ctx, id, cl, "vor-rollback")
	if err != nil {
		cl.Close()
		return fmt.Errorf("Sicherheits-Backup: %w – nichts geändert", err)
	}
	p.jobs.step(x, dev, "aktueller Stand gespeichert: %s", desc)

	if err := p.loadBackup(ctx, id, cl, x, dev, tmp, name); err != nil {
		return fmt.Errorf("%w – der Stand vor dem Rollback liegt als …-vor-rollback.backup auf dem Manager", err)
	}
	p.jobs.step(x, dev, "zurückgespielt: %s – Gerät wieder erreichbar", name)
	p.Trigger(id)
	return nil
}

// copyToTemp kopiert eine Backup-Datei in eine temporäre Datei daneben (0600).
// Der Name passt nicht zu backupNameRe, taucht also in keiner Liste auf.
func copyToTemp(path string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(path), ".rollback-*.backup")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), out.Close()
}
