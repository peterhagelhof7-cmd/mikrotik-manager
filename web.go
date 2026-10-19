package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type web struct {
	cfg   Config
	st    *Store
	pl    *Poller
	md    *MNDP
	vault *Vault
	user  string
	pass  string
	upd   *selfUpdater
}

// ensureWebPassword: WEB_PASSWORD gewinnt; sonst wird beim ersten Start ein
// Passwort erzeugt, verschlüsselt abgelegt und einmal nach
// DATA_DIR/web-password.txt geschrieben. Ohne Schutz geht es nicht – der
// Dienst verwaltet Admin-Zugänge aller Router.
func ensureWebPassword(cfg Config, v *Vault) string {
	if cfg.WebPassword != "" {
		return cfg.WebPassword
	}
	if c, ok := v.Get("web"); ok {
		return c.Password
	}
	b := make([]byte, 12)
	rand.Read(b)
	pw := base64.RawURLEncoding.EncodeToString(b)
	if err := v.Set("web", Cred{User: cfg.WebUser, Password: pw}); err != nil {
		log.Fatalf("Web-Passwort speichern: %v", err)
	}
	f := filepath.Join(cfg.DataDir, "web-password.txt")
	os.WriteFile(f, []byte(cfg.WebUser+" / "+pw+"\n"), 0o600)
	log.Printf("Web-Passwort erzeugt (Benutzer %s), steht in %s", cfg.WebUser, f)
	return pw
}

func newWeb(cfg Config, st *Store, pl *Poller, md *MNDP, v *Vault, pass string, static fs.FS, upd *selfUpdater) http.Handler {
	w := &web{cfg: cfg, st: st, pl: pl, md: md, vault: v, user: cfg.WebUser, pass: pass, upd: upd}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("ok")) })
	mux.Handle("GET /", w.auth(http.FileServerFS(static)))
	mux.Handle("GET /api/devices", w.auth(http.HandlerFunc(w.devices)))
	mux.Handle("POST /api/discover", w.auth(http.HandlerFunc(w.discover)))
	mux.Handle("POST /api/credentials/default", w.auth(http.HandlerFunc(w.setDefaultCred)))
	mux.Handle("POST /api/devices/{id}/credentials", w.auth(http.HandlerFunc(w.setDeviceCred)))
	mux.Handle("DELETE /api/devices/{id}/credentials", w.auth(http.HandlerFunc(w.delDeviceCred)))
	mux.Handle("POST /api/devices/{id}/poll", w.auth(http.HandlerFunc(w.poll)))
	mux.Handle("DELETE /api/devices/{id}", w.auth(http.HandlerFunc(w.forget)))
	mux.Handle("GET /api/manager-user", w.auth(http.HandlerFunc(w.managerUser)))
	mux.Handle("POST /api/manager-user", w.auth(http.HandlerFunc(w.ensureManagerUser)))
	mux.Handle("POST /api/user-password", w.auth(http.HandlerFunc(w.setUserPassword)))
	mux.Handle("POST /api/update", w.auth(http.HandlerFunc(w.jobAction(w.pl.StartUpdate))))
	mux.Handle("POST /api/reboot", w.auth(http.HandlerFunc(w.jobAction(w.pl.StartReboot))))
	mux.Handle("POST /api/devices/{id}/identity", w.auth(http.HandlerFunc(w.setIdentity)))
	mux.Handle("POST /api/devices/{id}/plan", w.auth(http.HandlerFunc(w.plan)))
	mux.Handle("POST /api/devices/{id}/change", w.auth(http.HandlerFunc(w.change)))
	mux.Handle("GET /api/devices/{id}/wlan", w.auth(http.HandlerFunc(w.wlan)))
	mux.Handle("POST /api/wlan/preview", w.auth(http.HandlerFunc(w.wlanBulkPreview)))
	mux.Handle("POST /api/wlan/apply", w.auth(http.HandlerFunc(w.wlanBulkApply)))
	mux.Handle("POST /api/wlan/clients/refresh", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.pl.RefreshAllClients(r.Context())
		writeJSON(rw, map[string]bool{"ok": true})
	})))
	mux.Handle("POST /api/self-update/check", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.upd.Check(r.Context())
		writeJSON(rw, w.upd.Info())
	})))
	mux.Handle("POST /api/self-update/apply", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if err := w.upd.Start(); err != nil {
			http.Error(rw, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(rw, w.upd.Info())
	})))
	mux.Handle("GET /api/auto-update", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { writeJSON(rw, w.pl.AutoUpdateView()) })))
	mux.Handle("POST /api/auto-update", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var a AutoUpdate
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&a); err != nil {
			http.Error(rw, "ungültige Daten", http.StatusBadRequest)
			return
		}
		if err := w.pl.SaveAutoUpdate(a); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(rw, w.pl.AutoUpdateView())
	})))
	mux.Handle("POST /api/auto-update/run", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if err := w.pl.RunAutoUpdate("von Hand"); err != nil {
			http.Error(rw, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(rw, w.pl.AutoUpdateView())
	})))
	mux.Handle("GET /api/central", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { writeJSON(rw, w.pl.Central()) })))
	mux.Handle("POST /api/central", w.auth(http.HandlerFunc(w.saveCentral)))
	mux.Handle("POST /api/central/preview", w.auth(http.HandlerFunc(w.centralPreview)))
	mux.Handle("POST /api/central/apply", w.auth(http.HandlerFunc(w.centralApply)))
	mux.Handle("POST /api/backup", w.auth(http.HandlerFunc(w.jobAction(w.pl.StartBackup))))
	mux.Handle("GET /api/devices/{id}/backups", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, w.pl.ListBackups(r.PathValue("id")))
	})))
	mux.Handle("GET /api/devices/{id}/backups/{name}", w.auth(http.HandlerFunc(w.downloadBackup)))
	mux.Handle("GET /api/devices/{id}/backups/{name}/restore", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		writeJSON(rw, w.pl.CheckRestore(ctx, r.PathValue("id"), r.PathValue("name")))
	})))
	mux.Handle("POST /api/devices/{id}/backups/{name}/restore", w.auth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id, name := r.PathValue("id"), r.PathValue("name")
		if p := w.pl.BackupPath(id, name); p == "" || !strings.HasSuffix(name, ".backup") {
			http.Error(rw, "nur .backup-Dateien", http.StatusBadRequest)
			return
		}
		writeJSON(rw, map[string]any{"results": w.pl.StartRestore(id, name)})
	})))
	return mux
}

