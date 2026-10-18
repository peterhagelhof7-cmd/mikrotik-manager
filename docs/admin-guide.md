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
  später, verweigert der Manager das Backup.
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

**Wiederherstellen:** `.backup`-Datei in WinBox/WebFig unter *Files* hochladen →
*System → Backup → Restore* (Gerät startet neu). Ein Backup gehört zu genau diesem
Gerät (enthält MAC-Adressen).

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
Stelle fort. Abgelehnt wird bei Station-/WDS-Modus, VLAN-Modus, EAP/RADIUS/WEP,
CAPsMAN und zu wenig Flash.

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
| Backup: SSH-Schlüssel geändert | Gerät neu aufgesetzt? Eintrag in `ssh_known_hosts.json` löschen |
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
