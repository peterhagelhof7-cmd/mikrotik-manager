package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Umzug in die Cloud: Die MikroTik-Manager Cloud übernimmt die Verwaltung eines Geräts,
// dieser Manager liest weiter mit. Nur einer schreibt, sonst überschreiben sich zentrales
// WLAN und Updates gegenseitig, und die Wächter stören einander.
//
// Der Manager erledigt, was sonst der Einzeiler auf dem Gerät tut: WireGuard-
// Schnittstelle anlegen, öffentlichen Schlüssel an die Cloud schicken, das
// Abschluss-Skript auf dem Gerät ausführen. So braucht das Gerät kein /tool fetch
// (im Device-Mode home gesperrt), und der Code landet nie auf dem Gerät.
//
// Die Cloud legt ihren eigenen Benutzer mikrotik-manager-cloud an; der Zugang dieses
// Managers bleibt bestehen. Eine ältere Cloud ohne eigenen Benutzer würde dessen
// Passwort überschreiben, deshalb wird sie abgelehnt (Check, register). Ohne Handshake
// wird alles zurückgenommen. Ist die Cloud weg, gibt CloudRelease das Schreiben frei.

const (
	cloudIface       = "wg-mm"
	cloudUser        = "mikrotik-manager-cloud" // Benutzer und Kommentar der Cloud
	cloudHandshake   = 60 * time.Second
	cloudFinishHead  = "# mikrotik-manager: Abschluss"
	cloudStartMarker = "mikrotik-manager: Anmeldung"
)

var (
	cloudCodeRe = regexp.MustCompile(`(https://[^\s"'/]+(?:/[^\s"'/]+)*?)/enroll/([A-Za-z0-9_-]{32})(?:-nocheck)?(?:\.rsc)?(?:["'\s]|$)`)
	cloudErrRe  = regexp.MustCompile(`:error "(?:mikrotik-manager: )?(.*)"`)
	cloudHTTP   = &http.Client{Timeout: 30 * time.Second}

	errCloudOld = errors.New("Die Cloud ist zu alt: Sie würde den Zugang dieses Managers überschreiben. Erst die Cloud aktualisieren.")

	// errCloudCode: Die Cloud lehnt den Code ab, weitere Geräte sind zwecklos.
	errCloudCode = errors.New("Cloud lehnt den Code ab")
)

// cloudCode ist Adresse und Code aus dem Anmelde-Befehl der Cloud.
type cloudCode struct{ Base, Token string }

// parseCloudCode nimmt den Befehl oder die Adresse, wie die Cloud sie anzeigt.
func parseCloudCode(s string) (cloudCode, error) {
	m := cloudCodeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return cloudCode{}, errors.New("Keine Anmelde-Adresse erkannt. Den Befehl aus der Cloud einfügen (https://…/enroll/<Code>.rsc).")
	}
	return cloudCode{Base: m[1], Token: m[2]}, nil
}

// cloudErr entfernt die Adresse aus Netzwerkfehlern: Sie enthält den Code.
func cloudErr(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

func cloudReply(r *http.Response) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		return "", cloudErr(err)
	}
	body := string(b)
	if r.StatusCode == http.StatusOK {
		return body, nil
	}
	msg := fmt.Sprintf("HTTP %d", r.StatusCode)
	if m := cloudErrRe.FindStringSubmatch(body); m != nil {
		msg = m[1]
	}
	if r.StatusCode == http.StatusForbidden || r.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: %s", errCloudCode, msg)
	}
	return "", fmt.Errorf("Cloud: %s", msg)
}

// Check prüft, ob die Cloud den Code annimmt; das verbraucht ihn nicht.
func (c cloudCode) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/enroll/"+c.Token+".rsc", nil)
	if err != nil {
		return errors.New("Adresse ungültig")
	}
	r, err := cloudHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("Cloud nicht erreichbar: %w", cloudErr(err))
	}
	defer r.Body.Close()
	body, err := cloudReply(r)
	if err != nil {
		return err
	}
	if !strings.Contains(body, cloudStartMarker) {
		return errors.New("Unter dieser Adresse antwortet kein MikroTik-Manager")
	}
	if !strings.Contains(body, cloudUser) {
		return errCloudOld
	}
	return nil
}

