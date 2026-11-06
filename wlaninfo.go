package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	qrcode "github.com/skip2/go-qrcode"
)

// Infopage eines WLANs: eine A4-Seite mit WLAN-Name, Passwort und QR-Code zum Aushängen.
// Sie nutzt den gespeicherten Stand der zentralen Verwaltung. Für ein PPSK-WLAN gibt es
// keine, dort hat jeder Benutzer ein eigenes Passwort.

// wifiQR: Inhalt des Standard-WLAN-QR-Codes (Android/iOS-Kamera verbinden direkt).
func wifiQR(ssid, pw string, hidden bool) string {
	q := strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`, `:`, `\:`, `"`, `\"`)
	s := "WIFI:T:WPA;S:" + q.Replace(ssid) + ";P:" + q.Replace(pw) + ";"
	if hidden {
		s += "H:true;"
	}
	return s + ";"
}

// pdfFileName macht aus einem Namen einen Dateinamen ohne Sonderzeichen.
func pdfFileName(s string) string {
	s = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss").Replace(s)
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			b.WriteRune(c)
		default:
			if !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func findNet(nets []CentralNet, ssid string) (CentralNet, bool) {
	for _, n := range nets {
		if n.SSID == ssid {
			return n, true
		}
	}
	return CentralNet{}, false
}

// wlanInfoFont: Das Passwort darf bis 63 Zeichen lang sein und muss in den Kasten passen.
func wlanInfoFont(pw string) float64 {
	const width, perPoint = 160.0, 0.2117 // mm; Courier: 0,6 em je Zeichen, 1 pt = 0,3528 mm
	if s := width / (float64(len(pw)) * perPoint); s < 18 {
		return s
	}
	return 18
}

type wlanInfo struct {
	Org, Site string
	Net       CentralNet
	At        time.Time
}

func (d wlanInfo) write(w io.Writer) error {
	png, err := qrcode.Encode(wifiQR(d.Net.SSID, d.Net.Passphrase, d.Net.Hidden), qrcode.Medium, 768)
	if err != nil {
		return err
	}
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetTitle("WLAN "+d.Net.SSID, true)
	pdf.SetCreator("MikroTik-Manager", true)
	pdf.SetMargins(20, 20, 20)
	pdf.SetAutoPageBreak(true, 20)
	tr := pdf.UnicodeTranslatorFromDescriptor("") // cp1252: Umlaute, „ “, –
	pdf.SetFooterFunc(func() {
		pdf.SetY(-14)
		pdf.SetFont("Helvetica", "", 8)
		pdf.SetTextColor(120, 120, 120)
		pdf.CellFormat(0, 5, tr("erstellt "+d.At.Format("02.01.2006 15:04")), "", 0, "C", false, 0, "")
	})
	pdf.AddPage()
	pdf.SetFont("Helvetica", "B", 26)
	pdf.CellFormat(0, 14, tr("WLAN-Zugang"), "", 1, "L", false, 0, "")
	head := d.Org
	if d.Site != "" {
		if head != "" {
			head += " · "
		}
		head += d.Site
	}
	if head != "" {
		pdf.SetFont("Helvetica", "", 13)
		pdf.SetTextColor(90, 90, 90)
		pdf.CellFormat(0, 8, tr(head), "", 1, "L", false, 0, "")
		pdf.SetTextColor(0, 0, 0)
	}
	pdf.Ln(6)

	y := pdf.GetY()
	pdf.SetDrawColor(30, 60, 110)
	pdf.SetLineWidth(0.6)
	pdf.Rect(20, y, 170, 38, "D")
	line := func(label, font string, size float64, h float64, val string) {
		pdf.SetX(25)
		pdf.SetFont("Helvetica", "", 10)
		pdf.SetTextColor(90, 90, 90)
		pdf.CellFormat(0, 5, tr(label), "", 1, "L", false, 0, "")
		pdf.SetX(25)
		pdf.SetFont(font, "B", size)
		pdf.SetTextColor(0, 0, 0)
		pdf.CellFormat(0, h, val, "", 1, "L", false, 0, "")
	}
	pdf.SetY(y + 4)
	line("WLAN-Name (SSID)", "Helvetica", 18, 9, tr(d.Net.SSID))
	line("Passwort", "Courier", wlanInfoFont(d.Net.Passphrase), 10, d.Net.Passphrase) // nur ASCII (validPSK)
	pdf.SetY(y + 46)

	pdf.RegisterImageOptionsReader("qr", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(png))
	qy := pdf.GetY()
	pdf.ImageOptions("qr", 55, qy, 100, 100, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
	pdf.SetY(qy + 106)
	pdf.SetFont("Helvetica", "B", 13)
	pdf.CellFormat(0, 8, tr("Schnell verbinden"), "", 1, "C", false, 0, "")
	pdf.SetFont("Helvetica", "", 12)
	text := "Den QR-Code mit der Kamera des Handys scannen und „Verbinden“ antippen. Alternativ das WLAN in der Liste wählen und das Passwort eingeben."
	if d.Net.Hidden {
		text = "Den QR-Code mit der Kamera des Handys scannen und „Verbinden“ antippen. Das WLAN ist versteckt und erscheint nicht in der Liste, von Hand den Namen und das Passwort eintragen."
	}
	pdf.MultiCell(0, 6, tr(text), "", "C", false)
	if err := pdf.Error(); err != nil {
		return err
	}
	return pdf.Output(w)
}

// serveWlanInfo liefert die Infopage des WLANs aus ?ssid= der übergebenen Liste.
func serveWlanInfo(rw http.ResponseWriter, r *http.Request, nets []CentralNet, org, site string) {
	ssid := r.URL.Query().Get("ssid")
	n, ok := findNet(nets, ssid)
	if !ok {
		http.Error(rw, fmt.Sprintf("WLAN %q ist nicht gespeichert", ssid), http.StatusConflict)
		return
	}
	var b bytes.Buffer
	if err := (wlanInfo{Org: org, Site: site, Net: n, At: time.Now()}).write(&b); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/pdf")
	rw.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="wlan-info-%s.pdf"`, pdfFileName(n.SSID)))
	rw.Header().Set("Cache-Control", "no-store")
	rw.Write(b.Bytes())
}
