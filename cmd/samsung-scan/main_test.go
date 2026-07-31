package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/karol/samsung-scan/internal/httpclient"
	"github.com/karol/samsung-scan/internal/imageutil"
	"github.com/karol/samsung-scan/internal/snmp"
)

// --- Fake implementations of the external dependencies ---

type fakeHTTP struct {
	registerReturn    int
	registerReturns   []int // optional per-call sequence; falls back to registerReturn
	registerCalls     int
	registerErr       error
	registerFailFirst int // fail the first N Register calls (simulates printer offline at startup)
	deregisterCalls   int
	appListCalls      []int // appIndex values
	userSelectRet     httpclient.Selection
}

func (f *fakeHTTP) Register(ip, userID, uid string) (int, error) {
	f.registerCalls++
	if f.registerCalls <= f.registerFailFirst {
		return 0, errors.New("simulated register failure (printer offline)")
	}
	if f.registerErr != nil {
		return 0, f.registerErr
	}
	if len(f.registerReturns) > 0 {
		i := f.registerCalls - 1
		if i >= len(f.registerReturns) {
			i = len(f.registerReturns) - 1
		}
		return f.registerReturns[i], nil
	}
	return f.registerReturn, nil
}
func (f *fakeHTTP) Deregister(ip, userID, uid string) error {
	f.deregisterCalls++
	return nil
}
func (f *fakeHTTP) PostAppList(ip string, idx int, p httpclient.Profile) {
	f.appListCalls = append(f.appListCalls, idx)
}
func (f *fakeHTTP) GetUserSelect(ip string) (httpclient.Selection, error) {
	return f.userSelectRet, nil
}

type fakeSNMP struct {
	states     []snmp.State
	errs       []error // parallel to states; nil means success
	pos        int
	polledWith []int // instanceID seen on each Poll call
}

func (f *fakeSNMP) Poll(ip string, instanceID int, timeout time.Duration) (snmp.State, error) {
	f.polledWith = append(f.polledWith, instanceID)
	if f.pos >= len(f.states) {
		return snmp.Idle, nil
	}
	s := f.states[f.pos]
	var err error
	if f.pos < len(f.errs) {
		err = f.errs[f.pos]
	}
	f.pos++
	return s, err
}

type fakeTCP struct {
	pageBytes [][]byte
	callCount int
}

func (f *fakeTCP) Download(ip string, resolution int) ([][]byte, error) {
	f.callCount++
	if len(f.pageBytes) == 0 {
		return [][]byte{makeJPEGBytes(10, 10)}, nil
	}
	return f.pageBytes, nil
}

// makeJPEGBytes creates a tiny valid JPEG for testing.
func makeJPEGBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{128, 128, 128, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	return buf.Bytes()
}

// --- Testable run function with injected dependencies ---

type deps struct {
	http interface {
		Register(ip, userID, uid string) (int, error)
		Deregister(ip, userID, uid string) error
		PostAppList(ip string, idx int, p httpclient.Profile)
		GetUserSelect(ip string) (httpclient.Selection, error)
	}
	snmp interface {
		Poll(ip string, instanceID int, timeout time.Duration) (snmp.State, error)
	}
	tcp interface {
		Download(ip string, resolution int) ([][]byte, error)
	}
}

