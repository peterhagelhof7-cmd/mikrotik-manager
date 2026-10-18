# mikrotik-manager auf Proxmox (LXC)

Empfohlen ist ein kleiner, unprivilegierter Debian-Container. Der Manager läuft dort
als systemd-Dienst und kann sich über die Weboberfläche **selbst aktualisieren**.
Dieselbe Anleitung gilt ab Schritt 2 für jede Debian-/Ubuntu-VM und jeden
Linux-Rechner mit systemd.

## 1. Container anlegen

Der Container muss im **selben Netz (L2-Segment) wie die MikroTik-Geräte** hängen:
Die Geräte werden per MNDP-Broadcast gefunden. Geräte in anderen Netzen lassen sich
zusätzlich per `SCAN_CIDRS` suchen (siehe [Admin-Guide](admin-guide.md#einstellungen)).

**Diese Werte vorher heraussuchen und unten einsetzen:**

| Platzhalter | Bedeutung | Beispiel |
|---|---|---|
| `<CTID>` | freie Container-Nummer | `120` |
| `<STORAGE>` | Speicher für das Root-Dateisystem | `local-lvm` |
| `<BRIDGE>` | Proxmox-Bridge im Netz der MikroTiks | `vmbr0` |
| `<IP>/<MASKE>` | feste Adresse des Containers | `192.168.1.20/24` |
| `<GATEWAY>` | Gateway des Netzes | `192.168.1.1` |
| `<TEMPLATE>` | Debian-Vorlage (siehe erster Befehl) | `debian-13-standard_13.1-2_amd64.tar.zst` |

Auf dem Proxmox-Host (Shell):

```sh
# Debian-Vorlage holen (Name der neuesten Version ablesen und als <TEMPLATE> verwenden)
pveam update
pveam available --section system | grep debian
pveam download local <TEMPLATE>

pct create <CTID> local:vztmpl/<TEMPLATE> \
  --hostname mikrotik-manager --unprivileged 1 --features nesting=1 \
  --cores 1 --memory 512 --swap 256 --rootfs <STORAGE>:4 \
  --net0 name=eth0,bridge=<BRIDGE>,ip=<IP>/<MASKE>,gw=<GATEWAY> \
  --onboot 1 --start 1
```

Hängen die MikroTiks in einem VLAN, das auf der Bridge getaggt ankommt, an `--net0`
zusätzlich `,tag=<VLAN-ID>` anhängen.

Über die Proxmox-Oberfläche geht es genauso: *CT erstellen* → unprivilegiert,
Debian-Vorlage, 1 Kern, 512 MB RAM, 4 GB Disk, Netzwerk wie oben, „Beim Booten starten“.

## 2. Manager installieren

Im Container (`pct enter <CTID>` oder Konsole in der Proxmox-Oberfläche):

```sh
apt update && apt install -y curl
curl -fsSL https://github.com/peterhagelhof7-cmd/mikrotik-manager/releases/latest/download/install.sh -o install.sh
sh install.sh
```

Das Script

- lädt das neueste Release für die passende Architektur und prüft die SHA256-Prüfsumme,
- legt den Dienst-Benutzer `mikrotik-manager` an, installiert das Programm nach
  `/opt/mikrotik-manager/`, die Dienst-Unit und – falls noch nicht vorhanden –
  `/etc/mikrotik-manager.env`,
- startet den Dienst und zeigt Adresse und Anmeldedaten an.

Die Oberfläche läuft auf `http://<IP>:8080`, Benutzer `admin`. Das beim ersten Start
erzeugte Passwort steht in `/var/lib/mikrotik-manager/web-password.txt`
(oder eigenes Passwort per `WEB_PASSWORD` in `/etc/mikrotik-manager.env` setzen und
`systemctl restart mikrotik-manager`).

Eine bestimmte Version: `VERSION=v0.12.0 sh install.sh`.
Ein selbst gebautes Programm: `sh install.sh ./mikrotik-manager-linux-amd64`.

## 3. Aktualisieren

- **In der Weboberfläche:** Bei einem neuen Release erscheint oben ein Hinweis mit
  **Jetzt aktualisieren**. Der Manager lädt die neue Version, prüft Prüfsumme und
  Probestart, ersetzt sich und startet neu (wenige Sekunden). Das alte Programm bleibt
  als `/opt/mikrotik-manager/mikrotik-manager.old` liegen.
- **Automatisch:** `AUTO_UPDATE=true` in `/etc/mikrotik-manager.env`, dann
  `systemctl restart mikrotik-manager`. Geprüft wird alle 6 Stunden; läuft gerade eine
  Aktion auf einem Gerät, wird bis zur nächsten Prüfung gewartet.
- **Von Hand:** `sh install.sh` erneut ausführen.

Ältere Installationen mit dem Programm unter `/usr/local/bin` können sich nicht selbst
aktualisieren (Verzeichnis für den Dienst schreibgeschützt). Einmal `sh install.sh`
ausführen – es zieht das Programm nach `/opt/mikrotik-manager` um, Daten und
Einstellungen bleiben.

**Zurück zur vorherigen Version:**

```sh
cd /opt/mikrotik-manager && mv mikrotik-manager.old mikrotik-manager && systemctl restart mikrotik-manager
```

## 4. Betrieb

| | |
|---|---|
| Status / Protokoll | `systemctl status mikrotik-manager`, `journalctl -u mikrotik-manager -f` |
| Einstellungen | `/etc/mikrotik-manager.env` (danach `systemctl restart mikrotik-manager`) |
| Daten | `/var/lib/mikrotik-manager` (Geräte, verschlüsselte Zugangsdaten, Backups) |
| Sicherung | Container in den Proxmox-Backup-Job aufnehmen – damit sind auch die Geräte-Backups gesichert |

Der Dienst läuft gehärtet (eigener Benutzer, `ProtectSystem=strict`, nur
Datenverzeichnis und Programmverzeichnis beschreibbar, 256 MB Speichergrenze).
