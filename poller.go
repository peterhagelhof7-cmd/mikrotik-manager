package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// Poller liest Geräte per RouterOS-API (8728 / API-SSL 8729) aus. Die
// klassische API gibt es in v6 und v7, REST erst ab v7 – deshalb API.
type Poller struct {
	cfg        Config
	st         *Store
	vault      *Vault
	queue      chan string
	mu         sync.Mutex
	busy       map[string]bool
	jobs       *Jobs
	hostKeys   *hostKeys
	nextBackup time.Time
	rediscover func()          // MNDP-Anfrage senden (nach IP-Wechsel)
	ctx        context.Context // Dienst-Kontext für Hintergrund-Jobs
	wc         clientCache     // WLAN-Clients je Gerät (nur im Speicher)
	auto       autoUpdater     // Zustand der automatischen Updates
}

func newPoller(cfg Config, st *Store, v *Vault) *Poller {
	return &Poller{cfg: cfg, st: st, vault: v, queue: make(chan string, 256), busy: map[string]bool{}, jobs: newJobs(), ctx: context.Background(),
		hostKeys: &hostKeys{path: filepath.Join(cfg.DataDir, "ssh_known_hosts.json")}}
}

// Trigger stellt ein Gerät zum baldigen Auslesen ein (nicht blockierend).
func (p *Poller) Trigger(id string) {
	select {
	case p.queue <- id:
	default:
	}
}

func (p *Poller) TriggerAll() {
	for _, d := range p.st.List() {
		p.Trigger(d.ID)
	}
}

func (p *Poller) run(ctx context.Context) {
	for i := 0; i < 4; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-p.queue:
					p.pollOne(ctx, id)
				}
			}
		}()
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.st.save()
			}
		}
	}()
	p.TriggerAll()
	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.TriggerAll()
		}
	}
}

func (p *Poller) pollOne(ctx context.Context, id string) {
	p.mu.Lock()
	if p.busy[id] {
		p.mu.Unlock()
		return
	}
	p.busy[id] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.busy, id)
		p.mu.Unlock()
	}()

	d, ok := p.st.Get(id)
	if !ok || d.IP == "" {
		return
	}
	cred, src := p.vault.For(id)
	info, err := p.readDevice(ctx, d.IP, cred)
	if err != nil {
		status, msg := classifyErr(err, p.cfg.APIPort)
		p.st.Update(id, func(x *Device) {
			x.APIStatus, x.APIError, x.CredSource, x.LastPoll = status, msg, src, time.Now()
		})
		return
	}
	info.IP = d.IP
	newID, _ := p.st.Observe(Device{SoftwareID: info.SoftwareID, IP: d.IP, Identity: info.Identity, Version: info.Version, Board: info.Board, Uptime: info.Uptime}, "api")
	if newID != id {
		// Zugangsdaten wandern mit, falls das Gerät gerade umgeschlüsselt wurde.
		if c, ok := p.vault.Get("dev:" + id); ok {
			p.vault.Set("dev:"+newID, c)
			p.vault.Delete("dev:" + id)
		}
	}
	p.st.Update(newID, func(x *Device) {
		x.Model, x.Serial, x.Arch, x.RouterBoard = info.Model, info.Serial, info.Arch, info.RouterBoard
		x.CurrentFW, x.UpgradeFW = info.CurrentFW, info.UpgradeFW
		x.CPULoad, x.FreeMem, x.TotalMem, x.FreeHDD, x.TotalHDD = info.CPULoad, info.FreeMem, info.TotalMem, info.FreeHDD, info.TotalHDD
		x.Packages, x.WirelessPkg, x.Channel, x.Latest = info.Packages, info.WirelessPkg, info.Channel, info.Latest
		x.APIStatus, x.APIError, x.CredSource, x.LastPoll = "ok", "", src, time.Now()
	})
}

func (p *Poller) dial(ctx context.Context, ip string, c Cred) (*routeros.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	addr := net.JoinHostPort(ip, strconv.Itoa(p.cfg.APIPort))
	if p.cfg.APITLS {
		// Werks-/selbstsignierte Zertifikate sind die Regel; Absicherung
		// gegen MITM wäre Zertifikats-Pinning (später).
		return routeros.DialTLSContext(ctx, addr, c.User, c.Password, &tls.Config{InsecureSkipVerify: true})
	}
	return routeros.DialContext(ctx, addr, c.User, c.Password)
}

