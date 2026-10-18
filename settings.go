package main

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/go-routeros/routeros/v3"
)

// Konfigurationsänderungen, bei denen sich der Manager aussperren kann
// (Bridge, DHCP-Client), laufen mit Sicherheitsnetz („Commit-Confirm“):
//  1. Backup auf den Manager
//  2. Skript mm-undo (Rückweg) und mm-apply (Änderung) auf dem Gerät anlegen
//  3. Wächter starten: nach rollbackAfter mm-undo ausführen, falls es noch existiert
//  4. mm-apply im Hintergrund ausführen (überlebt den Verbindungsabbruch)
//  5. Gerät neu verbinden (ggf. unter neuer IP), Ergebnis prüfen
//  6. Erfolg → mm-undo löschen (= bestätigen). Kommt der Manager nicht mehr
//     dran, rollt das Gerät selbst zurück.
const rollbackAfter = 5 * time.Minute

const mmComment = "mikrotik-manager"

// Plan beschreibt eine Änderung, bevor sie ausgeführt wird (Vorschau).
type Plan struct {
	Action  string   `json:"action"`
	Steps   []string `json:"steps"`
	Warn    []string `json:"warn,omitempty"`
	Blocked string   `json:"blocked,omitempty"` // Grund, warum nicht ausgeführt wird
	Noop    bool     `json:"noop"`
	Apply   string   `json:"-"`
	Undo    string   `json:"-"`
	NewIP   bool     `json:"newIp"` // IP ändert sich voraussichtlich
	Bridge  string   `json:"bridge,omitempty"`

	wlan []WlanEdit // WLAN: auf Schnittstellennamen festgelegte Änderungen
	mig  *migration // Umstellung wireless → wifi
}

// ChangeReq ist eine gewünschte Änderung aus der Weboberfläche.
type ChangeReq struct {
	Action       string     `json:"action"` // bridge | dhcp | wlan | wifimigrate
	RemoveStatic bool       `json:"removeStatic,omitempty"`
	WLAN         []WlanEdit `json:"wlan,omitempty"`
	Country      string     `json:"country,omitempty"` // WLAN-Land für alle Funkteile
}

// q setzt einen Wert für RouterOS-Skripte in Anführungszeichen.
func q(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`).Replace(s) + `"`
}

// try verpackt eine Zeile so, dass ein Fehler das Rückroll-Skript nicht abbricht.
func try(cmd string) string { return ":do { " + cmd + " } on-error={}\n" }

func printAll(ctx context.Context, cl *routeros.Client, cmd string, args ...string) ([]map[string]string, error) {
	r, err := cl.RunContext(ctx, append([]string{cmd}, args...)...)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(r.Re))
	for _, s := range r.Re {
		out = append(out, s.Map)
	}
	return out, nil
}

// --- Name ---

func validIdentity(n string) error {
	if n == "" || len(n) > 64 {
		return fmt.Errorf("Name muss 1–64 Zeichen lang sein")
	}
	for _, r := range n {
		if unicode.IsControl(r) || r > unicode.MaxASCII {
			return fmt.Errorf("Name nur mit ASCII-Zeichen (keine Umlaute)")
		}
	}
	return nil
}

// SetIdentity setzt den Gerätenamen (/system identity). Ungefährlich, daher
// ohne Sicherheitsnetz, aber mit Gegenprobe.
func (p *Poller) SetIdentity(ctx context.Context, id, name string) Result {
	d, _ := p.st.Get(id)
	r := Result{ID: id, Identity: d.Identity}
	name = strings.TrimSpace(name)
	if err := validIdentity(name); err != nil {
		r.Msg = err.Error()
		return r
	}
	err := p.withClient(ctx, d, func(ctx context.Context, cl *routeros.Client) error {
		if _, err := cl.RunContext(ctx, "/system/identity/set", "=name="+name); err != nil {
			return err
		}
		m, err := printAll(ctx, cl, "/system/identity/print")
		if err != nil {
			return err
		}
		if len(m) == 0 || m[0]["name"] != name {
			return fmt.Errorf("Gegenprobe: Name ist nicht übernommen")
		}
		return nil
	})
	if err != nil {
		r.Msg = err.Error()
		return r
	}
	p.st.Update(id, func(x *Device) { x.Identity = name })
	p.Trigger(id)
	r.OK, r.Msg = true, fmt.Sprintf("umbenannt: %s → %s", d.Identity, name)
	return r
}