func runWithDeps(ctx context.Context, ip, output string, profile httpclient.Profile, d deps) error {
	uid := "testuid"

	d.http.Deregister(ip, userID, uid)

	instanceID, err := d.http.Register(ip, userID, uid)
	consecutiveFailures := 0
	if err != nil {
		consecutiveFailures = reRegisterAfterFailures
	}

	defer d.http.Deregister(ip, userID, uid)

	lastState := snmp.Idle
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		state, err := d.snmp.Poll(ip, instanceID, 2*time.Second)
		if err != nil {
			consecutiveFailures++
			if consecutiveFailures >= reRegisterAfterFailures {
				d.http.Deregister(ip, userID, uid)
				if newID, rerr := d.http.Register(ip, userID, uid); rerr == nil {
					instanceID = newID
					lastState = snmp.Idle
					consecutiveFailures = 0
				}
			}
			if _, ok := ctx.Deadline(); !ok {
				if f, ok := d.snmp.(*fakeSNMP); ok && f.pos >= len(f.states) {
					return nil
				}
			}
			continue
		}
		consecutiveFailures = 0

		if state == snmp.Triggered && lastState != snmp.Triggered {
			d.http.PostAppList(ip, appIndex, profile)
		}

		if state == snmp.Ready {
			sel, _ := d.http.GetUserSelect(ip)
			if sel.AppIndex == appIndex {
				resolution := resolutionMap[sel.Resolution]
				if resolution == 0 {
					resolution = 300
				}
				if err := downloadAndSaveDeps(d.tcp, ip, output, resolution, sel.Format); err != nil {
					return err
				}
			}
		}

		lastState = state

		// In tests we use a cancelable context with no real ticking
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if _, ok := ctx.Deadline(); !ok {
			// No deadline set — check if snmp fake is exhausted to avoid infinite loop
			if f, ok := d.snmp.(*fakeSNMP); ok && f.pos >= len(f.states) {
				return nil
			}
		}
	}
}

func downloadAndSaveDeps(tcpDep interface {
	Download(ip string, resolution int) ([][]byte, error)
}, ip, output string, resolution int, format string) error {
	pages, err := tcpDep.Download(ip, resolution)
	if err != nil {
		return err
	}

	ts := time.Now().Format("20060102_150405")
	var (
		data []byte
		ext  string
	)
	isPDF := strings.Contains(format, "PDF")
	if isPDF {
		data, err = imageutil.PagesToPDF(pages)
		if err != nil {
			return err
		}
		ext = "pdf"
	} else {
		data = pages[0]
		ext = "jpg"
	}

	path := filepath.Join(output, "scan_"+ts+"."+ext)
	return os.WriteFile(path, data, 0644)
}

// --- Tests ---

func makeActiveSelection() httpclient.Selection {
	return httpclient.Selection{
		AppIndex:   appIndex,
		Resolution: "DPI_300",
		Color:      "COLOR_TRUE",
		Format:     "FORMAT_M_PDF",
		Size:       "SIZE_A4",
	}
}

func TestFullFlow(t *testing.T) {
	tmp := t.TempDir()
	fh := &fakeHTTP{registerReturn: 3, userSelectRet: makeActiveSelection()}
	fs := &fakeSNMP{states: []snmp.State{snmp.Idle, snmp.Triggered, snmp.Ready}}
	ft := &fakeTCP{pageBytes: [][]byte{makeJPEGBytes(50, 50)}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft})
	if err != nil {
		t.Fatal(err)
	}

	if len(fh.appListCalls) == 0 {
		t.Error("expected PostAppList to be called")
	}
	if ft.callCount == 0 {
		t.Error("expected Download to be called")
	}
}

func TestIdleDoesNotDownload(t *testing.T) {
	tmp := t.TempDir()
	fh := &fakeHTTP{registerReturn: 3}
	fs := &fakeSNMP{states: []snmp.State{snmp.Idle, snmp.Idle, snmp.Idle}}
	ft := &fakeTCP{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft})

	if ft.callCount != 0 {
		t.Error("Download should not be called when state is always idle")
	}
	if len(fh.appListCalls) != 0 {
		t.Error("PostAppList should not be called when state is always idle")
	}
}

func TestSavesToOutput(t *testing.T) {
	tmp := t.TempDir()
	jpegBytes := makeJPEGBytes(50, 50)
	fh := &fakeHTTP{registerReturn: 3, userSelectRet: httpclient.Selection{
		AppIndex: appIndex, Resolution: "DPI_300", Format: "FORMAT_JPEG",
	}}
	fs := &fakeSNMP{states: []snmp.State{snmp.Triggered, snmp.Ready}}
	ft := &fakeTCP{pageBytes: [][]byte{jpegBytes}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft})

	entries, _ := os.ReadDir(tmp)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
}

