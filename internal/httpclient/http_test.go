package httpclient

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const capXML = `<?xml version="1.0" encoding="UTF-8"?>
<root><DeviceCapability>
  <ProductInfo><ModelName>Samsung M2070 Series</ModelName></ProductInfo>
  <SoftwareInterfaceInfo><Scan2PC InterfaceVersion="2.0"><Scan2PCInfo>
    <HTTPURL>
      <POST Value="/IDS/ScanFaxToPC.cgi"/>
      <GET Value="/IDS/UserSelect.xml"/>
    </HTTPURL>
    <FileFormat DefaultValue="FORMAT_M_PDF">
      <Format ID="FORMAT_JPEG" STRING="JPEG"/>
      <Format ID="FORMAT_M_PDF" STRING="Multi page PDF"/>
    </FileFormat>
    <ScanColor>
      <Color ID="COLOR_TRUE" STRING="True Color"/>
      <Color ID="COLOR_GRAY" STRING="Gray"/>
    </ScanColor>
    <ScanResolution>
      <Resolution ID="DPI_100" STRING="100 dpi"/>
      <Resolution ID="DPI_200" STRING="200 dpi"/>
      <Resolution ID="DPI_300" STRING="300 dpi"/>
    </ScanResolution>
  </Scan2PCInfo></Scan2PC></SoftwareInterfaceInfo>
</DeviceCapability></root>`

const userSelectXML = `<?xml version="1.0" encoding="UTF-8"?>
<root><S2PC_Select>
  <AppIndex Value="3"/>
  <Resolution Value="DPI_300"/>
  <Color Value="COLOR_TRUE"/>
  <FileFormat Value="FORMAT_M_PDF"/>
  <ScanSize Value="SIZE_A4"/>
</S2PC_Select></root>`

const registerOKXML = `<?xml version="1.0"?><root><S2PC_Regi UserID="PC" Result="ADD_OK" InstanceID="2" /></root>`

// extractAppListXML parses the AppList XML payload from a captured request body.
func extractAppListXML(body string) map[string]string {
	// The XML is between the first blank line and the last boundary
	parts := strings.SplitN(body, "\r\n\r\n", 2)
	if len(parts) < 2 {
		return nil
	}
	xmlPart := strings.SplitN(parts[1], "\r\n--", 2)[0]

	var root struct {
		List struct {
			AppType    struct{ Value string `xml:"Value,attr"` } `xml:"AppType"`
			Resolution struct{ Value string `xml:"Value,attr"` } `xml:"Resolution"`
			Color      struct{ Value string `xml:"Value,attr"` } `xml:"Color"`
			FileFormat struct{ Value string `xml:"Value,attr"` } `xml:"FileFormat"`
			AppIndex   struct{ Value int    `xml:"Value,attr"` } `xml:"AppIndex"`
		} `xml:"S2PC_AppList>List"`
	}
	xml.Unmarshal([]byte(xmlPart), &root)
	return map[string]string{
		"AppType":    root.List.AppType.Value,
		"Resolution": root.List.Resolution.Value,
		"Color":      root.List.Color.Value,
		"FileFormat": root.List.FileFormat.Value,
	}
}

func TestGetCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(capXML))
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")

	data, err := GetCapabilities(ip)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Samsung M2070 Series") {
		t.Error("expected model name in capabilities")
	}
}

func TestGetCapabilitiesUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte(capXML))
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	GetCapabilities(ip)
	if gotUA != userAgent {
		t.Errorf("want User-Agent %q, got %q", userAgent, gotUA)
	}
}

func TestRegisterURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(registerOKXML))
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	Register(ip, "My Mac", "abc123")
	if gotPath != scanPath {
		t.Errorf("want path %q, got %q", scanPath, gotPath)
	}
}

func TestRegisterReturnsInstanceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(registerOKXML))
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	id, err := Register(ip, "My Mac", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Errorf("want instanceID=2, got %d", id)
	}
}

