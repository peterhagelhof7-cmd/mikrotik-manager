package main

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-routeros/routeros/v3"
)

// WLAN-Clients: die Registrierungstabelle aller Geräte, jede Minute gelesen
// (nur lesend) und im Speicher gehalten.

const clientsInterval = time.Minute

// WlanClient ist eine WLAN-Verbindung an einem Gerät.
type WlanClient struct {
	MAC       string `json:"mac"`
	IP        string `json:"ip,omitempty"`
	Interface string `json:"interface"`
	SSID      string `json:"ssid"`
	Band      string `json:"band"`             // "2,4 GHz" / "5 GHz"
	Signal    int    `json:"signal,omitempty"` // dBm, 0 = unbekannt
	TxRate    string `json:"txRate,omitempty"`
	RxRate    string `json:"rxRate,omitempty"`
	Uptime    int64  `json:"uptime"` // Sekunden
}

type clientSnap struct {
	Clients []WlanClient
	At      time.Time
	Err     string
}

type clientCache struct {
	mu sync.Mutex
	m  map[string]clientSnap
}

func (c *clientCache) get(id string) (clientSnap, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[id]
	return s, ok
}

func (c *clientCache) set(id string, s clientSnap) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]clientSnap{}
	}
	c.m[id] = s
}

// leadingInt liest die Zahl am Anfang ("-62@6Mbps" → -62, "-55dBm" → -55).
func leadingInt(s string) int {
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		i++
	}
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, _ := strconv.Atoi(s[:i])
	return n
}

// fmtRate vereinheitlicht die Datenrate: wifi liefert Bit/s ("19000000"),
// wireless Text ("43.3Mbps-20MHz/1S/SGI") → "19 Mbit/s" bzw. "43.3 Mbit/s".
func fmtRate(r string) string {
	if r == "" {
		return ""
	}
	if n, err := strconv.ParseFloat(r, 64); err == nil {
		return strconv.FormatFloat(n/1e6, 'f', -1, 64) + " Mbit/s"
	}
	if i := strings.Index(r, "Mbps"); i > 0 {
		return r[:i] + " Mbit/s"
	}
	return r
}

// readClients liest die WLAN-Verbindungen (altes wireless- und neues wifi-Paket).
func readClients(ctx context.Context, cl *routeros.Client) ([]WlanClient, error) {
	arp := map[string]string{}
	if l, err := printAll(ctx, cl, "/ip/arp/print", "=.proplist=address,mac-address"); err == nil {
		for _, m := range l {
			arp[strings.ToUpper(m["mac-address"])] = m["address"]
		}
	}
	var out []WlanClient
	var lastErr error
	ok := false

	// wireless: SSID/Band kommen von der Schnittstelle (virtuelle vom Funkteil)
	if ifs, err := printAll(ctx, cl, "/interface/wireless/print", "=.proplist=name,ssid,band,master-interface"); err == nil && len(ifs) > 0 {
		byName := map[string]map[string]string{}
		for _, m := range ifs {
			byName[m["name"]] = m
		}
		reg, err := printAll(ctx, cl, "/interface/wireless/registration-table/print")
		if err != nil {
			lastErr = err
		} else {
			ok = true
			for _, m := range reg {
				x := byName[m["interface"]]
				band := x["band"]
				if mi := masterOf(x); mi != "" {
					band = byName[mi]["band"]
				}
				mac := strings.ToUpper(m["mac-address"])
				ip := m["last-ip"]
				if ip == "" {
					ip = arp[mac]
				}
				out = append(out, WlanClient{MAC: mac, IP: ip, Interface: m["interface"], SSID: x["ssid"], Band: bandName(band),
					Signal: leadingInt(m["signal-strength"]), TxRate: fmtRate(m["tx-rate"]), RxRate: fmtRate(m["rx-rate"]), Uptime: parseROSDuration(m["uptime"])})
			}
		}
	}

	// wifi: SSID und Band stehen in der Registrierungstabelle
	if reg, err := printAll(ctx, cl, "/interface/wifi/registration-table/print"); err == nil {
		ok = true
		for _, m := range reg {
			mac := strings.ToUpper(m["mac-address"])
			out = append(out, WlanClient{MAC: mac, IP: arp[mac], Interface: m["interface"], SSID: m["ssid"], Band: bandName(m["band"]),
				Signal: leadingInt(m["signal"]), TxRate: fmtRate(m["tx-rate"]), RxRate: fmtRate(m["rx-rate"]), Uptime: parseROSDuration(m["uptime"])})
		}
	} else if !ok {
		lastErr = err
	}
	if !ok {
		return nil, lastErr
	}
	return out, nil
}

// refreshClients liest die Clients eines Geräts und legt sie im Speicher ab.
func (p *Poller) refreshClients(ctx context.Context, id string) {
	d, ok := p.st.Get(id)
	if !ok || d.APIStatus != "ok" || d.WirelessPkg == "" {
		return
	}
	var list []WlanClient
	err := p.withClient(ctx, d, func(ctx context.Context, cl *routeros.Client) (err error) {
		list, err = readClients(ctx, cl)
		return err
	})
	s := clientSnap{Clients: list, At: time.Now()}
	if err != nil {
		// letzte bekannte Liste behalten, Fehler anzeigen
		old, _ := p.wc.get(id)
		s = clientSnap{Clients: old.Clients, At: old.At, Err: err.Error()}
	}
	p.wc.set(id, s)
}

// RefreshAllClients liest alle Geräte parallel (z. B. Knopf „jetzt aktualisieren“).
func (p *Poller) RefreshAllClients(ctx context.Context) {
	var wg sync.WaitGroup
	for _, d := range p.st.List() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			p.refreshClients(rctx, d.ID)
		}()
	}
	wg.Wait()
}

// runClients aktualisiert die Clients jede Minute.
func (p *Poller) runClients(ctx context.Context) {
	time.Sleep(10 * time.Second) // erst die Geräte einmal auslesen lassen
	t := time.NewTicker(clientsInterval)
	defer t.Stop()
	for {
		p.RefreshAllClients(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