// --- Alle Ports in eine Bridge ---

func planBridge(ctx context.Context, cl *routeros.Client) (Plan, error) {
	p := Plan{Action: "bridge"}
	bridges, err := printAll(ctx, cl, "/interface/bridge/print", "=.proplist=name")
	if err != nil {
		return p, err
	}
	var apply, undo strings.Builder
	if len(bridges) > 0 {
		p.Bridge = bridges[0]["name"]
	} else {
		p.Bridge = "bridge1"
		p.Steps = append(p.Steps, "Bridge bridge1 anlegen (RSTP an, schützt vor Schleifen)")
		apply.WriteString("/interface bridge add name=" + q(p.Bridge) + " comment=" + q(mmComment) + "\n")
	}
	if len(bridges) > 1 {
		p.Warn = append(p.Warn, fmt.Sprintf("%d Bridges vorhanden – verwendet wird %s", len(bridges), p.Bridge))
	}
	ports, err := printAll(ctx, cl, "/interface/bridge/port/print", "=.proplist=interface,bridge")
	if err != nil {
		return p, err
	}
	inBridge := map[string]string{}
	allIn := "" // Bridge, die per Schnittstellenliste "all" ohnehin alles enthält
	lists := map[string]string{}
	if l, err := printAll(ctx, cl, "/interface/list/print", "=.proplist=name"); err == nil {
		for _, m := range l {
			lists[m["name"]] = ""
		}
	}
	for _, m := range ports {
		ifc := m["interface"]
		switch _, isList := lists[ifc]; {
		case ifc == "all":
			allIn = m["bridge"]
		case isList:
			// Port ist eine Schnittstellenliste: deren Mitglieder sind in der Bridge
			if mem, err := printAll(ctx, cl, "/interface/list/member/print", "=.proplist=list,interface"); err == nil {
				for _, x := range mem {
					if x["list"] == ifc {
						inBridge[x["interface"]] = m["bridge"]
					}
				}
			}
		default:
			inBridge[ifc] = m["bridge"]
		}
	}

	// Kandidaten: Ethernet + physische WLAN-Schnittstellen (altes wireless und
	// neues wifi-Paket). Virtuelle WLANs (master-interface gesetzt) sind oft
	// Gäste-/Sondernetze und werden NIE automatisch in die Bridge genommen.
	var cands, virtual []string
	for _, menu := range []string{"/interface/ethernet/print", "/interface/wireless/print", "/interface/wifi/print"} {
		l, err := printAll(ctx, cl, menu, "=.proplist=name,master-interface")
		if err != nil {
			continue // Menü gibt es auf dem Gerät nicht
		}
		for _, m := range l {
			if mi := m["master-interface"]; mi != "" && mi != "none" {
				virtual = append(virtual, m["name"])
				continue
			}
			cands = append(cands, m["name"])
		}
	}
	sort.Strings(cands)
	var add []string
	for _, ifc := range cands {
		b, ok := inBridge[ifc]
		if !ok && allIn != "" {
			b, ok = allIn, true
		}
		switch {
		case !ok:
			add = append(add, ifc)
		case b != p.Bridge:
			p.Warn = append(p.Warn, fmt.Sprintf("%s hängt an %s – bleibt dort", ifc, b))
		}
	}
	var vOut []string
	for _, v := range virtual {
		if _, ok := inBridge[v]; !ok && allIn == "" {
			vOut = append(vOut, v)
		}
	}
	if len(vOut) > 0 {
		sort.Strings(vOut)
		p.Warn = append(p.Warn, "Virtuelle WLANs ohne Bridge (werden nicht angefasst, ggf. Gästenetz – bitte selbst prüfen): "+strings.Join(vOut, ", "))
	}
	if len(add) == 0 && p.Steps == nil {
		p.Noop = true
		p.Steps = []string{fmt.Sprintf("Alle Ports sind bereits in %s – nichts zu tun", p.Bridge)}
		return p, nil
	}

	// Router-Schutz: ein aktiver DHCP-Server würde nach dem Zusammenlegen ins
	// vorgelagerte Netz verteilen.
	if srv, err := printAll(ctx, cl, "/ip/dhcp-server/print", "=.proplist=name,disabled"); err == nil {
		for _, s := range srv {
			if s["disabled"] != "true" {
				p.Blocked = fmt.Sprintf("DHCP-Server %q ist aktiv – das Gerät arbeitet als Router. Erst DHCP-Server/NAT abschalten, sonst verteilt es Adressen ins ganze Netz.", s["name"])
			}
		}
	}

	addSet := map[string]bool{}
	for _, ifc := range add {
		addSet[ifc] = true
		p.Steps = append(p.Steps, fmt.Sprintf("%s in %s aufnehmen", ifc, p.Bridge))
		apply.WriteString("/interface bridge port add bridge=" + q(p.Bridge) + " interface=" + q(ifc) + "\n")
		undo.WriteString(try("/interface bridge port remove [find bridge=" + q(p.Bridge) + " interface=" + q(ifc) + "]"))
	}
	// IP-Adressen und DHCP-Clients der aufgenommenen Ports wandern auf die Bridge
	addrs, err := printAll(ctx, cl, "/ip/address/print", "=.proplist=.id,address,interface,dynamic")
	if err != nil {
		return p, err
	}
	for _, a := range addrs {
		if addSet[a["interface"]] && a["dynamic"] != "true" {
			p.Steps = append(p.Steps, fmt.Sprintf("IP %s von %s auf %s verschieben", a["address"], a["interface"], p.Bridge))
			apply.WriteString("/ip address set numbers=" + a[".id"] + " interface=" + q(p.Bridge) + "\n")
			undo.WriteString(try("/ip address set numbers=" + a[".id"] + " interface=" + q(a["interface"])))
		}
	}
	if dcs, err := printAll(ctx, cl, "/ip/dhcp-client/print", "=.proplist=.id,interface"); err == nil {
		for _, c := range dcs {
			if addSet[c["interface"]] {
				p.Steps = append(p.Steps, fmt.Sprintf("DHCP-Client von %s auf %s verschieben", c["interface"], p.Bridge))
				apply.WriteString("/ip dhcp-client set numbers=" + c[".id"] + " interface=" + q(p.Bridge) + "\n")
				undo.WriteString(try("/ip dhcp-client set numbers=" + c[".id"] + " interface=" + q(c["interface"])))
			}
		}
	}
	if len(bridges) == 0 {
		undo.WriteString(try("/interface bridge remove [find name=" + q(p.Bridge) + " comment=" + q(mmComment) + "]"))
	}
	p.Warn = append(p.Warn, "Die Verbindung zum Gerät bricht kurz ab, während die Ports umgehängt werden.")
	p.Apply, p.Undo = apply.String(), undo.String()
	return p, nil
}