func (w *web) downloadBackup(rw http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	path := w.pl.BackupPath(id, name)
	if path == "" {
		http.NotFound(rw, r)
		return
	}
	d, _ := w.st.Get(id)
	// Dateiname mit Gerätenamen, damit Downloads unterscheidbar bleiben
	fn := strings.Map(func(c rune) rune {
		if c < 0x20 || c == '"' || c == '/' || c == '\\' {
			return '_'
		}
		return c
	}, d.Identity+"-"+name)
	rw.Header().Set("Content-Disposition", `attachment; filename="`+fn+`"`)
	rw.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(rw, r, path)
}

func (w *web) setIdentity(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(rw, "name nötig", http.StatusBadRequest)
		return
	}
	writeJSON(rw, w.pl.SetIdentity(r.Context(), r.PathValue("id"), req.Name))
}

func readChange(r *http.Request) (ChangeReq, bool) {
	var req ChangeReq
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&req)
	return req, err == nil && req.Action != ""
}

// plan zeigt, was eine Änderung tun würde (ändert nichts). POST, weil die
// gewünschten Werte (z. B. WLAN-Passwörter) nicht in die URL gehören.
func (w *web) plan(rw http.ResponseWriter, r *http.Request) {
	req, ok := readChange(r)
	if !ok {
		http.Error(rw, "action nötig", http.StatusBadRequest)
		return
	}
	p, err := w.pl.Preview(r.Context(), r.PathValue("id"), req)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, p)
}

func (w *web) change(rw http.ResponseWriter, r *http.Request) {
	req, ok := readChange(r)
	if !ok {
		http.Error(rw, "action nötig", http.StatusBadRequest)
		return
	}
	res := w.pl.StartChange([]string{r.PathValue("id")}, req)
	if len(res) == 0 {
		res = []Result{{ID: r.PathValue("id"), Msg: "Gerät unbekannt"}}
	}
	writeJSON(rw, res[0])
}

func (w *web) wlan(rw http.ResponseWriter, r *http.Request) {
	info, err := w.pl.ReadWLAN(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, info)
}

// WLAN im ganzen Netz: dieselbe Änderung für alle Schnittstellen mit einer
// SSID auf den genannten Geräten.
type bulkWlan struct {
	IDs  []string `json:"ids"`
	Edit WlanEdit `json:"edit"`
}

func readBulk(r *http.Request) (bulkWlan, bool) {
	var b bulkWlan
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&b)
	return b, err == nil && len(b.IDs) > 0 && b.Edit.MatchSSID != "" && b.Edit.Name == ""
}

