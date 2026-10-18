#!/bin/sh
# Installiert oder aktualisiert mikrotik-manager als systemd-Dienst
# (Debian/Ubuntu, z. B. Proxmox-LXC). Als root ausführen:
#
#   neuestes Release von GitHub:   sh install.sh
#   bestimmte Version:             VERSION=v0.12.0 sh install.sh
#   eigenes Programm (Build):      sh install.sh ./mikrotik-manager-linux-amd64
#
# Das Programm liegt in /opt/mikrotik-manager (für den Dienst beschreibbar,
# damit das Selbst-Update in der Weboberfläche funktioniert), Daten in
# /var/lib/mikrotik-manager, Einstellungen in /etc/mikrotik-manager.env.
set -eu

REPO=${REPO:-peterhagelhof7-cmd/mikrotik-manager}
VERSION=${VERSION:-latest}
DIR=/opt/mikrotik-manager
BIN=$DIR/mikrotik-manager
HERE=$(cd "$(dirname "$0")" && pwd)
RAW="https://raw.githubusercontent.com/$REPO/main/deploy"

[ "$(id -u)" = 0 ] || { echo "Bitte als root ausführen." >&2; exit 1; }
command -v systemctl >/dev/null || { echo "systemd fehlt – für Docker siehe docs/docker.md" >&2; exit 1; }

fetch() { # URL Ziel
	if command -v curl >/dev/null; then curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null; then wget -qO "$2" "$1"
	else echo "curl oder wget wird benötigt" >&2; exit 1; fi
}

case "$(uname -m)" in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	armv7l | armv7) ARCH=armv7 ;;
	*) echo "Architektur $(uname -m) wird nicht unterstützt" >&2; exit 1 ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

if [ $# -ge 1 ]; then
	cp "$1" "$TMP/mikrotik-manager"
	echo "Installiere $1"
else
	if [ "$VERSION" = latest ]; then
		URL="https://github.com/$REPO/releases/latest/download"
	else
		URL="https://github.com/$REPO/releases/download/$VERSION"
	fi
	ASSET="mikrotik-manager-linux-$ARCH"
	echo "Lade $ASSET ($VERSION) …"
	fetch "$URL/$ASSET" "$TMP/mikrotik-manager"
	fetch "$URL/SHA256SUMS" "$TMP/SHA256SUMS"
	WANT=$(grep " \*\{0,1\}$ASSET\$" "$TMP/SHA256SUMS" | cut -d' ' -f1)
	GOT=$(sha256sum "$TMP/mikrotik-manager" | cut -d' ' -f1)
	[ -n "$WANT" ] && [ "$WANT" = "$GOT" ] || { echo "Prüfsumme stimmt nicht – Abbruch" >&2; exit 1; }
	echo "Prüfsumme ok"
fi
chmod 0755 "$TMP/mikrotik-manager"
NEW=$("$TMP/mikrotik-manager" version) || { echo "Das Programm startet auf diesem System nicht (Architektur?)" >&2; exit 1; }

# Dienst-Benutzer und Verzeichnisse
id mikrotik-manager >/dev/null 2>&1 ||
	useradd -r -s /usr/sbin/nologin -d /var/lib/mikrotik-manager mikrotik-manager
install -d -o mikrotik-manager -g mikrotik-manager -m 0755 "$DIR"
install -o mikrotik-manager -g mikrotik-manager -m 0755 "$TMP/mikrotik-manager" "$BIN.new"
mv "$BIN.new" "$BIN"
# ältere Installationen lagen in /usr/local/bin (dort ohne Selbst-Update)
rm -f /usr/local/bin/mikrotik-manager

# Service-Unit und Einstellungen (Einstellungen werden nie überschrieben)
for f in mikrotik-manager.service mikrotik-manager.env; do
	if [ -f "$HERE/$f" ]; then cp "$HERE/$f" "$TMP/$f"; else fetch "$RAW/$f" "$TMP/$f"; fi
done
install -m 0644 "$TMP/mikrotik-manager.service" /etc/systemd/system/mikrotik-manager.service
[ -e /etc/mikrotik-manager.env ] || install -m 0600 "$TMP/mikrotik-manager.env" /etc/mikrotik-manager.env

systemctl daemon-reload
systemctl enable mikrotik-manager >/dev/null 2>&1
systemctl restart mikrotik-manager
sleep 3
systemctl --no-pager --lines=5 status mikrotik-manager || true

echo
echo "mikrotik-manager $NEW installiert."
IP=$(hostname -I 2>/dev/null | cut -d' ' -f1)
echo "Weboberfläche: http://${IP:-<IP-dieses-Rechners>}:8080"
if [ -f /var/lib/mikrotik-manager/web-password.txt ]; then
	echo "Anmeldung:     $(cat /var/lib/mikrotik-manager/web-password.txt)"
fi
