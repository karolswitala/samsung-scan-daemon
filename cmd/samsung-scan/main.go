// Command samsung-scan is a background daemon for the Samsung M2070W Scan-to-PC
// protocol. It polls the printer via SNMP, registers this machine as a named
// destination ("My Mac"), and downloads scans over TCP port 9400 whenever the
// user initiates a scan from the printer's LCD.
//
// Usage:
//
//	samsung-scan --ip <printer-ip> [--output <dir>] [--poll <duration>] [--log-level <level>]
//	samsung-scan --ip <printer-ip> --cleanup
//
// The format, resolution, and color mode are selected by the user on the printer
// LCD and are read from UserSelect.xml after each scan; they are not CLI flags.
//
// Output files are written as scan_YYYYMMDD_HHMMSS.{pdf|jpg} in the --output
// directory. PDF is used when the user selects any PDF format on the printer;
// JPEG is the fallback.
//
// The daemon runs as a per-user LaunchAgent — one instance per logged-in user,
// running as that user. When --output is omitted it writes to the running user's
// own ~/Desktop.
//
// If the printer is power-cycled while the daemon runs, it drops the in-memory
// Scan2PC registration. The poll loop detects the printer no longer answering for
// our OID and automatically re-registers when it becomes reachable again.
//
// SIGINT and SIGTERM trigger a clean shutdown that deregisters this machine from
// the printer before exiting.
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

	// reRegisterAfterFailures is how many consecutive polls with no usable
	// response we tolerate before assuming the printer was power-cycled (or went
	// offline) and re-registering. At the default 3s poll interval this is ~9s.
	reRegisterAfterFailures = 3
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

// outputFlag is "" when auto-detection is active, or an explicit path when --output is set.
func run(ctx context.Context, ip, outputFlag string, pollInterval time.Duration) error {
	profile := defaultProfile
	uid := uniqueID()

	instanceID, err := registerWithPrinter(ip, uid, false)
	consecutiveFailures := 0
	if err != nil {
		// The printer may be off or asleep at startup. Don't exit (a fatal exit
		// just crash-loops under launchd and rarely catches a wake window) —
		// enter the poll loop and let the recovery path register once it's
		// reachable. Pre-arm the counter so the first failed poll re-registers.
		slog.Warn("initial registration failed — will keep retrying until the printer is reachable", "err", err)
		consecutiveFailures = reRegisterAfterFailures
	} else {
		slog.Info("registered", "instanceID", instanceID, "slot", appIndex)
		slog.Info("press Scan → PC → My Mac on the printer to scan")
	}

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
			consecutiveFailures++
			// Log the outage once at WARN, then keep the per-poll repeats at DEBUG
			// so a long outage doesn't flood the log.
			if consecutiveFailures == 1 {
				slog.Warn("printer unreachable — polling until it returns", "err", err)
			} else {
				slog.Debug("SNMP poll error", "err", err, "consecutive", consecutiveFailures)
			}

			// After a run of failures, assume the printer was power-cycled and
			// dropped our registration — re-register so "My Mac" reappears. If
			// the printer is still offline the register also fails and we retry
			// on the next poll. Only the first attempt of an outage is a WARN.
			if consecutiveFailures >= reRegisterAfterFailures {
				quiet := consecutiveFailures > reRegisterAfterFailures
				if newID, rerr := registerWithPrinter(ip, uid, quiet); rerr != nil {
					if quiet {
						slog.Debug("re-registration retry failed", "err", rerr, "consecutive", consecutiveFailures)
					} else {
						slog.Warn("re-registration failed (printer offline?), retrying each poll until it returns", "err", rerr)
					}
				} else {
					instanceID = newID
					lastState = snmp.Idle // re-arm PostAppList on the next Triggered
					slog.Info("re-registered after outage", "instanceID", instanceID, "slot", appIndex, "afterFailedPolls", consecutiveFailures)
					consecutiveFailures = 0
				}
			}
			continue
		}
		if consecutiveFailures > 0 {
			slog.Info("printer reachable again", "afterFailedPolls", consecutiveFailures)
			consecutiveFailures = 0
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

			outDir := outputFlag
			if outDir == "" {
				outDir = expandHome("~/Desktop")
			}

			actualFormat := sel.Format
			if err := downloadAndSave(ip, outDir, resolution, actualFormat); err != nil {
				slog.Error("download failed", "err", err)
			}
		}

		lastState = state
	}
}

func downloadAndSave(ip, output string, resolution int, format string) error {
	slog.Info("downloading")
	rawPages, err := tcp.Download(ip, resolution)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	var pages [][]byte
	for i, raw := range rawPages {
		assembled, err := imageutil.AssembleStrips(raw)
		if err != nil {
			return fmt.Errorf("assemble page %d: %w", i+1, err)
		}
		pages = append(pages, assembled)
	}
	slog.Info("download complete", "pages", len(pages))

	ts := time.Now().Format("20060102_150405")
	var (
		data []byte
		ext  string
	)

	isPDF := strings.Contains(format, "PDF")
	if isPDF {
		var pdfErr error
		data, pdfErr = imageutil.PagesToPDF(pages)
		if pdfErr != nil {
			return fmt.Errorf("PDF generation: %w", pdfErr)
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

// registerWithPrinter clears any stale entry then registers this machine,
// returning the InstanceID the printer assigns. Used at startup and to recover
// after the printer is power-cycled. When quiet is true the deregister warning is
// demoted to debug — during an ongoing outage the deregister fails every retry
// with a connectivity error, which is noise, not a stale-entry problem.
func registerWithPrinter(ip, uid string, quiet bool) (int, error) {
	if err := httpclient.Deregister(ip, userID, uid); err != nil {
		if quiet {
			slog.Debug("deregister before register failed", "err", err)
		} else {
			slog.Warn("deregister before register failed — stale entry with different UniqueID? Use --cleanup or curl to remove it manually", "err", err)
		}
	}
	return httpclient.Register(ip, userID, uid)
}

func main() {
	ip := flag.String("ip", "", "Printer IP address (required)")
	output := flag.String("output", "", "Output directory for scanned files (default: active user's Desktop)")
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
