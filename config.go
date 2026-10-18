package main

import (
	"log"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config kommt komplett aus Umgebungsvariablen, damit systemd
// (EnvironmentFile) und Docker (environment:) gleich konfiguriert werden.
type Config struct {
	Listen       string
	DataDir      string
	WebUser      string
	WebPassword  string // leer = beim ersten Start erzeugen
	ScanCIDRs    []netip.Prefix
	PollInterval time.Duration
	APITLS       bool // API-SSL (8729) statt 8728
	APIPort      int
	BackupTime   string // "hh:mm" täglich, "off" = aus
	UpdateRepo   string // GitHub "owner/repo" für das Selbst-Update, "off" = keine Prüfung
	AutoUpdate   bool   // neue Releases selbst installieren (nur systemd-Installation)
}

func loadConfig() Config {
	c := Config{
		Listen:       env("LISTEN", ":8080"),
		DataDir:      env("DATA_DIR", "/var/lib/mikrotik-manager"),
		WebUser:      env("WEB_USER", "admin"),
		WebPassword:  os.Getenv("WEB_PASSWORD"),
		PollInterval: envDuration("POLL_INTERVAL", 5*time.Minute),
		APITLS:       envBool("API_TLS", false),
		BackupTime:   env("BACKUP_TIME", "04:00"),
		UpdateRepo:   env("UPDATE_REPO", defaultUpdateRepo),
		AutoUpdate:   envBool("AUTO_UPDATE", false),
	}
	c.APIPort = 8728
	if c.APITLS {
		c.APIPort = 8729
	}
	if p, err := strconv.Atoi(os.Getenv("API_PORT")); err == nil && p > 0 {
		c.APIPort = p
	}
	for _, s := range strings.FieldsFunc(os.Getenv("SCAN_CIDRS"), func(r rune) bool { return r == ',' || r == ' ' }) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			log.Printf("SCAN_CIDRS: %q ignoriert: %v", s, err)
			continue
		}
		if p.Bits() < 20 {
			log.Printf("SCAN_CIDRS: %s ist größer als /20, ignoriert", p)
			continue
		}
		c.ScanCIDRs = append(c.ScanCIDRs, p.Masked())
	}
	return c
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v, err := strconv.ParseBool(os.Getenv(k))
	if err != nil {
		return def
	}
	return v
}

func envDuration(k string, def time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(k))
	if err != nil || v < 30*time.Second {
		return def
	}
	return v
}