// register meldet ein Gerät an und liefert das Abschluss-Skript.
func (c cloudCode) register(ctx context.Context, pub, identity string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/enroll/"+c.Token, strings.NewReader(pub+"\n"+identity))
	if err != nil {
		return "", errors.New("Adresse ungültig")
	}
	req.Header.Set("Content-Type", "text/plain")
	r, err := cloudHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("Cloud nicht erreichbar: %w", cloudErr(err))
	}
	defer r.Body.Close()
	body, err := cloudReply(r)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(body, cloudFinishHead) {
		return "", errors.New("unerwartete Antwort der Cloud")
	}
	if !strings.Contains(body, cloudUser) {
		return "", errCloudOld
	}
	return body, nil
}

// cloudBlock sagt, warum ein Gerät nicht umziehen kann ("" = es kann).
func cloudBlock(d Device) string {
	switch {
	case d.Moved():
		return "bereits umgezogen"
	case d.APIStatus != "ok":
		return "nicht erreichbar oder Login fehlgeschlagen"
	case d.Major() != "7":
		return "RouterOS 7 nötig (Gerät hat " + d.Version + ")"
	}
	return ""
}

// CloudPreview prüft die Geräte; leer = alle. Ändert nichts.
func (p *Poller) CloudPreview(ctx context.Context, ids []string) []Result {
	if len(ids) == 0 {
		for _, d := range p.st.List() {
			ids = append(ids, d.ID)
		}
	}
	var out []Result
	var todo []string
	for _, id := range ids {
		d, ok := p.st.Get(id)
		if !ok {
			continue
		}
		if why := cloudBlock(d); why != "" {
			out = append(out, Result{ID: id, Identity: d.Identity, Msg: why})
		} else {
			todo = append(todo, id)
		}
	}
	return append(out, p.forEach(todo, func(d Device) Result {
		r := Result{ID: d.ID, Identity: d.Identity}
		err := p.withClient(ctx, d, func(ctx context.Context, cl *routeros.Client) error {
			l, err := printAll(ctx, cl, "/interface/wireguard/print", "?name="+cloudIface, "=.proplist=name")
			if err != nil {
				return err
			}
			r.OK, r.Msg = true, "bereit"
			if len(l) > 0 {
				r.Msg = cloudIface + " ist schon vorhanden (war das Gerät in einer Cloud?) und wird weiterverwendet"
			}
			return nil
		})
		if err != nil {
			r.Msg = err.Error()
		}
		return r
	})...)
}

// StartCloudMove zieht die Geräte nacheinander um. Lehnt die Cloud den Code ab
// (z. B. Einmal-Code beim zweiten Gerät), bleiben die übrigen Geräte unberührt.
func (p *Poller) StartCloudMove(c cloudCode, ids []string) []Result {
	var refused atomic.Bool
	return p.startJobs(p.ctx, ids, "cloudmove", func(ctx context.Context, id string, x *Job) error {
		if refused.Load() {
			return errors.New("nicht versucht, die Cloud hat den Code abgelehnt")
		}
		err := p.doCloudMove(ctx, c, id, x)
		if errors.Is(err, errCloudCode) {
			refused.Store(true)
		}
		return err
	})
}

// cloudUndo nimmt das Abschluss-Skript zurück. Offen bleibt nur die Freigabe des
// Hubs in /ip service: Sie gibt ohne Tunnel keinen Zugang.
func cloudUndo(hadUser, createdIface bool) string {
	var b strings.Builder
	b.WriteString(try("/ip firewall filter remove [find in-interface=" + cloudIface + "]"))
	b.WriteString(try("/ip address remove [find interface=" + cloudIface + "]"))
	b.WriteString(try("/interface wireguard peers remove [find interface=" + cloudIface + "]"))
	if createdIface {
		b.WriteString(try("/interface wireguard remove [find name=" + cloudIface + "]"))
	}
	if !hadUser {
		b.WriteString(try("/user remove [find name=" + q(cloudUser) + "]"))
	}
	return b.String()
}

