# Entscheidungen

## 2026-10-18 – Grundaufbau (v0.1.0-dev, M1: Discovery + Inventar, nur lesend)

- **Go, ein Binary, zwei Betriebsarten.** Gleicher Code läuft als systemd-Dienst
  (Proxmox-LXC, `deploy/install-lxc.sh`) und als Docker-Container mit
  `network_mode: host` (z. B. DS415+, `docker-compose.yml`). Konfiguration nur
  über Umgebungsvariablen (`/etc/mikrotik-manager.env` bzw. `environment:`).
  Aufbau wie shelly-watcher (CI, GHCR, scratch-Image).
- **Discovery: MNDP (UDP 5678)** passiv + aktive Anfrage alle 2 min, an
  255.255.255.255 und an die Broadcast-Adresse jedes lokalen Netzes. Reicht nur
  im eigenen L2-Segment → optional `SCAN_CIDRS` (TCP-Connect auf den API-Port).
- **Geräteschlüssel = Software-ID** (MNDP-TLV 11 bzw. `/system/license`), nicht
  MAC/IP: MNDP meldet je Schnittstelle eine andere MAC, und die IP ändert sich
  z. B. nach „DHCP-Client auf Bridge“. Einträge, die nur per IP/MAC bekannt
  sind, werden beim ersten API-Kontakt umgeschlüsselt.
- **Zugriff über die klassische RouterOS-API (8728/8729)**, nicht REST: REST gibt
  es erst ab v7, wir müssen aber v6-Geräte migrieren. Library
  `go-routeros/routeros/v3` (kann auch den Challenge-Login vor 6.43).
- **Zugangsdaten** AES-256-GCM in `credentials.json`, Schlüssel separat in
  `secret.key` (0600); Gerätename als Additional Data. Reihenfolge: eigene
  Zugangsdaten des Geräts → Standard-Zugangsdaten → `admin` ohne Passwort.
- **Weboberfläche immer mit Passwort** (Basic-Auth). Ohne `WEB_PASSWORD` wird
  beim ersten Start eines erzeugt → `DATA_DIR/web-password.txt`. Schreibende
  Aufrufe nur mit `Content-Type: application/json` (einfacher CSRF-Schutz).
- **Test-Deployment:** Proxmox-LXC, Debian 13, unprivilegiert, nesting=1,
  1 Kern, 512 MB, 4 GB, feste IP im Netz der Geräte, onboot=1, im Backup-Job des Hosts.

### Befund beim ersten Lauf (2026-10-18)

- 4 Geräte per MNDP im Netz, **alle bereits RouterOS 7.24.5 (stable)**:
  AP1 (RB952, mipsbe, `wireless`), AP2 (cAP ac, `wireless`),
  AP3 (cAP ac, `wifi`), AP4 (hAP ac², `wifi`).
- **Alle vier nehmen `admin` ohne Passwort an** → dringend Passwort setzen
  (kommt mit M3, bis dahin manuell).
- RouterBOARD-Firmware 7.24.1 → 7.24.5 ausstehend auf AP1, AP3, AP4.
- v6-Geräte wurden in diesem Segment nicht gefunden.

## Geplant: v6 → v7-Migration (M2)

Ablauf je Gerät, seriell, mit Backup davor und Health-Check danach:

1. `/system backup save` + `/export` herunterladen (Rückfall).
2. Ist v6 < 6.49.x: zuerst auf die neueste 6.49 (Kanal `long-term`) – nur von
   dort ist der Sprung auf v7 vorgesehen. Reboot, prüfen.
3. Kanal `upgrade` setzen → `check-for-updates` → `install` (lädt v7 stable).
   Reboot, prüfen (Version, Erreichbarkeit per Software-ID, ggf. neue IP).
4. `/system routerboard upgrade` → Reboot → Firmware prüfen.
5. Kanal danach auf `stable` stellen.

Vorab je Gerät prüfen: freier Flash (16-MB-Geräte wie hAP lite/mAP lite sind
knapp), Architektur (PowerPC-Geräte gibt es unter v7 nicht), WLAN-Paket
(`wireless` bleibt unter v7 für ältere Hardware; `wifi-qcom-ac` nur bewusst
umstellen, die WLAN-Konfiguration wird dabei NICHT übernommen).