func TestFilenameHasTimestamp(t *testing.T) {
	tmp := t.TempDir()
	fh := &fakeHTTP{registerReturn: 3, userSelectRet: httpclient.Selection{
		AppIndex: appIndex, Resolution: "DPI_300", Format: "FORMAT_JPEG",
	}}
	fs := &fakeSNMP{states: []snmp.State{snmp.Ready}}
	ft := &fakeTCP{pageBytes: [][]byte{makeJPEGBytes(50, 50)}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft})

	entries, _ := os.ReadDir(tmp)
	if len(entries) == 0 {
		t.Fatal("no files saved")
	}
	name := entries[0].Name()
	if !regexp.MustCompile(`^scan_\d{8}_\d{6}\.(jpg|pdf)$`).MatchString(name) {
		t.Errorf("unexpected filename: %q", name)
	}
}

func TestResolutionPassedToDownload(t *testing.T) {
	tmp := t.TempDir()
	fh := &fakeHTTP{registerReturn: 3, userSelectRet: httpclient.Selection{
		AppIndex: appIndex, Resolution: "DPI_150", Format: "FORMAT_JPEG",
	}}
	fs := &fakeSNMP{states: []snmp.State{snmp.Ready}}
	ft := &fakeTCP{pageBytes: [][]byte{makeJPEGBytes(50, 50)}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Wire a capturing TCP dep
	var capturedResolution int
	captureTCP := &captureResolutionTCP{inner: ft, captured: &capturedResolution}
	runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, captureTCP})

	if capturedResolution != 150 {
		t.Errorf("want resolution=150, got %d", capturedResolution)
	}
}

type captureResolutionTCP struct {
	inner    *fakeTCP
	captured *int
}

func (c *captureResolutionTCP) Download(ip string, resolution int) ([][]byte, error) {
	*c.captured = resolution
	return c.inner.Download(ip, resolution)
}

func TestTriggeredSendsAppListOnlyOnTransition(t *testing.T) {
	tmp := t.TempDir()
	fh := &fakeHTTP{registerReturn: 3, userSelectRet: httpclient.Selection{AppIndex: 0}}
	fs := &fakeSNMP{states: []snmp.State{snmp.Triggered, snmp.Triggered, snmp.Idle}}
	ft := &fakeTCP{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft})

	if len(fh.appListCalls) != 1 {
		t.Errorf("expected PostAppList called once, got %d", len(fh.appListCalls))
	}
}

func TestWrongAppIndexDoesNotDownload(t *testing.T) {
	tmp := t.TempDir()
	fh := &fakeHTTP{registerReturn: 3, userSelectRet: httpclient.Selection{
		AppIndex: appIndex + 1, Resolution: "DPI_300", Format: "FORMAT_JPEG",
	}}
	fs := &fakeSNMP{states: []snmp.State{snmp.Ready}}
	ft := &fakeTCP{pageBytes: [][]byte{makeJPEGBytes(50, 50)}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft})

	if ft.callCount != 0 {
		t.Error("Download should not be called for wrong AppIndex")
	}
}

func TestCleanupCalledAtStartup(t *testing.T) {
	tmp := t.TempDir()
	fh := &fakeHTTP{registerReturn: 3, userSelectRet: httpclient.Selection{AppIndex: 0}}
	fs := &fakeSNMP{states: []snmp.State{snmp.Idle}}
	ft := &fakeTCP{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft})

	// deregister called at startup (cleanup) + defer at shutdown = ≥2
	if fh.deregisterCalls < 2 {
		t.Errorf("expected deregister at startup+shutdown, got %d calls", fh.deregisterCalls)
	}
}