func (w *web) wlanBulkPreview(rw http.ResponseWriter, r *http.Request) {
	b, ok := readBulk(r)
	if !ok {
		http.Error(rw, "ids und edit.matchSsid nötig", http.StatusBadRequest)
		return
	}
	type row struct {
		ID       string `json:"id"`
		Identity string `json:"identity"`
		Plan     *Plan  `json:"plan,omitempty"`
		Error    string `json:"error,omitempty"`
	}
	out := make([]row, len(b.IDs))
	var wg sync.WaitGroup
	for i, id := range b.IDs {
		d, _ := w.st.Get(id)
		out[i] = row{ID: id, Identity: d.Identity}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := w.pl.Preview(r.Context(), id, ChangeReq{Action: "wlan", WLAN: []WlanEdit{b.Edit}})
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			out[i].Plan = &p
		}()
	}
	wg.Wait()
	writeJSON(rw, out)
}

// Zentrale WLAN-Verwaltung: Speichern ändert nichts an Geräten; erst „Ausrollen“.
func (w *web) saveCentral(rw http.ResponseWriter, r *http.Request) {
	var c CentralWLAN
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&c); err != nil {
		http.Error(rw, "ungültige Daten", http.StatusBadRequest)
		return
	}
	if err := w.pl.SaveCentral(c); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(rw, w.pl.Central())
}

// centralIDs: die genannten Geräte, sonst alle ausgewählten.
func (w *web) centralIDs(r *http.Request) []string {
	var req struct {
		IDs []string `json:"ids"`
	}
	json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&req)
	if len(req.IDs) == 0 {
		return w.pl.Central().Devices
	}
	return req.IDs
}

func (w *web) centralPreview(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, w.pl.CentralPreview(r.Context(), w.centralIDs(r)))
}

func (w *web) centralApply(rw http.ResponseWriter, r *http.Request) {
	c := w.pl.Central()
	if !c.Enabled {
		http.Error(rw, "zentrale WLAN-Verwaltung ist nicht aktiviert", http.StatusBadRequest)
		return
	}
	var ids []string
	for _, id := range w.centralIDs(r) {
		if contains(c.Devices, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		http.Error(rw, "keine ausgewählten Geräte", http.StatusBadRequest)
		return
	}
	writeJSON(rw, map[string]any{"results": w.pl.StartChange(ids, ChangeReq{Action: "central"})})
}

func (w *web) wlanBulkApply(rw http.ResponseWriter, r *http.Request) {
	b, ok := readBulk(r)
	if !ok {
		http.Error(rw, "ids und edit.matchSsid nötig", http.StatusBadRequest)
		return
	}
	writeJSON(rw, map[string]any{"results": w.pl.StartChange(b.IDs, ChangeReq{Action: "wlan", WLAN: []WlanEdit{b.Edit}})})
}

// jobAction startet Hintergrund-Jobs. Anders als bei den Benutzer-Aktionen
// müssen die Geräte ausdrücklich genannt werden (kein „alle“ aus Versehen).
func (w *web) jobAction(fn func([]string) []Result) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&req); err != nil || len(req.IDs) == 0 {
			http.Error(rw, "ids nötig", http.StatusBadRequest)
			return
		}
		writeJSON(rw, map[string]any{"results": fn(req.IDs)})
	}
}

// actionIDs liest {"ids": [...]}; leer = alle Geräte mit funktionierender API.
func (w *web) actionIDs(ids []string) []string {
	if len(ids) > 0 {
		return ids
	}
	for _, d := range w.st.List() {
		if d.APIStatus == "ok" {
			ids = append(ids, d.ID)
		}
	}
	return ids
}

// managerUser zeigt die Zugangsdaten des Verwaltungsbenutzers an (bewusst im
// Klartext – die Weboberfläche ist passwortgeschützt).
func (w *web) managerUser(rw http.ResponseWriter, r *http.Request) {
	c, ok := w.vault.Get(mgrCredKey)
	def, _ := w.vault.Get(defaultCredKey)
	writeJSON(rw, map[string]any{"exists": ok, "user": c.User, "password": c.Password, "isDefault": ok && def == c})
}

func (w *web) ensureManagerUser(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&req)
	writeJSON(rw, map[string]any{"results": w.pl.EnsureManagerUser(r.Context(), w.actionIDs(req.IDs))})
}

