package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testTok = "abcdefghijklmnopqrstuvwxyz012345"

func TestParseCloudCode(t *testing.T) {
	want := cloudCode{Base: "https://mm.example.com", Token: testTok}
	for _, in := range []string{
		`/tool fetch url="https://mm.example.com/enroll/` + testTok + `.rsc" dst-path=mm-enroll.rsc check-certificate=yes-without-crl; /import mm-enroll.rsc`,
		`/tool fetch url="https://mm.example.com/enroll/` + testTok + `-nocheck.rsc" dst-path=x check-certificate=no`,
		"  https://mm.example.com/enroll/" + testTok + ".rsc\n",
		"https://mm.example.com/enroll/" + testTok,
	} {
		if got, err := parseCloudCode(in); err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	if got, err := parseCloudCode("https://x.example/pfad/enroll/" + testTok); err != nil || got.Base != "https://x.example/pfad" {
		t.Errorf("Pfad: %+v %v", got, err)
	}
	for _, bad := range []string{"", testTok, "http://mm.example.com/enroll/" + testTok, "https://mm.example.com/enroll/kurz"} {
		if _, err := parseCloudCode(bad); err == nil {
			t.Errorf("%q angenommen", bad)
		}
	}
}

// cloudServer antwortet wie enroll.go der Cloud; used = Code schon verbraucht.
func cloudServer(t *testing.T, used *bool) cloudCode {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if *used {
			rw.WriteHeader(http.StatusForbidden)
			io.WriteString(rw, ":error \"mikrotik-manager: Token wurde schon benutzt\"\n")
			return
		}
		if r.Method == http.MethodGet {
			io.WriteString(rw, "# mikrotik-manager: Anmeldung\n/interface wireguard add comment=\"mikrotik-manager-cloud\"\n")
			return
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "PUB\nAP Keller" || r.Header.Get("Content-Type") != "text/plain" {
			t.Errorf("Anfrage: %q", b)
		}
		io.WriteString(rw, "# mikrotik-manager: Abschluss\n/user add name=\"mikrotik-manager-cloud\"\n")
	}))
	old := cloudHTTP
	cloudHTTP = srv.Client()
	t.Cleanup(func() { cloudHTTP = old; srv.Close() })
	return cloudCode{Base: srv.URL, Token: testTok}
}

func TestCloudRegister(t *testing.T) {
	used := false
	c := cloudServer(t, &used)
	ctx := context.Background()
	if err := c.Check(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := c.register(ctx, "PUB", "AP Keller")
	if err != nil || !strings.HasPrefix(s, cloudFinishHead) {
		t.Fatal(s, err)
	}
	used = true
	for _, err := range []error{c.Check(ctx), func() error { _, e := c.register(ctx, "PUB", "AP Keller"); return e }()} {
		if !errors.Is(err, errCloudCode) || !strings.Contains(err.Error(), "schon benutzt") {
			t.Fatal(err)
		}
	}
}

func TestCloudForeignServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { io.WriteString(rw, "<html>Hallo</html>") }))
	defer srv.Close()
	old := cloudHTTP
	cloudHTTP = srv.Client()
	defer func() { cloudHTTP = old }()
	c := cloudCode{Base: srv.URL, Token: testTok}
	if err := c.Check(context.Background()); err == nil {
		t.Fatal("fremder Server angenommen")
	}
	// Eine fremde Antwort darf nie als Skript auf einem Gerät landen.
	if s, err := c.register(context.Background(), "PUB", "x"); err == nil || s != "" {
		t.Fatal(s, err)
	}
}

func TestCloudErrorHidesCode(t *testing.T) {
	c := cloudCode{Base: "https://127.0.0.1:1", Token: testTok}
	err := c.Check(context.Background())
	if err == nil || strings.Contains(err.Error(), testTok) {
		t.Fatal(err)
	}
	if _, err := c.register(context.Background(), "PUB", "x"); err == nil || strings.Contains(err.Error(), testTok) {
		t.Fatal(err)
	}
}

