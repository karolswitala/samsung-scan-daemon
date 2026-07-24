# Security Audit — samsung-scan

**Date:** 2026-06-21 (updated 2026-07-24 for the per-user LaunchAgent model)
**Target:** `samsung-scan` — Go daemon implementing the reverse-engineered
Samsung M2070W Scan-to-PC protocol (~2,700 LOC, Go 1.26).
**Auditor:** Claude (Opus 4.8), source review.

> **2026-07-24 update.** The deployment model is now a **per-user LaunchAgent**
> (`~/Library/LaunchAgents`, running as the logged-in user), not a root system
> `LaunchDaemon`. The daemon no longer runs as root, no longer detects the console
> user, and no longer `chown`s output files. This voids the root-based findings:
> **H1** downgrades to Low, **H2** is resolved (log moved to
> `~/Library/Logs/samsung-scan.log`), and **M2**/**M3** are eliminated. Sections
> below reflect the update.

## Scope

| Component | File | Concern |
|-----------|------|---------|
| Daemon loop, console-user detection, file write | `cmd/samsung-scan/main.go` | privilege, file handling |
| Binary download protocol (untrusted wire data) | `internal/tcp/tcp.go` | parsing, allocation |
| HTTP register / AppList / UserSelect (XML) | `internal/httpclient/http.go` | XML injection, XXE |
| SNMPv1 poller / parser | `internal/snmp/snmp.go` | parsing bounds |
| JPEG strip reassembly, PDF generation | `internal/imageutil/assemble.go` | image-bomb, third-party PDF lib |
| Deployment | `launchd/com.local.samsung-scan.plist`, `install.sh`, `Dockerfile`, `.gitignore` | privilege, log handling, hygiene |

## Threat model

The audit assumes a **trusted home LAN**: the printer and the network are taken
to be genuine. Under that assumption, attacks that require impersonating the
printer on the network (rogue device, ARP spoofing, feeding crafted protocol
data) are documented as **low / accepted** risk.

The audit therefore focuses on **local privilege and file-handling** exposure.
Under the current **per-user LaunchAgent** model the daemon runs as the logged-in
user (not root); each user's own agent writes only into that user's home. Where a
finding would escalate to high severity on an untrusted/shared network, that is
noted.

## Executive summary

| Severity | Count | IDs |
|----------|-------|-----|
| High | 0 | — |
| Medium | 0 | — |
| Low | 5 | H1, L1, L2, L3, L4 |
| Informational | 6 | H2, M2, M3, I1, I2, I3 (H2/M2/M3 resolved — see notes) |

The original High/Medium findings (H1, H2, M2, M3) all stemmed from the **root
system-daemon** posture. Under the per-user LaunchAgent model that posture is gone:
**H1** is now Low (user-privilege parsing), and **H2/M2/M3** are resolved or
eliminated by the migration (see each finding).

**Remaining focus:** the code is defensively written for protocol parsing
(fixed-size reads via `io.ReadFull`, bounded SNMP scanning, uint16-bounded chunk
sizes — see I3). The residual items are Low: allocation caps (L1), XML-escaping
hygiene (L2), gitignoring logs (L3), and the accepted plaintext-LAN trust
assumption (L4).

## Findings

| ID | Title | Severity | Location |
|----|-------|----------|----------|
| H1 | Network/image/PDF parsing stack (now at user privilege) | Low (was High) | `launchd/com.local.samsung-scan.plist`, `internal/imageutil/assemble.go:49,79` |
| H2 | Log path in world-writable `/tmp` — **resolved** (moved to `~/Library/Logs`) | Info (was High) | `launchd/com.local.samsung-scan.plist` |
| M2 | Console-user detection via `stat`/PATH — **eliminated** (code removed) | Info (was Medium) | `cmd/samsung-scan/main.go` |
| M3 | Root write into user dir with TOCTOU — **eliminated** (no root, no chown) | Info (was Medium) | `cmd/samsung-scan/main.go` |
| L1 | Unbounded allocation from printer-supplied data (DoS) | Low | `internal/tcp/tcp.go:143,350,387`; `internal/imageutil/assemble.go:54,58` |
| L2 | XML built with `fmt.Sprintf`, no escaping | Low | `internal/httpclient/http.go:130,156,181` |
| L3 | `*.log` not gitignored; real log present in tree | Low | `.gitignore`, `log_1633_20062026.log` |
| L4 | Plaintext, unauthenticated protocols | Low (accepted) | `internal/snmp/snmp.go:29` |
| I1 | Docker image runs as root, no `USER` | Info | `Dockerfile` |
| I2 | `uniqueID` uses MD5 (non-crypto identifier) | Info | `cmd/samsung-scan/main.go:68` |
| I3 | Positive controls observed | Info | multiple |

---

### H1 — Network/image/PDF parsing stack (now runs at user privilege)

**Severity:** Low (was High under the root system-daemon model)

The daemon now runs as a **per-user LaunchAgent** — `launchd/com.local.samsung-scan.plist`
is installed under `~/Library/LaunchAgents` and executes as the logged-in user, not
root. The package documentation describes this per-user model.

Everything that processes externally-supplied bytes still runs in that process:

- the binary protocol parser in `internal/tcp/tcp.go`,
- `image/jpeg` decoding of every scan strip — `imageutil/assemble.go:49` and
  `:79`,
- the third-party PDF encoder `github.com/go-pdf/fpdf` — `imageutil/assemble.go:75-108`,
  which consumes the (image-derived) page data.

**Impact.** A memory-safety or parsing defect in the standard-library JPEG
decoder, in `fpdf`, or in the hand-written protocol code now executes at **user
privilege** — the same privilege as the user who initiated the scan and owns the
output directory. The root blast radius that made this High is gone; residual risk
is a user-session compromise from a malicious/malfunctioning device on the trusted
LAN (bounded further by the network deadlines in L1).

**Recommendation.**
- Treat `github.com/go-pdf/fpdf` as a third-party dependency processing
  attacker-influenced input; pin and track it for advisories (it is the only
  non-stdlib dependency — see `go.mod`).
- Keep the allocation caps in L1 as the meaningful hardening for this stack.
- Do not reintroduce a root system-daemon deployment without also isolating this
  parsing stack.

---

### H2 — Log path in world-writable `/tmp` — **resolved**

**Severity:** Info (was High under the root system-daemon model)

**Original finding.** The plist pointed `StandardOutPath` / `StandardErrorPath` at
`/tmp/samsung-scan.log`. Because the daemon then ran as **root**, a local user
could pre-create that predictable, world-writable path as a **symlink** to redirect
root's append output to an arbitrary file (a classic local privilege-relevant
primitive), and the world-readable log disclosed scan file paths, `instanceID`, and
state transitions.

**Resolution.** Two changes remove the vector:
- The log moved to per-user `~/Library/Logs/samsung-scan.log`
  (`launchd/com.local.samsung-scan.plist`, substituted from `__HOME__` by
  `install.sh`). This is inside the user's own home — not world-writable, not a
  shared predictable path.
- The daemon no longer runs as root (per-user LaunchAgent), so there is no
  privileged writer to redirect. Each user's log is written and read as that user,
  so the cross-user information-disclosure concern is also gone.

No residual action required.

---

### M2 — Console-user detection shells out to `stat` via a PATH lookup

**Severity:** Info — **eliminated** (was Medium)

**Resolution.** The per-user LaunchAgent runs as the target user, so there is no
console-user detection: `consoleUser()` and its `exec.Command("stat", …)` call were
removed from `cmd/samsung-scan/main.go`. The daemon writes to its own
`~/Desktop`. The finding no longer applies. Original write-up retained below.

`cmd/samsung-scan/main.go:261` (removed):

```go
out, err := exec.Command("stat", "-f", "%Su", "/dev/console").Output()
```

`stat` is invoked by bare name, resolved through `$PATH` of the root daemon
process.

**Impact.** Executing a bare binary name from a root process depends on the
inherited `PATH`. In the current launchd deployment the inherited `PATH`
(`/usr/bin:/bin:/usr/sbin:/sbin`) is not user-writable, so this is **not
presently exploitable** — but it is a fragile pattern for a root process and a
single misconfiguration (or a future non-launchd launch context) reintroduces
risk.

**Recommendation.** Use the absolute path `/usr/bin/stat`. Defense-in-depth, zero
behavioural change.

---

### M3 — Root writes into a user-controlled directory with a TOCTOU window

**Severity:** Info — **eliminated** (was Medium)

**Resolution.** Under the per-user LaunchAgent the daemon runs as the user and
writes to that user's own `~/Desktop`; the output files are created already owned by
the user, so the post-write `os.Stat`/`os.Chown` step was removed and
`activeUserDesktop()` no longer exists. There is no root writer and no
resolve→write→chown race. The finding no longer applies. Original write-up retained
below.

When `--output` is omitted, `activeUserDesktop()` (removed) resolved the
console user and returns `/Users/<user>/Desktop`. `downloadAndSave`
(`main.go:208-218`) then, as root:

```go
os.WriteFile(path, data, 0644)          // path under the user's Desktop
...
if info, err := os.Stat(output); ... {  // re-stat the directory
    os.Chown(path, int(stat.Uid), int(stat.Gid))
}
```

The destination tree is owned by an unprivileged user, and there is a
time-of-check/time-of-use gap between resolving the console user, writing the
file, and re-stat-ing the directory to choose the `chown` target.

**Impact.** A root process writing and `chown`-ing into a user-owned directory
warrants care: between detection and write the active console user can change
(fast user switching), or a path component could be swapped for a symlink,
causing the root write to land — or the ownership to be applied — somewhere
unintended. Practical risk on a single-user Mac is low, but the daemon explicitly
advertises the multi-user mode where this matters.

**Recommendation.** Validate that the resolved output path is a real directory
owned by the expected user before writing; open with `O_NOFOLLOW`/`openat`-style
safety relative to a verified directory fd; avoid re-stat-then-chown races by
deriving the ownership target from the same verified handle.

---

### L1 — Unbounded memory allocation from printer-supplied data (DoS)

**Severity:** Low (would be Medium/High on an untrusted network)

No cap bounds the volume of data accepted from the printer:

- the download loop appends strips and concatenates pages with no limit on strip
  count, page count, or total bytes — `tcp.go:350`, `:371`, `:387-391`;
- `readJPEGStrip` (`tcp.go:143-162`) appends bytes until it sees an EOI marker
  (`0xff 0xd9`), with no maximum strip size;
- `imageutil.AssembleStrips` allocates an RGBA canvas sized by the **sum of the
  decoded strip heights** — `assemble.go:54` (`totalH += img.Bounds().Dy()`) and
  `:58` (`image.NewRGBA(image.Rect(0, 0, width, totalH))`). Both width and height
  derive from the decoded JPEG headers, i.e. from printer-supplied data.

**Impact.** A malfunctioning or malicious printer (or anything answering on the
printer's IP) can drive memory consumption arbitrarily and OOM-kill the daemon —
which, per H1, runs as root. Mitigated in practice by the 30 s / 2 s network
deadlines (`tcp.go:89,288,360`), which bound how long data can stream in.

**Recommendation.** Impose sane upper bounds: a maximum total scan size, a
maximum per-strip size in `readJPEGStrip`, a maximum strip/page count, and a
maximum decoded canvas dimension before allocating in `AssembleStrips`.

---

### L2 — XML built with `fmt.Sprintf`, no escaping

**Severity:** Low

`Register`, `Deregister`, and `PostAppList` (`http.go:130-133`, `:156-159`,
`:181-193`) build request XML by string-formatting values directly into the
document, with no XML escaping.

**Impact.** Currently **safe**: every interpolated value is either a hardcoded
constant (`userID = "My Mac"`, the `Profile` fields from `defaultProfile`) or the
MD5-hex `uniqueID` (`main.go:66-70`) — none are externally controlled, and all
are within the XML character set. The finding is recorded as **fragile design**:
if any user- or printer-derived string were ever interpolated here, it would
become an XML-injection vector.

**Positive note (no XXE).** Response parsing uses `encoding/xml.Unmarshal`
(`http.go:145,169,225`). Go's `encoding/xml` does not resolve external entities
or fetch DTDs, so the XML responses from the printer carry **no XXE exposure** —
a meaningful positive for a parser fed by the network.

**Recommendation.** If the XML payloads ever gain dynamic input, switch to
`encoding/xml` marshalling or `xml.EscapeText` rather than `fmt.Sprintf`.

---

### L3 — `*.log` not gitignored; a real log is sitting in the working tree

**Severity:** Low

`.gitignore` excludes `/samsung-scan`, `dist/`, and `*.exe`, but **not** `*.log`.
The working tree currently contains `log_1633_20062026.log` (~61 KB, untracked).

**Impact.** The operational log risks being committed. Daemon logs can contain
scan file paths, usernames, and printer internals, which would then be disclosed
in the git history.

**Recommendation.** Add `*.log` to `.gitignore` and keep operational logs out of
the repository. (Sample inspected: this particular log contains poll/state lines
and `instanceID`, not IPs or usernames — but log content is configuration- and
verbosity-dependent, so it should not be tracked.)

---

### L4 — Plaintext, unauthenticated protocols (accepted under trusted-LAN model)

**Severity:** Low (accepted)

- SNMP uses the default community string `"public"` (`snmp.go:29`).
- HTTP (port 80) and the binary channel (port 9400) are plaintext with no
  authentication of the printer.

**Impact.** On an untrusted or shared network, any device could impersonate the
printer and feed the parsers covered above (see L1), or read/replay the
registration traffic. Under the **trusted home LAN** model adopted for this
audit, this is an **accepted risk** inherent to the reverse-engineered protocol —
the printer firmware dictates these choices and they cannot be unilaterally
hardened by the client.

**Recommendation.** Document the trust assumption for operators. Do not deploy on
untrusted/multi-tenant networks. No code change expected.

---

### Informational

- **I1 — Docker image runs as root.** `Dockerfile` builds `FROM scratch` with no
  `USER` directive, so the container runs as uid 0. Minor, given the image holds
  only the static binary, but add a non-root `USER` for defense-in-depth if the
  container path is used.
- **I2 — `uniqueID` uses MD5.** `main.go:68` hashes the hostname with MD5 to
  produce a printer-facing identifier. This is **not** a security control and has
  no cryptographic dependency, so MD5 is acceptable here; recorded to preempt
  false-positive "weak hash" scanner hits.
- **I3 — Positive controls observed.**
  - All protocol status reads use `recvExact` → `io.ReadFull` into fixed-size
    buffers (`tcp.go:129-133`), so every `status[...]` index (`status[2]`,
    `status[3]`, `status[6:8]`) is panic-safe — the 32-byte length is guaranteed
    before indexing.
  - The SNMP response scanner is correctly bounded: the loop condition
    `i < len(data)-5` ensures `data[i+2:i+6]` never exceeds the buffer
    (`snmp.go:121-128`), and the read buffer is a fixed 1024 bytes.
  - Per-chunk size is `uint16`, so a single non-streaming FETCH is bounded to
    ≤ 64 KB (`tcp.go:331`).
  - Network operations consistently set deadlines (`tcp.go:214,227,288,360`;
    `snmp.go:141`), preventing indefinite hangs.

## Appendix

### Accepted risks (trusted-LAN model)
- **L4** — plaintext + default SNMP community: inherent to the printer protocol;
  acceptable only on a trusted network.
- **L1** (DoS via crafted data): downgraded to Low because it requires a
  malicious/malfunctioning device on the trusted LAN; would be elevated on an
  untrusted network.

### Methodology
Manual source review of all Go packages and deployment artifacts listed in
*Scope*. Focus areas: handling of untrusted network input (length/size parsing,
buffer allocation, slice indexing, XML/XXE), privilege level of the process,
file/log creation paths and permissions, and command execution. No dynamic
testing or fuzzing was performed; findings are from static reading of the code at
the current tree state.

### Remediation priority
Post per-user migration, the High/Medium items are resolved:
- ~~H1 — drop root / isolate parsing~~ — daemon runs at user privilege (now Low).
- ~~H2 — relocate logs off `/tmp`~~ — moved to `~/Library/Logs/samsung-scan.log`.
- ~~M2, M3 — harden root command-exec / file-write paths~~ — code removed.

Remaining, in priority order:
1. L1 — add allocation caps.
2. L3 — gitignore `*.log`.
3. L2, L4, I1 — hardening / documentation as convenient.
