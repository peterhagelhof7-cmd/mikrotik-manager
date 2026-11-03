package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newTestHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestHostKeyChangeAcceptAndForget(t *testing.T) {
	dir := t.TempDir()
	h := &hostKeys{path: filepath.Join(dir, "ssh_known_hosts.json")}
	oldKey, newKey := newTestHostKey(t), newTestHostKey(t)

	// Erster Kontakt: wird gemerkt (TOFU), auch ein zweites Gerät.
	if err := h.callback("AAAA-1111")("", nil, oldKey); err != nil {
		t.Fatalf("erster Kontakt: %v", err)
	}
	other := newTestHostKey(t)
	if err := h.callback("BBBB-2222")("", nil, other); err != nil {
		t.Fatal(err)
	}

	// Geänderter Schlüssel: abgelehnt und vorgemerkt.
	err := h.callback("AAAA-1111")("", nil, newKey)
	if err == nil || !strings.Contains(err.Error(), "geändert") {
		t.Fatalf("geänderter Schlüssel muss abgelehnt werden, err=%v", err)
	}
	fp := ssh.FingerprintSHA256(newKey)
	if got := h.Changed("AAAA-1111"); got != fp {
		t.Fatalf("Changed = %q, will %q", got, fp)
	}
	if h.Changed("BBBB-2222") != "" {
		t.Fatal("anderes Gerät darf nicht als geändert gelten")
	}

	// Falscher Fingerabdruck wird nicht übernommen.
	if err := h.Accept("AAAA-1111", ssh.FingerprintSHA256(oldKey)); err == nil {
		t.Fatal("falscher Fingerabdruck muss scheitern")
	}
	if err := h.Accept("BBBB-2222", fp); err == nil {
		t.Fatal("ohne vorgemerkten Schlüssel muss Accept scheitern")
	}

	// Richtiger Fingerabdruck: übernommen, neuer Schlüssel geht, alter nicht mehr.
	if err := h.Accept("AAAA-1111", fp); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if h.Changed("AAAA-1111") != "" {
		t.Fatal("nach Accept darf nichts mehr vorgemerkt sein")
	}
	if err := h.callback("AAAA-1111")("", nil, newKey); err != nil {
		t.Fatalf("neuer Schlüssel nach Accept: %v", err)
	}
	if err := h.callback("AAAA-1111")("", nil, oldKey); err == nil {
		t.Fatal("alter Schlüssel muss nach Accept abgelehnt werden")
	}
	// Das andere Gerät ist unberührt.
	if err := h.callback("BBBB-2222")("", nil, other); err != nil {
		t.Fatalf("anderes Gerät: %v", err)
	}

	// Forget: Eintrag weg, nächster Kontakt merkt sich wieder (TOFU).
	if err := h.Forget("BBBB-2222"); err != nil {
		t.Fatal(err)
	}
	if err := h.callback("BBBB-2222")("", nil, newTestHostKey(t)); err != nil {
		t.Fatalf("nach Forget: %v", err)
	}
}

func TestHostKeyCorruptFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ssh_known_hosts.json")
	os.WriteFile(p, []byte("{kaputt"), 0o600)
	h := &hostKeys{path: p}
	if err := h.callback("AAAA-1111")("", nil, newTestHostKey(t)); err == nil {
		t.Fatal("beschädigte Datei muss zu einem Fehler führen")
	}
	if b, _ := os.ReadFile(p); string(b) != "{kaputt" {
		t.Fatalf("beschädigte Datei wurde überschrieben: %q", b)
	}
}