// --- DHCP-Client auf der Bridge ---

func planDHCP(ctx context.Context, cl *routeros.Client, removeStatic bool) (Plan, error) {
	p := Plan{Action: "dhcp"}
	bridges, err := printAll(ctx, cl, "/interface/bridge/print", "=.proplist=name")
	if err != nil {
		return p, err
	}
	if len(bridges) == 0 {
		p.Blocked = "Keine Bridge vorhanden – zuerst „Alle Ports in Bridge“ ausführen."
		return p, nil
	}
	br := bridges[0]["name"]
	p.Bridge = br

	dcs, err := printAll(ctx, cl, "/ip/dhcp-client/print", "=.proplist=.id,interface,disabled,status")
	if err != nil {
		return p, err
	}
	var existing map[string]string
	for _, c := range dcs {
		if c["interface"] == br {
			existing = c
		}
	}
	addrs, err := printAll(ctx, cl, "/ip/address/print", "=.proplist=.id,address,interface,dynamic")
	if err != nil {
		return p, err
	}
	var statics []map[string]string
	for _, a := range addrs {
		if a["interface"] == br && a["dynamic"] != "true" {
			statics = append(statics, a)
		}
	}
	routes, err := printAll(ctx, cl, "/ip/route/print", "=.proplist=.id,dst-address,gateway,distance,dynamic,routing-table")
	if err != nil {
		return p, err
	}
	var defRoutes []map[string]string
	for _, r := range routes {
		if r["dst-address"] == "0.0.0.0/0" && r["dynamic"] != "true" {
			defRoutes = append(defRoutes, r)
		}
	}

	clientOK := existing != nil && existing["disabled"] != "true"
	if clientOK && (!removeStatic || (len(statics) == 0 && len(defRoutes) == 0)) {
		p.Noop = true
		p.Steps = []string{fmt.Sprintf("DHCP-Client auf %s ist bereits aktiv (%s) – nichts zu tun", br, existing["status"])}
		return p, nil
	}

	var apply, undo strings.Builder
	// Rückweg zuerst für den Client, dann statische Werte wiederherstellen
	switch {
	case existing == nil:
		p.Steps = append(p.Steps, fmt.Sprintf("DHCP-Client auf %s anlegen (Default-Route + DNS vom DHCP-Server)", br))
		apply.WriteString("/ip dhcp-client add interface=" + q(br) + " disabled=no add-default-route=yes use-peer-dns=yes comment=" + q(mmComment) + "\n")
		apply.WriteString(":local c [/ip dhcp-client find interface=" + q(br) + "]\n")
		undo.WriteString(try("/ip dhcp-client remove [find interface=" + q(br) + " comment=" + q(mmComment) + "]"))
	case existing["disabled"] == "true":
		p.Steps = append(p.Steps, fmt.Sprintf("vorhandenen DHCP-Client auf %s einschalten", br))
		apply.WriteString(":local c " + existing[".id"] + "\n/ip dhcp-client enable numbers=$c\n")
		undo.WriteString(try("/ip dhcp-client disable numbers=" + existing[".id"]))
	default:
		apply.WriteString(":local c " + existing[".id"] + "\n")
	}
	// Erst wenn eine Adresse bezogen ist, werden die statischen Werte entfernt.
	apply.WriteString(":local ok false\n:for i from=1 to=40 do={ :if (!$ok) do={ :if ([/ip dhcp-client get $c status] = \"bound\") do={ :set ok true } else={ :delay 1s } } }\n")
	apply.WriteString(":if ($ok) do={\n")
	if removeStatic {
		for _, a := range statics {
			p.Steps = append(p.Steps, fmt.Sprintf("feste IP %s entfernen (sobald DHCP-Adresse da ist)", a["address"]))
			apply.WriteString("  /ip address remove numbers=" + a[".id"] + "\n")
			undo.WriteString(try("/ip address add address=" + q(a["address"]) + " interface=" + q(br)))
			p.NewIP = true
		}
		for _, r := range defRoutes {
			p.Steps = append(p.Steps, fmt.Sprintf("feste Default-Route über %s entfernen", r["gateway"]))
			apply.WriteString("  /ip route remove numbers=" + r[".id"] + "\n")
			cmd := "/ip route add dst-address=0.0.0.0/0 gateway=" + q(r["gateway"])
			if r["distance"] != "" {
				cmd += " distance=" + r["distance"]
			}
			undo.WriteString(try(cmd))
		}
	}
	apply.WriteString("  :log info \"mikrotik-manager: DHCP-Client gebunden\"\n} else={\n")
	if existing == nil {
		apply.WriteString("  /ip dhcp-client remove $c\n")
	} else if existing["disabled"] == "true" {
		apply.WriteString("  /ip dhcp-client disable numbers=$c\n")
	}
	apply.WriteString("  :log error \"mikrotik-manager: kein DHCP-Lease - feste IP bleibt\"\n}\n")

	if p.NewIP {
		p.Warn = append(p.Warn, "Das Gerät bekommt eine neue IP vom DHCP-Server (außer es gibt dort eine Reservierung). Der Manager findet es per MNDP wieder.")
	}
	if !removeStatic && len(statics) > 0 {
		p.Warn = append(p.Warn, "Die feste IP bleibt zusätzlich bestehen.")
	}
	p.Steps = append(p.Steps, "bekommt das Gerät innerhalb von 40 s keine Adresse, wird nichts entfernt")
	p.Apply, p.Undo = apply.String(), undo.String()
	return p, nil
}

