# Completeness Assessment — samsung-scan

_Assessed 2026-06-23. Go daemon implementing the reverse-engineered Samsung
M2070W Scan-to-PC protocol._

## Verdict

**Functionally complete and production-shaped.** Every protocol phase documented
in `PROTOCOL.md` / `CLAUDE.md` maps to implemented code; the test suite is green;
`go vet` is clean; both target binaries build. No `TODO`/`FIXME`/`XXX`/`HACK`,
no `panic`, no `log.Fatal`, no stubs or empty function bodies in the source. All
findings from the security audit are remediated in the current tree.

The main remaining gap is project infrastructure, not functionality: **there is
no CI**, so the green state below is reproduced manually rather than enforced.

## Verified this run

| Check | Command | Result |
|-------|---------|--------|
| Unit tests | `go test ./...` | **52 passed, 0 failed** across 5 packages |
| Static analysis | `make lint` (`go vet ./...`) | clean (exit 0) |
| macOS build | `make build-mac` | builds `dist/samsung-scan-macos` |
| Toolchain | — | Go 1.26.4, darwin/arm64 |

Tests are fully hermetic — mock UDP/TCP/HTTP servers, `httptest`,
dependency-injected fakes, `t.TempDir()`. No live printer required.

## Protocol phase completeness

Every phase is implemented with traceable evidence:

| Phase | Status | Location |
|-------|--------|----------|
| Register / Deregister (S2PC_Regi ADD/DELETE) | ✅ | `internal/httpclient/http.go:158-207` |
| Startup stale-entry cleanup | ✅ | `cmd/samsung-scan/main.go` (startup deregister) |
| SNMPv1 polling + state machine (idle/triggered/ready) | ✅ | `internal/snmp/snmp.go` |
| OID BER encoding, big- + little-endian state parse | ✅ | `internal/snmp/snmp.go:54-167` |
| AppList announcement (S2PC_AppList) | ✅ | `internal/httpclient/http.go:209-234` |
| GetUserSelect.xml parse | ✅ | `internal/httpclient/http.go:236-263` |
| Unquoted multipart boundary quirk | ✅ | `internal/httpclient/http.go` (no `FormDataContentType`) |
| TCP probe connection + handshake | ✅ | `internal/tcp/tcp.go:181-228` |
| REQUEST → PARAMS → EXTRA → DIMS → READY setup | ✅ | `internal/tcp/tcp.go:246-288` |
| POLL/FETCH — normal mode (page 1, WIN firmware) | ✅ | `internal/tcp/tcp.go:304-401` |
| POLL/FETCH — streaming mode (page 2+, Mac firmware) | ✅ | `internal/tcp/tcp.go:345-379` |
| Next-page check loop (status `0x04` terminate) | ✅ | `internal/tcp/tcp.go:425-462` |
| END (`0x06`) + DISC (`0x17`) teardown | ✅ | `internal/tcp/tcp.go:465-477` |
| JPEG strip reassembly (multi-SOI compositing) | ✅ | `internal/imageutil/assemble.go:46-84` |
| Multi-page PDF (go-pdf/fpdf, A4 fit) | ✅ | `internal/imageutil/assemble.go:86-122` |
| Format routing (PDF vs JPEG) + timestamped filename | ✅ | `cmd/samsung-scan/main.go:252-265` |
| Active-console-user gating (`/dev/console` owner) | ✅ | `cmd/samsung-scan/main.go:309-318` |
| Network guard (ARP MAC verify, macOS-only, graceful no-op) | ✅ | `cmd/samsung-scan/main.go:94-147` |
| Signal handling + graceful deregister on shutdown | ✅ | `cmd/samsung-scan/main.go` |

### CLI flags
All six flags are both documented and wired: `--ip` (required), `--output`,
`--poll`, `--cleanup`, `--log-level`, `--enable-network-guard`. No documented
flag is unimplemented, and no implemented flag is undocumented.

## Test coverage