## 2026-10-18 – Verwaltungsbenutzer + Passwörter (v0.2.0-dev)

- Eigener Benutzer **`mikrotik-manager`** (Gruppe full, 20-stelliges
  Zufallspasswort) auf jedem Gerät. Er wird erst nach erfolgreicher Gegenprobe
  zur Standard-Anmeldung; danach entfallen gerätespezifische Zugangsdaten.
  So hängt der Manager nicht am admin-Passwort.
- Das Passwort wird **auf Wunsch im Webinterface im Klartext angezeigt**
  (Karte „Verwaltungsbenutzer“). Vertretbar, weil die Oberfläche selbst
  passwortgeschützt ist.
- Massenaktion „Benutzer-Passwort setzen“ (`/user set`), mit Gegenprobe per
  Anmeldung. Sie verweigert den Benutzer, mit dem sich der Manager selbst
  anmeldet (Aussperrschutz).
- Live ausgeführt: Verwaltungsbenutzer auf allen 4 Geräten angelegt und geprüft;
  **admin-Passwort auf allen 4 gesetzt**, extern per REST geprüft (leer → 401,
  neues → 200).
- Laut User gibt es in der Umgebung keine v6-Geräte mehr.

## 2026-10-18 – Updates + Neustart (v0.3.x-dev)

- Update-Job je Gerät: Backup → `check-for-updates` → ggf. `update/install`
  (Neustart, Warten, Version prüfen) → bei abweichender Firmware
  `routerboard/upgrade` + Neustart → Firmware prüfen. Jobs laufen nacheinander
  im Hintergrund, im Dienst-Kontext (nicht am HTTP-Request). Ein Panic in einem
  Job beendet nur den Job, nicht den Dienst.
- „Wieder da“ = API erreichbar UND Uptime kleiner als vorher. Die IP kommt bei
  jedem Versuch neu aus dem Store (MNDP), falls DHCP eine andere vergibt.
- Für Update/Neustart müssen die Geräte ausdrücklich angegeben werden (kein
  „alle“ als Standard). Die Weboberfläche fragt vor jedem Klick nach.
- **Bug (behoben, 0.3.1):** `verNum("")` hat den Dienst mit einem Panic beendet
  (latest-version vor dem Check leer). Gerät war dabei unberührt.
- **Bug (behoben, 0.3.2):** Backup ohne `flash/`-Präfix lag auf 16-MB-Geräten
  (RB952) im RAM und war nach dem Neustart weg. Jetzt `flash/`, wenn vorhanden,
  plus Existenzprüfung; fehlt die Datei, bricht das Update ab.
- **Live-Test AP1 (RB952):** Firmware 7.24.1 → 7.24.5, Neustart ~30 s,
  per REST gegengeprüft. RouterOS-Install-Pfad noch NICHT live getestet (alle
  Geräte schon auf der neuesten Version).
- Weboberfläche: Statusleiste für laufende und zuletzt beendete Aktionen (15 min),
  Infotext (title) an jeder Schaltfläche.

## 2026-10-18 – Backups auf dem Manager (v0.4.0-dev)

- User-Wunsch: Backups nicht auf dem Gerät, sondern auf dem Manager.
- Ablauf: `/system backup save` (unverschlüsselt, in `flash/` falls vorhanden)
  → Download per **SFTP** (Port 22, Verwaltungsbenutzer) → Größenvergleich →
  Gerätekopie löschen (auch bei Fehlschlag). Zusätzlich `/export show-sensitive`
  (v6: `/export`) per SSH als `.rsc` (lesbar, diffbar).
- Ablage: `DATA_DIR/backups/<Software-ID>/<JJJJMMTT-hhmmss>-<anlass>.{backup,rsc}`,
  Dateien 0600, die letzten 20 Sicherungen je Gerät. Liegt damit auch im
  nächtlichen Proxmox-Backup des CT auf dem NAS (bzw. im Docker-Volume).