// --- Ausführung mit Sicherheitsnetz ---

// Preview berechnet den Plan, ohne etwas zu ändern.
func (p *Poller) Preview(ctx context.Context, id string, req ChangeReq) (Plan, error) {
	d, ok := p.st.Get(id)
	if !ok {
		return Plan{}, fmt.Errorf("Gerät unbekannt")
	}
	var plan Plan
	err := p.withClient(ctx, d, func(ctx context.Context, cl *routeros.Client) (err error) {
		plan, err = p.planFor(ctx, cl, id, req)
		return err
	})
	return plan, err
}

// planFor plant eine Änderung für ein bestimmtes Gerät (Umstellung und zentrale
// WLAN-Verwaltung brauchen dafür Daten des Managers).
func (p *Poller) planFor(ctx context.Context, cl *routeros.Client, id string, req ChangeReq) (Plan, error) {
	switch req.Action {
	case "wifimigrate":
		return p.planMigrate(ctx, cl, id, true)
	case "central":
		// Prüfen geht auch ohne Aktivierung; Ausrollen prüft die Aktivierung (centralApply).
		c := p.Central()
		if !contains(c.Devices, id) {
			return Plan{Action: "central", Blocked: "Das Gerät ist für die zentrale WLAN-Verwaltung nicht ausgewählt."}, nil
		}
		return planCentral(ctx, cl, c, p.plan2G(c, id))
	case "wlan":
		if p.Managed(id) {
			return Plan{Action: "wlan", Blocked: "Das WLAN dieses Geräts wird zentral verwaltet – Änderungen im Tab „Zentrale WLAN-Verwaltung“."}, nil
		}
	}
	return makePlan(ctx, cl, req)
}

