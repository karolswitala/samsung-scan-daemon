// Package httpclient handles the HTTP layer of the Samsung M2070W Scan-to-PC
// protocol (TCP port 80).
//
// Three operations are used:
//
//   - Register / Deregister — S2PC_Regi ADD/DELETE creates or removes the named
//     "My Mac" entry in the printer's scan destination list. Register returns the
//     InstanceID assigned by the printer, which is used as the trailing SNMP OID
//     arc for state polling.
//
//   - PostAppList — S2PC_AppList announces the scan profile for this machine so
//     the printer marks it "Available" on the LCD. Must be sent while the SNMP
//     state is "triggered" (user has the scan menu open). The printer never sends
//     an HTTP response; the connection is abandoned after 8 seconds.
//
//   - GetUserSelect — reads /IDS/UserSelect.xml after the SNMP state reaches
//     "ready". Returns the format, resolution, color, and scan size the user
//     confirmed on the printer.
//
// All requests carry User-Agent: EPM Scan2PC and Connection: Keep-Alive; the
// printer gates some responses on the User-Agent header.
//
// IMPORTANT: The multipart boundary in the Content-Type header must be unquoted.
// Go's mime/multipart produces boundary="EPM Scan2PC Post Request" (RFC-correct).
// The Samsung printer rejects this with an XML parse error. All POST requests
// use a manually constructed Content-Type header:
//
//	Content-Type: multipart/form-data; boundary=EPM Scan2PC Post Request
package httpclient

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

const (
	boundary  = "EPM Scan2PC Post Request"
	userAgent = "EPM Scan2PC"
	scanPath  = "/IDS/ScanFaxToPC.cgi"
)

// Profile describes the scan settings announced to the printer via AppList.
type Profile struct {
	Resolution string // e.g. "DPI_300"
	Color      string // e.g. "COLOR_TRUE"
	Format     string // e.g. "FORMAT_M_PDF"
	Size       string // e.g. "SIZE_A4"
}

// Selection holds the scan parameters the user confirmed on the printer LCD.
type Selection struct {
	AppIndex   int
	Resolution string
	Color      string
	Format     string
	Size       string
}

// client is a shared http.Client with the EPM User-Agent injected on every request.
var client = &http.Client{
	Transport: &epmTransport{http.DefaultTransport},
}

type epmTransport struct{ rt http.RoundTripper }

func (t *epmTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("User-Agent", userAgent)
	r.Header.Set("Connection", "Keep-Alive")
	return t.rt.RoundTrip(r)
}

