package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Verwaltungsbenutzer: eigener Benutzer des Managers auf jedem Gerät (Gruppe
// full), damit admin-Passwörter unabhängig davon geändert werden können.
const (
	mgrUser    = "mikrotik-manager"
	mgrCredKey = "mgr"
)

// Result ist das Ergebnis einer Aktion auf einem Gerät.
type Result struct {
	ID       string `json:"id"`
	Identity string `json:"identity"`
	OK       bool   `json:"ok"`
	Msg      string `json:"msg"`
}

func randomPassword(n int) string {
	const chars = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[k.Int64()]
	}
	return string(b)
}

// managerCred liefert die Zugangsdaten des Verwaltungsbenutzers und erzeugt
// sie beim ersten Aufruf.
func (p *Poller) managerCred() (Cred, error) {
	if c, ok := p.vault.Get(mgrCredKey); ok {
		return c, nil
	}
	c := Cred{User: mgrUser, Password: randomPassword(20)}
	return c, p.vault.Set(mgrCredKey, c)
}

// forEach führt fn seriell-parallel (max. 4) auf den Geräten aus.
func (p *Poller) forEach(ids []string, fn func(d Device) Result) []Result {
	var (
		mu  sync.Mutex
		out []Result
		wg  sync.WaitGroup
		sem = make(chan struct{}, 4)
	)
	for _, id := range ids {
		d, ok := p.st.Get(id)
		if !ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			r := fn(d)
			r.ID, r.Identity = d.ID, d.Identity
			log.Printf("Aktion %s (%s): ok=%v %s", d.Identity, d.IP, r.OK, r.Msg)
			mu.Lock()
			out = append(out, r)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// withClient meldet sich mit den aktuell gültigen Zugangsdaten an.
func (p *Poller) withClient(ctx context.Context, d Device, fn func(ctx context.Context, cl *routeros.Client) error) error {
	if d.IP == "" {
		return fmt.Errorf("keine IP bekannt")
	}
	c, _ := p.vault.For(d.ID)
	cl, err := p.dial(ctx, d.IP, c)
	if err != nil {
		_, msg := classifyErr(err, p.cfg.APIPort)
		return fmt.Errorf("%s", msg)
	}
	defer cl.Close()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return fn(rctx, cl)
}

// findUser liefert die .id eines lokalen Benutzers oder "".
func findUser(ctx context.Context, cl *routeros.Client, name string) (string, error) {
	r, err := cl.RunContext(ctx, "/user/print", "?name="+name, "=.proplist=.id")
	if err != nil {
		return "", err
	}
	if len(r.Re) == 0 {
		return "", nil
	}
	return r.Re[0].Map[".id"], nil
}

// EnsureManagerUser legt den Verwaltungsbenutzer auf den Geräten an (bzw.
// setzt sein Passwort), prüft die Anmeldung damit und macht ihn erst dann zu
// den Zugangsdaten des Geräts. Gerätespezifische Zugangsdaten entfallen danach.
func (p *Poller) EnsureManagerUser(ctx context.Context, ids []string) []Result {
	mc, err := p.managerCred()
	if err != nil {
		return []Result{{Msg: "Passwort speichern: " + err.Error()}}
	}
	res := p.forEach(ids, func(d Device) Result {
		err := p.withClient(ctx, d, func(ctx context.Context, cl *routeros.Client) error {
			id, err := findUser(ctx, cl, mgrUser)
			if err != nil {
				return err
			}
			if id == "" {
				_, err = cl.RunContext(ctx, "/user/add", "=name="+mgrUser, "=group=full",
					"=password="+mc.Password, "=comment=mikrotik-manager (nicht loeschen)")
			} else {
				_, err = cl.RunContext(ctx, "/user/set", "=.id="+id, "=group=full", "=password="+mc.Password)
			}
			return err
		})
		if err != nil {
			return Result{Msg: err.Error()}
		}
		// Gegenprobe mit dem neuen Benutzer
		cl, err := p.dial(ctx, d.IP, mc)
		if err != nil {
			return Result{Msg: "angelegt, aber Anmeldung damit scheitert: " + err.Error()}
		}
		cl.Close()
		return Result{OK: true, Msg: "Verwaltungsbenutzer eingerichtet"}
	})
	ok := 0
	for _, r := range res {
		if r.OK {
			ok++
			p.vault.Delete("dev:" + r.ID)
		}
	}
	// Standard-Zugangsdaten erst umstellen, wenn mindestens ein Gerät geprüft
	// funktioniert; Geräte ohne den Benutzer haben ggf. noch eigene Daten.
	if ok > 0 {
		if prev, had := p.vault.Get(defaultCredKey); !had || prev != mc {
			// Bisherige Standard-Zugangsdaten bleiben für fehlgeschlagene Geräte erhalten.
			for _, r := range res {
				if !r.OK && had {
					if _, own := p.vault.Get("dev:" + r.ID); !own {
						p.vault.Set("dev:"+r.ID, prev)
					}
				}
			}
			p.vault.Set(defaultCredKey, mc)
		}
	}
	for _, r := range res {
		p.Trigger(r.ID)
	}
	return res
}

// SetUserPassword setzt das Passwort eines lokalen Benutzers (z. B. admin)
// und prüft danach die Anmeldung damit.
func (p *Poller) SetUserPassword(ctx context.Context, ids []string, user, pw string) []Result {
	return p.forEach(ids, func(d Device) Result {
		if c, _ := p.vault.For(d.ID); c.User == user {
			return Result{Msg: "Manager meldet sich selbst als " + user + " an – zuerst Verwaltungsbenutzer einrichten"}
		}
		err := p.withClient(ctx, d, func(ctx context.Context, cl *routeros.Client) error {
			id, err := findUser(ctx, cl, user)
			if err != nil {
				return err
			}
			if id == "" {
				return fmt.Errorf("Benutzer %s gibt es auf dem Gerät nicht", user)
			}
			_, err = cl.RunContext(ctx, "/user/set", "=.id="+id, "=password="+pw)
			return err
		})
		if err != nil {
			return Result{Msg: err.Error()}
		}
		cl, err := p.dial(ctx, d.IP, Cred{User: user, Password: pw})
		if err != nil {
			if strings.Contains(err.Error(), "invalid user name or password") {
				return Result{Msg: "gesetzt, aber Anmeldung damit scheitert"}
			}
			// z. B. Benutzergruppe ohne API-Recht: Passwort ist trotzdem gesetzt
			return Result{OK: true, Msg: "gesetzt (Gegenprobe nicht möglich: " + err.Error() + ")"}
		}
		cl.Close()
		return Result{OK: true, Msg: "Passwort gesetzt und geprüft"}
	})
}
