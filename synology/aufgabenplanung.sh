# mikrotik-manager für die Synology-Aufgabenplanung (Benutzer: root).
# Legt den Container an bzw. aktualisiert ihn auf das neueste Release.
# Gefundene Geräte, Zugangsdaten und Backups bleiben im Volume mikrotik-data erhalten.

# ===== VOR DEM AUSFÜHREN AUSFÜLLEN ==========================================
# Passwort für die Weboberfläche (Benutzer: admin)
WEB_PASSWORD='HIER_EINTRAGEN'
# Port der Weboberfläche
PORT=8080
# Passwort in einfachen Anführungszeichen lassen, es darf selbst kein ' enthalten.
# ===========================================================================

IMAGE=ghcr.io/peterhagelhof7-cmd/mikrotik-manager:latest
DOCKER=/usr/local/bin/docker

case "$WEB_PASSWORD" in
*HIER_EINTRAGEN* | '')
	echo "ABBRUCH: Bitte zuerst oben WEB_PASSWORD ausfüllen."
	exit 1
	;;
esac

$DOCKER pull "$IMAGE" || exit 1
$DOCKER rm -f mikrotik-manager >/dev/null 2>&1
$DOCKER run -d --name mikrotik-manager \
	--network host --restart unless-stopped -m 128m \
	-e TZ=Europe/Berlin \
	-e LISTEN=":$PORT" \
	-e WEB_PASSWORD="$WEB_PASSWORD" \
	-v mikrotik-data:/data \
	"$IMAGE" || exit 1
echo "OK: mikrotik-manager läuft, Oberfläche unter http://<NAS-IP>:$PORT (Benutzer admin)"