func TestReRegistersAfterOutage(t *testing.T) {
	tmp := t.TempDir()
	// First Register returns 3 (startup); the recovery Register returns 7.
	fh := &fakeHTTP{registerReturns: []int{3, 7}, userSelectRet: makeActiveSelection()}
	// 3 failing polls (printer offline) then an idle success (recovered).
	fs := &fakeSNMP{
		states: []snmp.State{snmp.Idle, snmp.Idle, snmp.Idle, snmp.Idle},
		errs:   []error{snmp.ErrNoResponse, snmp.ErrNoResponse, snmp.ErrNoResponse, nil},
	}
	ft := &fakeTCP{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft}); err != nil {
		t.Fatal(err)
	}

	if fh.registerCalls != 2 {
		t.Errorf("expected re-registration (2 Register calls), got %d", fh.registerCalls)
	}
	// After recovery, subsequent polls must use the new instanceID (7), not 3.
	last := fs.polledWith[len(fs.polledWith)-1]
	if last != 7 {
		t.Errorf("expected polling with new instanceID 7 after re-register, got %d", last)
	}
}

func TestSurvivesInitialRegisterFailure(t *testing.T) {
	tmp := t.TempDir()
	// Startup Register fails (printer offline/asleep); the recovery Register
	// returns instanceID 7.
	fh := &fakeHTTP{registerFailFirst: 1, registerReturn: 7, userSelectRet: makeActiveSelection()}
	// One failing poll (printer still down), then a successful idle poll.
	fs := &fakeSNMP{
		states: []snmp.State{snmp.Idle, snmp.Idle},
		errs:   []error{snmp.ErrNoResponse, nil},
	}
	ft := &fakeTCP{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Must NOT return an error despite the failed startup registration.
	if err := runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft}); err != nil {
		t.Fatalf("daemon exited on startup register failure: %v", err)
	}

	if fh.registerCalls != 2 {
		t.Errorf("expected startup + recovery Register (2 calls), got %d", fh.registerCalls)
	}
	if last := fs.polledWith[len(fs.polledWith)-1]; last != 7 {
		t.Errorf("expected polling with recovered instanceID 7, got %d", last)
	}
}

func TestMultiPagePDFOutput(t *testing.T) {
	tmp := t.TempDir()
	page1 := makeJPEGBytes(50, 70)
	page2 := makeJPEGBytes(50, 70)
	fh := &fakeHTTP{registerReturn: 3, userSelectRet: httpclient.Selection{
		AppIndex: appIndex, Resolution: "DPI_300", Format: "FORMAT_M_PDF",
	}}
	fs := &fakeSNMP{states: []snmp.State{snmp.Ready}}
	ft := &fakeTCP{pageBytes: [][]byte{page1, page2}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft})

	entries, _ := os.ReadDir(tmp)
	if len(entries) == 0 {
		t.Fatal("no output file saved")
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, ".pdf") {
		t.Errorf("multi-page PDF format: want .pdf suffix, got %q", name)
	}
}

func TestFormatRoutingJPEG(t *testing.T) {
	tmp := t.TempDir()
	fh := &fakeHTTP{registerReturn: 3, userSelectRet: httpclient.Selection{
		AppIndex: appIndex, Resolution: "DPI_300", Format: "FORMAT_JPEG",
	}}
	fs := &fakeSNMP{states: []snmp.State{snmp.Ready}}
	ft := &fakeTCP{pageBytes: [][]byte{makeJPEGBytes(50, 50)}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runWithDeps(ctx, "192.168.1.128", tmp, httpclient.Profile{}, deps{fh, fs, ft})

	entries, _ := os.ReadDir(tmp)
	if len(entries) == 0 {
		t.Fatal("no output file")
	}
	if !strings.HasSuffix(entries[0].Name(), ".jpg") {
		t.Errorf("JPEG format: want .jpg, got %q", entries[0].Name())
	}
}
