package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// rec trägt eine ändernde Aktion in den Verlauf der betroffenen Geräte ein:
// Gerät aus dem Pfad ({id}), aus "ids" im Body oder aus "results" der Antwort.
func (w *web) rec(action string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
		cw := &captureWriter{ResponseWriter: rw, code: 200}
		h.ServeHTTP(cw, r)

		var req struct {
			IDs      []string `json:"ids"`
			Action   string   `json:"action"`
			Name     string   `json:"name"`
			User     string   `json:"user"`
			Location *string  `json:"location"`
		}
		json.Unmarshal(body, &req)
		var resp struct {
			OK      *bool  `json:"ok"`
			Msg     string `json:"msg"`
			Results []struct {
				ID  string `json:"id"`
				OK  bool   `json:"ok"`
				Msg string `json:"msg"`
			} `json:"results"`
		}
		json.Unmarshal(cw.buf.Bytes(), &resp)

		detail := ""
		switch {
		case req.Action != "":
			detail = changeLabel(req.Action)
		case req.Name != "":
			detail = "neuer Name: " + req.Name
		case req.User != "":
			detail = "Benutzer: " + req.User
		case req.Location != nil:
			detail = "Standort: " + *req.Location
		}
		if n := r.PathValue("name"); n != "" {
			detail = n
		}
		var fail error
		if cw.code >= 400 {
			fail = fmt.Errorf("%s", strings.TrimSpace(cw.buf.String()))
		} else if resp.OK != nil && !*resp.OK {
			fail = fmt.Errorf("%s", resp.Msg)
		}
		seen := map[string]bool{}
		add := func(id string, err error) {
			if id == "" || seen[id] {
				return
			}
			seen[id] = true
			w.pl.hist.Add(id, w.user, action, detail, err)
		}
		for _, x := range resp.Results {
			var err error
			if !x.OK {
				err = fmt.Errorf("%s", x.Msg)
			}
			add(x.ID, err)
		}
		add(r.PathValue("id"), fail)
		for _, id := range req.IDs {
			add(id, fail)
		}
	})
}