// cloudTunnelUp wartet auf den Handshake. Fehler = Verbindung zum Gerät weg.
func cloudTunnelUp(ctx context.Context, cl *routeros.Client, wait time.Duration) (bool, error) {
	deadline := time.Now().Add(wait)
	for {
		l, err := printAll(ctx, cl, "/interface/wireguard/peers/print", "?interface="+cloudIface, "=.proplist=last-handshake")
		if err != nil {
			return false, err
		}
		for _, x := range l {
			if x["last-handshake"] != "" {
				return true, nil
			}
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func (p *Poller) doCloudMove(ctx context.Context, c cloudCode, id string, x *Job) error {
	d, _ := p.st.Get(id)
	name := d.Identity
	if why := cloudBlock(d); why != "" {
		return errors.New(why)
	}
	cl, err := p.session(ctx, id)
	if err != nil {
		return err
	}
	defer cl.Close()

	p.jobs.step(x, name, "erstelle Backup und lade es auf den Manager …")
	bk, err := p.BackupToManager(ctx, id, cl, "vor-cloud")
	if err != nil {
		return fmt.Errorf("Backup: %w – nichts geändert", err)
	}
	p.jobs.step(x, name, "Backup gespeichert: %s", bk)

	rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	uid, err := findUser(rctx, cl, cloudUser)
	if err != nil {
		return err
	}
	l, err := printAll(rctx, cl, "/interface/wireguard/print", "?name="+cloudIface, "=.proplist=public-key")
	if err != nil {
		return err
	}
	created := len(l) == 0
	if created {
		if _, err := cl.RunContext(rctx, "/interface/wireguard/add", "=name="+cloudIface, "=comment="+cloudUser); err != nil {
			return fmt.Errorf("WireGuard anlegen: %w", err)
		}
		time.Sleep(time.Second)
		if l, err = printAll(rctx, cl, "/interface/wireguard/print", "?name="+cloudIface, "=.proplist=public-key"); err != nil {
			return err
		}
	}
	dropIface := func() {
		if created {
			p.runQuiet(rctx, cl, try("/interface wireguard remove [find name="+cloudIface+"]"), 30*time.Second)
		}
	}
	if len(l) == 0 || l[0]["public-key"] == "" {
		dropIface()
		return errors.New("WireGuard-Schlüssel nicht lesbar")
	}

	p.jobs.step(x, name, "melde das Gerät bei der Cloud an …")
	script, err := c.register(rctx, l[0]["public-key"], name)
	if err != nil {
		dropIface()
		return err
	}
	p.jobs.step(x, name, "richte den Tunnel ein …")
	runErr := p.runQuiet(rctx, cl, script, 30*time.Second)
	up := false
	if runErr == nil {
		p.jobs.step(x, name, "warte auf den Tunnel (bis %s) …", cloudHandshake)
		if up, runErr = cloudTunnelUp(rctx, cl, cloudHandshake); runErr != nil {
			return fmt.Errorf("Verbindung zum Gerät verloren (%v) – in der Cloud prüfen, ob es angekommen ist", runErr)
		}
	}
	if !up {
		p.jobs.step(x, name, "kein Tunnel – nehme die Änderung zurück …")
		if err := p.runQuiet(rctx, cl, cloudUndo(uid != "", created), 30*time.Second); err != nil {
			return fmt.Errorf("Tunnel steht nicht, Rücknahme fehlgeschlagen: %v", err)
		}
		if runErr != nil {
			return fmt.Errorf("Abschluss-Skript: %v – zurückgenommen, in der Cloud den Eintrag vergessen", runErr)
		}
		return errors.New("kein Tunnel zur Cloud (UDP gesperrt?) – zurückgenommen, in der Cloud den Eintrag vergessen")
	}
	now := time.Now()
	p.st.Update(id, func(x *Device) {
		x.CloudURL, x.CloudAt = c.Base, now
	})
	p.jobs.step(x, name, "Tunnel steht – die Cloud verwaltet das Gerät, dieser Manager liest mit")
	return nil
}

// CloudRelease gibt das Schreiben wieder frei, z. B. wenn die Cloud ausfällt oder das Gerät
// dort entfernt wurde. Das Gerät selbst ändert sich nicht.
func (p *Poller) CloudRelease(id string) Result {
	d, ok := p.st.Get(id)
	if !ok {
		return Result{ID: id, Msg: "Gerät unbekannt"}
	}
	p.st.Update(id, func(x *Device) { x.CloudURL, x.CloudAt = "", time.Time{} })
	return Result{ID: id, Identity: d.Identity, OK: true, Msg: "dieser Manager verwaltet das Gerät wieder"}
}
