# mikrotik-manager

Findet **alle MikroTik-Geräte im Netz** (MNDP, optional API-Scan) und verwaltet sie
über eine Weboberfläche – gedacht für Heim- und Kleinnetze mit mehreren Access Points.
Ein einzelnes Go-Programm, läuft als systemd-Dienst (z. B. Proxmox-LXC) oder im
Docker-Container (z. B. Synology).

- **Übersicht:** Modell, RouterOS, Firmware, WLAN-Paket, Laufzeit, API-Status,
  WLAN-Clients je Gerät und über alle Geräte (SSID, Band, Signal, Rate)
- **Updates:** RouterOS und RouterBOARD-Firmware, Neustart – nacheinander, mit Backup vorab
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

**Proxmox-LXC / Debian / Ubuntu** (als root, Details: [docs/proxmox.md](docs/proxmox.md)):

```sh
curl -fsSL https://github.com/peterhagelhof7-cmd/mikrotik-manager/releases/latest/download/install.sh -o install.sh
sh install.sh
```

**Docker** (Host-Netz ist Pflicht, Details inkl. Synology: [docs/docker.md](docs/docker.md)):

```sh
curl -LO https://raw.githubusercontent.com/peterhagelhof7-cmd/mikrotik-manager/main/docker-compose.yml
docker compose up -d
```

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
