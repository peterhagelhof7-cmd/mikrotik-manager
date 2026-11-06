package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWifiQRInfo(t *testing.T) {
	if got := wifiQR(`Haus;1`, `a:b\c,d"e`, false); got != `WIFI:T:WPA;S:Haus\;1;P:a\:b\\c\,d\"e;;` {
		t.Fatal(got)
	}
	if got := wifiQR("Gast", "12345678", true); got != "WIFI:T:WPA;S:Gast;P:12345678;H:true;;" {
		t.Fatal(got)
	}
}

// Das längste Passwort muss in den Kasten passen, kurze bleiben groß.
func TestWlanInfoFont(t *testing.T) {
	if s := wlanInfoFont("12345678"); s != 18 {
		t.Fatal(s)
	}
	long := strings.Repeat("x", 63)
	if s := wlanInfoFont(long); s >= 18 || float64(len(long))*s*0.2117 > 160.01 {
		t.Fatal(s)
	}
}

func TestWlanInfoPDF(t *testing.T) {
	for _, n := range []CentralNet{
		{SSID: "Gäste", Passphrase: strings.Repeat("x", 63), Hidden: true},
		{SSID: "Büro", Passphrase: "12345678"},
	} {
		var b bytes.Buffer
		if err := (wlanInfo{Org: "Müller", Site: "Haus A", Net: n, At: time.Now()}).write(&b); err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b.Bytes(), []byte("%PDF")) {
			t.Fatal("kein PDF")
		}
		if bytes.Contains(b.Bytes(), []byte("/Pages 3")) { // alles passt auf eine Seite
			t.Fatal("mehr als eine Seite")
		}
	}
}

func TestServeWlanInfo(t *testing.T) {
	nets := []CentralNet{{SSID: "Gast", Passphrase: "12345678"}}
	rw := httptest.NewRecorder()
	serveWlanInfo(rw, httptest.NewRequest("GET", "/x?ssid=Gast", nil), nets, "", "")
	if rw.Code != http.StatusOK || !strings.Contains(rw.Header().Get("Content-Disposition"), "wlan-info-Gast.pdf") {
		t.Fatal(rw.Code, rw.Header())
	}
	rw = httptest.NewRecorder()
	serveWlanInfo(rw, httptest.NewRequest("GET", "/x?ssid=Fremd", nil), nets, "", "")
	if rw.Code != http.StatusConflict {
		t.Fatal(rw.Code)
	}
}
