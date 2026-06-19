# samsung-scan — context for Claude

## What this is
Go daemon that implements the reverse-engineered Samsung M2070W Scan-to-PC protocol.
Reference Python implementation lives in `../mvp/`. Protocol spec in `../PROTOCOL.md`.

## Build & run
```bash
export PATH=$PATH:/usr/local/go/bin   # Go is at /usr/local/go/bin/go
make build-mac                         # → dist/samsung-scan-macos (ARM64)
make build-linux                       # → dist/samsung-scan-linux (AMD64, static)
make test                              # 37 tests across 5 packages
./dist/samsung-scan-macos --ip 192.168.1.128 --output ~/Desktop --log-level debug
```

## Printer
- IP: `192.168.1.128` (Samsung M2070W)
- Before registering, check for stale `My Mac` entries — deregister first or use `--cleanup`
- The daemon deregisters at startup automatically, but if a previous run crashed the entry
  may have a different UniqueID; delete it manually via curl if needed:
  ```bash
  BOUNDARY="EPM Scan2PC Post Request"
  printf -- "--${BOUNDARY}\r\nContent-Disposition: form-data; name=\"EPMScan2PC_Post\";filename=\"\"\r\nContent-Type: application/octet-stream\r\n\r\n<?xml version=\"1.0\" encoding=\"utf-8\"?><root><S2PC_Regi RegiType=\"DELETE\" UserID=\"My Mac\" UniqueID=\"REPLACE_ME\" /></root>\r\n--${BOUNDARY}--\r\n" | \
    curl -s -X POST "http://192.168.1.128/IDS/ScanFaxToPC.cgi" \
    -H "Content-Type: multipart/form-data; boundary=${BOUNDARY}" \
    -H "User-Agent: EPM Scan2PC" --data-binary @-
  ```
- Our machine's UniqueID: `877a91c4c5ce9836` (MD5 of hostname, first 16 hex chars)

## Protocol gotchas (bugs found in live testing)

### 1. Multipart boundary must NOT be quoted
Go's `mime/multipart` produces `boundary="EPM Scan2PC Post Request"` (RFC-correct).
The Samsung printer rejects this — it needs the unquoted form:
`Content-Type: multipart/form-data; boundary=EPM Scan2PC Post Request`
Fixed in `internal/httpclient/http.go` — do not use `w.FormDataContentType()`.

### 2. Next-page check loop must keep iterating
After the POLL/FETCH image download, the client sends PARAMS and loops until the printer
responds with `status[1] == 0x04` ("no more pages"). The printer may send intermediate
responses before the final `0x04`. Breaking on the first non-`0x04` byte was incorrectly
treated as "another page ready", causing a second Download() call that failed with EOF.
Mirrors Python's `while True: ... if status[1] == 0x04: break`.

### 3. Multi-page ADF support is incomplete
`Download()` returns `(bytes, hasNextPage=false, err)` — always false for now.
True multi-page (ADF) requires knowing the printer's "next page ready" status byte,
which needs a protocol capture with an actual multi-page ADF scan.
The interface is already in place; the daemon loops on `hasNextPage` correctly.

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
- Filename: `scan_YYYYMMDD_HHMMSS.{ext}` in `--output` directory

## Shell hygiene reminder
When launching the daemon for testing, kill previous instances first:
```bash
pkill -f samsung-scan-macos
```
Running two instances simultaneously causes DUPLICATE_USER errors on the next launch.