- Die Dateien enthalten Geheimnisse (WLAN-Schlüssel, Benutzer) → nur über die
  passwortgeschützte Weboberfläche herunterladbar, Namen per Regex geprüft.
- SSH-Hostkeys: Trust-on-first-use in `ssh_known_hosts.json`; ein geänderter
  Key wird abgelehnt (nach Netinstall/Reset den Eintrag löschen).
- Update bricht ab, wenn das Backup nicht auf dem Manager ankommt.
- Live geprüft am AP1: manuelles Backup (32 KB + 2,5 KB .rsc), Download
  bitgleich (SHA-256), Gerätekopie entfernt, Update-Ablauf mit Backup ok.

## 2026-10-18 – Nächtliches Backup (v0.5.0-dev)

- `BACKUP_TIME` (Standard `04:00`, Ortszeit; `off` = aus): täglich Backup aller
  Geräte mit funktionierender API auf den Manager, nacheinander, Anlass `nachts`.
  Termin per `AddDate` (stimmt über die Zeitumstellung, Test vorhanden). Ein
  verpasster Termin (Dienst aus) wird nicht nachgeholt.
- Weboberfläche: nächster Lauf in der Fußzeile, Kachel „ohne Backup (36 h)“.
- Live getestet mit Testzeit 16:11: alle 4 Geräte gesichert, keine Reste auf den
  Geräten; danach wieder 04:00.
- Firmware-Updates AP3 (cAP ac) und AP4 (hAP ac²) 7.24.1 → 7.24.5 über
  den Manager durchgeführt und per REST gegengeprüft. Alle 4 Geräte jetzt
  RouterOS 7.24.5 / Firmware 7.24.5.

## 2026-10-18 – Name, Bridge, DHCP-Client (v0.6.0-dev)

- **Name:** `/system identity set` direkt, mit Gegenprobe; nur ASCII, 1–64 Zeichen.
- **Bridge und DHCP mit Sicherheitsnetz (Commit-Confirm):** Vorschau (ändert
  nichts) → Backup auf den Manager → Skripte `mm-undo`/`mm-apply` auf dem Gerät →
  Wächter per `/execute` (`:delay 300s`, führt `mm-undo` aus, falls es noch
  existiert) → `mm-apply` im Hintergrund → Neuverbindung (neue IP, alte IP,
  MNDP; Software-ID muss passen) → erneute Planung muss „nichts zu tun“ ergeben
  → `mm-undo` löschen = bestätigt. Rückweg-Befehle in `:do {} on-error={}`.
- **Bridge:** Ethernet + physische WLANs; IPs/DHCP-Clients der Ports wandern auf
  die Bridge. Verweigert bei aktivem DHCP-Server (Router). Ports aus
  Schnittstellenlisten (`interface=all`, eigene Listen) zählen als drin.
  **Virtuelle WLANs (master-interface) werden nie automatisch aufgenommen**
  (Gästenetz-Gefahr), nur als Hinweis gezeigt.
- **DHCP:** Client auf der ersten Bridge; feste IP/Default-Route werden erst
  entfernt, wenn der Client „bound“ ist (40 s), sonst bleibt alles wie es war.
- Vorschau live auf allen 4 Geräten geprüft: Bridge überall „nichts zu tun“;
  DHCP würde .7–.10 durch DHCP-Adressen ersetzen.
- **Befund AP1:** virtuelle WLANs `Gast` (wlan3/wlan4), `Heim`,
  `Heim-5` senden, hängen aber an keiner Bridge und haben keine IP →
  Clients bekommen dort kein Netz. Nicht angefasst, User informiert.
- Live-Test AP1 (mit OK des Users): Umbenennen hin und zurück ok; DHCP ohne
  Entfernen der festen IP ok (gebunden nach 8 s, bestätigt, Wächter lief ohne
  Rückrollung ab), danach DHCP-Client auf Wunsch wieder entfernt. Nicht live
  getestet: tatsächliche Rückrollung, `removeStatic`, Bridge mit Änderungen.

