package main

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-routeros/routeros/v3"
)

// Feste IP (Adresse, Subnetz, Gateway, DNS) oder zurück auf DHCP, gedacht für
// Access Points und Switches: eine Schnittstelle mit IP, kein DHCP-Server.
//
// Rückfall auf DHCP, zweistufig:
//   - Während der Änderung gilt der Wächter wie bei jeder Änderung (settings.go):
//     Erreicht der Manager das Gerät nicht mehr, stellt es den alten Zustand her.
//   - Danach prüft ein Zeitplan auf dem Gerät jede Minute das Gateway. Bleibt es
//     ipFallbackRuns Minuten stumm (Gerät umgezogen, Netz geändert), schaltet er den
//     DHCP-Client ein, die feste IP bleibt daneben stehen. Antwortet das Gateway
//     wieder, geht der DHCP-Client aus. Ein Gateway, das Ping nicht beantwortet,
//     führt also nur zu einer zweiten Adresse, nicht zum Ausfall.
//
// Der Device-Mode home sperrt den Zeitplan; dann gibt es nur den Wächter.

const (
	ipFallbackName = "mm-ip-fallback"
	ipFallbackRuns = 5
	ipPolicy       = "read,write,test"
)

// ipReq ist eine geprüfte Eingabe für die feste IP.
type ipReq struct {
	addr netip.Prefix // Adresse/Präfix
	gw   netip.Addr
	dns  []string
}

var (
	ipSepRe    = regexp.MustCompile(`[,;\s]+`)
	ipIfaceOK  = map[string]bool{"ether": true, "bridge": true, "vlan": true, "bond": true}
	ipMaskBits = func() map[string]int {
		m := map[string]int{}
		for b := 0; b <= 32; b++ {
			v := uint32(0)
			if b > 0 {
				v = ^uint32(0) << (32 - b)
			}
			m[netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}).String()] = b
		}
		return m
	}()
)

