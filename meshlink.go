package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/go-routeros/routeros/v3"
)

// Meshlink: Ein AP ohne Etherlink hängt sich automatisch per WLAN an einen AP
// mit Etherlink und reicht dessen Netz (alle VLANs) weiter. Teil der zentralen
// WLAN-Verwaltung; nur für das wifi-Paket (wifi-qcom / wifi-qcom-ac).
//
// Jeder verwaltete AP bekommt auf seinem 5-GHz-Funkteil
//   - mm-mesh-ap:  verstecktes WLAN, das Knoten annimmt (aktiv, solange ether1 Link hat)
//   - mm-mesh-sta: Station im Modus station-bridge (aktiv, solange ether1 KEINEN Link hat)
// und ein Wächter-Skript, das alle 30 s umschaltet (entprellt über 20 s).
//
// Getestet am 2026-11-03 (cAP ax ↔ wAP ac, beide Richtungen): siehe docs/entscheidungen.md.
// Wichtig: Die Station ist virtuell und kann den Kanal nicht wechseln – alle APs
// eines Standorts brauchen denselben FESTEN 5-GHz-Kanal.

const (
	meshAP      = "mm-mesh-ap"
	meshSTA     = "mm-mesh-sta"
	meshComment = "mm-meshlink"
	meshScript  = "mm-meshlink"
	meshUplink  = "ether1"
)

// CentralMeshlink ist die Meshlink-Einstellung der zentralen WLAN-Konfiguration.
type CentralMeshlink struct {
	Enabled    bool   `json:"enabled"`
	SSID       string `json:"ssid,omitempty"`       // vom Manager erzeugt, versteckt
	Passphrase string `json:"passphrase,omitempty"` // vom Manager erzeugt
}

// meshWatch ist das Wächter-Skript auf dem Gerät (nur ASCII – RouterOS gibt
// Umlaute in anderer Kodierung zurück). Globale Variablen überleben Läufe aus
// dem Zeitplan nicht, deshalb wird im selben Lauf per :delay nachgeprüft. Nur der Weg zum
// Meshlink wartet: Sind Kabel und Station zugleich aktiv, ist das eine Schleife, und Loop
// Protect sperrt den Port nach Sekunden. Der Rückweg muss also sofort greifen.
const meshWatch = `# mm-meshlink (vom mikrotik-manager): ohne Etherlink ueber die Meshlink-Station
:local sta [/interface wifi find name="mm-mesh-sta"]
:local ap [/interface wifi find name="mm-mesh-ap"]
:local eth [/interface find name="ether1"]
:if ([:len $sta] = 0 || [:len $ap] = 0 || [:len $eth] = 0) do={ :error "mm-meshlink: mm-mesh-sta/mm-mesh-ap/ether1 fehlt" }
:local staOff [/interface wifi get $sta disabled]
:local link [/interface get $eth running]
:if ($link != $staOff) do={
  :if ($link) do={
    /interface wifi set $sta disabled=yes
    /interface wifi set $ap disabled=no
    :log warning "mm-meshlink: Etherlink wieder da - Meshlink-Station aus, Meshlink-Angebot an"
  } else={
    :delay 10s
    :local same ([/interface get $eth running] = $link)
    :if ($same) do={
      :delay 10s
      :set same ([/interface get $eth running] = $link)
    }
    :if ($same) do={
      /interface wifi set $ap disabled=yes
      /interface wifi set $sta disabled=no
      :log warning "mm-meshlink: kein Etherlink - Meshlink-Station an, Meshlink-Angebot aus"
    }
  }
}
`

// meshState: was auf dem Gerät zu Meshlink gehört (Skript, Zeitplan, ether1).
type meshState struct {
	read       bool
	script     string // Quelltext von mm-meshlink, "" = fehlt
	scheduler  bool
	hasUplink  bool // ether1 vorhanden
	uplinkLink bool // ether1 hat Link
}

func readMeshState(ctx context.Context, cl *routeros.Client) meshState {
	m := meshState{read: true}
	if l, err := printAll(ctx, cl, "/system/script/print", "?name="+meshScript, "=.proplist=source"); err == nil && len(l) > 0 {
		m.script = l[0]["source"]
	}
	if l, err := printAll(ctx, cl, "/system/scheduler/print", "?name="+meshScript, "=.proplist=name"); err == nil {
		m.scheduler = len(l) > 0
	}
	if l, err := printAll(ctx, cl, "/interface/print", "?name="+meshUplink, "=.proplist=running"); err == nil && len(l) > 0 {
		m.hasUplink, m.uplinkLink = true, l[0]["running"] == "true"
	}
	return m
}