## 2026-10-18 – WLAN-Konfiguration + Umstellung wireless → wifi (v0.7.0-dev)

- **Beide Pakete:** altes `wireless` (AP1, AP2) und neues `wifi`
  (AP3, AP4). Gelesen werden die wirksamen Werte (`/interface/wifi/print`
  zeigt geerbte Profilwerte mit an); Band/Chip beim wifi-Paket aus `/interface/wifi/radio`.
- **Änderbar:** SSID, Passwort, an/aus – je Schnittstelle oder „im ganzen Netz“
  (alle Schnittstellen mit einer SSID auf allen Geräten, nacheinander). Läuft über
  denselben Commit-Confirm-Weg wie Bridge/DHCP (Backup, Wächter, Prüfung per
  Neuplanung = „nichts zu tun“). Auswahl per SSID wird vor dem Ändern auf
  Schnittstellennamen festgelegt (sonst wäre die Prüfung nach Umbenennen leer).
- **Profile:** Benutzen genau die geänderten Schnittstellen ein Profil und
  bekommen alle denselben Wert → Profil ändern. Sonst nur die Schnittstelle
  (wireless: neues Sicherheitsprofil `mm-N`; wifi: Wert direkt an der
  Schnittstelle), damit z. B. `Heim-5` nicht ungewollt mitwandert – auf
  AP3/AP4 teilen sich beide Netze `sec1`.
- **Validierung:** SSID 1–32, Passwort 8–63 Zeichen, nur druckbares ASCII.
  Passwörter stehen nicht in Plan-Schritten/Job-Protokoll. Plan-Endpunkt ist jetzt
  POST (Passwörter gehören nicht in die URL).
- **Umstellung wireless → wifi-qcom-ac:** nur 802.11ac-Wave-2-Chips (IPQ4018/4019,
  QCA9984/9888); AP1 (AR9300/AR9888, mipsbe) bleibt auf wireless.
  Ablauf in zwei Neustarts, weil der Flash (16 MB, AP2 930 KB frei) nicht
  beide Pakete gleichzeitig fasst: wireless deinstallieren → Neustart →
  `/system/package/enable wifi-qcom-ac` + `apply-changes` (Download) → Neustart →
  übersetzte Konfiguration (Profile `secN`/`cfgN` wie auf AP3, Kanal-Band/
  -Breite/-Frequenz, Land, Bridge-Ports inkl. PVID, Schnittstellenlisten) als Skript
  einspielen und prüfen. Skript liegt zusätzlich als `…-wifi-config.rsc` bei den
  Backups. Zuordnung alt→neu per MAC, sonst per Band.
  Scheitert die Paket-Installation: wireless wieder installieren, Backup per SFTP
  hochladen, `/system backup load` → alter Zustand.
  Verweigert bei: Nicht-AP-Modus, WDS, VLAN-Modus, EAP/RADIUS/WEP, CAPsMAN, zu
  wenig Flash (< 64 KB Rest), Paket nicht angeboten. Access-/Connect-List → Hinweis.
- Live geprüft (nur lesend/Vorschau): WLAN aller 4 Geräte, Umstellungs-Vorschau
  (AP1 blockiert wegen Chip, AP2 möglich – danach 116 KB frei,
  AP3 „nichts zu tun“), Profil-Logik und „im ganzen Netz“. Ausführung noch
  nicht – braucht OK des Users.

## 2026-10-18 – Erste echte Umstellung AP2, Fehler + Fortsetzbarkeit (v0.7.1-dev)

- **Fehler im ersten Lauf (17:54):** Nach dem Deinstallieren von wireless und dem
  Neustart war die Liste installierbarer Pakete leer – RouterOS füllt sie erst nach
  `check-for-updates` wieder. `enable wifi-qcom-ac` und der Rückweg `enable wireless`
  fanden das Paket nicht → Gerät ohne WLAN-Paket. In der Vorschau unsichtbar, weil
  die Liste vor dem ersten Neustart noch gefüllt war.
- **Fix:** `pkgChange` ruft bei fehlendem Paket `check-for-updates` auf und wartet
  bis 30 s, bis die Liste gefüllt ist.