// captureWriter merkt sich Statuscode und den Anfang der Antwort.
type captureWriter struct {
	http.ResponseWriter
	code int
	buf  bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) { c.code = code; c.ResponseWriter.WriteHeader(code) }
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.buf.Len() < 64*1024 {
		c.buf.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

func changeLabel(a string) string {
	if l, ok := map[string]string{"bridge": "alle Ports in die Bridge", "dhcp": "DHCP-Client auf der Bridge", "wlan": "WLAN ändern",
		"central": "zentrale WLAN-Konfiguration", "wifimigrate": "Umstellung auf wifi-Paket", "wwan": "WWAN / LTE-Router", "router": "Router-Konfiguration", "portvlan": "Port-VLAN", "poecycle": "PoE-Cycle", "ipconf": "IP-Einstellungen", "snmp": "SNMP", "cloudmove": "Umzug in die Cloud"}[a]; ok {
		return l
	}
	return a
}

// jobKindLabel für Einträge am Ende eines Jobs.
func jobKindLabel(k string) string {
	if l, ok := map[string]string{"update": "Update", "reboot": "Neustart", "backup": "Backup", "bridge": "Bridge",
		"dhcp": "DHCP-Client", "wlan": "WLAN", "central": "zentrale WLAN-Konfiguration", "wifimigrate": "Umstellung wifi",
		"restore": "Rollback", "wwan": "WWAN / LTE-Router", "router": "Router-Konfiguration", "portvlan": "Port-VLAN", "poecycle": "PoE-Cycle", "ipconf": "IP-Einstellungen", "snmp": "SNMP", "cloudmove": "Umzug in die Cloud"}[k]; ok {
		return l
	}
	return k
}

// --- Detailseite ---

func (w *web) devicePortVLANs(rw http.ResponseWriter, r *http.Request) {
	l, isRouter, err := w.pl.SiteVLANs(r.PathValue("id"))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(rw, map[string]any{"vlans": l, "router": isRouter})
}

func (w *web) deviceRouterWan(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	l, err := w.pl.RouterWan(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, l)
}

func (w *web) devicePorts(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	l, err := w.pl.Ports(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(rw, "Ports lesen: "+err.Error(), http.StatusBadGateway)
		return
	}
	if l == nil {
		l = []PortInfo{}
	}
	writeJSON(rw, l)
}

func (w *web) deviceFiles(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	l, err := w.pl.DeviceFiles(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(rw, "Dateien lesen: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, l)
}

// deviceHistory: Manager-Verlauf, Verlauf vom Gerät und Konfig-Unterschiede.
// Ist das Gerät nicht erreichbar, kommt der Rest trotzdem (deviceError).
func (w *web) deviceHistory(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cfg := w.pl.ConfigChanges(id, 30)
	if cfg == nil {
		cfg = []ConfigChange{}
	}
	out := map[string]any{"manager": w.managerHistory(r, id), "config": cfg}
	if l, err := w.pl.DeviceHistory(ctx, id); err != nil {
		out["device"], out["deviceError"] = []DeviceHistItem{}, err.Error()
	} else {
		out["device"] = l
	}
	writeJSON(rw, out)
}

// managerHistory: was über den Manager an diesem Gerät gemacht wurde.
func (w *web) managerHistory(_ *http.Request, id string) []HistEntry {
	l := w.pl.hist.List(id, 300)
	if l == nil {
		l = []HistEntry{}
	}
	return l
}

func (w *web) setLocation(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Location string `json:"location"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(rw, "ungültige Anfrage", http.StatusBadRequest)
		return
	}
	loc := strings.TrimSpace(req.Location)
	if len([]rune(loc)) > 200 {
		http.Error(rw, "höchstens 200 Zeichen", http.StatusBadRequest)
		return
	}
	if !w.st.Update(r.PathValue("id"), func(d *Device) { d.Location = loc }) {
		http.NotFound(rw, r)
		return
	}
	w.st.save()
	writeJSON(rw, map[string]any{"ok": true, "msg": "Standort gespeichert"})
}

func (w *web) deviceWlanStatus(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	st, err := w.pl.WlanStatus(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(rw, "WLAN-Zustand lesen: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, st)
}

// --- WWAN / LTE-Router ---

func (w *web) deviceWwan(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg, ok := w.pl.WwanConfig(id)
	out := map[string]any{"configured": ok, "config": cfg, "usage": w.pl.WwanUsage(id),
		"defaults": map[string]any{"host": wwanHost, "poolLo": wwanPoolLo, "poolHi": wwanPoolHi, "dns": wwanDNS, "ntp": wwanNTP}}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if l, err := w.pl.LteStatus(ctx, id); err != nil {
		out["lteError"] = err.Error()
	} else {
		out["lte"] = l
	}
	writeJSON(rw, out)
}

func (w *web) saveWwan(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Prefix string            `json:"prefix"`
		APN    string            `json:"apn"`
		Pins   map[string]string `json:"pins"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(rw, "ungültige Anfrage", http.StatusBadRequest)
		return
	}
	c, err := w.pl.SaveWwan(r.PathValue("id"), req.Prefix, req.APN, req.Pins)
	if err != nil {
		writeJSON(rw, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "msg": "WWAN-Einstellung gespeichert – an den Geräten ändert sich erst mit „Vorschau & ausrollen“ etwas", "config": c})
}

// wwanCSV: Monatsvolumen als CSV (Semikolon, für Excel). ids: Geräte, month: "" = alle Monate.
func (w *web) wwanCSV(rw http.ResponseWriter, ids []string, month, file string) {
	rw.Header().Set("Content-Type", "text/csv; charset=utf-8")
	rw.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	rw.Write([]byte{0xef, 0xbb, 0xbf})
	cw := csv.NewWriter(rw)
	cw.Comma = ';'
	cw.Write([]string{"Monat", "Gerät", "Standort", "Empfangen (MB)", "Gesendet (MB)", "Summe (MB)"})
	mb := func(b int64) string {
		return strings.Replace(strconv.FormatFloat(float64(b)/1e6, 'f', 1, 64), ".", ",", 1)
	}
	for _, id := range ids {
		d, _ := w.st.Get(id)
		for _, m := range w.pl.WwanUsage(id) {
			if month != "" && m.Month != month {
				continue
			}
			cw.Write([]string{m.Month, d.Identity, d.Location, mb(m.Rx), mb(m.Tx), mb(m.Rx + m.Tx)})
		}
	}
	cw.Flush()
}

var monthRe = regexp.MustCompile(`^\d{4}-\d{2}$`)

func (w *web) deviceWwanCSV(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.wwanCSV(rw, []string{id}, "", "wwan-"+safeName(id)+".csv")
}

func (w *web) allWwanCSV(rw http.ResponseWriter, r *http.Request) {
	month := r.URL.Query().Get("month")
	if month != "" && !monthRe.MatchString(month) {
		http.Error(rw, "Monat als JJJJ-MM", http.StatusBadRequest)
		return
	}
	var ids []string
	for _, d := range w.st.List() {
		if len(d.LTE) > 0 || len(w.pl.WwanUsage(d.ID)) > 0 {
			ids = append(ids, d.ID)
		}
	}
	name := "wwan-volumen.csv"
	if month != "" {
		name = "wwan-volumen-" + month + ".csv"
	}
	w.wwanCSV(rw, ids, month, name)
}

// --- Netzwerk-Baum (MAC-Liste) ---

func (w *web) topology(rw http.ResponseWriter, r *http.Request) {
	var ids []string
	for _, d := range w.st.List() {
		ids = append(ids, d.ID)
	}
	root := r.URL.Query().Get("root")
	if root == "" {
		root = w.pl.TopoRoot("")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	writeJSON(rw, w.pl.Topology(ctx, ids, root))
}

func (w *web) setTopoRoot(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Root string `json:"root"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(rw, "ungültige Anfrage", http.StatusBadRequest)
		return
	}
	if _, ok := w.st.Get(req.Root); req.Root != "" && req.Root != TopoForeign && !ok {
		http.Error(rw, "Gerät unbekannt", http.StatusBadRequest)
		return
	}
	if err := w.pl.SetTopoRoot("", req.Root); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{"ok": true})
}