func (p *Poller) readDevice(ctx context.Context, ip string, c Cred) (Device, error) {
	var d Device
	cl, err := p.dial(ctx, ip, c)
	if err != nil {
		return d, err
	}
	defer cl.Close()
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	one := func(cmd string) (map[string]string, error) {
		r, err := cl.RunContext(rctx, cmd)
		if err != nil {
			return nil, err
		}
		if len(r.Re) == 0 {
			return map[string]string{}, nil
		}
		return r.Re[0].Map, nil
	}

	res, err := one("/system/resource/print")
	if err != nil {
		return d, err
	}
	d.Version = res["version"]
	d.Board = res["board-name"]
	d.Arch = res["architecture-name"]
	d.CPULoad, _ = strconv.Atoi(res["cpu-load"])
	d.FreeMem, _ = strconv.ParseInt(res["free-memory"], 10, 64)
	d.TotalMem, _ = strconv.ParseInt(res["total-memory"], 10, 64)
	d.FreeHDD, _ = strconv.ParseInt(res["free-hdd-space"], 10, 64)
	d.TotalHDD, _ = strconv.ParseInt(res["total-hdd-space"], 10, 64)
	d.Uptime = parseROSDuration(res["uptime"])

	if m, err := one("/system/identity/print"); err == nil {
		d.Identity = m["name"]
	}
	if m, err := one("/system/routerboard/print"); err == nil {
		d.RouterBoard = m["routerboard"] == "true"
		d.Model = m["model"]
		d.Serial = m["serial-number"]
		d.CurrentFW = m["current-firmware"]
		d.UpgradeFW = m["upgrade-firmware"]
	}
	if m, err := one("/system/license/print"); err == nil {
		d.SoftwareID = m["software-id"]
		if d.SoftwareID == "" {
			d.SoftwareID = m["system-id"] // CHR unter v7
		}
	}
	if r, err := cl.RunContext(rctx, "/system/package/print"); err == nil {
		for _, s := range r.Re {
			name := s.Map["name"]
			if s.Map["disabled"] == "true" {
				name += " (aus)"
			} else {
				switch {
				case name == "wifiwave2":
					d.WirelessPkg = "wifiwave2"
				case strings.HasPrefix(name, "wifi-qcom"):
					d.WirelessPkg = "wifi"
				case strings.HasPrefix(name, "wireless") && d.WirelessPkg == "":
					d.WirelessPkg = "wireless"
				}
			}
			d.Packages = append(d.Packages, name+" "+s.Map["version"])
		}
	}
	if m, err := one("/system/package/update/print"); err == nil {
		d.Channel = m["channel"]
		d.Latest = m["latest-version"]
	}
	return d, nil
}

func classifyErr(err error, port int) (status, msg string) {
	var ne net.Error
	var oe *net.OpError
	s := err.Error()
	switch {
	case strings.Contains(s, "invalid user name or password"), strings.Contains(s, "cannot log in"),
		strings.Contains(s, "not logged in"):
		return "auth", "Anmeldung abgelehnt – Zugangsdaten prüfen"
	case strings.Contains(s, "connection refused"):
		return "unreachable", fmt.Sprintf("Port %d zu – API-Dienst aus? (/ip service enable api)", port)
	case errors.As(err, &ne) && ne.Timeout(), errors.As(err, &oe), errors.Is(err, context.DeadlineExceeded):
		return "unreachable", "nicht erreichbar: " + s
	}
	return "error", s
}

var rosDurRe = regexp.MustCompile(`(\d+)([wdhms])`)

// parseROSDuration wandelt "1w2d03:04:05" (v6) bzw. "1w2d3h4m5s" (v7) in Sekunden.
func parseROSDuration(s string) int64 {
	var total int64
	if i := strings.LastIndexByte(s, ':'); i > 0 {
		// v6-Form: Rest vor der Uhrzeit enthält w/d
		start := strings.LastIndexAny(s[:i], "wd") + 1
		var h, m, sec int64
		fmt.Sscanf(s[start:], "%d:%d:%d", &h, &m, &sec)
		total = h*3600 + m*60 + sec
		s = s[:start]
	}
	mult := map[string]int64{"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}
	for _, m := range rosDurRe.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		total += n * mult[m[2]]
	}
	return total
}

// runScanner sucht Geräte in SCAN_CIDRS über den offenen API-Port. Nötig für
// Netze, in denen MNDP nicht ankommt (anderes VLAN/Subnetz hinter Routern).
func runScanner(ctx context.Context, cfg Config, st *Store, pl *Poller) {
	if len(cfg.ScanCIDRs) == 0 {
		return
	}
	scan := func() {
		known := map[string]bool{}
		for _, d := range st.List() {
			known[d.IP] = true
		}
		sem := make(chan struct{}, 64)
		var wg sync.WaitGroup
		for _, pfx := range cfg.ScanCIDRs {
			for a := pfx.Addr().Next(); pfx.Contains(a) && a != broadcastAddr(pfx); a = a.Next() {
				ip := a.String()
				if known[ip] {
					continue
				}
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer func() { <-sem; wg.Done() }()
					c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.APIPort)), 800*time.Millisecond)
					if err != nil {
						return
					}
					c.Close()
					id, isNew := st.Observe(Device{IP: ip}, "scan")
					if isNew {
						log.Printf("API-Port offen auf %s, lese aus", ip)
						pl.Trigger(id)
					}
				}()
			}
		}
		wg.Wait()
	}
	scan()
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scan()
		}
	}
}
