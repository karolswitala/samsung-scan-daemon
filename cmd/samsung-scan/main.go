// Command samsung-scan is a per-user LaunchAgent daemon for the Samsung M2070W
// Scan-to-PC protocol. It polls the printer via SNMP, registers this machine as
// a named destination ("My Mac"), and downloads scans over TCP port 9400 whenever
// the user initiates a scan from the printer's LCD.
//
// Usage:
//
//	samsung-scan --ip <printer-ip> [--output <dir>] [--poll <duration>] [--log-level <level>] [--enable-network-guard <mac>]
//	samsung-scan --ip <printer-ip> --cleanup
//
// The format, resolution, and color mode are selected by the user on the printer
// LCD and are read from UserSelect.xml after each scan; they are not CLI flags.
//
// Output files are written as scan_YYYYMMDD_HHMMSS.{pdf|jpg} in the --output
// directory. PDF is used when the user selects any PDF format on the printer;
// JPEG is the fallback.
//
// When --output is omitted the daemon writes to the user's own Desktop directory.
// Under fast user switching, each logged-in user runs their own agent instance;
// only the active (foreground) console user registers with the printer and receives
// scans. All users share the same printer identity ("My Mac") so exactly one scan
// target appears on the LCD at all times.
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
	"os/exec"
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

// outputFlag is "" when auto-detection is active, or an explicit path when --output is set.
// expectedMAC is "" when the network guard is disabled, or a normalized MAC to verify via ARP.
func run(ctx context.Context, ip, outputFlag string, pollInterval time.Duration, expectedMAC string) error {
	profile := defaultProfile
	uid := uniqueID()

	// Disable the guard if /usr/sbin/arp is absent (e.g. Linux scratch container).
	// Docker deployments run on trusted local networks and don't need the guard.
	guardMAC := expectedMAC
	if guardMAC != "" {
		if _, err := exec.LookPath("/usr/sbin/arp"); err != nil {
			slog.Info("network guard requested but /usr/sbin/arp not available on this platform — guard disabled",
				"flag", "--enable-network-guard")
			guardMAC = ""
		}
	}

	// Clean up stale registration from a previous crashed run.
	if err := httpclient.Deregister(ip, userID, uid); err != nil {
		slog.Warn("startup deregister failed — stale entry with different UniqueID? Use --cleanup or curl to remove it manually", "err", err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	registered := false
	var instanceID int
	lastState := snmp.Idle

	defer func() {
		if registered {
			slog.Info("deregistering")
			httpclient.Deregister(ip, userID, uid)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		active := isActiveConsoleUser()

		if !registered && active {
			if guardMAC != "" {
				mac, err := lookupARPMAC(ip)
				if err != nil {
					slog.Warn("MAC lookup failed — skipping guard this tick", "err", err)
				} else if mac == "" {
					slog.Debug("printer not reachable via ARP — will retry")
					continue
				} else if mac != guardMAC {
					slog.Warn("printer MAC mismatch — likely wrong network; not registering",
						"expected", guardMAC, "got", mac)
					continue
				} else {
					slog.Debug("printer MAC verified", "mac", mac)
				}
			}

			id, err := httpclient.Register(ip, userID, uid)
			if err != nil {
				slog.Warn("register failed", "err", err)
				continue
			}
			instanceID = id
			registered = true
			lastState = snmp.Idle
			slog.Info("registered", "instanceID", instanceID, "slot", appIndex)
			slog.Info("press Scan → PC → My Mac on the printer to scan")
			httpclient.PostAppList(ip, appIndex, profile)
			continue
		}

		if registered && !active {
			slog.Info("no longer active console user — deregistering")
			httpclient.Deregister(ip, userID, uid)
			registered = false
			lastState = snmp.Idle
			continue
		}

		if !registered {
			slog.Debug("idle — not active console user")
			continue
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

			outDir := outputFlag
			if outDir == "" {
				outDir = activeUserDesktop()
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

func main() {
	ip := flag.String("ip", "", "Printer IP address (required)")
	output := flag.String("output", "", "Output directory for scanned files (default: active user's Desktop)")
	poll := flag.Duration("poll", 3*time.Second, "SNMP poll interval")
	cleanup := flag.Bool("cleanup", false, "Deregister stale entries and exit")
	logLevel := flag.String("log-level", "info", "Log level: debug/info/warn/error")
	enableGuard := flag.String("enable-network-guard", "", "Expected printer MAC (e.g. 30:cd:a7:b8:c7:e9); enables ARP-based network guard (macOS only)")
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

	if err := run(ctx, *ip, *output, *poll, normalizeMAC(*enableGuard)); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// isActiveConsoleUser reports whether this process's user owns /dev/console,
// which is the foreground GUI user on macOS and updates on fast user switching.
func isActiveConsoleUser() bool {
	info, err := os.Stat("/dev/console")
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// activeUserDesktop returns the Desktop path for the running user.
func activeUserDesktop() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return expandHome("~/Desktop")
	}
	return filepath.Join(home, "Desktop")
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

// lookupARPMAC returns the normalized MAC address of the device at ip via
// /usr/sbin/arp -n, which sends an active ARP probe if the entry is not cached.
// Returns "" (no error) when the device does not respond to ARP.
func lookupARPMAC(ip string) (string, error) {
	out, err := exec.Command("/usr/sbin/arp", "-n", ip).Output()
	if err != nil {
		if len(out) > 0 && strings.Contains(string(out), "no entry") {
			return "", nil
		}
		return "", fmt.Errorf("arp -n %s: %w", ip, err)
	}
	// "? (192.168.1.128) at 30:cd:a7:b8:c7:e9 on en0 ifscope [ethernet]"
	line := string(out)
	idx := strings.Index(line, " at ")
	if idx < 0 {
		return "", nil
	}
	fields := strings.Fields(line[idx+4:])
	if len(fields) == 0 {
		return "", nil
	}
	return normalizeMAC(fields[0]), nil
}

// normalizeMAC lowercases s and reformats it as colon-separated hex pairs,
// accepting colons, dashes, or no separators as input.
func normalizeMAC(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer(":", "", "-", "", ".", "").Replace(s)
	if len(s) != 12 {
		return s
	}
	pairs := make([]string, 6)
	for i := range pairs {
		pairs[i] = s[i*2 : i*2+2]
	}
	return strings.Join(pairs, ":")
}
