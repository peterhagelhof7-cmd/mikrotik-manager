package main

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// MNDP (MikroTik Neighbor Discovery Protocol): Geräte senden alle ~60 s einen
// UDP-Broadcast an Port 5678 und antworten auf eine 4-Byte-Anfrage aus Nullen
// (so sucht auch Winbox). Funktioniert nur im selben L2-Segment, deshalb
// braucht der Docker-Container network_mode: host.
const mndpPort = 5678

// TLV-Typen laut Wireshark-Dissector (packet-mndp.c).
const (
	tlvMAC        = 1
	tlvIdentity   = 5
	tlvVersion    = 7
	tlvPlatform   = 8
	tlvUptime     = 10
	tlvSoftwareID = 11
	tlvBoard      = 12
	tlvInterface  = 16
	tlvIPv4       = 17
)

type MNDP struct {
	st   *Store
	pl   *Poller
	mu   sync.Mutex
	conn *net.UDPConn
}

func newMNDP(st *Store, pl *Poller) *MNDP { return &MNDP{st: st, pl: pl} }

func (m *MNDP) run(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: mndpPort})
		if err != nil {
			log.Printf("MNDP: Port %d nicht nutzbar: %v (neuer Versuch in 30 s)", mndpPort, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}
		m.mu.Lock()
		m.conn = conn
		m.mu.Unlock()
		go func() {
			<-ctx.Done()
			conn.Close()
		}()
		go m.requestLoop(ctx)
		m.readLoop(ctx, conn)
		conn.Close()
	}
}

func (m *MNDP) readLoop(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("MNDP lesen: %v", err)
			}
			return
		}
		d, ok := parseMNDP(buf[:n])
		if !ok {
			continue // eigene Anfrage oder Müll
		}
		if d.IP == "" {
			d.IP = from.Addr().Unmap().String()
		}
		id, isNew := m.st.Observe(d, "mndp")
		if isNew {
			log.Printf("neues Gerät per MNDP: %s %s %s (%s)", d.Identity, d.IP, d.MAC, d.Version)
			m.pl.Trigger(id)
		}
	}
}

// requestLoop fragt alle 2 Minuten aktiv nach – schneller als auf den
// 60-s-Takt der Geräte zu warten, und auch nach Neustart des Dienstes sofort.
func (m *MNDP) requestLoop(ctx context.Context) {
	m.Request()
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Request()
		}
	}
}

// Request sendet die Suchanfrage an den globalen Broadcast und an die
// Broadcast-Adresse jeder IPv4-Schnittstelle.
func (m *MNDP) Request() {
	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()
	if conn == nil {
		return
	}
	req := []byte{0, 0, 0, 0}
	targets := map[string]bool{"255.255.255.255": true}
	for _, p := range localPrefixes() {
		targets[broadcastAddr(p).String()] = true
	}
	for t := range targets {
		if _, err := conn.WriteToUDP(req, &net.UDPAddr{IP: net.ParseIP(t), Port: mndpPort}); err != nil {
			log.Printf("MNDP-Anfrage an %s: %v", t, err)
		}
	}
}

func parseMNDP(b []byte) (Device, bool) {
	var d Device
	if len(b) < 8 {
		return d, false
	}
	b = b[4:] // 2 Byte Header + 2 Byte Sequenznummer
	for len(b) >= 4 {
		typ := binary.BigEndian.Uint16(b[0:2])
		l := int(binary.BigEndian.Uint16(b[2:4]))
		if len(b) < 4+l {
			break
		}
		v := b[4 : 4+l]
		b = b[4+l:]
		switch typ {
		case tlvMAC:
			if l == 6 {
				d.MAC = strings.ToUpper(net.HardwareAddr(v).String())
			}
		case tlvIdentity:
			d.Identity = string(v)
		case tlvVersion:
			d.Version = string(v)
		case tlvPlatform:
			d.Platform = string(v)
		case tlvUptime:
			if l == 4 {
				d.Uptime = int64(binary.LittleEndian.Uint32(v))
			}
		case tlvSoftwareID:
			d.SoftwareID = string(v)
		case tlvBoard:
			d.Board = string(v)
		case tlvInterface:
			d.Interface = string(v)
		case tlvIPv4:
			if l == 4 {
				d.IP = netip.AddrFrom4([4]byte(v)).String()
			}
		}
	}
	return d, d.MAC != ""
}

// localPrefixes liefert die IPv4-Netze aller aktiven, nicht-Loopback-
// Schnittstellen (ohne Docker-Bridges).
func localPrefixes() []netip.Prefix {
	var out []netip.Prefix
	ifs, _ := net.Interfaces()
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.HasPrefix(ifi.Name, "docker") || strings.HasPrefix(ifi.Name, "br-") || strings.HasPrefix(ifi.Name, "veth") {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			p, err := netip.ParsePrefix(ipn.String())
			if err == nil && p.Bits() >= 16 && p.Bits() < 31 {
				out = append(out, p)
			}
		}
	}
	return out
}

func broadcastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := binary.BigEndian.Uint32(a[:]) | host
	var r [4]byte
	binary.BigEndian.PutUint32(r[:], v)
	return netip.AddrFrom4(r)
}
