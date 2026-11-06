package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeviceAnswered(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"Login ok", nil, true},
		{"Login abgelehnt", errors.New("from RouterOS device: invalid user name or password (6)"), true},
		{"Port zu", fmt.Errorf("could not connect: %w", errors.New("connect: connection refused")), true},
		{"Zeitüberschreitung", context.DeadlineExceeded, false},
		{"Tunnel weg", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("Tunnel weg – letzter Kontakt vor 1h")}, false},
		{"IP unbekannt", errors.New("Gerät/IP unbekannt"), false},
	} {
		if got := deviceAnswered(c.err); got != c.want {
			t.Errorf("%s: %v, erwartet %v", c.name, got, c.want)
		}
	}
}

func forgetEnv(t *testing.T, ip string) (*Poller, *Store, string) {
	t.Helper()
	dir := t.TempDir()
	v, err := openVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(dir)
	// geschlossener Port auf localhost
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	pl := newPoller(Config{DataDir: dir, APIPort: port}, st, v)
	id, _ := st.Observe(Device{SoftwareID: "ABCD-1234", IP: ip, Identity: "ap"}, "api")
	return pl, st, id
}

func TestForgetBlockedWhileDeviceAnswers(t *testing.T) {
	pl, st, id := forgetEnv(t, "127.0.0.1")
	if err := pl.ForgetOffline(context.Background(), id); !errors.Is(err, errForgetOnline) {
		t.Fatalf("Fehler %v, erwartet errForgetOnline", err)
	}
	if _, ok := st.Get(id); !ok {
		t.Fatal("erreichbares Gerät wurde vergessen")
	}
}

func TestForgetRemovesUnreachableDevice(t *testing.T) {
	pl, st, id := forgetEnv(t, "192.0.2.1") // TEST-NET, antwortet nie
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := pl.ForgetOffline(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get(id); ok {
		t.Fatal("nicht erreichbares Gerät steht noch in der Liste")
	}
}

func TestForgetBlockedWhileJobRuns(t *testing.T) {
	pl, st, id := forgetEnv(t, "192.0.2.1")
	if _, ok := pl.jobs.start(id, "backup"); !ok {
		t.Fatal("Job startet nicht")
	}
	if err := pl.ForgetOffline(context.Background(), id); err == nil {
		t.Fatal("Vergessen trotz laufender Aktion")
	}
	if _, ok := st.Get(id); !ok {
		t.Fatal("Gerät wurde trotz laufender Aktion vergessen")
	}
}

// Im Container gibt es kein /tmp, das Zurücksetzen scheiterte daran.
func TestResetScriptWithoutSystemTemp(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gibt-es-nicht")
	t.Setenv("TMPDIR", gone)
	t.Setenv("TMP", gone)
	t.Setenv("TEMP", gone)
	dir := t.TempDir()
	p, err := writeResetScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != dir {
		t.Fatalf("Skript liegt in %s, erwartet %s", filepath.Dir(p), dir)
	}
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "/ip dhcp-client add interface=bridge") {
		t.Fatalf("Skript falsch: %v", err)
	}
}

func TestResetScriptRemovesOwnFiles(t *testing.T) {
	s := resetScript()
	if strings.Contains(s, "/system identity") {
		t.Fatal("Skript setzt den Namen wieder")
	}
	line := `/file remove [find name~"mm-(undo|reset)"]`
	if !strings.HasSuffix(strings.TrimSpace(s), line) {
		t.Fatalf("Dateien nicht als Letztes entfernt:\n%s", s)
	}
}

func TestResetBlockedForDeviceInCloud(t *testing.T) {
	pl, st, id := forgetEnv(t, "127.0.0.1")
	st.Update(id, func(d *Device) { d.CloudURL = "https://cloud.example" })
	if _, err := pl.ResetDevice(context.Background(), id); err == nil {
		t.Fatal("Zurücksetzen trotz Cloud-Verwaltung")
	}
}
