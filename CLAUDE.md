# samsung-scan — context for Claude

## What this is
Go daemon that implements the reverse-engineered Samsung M2070W Scan-to-PC protocol.
Protocol spec in `./PROTOCOL.md`.

## Build & run
```bash
make build-mac                         # → dist/samsung-scan-macos (ARM64)
make build-linux                       # → dist/samsung-scan-linux (AMD64, static)
make test                              # unit tests across all packages
./dist/samsung-scan-macos --ip <printer-ip> --log-level debug
# Flags: --ip (required), --output, --poll, --cleanup, --log-level
# Resolution/color/format are NOT CLI flags — they come from the printer (GetUserSelect)
```

Go must be on PATH. If not found: `export PATH=$PATH:/usr/local/go/bin`

## Printer
- Before registering, check for stale `My Mac` entries — deregister first or use `--cleanup`
- The daemon deregisters at startup automatically, but if a previous run crashed the entry
  may have a different UniqueID; delete it manually via curl if needed:
  ```bash
  BOUNDARY="EPM Scan2PC Post Request"
  printf -- "--${BOUNDARY}\r\nContent-Disposition: form-data; name=\"EPMScan2PC_Post\";filename=\"\"\r\nContent-Type: application/octet-stream\r\n\r\n<?xml version=\"1.0\" encoding=\"utf-8\"?><root><S2PC_Regi RegiType=\"DELETE\" UserID=\"My Mac\" UniqueID=\"REPLACE_ME\" /></root>\r\n--${BOUNDARY}--\r\n" | \
    curl -s -X POST "http://<printer-ip>/IDS/ScanFaxToPC.cgi" \
    -H "Content-Type: multipart/form-data; boundary=${BOUNDARY}" \
    -H "User-Agent: EPM Scan2PC" --data-binary @-
  ```
  Replace `REPLACE_ME` with the stale UniqueID from the printer's web UI, and `<printer-ip>` with your printer's IP.

## Protocol gotchas

### 1. Multipart boundary must NOT be quoted
Go's `mime/multipart` produces `boundary="EPM Scan2PC Post Request"` (RFC-correct).
The Samsung printer rejects this — it needs the unquoted form:
`Content-Type: multipart/form-data; boundary=EPM Scan2PC Post Request`
Fixed in `internal/httpclient/http.go` — do not use `w.FormDataContentType()`.

### 2. Next-page check loop must keep iterating
After the POLL/FETCH image download, the client sends PARAMS and loops until the printer
responds with `status[1] == 0x04` ("no more pages"). The printer sends intermediate
`0x08` responses while the ADF feeds the next page. Breaking early on any non-`0x04`
byte causes the connection to desync.

## Architecture
```
cmd/samsung-scan/main.go       — daemon loop, signal handling, format routing
internal/snmp/                 — SNMPv1 poller (UDP 161), state machine
internal/httpclient/           — HTTP registration, AppList, UserSelect (TCP 80)
internal/tcp/                  — binary image download (TCP 9400)
internal/imageutil/            — JPEG strip assembly, multi-page PDF (go-pdf/fpdf)
```

## Key constants
- `appIndex = 1` — AppList slot, always 1, independent of S2PC_Regi InstanceID
- `userID = "My Mac"` — display name on printer LCD
- SNMP OID: `1.3.6.1.4.1.236.11.5.11.81.11.7.2.1.2.{InstanceID}`
- Multipart boundary: `EPM Scan2PC Post Request` (exact string, unquoted in Content-Type)
- User-Agent: `EPM Scan2PC`
- TCP port 9400: probe connection (handshake only) then data connection (full protocol)

## Output formats
- `FORMAT_S_PDF` / `FORMAT_M_PDF` (contains "PDF") → saves `.pdf` via go-pdf/fpdf
- `FORMAT_JPEG` or anything else → saves `.jpg` (first page only)
- Filename: `scan_YYYYMMDD_HHMMSS.{ext}` in the output directory

## Shell hygiene
When launching the daemon manually for testing, kill previous instances first:
```bash
pkill -f samsung-scan-macos
```
Running two instances simultaneously causes DUPLICATE_USER errors on the next launch.