func (w *web) setUserPassword(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs      []string `json:"ids"`
		User     string   `json:"user"`
		Password string   `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&req); err != nil || req.User == "" || req.Password == "" {
		http.Error(rw, "user und password nötig", http.StatusBadRequest)
		return
	}
	writeJSON(rw, map[string]any{"results": w.pl.SetUserPassword(r.Context(), w.actionIDs(req.IDs), req.User, req.Password)})
}

func (w *web) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(w.user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(w.pass)) != 1 {
			time.Sleep(500 * time.Millisecond)
			rw.Header().Set("WWW-Authenticate", `Basic realm="mikrotik-manager", charset="UTF-8"`)
			http.Error(rw, "Anmeldung erforderlich", http.StatusUnauthorized)
			return
		}
		// Schreibende Aufrufe nur als JSON: Fremde Seiten können das nicht
		// ohne CORS-Preflight senden (einfacher CSRF-Schutz bei Basic-Auth).
		if r.Method != http.MethodGet && r.Method != http.MethodHead &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(rw, "Content-Type application/json erwartet", http.StatusUnsupportedMediaType)
			return
		}
		h.ServeHTTP(rw, r)
	})
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(rw).Encode(v)
}

func (w *web) devices(rw http.ResponseWriter, r *http.Request) {
	type row struct {
		Device
		HasOwnCred bool      `json:"hasOwnCred"`
		Job        *Job      `json:"job,omitempty"`
		Backups    int       `json:"backups"`
		LastBackup time.Time `json:"lastBackup,omitempty"`
		// WLAN-Clients (jede Minute gelesen); nil = noch nicht gelesen
		Clients   []WlanClient `json:"wlanClients"`
		ClientsAt time.Time    `json:"wlanClientsAt,omitempty"`
		ClientErr string       `json:"wlanClientsError,omitempty"`
	}
	var out []row
	for _, d := range w.st.List() {
		x := row{Device: d, HasOwnCred: w.vault.Has("dev:" + d.ID)}
		if bl := w.pl.ListBackups(d.ID); len(bl) > 0 {
			x.Backups, x.LastBackup = len(bl), bl[0].Time
		}
		if j, ok := w.pl.jobs.Get(d.ID); ok {
			x.Job = &j
		}
		if s, ok := w.pl.wc.get(d.ID); ok {
			x.Clients, x.ClientsAt, x.ClientErr = s.Clients, s.At, s.Err
			if x.Clients == nil {
				x.Clients = []WlanClient{}
			}
		}
		out = append(out, x)
	}
	writeJSON(rw, map[string]any{
		"version":        version,
		"startedAt":      startedAt,
		"apiPort":        w.cfg.APIPort,
		"nextBackup":     w.pl.NextBackup(),
		"defaultCredSet": w.vault.Has(defaultCredKey),
		"update":         w.upd.Info(),
		"devices":        out,
	})
}

func (w *web) discover(rw http.ResponseWriter, r *http.Request) {
	w.md.Request()
	w.pl.TriggerAll()
	writeJSON(rw, map[string]bool{"ok": true})
}

func readCred(r *http.Request) (Cred, bool) {
	var c Cred
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&c); err != nil {
		return c, false
	}
	c.User = strings.TrimSpace(c.User)
	return c, c.User != ""
}

func (w *web) setDefaultCred(rw http.ResponseWriter, r *http.Request) {
	c, ok := readCred(r)
	if !ok {
		http.Error(rw, "Benutzer fehlt", http.StatusBadRequest)
		return
	}
	if err := w.vault.Set(defaultCredKey, c); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	w.pl.TriggerAll()
	writeJSON(rw, map[string]bool{"ok": true})
}

func (w *web) setDeviceCred(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := w.st.Get(id); !ok {
		http.NotFound(rw, r)
		return
	}
	c, ok := readCred(r)
	if !ok {
		http.Error(rw, "Benutzer fehlt", http.StatusBadRequest)
		return
	}
	if err := w.vault.Set("dev:"+id, c); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	w.pl.Trigger(id)
	writeJSON(rw, map[string]bool{"ok": true})
}

func (w *web) delDeviceCred(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.vault.Delete("dev:" + id)
	w.pl.Trigger(id)
	writeJSON(rw, map[string]bool{"ok": true})
}

func (w *web) poll(rw http.ResponseWriter, r *http.Request) {
	w.pl.Trigger(r.PathValue("id"))
	writeJSON(rw, map[string]bool{"ok": true})
}

func (w *web) forget(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.st.Delete(id)
	w.vault.Delete("dev:" + id)
	w.st.save()
	writeJSON(rw, map[string]bool{"ok": true})
}