func isMesh(name string) bool { return name == meshAP || name == meshSTA }

// fillMeshlink erzeugt SSID und Passwort, wenn Meshlink eingeschaltet wird.
func fillMeshlink(m *CentralMeshlink) {
	if !m.Enabled {
		return
	}
	if m.SSID == "" {
		m.SSID = "mm-mesh-" + randString(6, "abcdefghijkmnpqrstuvwxyz23456789")
	}
	if m.Passphrase == "" {
		m.Passphrase = randString(32, "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789")
	}
}

func randString(n int, alphabet string) string {
	b := make([]byte, n)
	for i := range b {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[k.Int64()]
	}
	return string(b)
}

// meshRadio liefert das 5-GHz-Funkteil der zentralen Konfiguration (nil = keins).
func meshRadio(radios []*centralRadio) *centralRadio {
	for _, cr := range radios {
		if cr.is5 {
			return cr
		}
	}
	return nil
}

// meshTagged: Ports, über die die VLANs getaggt laufen – mit Meshlink auch dessen Schnittstellen.
func meshTagged(ethers []string, meshOn bool) []string {
	out := append([]string{}, ethers...)
	if meshOn {
		out = append(out, meshAP, meshSTA)
	}
	sort.Strings(out)
	return out
}

// planMeshlink prüft Meshlink gegen das Gerät. on = Meshlink wird auf diesem
// Gerät ausgerollt (sonst ggf. entfernt). blocked != "" verhindert das Ausrollen.
func planMeshlink(s *wlanState, bi bridgeInfo, c CentralWLAN, radios []*centralRadio) (on bool, diffs, warn []string, blocked string) {
	ml := c.Meshlink
	ms := s.mesh
	var ap, sta *wlanIfc
	for _, x := range s.ifs {
		switch x.Name {
		case meshAP:
			ap = x
		case meshSTA:
			sta = x
		}
	}
	present := ap != nil || sta != nil || ms.script != "" || ms.scheduler
	// Knoten hängt gerade am Meshlink: Station an (Etherlink fehlt)
	onMesh := sta != nil && !sta.Disabled

	if !ml.Enabled {
		if present {
			if onMesh && !ms.uplinkLink {
				blocked = "Das Gerät ist gerade nur über Meshlink angebunden (kein Etherlink) – Meshlink abzuschalten würde es vom Netz trennen. Erst ein Kabel anschließen."
				return
			}
			diffs = append(diffs, "Meshlink ist noch eingerichtet und wird entfernt")
		}
		return
	}
	if s.pkg != "wifi" {
		warn = append(warn, "Meshlink gibt es nur mit dem neuen wifi-Paket – auf diesem Gerät (altes wireless-Paket) wird es nicht eingerichtet.")
		return
	}
	cr := meshRadio(radios)
	if cr == nil || cr.off {
		warn = append(warn, "Kein 5-GHz-Funkteil in Betrieb – Meshlink wird auf diesem Gerät nicht eingerichtet.")
		return
	}
	if cr.freq == "" || cr.freq == "auto" {
		blocked = "Meshlink braucht einen festen 5-GHz-Kanal (alle APs des Standorts auf demselben Kanal) – unter „Funk“ einen Kanal statt „automatisch“ wählen, am besten 36–48 (ohne Radar-Prüfung)."
		return
	}
	if ms.read && !ms.hasUplink {
		blocked = "Meshlink braucht ether1 als Kabel-Uplink – das Gerät hat keinen Port ether1."
		return
	}
	if bi.name == "" {
		blocked = "Meshlink braucht eine Bridge auf dem Gerät."
		return
	}
	on = true
	if ml.SSID == "" || ml.Passphrase == "" {
		blocked = "Meshlink-Zugangsdaten fehlen – zentrale Konfiguration einmal speichern."
		return
	}
	check := func(x *wlanIfc, name, mode string) {
		switch {
		case x == nil:
			diffs = append(diffs, name+" fehlt")
		case x.Master != cr.x.Name || x.raw["configuration.mode"] != mode || x.SSID != ml.SSID || x.Passphrase != ml.Passphrase:
			diffs = append(diffs, name+" weicht ab")
		case x.Bridge != bi.name:
			diffs = append(diffs, name+" an keiner Bridge")
		}
	}
	check(ap, meshAP, "ap")
	check(sta, meshSTA, "station-bridge")
	if ap != nil && !ap.Hidden {
		diffs = append(diffs, meshAP+" ist nicht versteckt")
	}
	if strings.TrimSpace(ms.script) != strings.TrimSpace(meshWatch) {
		diffs = append(diffs, "Wächter-Skript fehlt oder ist veraltet")
	}
	if !ms.scheduler {
		diffs = append(diffs, "Zeitplan für den Wächter fehlt")
	}
	if onMesh {
		warn = append(warn, "Das Gerät hängt gerade am Meshlink (kein Etherlink): Beim Ausrollen ist es kurz nicht erreichbar, verbindet sich danach selbst wieder.")
	}
	return
}