func parseIPReq(r ChangeReq) (ipReq, error) {
	var out ipReq
	ip, err := netip.ParseAddr(strings.TrimSpace(r.IPAddress))
	if err != nil || !ip.Is4() {
		return out, fmt.Errorf("IP-Adresse ungültig")
	}
	m := strings.TrimPrefix(strings.TrimSpace(r.IPMask), "/")
	bits, ok := ipMaskBits[m]
	if !ok {
		if bits, err = strconv.Atoi(m); err != nil || bits > 32 {
			return out, fmt.Errorf("Subnetz ungültig: Maske wie 255.255.255.0 oder Präfix wie 24")
		}
	}
	if bits < 8 || bits > 30 {
		return out, fmt.Errorf("Subnetz muss zwischen /8 und /30 liegen")
	}
	pfx := netip.PrefixFrom(ip, bits)
	v := ip.As4()
	n := uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
	host := n &^ (^uint32(0) << (32 - bits))
	if host == 0 || host == ^(^uint32(0)<<(32-bits)) {
		return out, fmt.Errorf("%s ist die Netz- oder Broadcast-Adresse", ip)
	}
	gw, err := netip.ParseAddr(strings.TrimSpace(r.IPGateway))
	if err != nil || !gw.Is4() {
		return out, fmt.Errorf("Gateway ungültig")
	}
	if gw == ip || !pfx.Masked().Contains(gw) {
		return out, fmt.Errorf("Gateway muss im Subnetz liegen und darf nicht die Geräteadresse sein")
	}
	for _, s := range ipSepRe.Split(strings.TrimSpace(r.IPDNS), -1) {
		if s == "" {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() {
			return out, fmt.Errorf("DNS-Server %q ungültig", s)
		}
		out.dns = append(out.dns, a.String())
	}
	if len(out.dns) == 0 {
		return out, fmt.Errorf("mindestens ein DNS-Server nötig")
	}
	out.addr, out.gw = pfx, gw
	return out, nil
}

// ipState ist der Ist-Zustand der Verwaltungs-Schnittstelle.
type ipState struct {
	Iface       string
	Statics     []map[string]string // feste Adressen dort
	Routes      []map[string]string // feste Default-Routen (Tabelle main)
	DHCP        map[string]string   // DHCP-Client dort, nil = keiner
	DNS         string
	FallbackSrc string // Skript des Zeitplans, "" = keiner
	CurOnIface  bool   // Der Manager erreicht das Gerät über diese Schnittstelle
	NoScheduler bool   // Device-Mode sperrt Zeitpläne
}

// chooseIPIface wählt die Schnittstelle mit IP oder DHCP-Client. types: Name → Typ.
// Tunnel und Mobilfunk zählen nicht. Mehrere = Router, das entscheidet der Manager nicht.
func chooseIPIface(addrs, dcs []map[string]string, types map[string]string) (string, string) {
	var cand []string
	add := func(n string) {
		if ipIfaceOK[types[n]] && !contains(cand, n) {
			cand = append(cand, n)
		}
	}
	for _, a := range addrs {
		if a["disabled"] != "true" {
			add(a["interface"])
		}
	}
	for _, c := range dcs {
		if c["disabled"] != "true" {
			add(c["interface"])
		}
	}
	switch len(cand) {
	case 0:
		return "", "Keine Schnittstelle mit IP gefunden – zuerst „DHCP-Client auf der Bridge“ einrichten."
	case 1:
		return cand[0], ""
	}
	return "", "Das Gerät hat IPs auf " + strings.Join(cand, ", ") + " (Router?). Hier nicht automatisch ändern."
}

func planIPConf(ctx context.Context, cl *routeros.Client, curIP string, req ChangeReq) (Plan, error) {
	p := Plan{Action: "ipconf"}
	var r ipReq
	if req.IPMode != "dhcp" {
		var err error
		if req.IPMode != "static" {
			return p, fmt.Errorf("IP-Modus %q unbekannt", req.IPMode)
		}
		if r, err = parseIPReq(req); err != nil {
			p.Blocked = err.Error()
			return p, nil
		}
	}
	read := func(cmd string, args ...string) []map[string]string {
		if p.Blocked != "" {
			return nil
		}
		l, err := printAll(ctx, cl, cmd, args...)
		if err != nil {
			p.Blocked = cmd + ": " + err.Error()
		}
		return l
	}
	ifs := read("/interface/print", "=.proplist=name,type")
	addrs := read("/ip/address/print", "=.proplist=.id,address,interface,dynamic,disabled,comment")
	dcs := read("/ip/dhcp-client/print", "=.proplist=.id,interface,disabled,status")
	srv := read("/ip/dhcp-server/print", "=.proplist=name,disabled")
	routes := read("/ip/route/print", "=.proplist=.id,dst-address,gateway,distance,dynamic,routing-table,comment")
	dns := read("/ip/dns/print", "=.proplist=servers")
	if p.Blocked != "" {
		return p, nil
	}
	for _, s := range srv {
		if s["disabled"] != "true" {
			p.Blocked = "Auf dem Gerät läuft ein DHCP-Server (Router). Hier nicht automatisch ändern."
			return p, nil
		}
	}
	types := map[string]string{}
	for _, i := range ifs {
		types[i["name"]] = i["type"]
	}
	st := ipState{}
	var why string
	if st.Iface, why = chooseIPIface(addrs, dcs, types); why != "" {
		p.Blocked = why
		return p, nil
	}
	for _, a := range addrs {
		if a["interface"] != st.Iface {
			continue
		}
		if ip, _, _ := strings.Cut(a["address"], "/"); ip == curIP {
			st.CurOnIface = true
		}
		if a["dynamic"] != "true" {
			st.Statics = append(st.Statics, a)
		}
	}
	for _, c := range dcs {
		if c["interface"] == st.Iface {
			st.DHCP = c
		}
	}
	for _, x := range routes {
		if x["dst-address"] == "0.0.0.0/0" && x["dynamic"] != "true" && (x["routing-table"] == "" || x["routing-table"] == "main") {
			st.Routes = append(st.Routes, x)
		}
	}
	if len(dns) > 0 {
		st.DNS = dns[0]["servers"]
	}
	if l, err := printAll(ctx, cl, "/system/script/print", "?name="+ipFallbackName, "=.proplist=source"); err == nil && len(l) > 0 {
		// RouterOS kann Zeilenenden umschreiben; Vergleich mit ipFallbackSource.
		st.FallbackSrc = strings.TrimSpace(strings.ReplaceAll(l[0]["source"], "\r", ""))
	}
	// Ältere RouterOS kennen das Menü nicht; dann ist auch nichts gesperrt.
	if l, err := printAll(ctx, cl, "/system/device-mode/print"); err == nil && len(l) > 0 {
		s := l[0]["scheduler"]
		st.NoScheduler = s == "no" || s == "false" || (s == "" && l[0]["mode"] == "home")
	}
	if req.IPMode == "dhcp" {
		return buildIPDhcpPlan(st), nil
	}
	return buildIPStaticPlan(st, r), nil
}

// ipFallbackSource prüft das Gateway; Zähler in einer globalen Variable (ein
// Neustart beginnt von vorn, das schadet nicht).
func ipFallbackSource(iface, gw string) string {
	return strings.TrimSpace(fmt.Sprintf(`:global mmGwFail
:if ([:typeof $mmGwFail] != "num") do={ :set mmGwFail 0 }
:local c [/ip dhcp-client find interface=%[1]s]
:if ([/ping %[2]s count=3] > 0) do={
  :set mmGwFail 0
  :if (![/ip dhcp-client get $c disabled]) do={
    /ip dhcp-client disable $c
    :log info "mikrotik-manager: Gateway antwortet wieder, DHCP-Client aus"
  }
} else={
  :set mmGwFail ($mmGwFail + 1)
  :if (($mmGwFail >= %[3]d) && [/ip dhcp-client get $c disabled]) do={
    /ip dhcp-client enable $c
    :log warning "mikrotik-manager: Gateway stumm, DHCP-Client ein"
  }
}
`, q(iface), gw, ipFallbackRuns))
}

func ipFallbackRemove() string {
	n := q(ipFallbackName)
	return "/system scheduler remove [find name=" + n + "]\n/system script remove [find name=" + n + "]\n"
}

func ipFallbackAdd(src string) string {
	n, c := q(ipFallbackName), q(mmComment)
	return "/system script add name=" + n + " comment=" + c + " policy=" + ipPolicy + " source=" + rsrc(src) + "\n" +
		"/system scheduler add name=" + n + " comment=" + c + " policy=" + ipPolicy + " start-time=startup interval=1m on-event=" + n + "\n"
}

// rsrc setzt ein Skript als Zeichenkette in ein anderes Skript.
func rsrc(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\n", `\n`).Replace(s) + `"`
}

// ipRestore stellt alte Adressen, Routen und den Zeitplan wieder her (Rückweg).
func ipRestore(undo *strings.Builder, st ipState, statics, routes []map[string]string) {
	for _, a := range statics {
		cmd := "/ip address add address=" + q(a["address"]) + " interface=" + q(st.Iface)
		if a["comment"] != "" {
			cmd += " comment=" + q(a["comment"])
		}
		// Der Rückweg läuft auch, wenn das Apply-Skript vorher abbrach: Dann ist noch alles da.
		undo.WriteString(":if ([:len [/ip address find address=" + q(a["address"]) + " interface=" + q(st.Iface) + "]] = 0) do={\n" + try(cmd) + "}\n")
	}
	for _, r := range routes {
		cmd := "/ip route add dst-address=0.0.0.0/0 gateway=" + q(r["gateway"])
		if r["distance"] != "" {
			cmd += " distance=" + r["distance"]
		}
		if r["comment"] != "" {
			cmd += " comment=" + q(r["comment"])
		}
		undo.WriteString(":if ([:len [/ip route find dst-address=0.0.0.0/0 gateway=" + q(r["gateway"]) + " dynamic=no]] = 0) do={\n" + try(cmd) + "}\n")
	}
}

func buildIPStaticPlan(st ipState, r ipReq) Plan {
	p := Plan{Action: "ipconf", Bridge: st.Iface}
	addr, gw, dns := r.addr.String(), r.gw.String(), strings.Join(r.dns, ",")
	var keepAddr bool
	var dropA, dropR []map[string]string
	for _, a := range st.Statics {
		if a["address"] == addr {
			keepAddr = true
		} else {
			dropA = append(dropA, a)
		}
	}
	var keepRoute bool
	for _, x := range st.Routes {
		if x["gateway"] == gw {
			keepRoute = true
		} else {
			dropR = append(dropR, x)
		}
	}
	dhcpOn := st.DHCP != nil && st.DHCP["disabled"] != "true"
	wantSrc := ipFallbackSource(st.Iface, gw)
	if st.NoScheduler {
		p.Warn = append(p.Warn, "Device-Mode home sperrt Zeitpläne: Es gibt keinen dauerhaften Rückfall auf DHCP. Während der Änderung schützt der Wächter trotzdem.")
	}
	fbOK := st.NoScheduler || (st.FallbackSrc == wantSrc && st.DHCP != nil)
	if keepAddr && keepRoute && len(dropA) == 0 && len(dropR) == 0 && !dhcpOn && st.DNS == dns && fbOK {
		p.Noop = true
		p.Steps = []string{fmt.Sprintf("%s hat bereits %s, Gateway %s, DNS %s – nichts zu tun", st.Iface, addr, gw, dns)}
		return p
	}

	var apply, undo strings.Builder
	if !fbOK {
		undo.WriteString(try(strings.ReplaceAll(strings.TrimSpace(ipFallbackRemove()), "\n", "; ")))
	}
	// Rückweg in umgekehrter Reihenfolge: erst das Neue weg, dann das Alte her.
	if !keepRoute {
		undo.WriteString(try("/ip route remove [find dst-address=0.0.0.0/0 gateway=" + q(gw) + "]"))
	}
	if !keepAddr {
		undo.WriteString(try("/ip address remove [find address=" + q(addr) + " interface=" + q(st.Iface) + "]"))
	}
	ipRestore(&undo, st, dropA, dropR)
	dc := "[find interface=" + q(st.Iface) + "]"
	switch {
	case st.DHCP == nil:
		// Der Rückfall braucht einen Client; er bleibt ausgeschaltet.
		if !st.NoScheduler {
			p.Steps = append(p.Steps, "DHCP-Client auf "+st.Iface+" ausgeschaltet anlegen (für den Rückfall)")
			apply.WriteString("/ip dhcp-client add interface=" + q(st.Iface) + " disabled=yes add-default-route=yes use-peer-dns=yes comment=" + q(mmComment) + "\n")
			undo.WriteString(try("/ip dhcp-client remove " + dc))
		}
	case dhcpOn:
		p.Steps = append(p.Steps, "DHCP-Client auf "+st.Iface+" ausschalten")
		apply.WriteString("/ip dhcp-client disable " + dc + "\n")
		undo.WriteString(try("/ip dhcp-client enable " + dc))
	}
	for _, a := range dropA {
		p.Steps = append(p.Steps, "feste IP "+a["address"]+" entfernen")
		apply.WriteString("/ip address remove numbers=" + a[".id"] + "\n")
	}
	for _, x := range dropR {
		p.Steps = append(p.Steps, "Default-Route über "+x["gateway"]+" entfernen")
		apply.WriteString("/ip route remove numbers=" + x[".id"] + "\n")
	}
	if !keepAddr {
		p.Steps = append(p.Steps, fmt.Sprintf("feste IP %s auf %s setzen", addr, st.Iface))
		apply.WriteString("/ip address add address=" + q(addr) + " interface=" + q(st.Iface) + " comment=" + q(mmComment) + "\n")
	}
	if !keepRoute {
		p.Steps = append(p.Steps, "Default-Route über "+gw)
		apply.WriteString("/ip route add dst-address=0.0.0.0/0 gateway=" + q(gw) + " comment=" + q(mmComment) + "\n")
	}
	if st.DNS != dns {
		p.Steps = append(p.Steps, "DNS: "+dns)
		apply.WriteString("/ip dns set servers=" + q(dns) + "\n")
		undo.WriteString(try("/ip dns set servers=" + q(st.DNS)))
	}
	if !fbOK {
		p.Steps = append(p.Steps, fmt.Sprintf("Rückfall einrichten: Gateway %d Minuten stumm → DHCP-Client ein, antwortet es wieder → aus", ipFallbackRuns))
		apply.WriteString(":do {\n" + ipFallbackRemove() + "} on-error={}\n" + ipFallbackAdd(wantSrc))
	}
	if !fbOK && st.FallbackSrc != "" {
		undo.WriteString(try(strings.ReplaceAll(strings.TrimSpace(ipFallbackAdd(st.FallbackSrc)), "\n", "; ")))
	}
	if st.CurOnIface && !keepAddr {
		p.NewAddr = r.addr.Addr().String()
		p.Warn = append(p.Warn, "Der Manager verbindet sich danach neu mit "+p.NewAddr+". Klappt das nicht, stellt der Wächter nach 5 Minuten den alten Zustand wieder her.")
	}
	p.Apply, p.Undo = apply.String(), undo.String()
	return p
}

func buildIPDhcpPlan(st ipState) Plan {
	p := Plan{Action: "ipconf", Bridge: st.Iface}
	dhcpOn := st.DHCP != nil && st.DHCP["disabled"] != "true"
	if dhcpOn && len(st.Statics) == 0 && len(st.Routes) == 0 && st.FallbackSrc == "" {
		p.Noop = true
		p.Steps = []string{"DHCP-Client auf " + st.Iface + " ist aktiv, keine feste IP – nichts zu tun"}
		return p
	}
	var apply, undo strings.Builder
	// Zuerst: Der Zeitplan schaltet den Client wieder aus, solange das Gateway antwortet.
	if st.FallbackSrc != "" {
		p.Steps = append(p.Steps, "Rückfall-Zeitplan entfernen")
		apply.WriteString(":do {\n" + ipFallbackRemove() + "} on-error={}\n")
		undo.WriteString(try(strings.ReplaceAll(strings.TrimSpace(ipFallbackRemove()), "\n", "; ")))
		undo.WriteString(try(strings.ReplaceAll(strings.TrimSpace(ipFallbackAdd(st.FallbackSrc)), "\n", "; ")))
	}
	dc := "[find interface=" + q(st.Iface) + "]"
	switch {
	case st.DHCP == nil:
		p.Steps = append(p.Steps, "DHCP-Client auf "+st.Iface+" anlegen")
		apply.WriteString("/ip dhcp-client add interface=" + q(st.Iface) + " disabled=no add-default-route=yes use-peer-dns=yes comment=" + q(mmComment) + "\n")
		undo.WriteString(try("/ip dhcp-client remove " + dc))
	case !dhcpOn:
		p.Steps = append(p.Steps, "DHCP-Client auf "+st.Iface+" einschalten")
		apply.WriteString("/ip dhcp-client enable " + dc + "\n")
		undo.WriteString(try("/ip dhcp-client disable " + dc))
	}
	// Ohne Lease bricht das Skript ab, und doChange rollt sofort zurück.
	apply.WriteString(":local c [/ip dhcp-client find interface=" + q(st.Iface) + "]\n:local ok false\n")
	apply.WriteString(":for i from=1 to=40 do={ :if (!$ok) do={ :if ([/ip dhcp-client get $c status] = \"bound\") do={ :set ok true } else={ :delay 1s } } }\n")
	apply.WriteString(":if (!$ok) do={ :error \"kein DHCP-Lease\" }\n")
	for _, a := range st.Statics {
		p.Steps = append(p.Steps, "feste IP "+a["address"]+" entfernen (sobald eine DHCP-Adresse da ist)")
		apply.WriteString("/ip address remove numbers=" + a[".id"] + "\n")
	}
	for _, x := range st.Routes {
		p.Steps = append(p.Steps, "feste Default-Route über "+x["gateway"]+" entfernen")
		apply.WriteString("/ip route remove numbers=" + x[".id"] + "\n")
	}
	ipRestore(&undo, st, st.Statics, st.Routes)
	if st.CurOnIface && len(st.Statics) > 0 {
		p.NewIP = true
		p.Warn = append(p.Warn, "Das Gerät bekommt seine IP vom DHCP-Server. Der Manager findet es danach per MNDP wieder.")
	}
	p.Steps = append(p.Steps, "bekommt das Gerät innerhalb von 40 s keine Adresse, wird alles zurückgenommen")
	p.Apply, p.Undo = apply.String(), undo.String()
	return p
}