func TestCloudOldRefused(t *testing.T) {
	// Eine Cloud ohne eigenen Benutzer überschriebe das Passwort dieses Managers.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(rw, "# mikrotik-manager: Anmeldung\n/interface wireguard add comment=\"mikrotik-manager\"\n")
			return
		}
		io.WriteString(rw, "# mikrotik-manager: Abschluss\n/user set [find name=\"mikrotik-manager\"] password=\"x\"\n")
	}))
	defer srv.Close()
	old := cloudHTTP
	cloudHTTP = srv.Client()
	defer func() { cloudHTTP = old }()
	c := cloudCode{Base: srv.URL, Token: testTok}
	if err := c.Check(context.Background()); !errors.Is(err, errCloudOld) {
		t.Fatal(err)
	}
	if s, err := c.register(context.Background(), "PUB", "x"); !errors.Is(err, errCloudOld) || s != "" {
		t.Fatal(s, err)
	}
}

func TestCloudUndo(t *testing.T) {
	s := cloudUndo(false, true)
	for _, w := range []string{"/ip firewall filter remove [find in-interface=wg-mm]", "peers remove [find interface=wg-mm]",
		"/interface wireguard remove [find name=wg-mm]", `/user remove [find name="mikrotik-manager-cloud"]`} {
		if !strings.Contains(s, w) {
			t.Errorf("fehlt %s in\n%s", w, s)
		}
	}
	// Der Benutzer dieses Managers bleibt immer unangetastet.
	s = cloudUndo(true, false)
	if strings.Contains(s, "/user") || strings.Contains(s, "/interface wireguard remove") {
		t.Error(s)
	}
}

func TestCloudBlock(t *testing.T) {
	ok := Device{ID: "a", APIStatus: "ok", Version: "7.25.1"}
	if why := cloudBlock(ok); why != "" {
		t.Fatal(why)
	}
	v6 := ok
	v6.Version = "6.49.10"
	moved := ok
	moved.CloudURL = "https://x"
	off := ok
	off.APIStatus = "auth"
	for _, d := range []Device{v6, moved, off} {
		if cloudBlock(d) == "" {
			t.Errorf("%+v", d)
		}
	}
}

func TestMovedDeviceReadOnly(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), APIPort: 1}
	st := newStore(cfg.DataDir)
	v, err := openVault(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := st.Observe(Device{SoftwareID: "A", IP: "127.0.0.1", Identity: "AP"}, "api")
	st.Update(id, func(d *Device) { d.CloudURL, d.APIStatus = "https://x", "ok" })
	p := newPoller(cfg, st, v)
	want := errMoved.Error()
	for name, res := range map[string][]Result{
		"Update":   p.StartUpdate([]string{id}),
		"Neustart": p.StartReboot([]string{id}),
		"Änderung": p.StartChange([]string{id}, ChangeReq{Action: "snmp", SNMPOn: true}),
		"Benutzer": p.EnsureManagerUser(context.Background(), []string{id}),
		"Passwort": p.SetUserPassword(context.Background(), []string{id}, "admin", "geheimes-passwort"),
		"Name":     {p.SetIdentity(context.Background(), id, "neu")},
		"Rollback": p.StartRestore(id, "x.backup"),
	} {
		if len(res) != 1 || res[0].OK || res[0].Msg != want {
			t.Errorf("%s: %+v", name, res)
		}
	}
	// Lesen und Backups laufen weiter.
	if _, err := p.session(context.Background(), id); err != nil && strings.Contains(err.Error(), "Cloud") {
		t.Fatal(err)
	}
	if res := p.StartBackup([]string{id}); len(res) != 1 || !res[0].OK {
		t.Fatalf("Backup: %+v", res)
	}
	p.pollOne(context.Background(), id)
	if d, _ := st.Get(id); d.LastPoll.IsZero() {
		t.Fatal("wird nicht mehr abgefragt")
	}
	if r := p.CloudRelease(id); !r.OK {
		t.Fatalf("%+v", r)
	}
	if d, _ := st.Get(id); d.Moved() {
		t.Fatal("noch umgezogen")
	}
}