func makePlan(ctx context.Context, cl *routeros.Client, req ChangeReq) (Plan, error) {
	switch req.Action {
	case "bridge":
		return planBridge(ctx, cl)
	case "dhcp":
		return planDHCP(ctx, cl, req.RemoveStatic)
	case "wlan":
		return planWLAN(ctx, cl, req.WLAN, req.Country)
	}
	return Plan{}, fmt.Errorf("unbekannte Aktion %q", req.Action)
}

// StartChange startet Änderungen als Hintergrund-Jobs (nacheinander).
func (p *Poller) StartChange(ids []string, req ChangeReq) []Result {
	fn := func(ctx context.Context, id string, x *Job) error { return p.doChange(ctx, id, x, req) }
	switch req.Action {
	case "bridge", "dhcp", "wlan", "central":
	case "wifimigrate":
		fn = p.doMigrate
	default:
		return []Result{{Msg: "unbekannte Aktion"}}
	}
	return p.startJobs(p.ctx, ids, req.Action, fn)
}

func (p *Poller) doChange(ctx context.Context, id string, x *Job, req ChangeReq) error {
	action := req.Action
	d, _ := p.st.Get(id)
	name := d.Identity
	oldIP := d.IP
	cl, err := p.session(ctx, id)
	if err != nil {
		return err
	}
	defer func() { cl.Close() }()

	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	plan, err := p.planFor(rctx, cl, id, req)
	cancel()
	if err != nil {
		return err
	}
	if plan.Blocked != "" {
		return fmt.Errorf("nicht ausgeführt: %s", plan.Blocked)
	}
	if plan.Noop {
		p.jobs.step(x, name, "%s", strings.Join(plan.Steps, "; "))
		return nil
	}

	p.jobs.step(x, name, "erstelle Backup und lade es auf den Manager …")
	bk, err := p.BackupToManager(ctx, id, cl, "vor-"+action)
	if err != nil {
		return fmt.Errorf("Backup: %w – nichts geändert", err)
	}
	p.jobs.step(x, name, "Backup gespeichert: %s", bk)

	tag := time.Now().Format("150405")
	// Zentrale WLAN-Verwaltung baut alles neu auf – Rückweg ist ein Backup auf dem Gerät.
	undoFile := ""
	if action == "central" {
		rctx, cancel = context.WithTimeout(ctx, 90*time.Second)
		undoFile, err = saveBackup(rctx, cl, "mm-undo-"+tag)
		cancel()
		if err != nil {
			return fmt.Errorf("Rückweg-Backup auf dem Gerät: %w – nichts geändert", err)
		}
		plan.Undo = "/system backup load name=" + q(undoFile) + " password=\"\"\n"
	}
	rctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	err = installScripts(rctx, cl, plan, tag)
	cancel()
	if err != nil {
		return fmt.Errorf("Skripte anlegen: %w – nichts geändert", err)
	}
	p.jobs.step(x, name, "Rückroll-Wächter aktiv (%s), führe aus: %s", rollbackAfter, strings.Join(plan.Steps, "; "))
	rctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	_, err = cl.RunContext(rctx, "/execute", "=script=/system script run mm-apply-"+tag)
	cancel()
	if err != nil {
		return fmt.Errorf("Ausführen: %w (Wächter rollt ggf. zurück)", err)
	}

	// Neue Adresse möglichst noch über die alte Verbindung erfahren
	newIP := ""
	if plan.NewIP {
		newIP = p.watchDHCPAddress(ctx, cl, plan.Bridge)
		if newIP != "" {
			p.jobs.step(x, name, "DHCP-Adresse bezogen: %s", newIP)
		}
	} else {
		switch p.waitApplied(ctx, cl, tag, 90*time.Second) {
		case "failed":
			p.jobs.step(x, name, "das Gerät meldet einen Fehler beim Ausführen")
		case "":
			time.Sleep(8 * time.Second) // Verbindung weg oder keine Meldung
		}
		time.Sleep(3 * time.Second) // Funkteile/Ports kurz anlaufen lassen
	}
	cl.Close()

	// Wieder verbinden: neue IP, alte IP, was MNDP meldet
	p.jobs.step(x, name, "verbinde neu …")
	ncl, ip, err := p.reconnect(ctx, id, d.SoftwareID, []string{newIP, oldIP}, 3*time.Minute)
	if err != nil {
		p.jobs.step(x, name, "Gerät nicht wieder erreichbar – das Gerät rollt nach Ablauf des Wächters selbst zurück")
		return fmt.Errorf("nicht bestätigt: %v", err)
	}
	cl = ncl
	if ip != oldIP {
		p.st.Update(id, func(x *Device) { x.IP = ip })
	}

	// Prüfen: Ein erneuter Plan muss „nichts zu tun“ ergeben.
	rctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	if action == "wlan" {
		req.WLAN = plan.wlan // Auswahl per SSID: nach Umbenennen dieselben Schnittstellen prüfen
	}
	check, err := p.planFor(rctx, cl, id, req)
	cancel()
	if err != nil {
		return fmt.Errorf("Prüfung: %w (Wächter rollt zurück)", err)
	}
	if !check.Noop {
		// Gerät ist erreichbar → gleich zurückrollen statt 5 Minuten auf den Wächter zu warten.
		p.jobs.step(x, name, "Änderung nicht vollständig wirksam (%s) – rolle sofort zurück …", strings.Join(check.Steps, "; "))
		uerr := runIgnoringDisconnect(ctx, cl, time.Minute, "/system/script/run", "=number=mm-undo-"+tag)
		if uerr == nil && undoFile != "" {
			// Backup wird geladen, das Gerät startet neu
			return fmt.Errorf("Änderung nicht vollständig wirksam – das Gerät lädt das Backup von vorher und startet neu")
		}
		if uerr == nil {
			rctx, cancel = context.WithTimeout(ctx, 30*time.Second)
			uerr = removeScripts(rctx, cl, tag)
			cancel()
		}
		if uerr != nil {
			return fmt.Errorf("Änderung nicht vollständig wirksam, sofortiges Zurückrollen fehlgeschlagen (%v) – Wächter rollt zurück", uerr)
		}
		return fmt.Errorf("Änderung nicht vollständig wirksam – alter Zustand wiederhergestellt (vermutlich lehnt das Gerät einen Wert ab)")
	}
	if action == "dhcp" {
		rctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		l, err := printAll(rctx, cl, "/ip/dhcp-client/print", "=.proplist=interface,status,address")
		cancel()
		bound := ""
		for _, c := range l {
			if c["interface"] == plan.Bridge && c["status"] == "bound" {
				bound = c["address"]
			}
		}
		if err != nil || bound == "" {
			return fmt.Errorf("DHCP-Client nicht gebunden – Wächter stellt den alten Zustand her")
		}
		p.jobs.step(x, name, "DHCP-Client gebunden: %s", bound)
	}

	// Bestätigen = Rückweg löschen
	rctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	err = removeScripts(rctx, cl, tag)
	cancel()
	if err == nil && undoFile != "" {
		removeDeviceFile(ctx, cl, undoFile)
	}
	if err != nil {
		return fmt.Errorf("Bestätigen fehlgeschlagen (%v) – Wächter rollt zurück", err)
	}
	p.jobs.step(x, name, "bestätigt, Rückroll-Wächter entschärft – erreichbar unter %s", ip)
	p.Trigger(id)
	return nil
}

