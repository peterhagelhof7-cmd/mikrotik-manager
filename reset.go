package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"
	"time"
)

// Zurücksetzen = Gerät aus der Verwaltung nehmen: vorher Backup auf den Manager,
// dann /system reset-configuration ohne Standardkonfiguration mit eigenem Skript
// danach – alle Ports in einer Bridge, DHCP-Client, admin ohne Passwort, kein
// WireGuard, kein Verwaltungsbenutzer. Der Manager vergisst das Gerät; seine
// Backups bleiben auf dem Manager.

const resetFile = "mm-reset.rsc"

// resetScript läuft nach dem Zurücksetzen auf dem Gerät (ohne Standard-
// konfiguration gibt es keine virtuellen WLANs). Pakete, die es nicht gibt
// (wireless/wifi), dürfen das Skript nicht abbrechen → :parse. Zuletzt gehen
// die eigenen Dateien: Liegengebliebene mm-undo-Backups sind unverschlüsselt.
func resetScript() string {
	return `# zurückgesetzt vom mikrotik-manager: alle Ports in einer Bridge, DHCP-Client, admin ohne Passwort
:delay 15s
/interface bridge add name=bridge
:foreach i in=[/interface ethernet find] do={ /interface bridge port add bridge=bridge interface=$i }
:foreach m in={"wireless";"wifi"} do={
  :do { :local f [:parse (":foreach i in=[/interface " . $m . " find] do={ /interface bridge port add bridge=bridge interface=\$i }")]; $f } on-error={}
}
/ip dhcp-client add interface=bridge disabled=no
/user set [find name=admin] password=""
/file remove [find name~"mm-(undo|reset)"]
`
}

// writeResetScript legt das Skript im Datenverzeichnis ab: Das Container-Image
// basiert auf scratch und hat kein /tmp.
func writeResetScript(dir string) (string, error) {
	f, err := os.CreateTemp(dir, ".mm-reset-*.rsc")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(resetScript()); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// ResetDevice setzt das Gerät zurück und entfernt es aus dem Manager. Läuft
// synchron (Backup + Hochladen + Befehl, typisch unter einer Minute).
func (p *Poller) ResetDevice(ctx context.Context, id string) (string, error) {
	d, ok := p.st.Get(id)
	if !ok {
		return "", fmt.Errorf("Gerät nicht gefunden")
	}
	if msg := cloudBlock(d); msg != "" {
		return "", errors.New(msg)
	}
	if j, ok := p.jobs.Get(id); ok && j.Running {
		return "", fmt.Errorf("auf dem Gerät läuft bereits eine Aktion")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cl, err := p.session(ctx, id)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	desc, err := p.BackupToManager(ctx, id, cl, "vor-reset")
	if err != nil {
		return "", fmt.Errorf("Backup vor dem Zurücksetzen: %w – nichts geändert", err)
	}
	log.Printf("%s: Backup vor dem Zurücksetzen: %s", d.Identity, desc)

	// Auf Geräten mit flash/ liegt alles andere im RAM und wäre nach dem Neustart weg.
	remote := resetFile
	rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
	if r, err := cl.RunContext(rctx, "/file/print", "?name=flash", "=.proplist=name"); err == nil && len(r.Re) > 0 {
		remote = "flash/" + remote
	}
	rcancel()
	script, err := writeResetScript(p.cfg.DataDir)
	if err != nil {
		return "", fmt.Errorf("Skript schreiben: %w – nichts geändert", err)
	}
	defer os.Remove(script)
	sc, err := p.sshDial(ctx, id)
	if err != nil {
		return "", err
	}
	err = sftpUpload(sc, script, remote)
	sc.Close()
	if err != nil {
		return "", fmt.Errorf("Skript hochladen: %w – nichts geändert", err)
	}
	if err := runIgnoringDisconnect(ctx, cl, 30*time.Second, "/system/reset-configuration",
		"=no-defaults=yes", "=skip-backup=yes", "=run-after-reset="+remote); err != nil {
		return "", fmt.Errorf("Zurücksetzen: %w", err)
	}
	log.Printf("%s (%s): zurückgesetzt und aus der Verwaltung entfernt", d.Identity, d.IP)
	p.forgetDevice(id)
	return "Backup " + desc + " gespeichert, Gerät wird zurückgesetzt (Neustart) und ist aus der Verwaltung entfernt", nil
}

// errForgetOnline: Ein erreichbares Gerät wird zurückgesetzt, nicht vergessen.
var errForgetOnline = errors.New("Gerät ist online – zum Entfernen „Zurücksetzen“ verwenden. Vergessen geht nur bei Geräten, die nicht erreichbar sind")

// deviceAnswered sagt, ob ein Verbindungsversuch das Gerät erreicht hat. Auch
// abgelehnter Login und geschlossener Port heißen: Das Gerät läuft.
func deviceAnswered(err error) bool {
	if err == nil {
		return true
	}
	status, _ := classifyErr(err, 0)
	return status == "auth" || isRefused(err)
}

// isRefused: Das Gerät hat die Verbindung abgewiesen (10061 = Windows).
func isRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.Errno(10061)) ||
		strings.Contains(err.Error(), "connection refused")
}

// deviceOnline prüft jetzt, statt dem letzten Abfrage-Stand zu glauben.
func (p *Poller) deviceOnline(ctx context.Context, d Device) bool {
	cl, err := p.session(ctx, d.ID)
	if err == nil {
		cl.Close()
	}
	return deviceAnswered(err)
}

// ForgetOffline vergisst ein Gerät, das nicht erreichbar ist. Am Gerät bleibt
// dabei alles stehen, darum ist für erreichbare Geräte ResetDevice der Weg.
func (p *Poller) ForgetOffline(ctx context.Context, id string) error {
	d, ok := p.st.Get(id)
	if !ok {
		return nil
	}
	if j, ok := p.jobs.Get(id); ok && j.Running {
		return fmt.Errorf("auf dem Gerät läuft bereits eine Aktion")
	}
	if p.deviceOnline(ctx, d) {
		return errForgetOnline
	}
	log.Printf("%s (%s): vergessen, war nicht erreichbar", d.Identity, d.IP)
	p.forgetDevice(id)
	return nil
}

// forgetDevice entfernt ein Gerät aus dem Manager. Backups bleiben liegen.
func (p *Poller) forgetDevice(id string) {
	p.st.Delete(id)
	p.vault.Delete("dev:" + id)
	p.hostKeys.Forget(id)
	p.hist.Forget(id)
	p.forgetWwan(id)
	p.st.save()
}