// meshScriptRos baut den Teil des Ausroll-Skripts für Meshlink. Die alten
// Meshlink-Schnittstellen hat das Zurücksetzen der WLAN-Konfiguration schon
// entfernt (virtuelle WLANs); hier werden Skript/Zeitplan ersetzt und – wenn
// eingeschaltet – alles neu angelegt, im Zustand passend zum Etherlink.
func meshScriptRos(on bool, c CentralWLAN, cr *centralRadio, br string) string {
	var b strings.Builder
	b.WriteString("# Meshlink\n")
	b.WriteString(try("/system scheduler remove [find name=" + q(meshScript) + "]"))
	b.WriteString(try("/system script remove [find name=" + q(meshScript) + "]"))
	b.WriteString(try("/interface bridge port remove [find comment=" + q(meshComment) + "]"))
	b.WriteString(try("/interface wifi remove [find name=" + q(meshAP) + "]"))
	b.WriteString(try("/interface wifi remove [find name=" + q(meshSTA) + "]"))
	if !on {
		return b.String()
	}
	ml := c.Meshlink
	common := " master-interface=" + q(cr.x.Name) + " configuration.ssid=" + q(ml.SSID) +
		" configuration.country=" + q(wifiCountry(c.Country)) +
		" security.authentication-types=wpa2-psk,wpa3-psk security.passphrase=" + q(ml.Passphrase) +
		" comment=" + q(meshComment)
	addAP := "/interface wifi add name=" + q(meshAP) + common + " configuration.mode=ap configuration.hide-ssid=yes disabled="
	addSTA := "/interface wifi add name=" + q(meshSTA) + common + " configuration.mode=station-bridge disabled="
	// Startzustand nach dem Etherlink im Moment des Ausrollens (danach schaltet der Wächter)
	b.WriteString(":if ([/interface get [/interface find name=" + q(meshUplink) + "] running]) do={\n")
	b.WriteString("  " + addAP + "no\n  " + addSTA + "yes\n")
	b.WriteString("} else={\n")
	b.WriteString("  " + addAP + "yes\n  " + addSTA + "no\n")
	b.WriteString("}\n")
	b.WriteString("/interface bridge port add bridge=" + q(br) + " interface=" + q(meshAP) + " comment=" + q(meshComment) + "\n")
	b.WriteString("/interface bridge port add bridge=" + q(br) + " interface=" + q(meshSTA) + " comment=" + q(meshComment) + "\n")
	b.WriteString("/system script add name=" + q(meshScript) + " policy=read,write,test comment=" + q(meshComment) + " source=" + qml(meshWatch) + "\n")
	b.WriteString("/system scheduler add name=" + q(meshScript) + " interval=30s policy=read,write,test comment=" + q(meshComment) +
		" on-event=" + q("/system script run "+meshScript) + "\n")
	return b.String()
}

// qml: wie q, zusätzlich Zeilenumbrüche als \n (mehrzeiliger Skript-Quelltext).
func qml(s string) string {
	return strings.ReplaceAll(q(s), "\n", `\n`)
}

// meshlinkSummary für die Schritte der Vorschau.
func meshlinkSummary(on bool, c CentralWLAN, cr *centralRadio) string {
	if !on {
		return "Meshlink entfernen (Schnittstellen, Wächter-Skript, Zeitplan)"
	}
	return fmt.Sprintf("Meshlink auf %s (Kanal %s): %s (versteckt, nimmt Knoten an) und %s (verbindet sich ohne Etherlink), Wächter-Skript alle 30 s – aktiv ist je nach Link an %s das eine oder das andere",
		cr.x.Name, chanLabel(cr.freq), meshAP, meshSTA, meshUplink)
}