// installScripts legt mm-undo-<tag>/mm-apply-<tag> an und startet den Wächter.
// Der Tag trennt die Skripte aufeinanderfolgender Änderungen: Der Wächter einer
// früheren Änderung darf nie den Rückweg einer späteren ausführen.
func installScripts(ctx context.Context, cl *routeros.Client, plan Plan, tag string) error {
	l, err := printAll(ctx, cl, "/system/script/print", "=.proplist=.id,name")
	if err != nil {
		return err
	}
	for _, s := range l {
		if strings.HasPrefix(s["name"], "mm-undo") {
			return fmt.Errorf("eine frühere Änderung ist noch nicht bestätigt (%s) – ihr Wächter stellt gerade den alten Zustand her; in 5 Minuten erneut versuchen", s["name"])
		}
		if strings.HasPrefix(s["name"], "mm-apply") {
			cl.RunContext(ctx, "/system/script/remove", "=.id="+s[".id"])
		}
	}
	undoName, applyName := "mm-undo-"+tag, "mm-apply-"+tag
	// Fertig- bzw. Fehlermeldung im Log, damit der Manager nicht raten muss (waitApplied)
	// Ergebnis zusätzlich in einer globalen Variable: das Log ist auf manchen
	// Geräten leer bzw. nicht per API lesbar.
	apply := ":global mmApplied \"\"\n:do {\n" + plan.Apply + "\n:set mmApplied \"done " + tag + "\"\n:log info \"mm-apply-done " + tag + "\"\n} on-error={ :set mmApplied \"failed " + tag + "\"; :log error \"mm-apply-failed " + tag + "\" }\n"
	for _, s := range []struct{ name, src string }{{undoName, plan.Undo}, {applyName, apply}} {
		if _, err := cl.RunContext(ctx, "/system/script/add", "=name="+s.name, "=source="+s.src,
			"=comment="+mmComment, "=policy=read,write,policy,test,reboot,sensitive,ftp,password,sniff,romon"); err != nil {
			return err
		}
	}
	// Wächter: läuft im Hintergrund weiter, auch wenn die API-Verbindung abbricht.
	watch := fmt.Sprintf(":delay %ds\n:if ([:len [/system script find name=\"%[2]s\"]] > 0) do={ /system script run %[2]s; /system script remove [find name=\"%[2]s\"]; /system script remove [find name=\"%[3]s\"]; :log warning \"mikrotik-manager: nicht bestaetigt - Aenderung zurueckgerollt\" }",
		int(rollbackAfter.Seconds()), undoName, applyName)
	_, err = cl.RunContext(ctx, "/execute", "=script="+watch)
	return err
}

