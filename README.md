# samsung-scan

Background daemon that implements the Samsung M2070W Scan-to-PC protocol in Go.
Place a document in the flatbed (or ADF), press **Scan → PC → My Mac** on the
printer, and a timestamped JPEG or PDF appears on your Desktop — no Samsung
software required.

Runs on macOS M-series (ARM64) via launchd and on Linux/x86 via Docker.
Idle memory footprint: ~8 MB RSS. Static binary, no runtime dependencies.

---

## Table of Contents

1. [Architecture](#architecture)
2. [Requirements](#requirements)
3. [Quick start](#quick-start)
4. [Building](#building)
5. [Installing on macOS (launchd)](#installing-on-macos-launchd)
6. [Running in Docker](#running-in-docker)
7. [Flags](#flags)
8. [How scanning works](#how-scanning-works)
9. [Output formats](#output-formats)
10. [Multi-page ADF scans](#multi-page-adf-scans)
11. [Development](#development)
12. [Troubleshooting](#troubleshooting)
13. [Protocol reference](#protocol-reference)

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│ samsung-scan daemon                                             │
│                                                                 │
│  internal/snmp        SNMP GET every 3 s (UDP 161)             │
│                       OID .1.3.6.1.4.1.236.11.5.11.81.11.      │
│                       7.2.1.2.{InstanceID}                      │
│     idle → triggered → ready                                    │
│          │                                                      │
│  internal/httpclient  HTTP (TCP 80)                             │
│                       S2PC_Regi    — register / deregister      │
│                       S2PC_AppList — announce scan profile      │
│                       UserSelect   — read user's selection      │
│                                                                 │
│  internal/tcp         Binary protocol (TCP 9400)               │
│                       probe conn → data conn                    │
│                       REQUEST / PARAMS / DIMS / READY           │
│                       POLL / FETCH loop (all pages, same conn)  │
│                                                                 │
│  internal/imageutil   Post-processing                           │
│                       AssembleStrips → PagesToPDF / JPEG        │
│                                                                 │
│  cmd/samsung-scan     Daemon loop, signal handling, file I/O   │
└─────────────────────────────────────────────────────────────────┘
                              │
                    ~/Desktop/scan_20260620_143022.pdf
```

### State machine

```
              ┌─────────────────────────────────────┐
  startup ───▶│               idle                  │◀─── after save
              └──────────────────┬──────────────────┘
                                 │ SNMP = 0x01
                                 │ (user opened scan menu)
                                 ▼
              ┌─────────────────────────────────────┐
              │            triggered                │──── send HTTP AppList
              └──────────────────┬──────────────────┘     "My Mac" = Available
                                 │ SNMP = 0x02
                                 │ (user confirmed scan)
                                 ▼
              ┌─────────────────────────────────────┐
              │              ready                  │──── GetUserSelect (HTTP)
              └──────────────────┬──────────────────┘     Download (TCP 9400)
                                 │                        AssembleStrips
                                 │                        save to --output dir
                                 └────────────────────▶ idle
```

---

## Requirements

- **Go 1.23+** (`brew install go` or [go.dev/dl](https://go.dev/dl))
- Samsung M2070W printer reachable on the local network
- macOS for the launchd path; any Linux host or Docker for the container path

---

## Quick start

```bash
# Build for macOS M-series
make build-mac

# Kill any previous instance first (important — see Troubleshooting)
pkill -f samsung-scan-macos || true

# Launch against your printer
./dist/samsung-scan-macos --ip 192.168.1.128 --output ~/Desktop --log-level debug

# On the printer: Scan → PC → My Mac → (adjust settings if desired) → Start
# File appears in ~/Desktop/scan_YYYYMMDD_HHMMSS.{pdf|jpg}
```

---

## Building

```bash
make build-mac      # dist/samsung-scan-macos  (darwin/arm64)
make build-linux    # dist/samsung-scan-linux  (linux/amd64, fully static)
make test           # run all tests with -v
make lint           # go vet ./...
make docker         # multi-arch Docker image (requires buildx)
make clean          # remove dist/
```

Go must be on `PATH` or at `/usr/local/go/bin/go`. Override the path if needed:

```bash
GO=/opt/homebrew/bin/go make build-mac
```

---

## Installing on macOS (launchd)

The daemon runs as a user LaunchAgent — it starts at login and restarts
automatically if it crashes.

### One-step install

```bash
./install.sh
```

This builds the binary, copies it to `/usr/local/bin/samsung-scan`, and
installs the plist under `~/Library/LaunchAgents/`. Then edit the plist to
set your printer IP and output directory:

```bash
$EDITOR ~/Library/LaunchAgents/com.local.samsung-scan.plist
```

Change `192.168.1.128` and `/Users/karol/Desktop` to your values.

### Load / unload

```bash
# Start the agent (also happens automatically at next login)
launchctl load ~/Library/LaunchAgents/com.local.samsung-scan.plist

# Stop the agent
launchctl unload ~/Library/LaunchAgents/com.local.samsung-scan.plist

# View merged stdout/stderr log
tail -f /tmp/samsung-scan.log
```

### Uninstall

```bash
launchctl unload ~/Library/LaunchAgents/com.local.samsung-scan.plist
rm ~/Library/LaunchAgents/com.local.samsung-scan.plist
sudo rm /usr/local/bin/samsung-scan
```

---

## Running in Docker

```bash
# Build the image (~10 MB, FROM scratch)
make docker

# Run with a mounted output directory
docker run --rm \
  -v /tmp/scans:/scans \
  samsung-scan:latest \
  --ip 192.168.1.128 \
  --output /scans \
  --log-level info
```

The final image is built `FROM scratch` with a fully static binary — no OS
layer, no shell, no libc.

---

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--ip` | *(required)* | Printer IP address |
| `--output` | `~/Desktop` | Directory where scanned files are saved |
| `--poll` | `3s` | SNMP poll interval (Go duration: `2s`, `500ms`, …) |
| `--cleanup` | `false` | Deregister this machine from the printer and exit |
| `--log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |

### Format, resolution, and color are not flags

These come from the user's selection on the printer LCD, not from CLI arguments.
The daemon advertises a default profile (300 DPI / color / PDF) via
`S2PC_AppList`, then reads back whatever the user actually confirmed via
`UserSelect.xml`. The printer's selection always overrides.

### `--cleanup`

Removes a stale `S2PC_Regi` entry left by a crashed or force-killed previous
run. Use when `My Mac` appears on the printer but the daemon is not running:

```bash
./dist/samsung-scan-macos --ip 192.168.1.128 --cleanup
```

---

## How scanning works

### 1. Registration (startup)

The daemon sends HTTP `S2PC_Regi ADD` to create a named entry (`My Mac`) in
the printer's scan destination list. The printer returns an `InstanceID` — a
monotonically increasing counter used as the trailing arc of the SNMP OID for
state polling.

A deregister is always attempted first to clear stale entries from previous runs.

### 2. SNMP polling

Every `--poll` interval the daemon sends an SNMPv1 GET to OID
`1.3.6.1.4.1.236.11.5.11.81.11.7.2.1.2.{InstanceID}`. The response encodes
one of three states:

| State | Byte value (BE) | Meaning |
|-------|----------------|---------|
| `idle` | `00 00 00 00` | No scan activity |
| `triggered` | `00 00 00 01` | User opened the scan menu |
| `ready` | `00 00 00 02` | User confirmed destination and pressed Start |

Both big-endian and little-endian 4-byte forms are accepted (firmware varies).

### 3. AppList (idle → triggered edge)

On the first poll that returns `triggered`, the daemon immediately POSTs
`S2PC_AppList`. This is what makes `My Mac` appear as **Available** (not greyed
out) on the LCD. The POST must arrive while the user still has the scan menu
open. The printer never sends an HTTP response to AppList — the daemon
abandons the connection after 8 seconds.

### 4. Download (ready state)

When the state reaches `ready`:

1. `GET /IDS/UserSelect.xml` — reads the format, resolution, and color the
   user selected on the LCD.
2. Opens TCP port 9400, runs the binary scan protocol, and receives all image
   strips on a single connection.
3. Reassembles JPEG strips into complete page images.
4. Encodes as PDF or saves as JPEG.
5. Writes `scan_YYYYMMDD_HHMMSS.{pdf|jpg}` to `--output`.

### 5. Clean shutdown

`SIGINT` / `SIGTERM` trigger a `S2PC_Regi DELETE` before the process exits, so
`My Mac` disappears from the printer's destination list immediately.

---

## Output formats

The format is determined by what the user selects on the printer LCD.

| LCD selection | Saved as | Notes |
|---------------|----------|-------|
| `FORMAT_M_PDF` | `.pdf` | Multi-page PDF; all ADF pages in one file |
| `FORMAT_S_PDF` | `.pdf` | Single-page PDF |
| `FORMAT_JPEG` | `.jpg` | JPEG; for multi-page scans only the first page |
| `FORMAT_M_TIFF` / `FORMAT_S_TIFF` | `.jpg` | TIFF not supported; falls back to JPEG |

Filename pattern: `scan_YYYYMMDD_HHMMSS.{pdf|jpg}` in `--output`.

---

## Multi-page ADF scans

Put a stack of documents in the ADF and scan normally. All pages are downloaded
over the **same** TCP connection — the daemon re-enters the POLL/FETCH loop
without reconnecting between pages.

After each page's last chunk is received, the daemon sends a PARAMS packet and
checks the 255-byte next-page status response:

| `status[1]` | Meaning | Action |
|-------------|---------|--------|
| `0x04` | No more pages | Send END + DISC, close connection |
| `0x00` | Next page ready | Re-enter POLL/FETCH on same connection |
| other (`0x08`, …) | Printer still processing | Send PARAMS again |

Run with `--log-level debug` to see the exact status byte
(`msg=next-page status=0x??`) on each iteration — useful for verifying
behavior with your specific firmware version.

---

## Development

### Running tests

```bash
make test      # go test ./... -v
make lint      # go vet ./...
```

All tests are fully offline — no printer required. TCP tests use an
in-process mock server (`ProtocolServer` in `internal/tcp/tcp_test.go`) that
speaks the exact binary protocol. SNMP tests use a non-replying UDP socket to
exercise timeout handling. HTTP tests use `httptest.NewServer`.

### Package layout

```
cmd/samsung-scan/
    main.go           daemon loop, CLI flags, signal handling, file I/O
    main_test.go      integration tests with fake SNMP/HTTP/TCP deps

internal/snmp/
    snmp.go           SNMPv1 GET poller (UDP 161)
    snmp_test.go

internal/httpclient/
    http.go           S2PC_Regi, S2PC_AppList, UserSelect.xml (TCP 80)
    http_test.go

internal/tcp/
    tcp.go            binary image download protocol (TCP 9400)
    tcp_test.go       ProtocolServer mock + multi-page tests

internal/imageutil/
    assemble.go       JPEG strip assembly, multi-page PDF writer
    assemble_test.go
```

### Protocol quirks (do not change without understanding)

**Unquoted multipart boundary.** Go's `mime/multipart` produces
`boundary="EPM Scan2PC Post Request"` (RFC-correct, with quotes). The Samsung
printer rejects this and returns an XML parse error. The daemon manually
constructs the Content-Type header:
```
Content-Type: multipart/form-data; boundary=EPM Scan2PC Post Request
```
See `internal/httpclient/http.go` → `buildMultipart`.

**Next-page PARAMS loop.** After the last image chunk, the printer may send
several intermediate status bytes (e.g. `0x08`) before signalling done
(`0x04`) or next-page ready (`0x00`). The loop must not break on the first
non-`0x04` byte. See `internal/tcp/tcp.go` → `Download`.

**AppList timing.** `S2PC_AppList` must arrive while SNMP shows `triggered`.
A POST at any other time is silently ignored.

**Same TCP connection for multi-page.** After each page, re-enter the
POLL/FETCH loop on the existing data connection. Opening a new TCP connection
per page causes the printer to close the connection with EOF.

---

## Troubleshooting

### `My Mac` shows as "Not Available" on the LCD

The AppList POST did not arrive while the scan menu was open. Ensure the
daemon is running *before* opening the scan menu. If the issue persists, check
the log for `scan menu opened — announcing profile`.

### Registration fails or `DUPLICATE_USER`

A previous run did not deregister cleanly. Fix:

```bash
./dist/samsung-scan-macos --ip 192.168.1.128 --cleanup
```

If the stale entry has a different UniqueID (from a test run or a different
machine), deregister manually with curl:

```bash
BOUNDARY="EPM Scan2PC Post Request"
printf -- "--${BOUNDARY}\r\nContent-Disposition: form-data; name=\"EPMScan2PC_Post\";filename=\"\"\r\nContent-Type: application/octet-stream\r\n\r\n<?xml version=\"1.0\" encoding=\"utf-8\"?><root><S2PC_Regi RegiType=\"DELETE\" UserID=\"My Mac\" UniqueID=\"REPLACE_ME\" /></root>\r\n--${BOUNDARY}--\r\n" | \
  curl -s -X POST "http://192.168.1.128/IDS/ScanFaxToPC.cgi" \
  -H "Content-Type: multipart/form-data; boundary=${BOUNDARY}" \
  -H "User-Agent: EPM Scan2PC" --data-binary @-
```

Replace `REPLACE_ME` with the stale UniqueID (first 16 hex chars of the MD5
of the relevant machine's hostname).

### Two daemon instances running simultaneously

Always kill the previous instance before launching a new one:

```bash
pkill -f samsung-scan-macos || true
./dist/samsung-scan-macos --ip 192.168.1.128 ...
```

Two instances share the same registration and cause `DUPLICATE_USER` errors.

### Download fails or produces a corrupt/empty file

Run with `--log-level debug` and scan again. Look for error lines. Common
causes: 30-second TCP deadline exceeded (very large scan at high DPI), or a
network interruption between chunks.

---

## Protocol reference

The full wire-level specification is in [`../PROTOCOL.md`](../PROTOCOL.md).

Key facts for the Go implementation:

| Channel | Transport | Role |
|---------|-----------|------|
| SNMP | UDP 161 | State polling (idle/triggered/ready) |
| HTTP | TCP 80 | Registration, AppList, UserSelect |
| Binary | TCP 9400 | Image download |

- SNMP OID: `1.3.6.1.4.1.236.11.5.11.81.11.7.2.1.2.{InstanceID}`
- Multipart boundary must be **unquoted** in Content-Type
- TCP 9400: probe connection (handshake only) then data connection (full protocol)
- Each FETCH response is a self-contained JPEG for 32 scan lines; strips must be reassembled
- ADF multi-page: re-enter POLL/FETCH on the **same** data connection; do not reconnect