- **Fortsetzbar:** Der Fortschritt (übersetzte WLAN-Konfiguration + Backup-Pfad)
  liegt ab Stufe 1 in `DATA_DIR/migrations/<id>.json` (0600, enthält Passwörter).
  Die Vorschau erkennt: wifi da + Stand → nur Konfiguration einspielen; kein
  WLAN-Paket + Stand → wifi installieren; kein WLAN-Paket ohne Stand → wireless
  wieder installieren, letztes `…-vor-wifi.backup` einspielen, dann neu umstellen.
- **Rückroll-Skripte je Job eindeutig** (`mm-undo-<hhmmss>`): Der Wächter einer
  früheren Änderung konnte sonst den Rückweg einer späteren (≤ 5 min danach)
  ausführen. Liegt noch ein unbestätigtes `mm-undo*`, wird eine neue Änderung
  abgelehnt, bis sein Wächter fertig ist.
- **Live (18:00–18:04, mit OK des Users):** Manager hat den halben Zustand selbst
  erkannt und behoben: wireless neu installiert, Backup eingespielt (damit den
  Rückweg real getestet), dann Umstellung komplett: wlan1→wifi1 (Heim,
  2,4 GHz), wlan2→wifi2 (Heim-5, 5 GHz), Passwort identisch (Hash
  verglichen), beide an bridge1, 104 KB Flash frei. Clients kommen zurück.

## 2026-10-18 – Funkwerte: Band, Breite, Kanal, DFS, Land (v0.8.0-dev)

- Je Funkteil (nur physische Schnittstellen): Band, Kanalbreite, Kanal (fest oder
  automatisch), DFS-Kanäle auslassen; je Gerät das Land. Anzeige zusätzlich des
  tatsächlich genutzten Kanals (`monitor once`).
- **Wählbare Werte:** wifi-Paket aus `/interface/wifi/radio` (`bands` inkl. Breiten,
  `2g-channels`/`5g-channels`); wireless kennt das per API nicht → feste Listen
  (EU-Kanäle 1–13, 36–64, 100–140; ac-Bänder nur bei ac-fähigem Chip). Länder:
  feste Liste (UI und Server gleich), damit Tippfehler nicht erst am Gerät scheitern.
- **Schreiben:** wireless direkt an der Schnittstelle; wifi inline `channel.*`,
  „automatisch“/„Standard“ = `unset value-name=…`. Hängt am Funkteil ein
  Kanalprofil, wird abgelehnt (dort ändern). Land beim wifi-Paket über die
  Konfigurationsprofile (einmal je Profil), sonst inline.
- **Prüfung schlägt fehl → sofort zurückrollen** (statt 5 Minuten auf den Wächter
  zu warten), solange der Manager das Gerät erreicht – z. B. wenn das Gerät einen
  Wert ablehnt.
- **Empfehlung (Knopf „Empfehlung eintragen“, ändert nichts):** Land Deutschland;
  2,4 GHz 20 MHz auf 1/6/11, reihum nach Gerätename verteilt (AP1 1,
  AP2 6, AP3 11, AP4 1); 5 GHz 80 MHz, Kanal automatisch,
  DFS „ohne 10-min-Radarkanäle“ (Wetterradar 120–128); Band = neuester Standard
  des Funkteils, gemischt bei wireless.
- Befund Ist-Stand: 2,4 GHz alle mit 40 MHz auf 6/11/2/3 (überlappen); 5 GHz
  AP1 und AP3 beide auf 128 (10-min-Radarkanal).
- UI per Headless-Chrome geprüft (lokaler Proxy mit Basic Auth, Testseite öffnet
  Dialog, „Empfehlung eintragen“, Vorschau). Ausführung am Gerät noch nicht.

## 2026-10-18 – Verschlüsselung einstellbar, Empfehlung WPA2/WPA3 (v0.8.1-dev)

- Je WLAN wählbar: wifi-Paket WPA2/WPA3 (empfohlen), nur WPA3, nur WPA2;
  wireless-Paket WPA2, WPA/WPA2 (kein WPA3 möglich). Auch in „WLAN im ganzen Netz“.