// buildMultipart wraps xml in the EPM multipart envelope.
// The Content-Type boundary must be UNQUOTED ("boundary=X", not "boundary=\"X\"")
// because the Samsung printer's parser rejects the RFC-compliant quoted form.
func buildMultipart(xmlBody string) (io.Reader, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.SetBoundary(boundary)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="EPMScan2PC_Post";filename=""`}
	h["Content-Type"] = []string{"application/octet-stream"}
	part, _ := w.CreatePart(h)
	io.WriteString(part, xmlBody)
	w.Close()
	// Do NOT use w.FormDataContentType() — it quotes the boundary, which breaks the printer.
	ct := "multipart/form-data; boundary=" + boundary
	return &buf, ct
}

func post(ip, xmlBody string, timeout time.Duration) ([]byte, error) {
	body, ct := buildMultipart(xmlBody)
	req, err := http.NewRequest("POST", "http://"+ip+scanPath, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ct)

	c := *client
	c.Timeout = timeout
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func get(ip, path string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest("GET", "http://"+ip+path, nil)
	if err != nil {
		return nil, err
	}
	c := *client
	c.Timeout = timeout
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Register creates a Scan-to-PC slot and returns the InstanceID assigned by the printer.
func Register(ip, userID, uniqueID string) (int, error) {
	xmlBody := fmt.Sprintf(
		`<?xml version="1.0" encoding="utf-8"?><root><S2PC_Regi RegiType="ADD" UserID="%s" UniqueID="%s" /></root>`,
		userID, uniqueID,
	)
	data, err := post(ip, xmlBody, 10*time.Second)
	if err != nil {
		return 0, fmt.Errorf("register POST: %w", err)
	}

	var root struct {
		Regi struct {
			Result     string `xml:"Result,attr"`
			InstanceID int    `xml:"InstanceID,attr"`
		} `xml:"S2PC_Regi"`
	}
	if err := xml.Unmarshal(data, &root); err != nil {
		return 0, fmt.Errorf("register parse: %w (body: %q)", err, data)
	}
	if root.Regi.Result != "ADD_OK" {
		return 0, fmt.Errorf("registration failed: result=%q", root.Regi.Result)
	}
	return root.Regi.InstanceID, nil
}

// Deregister removes this machine from the printer's Scan-to-PC list.
func Deregister(ip, userID, uniqueID string) error {
	xmlBody := fmt.Sprintf(
		`<?xml version="1.0" encoding="utf-8"?><root><S2PC_Regi RegiType="DELETE" UserID="%s" UniqueID="%s" /></root>`,
		userID, uniqueID,
	)
	_, err := post(ip, xmlBody, 10*time.Second)
	return err
}

// PostAppList announces a scan profile so this machine appears as "Available" on the LCD.
// The printer never sends an HTTP response to this request — all errors are swallowed.
func PostAppList(ip string, appIndex int, p Profile) {
	xmlBody := fmt.Sprintf(
		`<?xml version="1.0" encoding="utf-8"`+
			`?><root><S2PC_AppList><List>`+
			`<AppIndex Value="%d" />`+
			`<AppName Value="My Mac" />`+
			`<AppType Value="MAC" />`+
			`<ScanSize Value="%s" />`+
			`<FileFormat Value="%s" />`+
			`<Color Value="%s" />`+
			`<Resolution Value="%s" />`+
			`</List></S2PC_AppList></root>`,
		appIndex, p.Size, p.Format, p.Color, p.Resolution,
	)
	body, ct := buildMultipart(xmlBody)
	req, err := http.NewRequest("POST", "http://"+ip+scanPath, body)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", ct)
	c := *client
	c.Timeout = 8 * time.Second
	resp, err := c.Do(req)
	if err != nil {
		return // printer does not respond — expected
	}
	resp.Body.Close()
}

// GetUserSelect returns the scan parameters the user has confirmed on the printer LCD.
func GetUserSelect(ip string) (Selection, error) {
	data, err := get(ip, "/IDS/UserSelect.xml", 5*time.Second)
	if err != nil {
		return Selection{}, err
	}

	var root struct {
		Select struct {
			AppIndex   struct{ Value int    `xml:"Value,attr"` } `xml:"AppIndex"`
			Resolution struct{ Value string `xml:"Value,attr"` } `xml:"Resolution"`
			Color      struct{ Value string `xml:"Value,attr"` } `xml:"Color"`
			FileFormat struct{ Value string `xml:"Value,attr"` } `xml:"FileFormat"`
			ScanSize   struct{ Value string `xml:"Value,attr"` } `xml:"ScanSize"`
		} `xml:"S2PC_Select"`
	}
	if err := xml.Unmarshal(data, &root); err != nil {
		return Selection{}, fmt.Errorf("UserSelect parse: %w", err)
	}
	s := root.Select
	return Selection{
		AppIndex:   s.AppIndex.Value,
		Resolution: s.Resolution.Value,
		Color:      s.Color.Value,
		Format:     s.FileFormat.Value,
		Size:       s.ScanSize.Value,
	}, nil
}

// GetCapabilities queries the printer's capability XML. Returns raw bytes.
func GetCapabilities(ip string) ([]byte, error) {
	return get(ip, "/IDS/CAP.XML", 5*time.Second)
}
