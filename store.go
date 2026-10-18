package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Device ist ein MikroTik-Gerät. Schlüssel ist die Software-ID (bleibt auch
// bei IP-Wechsel, Umbenennung oder neuem Bridge-Aufbau gleich); solange sie
// unbekannt ist, die MAC aus MNDP.
type Device struct {
	ID         string    `json:"id"`
	SoftwareID string    `json:"softwareId,omitempty"`
	MAC        string    `json:"mac,omitempty"`
	IP         string    `json:"ip,omitempty"`
	Identity   string    `json:"identity,omitempty"`
	Version    string    `json:"version,omitempty"` // RouterOS-Version
	Platform   string    `json:"platform,omitempty"`
	Board      string    `json:"board,omitempty"`
	Interface  string    `json:"interface,omitempty"` // Schnittstelle, auf der MNDP kam (Geräteseite)
	Uptime     int64     `json:"uptime,omitempty"`    // Sekunden
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"` // MNDP oder API erreichbar
	Sources    []string  `json:"sources,omitempty"`

	// per API
	Model       string    `json:"model,omitempty"`
	Serial      string    `json:"serial,omitempty"`
	Arch        string    `json:"arch,omitempty"`
	RouterBoard bool      `json:"routerboard"`
	CurrentFW   string    `json:"currentFirmware,omitempty"`
	UpgradeFW   string    `json:"upgradeFirmware,omitempty"`
	CPULoad     int       `json:"cpuLoad"`
	FreeMem     int64     `json:"freeMem,omitempty"`
	TotalMem    int64     `json:"totalMem,omitempty"`
	FreeHDD     int64     `json:"freeHdd,omitempty"`
	TotalHDD    int64     `json:"totalHdd,omitempty"`
	Packages    []string  `json:"packages,omitempty"`
	WirelessPkg string    `json:"wirelessPkg,omitempty"` // wireless | wifi | wifiwave2 | ""
	Channel     string    `json:"channel,omitempty"`     // Update-Kanal
	Latest      string    `json:"latestVersion,omitempty"`
	APIStatus   string    `json:"apiStatus,omitempty"` // ok | auth | unreachable | error
	APIError    string    `json:"apiError,omitempty"`
	CredSource  string    `json:"credSource,omitempty"` // device | default | factory
	LastPoll    time.Time `json:"lastPoll,omitempty"`
}

// Major liefert die RouterOS-Hauptversion ("6", "7") oder "".
func (d *Device) Major() string {
	v, _, _ := strings.Cut(d.Version, ".")
	return v
}

type Store struct {
	mu    sync.RWMutex
	path  string
	devs  map[string]*Device
	dirty bool
}

func newStore(dir string) *Store {
	return &Store{path: filepath.Join(dir, "devices.json"), devs: map[string]*Device{}}
}

func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var list []*Device
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("devices.json unlesbar: %v", err)
		return
	}
	for _, d := range list {
		s.devs[d.ID] = d
	}
	log.Printf("%d bekannte Geräte geladen", len(list))
}

func (s *Store) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	b, _ := json.MarshalIndent(s.listLocked(), "", "  ")
	if err := writeFileAtomic(s.path, b, 0o600); err != nil {
		log.Printf("devices.json schreiben: %v", err)
		return
	}
	s.dirty = false
}

func (s *Store) listLocked() []*Device {
	out := make([]*Device, 0, len(s.devs))
	for _, d := range s.devs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Identity), strings.ToLower(out[j].Identity)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// List liefert Kopien, damit Aufrufer ohne Lock lesen können.
func (s *Store) List() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Device
	for _, d := range s.listLocked() {
		out = append(out, *d)
	}
	return out
}

func (s *Store) Get(id string) (Device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devs[id]
	if !ok {
		return Device{}, false
	}
	return *d, true
}

// Update ändert ein Gerät unter Lock.
func (s *Store) Update(id string, fn func(d *Device)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devs[id]
	if !ok {
		return false
	}
	fn(d)
	s.dirty = true
	return true
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devs, id)
	s.dirty = true
}

// Observe trägt eine Sichtung ein (MNDP oder Scan) und liefert die Geräte-ID.
// Zuordnung: Software-ID, sonst MAC, sonst IP. Ein Eintrag, der bisher unter
// MAC/IP lief, wird auf die Software-ID umgeschlüsselt, sobald sie bekannt ist.
func (s *Store) Observe(o Device, source string) (id string, isNew bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var d *Device
	if o.SoftwareID != "" {
		d = s.devs[o.SoftwareID]
	}
	if d == nil && o.MAC != "" {
		for _, x := range s.devs {
			if strings.EqualFold(x.MAC, o.MAC) {
				d = x
				break
			}
		}
	}
	if d == nil && o.MAC == "" && o.IP != "" {
		for _, x := range s.devs {
			if x.IP == o.IP {
				d = x
				break
			}
		}
	}
	if d == nil {
		d = &Device{FirstSeen: now}
		isNew = true
	}
	merge(d, o)
	d.LastSeen = now
	if !contains(d.Sources, source) {
		d.Sources = append(d.Sources, source)
	}
	newID := d.SoftwareID
	if newID == "" {
		newID = d.MAC
	}
	if newID == "" {
		newID = "ip:" + d.IP
	}
	if d.ID != newID {
		if d.ID != "" {
			delete(s.devs, d.ID)
		}
		// Gibt es unter der Software-ID schon einen Eintrag (z. B. zweite
		// Schnittstelle desselben Geräts per MNDP), wird zusammengelegt.
		if old, ok := s.devs[newID]; ok && old != d {
			merge(old, *d)
			old.LastSeen = now
			d = old
		}
		d.ID = newID
		s.devs[newID] = d
	}
	s.dirty = true
	return d.ID, isNew
}

// merge übernimmt nichtleere Felder einer Sichtung.
func merge(d *Device, o Device) {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&d.SoftwareID, o.SoftwareID)
	set(&d.MAC, strings.ToUpper(o.MAC))
	set(&d.IP, o.IP)
	set(&d.Identity, o.Identity)
	set(&d.Version, o.Version)
	set(&d.Platform, o.Platform)
	set(&d.Board, o.Board)
	set(&d.Interface, o.Interface)
	if o.Uptime > 0 {
		d.Uptime = o.Uptime
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