- User-Wunsch „Empfehlung WPA3 /wpa3“ als **WPA2/WPA3 gemischt** umgesetzt (neue
  Geräte WPA3, ältere weiter WPA2, auf allen APs gleich fürs Roaming); „nur WPA3“
  als Option. Bei Bedarf Empfehlung auf „nur WPA3“ umstellen = eine Zeile in index.html.
- wireless + WPA2/WPA3 → WPA2 mit Hinweis (damit „im ganzen Netz“ überall geht);
  wireless + nur WPA3 → abgelehnt.
- Verschlüsselung und Passwort werden als Paar geplant (gleiche Profil-Logik wie
  vorher das Passwort): alle Benutzer eines Profils gleich → Profil, sonst
  Schnittstelle (wireless: eigenes Profil mm-N). Reihenfolge egal (normAuth).
- Ist-Stand: AP3/AP4 schon WPA2/WPA3; AP2 (nach Umstellung
  1:1 übernommen) und AP1 WPA2.

## 2026-10-18 – Zentrale WLAN-Verwaltung (v0.9.0-dev)

- Eigener Tab. Konfiguration auf dem Manager (`DATA_DIR/wlan-central.json`, 0600):
  aktiviert, Geräteauswahl, Land, bis 8 WLANs (SSID, Passwort, Verschlüsselung,
  Bänder beide/2,4/5, versteckt), Funk je Band (an, Breite, Kanal, DFS).
  Default = Empfehlung (2,4: 20 MHz Kanalplan 1/6/11 reihum nach Name; 5: 80 MHz
  auto ohne 10-min-DFS; Deutschland; WPA2/WPA3). „von Gerät übernehmen“ liest die
  WLANs eines Geräts ein.
- **Speichern ändert nichts an Geräten.** „Speichern & prüfen“ plant jedes
  ausgewählte Gerät (auch ohne Aktivierung) und zeigt Abweichungen.
  **Ausrollen** nur bei Aktivierung (Server prüft), nur abweichende Geräte,
  nacheinander als Job „central“.
- **Layout je Gerät:** je Band trägt das Funkteil das erste passende WLAN, weitere
  werden virtuelle WLANs (`<funkteil>-vN`) an der ersten Bridge. Band ohne WLAN oder
  abgeschaltet → Funkteil aus.
- **Zurücksetzen:** wifi: virtuelle WLANs entfernen, an den Funkteilen alle
  `configuration*`/`security*`/`channel*`-Werte unsetten (aus dem print gelesen),
  alle configuration/security/channel-Profile entfernen (datapath bleibt, wegen
  Bridge-Zuordnung), neu `mm-secN`/`mm-cfgN`. wireless: virtuelle entfernen,
  Profile außer default entfernen, `mm-secN`, Werte direkt am Funkteil; WPA3 → WPA2.
- **Rückweg:** zusätzlich zum Backup auf dem Manager ein Backup auf dem Gerät
  (`flash/mm-undo-<tag>.backup`); mm-undo = `/system backup load` (Neustart).
  Prüfung = Neuplanung „entspricht“. Scheitert sie → sofort Backup laden.
- **Apply-Skripte allgemein** jetzt in `:do {} on-error={}` mit Logmeldung
  `mm-apply-done|failed <tag>`; `waitApplied` wartet darauf statt fester 8 s.
- **Sperre:** Ist die Verwaltung aktiv, lehnt die WLAN-Einzeländerung für
  ausgewählte Geräte ab (Hinweis im WLAN-Dialog, Badge „zentral“ in der Liste).
  Umstellung wireless→wifi bleibt erlaubt (danach neu ausrollen).
- Live: Konfiguration mit den WLANs von AP3 gespeichert (NICHT aktiviert),
  Prüfung aller 4 Geräte ok; Ausrollen noch nicht getestet. AP1 würde dabei
  `Heim-AP1` und `Gast` verlieren (stehen nicht in der Vorgabe).

## 2026-10-18 – WLAN-Clients in der Übersicht (v0.10.0-dev)