// waitApplied wartet auf das Ergebnis des Apply-Skripts (globale Variable
// mmApplied = "done <tag>" | "failed <tag>"): "done", "failed" oder ""
// (kein Ergebnis, z. B. Verbindung weg).
func (p *Poller) waitApplied(ctx context.Context, cl *routeros.Client, tag string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		l, err := printAll(rctx, cl, "/system/script/environment/print", "?name=mmApplied")
		cancel()
		if err != nil {
			return ""
		}
		for _, m := range l {
			if st, t, _ := strings.Cut(m["value"], " "); t == tag && (st == "done" || st == "failed") {
				return st
			}
		}
		time.Sleep(time.Second)
	}
	return ""
}

// removeScripts bestätigt eine Änderung: Ohne mm-undo-<tag> tut ihr Wächter nichts.
func removeScripts(ctx context.Context, cl *routeros.Client, tag string) error {
	l, err := printAll(ctx, cl, "/system/script/print", "=.proplist=.id,name")
	if err != nil {
		return err
	}
	for _, s := range l {
		if s["name"] == "mm-undo-"+tag || s["name"] == "mm-apply-"+tag {
			if _, err := cl.RunContext(ctx, "/system/script/remove", "=.id="+s[".id"]); err != nil {
				return err
			}
		}
	}
	return nil
}

