# mikrotik-manager mit Docker

Das Image `ghcr.io/peterhagelhof7-cmd/mikrotik-manager` gibt es für `amd64`,
`arm64` und `arm/v7`. Es ist ein `scratch`-Image (nur das Programm, ca. 10 MB),
läuft als unprivilegierter Benutzer und braucht etwa 20–40 MB RAM.

| Tag | Inhalt |
|---|---|
| `latest` | neuestes Release (empfohlen) |
| `0.12`, `0.12.0` … | eine bestimmte Version |
| `edge` | aktueller Entwicklungsstand (`main`), ungetestet |

## Voraussetzungen

- **Linux-Host im selben Netz (L2-Segment) wie die MikroTik-Geräte.** Die Geräte
  werden per MNDP-Broadcast (UDP 5678) gefunden – der kommt nur mit
  **Host-Netzwerk** (`network_mode: host`) im Container an. Docker Desktop unter
  Windows/macOS hat kein echtes Host-Netz; dort werden nur Geräte gefunden, die
  per `SCAN_CIDRS` gescannt werden.
- Auf den MikroTiks: API-Dienst an (`/ip service` → `api` Port 8728, oder
  `api-ssl` 8729 mit `API_TLS=true`) und SSH (Port 22, für Backups).

## Start mit Docker Compose

```sh
mkdir mikrotik-manager && cd mikrotik-manager
curl -LO https://raw.githubusercontent.com/peterhagelhof7-cmd/mikrotik-manager/main/docker-compose.yml
# WEB_PASSWORD eintragen (sonst wird eines erzeugt, siehe unten)
nano docker-compose.yml
docker compose up -d
```

Die Oberfläche ist dann unter `http://<Host-IP>:8080` erreichbar, Benutzer `admin`.

**Ohne `WEB_PASSWORD`** erzeugt der Manager beim ersten Start ein Passwort und legt
es im Datenvolume ab. Das Image enthält keine Shell, deshalb so auslesen:

```sh
docker cp mikrotik-manager:/data/web-password.txt - | tar -xO
```

Weitere Einstellungen (Port, Scan-Netze, Backup-Zeit, API-SSL …) stehen als
Kommentare in der `docker-compose.yml` und im [Admin-Guide](admin-guide.md#einstellungen).

## Aktualisieren

Ein Container ersetzt sich nicht selbst. Die Weboberfläche zeigt an, wenn ein neues
Release da ist; aktualisiert wird das Image:

```sh
docker compose pull && docker compose up -d
```

Daten (Geräte, verschlüsselte Zugangsdaten, Backups, zentrale WLAN-Konfiguration)
liegen im Volume `mikrotik-data` und bleiben erhalten.
Wer automatisch aktualisieren möchte, kann z. B. Watchtower verwenden.

> Vor dem Aktualisieren prüfen, dass in der Oberfläche keine Aktion läuft
> (Update, Ausrollen …) – ein Neustart des Containers bricht sie ab.

## Synology

### DSM 7.2 und neuer (Container Manager)

1. **Container Manager → Projekt → Erstellen**, Pfad z. B. `/docker/mikrotik-manager`.
2. Als Quelle „docker-compose.yml erstellen“ und den Inhalt der
   [`docker-compose.yml`](../docker-compose.yml) einfügen, `WEB_PASSWORD` setzen.
3. Erstellen und starten. Oberfläche: `http://<NAS-IP>:8080`.

Aktualisieren: Projekt → **Aktion → Erstellen** (lädt das neueste Image).

### DSM 7.0/7.1 (Paket „Docker“, ohne Compose)

Das ältere Docker-Paket kennt keine Compose-Projekte. Der Container wird über die
**Aufgabenplanung** angelegt – ohne SSH. Dieselbe Aufgabe aktualisiert ihn später.

1. **Systemsteuerung → Aufgabenplanung → Erstellen → Geplante Aufgabe → Benutzerdefiniertes Script**
2. Allgemein: Name `mikrotik-manager`, Benutzer **root**, Häkchen bei „Aktiviert“ entfernen
   (die Aufgabe wird nur von Hand ausgeführt).
3. Aufgabeneinstellungen → Script: den Inhalt von
   [`synology/aufgabenplanung.sh`](../synology/aufgabenplanung.sh) einfügen und
   oben `WEB_PASSWORD` eintragen. Solange dort `HIER_EINTRAGEN` steht, bricht das
   Script ab, ohne etwas anzulegen.
4. Speichern, Aufgabe markieren, **Ausführen**. Ergebnis: **Aktion → Ergebnis anzeigen**.

Aktualisieren: dieselbe Aufgabe erneut ausführen.

Die Meldung `Your kernel does not support swap limit capabilities` ist auf Synology
normal und harmlos.

## Daten sichern

Alles liegt im Volume `mikrotik-data` (`/data` im Container):

| Datei | Inhalt |
|---|---|
| `secret.key` | Schlüssel für die verschlüsselten Zugangsdaten – **ohne ihn sind `credentials.json` wertlos** |
| `credentials.json` | Zugangsdaten zu den Geräten (verschlüsselt) |
| `devices.json` | bekannte Geräte |
| `backups/` | Geräte-Backups (`.backup` + `.rsc`, enthalten Passwörter und WLAN-Schlüssel) |
| `wlan-central.json` | zentrale WLAN-Konfiguration (enthält WLAN-Passwörter) |

Das Volume wie andere vertrauliche Daten sichern und nicht öffentlich ablegen.
