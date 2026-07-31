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
// If the printer is power-cycled it drops its in-memory Scan-to-PC table, and
// "My Mac" silently disappears from the LCD menu. The poll loop detects the
// printer no longer answering for our OID and re-registers once it is reachable
// again, so the destination reappears without restarting the daemon.
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

	// reRegisterAfterFailures is how many consecutive polls with no usable
	// response we tolerate before assuming the printer was power-cycled (or went
	// offline) and re-registering. At the default 3s poll interval this is ~9s.
	reRegisterAfterFailures = 3
)

// Indirected so tests can drive run() without a printer or a GUI session.
// Production always uses the real implementations.
var (
	pollFn   = snmp.Poll
	isActive = isActiveConsoleUser
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

	// consecutiveFailures counts polls that returned no usable response. It is
	// deliberately NOT reset when the outage clears registration below: once
	// registered goes false the loop stops polling, so the count freezes at the
	// value that tripped the threshold, which is what afterFailedPolls reports on
	// the recovery log line.
	consecutiveFailures := 0

	// registerFailures throttles the registration WARN separately, because that
	// path also runs before we have ever registered — a printer switched off at
	// login is retried every tick with consecutiveFailures still at 0.
	registerFailures := 0

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

		active := isActive()

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
				// This retries every tick for as long as the printer is
				// unreachable — overnight, if it was switched off. Only the
				// first attempt is a WARN; the rest would flood a log file that
				// has no rotation.
				registerFailures++
				if registerFailures == 1 {
					slog.Warn("register failed — retrying every poll until the printer answers", "err", err)
				} else {
					slog.Debug("register retry failed", "err", err, "consecutiveRegisterFailures", registerFailures)
				}
				continue
			}
			registerFailures = 0
			instanceID = id
			registered = true
			lastState = snmp.Idle
			if consecutiveFailures > 0 {
				slog.Info("re-registered after outage", "instanceID", instanceID, "slot", appIndex, "afterFailedPolls", consecutiveFailures)
				consecutiveFailures = 0
			} else {
				slog.Info("registered", "instanceID", instanceID, "slot", appIndex)
				slog.Info("press Scan → PC → My Mac on the printer to scan")
			}
			httpclient.PostAppList(ip, appIndex, profile)
			continue
		}

		if registered && !active {
			slog.Info("no longer active console user — deregistering")
			httpclient.Deregister(ip, userID, uid)
			registered = false
			lastState = snmp.Idle
			// A user switch is a clean handover, not an outage — clear the count
			// so registering as the next console user isn't logged as a recovery.
			consecutiveFailures = 0
			continue
		}

		if !registered {
			slog.Debug("idle — not active console user")
			continue
		}

		state, err := pollFn(ip, instanceID, 2*time.Second)
		if err != nil {
			consecutiveFailures++
			// Log the outage once at WARN, then keep the per-poll repeats at
			// DEBUG so a long outage doesn't flood the log.
			if consecutiveFailures == 1 {
				slog.Warn("printer unreachable — polling until it returns", "err", err)
			} else {
				slog.Debug("SNMP poll error", "err", err, "consecutive", consecutiveFailures)
			}

			// After a run of failures, assume the printer was power-cycled and
			// dropped our registration. Clearing `registered` hands recovery to
			// the registration branch above, so the ARP guard and console-user
			// check still apply on the way back in.
			if consecutiveFailures >= reRegisterAfterFailures {
				// Clear any entry the printer may still be holding for us —
				// otherwise the re-register can come back DUPLICATE_USER. This
				// fails with a connectivity error while the printer is still
				// down, which is expected noise, so it stays at DEBUG.
				if derr := httpclient.Deregister(ip, userID, uid); derr != nil {
					slog.Debug("deregister before re-register failed", "err", derr)
				}
				registered = false
				lastState = snmp.Idle
			}
			continue
		}
		if consecutiveFailures > 0 {
			// Recovered without ever tripping the threshold — a brief blip, so
			// the registration is still valid and nothing needs re-registering.
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