- Registrierungstabellen aller Geräte jede Minute lesen (nur lesend, beide Pakete),
  im Speicher halten; `/api/devices` liefert je Gerät `wlanClients` (+ Zeitpunkt,
  Fehler – dann bleibt die letzte Liste mit Kennzeichen „alt“).
- Gerätezeile: „▸ N Clients“ klappt die Verbindungen auf (SSID/Schnittstelle, Band,
  Signal mit gut/mittel/schwach, Rate senden/empfangen, Dauer, MAC, IP).
  Kachel „WLAN-Clients“ = Summe; Klick → alle Verbindungen über alle Geräte,
  sortierbar, „jetzt aktualisieren“.
- Rate vereinheitlicht: wifi liefert Bit/s, wireless Text → Mbit/s.
- IP: wireless `last-ip`; wifi nur aus der ARP-Tabelle des APs (meist leer, da
  reine Bridge). Bessere Quelle wären die DHCP-Leases des DHCP-Servers (Gateway).
- **Ergebnis des Apply-Skripts jetzt über globale Variable `mmApplied`** statt Log:
  Das Log ist auf den Geräten per API leer → `waitApplied` lief bisher immer in die
  90 s; die Prüfung war trotzdem korrekt.
- Beobachtet: Der User hat die zentrale Verwaltung um 18:49–19:13 selbst aktiviert
  und auf alle 4 Geräte ausgerollt (Heim nur 2,4, Heim-5 nur 5,
  Heim-AP1 + Gast beide) – alle bestätigt. Deploys von Claude kamen danach,
  aber Jobs liegen nur im Speicher → **vor Deploy prüfen, dass kein Job läuft**.

## 2026-10-18 – VLAN je SSID in der zentralen WLAN-Verwaltung (v0.11.0-dev)

- Je zentralem WLAN optional eine VLAN-ID (2–4094; leer/1 = keins).
- Umsetzung für beide Pakete gleich über die Bridge (statt wifi-datapath bzw.
  wireless vlan-mode): WLAN-Port `pvid=<VLAN>`, Bridge-VLAN-Eintrag mit
  `tagged=<alle Ethernet-Ports der Bridge>` und Kommentar `mikrotik-manager`,
  `vlan-filtering=yes` als letzter Schritt. Verwaltung des Geräts bleibt
  ungetaggt (PVID 1 der Bridge). Eigene Bridge-VLANs des Users bleiben
  unangetastet; vlan-filtering wird nie automatisch wieder abgeschaltet.
- Prüfung vergleicht zusätzlich PVID der WLAN-Ports, VLAN-Filter und die vom
  Manager angelegten Bridge-VLANs (fehlt / andere tagged-Ports / nicht mehr
  gebraucht). Blockiert ohne Bridge bzw. ohne Ethernet-Port in der Bridge.
- Hinweise in der Vorschau: Switch-Port muss das VLAN getaggt liefern (das merkt
  die Prüfung nicht); VLAN-Filter kann HW-Offload abschalten.
- Live: Prüfung aller 4 Geräte mit der aktuellen (VLAN-losen) Vorgabe →
  „entspricht“ überall, also keine Fehlalarme. Mit echtem VLAN noch nicht ausgerollt.

## 2026-10-18 – Veröffentlichung, Selbst-Update, Installation (v0.12.0)

- **Öffentlich auf GitHub**, MIT wie das Schwesterprojekt shelly-watcher. Neue
  Historie ohne die Entwicklungs-Commits (die enthielten Netz- und Gerätedetails);
  Gerätenamen, SSIDs und Adressen in Doku und Tests durch Platzhalter ersetzt.
- **Selbst-Update** (`selfupdate.go`): GitHub-Release alle 6 h prüfen; bei
  systemd-Installation Programm laden, gegen `SHA256SUMS` prüfen, Probestart
  `mikrotik-manager version`, altes als `.old` behalten, `exec` auf das neue
  (gleiche PID, systemd merkt nichts). Nicht während eines Jobs. `AUTO_UPDATE=true`
  installiert ohne Klick. Docker (`MM_DOCKER=1` im Image): nur Hinweis + Befehle.