| Package | Tests | Notes |
|---------|------:|-------|
| `cmd/samsung-scan` | 10 | full state-machine flow via injected fakes, format routing, cleanup, multi-page PDF |
| `internal/httpclient` | 12 | all 5 exported fns; validates boundary quirk, User-Agent, MAC type, no-response tolerance |
| `internal/snmp` | 9 | state parse (both endiannesses), OID multi-byte arc encoding, timeout→idle |
| `internal/imageutil` | 8 | strip assembly, SOI detection, single/multi-page PDF, allocation caps |
| `internal/tcp` | 14 | full `ProtocolServer` mock: handshake, chunking, multi-page ADF, strip-size cap |

100% of exported functions are covered; private helpers are exercised indirectly.
Total = 52 (matches `go test` count above).

## Build, deploy & tooling

- **Makefile**: `build-mac`, `build-linux` (static AMD64), `test`, `lint`, `docker`, `clean` — all present and working.
- **Dockerfile**: multi-stage → `scratch`, `CGO_ENABLED=0`, non-root `nobody` UID.
- **install.sh**: ARP-based printer MAC discovery + LaunchAgent plist templating.
- **launchd/com.local.samsung-scan.plist**: per-user Aqua LaunchAgent (not root), KeepAlive, logs to `~/Library/Logs`.
- **Dependencies**: single direct dep, `github.com/go-pdf/fpdf`.

## Security findings — all remediated in current tree

`SECURITY_AUDIT.md` (2 High / 2 Medium / 4 Low) was written against a
pre-remediation state. Verified against current code, every actionable finding is
addressed:

| ID | Finding | Status in current code |
|----|---------|------------------------|
| H1 | Stack ran as root | ✅ per-user Aqua LaunchAgent (`plist:8`) |
| H2 | Log in world-writable `/tmp` | ✅ now `~/Library/Logs/samsung-scan.log` (`plist:23`) |
| M2 | Console check shelled out to `stat` via PATH | ✅ in-process `os.Stat("/dev/console")` (`main.go:312`) |
| M3 | Root TOCTOU write into user dir | ✅ eliminated by per-user agent |
| L1 | Unbounded allocation from printer data | ✅ caps in `tcp.go:91-95`, `imageutil` |
| L2 | XML via `fmt.Sprintf`, no escaping | ✅ now `xml.Marshal` (`http.go:163,190,220`) |
| L3 | `*.log` not gitignored | ✅ `*.log` in `.gitignore`, no stray log in tree |
| L4 | Plaintext/unauthenticated protocols | Accepted by design (trusted-LAN model) |

## Gaps & caveats (the honest part)

1. **No CI** — no `.github/workflows`. Tests/builds/vet pass but are not enforced
   on change. This is the single most impactful missing piece.
2. **Platform scope** — deployment is macOS (LaunchAgent) + Docker only; no
   systemd/init units. By design, but limits Linux use to containers.
3. **Platform-specific no-ops** — network guard and active-user gating are
   macOS-only and silently disabled on Linux/Docker. Intentional; documented in
   `CLAUDE.md`. Expect the daemon to register unconditionally on Linux.
4. **Hardcoded A4 dimensions** in `tcp.go` — by design (the printer LCD selects
   size and the daemon downloads what arrives), but worth knowing.
5. **Lossy reassembly** — multi-strip JPEGs are re-encoded at Q95, not lossless.
   Acceptable for scan output.

## Recommended next steps

1. **Add CI** (GitHub Actions): run `make test` + `make lint` on push/PR, and
   optionally `make build-mac` / `build-linux` to keep the green state enforced.
2. **Reconcile the audit docs** — `SECURITY_AUDIT.md` / `REMEDIATION_PLAN.md`
   read as open work but the fixes have landed; mark findings resolved (or add a
   status column) so the docs match the code.
3. Consider a `go test -race` lane and basic coverage reporting in CI.
4. (Optional) systemd unit if non-container Linux deployment is ever desired.
