package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Lokale Benutzer eines Geräts für die Detailseite. Das Passwort läuft direkt über die API
// und nie durch ein Skript, RouterOS schreibt Skripttexte ins System-Log.

// mgmtUserPrefix: Name und Kommentar der Verwaltungsbenutzer von Manager und Cloud.
const mgmtUserPrefix = "mikrotik-manager"

type DeviceUser struct {
	Name      string `json:"name"`
	Group     string `json:"group,omitempty"`
	Comment   string `json:"comment,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	LastLogin string `json:"lastLogin,omitempty"`
	Managed   bool   `json:"managed,omitempty"` // Verwaltungsbenutzer: Passwort hier nicht änderbar
}

// parseDeviceUsers: Verwaltungsbenutzer ist, womit sich ein Manager anmeldet. Auch der Benutzer
// der Zugangsdaten zählt dazu, ein neues Passwort würde den Manager sonst aussperren.
func parseDeviceUsers(rows []map[string]string, credUser string) []DeviceUser {
	out := make([]DeviceUser, 0, len(rows))
	for _, x := range rows {
		out = append(out, DeviceUser{
			Name: x["name"], Group: x["group"], Comment: x["comment"], Disabled: x["disabled"] == "true", LastLogin: x["last-logged-in"],
			Managed: x["name"] == credUser || strings.HasPrefix(x["name"], mgmtUserPrefix) || strings.HasPrefix(x["comment"], mgmtUserPrefix),
		})
	}
	return out
}

// DeviceUsers liest die lokalen Benutzer des Geräts.
func (p *Poller) DeviceUsers(ctx context.Context, id string) ([]DeviceUser, error) {
	d, ok := p.st.Get(id)
	if !ok {
		return nil, fmt.Errorf("Gerät unbekannt")
	}
	var rows []map[string]string
	err := p.withClient(ctx, d, func(ctx context.Context, cl *routeros.Client) error {
		var err error
		rows, err = printAll(ctx, cl, "/user/print", "=.proplist=name,group,comment,disabled,last-logged-in")
		return err
	})
	if err != nil {
		return nil, err
	}
	return parseDeviceUsers(rows, p.deviceCredUser(id)), nil
}

// SetDeviceUserPassword setzt das Passwort eines Benutzers, Verwaltungsbenutzer sind tabu.
func (p *Poller) SetDeviceUserPassword(ctx context.Context, id, user, pw string) Result {
	l, err := p.DeviceUsers(ctx, id)
	if err != nil {
		return Result{ID: id, Msg: err.Error()}
	}
	for _, u := range l {
		if u.Name != user {
			continue
		}
		if u.Managed {
			return Result{ID: id, Msg: user + " ist ein Verwaltungsbenutzer, sein Passwort pflegt der Manager"}
		}
		r := p.setDeviceUser(ctx, id, user, pw)
		if len(r) == 0 {
			return Result{ID: id, Msg: "Gerät nicht verfügbar"}
		}
		return r[0]
	}
	return Result{ID: id, Msg: "Benutzer " + user + " gibt es auf dem Gerät nicht"}
}

func (w *web) deviceUsers(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	l, err := w.pl.DeviceUsers(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(rw, "Benutzer lesen: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, l)
}

func (w *web) deviceUserPassword(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil || req.User == "" || req.Password == "" {
		http.Error(rw, "Benutzer und Passwort nötig", http.StatusBadRequest)
		return
	}
	writeJSON(rw, w.pl.SetDeviceUserPassword(r.Context(), r.PathValue("id"), req.User, req.Password))
}