- **Programm nach `/opt/mikrotik-manager`** (Unit: `ReadWritePaths`), weil
  `ProtectSystem=strict` `/usr/local/bin` schreibschützt. `deploy/install.sh`
  ersetzt `install-lxc.sh`: lädt Release + prüft Prüfsumme oder nimmt eine lokale
  Datei, zieht alte Installationen um.
- **CI:** Tag `vX.Y.Z` → Programme amd64/arm64/armv7 + `SHA256SUMS` + `install.sh`
  als Release-Assets; Docker-Image wie bisher (`latest` nur für Releases).
- Doku: `docs/admin-guide.md`, `docs/proxmox.md`, `docs/docker.md`,
  `synology/aufgabenplanung.sh` (DSM 7.0/7.1 ohne Compose).

## 2026-10-18 – Automatische Updates (v0.13.0)

- Eigener Tab: aktiviert, Uhrzeit, Wochentage, Haken „RouterBOARD-Firmware mit
  aktualisieren“, Geräteauswahl (leer = alle); gespeichert in `auto-update.json`
  (in der Oberfläche änderbar, nicht per Umgebungsvariable).
- Lauf = `updateDevice(firmware)` je Gerät über die normale Job-Reihe (nacheinander).
  **Update prüft jetzt zuerst** (check-for-updates) und macht Backup/Installation nur
  bei echtem Update – gilt auch für den Update-Knopf. Ohne Firmware-Haken bleibt
  ausstehende Firmware stehen und wird im Ergebnis genannt.
- Planung: alle 30 s prüfen, ob seit dem letzten Blick ein Termin lag
  (`nextAutoAt(letzter Blick)` ≤ jetzt) – geänderte Einstellungen gelten sofort,
  DST-fest wie das nächtliche Backup. Läuft noch ein Job (Backup), bis 2 h warten.
- Ergebnis je Gerät (letzter Schritt des Jobs) wird mit Zeitpunkt gespeichert und im
  Tab angezeigt; „Jetzt ausführen“ startet einen Lauf von Hand.

## 2026-10-19 – Umstellung mit wireless vlan-mode=use-tag

- Anlass: AP mit Gästenetz (virtuelle WLANs auf beiden Funkteilen) in VLAN 178 per
  `vlan-mode=use-tag`, Bridge ohne VLAN-Filter. Umstellung war gesperrt.
- `wifi-qcom-ac` kann nicht an der WLAN-Schnittstelle taggen (MikroTik-Doku:
  „802.11ac chipsets do not support this type of VLAN tagging“, kein
  `datapath vlan-id` für diese Geräte). VLAN geht nur über die Bridge.
- Entscheidung (User): **eigene Bridge statt VLAN-Filter** – `vlanN` auf der
  bisherigen Bridge + `bridge-vlanN` mit `vlanN` und den WLANs. Die bisherige
  Bridge bleibt unverändert (kein Filter, andere getaggte VLANs laufen weiter
  durch, HW-Offload bleibt). VLAN-Schnittstelle bewusst auf der Bridge, nicht auf
  dem Ethernet-Port (Port gehört zur Bridge, VLAN-Schnittstelle auf einem
  Bridge-Port bekommt die Pakete nicht zuverlässig).
- Skript legt Bridge/VLAN/Port mit `:if` an → erneutes Einspielen doppelt nichts.
  Prüfung nach dem Einspielen: VLAN-Schnittstelle mit ID und Elternbridge.
- Gesperrt bleibt: Bridge schon mit VLAN-Filter, VLAN-ID schon als Schnittstelle
  vorhanden, Namen `vlanN`/`bridge-vlanN` vergeben, WLAN an keiner Bridge, ein VLAN
  an zwei Bridges, `use-service-tag`.
- Offen: Die zentrale WLAN-Verwaltung kennt diesen Aufbau nicht; „VLAN je SSID“
  würde die WLANs auf die Haupt-Bridge mit VLAN-Filter umbauen. Live noch nicht
  ausgeführt.
