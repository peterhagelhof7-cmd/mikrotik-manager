package main

import (
	"context"
	"strings"

	"github.com/go-routeros/routeros/v3"
)

// WLAN-Zustand für die Detailseite: was RouterOS selbst über jede
// WLAN-Schnittstelle sagt (monitor once) und die letzten WLAN-Meldungen aus
// dem Geräteprotokoll. Nur lesend – beantwortet „warum sendet es nicht?“.

type WlanIfStatus struct {
	Name    string `json:"name"`
	State   string `json:"state"`             // wie von RouterOS, z. B. running-ap, disabled, no available channels
	Channel string `json:"channel,omitempty"` // tatsächlicher Kanal, z. B. 5180/ax/Ceee
	TxPower string `json:"txPower,omitempty"`
	Peers   string `json:"peers,omitempty"` // angemeldete Clients laut monitor
}

type WlanLog struct {
	Time    string `json:"time"`
	Topics  string `json:"topics"`
	Message string `json:"message"`
}

type WlanStatus struct {
	Pkg        string         `json:"pkg"`
	Interfaces []WlanIfStatus `json:"interfaces"`
	Log        []WlanLog      `json:"log"`
	MonitorErr string         `json:"monitorError,omitempty"`
	LogErr     string         `json:"logError,omitempty"`
}

func (p *Poller) WlanStatus(ctx context.Context, id string) (WlanStatus, error) {
	cl, err := p.session(ctx, id)
	if err != nil {
		return WlanStatus{}, err
	}
	defer cl.Close()
	return readWlanStatus(ctx, cl)
}

func readWlanStatus(ctx context.Context, cl *routeros.Client) (WlanStatus, error) {
	st := WlanStatus{Interfaces: []WlanIfStatus{}, Log: []WlanLog{}}
	var names []string
	for _, pkg := range []string{"wifi", "wireless"} {
		l, err := printAll(ctx, cl, "/interface/"+pkg+"/print", "=.proplist=name")
		if err != nil || len(l) == 0 {
			continue
		}
		st.Pkg = pkg
		for _, m := range l {
			names = append(names, m["name"])
		}
		break
	}
	if st.Pkg == "" {
		return st, nil
	}
	mon, err := printAll(ctx, cl, "/interface/"+st.Pkg+"/monitor", "=numbers="+strings.Join(names, ","), "=once=")
	if err != nil {
		// Manche Versionen nehmen nur eine Schnittstelle je Aufruf.
		mon = nil
		for _, n := range names {
			if r, e := printAll(ctx, cl, "/interface/"+st.Pkg+"/monitor", "=numbers="+n, "=once="); e == nil {
				for _, m := range r {
					if m["name"] == "" {
						m["name"] = n
					}
					mon = append(mon, m)
				}
			} else {
				st.MonitorErr = e.Error()
			}
		}
	}
	var lg []map[string]string
	if l, err := printAll(ctx, cl, "/log/print", "=.proplist=time,topics,message"); err == nil {
		lg = l
	} else {
		st.LogErr = err.Error()
	}
	st.Interfaces, st.Log = buildWlanStatus(st.Pkg, names, mon, lg)
	return st, nil
}

// buildWlanStatus setzt monitor- und Protokolldaten zusammen (getrennt, damit testbar).
func buildWlanStatus(pkg string, names []string, mon, lg []map[string]string) ([]WlanIfStatus, []WlanLog) {
	byName := map[string]map[string]string{}
	for i, m := range mon {
		n := m["name"]
		if n == "" && i < len(names) {
			n = names[i] // ohne name-Feld kommen die Antworten in der Reihenfolge von numbers
		}
		byName[n] = m
	}
	ifs := []WlanIfStatus{}
	for _, n := range names {
		m := byName[n]
		x := WlanIfStatus{Name: n}
		if pkg == "wifi" {
			x.State, x.Channel, x.TxPower = m["state"], m["channel"], m["tx-power"]
			x.Peers = m["registered-peers"]
		} else {
			x.State, x.Channel, x.TxPower = m["status"], m["channel"], m["tx-power"]
			x.Peers = m["registered-clients"]
		}
		ifs = append(ifs, x)
	}
	logs := []WlanLog{}
	for i := len(lg) - 1; i >= 0 && len(logs) < 40; i-- {
		t := lg[i]["topics"]
		if !strings.Contains(t, "wireless") && !strings.Contains(t, "wifi") && !strings.Contains(t, "caps") {
			continue
		}
		logs = append(logs, WlanLog{Time: lg[i]["time"], Topics: t, Message: lg[i]["message"]})
	}
	return ifs, logs
}