func TestRegisterBodyHasADDType(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(registerOKXML))
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	Register(ip, "My Mac", "deadbeef01234567")
	if !strings.Contains(gotBody, `RegiType="ADD"`) {
		t.Error("expected RegiType=ADD in body")
	}
	if !strings.Contains(gotBody, `UserID="My Mac"`) {
		t.Error("expected UserID in body")
	}
	if !strings.Contains(gotBody, `UniqueID="deadbeef01234567"`) {
		t.Error("expected UniqueID in body")
	}
}

func TestPostAppListHasMACType(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	PostAppList(ip, 2, Profile{Resolution: "DPI_300", Color: "COLOR_TRUE", Format: "FORMAT_M_PDF", Size: "SIZE_A4"})
	fields := extractAppListXML(gotBody)
	if fields["AppType"] != "MAC" {
		t.Errorf("want AppType=MAC, got %q", fields["AppType"])
	}
}

func TestPostAppListResolutionAndColor(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	PostAppList(ip, 2, Profile{Resolution: "DPI_200", Color: "COLOR_GRAY", Format: "FORMAT_JPEG", Size: "SIZE_A4"})
	fields := extractAppListXML(gotBody)
	if fields["Resolution"] != "DPI_200" {
		t.Errorf("want DPI_200, got %q", fields["Resolution"])
	}
	if fields["Color"] != "COLOR_GRAY" {
		t.Errorf("want COLOR_GRAY, got %q", fields["Color"])
	}
}

func TestPostAppListMultipartAndEPMAgent(t *testing.T) {
	var gotCT, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotUA = r.Header.Get("User-Agent")
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	PostAppList(ip, 1, Profile{Resolution: "DPI_300", Color: "COLOR_TRUE", Format: "FORMAT_M_PDF", Size: "SIZE_A4"})
	if !strings.Contains(gotCT, "multipart/form-data") {
		t.Errorf("want multipart/form-data, got %q", gotCT)
	}
	if gotUA != userAgent {
		t.Errorf("want User-Agent %q, got %q", userAgent, gotUA)
	}
}

func TestPostAppListBoundaryExact(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	PostAppList(ip, 1, Profile{Resolution: "DPI_300", Color: "COLOR_TRUE", Format: "FORMAT_M_PDF", Size: "SIZE_A4"})
	if !strings.Contains(gotBody, "--"+boundary) {
		t.Errorf("expected exact boundary %q in body", boundary)
	}
}

func TestPostAppListToleratesNoResponse(t *testing.T) {
	// Server closes connection immediately without responding
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// do nothing — hijack and close
		h, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := h.Hijack()
		conn.Close()
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	// Must not panic or block forever
	PostAppList(ip, 1, Profile{Resolution: "DPI_300", Color: "COLOR_TRUE", Format: "FORMAT_M_PDF", Size: "SIZE_A4"})
}

func TestGetUserSelect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(userSelectXML))
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	sel, err := GetUserSelect(ip)
	if err != nil {
		t.Fatal(err)
	}
	if sel.AppIndex != 3 {
		t.Errorf("want AppIndex=3, got %d", sel.AppIndex)
	}
	if sel.Resolution != "DPI_300" {
		t.Errorf("want DPI_300, got %q", sel.Resolution)
	}
	if sel.Color != "COLOR_TRUE" {
		t.Errorf("want COLOR_TRUE, got %q", sel.Color)
	}
	if sel.Format != "FORMAT_M_PDF" {
		t.Errorf("want FORMAT_M_PDF, got %q", sel.Format)
	}
	if sel.Size != "SIZE_A4" {
		t.Errorf("want SIZE_A4, got %q", sel.Size)
	}
}

func TestGetUserSelectUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte(userSelectXML))
	}))
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	GetUserSelect(ip)
	if gotUA != userAgent {
		t.Errorf("want User-Agent %q, got %q", userAgent, gotUA)
	}
}
