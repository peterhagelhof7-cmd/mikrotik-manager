// mikrotik-manager findet alle MikroTik-Geräte im Netz (MNDP + optionaler
// API-Scan), liest Modell, RouterOS-Version und Firmware per API aus und zeigt
// sie zentral im Browser an. Läuft als systemd-Dienst (z. B. Proxmox-LXC) oder
// als Docker-Container mit Host-Netz.
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	version   = "dev" // per -ldflags gesetzt
	startedAt = time.Now()
)

//go:embed web
var webFS embed.FS

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		// Probestart beim Selbst-Update: läuft das Programm hier, welche Version?
		fmt.Println(version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		// Für HEALTHCHECK im scratch-Image (dort gibt es kein curl/wget).
		os.Exit(healthcheck())
	}
	log.SetFlags(log.Ldate | log.Ltime)
	cfg := loadConfig()
	log.Printf("mikrotik-manager %s startet, Web auf %s, Daten in %s", version, cfg.Listen, cfg.DataDir)

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatalf("DATA_DIR %s nicht nutzbar: %v", cfg.DataDir, err)
	}
	vault, err := openVault(cfg.DataDir)
	if err != nil {
		log.Fatalf("Schlüsselablage: %v", err)
	}
	webPass := ensureWebPassword(cfg, vault)

	st := newStore(cfg.DataDir)
	st.load()
	pl := newPoller(cfg, st, vault)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pl.ctx = ctx // vor dem Start der Weboberfläche setzen: Jobs laufen im Dienst-Kontext
	md := newMNDP(st, pl)
	pl.rediscover = md.Request
	go md.run(ctx)
	go runScanner(ctx, cfg, st, pl)
	go pl.run(ctx)
	go pl.runNightlyBackup(ctx)
	go pl.runClients(ctx)
	upd := newSelfUpdater(cfg, pl.jobs, st.save)
	go upd.run(ctx)

	sub, _ := fs.Sub(webFS, "web")
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           newWeb(cfg, st, pl, md, vault, webPass, sub, upd),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Web: %v", err)
	}
	st.save()
	log.Printf("beendet")
}

func healthcheck() int {
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
