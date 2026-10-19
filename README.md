# mikrotik-manager

Findet **alle MikroTik-Geräte im Netz** (MNDP, optional API-Scan) und verwaltet sie
über eine Weboberfläche – gedacht für Heim- und Kleinnetze mit mehreren Access Points.
Ein einzelnes Go-Programm, läuft als systemd-Dienst (z. B. Proxmox-LXC) oder im
Docker-Container (z. B. Synology).

- **Übersicht:** Modell, RouterOS, Firmware, WLAN-Paket, Laufzeit, API-Status,
  WLAN-Clients je Gerät und über alle Geräte (SSID, Band, Signal, Rate)
- **Updates:** RouterOS und RouterBOARD-Firmware, Neustart – nacheinander, mit Backup vorab;
  **automatisch zu einer festen Zeit** (Wochentage wählbar, Firmware per eigenem Haken)
- **Backups** auf den Manager: jede Nacht, vor jeder Änderung und auf Knopfdruck
  (`.backup` + `.rsc`, Download in der Oberfläche)
- **Grundeinstellungen:** Name, alle Ports in eine Bridge, DHCP-Client auf der Bridge,
  Verwaltungsbenutzer und Passwörter auf allen Geräten
- **WLAN:** SSID, Passwort, Verschlüsselung (WPA2/WPA3), an/aus, Band, Kanal, Breite,
  DFS, Land – je Gerät, für alle Geräte gleichzeitig oder als
  **zentrale WLAN-Verwaltung** mit VLAN je SSID; Empfehlung per Knopf
- **Umstellung** vom alten `wireless`- auf das neue `wifi`-Paket
- **Sicherheitsnetz:** Änderungen mit Vorschau, Backup und Wächter auf dem Gerät, der
  ohne Bestätigung nach 5 Minuten selbst zurückrollt
- **Selbst-Update** über GitHub-Releases (Prüfsumme + Probestart)

## Schnellstart

### Debian / Ubuntu ohne Docker (systemd)

Gilt für jeden Rechner mit systemd: Proxmox-LXC, VM oder normaler Server
(Details: [docs/proxmox.md](docs/proxmox.md)). Als root:

```sh
apt update && apt install -y curl
curl -fsSL https://github.com/peterhagelhof7-cmd/mikrotik-manager/releases/latest/download/install.sh -o install.sh
sh install.sh
```

Das Script lädt das Release für die passende Architektur (amd64/arm64/armv7), prüft die
SHA256-Prüfsumme, legt den Dienst-Benutzer `mikrotik-manager` an und startet den Dienst.
Bestimmte Version: `VERSION=v0.13.0 sh install.sh`, eigener Build:
`sh install.sh ./mikrotik-manager-linux-amd64`.

| | |
|---|---|
| Programm | `/opt/mikrotik-manager/` (beschreibbar für das Selbst-Update) |
| Einstellungen | `/etc/mikrotik-manager.env` (wird bei Updates nie überschrieben), danach `systemctl restart mikrotik-manager` |
| Daten | `/var/lib/mikrotik-manager` (Geräte, verschlüsselte Zugangsdaten, Backups) |
| Passwort | beim ersten Start erzeugt: `/var/lib/mikrotik-manager/web-password.txt` |
| Protokoll | `journalctl -u mikrotik-manager -f` |
| Aktualisieren | Knopf in der Oberfläche, `AUTO_UPDATE=true` oder `sh install.sh` erneut |
| Zurück | `cd /opt/mikrotik-manager && mv mikrotik-manager.old mikrotik-manager && systemctl restart mikrotik-manager` |

Der Rechner muss im **selben Netz (L2) wie die MikroTiks** hängen (MNDP-Broadcast);
Geräte in anderen Netzen per `SCAN_CIDRS`. Bei aktiver Firewall freigeben: eingehend
UDP 5678 (MNDP) und TCP 8080 (Oberfläche), ausgehend TCP 8728 (API), TCP 22 (Backups)
und HTTPS zu GitHub (Selbst-Update).

### Docker

Host-Netz ist Pflicht (Details inkl. Synology: [docs/docker.md](docs/docker.md)):

```sh
curl -LO https://raw.githubusercontent.com/peterhagelhof7-cmd/mikrotik-manager/main/docker-compose.yml
docker compose up -d
```

### Danach (beide Varianten)

Oberfläche: `http://<IP>:8080`, Benutzer `admin`. Auf den MikroTiks müssen der
API-Dienst (`/ip service` → `api`) und SSH (für Backups) an sein.

## Dokumentation

- [Admin-Guide](docs/admin-guide.md) – Einrichtung, alle Funktionen, Einstellungen, Fehlersuche
- [Proxmox / systemd](docs/proxmox.md)
- [Docker / Synology](docs/docker.md)
- [Entscheidungen](docs/entscheidungen.md) – warum etwas so gebaut ist

## Selbst bauen

```sh
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -tags timetzdata \
  -ldflags "-X main.version=dev" -o mikrotik-manager-linux-amd64 .
sudo sh deploy/install.sh ./mikrotik-manager-linux-amd64
```

Releases entstehen per Tag `vX.Y.Z` (GitHub Actions: Tests, Programme für
amd64/arm64/armv7 mit `SHA256SUMS`, Docker-Image auf GHCR).

## Lizenz

[MIT](LICENSE)