// watchDHCPAddress fragt bis zu 60 s über die noch offene Verbindung, welche
// Adresse der DHCP-Client bekommen hat (die Verbindung reißt ab, sobald die
// feste IP entfernt wird).
func (p *Poller) watchDHCPAddress(ctx context.Context, cl *routeros.Client, bridge string) string {
	for i := 0; i < 60; i++ {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		l, err := printAll(rctx, cl, "/ip/dhcp-client/print", "=.proplist=interface,status,address")
		cancel()
		if err != nil {
			return ""
		}
		for _, c := range l {
			if c["interface"] == bridge && c["status"] == "bound" && c["address"] != "" {
				ip, _, _ := net.ParseCIDR(c["address"])
				if ip != nil {
					return ip.String()
				}
			}
		}
		time.Sleep(time.Second)
	}
	return ""
}

// reconnect versucht die Kandidaten-IPs und die per MNDP gemeldete IP, bis
// sich das Gerät mit derselben Software-ID meldet.
func (p *Poller) reconnect(ctx context.Context, id, swid string, ips []string, timeout time.Duration) (*routeros.Client, string, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if p.rediscover != nil {
			p.rediscover()
		}
		cand := append([]string{}, ips...)
		if d, ok := p.st.Get(id); ok {
			cand = append(cand, d.IP)
		}
		seen := map[string]bool{}
		for _, ip := range cand {
			if ip == "" || seen[ip] {
				continue
			}
			seen[ip] = true
			c, _ := p.vault.For(id)
			cl, err := p.dial(ctx, ip, c)
			if err != nil {
				last = err
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			m, err := printAll(rctx, cl, "/system/license/print")
			cancel()
			if err == nil && len(m) > 0 && (swid == "" || m[0]["software-id"] == swid || m[0]["system-id"] == swid) {
				return cl, ip, nil
			}
			cl.Close()
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	if last == nil {
		last = fmt.Errorf("Zeitüberschreitung")
	}
	return nil, "", last
}
