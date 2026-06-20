package main

import (
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/karol/samsung-scan/internal/httpclient"
	"github.com/karol/samsung-scan/internal/imageutil"
	"github.com/karol/samsung-scan/internal/snmp"
	"github.com/karol/samsung-scan/internal/tcp"
)

const (
	userID   = "My Mac"
	appIndex = 1 // AppList profile slot; must be ≤ MaxUser (10). Independent of S2PC_Regi InstanceID.
)

var resolutionMap = map[string]int{
	"DPI_75": 75, "DPI_100": 100, "DPI_150": 150,
	"DPI_200": 200, "DPI_300": 300, "DPI_600": 600, "DPI_1200": 1200,
}

// defaultProfile is what we advertise to the printer LCD via PostAppList.
// The actual scan parameters always come from GetUserSelect (the user's choice on the printer).
var defaultProfile = httpclient.Profile{
	Resolution: "DPI_300",
	Color:      "COLOR_TRUE",
	Format:     "FORMAT_M_PDF",
	Size:       "SIZE_A4",
}

func uniqueID() string {
	hostname, _ := os.Hostname()
	sum := md5.Sum([]byte(hostname))
	return fmt.Sprintf("%x", sum)[:16]
}

func setupLogger(level string) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}

func run(ctx context.Context, ip, output string, pollInterval time.Duration) error {
	profile := defaultProfile
	uid := uniqueID()

	// Clean up stale registration from a previous run
	if err := httpclient.Deregister(ip, userID, uid); err != nil {
		slog.Debug("deregister at startup (expected if first run)", "err", err)
	}

	instanceID, err := httpclient.Register(ip, userID, uid)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	slog.Info("registered", "instanceID", instanceID, "slot", appIndex)
	slog.Info("press Scan → PC → My Mac on the printer to scan")

	defer func() {
		slog.Info("deregistering")
		httpclient.Deregister(ip, userID, uid)
	}()

	lastState := snmp.Idle
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		state, err := snmp.Poll(ip, instanceID, 2*time.Second)
		if err != nil {
			slog.Warn("SNMP poll error", "err", err)
			continue
		}

		if state != lastState {
			slog.Info("state transition", "from", lastState, "to", state)
		} else {
			slog.Debug("poll", "state", state)
		}

		if state == snmp.Triggered && lastState != snmp.Triggered {
			slog.Info("scan menu opened — announcing profile")
			httpclient.PostAppList(ip, appIndex, profile)
		}

		if state == snmp.Ready {
			sel, err := httpclient.GetUserSelect(ip)
			if err != nil {
				slog.Warn("GetUserSelect failed", "err", err)
				lastState = state
				continue
			}
			slog.Debug("UserSelect", "appIndex", sel.AppIndex, "format", sel.Format, "resolution", sel.Resolution)

			if sel.AppIndex != appIndex {
				slog.Debug("scan for different app index, skipping", "appIndex", sel.AppIndex)
				lastState = state
				continue
			}

			resolution := resolutionMap[sel.Resolution]
			if resolution == 0 {
				resolution = 300
			}

			actualFormat := sel.Format
			if err := downloadAndSave(ip, output, resolution, actualFormat); err != nil {
				slog.Error("download failed", "err", err)
			}
		}

		lastState = state
	}
}

func downloadAndSave(ip, output string, resolution int, format string) error {
	var pages [][]byte
	for {
		slog.Info("downloading page", "pageNum", len(pages)+1)
		pageBytes, hasNextPage, err := tcp.Download(ip, resolution)
		if err != nil {
			return fmt.Errorf("download page %d: %w", len(pages)+1, err)
		}

		assembled, err := imageutil.AssembleStrips(pageBytes)
		if err != nil {
			return fmt.Errorf("assemble strips page %d: %w", len(pages)+1, err)
		}
		pages = append(pages, assembled)
		slog.Info("page downloaded", "pageNum", len(pages))

		if !hasNextPage {
			break
		}
	}

	ts := time.Now().Format("20060102_150405")
	var (
		data []byte
		ext  string
		err  error
	)

	isPDF := strings.Contains(format, "PDF")
	if isPDF {
		data, err = imageutil.PagesToPDF(pages)
		if err != nil {
			return fmt.Errorf("PDF generation: %w", err)
		}
		ext = "pdf"
	} else {
		data = imageutil.PageToJPEG(pages[0])
		ext = "jpg"
	}

	path := filepath.Join(output, fmt.Sprintf("scan_%s.%s", ts, ext))
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	slog.Info("scan saved", "path", path, "pages", len(pages), "format", ext, "bytes", len(data))
	return nil
}

func main() {
	ip := flag.String("ip", "", "Printer IP address (required)")
	output := flag.String("output", expandHome("~/Desktop"), "Output directory for scanned files")
	poll := flag.Duration("poll", 3*time.Second, "SNMP poll interval")
	cleanup := flag.Bool("cleanup", false, "Deregister stale entries and exit")
	logLevel := flag.String("log-level", "info", "Log level: debug/info/warn/error")
	flag.Parse()

	setupLogger(*logLevel)

	if *ip == "" {
		slog.Error("--ip is required")
		flag.Usage()
		os.Exit(1)
	}

	if *cleanup {
		uid := uniqueID()
		if err := httpclient.Deregister(*ip, userID, uid); err != nil {
			slog.Warn("cleanup deregister", "err", err)
		} else {
			slog.Info("stale registration removed")
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *ip, *output, *poll); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
