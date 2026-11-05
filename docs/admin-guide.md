# mikrotik-manager – Admin-Guide

mikrotik-manager findet alle MikroTik-Geräte im Netz und verwaltet sie über eine
Weboberfläche: RouterOS- und Firmware-Updates, Neustart, Backups, Grundeinstellungen
und WLAN – einzeln, für alle Geräte gleichzeitig oder als zentrale WLAN-Konfiguration.

Inhalt

1. [Voraussetzungen auf den Geräten](#1-voraussetzungen-auf-den-geräten)
2. [Installation](#2-installation)
3. [Ersteinrichtung](#3-ersteinrichtung)
4. [Geräteübersicht](#4-geräteübersicht)
5. [Aktionen je Gerät](#5-aktionen-je-gerät)
6. [WLAN](#6-wlan)
7. [Zentrale WLAN-Verwaltung](#7-zentrale-wlan-verwaltung)
8. [Das Sicherheitsnetz bei Änderungen](#8-das-sicherheitsnetz-bei-änderungen)
9. [Selbst-Update](#9-selbst-update)
10. [Einstellungen](#einstellungen)
11. [Daten und Sicherheit](#11-daten-und-sicherheit)
12. [Fehlersuche](#12-fehlersuche)
13. [Grenzen](#13-grenzen)

## 1. Voraussetzungen auf den Geräten

- **RouterOS 7** (Geräte mit RouterOS 6 werden gefunden und ausgelesen; Änderungen
  und WLAN-Funktionen sind für RouterOS 7 gebaut).
- **API-Dienst an:** `/ip service` → `api` (Port 8728) oder `api-ssl` (8729, dann
  `API_TLS=true`). Ist der Dienst auf bestimmte Adressen beschränkt, die Adresse des
  Managers erlauben.
- **SSH an** (Port 22) – Backups werden per SFTP auf den Manager geholt. Der
  SSH-Schlüssel jedes Geräts wird beim ersten Kontakt gemerkt (TOFU); ändert er sich
  später, verweigert der Manager das Backup. Nach einer Neuinstallation zeigt die
  Geräteliste „SSH-Schlüssel geändert“ mit dem neuen Fingerabdruck; über
  **SSH-Schlüssel übernehmen** wird nach Rückfrage genau dieser Schlüssel gespeichert.
- Ein **Benutzer der Gruppe `full`** für die erste Anmeldung (z. B. `admin`). Danach
  legt der Manager einen eigenen Benutzer an (siehe 3.3).
- Der Manager muss die Geräte per **MNDP-Broadcast** (UDP 5678) sehen, also im selben
  Netzsegment hängen. Geräte dahinter (andere VLANs/Subnetze) findet er per
  `SCAN_CIDRS` über den API-Port.

## 2. Installation

- **Proxmox / Debian / Ubuntu (systemd):** [docs/proxmox.md](proxmox.md) – mit Selbst-Update.
- **Docker / Synology:** [docs/docker.md](docker.md).

Die Oberfläche läuft auf Port **8080**, Benutzer **`admin`**. Ohne `WEB_PASSWORD`
erzeugt der Manager beim ersten Start ein Passwort und legt es in
`web-password.txt` im Datenverzeichnis ab.

## 3. Ersteinrichtung

### 3.1 Anmelden und Geräte finden

Nach dem Anmelden sucht der Manager sofort per MNDP und liest jedes gefundene Gerät
aus. **Jetzt suchen** stößt eine neue Suche an. Ohne Zugangsdaten versucht er nur
`admin` ohne Passwort (Werkszustand).

### 3.2 Standard-Zugangsdaten

**Standard-Zugangsdaten** (oben): Benutzer und Passwort, die für alle Geräte gelten.
Weicht ein Gerät ab, in seiner Zeile **Login** → eigene Zugangsdaten. Gespeichert wird
verschlüsselt auf dem Manager, auf den Geräten ändert sich nichts.

### 3.3 Verwaltungsbenutzer (empfohlen)

**Verwaltungsbenutzer → Auf allen Geräten einrichten** legt auf jedem erreichbaren
Gerät den Benutzer `mikrotik-manager` (Gruppe `full`) mit einem Zufallspasswort an,
prüft die Anmeldung und verwendet ihn ab dann als Standard. Das Passwort wird in der
Oberfläche angezeigt (kopieren-Knopf). Neue Geräte: Knopf erneut drücken.

### 3.4 Passwort eines Benutzers auf allen Geräten setzen

**Benutzer-Passwort setzen** setzt z. B. das `admin`-Passwort auf allen Geräten und
prüft danach die Anmeldung. Den Benutzer, mit dem sich der Manager selbst anmeldet,
lehnt die Funktion ab (sonst sperrt er sich aus).

## 4. Geräteübersicht

- **Kacheln:** Anzahl Geräte, RouterOS 7/6, ausstehende Firmware-Updates, API-Probleme,
  Geräte ohne Backup in den letzten 36 h, **WLAN-Clients** (Klick → alle
  WLAN-Verbindungen über alle Geräte, sortierbar).
- **Gerätezeile:** Name (grüner Punkt = in den letzten 15 min gesehen), IP/MAC,
  Modell, RouterOS (Hinweis bei neuerer Version), Firmware (Hinweis bei Upgrade),
  WLAN-Paket (`wireless` alt / `wifi` neu) mit **„▸ N Clients“** zum Aufklappen
  (SSID, Band, Signal, Rate, Dauer, MAC, IP), Laufzeit, API-Status.
- **Statusleiste oben:** laufende Aktionen und die der letzten 15 Minuten. Solange
  dort etwas läuft, die Geräte nicht vom Strom trennen.
- Jeder Knopf erklärt beim Darüberfahren mit der Maus, was ein Klick tut.

Geräte werden alle 5 Minuten (`POLL_INTERVAL`) ausgelesen, WLAN-Clients jede Minute.

### 4.1 Detailseite eines Geräts

Klick auf den **Namen** in der Geräteliste öffnet die Detailseite (Adresse `#dev/<Geräte-ID>`,
lässt sich als Lesezeichen speichern; „← Geräte“ oder Zurück im Browser führt zur Liste).

- **Kopf:** IP, MAC, Modell, Seriennummer, RouterOS/Kanal, Firmware, Laufzeit, CPU/RAM.
- **Standort:** freier Text (z. B. „Technikraum EG, Rack 2“), nur auf dem Manager
  gespeichert, erscheint auch unter dem Namen in der Geräteliste.
- **Einstellungen & Aktionen:** dieselben Knöpfe wie in der Liste (Einstellungen, WLAN,
  Login, Update, Neustart, neu auslesen, ggf. SSH-Schlüssel übernehmen).
- **Ports:** alle Ethernet- und SFP-Ports wie an der Gerätefront – **schwarz** kein Link,
  **grün** Link, **rot** PoE versorgt ein Gerät, **rot blinkend** PoE liefert Strom, aber
  es gibt keinen Link (Gerät hängt, Kabel halb defekt …), schraffiert = deaktiviert.
  Darunter je Port Kommentar, Geschwindigkeit (Halbduplex markiert), Bridge, PoE-Status
  mit Watt und MAC. Wird beim Öffnen und danach alle 15 s live gelesen.
- **WLAN** (nur Geräte mit WLAN-Paket): je Schnittstelle eine Kachel – schwarz aus bzw.
  sendet nicht, grün umrandet sendet ohne Clients, grün sendet mit Clients, schraffiert
  deaktiviert; Tabelle mit SSID, Band, Funk (Breite/Kanal; virtuelle WLANs „wie
  Funkteil“), Verschlüsselung, VLAN (PVID am Bridge-Port), Bridge und den verbundenen
  Clients zum Aufklappen. Nur Anzeige, ändern über den Knopf „WLAN“. Jede Minute neu.
  Spalte **Zustand**: was RouterOS selbst meldet (`/interface wifi|wireless monitor once`),
  z. B. „sendet“, „Radar-Prüfung (DFS) läuft“, „kein erlaubter Kanal verfügbar“, „Funkteil
  sendet nicht“ (bei virtuellen WLANs), mit tatsächlichem Kanal und Sendeleistung; der
  Rohtext steht im Tooltip. Darunter aufklappbar die letzten WLAN-Meldungen aus dem
  Geräteprotokoll (`/log`, Themen wireless/wifi/caps – RouterOS hält es im Speicher).
- **Konfig-Historie:**
  - *über den Manager:* wer was ausgelöst hat (Umbenennen, Zugangsdaten, Update,
    Neustart, WLAN, Rollback, Standort …) und das Ergebnis jedes Jobs; „Manager“ =
    automatisch (nächtliches Backup nur bei Fehler, sonst steht es bei den Backups).
  - *Konfiguration geändert:* Unterschied zwischen zwei aufeinanderfolgenden
    gespeicherten Exports (`.rsc`, nachts und vor jeder Änderung) – so werden auch
    Änderungen sichtbar, die jemand direkt am Gerät gemacht hat. Passwörter, WLAN- und
    WireGuard-Schlüssel sind maskiert.
  - *am Gerät selbst:* der RouterOS-Verlauf (`/system history`) mit Benutzername –
    auch Änderungen per Winbox, WebFig oder SSH. RouterOS beginnt diese Liste nach
    jedem Neustart neu.
- **Backups auf dem Manager:** Backup jetzt, Liste zum Herunterladen/Zurückspielen.
- **Dateien auf dem Gerät:** nur Anzeige (`/file`).

Alles auf der Detailseite liest nur – außer dem Standort-Text und den Aktionsknöpfen, die
wie in der Liste mit Rückfrage arbeiten.

### 4.2 WWAN / LTE-Router (z. B. Chateau LTE7 ax)

Geräte mit LTE-Modem erkennt der Manager selbst (Hinweis **LTE** und Monatsvolumen in der
Geräteliste). Auf der Detailseite steht dann der Abschnitt **WWAN / LTE-Router**:

- **LTE-Zustand** live: Verbindung, Betreiber, Technik, Signal (RSRP/SINR), SIM, IMEI.
- **Einstellung je Gerät:** **Netz** als die ersten drei Oktette (z. B. `192.168.50`), optional
  **APN** (leer = automatisch) und eine **SIM-PIN je LTE-Schnittstelle**. Die PIN liegt
  verschlüsselt im Tresor des Managers (nie in `wwan.json`), „PIN löschen“ entfernt sie.
- **Vorschau & ausrollen** macht das Gerät zum Router ins Mobilfunknetz:
  - LAN: Gerät **.254**/24 auf der Bridge, DHCP-Server **.10–.200**, Gateway .254,
    DNS an die Clients **1.1.1.1 und 9.9.9.9**; ein DHCP-Client auf der Bridge wird entfernt
    (das Gerät ist jetzt selbst Router), ebenso andere feste Adressen im selben Netz.
  - Gerät: DNS 1.1.1.1/9.9.9.9, **NTP-Client** an (`de.pool.ntp.org`).
  - **WAN über Kabel:** ether4 und ether5 (soweit vorhanden) kommen aus dem LAN in eine
    eigene Bridge `mm-wan` mit DHCP-Client. Bekommt das Gerät dort eine Adresse **und** ist
    darüber das Internet erreichbar (Ping an 8.8.4.4 über das WAN-Gateway, rekursive Route
    mit `check-gateway`), ist das die **Standardroute** (Distanz 1); die LTE-Route hat
    Distanz 2 und springt als **Ersatz** ein, sobald das WAN kein Internet mehr hat. Die
    Routen setzt ein Skript am DHCP-Client (Kommentar `mm-wan`). 8.8.4.4 läuft dabei immer
    übers WAN – deshalb ist es nicht unter den DNS-Servern der Clients.
  - **NAT** (masquerade) über jede LTE-Schnittstelle und `mm-wan`; **Firewall** (Kommentar
    `mm-wwan …`): aus dem Internet (LTE und WAN) nur Antworten (established/related) – aufs
    Gerät und ins LAN.
  - PIN und APN werden zuerst gesetzt, dann das LAN umgebaut. Wie immer mit Backup und
    Rückroll-Wächter: kommt der Manager danach nicht wieder an das Gerät (z. B. falsche
    PIN, kein Netz), stellt es nach 5 Minuten selbst den alten Zustand her.
  - Die Verwaltung läuft danach über LTE (WireGuard-Tunnel zum Manager).
- **Volumen über LTE:** Der Manager liest bei jeder Abfrage die Byte-Zähler der
  LTE-Schnittstellen und bucht die Differenz auf den Kalendermonat – **Stichtag ist immer der
  1. des Monats** (Ortszeit des Managers). Nach einem Neustart beginnen die Zähler bei 0; der
  Manager erkennt das (kleinere Laufzeit bzw. kleinerer Zähler) und zählt den neuen Stand
  komplett. Verkehr unmittelbar vor einem Neustart, während der Manager das Gerät nicht
  erreicht, fehlt. Tabelle je Monat auf der Detailseite, CSV je Gerät, CSV aller LTE-Geräte
  unter `/api/wwan/usage.csv?month=JJJJ-MM`. Gespeichert in `wwan-usage/<Gerät>.json`
  (60 Monate).

**Stand:** am echten Chateau noch nicht ausgerollt (kein Testgerät) – vor dem ersten Kunden
an einem Testgerät prüfen (PIN-Befehl, LTE-Monitor-Felder, Firewall-Reihenfolge).

### 4.3 Netzwerk – MAC-Liste als Baum (Tab „Netzwerk“)

Tab **Netzwerk**: zeigt, an welchem Port welche MAC-Adresse hängt – als Baum mit dem
Router oder dem zentralen Switch als **Wurzel**.

- **Aufbau:** Wurzel → Ports → dort gelernte MAC-Adressen (mit IP und Hostname aus ARP-
  Tabellen und DHCP-Leases der Geräte, VLAN, WLAN-Kennzeichen) und die weiteren verwalteten
  Geräte am jeweiligen Port, mit deren Ports usw. Der Uplink eines Geräts (Port Richtung
  Wurzel) wird nicht aufgeführt; MACs, die ein tieferes Gerät selbst zeigt, stehen nur dort.
- **Erkennung:** welches Gerät an welchem Port hängt, über die Nachbarn (MNDP/LLDP); fehlt
  der Eintrag, über die MAC-Tabelle (dort, wo die MAC des Geräts am tiefsten gelernt wird –
  nie über einen Uplink). Was hinter einem **unverwalteten Switch** hängt, erscheint am Port
  des nächsten verwalteten Geräts. Der Punkt am Port zeigt Link bzw. bei WLAN „sendet“
  (Zustand laut RouterOS, nicht das running-Flag).
- **Wurzel:** „automatisch“ = Gerät mit den meisten Nachbarn; besser fest den Router bzw.
  den zentralen Switch wählen. Ist das kein MikroTik (z. B. Fritzbox oder fremder Switch):
  **„Fremdgerät“** – es steht als virtuelle Wurzel oben, alle verwalteten Geräte ohne
  verwaltetes Gerät darüber eine Stufe darunter. Die Richtung „nach oben“ erkennt der
  Manager dann am Port, an dem die MAC des Standard-Gateways gelernt wird; am Fremdgerät
  stehen die MACs, die nur über diese Uplinks gesehen werden (Gateway, Geräte direkt am
  Fremd-Router). Ports des Fremdgeräts kennt der Manager nicht.
  Die Wahl – die Wahl wird gespeichert.
- **Suchen** filtert nach MAC, IP, Hostname, Port oder Gerät (der Weg bleibt sichtbar),
  **CSV** lädt die Liste mit Pfad. „Details ›“ führt zur Detailseite des Geräts.
- **Nicht zugeordnet** = lesbar, aber an keinem Port erkennbar (kein Nachbar, MAC nirgends
  gelernt); **nicht lesbar** = Gerät gerade nicht erreichbar.
- Nur lesend: `interface/bridge/host`, `ip/neighbor`, `interface`, `ip/arp`,
  `ip/dhcp-server/lease` – bei jedem „Aktualisieren“ live von allen Geräten.

## 5. Aktionen je Gerät

Alle Aktionen laufen im Hintergrund und **nacheinander** (nie zwei Geräte
gleichzeitig) – fällt ein Gerät aus, bleibt der Rest unberührt.

### Update

Holt zuerst ein Backup auf den Manager, installiert dann eine neuere RouterOS-Version
(falls vorhanden, Kanal des Geräts) und danach die passende RouterBOARD-Firmware.
Ein bis zwei Neustarts, je ca. 1–2 Minuten offline. Der Manager prüft nach jedem
Neustart die erwartete Version.

Gibt es weder ein neueres RouterOS noch neue Firmware, tut der Knopf nichts (kein
Backup, kein Neustart).

### Automatische Updates (Tab)

Im Tab **Automatische Updates** legt man fest, **wann** aktualisiert wird:

- **aktiviert**, **Uhrzeit** und **Wochentage** (z. B. sonntags 03:00),
- **RouterBOARD-Firmware mit aktualisieren** – eigener Haken, weil die Firmware einen
  zusätzlichen Neustart je Gerät kostet; ohne Haken wird nur RouterOS aktualisiert und
  ausstehende Firmware lediglich angezeigt,
- **Geräte** – keines gewählt = alle.

Zum Termin prüft der Manager jedes Gerät und aktualisiert nur die mit Update – Gerät
für Gerät, jeweils mit Backup vorab, so dass nie alle Access Points gleichzeitig weg
sind. Läuft zur Startzeit noch etwas (z. B. das nächtliche Backup), wartet er.
**Jetzt ausführen** startet den Lauf sofort. Der Tab zeigt den nächsten Termin, den
Stand aller Geräte und das Ergebnis des letzten Laufs je Gerät.

Tipp: nicht dieselbe Uhrzeit wie das nächtliche Backup (`BACKUP_TIME`, 04:00) wählen.

### Neustart

Startet das Gerät neu und prüft, dass es wiederkommt.

### Backup

Erstellt ein Binär-Backup (`.backup`, zum Wiederherstellen) und einen Text-Export
(`.rsc`, zum Nachlesen) und speichert beides auf dem Manager; die Kopie auf dem
Gerät wird gelöscht. Je Gerät bleiben die letzten 20 Sicherungen.
Zusätzlich **jede Nacht um 04:00** für alle Geräte (`BACKUP_TIME`, `off` = aus) und
**automatisch vor jeder Änderung**. Download über den Knopf „N Dateien“ in der Zeile.

**Zurückspielen (Rollback):** In der Backup-Liste steht neben jeder `.backup`-Datei
**Zurückspielen**. Der Manager prüft zuerst und fragt nach, dann:

1. aktuellen Stand als Backup `…-vor-rollback` auf den Manager sichern (damit lässt
   sich der Rollback selbst wieder zurücknehmen)
2. Backup aufs Gerät laden und einspielen – Neustart, ca. 1–2 Minuten offline
3. warten, bis das Gerät wieder per API erreichbar ist

Die gesamte Konfiguration geht auf den Stand des Backups, auch IP-Adressen, Benutzer
und Passwörter. Gab es den Benutzer, mit dem sich der Manager anmeldet, damals noch
nicht oder mit anderem Passwort, kommt der Manager danach nicht mehr aufs Gerät –
dann die Zugangsdaten im Manager anpassen. Ein Backup enthält keine Pakete:
Braucht es ein anderes WLAN-Paket als installiert (z. B. ein Backup von vor der
Umstellung auf `wifi`), lehnt der Manager ab, weil das WLAN danach aus wäre.

Von Hand geht es auch: `.backup`-Datei herunterladen, in WinBox/WebFig unter *Files*
hochladen → *System → Backup → Restore*. Ein Backup gehört zu genau diesem Gerät
(enthält MAC-Adressen).

### Einstellungen

- **Name setzen** – sofort, mit Gegenprobe.
- **Alle Ports in eine Bridge** – nimmt alle Ethernet- und WLAN-Schnittstellen in die
  Bridge, IP-Adressen und DHCP-Clients der Ports wandern mit. Virtuelle WLANs
  (Gästenetze) werden nie automatisch aufgenommen. Wird verweigert, solange auf dem
  Gerät ein DHCP-Server läuft (es ist dann ein Router).
- **DHCP-Client auf der Bridge** – optional „feste IP entfernen“: Die feste IP wird
  erst entfernt, wenn eine DHCP-Adresse bezogen ist. Der Manager findet das Gerät
  danach unter der neuen IP wieder.

Bridge und DHCP laufen mit Vorschau und Sicherheitsnetz (Abschnitt 8).

### IP-Einstellungen und SNMP

**IP-Einstellungen** (Knopf „Einstellungen“): feste Adresse, Subnetz, Gateway und DNS oder
zurück auf DHCP, für Access Points und Switches. Verweigert, wenn ein DHCP-Server läuft oder
mehrere Netze anliegen (Router). Der Rückfall auf DHCP hat zwei Stufen:

- Während der Änderung gilt der Wächter: Erreicht der Manager das Gerät nicht, stellt es
  nach 5 Minuten den alten Zustand wieder her.
- Danach pingt ein Zeitplan `mm-ip-fallback` jede Minute das Gateway. Bleibt es 5 Minuten
  stumm, schaltet er den DHCP-Client ein, die feste IP bleibt daneben stehen. Antwortet das
  Gateway wieder, geht der Client aus. Ein Gateway, das Ping nicht beantwortet, führt also nur
  zu einer zweiten Adresse. Im Device-Mode `home` sperrt RouterOS Zeitpläne; dann gibt es nur
  den Wächter, die Vorschau weist darauf hin.

**SNMP** (je Gerät unter „Einstellungen“, für alle als Karte in der Geräteliste): v2c,
Community `public`, nur lesend. Wer das Gerät per UDP 161 erreicht, kann Gerätedaten lesen;
beschränken lässt sich das nur mit einer Firewall auf dem Gerät.

## Umzug in die Cloud

Tab **Cloud-Umzug**: bringt verwaltete Geräte in die MikroTik-Manager Cloud.

1. In der Cloud unter **Kunden & Anmeldung** einen **Mehrfach-Code** erzeugen. Ein Einmal-Code
   reicht nur für ein Gerät; danach bricht der Lauf für die übrigen ohne Änderung ab.
2. Den angezeigten Befehl einfügen, **Prüfen** (verbraucht den Code nicht), Geräte wählen,
   **Ausgewählte Geräte umziehen**.

Der Manager spricht selbst mit der Cloud und führt das Abschluss-Skript auf dem Gerät aus. Das
Gerät braucht darum kein `/tool fetch` (im Device-Mode `home` gesperrt), nur RouterOS 7 und
ausgehend UDP zum Cloud-Hub. Je Gerät: Backup, Tunnel einrichten, bis zu 60 s auf den Handshake
warten. Ohne Handshake wird alles zurückgenommen; den Eintrag in der Cloud dann „Vergessen“.

Danach **schreibt nur die Cloud**, dieser Manager liest mit: Status, Netzwerk-Baum, WLAN-Clients und
Backups laufen weiter, Änderungen, Updates, Neustart, Rollback und zentrales WLAN sind gesperrt. Sonst
würden sich beide Manager gegenseitig überschreiben. Die Cloud legt ihren eigenen Benutzer
`mikrotik-manager-cloud` an, der Zugang dieses Managers bleibt bestehen.

Fällt die Cloud aus oder wird das Gerät dort entfernt, gibt **zurückholen** in der Gerätezeile das
Schreiben wieder frei. Das ändert nichts am Gerät, der Tunnel bleibt stehen.

Die Cloud muss den eigenen Benutzer schon kennen; eine ältere Cloud lehnt dieser Manager beim Prüfen ab.

### Netzwerk-Baum: Scope

Im Tab Netzwerk steht hinter der Wurzel die Auswahl **Scope**: **alles** zeigt den ganzen Baum, **VLAN** blendet
ein zweites Feld mit allen VLANs des Standorts ein. Dann bleiben nur Geräte, Ports und MAC-Adressen, die dieses
VLAN nutzen, dazu der Weg dorthin. Ein Port zählt auch ohne aktive MAC, wenn das VLAN laut Bridge dort anliegt.
Eine MAC ohne VLAN-Angabe zählt zu VLAN 1. Ein Bridge ohne VLAN-Filter meldet nur VLAN 1, obwohl getaggter Verkehr
durchlaufen kann. CSV gibt nur die gefilterte Liste aus.

### Router

Tab **Router** (lokal für das ganze Netz, in der Cloud je Standort): Hier wählst du das Gerät, das Router
ist. Es bekommt die Badge **router**. Je Port stellst du Rolle, Typ und VLAN ein:

- **WAN:** primärer Internetzugang, genau einer. **Backup WAN:** höchstens einer. Bleiben über das WAN
  60 Sekunden lang die Prüfadressen stumm, übernimmt es; nach 60 stabilen Sekunden geht es zurück.
- **Typ:** IPoE (IP, Maske, Gateway, DNS), DHCPoE, PPPoE (Benutzer, Passwort), WWAN (nur LTE, PIN, APN).
- **Access:** Hier hängen Switches und Access Points. **Aus:** Port abgeschaltet.
- **VLAN am Access-Port:** leer = VLAN 1 ungetaggt, alle anderen getaggt. Ein Wert = nur dieses ungetaggt.
  Mehrere = nur diese getaggt, VLAN 1 darin ungetaggt.
- **VLAN am WAN:** Die Verbindung läuft darüber, das VLAN ist sonst überall gesperrt.

Auf der Geräteseite des Routers zeigt die Karte **WAN** je Verbindung IP-Adresse, Gateway, DNS, **Link up seit**,
bei DHCP **Lease bezogen am** und die Restlaufzeit, bei PPPoE und WWAN **IP bezogen am**. RouterOS nennt bei DHCP
nur die Restlaufzeit; den Zeitpunkt hält ein Skript am DHCP-Client fest. Das klappt also erst ab dem nächsten neuen
Lease nach dem Ausrollen. Die Badge **aktiv** zeigt, über welche Verbindung der Verkehr gerade läuft.

Unter **Netzwerke** ist die erste Zeile das Standard-LAN (VLAN 1), weitere kommen mit **+** dazu. Netzwerke
sind untereinander gesperrt und dürfen ins Internet; nur das Standard-LAN darf in die anderen. Von außen ist
alles zu, verwalten lässt sich der Router nur aus dem Standard-LAN und über den Tunnel.

**Ausrollen ersetzt die Router-Konfiguration ganz.** Vorher entsteht ein Backup, und kommt der Manager nicht
mehr aufs Gerät, stellt es nach 5 Minuten den alten Zustand wieder her. Geräte hinter dem Router behalten
ihre alte Adresse, bis ihre DHCP-Lease erneuert wird. Nicht enthalten: Portweiterleitung, feste
DHCP-Zuweisungen, IPv6, VPN-Einwahl, Lastverteilung.

## 6. WLAN

### 6.1 WLAN-Dialog eines Geräts (Knopf „WLAN“)

- **Je Schnittstelle:** SSID, Passwort (👁 zeigt es), Verschlüsselung, an/aus,
  Clients, Bridge-Zuordnung (⚠ = an keiner Bridge, dort verbundene Geräte bekommen
  kein Netz).
- **Funk je Funkteil:** Band, Kanalbreite, Kanal (automatisch oder fest; DFS-Kanäle
  sind markiert), welche DFS-Kanäle die Automatik auslässt, aktuell genutzter Kanal.
  Dazu das **Land** – es bestimmt erlaubte Kanäle und Sendeleistung und muss dem
  Standort entsprechen.
- **Empfehlung eintragen** füllt nur die Felder:
  - Verschlüsselung **WPA2/WPA3** (neue Geräte nutzen WPA3, ältere WPA2; das alte
    `wireless`-Paket kann nur WPA2),
  - 2,4 GHz **20 MHz auf Kanal 1, 6 oder 11** (die einzigen ohne Überlappung,
    reihum nach Gerätename verteilt),
  - 5 GHz **80 MHz, Kanal automatisch, ohne die 10-Minuten-Radarkanäle** (Wetterradar
    120–128; sonst bis zu 10 Minuten Funkstille nach Neustart/Radar),
  - Land Deutschland.
- **Vorschau** zeigt genau, was geändert würde; **Jetzt ausführen** mit Backup und
  Sicherheitsnetz.

Profile: Benutzen alle betroffenen Schnittstellen dasselbe Sicherheits- bzw.
Konfigurationsprofil und bekommen denselben Wert, wird das Profil geändert. Sonst nur
die einzelne Schnittstelle – damit wandert z. B. ein zweites Netz mit gleichem Profil
nicht ungewollt mit.

### 6.2 WLAN im ganzen Netz

**WLAN im ganzen Netz** (oben): eine SSID wählen, neue SSID und/oder neues Passwort
und/oder Verschlüsselung eingeben → Vorschau je Gerät → nacheinander ausführen.

### 6.3 Umstellung vom alten `wireless`- auf das neue `wifi`-Paket

Im WLAN-Dialog bei Geräten mit `wireless`: **Umstellung prüfen**. Möglich nur mit
802.11ac-Wave-2-Funkchips (z. B. cAP ac, hAP ac², hAP ac³: IPQ4018/4019, QCA9984/9888);
ältere (z. B. hAP ac lite) bleiben auf `wireless`. Ablauf:

1. Backup auf den Manager, Fortschritt auf dem Manager speichern
2. `wireless` deinstallieren → Neustart (der Flash fasst nicht beide Pakete)
3. `wifi-qcom-ac` aus dem Internet installieren → Neustart
4. SSIDs, Passwörter, an/aus, Kanal/Breite, Land, Bridge-Ports und
   Schnittstellenlisten übersetzt einspielen und prüfen

Das WLAN des Geräts ist dabei ca. 5 Minuten aus; das Gerät braucht Internetzugang.
Scheitert die Paket-Installation, installiert der Manager `wireless` wieder und spielt
das Backup ein. Bricht die Umstellung ab, setzt ein erneuter Start an der richtigen
Stelle fort. Abgelehnt wird bei Station-/WDS-Modus, EAP/RADIUS/WEP, CAPsMAN und
zu wenig Flash.

**WLANs mit VLAN-Tag** (`vlan-mode=use-tag`, z. B. Gästenetz in VLAN 178 an einer
Bridge ohne VLAN-Filter): `wifi-qcom-ac` kann an der WLAN-Schnittstelle nicht
taggen. Der Manager legt deshalb eine VLAN-Schnittstelle `vlan178` auf der
bisherigen Bridge und eine eigene Bridge `bridge-vlan178` an, in die `vlan178` und
diese WLANs kommen. Die bisherige Bridge bleibt unverändert (kein VLAN-Filter), auf
dem Ethernet-Port kommt VLAN 178 weiter getaggt an. Abgelehnt wird, wenn die Bridge
schon einen VLAN-Filter hat, das VLAN schon als Schnittstelle existiert, die Namen
vergeben sind oder das WLAN an keiner Bridge hängt.

## 7. Zentrale WLAN-Verwaltung

Tab **Zentrale WLAN-Verwaltung**: eine WLAN-Konfiguration, die auf allen ausgewählten
Geräten gleich ausgerollt wird.

- **Geräte** auswählen.
- **WLANs** (bis 8): SSID, Passwort, Verschlüsselung, Bänder (beide / nur 2,4 / nur 5),
  **VLAN** (optional), versteckt. „von Gerät übernehmen“ liest die WLANs eines Geräts ein.
- **Funk** je Band: an, Breite, Kanal (2,4 GHz: Kanalplan 1/6/11 reihum empfohlen),
  DFS-Einstellung; Land.
- **Speichern & prüfen** speichert und zeigt je Gerät „entspricht“ oder die
  Abweichungen – **ändert nichts an den Geräten**.
- **Ausrollen** (nur wenn „aktiviert“) auf die abweichenden Geräte, nacheinander:
  Backup auf den Manager und ein Rückweg-Backup auf das Gerät, dann die vorhandene
  WLAN-Konfiguration **zurücksetzen** (virtuelle WLANs, WLAN-Profile, Werte an den
  Funkteilen) und neu aufbauen. Danach wird geprüft; scheitert die Prüfung, lädt das
  Gerät sein Rückweg-Backup (Neustart).

**Aufbau auf jedem Gerät:** Je Band trägt das Funkteil das erste passende WLAN, weitere
WLANs werden virtuelle WLANs auf dem Funkteil (`wifi1-v1` …). Alle hängen an der
Bridge. Ein Band ohne WLAN oder abgeschaltet → Funkteil aus.

**VLAN je SSID:** Clients dieses WLANs landen im VLAN (WLAN-Port der Bridge mit
`pvid=<VLAN>`); das VLAN läuft **getaggt über alle Ethernet-Ports** der Bridge, der
VLAN-Filter der Bridge wird eingeschaltet. Die Verwaltung des Geräts bleibt ungetaggt.
Eigene Bridge-VLANs werden nicht angefasst. **Der Switch-Port zum Access Point muss das
VLAN getaggt liefern** – sonst haben die Clients kein Netz; das kann der Manager nicht
prüfen. Der VLAN-Filter kann auf manchen Geräten die Hardware-Beschleunigung der Bridge
abschalten.

**Aktiviert** sperrt Einzeländerungen am WLAN der ausgewählten Geräte (Hinweis im
WLAN-Dialog, Kennzeichen „zentral“ in der Liste), damit sie nicht auseinanderlaufen.

> Tipp: Neue Vorgaben zuerst mit nur **einem** ausgewählten Gerät ausrollen.

### 7.x Meshlink (APs ohne Netzwerkkabel)

Karte **Meshlink** in der zentralen WLAN-Verwaltung: Access Points, die nur Strom haben
(Netzteil oder PoE-Injektor ohne Datenkabel), hängen sich automatisch per WLAN an einen AP mit
Kabel und reichen dessen Netz weiter – alle WLANs und VLANs, auch den Verwaltungszugang
(WireGuard-Tunnel zum Manager).

- **Automatisch, ohne Rollen:** Jeder verwaltete AP bekommt auf seinem 5-GHz-Funkteil
  `mm-mesh-ap` (verstecktes WLAN, nimmt Knoten an) und `mm-mesh-sta` (Station im Modus
  `station-bridge`) sowie Skript und Zeitplan `mm-meshlink`. Das Skript prüft alle 30 s den
  Link an **ether1**: kein Link (20 s bestätigt) → Station an, Angebot aus; Link wieder da
  (20 s bestätigt) → zurück. Umschalten dauert ca. 20–50 s. Ein Knoten nimmt selbst keine
  Knoten an – es gibt nur einen Funksprung, keine Schleifen.
- **Voraussetzung:** ein **fester 5-GHz-Kanal** unter „Funk“ – alle APs müssen auf demselben
  Kanal senden (die Station hängt am selben Funkteil und kann den Kanal nicht wechseln). Ohne
  festen Kanal sperrt der Manager das Ausrollen. Empfehlung: 36–48 (5180–5240 MHz) ohne
  Radar-Prüfung; DFS-Kanäle kosten nach jedem Neustart eine Minute und können bei Radar
  den Meshlink unterbrechen.
- **Nur wifi-Paket** (wifi-qcom, wifi-qcom-ac; getestet cAP ax ↔ wAP ac gemischt). Geräte mit
  altem wireless-Paket bekommen einen Hinweis und kein Meshlink. Gerät braucht einen Port
  `ether1` und eine Bridge.
- **SSID und Passwort** erzeugt der Manager beim Speichern (versteckt, 32 Zeichen). Abschalten entfernt beim
  nächsten Ausrollen alles wieder – außer bei einem Gerät, das gerade nur über Meshlink
  angebunden ist (gesperrt, sonst wäre es weg).
- **Ausrollen an Knoten:** läuft wie immer lokal auf dem Gerät mit Rückweg-Backup und
  Wächter; der Knoten ist dabei kurz weg und verbindet sich danach selbst wieder.
- **Detailseite:** Zeilen `mm-mesh-ap` („Meshlink-Angebot“) und `mm-mesh-sta`
  („Meshlink-Station … verbunden mit <AP-MAC>“), Zustand „verbunden (Meshlink aktiv)“.
- **Durchsatz:** Ein Knoten teilt sein 5-GHz-Funkteil zwischen Meshlink und Clients – etwa
  die Hälfte des Durchsatzes eines AP mit Kabel.
- **Kurz „nicht erreichbar“ beim Umschalten ist normal:** Zwischen Kabel weg und Meshlink
  aktiv (bzw. zurück) ist der AP ca. 20–50 s nicht erreichbar. Fragt der Manager genau
  dann ab, steht in der Geräteliste z. B. „Tunnel steht, aber Port 8728 antwortet nicht …
  i/o timeout“ – die dort genannten Ursachen (Dienst, Firewall, Route) treffen dann nicht
  zu. Bei der nächsten Abfrage (spätestens nach `POLL_INTERVAL`, sofort mit „↻“) ist der
  AP wieder „ok“. Bleibt die Meldung länger als 2 Minuten: Detailseite → WLAN →
  Zeile `mm-mesh-sta` (Zustand, verbunden mit) bzw. die WLAN-Meldungen prüfen.
- **Probelauf 2026-11-03** (cAP ax + wAP ac, 5 GHz fest Kanal 36, VLAN 67): über den Manager
  ausgerollt (je Gerät 9–14 s, danach ohne Abweichung), dann am Switch jeweils den Port
  abgeschaltet (PoE lief weiter): beide Richtungen umgeschaltet, Manager erreichte den
  Knoten durchgehend über den Tunnel, VLAN 67 lief über `mm-mesh-sta`, nach dem
  Einschalten zurück aufs Kabel.

## 8. Das Sicherheitsnetz bei Änderungen

Bridge, DHCP-Client, WLAN-Änderungen und das Ausrollen laufen so ab:

1. Plan erstellen und anzeigen (Vorschau) – ändert nichts.
2. Backup auf den Manager.
3. Auf dem Gerät ein Rückweg-Skript anlegen und einen **Wächter** starten: Wird die
   Änderung nicht innerhalb von 5 Minuten bestätigt, führt das Gerät den Rückweg
   selbst aus.
4. Änderung ausführen (läuft auf dem Gerät weiter, auch wenn die Verbindung abbricht).
5. Neu verbinden (alte IP, neue IP, per MNDP gemeldete IP; Software-ID muss passen).
6. Prüfen: eine erneute Planung muss „nichts zu tun“ ergeben.
7. Erfolg → Rückweg löschen (= bestätigt). Erreicht der Manager das Gerät, aber die
   Prüfung scheitert → sofort zurückrollen.

Damit sperrt sich der Manager auch bei einer falschen Bridge-/DHCP-Einstellung nicht
dauerhaft aus.

## 9. Selbst-Update

Der Manager fragt alle 6 Stunden bei GitHub nach dem neuesten Release (Fußzeile:
Zeitpunkt der letzten Prüfung, **jetzt prüfen**). Ist eines da, erscheint oben ein
Hinweis mit Release-Notizen.

- **systemd-Installation** (Proxmox, Debian …): **Jetzt aktualisieren** lädt das
  Programm, prüft die SHA256-Prüfsumme und einen Probestart, ersetzt sich (alte
  Version bleibt als `.old`) und startet neu – wenige Sekunden, die Geräte merken
  davon nichts. Während auf einem Gerät eine Aktion läuft, wird nicht aktualisiert.
  Mit `AUTO_UPDATE=true` geschieht das ohne Klick.
- **Docker:** Der Hinweis nennt die Befehle zum Aktualisieren des Images
  ([docs/docker.md](docker.md#aktualisieren)).

## Einstellungen

Alle Einstellungen sind Umgebungsvariablen – bei systemd in
`/etc/mikrotik-manager.env`, bei Docker unter `environment:`.

| Variable | Standard | Bedeutung |
|---|---|---|
| `LISTEN` | `:8080` | Adresse/Port der Weboberfläche |
| `WEB_USER` | `admin` | Benutzer der Weboberfläche |
| `WEB_PASSWORD` | – | Passwort; leer = beim ersten Start erzeugen (`web-password.txt`) |
| `DATA_DIR` | `/var/lib/mikrotik-manager` (Docker `/data`) | Datenverzeichnis |
| `SCAN_CIDRS` | – | zusätzlich per API-Port durchsuchte Netze, z. B. `192.168.1.0/24, 10.0.20.0/24` (je höchstens /20) |
| `POLL_INTERVAL` | `5m` | wie oft alle Geräte ausgelesen werden |
| `API_TLS` | `false` | API-SSL (8729) statt 8728 |
| `API_PORT` | 8728/8729 | abweichender API-Port |
| `BACKUP_TIME` | `04:00` | tägliches Backup aller Geräte (Ortszeit), `off` = aus |
| `UPDATE_REPO` | dieses Repository | Quelle für das Selbst-Update, `off` = keine Prüfung |
| `AUTO_UPDATE` | `false` | neue Releases ohne Klick installieren (nur systemd) |
| `TZ` | – | Zeitzone, z. B. `Europe/Berlin` (für `BACKUP_TIME`) |

## 11. Daten und Sicherheit

| Datei (im Datenverzeichnis) | Inhalt |
|---|---|
| `secret.key` | AES-256-Schlüssel für die Zugangsdaten |
| `credentials.json` | Zugangsdaten zu den Geräten und zur Oberfläche (verschlüsselt) |
| `web-password.txt` | erzeugtes Passwort der Oberfläche (nur ohne `WEB_PASSWORD`) |
| `devices.json` | bekannte Geräte |
| `ssh_known_hosts.json` | gemerkte SSH-Schlüssel der Geräte |
| `backups/<Geräte-ID>/` | Backups – **enthalten Passwörter und WLAN-Schlüssel** |
| `wlan-central.json` | zentrale WLAN-Konfiguration – **enthält WLAN-Passwörter** |
| `migrations/` | Fortschritt laufender Umstellungen wireless → wifi |
| `auto-update.json` | Einstellung und letztes Ergebnis der automatischen Updates |
| `topology-root.json` | gewählte Wurzel des Netzwerk-Baums |
| `wwan.json` | WWAN-Einstellung je Gerät (Netz, APN; PINs nur im Tresor) |
| `wwan-usage/<Geräte-ID>.json` | Volumen über LTE je Monat |
| `history/<Geräte-ID>.jsonl` | Verlauf je Gerät: wer hat über den Manager was ausgelöst (gekürzt auf die neuesten ~1000) |

- Alle Dateien sind nur für den Dienst lesbar (0600/0700). Das Datenverzeichnis wie
  andere vertrauliche Daten sichern.
- Die Oberfläche nutzt HTTP mit Basic Auth. Nur im eigenen Netz betreiben; von außen
  nur über VPN oder einen Reverse-Proxy mit HTTPS.
- Schreibende Aufrufe der Oberfläche verlangen JSON (Schutz gegen Cross-Site-Requests).
- Der Manager spricht mit den Geräten über die RouterOS-API (ohne `API_TLS`
  unverschlüsselt im LAN) und SSH.

## 12. Fehlersuche

| Anzeige | Ursache / Abhilfe |
|---|---|
| API „nicht erreichbar“ | API-Dienst aus, Port gefiltert oder auf andere Adressen beschränkt (`/ip service`) |
| API „Login“ | Zugangsdaten falsch → Login-Knopf in der Zeile oder Standard-Zugangsdaten |
| Gerät wird nicht gefunden | anderes Netzsegment → `SCAN_CIDRS`; MNDP auf dem Gerät aus (`/ip neighbor discovery-settings`) |
| Meshlink-AP kurz „nicht erreichbar“ (Port 8728 / i/o timeout) | Normal während des Umschaltens Kabel ↔ Meshlink (20–50 s); mit „↻“ neu auslesen. Länger als 2 min: Detailseite → WLAN → `mm-mesh-sta` prüfen (gleicher fester 5-GHz-Kanal an allen APs?) |
| Backup: SSH-Schlüssel geändert | Gerät neu aufgesetzt? In der Geräteliste **SSH-Schlüssel übernehmen**. Wurde am Gerät nichts gemacht: erst Fingerabdruck prüfen (`ssh-keyscan <IP> \| ssh-keygen -lf -`) |
| Umstellung: Paket nicht angeboten | Gerät hat keinen Internetzugang zum MikroTik-Server |
| Clients eines VLAN-WLANs ohne Netz | Switch-Port liefert das VLAN nicht getaggt |
| „eine frühere Änderung ist noch nicht bestätigt“ | Wächter einer abgebrochenen Änderung läuft noch – höchstens 5 Minuten warten |

Protokoll: systemd `journalctl -u mikrotik-manager -f`, Docker `docker logs -f mikrotik-manager`.
Jede Aktion hat zusätzlich ein Protokoll in der Gerätezeile („Protokoll“ aufklappen).

## 13. Grenzen

- Laufende Aktionen leben im Speicher: Ein Neustart des Managers (auch ein Update)
  bricht sie ab – das Sicherheitsnetz auf dem Gerät greift trotzdem.
- RouterOS 6 → 7 wird nicht automatisch migriert.
- IP-Adressen der WLAN-Clients beim `wifi`-Paket nur, wenn der Access Point sie in
  seiner ARP-Tabelle hat.
- CAPsMAN-verwaltete Geräte werden beim WLAN nicht angefasst.
