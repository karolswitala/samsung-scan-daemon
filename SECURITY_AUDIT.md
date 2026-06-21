# Security Audit — samsung-scan

**Date:** 2026-06-21
**Target:** `samsung-scan` — Go daemon implementing the reverse-engineered
Samsung M2070W Scan-to-PC protocol (~2,700 LOC, Go 1.26).
**Method:** Manual source review of all Go packages and deployment artifacts (see
*Methodology*). No dynamic testing or fuzzing.

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

The audit therefore focuses on **local privilege and file-handling** exposure,
which is the realistic attack surface for a daemon that is currently designed to
run as **root** (system `LaunchDaemon`) on a potentially multi-user Mac. Where a
finding would escalate to high severity on an untrusted/shared network, that is
noted (see the Appendix for the public-WiFi scenario).

### Deployment requirements (constrain the remediations)

The recommendations below are written against these stated product requirements,
so that a reader can evaluate them without further context:

1. **Single-user Mac is the primary target**, but **fast user switching must work**:
   when more than one user is logged in simultaneously, the scan must be delivered
   to the **active (foreground) console user only**, and the printer's LCD must
   show a **single** scan target ("My Mac").
2. **Headless / not-logged-in delivery is *not* required.** The daemon only needs
   to deliver scans while a user is logged in at the GUI.
3. Delivered files must be owned by the receiving user and manageable in Finder
   without an authentication prompt.

## Executive summary

| Severity | Count | IDs |
|----------|-------|-----|
| High | 2 | H1, H2 |
| Medium | 2 | M2, M3 |
| Low | 4 | L1, L2, L3, L4 |
| Informational | 3 | I1, I2, I3 |

**Top two actions:**

1. **H1 — Stop running the untrusted-input parsing stack as root.** Today the
   JPEG decoder, the third-party PDF encoder, and the binary protocol parser all
   execute as uid 0. Given requirement (2) above, the recommended fix is to ship
   a **per-user `LaunchAgent`** rather than a root `LaunchDaemon`. This single
   change also resolves **M2** and **M3** and enables a clean **H2** fix.
2. **H2 — Move the daemon log off `/tmp`.** `/tmp/samsung-scan.log` is a
   world-writable, predictable path that a root writer opens for append — a
   classic local symlink-redirection / file-tampering primitive.

Overall the code is defensively written for protocol parsing (fixed-size reads
via `io.ReadFull`, bounded SNMP scanning, uint16-bounded chunk sizes — see I3).
The meaningful risk comes from the **deployment posture (root) and log
placement**, not from the wire parsing.

## Findings

| ID | Title | Severity | Location |
|----|-------|----------|----------|
| H1 | Network/image/PDF stack runs as root, no privilege drop | High | `launchd/com.local.samsung-scan.plist`, `internal/imageutil/assemble.go:49,79` |
| H2 | Root log path in world-writable `/tmp` (symlink redirection) | High | `launchd/com.local.samsung-scan.plist` |
| M2 | Console-user detection shells out to `stat` via PATH | Medium | `cmd/samsung-scan/main.go:261` |
| M3 | Root write into user-owned directory with TOCTOU window | Medium | `cmd/samsung-scan/main.go:208-218` |
| L1 | Unbounded allocation from printer-supplied data (DoS) | Low | `internal/tcp/tcp.go:143,350,387`; `internal/imageutil/assemble.go:54,58` |
| L2 | XML built with `fmt.Sprintf`, no escaping | Low | `internal/httpclient/http.go:130,156,181` |
| L3 | `*.log` not gitignored; real log present in tree | Low | `.gitignore`, `log_1633_20062026.log` |
| L4 | Plaintext, unauthenticated protocols | Low (accepted) | `internal/snmp/snmp.go:29` |
| I1 | Docker image runs as root, no `USER` | Info | `Dockerfile` |
| I2 | `uniqueID` uses MD5 (non-crypto identifier) | Info | `cmd/samsung-scan/main.go:68` |
| I3 | Positive controls observed | Info | multiple |

A step-by-step implementation of the recommendations is in `REMEDIATION_PLAN.md`.

---

### H1 — Full network/image/PDF stack runs as root with no privilege drop

**Severity:** High

The daemon is designed to run as a system `LaunchDaemon`, which executes as
**root** — `launchd/com.local.samsung-scan.plist` has no `UserName` key, and the
package documentation (`cmd/samsung-scan/main.go:18-21`) describes the root
multi-user model explicitly ("a single system daemon (running as root) to serve
multiple user accounts").

Everything that processes externally-supplied bytes runs in that root process:

- the binary protocol parser in `internal/tcp/tcp.go`,
- `image/jpeg` decoding of every scan strip — `imageutil/assemble.go:49` and
  `:79`,
- the third-party PDF encoder `github.com/go-pdf/fpdf` — `imageutil/assemble.go:75-108`,
  which consumes the (image-derived) page data.

**Impact.** A memory-safety or parsing defect in the standard-library JPEG
decoder, in `fpdf`, or in the hand-written protocol code becomes a **root-level**
compromise rather than a user-level one. The blast radius of any such bug is
maximized by the deployment posture.

**Recommended remediation — per-user `LaunchAgent`.** Because headless delivery is
not required (deployment requirement 2), the daemon does not need to be a system
service. Ship it as a per-user `LaunchAgent` instead of a root `LaunchDaemon`.
launchd then runs one instance inside each logged-in user's GUI session, **as that
user**:

- The parsing stack (TCP parser, `image/jpeg`, `fpdf`) never runs as uid 0 —
  H1 resolved.
- `activeUserDesktop()` reduces to `os.UserHomeDir()`, and `os.WriteFile` lands in
  the user's own Desktop already correctly owned — the `Stat`+`Chown` block and its
  TOCTOU window (M3) are deleted.
- Set `LimitLoadToSessionType = Aqua` so the agent only runs while a user is at the
  console (this also limits unattended exposure on untrusted networks — see the
  Appendix).

**Fast user switching (deployment requirement 1).** launchd runs one agent per
logged-in user, so each agent must register and serve **only while it is the
active console user**. This keeps a single "My Mac" target on the printer LCD that
always routes the scan to the foreground user. Detect active-ness with a non-root,
in-process self-check — compare the owner of `/dev/console` to `os.Getuid()` via
`os.Stat` — and keep a **shared** printer identity (`userID`/`uniqueID`) across all
users so exactly one slot exists. (See M2 for why this check does not reintroduce
that finding, and `REMEDIATION_PLAN.md` Phase 1 for the loop logic.)

**Alternatives** (only relevant if a single shared root daemon is ever
reintroduced, e.g. to gain headless delivery): a small privileged helper that
performs **only** the `chown` while the parsing stack runs unprivileged, or an
in-process privilege drop (`setuid`/`setgid`) after setup. Both are weaker and more
complex than the LaunchAgent model.

**Independent of the privilege model:** treat `github.com/go-pdf/fpdf` as a
third-party dependency processing attacker-influenced input; pin and track it for
advisories (it is the only non-stdlib dependency — see `go.mod`).

---

### H2 — Log path `/tmp/samsung-scan.log` is a world-writable, predictable, symlink-followable target for a root writer

**Severity:** High

`launchd/com.local.samsung-scan.plist` sets both `StandardOutPath` and
`StandardErrorPath` to `/tmp/samsung-scan.log`. `install.sh:23` documents the
same path. `/tmp` is world-writable and shared across all local users.

**Impact.**
- *Symlink redirection / file tampering.* A local unprivileged user can
  pre-create `/tmp/samsung-scan.log` as a **symlink** pointing at an arbitrary
  file. When launchd opens the path for append as root, daemon output is appended
  to the attacker-chosen target. This is a well-known local privilege-relevant
  primitive for predictable root-writable paths in shared directories.
- *Information disclosure.* The log is world-readable and records scan file
  paths (`scan saved path=/Users/<user>/Desktop/...`), the printer `instanceID`,
  and state transitions — readable by any local user.

**Recommendation.** With the H1 LaunchAgent migration the daemon is no longer root,
which removes the symlink-redirection primitive entirely; point `StandardOutPath` /
`StandardErrorPath` at the user's own `~/Library/Logs/samsung-scan.log` (created and
owned by that user, not in a shared directory). For a root daemon instead, write to
a root-owned directory with safe permissions, e.g. `/var/log/samsung-scan/` (`0750`,
files `0640`) — never `/tmp`; if `/tmp` is unavoidable, create the file with
`O_CREAT|O_EXCL|O_NOFOLLOW` rather than letting launchd open a pre-existing path.

---

### M2 — Console-user detection shells out to `stat` via a PATH lookup

**Severity:** Medium

`cmd/samsung-scan/main.go:261`:

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

**Recommendation.** The H1 migration removes the root, `$PATH`-resolved shell-out.
A console-user check is still required — to gate which per-user agent serves under
fast user switching (H1) — but it becomes a **non-root, in-process
self-comparison**: `os.Stat("/dev/console")`, then compare the owner Uid to
`os.Getuid()`. No subprocess, no `$PATH`, no privilege, so this finding does not
reapply. (If a shell-out is ever retained, use the absolute path `/usr/bin/stat`.)

---

### M3 — Root writes into a user-controlled directory with a TOCTOU window

**Severity:** Medium

When `--output` is omitted, `activeUserDesktop()` (`main.go:274-286`) resolves the
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

**Recommendation.** Eliminated by the H1 LaunchAgent migration: a per-user agent
writes to its own Desktop as itself, so there is no cross-user write and no `chown`
step. If a root writer is ever reintroduced, validate that the resolved output path
is a real directory owned by the expected user before writing; open with
`O_NOFOLLOW`/`openat`-style safety relative to a verified directory fd; avoid
re-stat-then-chown races by deriving the ownership target from the same verified
handle.

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
printer's IP) can drive memory consumption arbitrarily and OOM-kill the daemon.
Mitigated in practice by the 30 s / 2 s network deadlines (`tcp.go:89,288,360`),
which bound how long data can stream in.

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

### Operational note — untrusted networks (e.g. public WiFi)

The daemon is an **outbound-only client** — there is no production listener (the
only `net.Listen` / `ListenPacket` calls are in `_test.go`). It therefore cannot
be attacked as a server. The realistic exposure when it runs off the trusted home
LAN is **printer impersonation by IP collision**: the configured `--ip` is an
RFC1918 address, and public networks hand out addresses from the same ranges, so
another device may legitimately hold that IP. With no authentication of the printer
(**L4**), whatever answers at `--ip` *is* "the printer" as far as the daemon is
concerned — it then feeds the parsing stack (**L1**, and, while running as root,
**H1**) and writes attacker-controlled bytes to the Desktop (**M3**).

Mitigations:
- The H1 LaunchAgent model means the daemon only runs while a user is logged in,
  not unattended.
- Do not run the daemon on untrusted/multi-tenant networks.
- Optionally add a startup guard that only proceeds when the expected home network
  is present (gateway IP, Wi-Fi SSID, or the printer's MAC via ARP).

### Methodology

Manual source review of all Go packages and deployment artifacts listed in
*Scope*. Focus areas: handling of untrusted network input (length/size parsing,
buffer allocation, slice indexing, XML/XXE), privilege level of the process,
file/log creation paths and permissions, and command execution. No dynamic
testing or fuzzing was performed; findings are from static reading of the code at
the current tree state.

### Remediation priority

1. **H1 — migrate to a per-user LaunchAgent** (headless delivery not required).
   This single change also resolves **M2** and **M3** and lets **H2** use a
   user-owned log path.
2. **H2** — relocate logs to `~/Library/Logs/` (LaunchAgent) or, for a root daemon,
   off `/tmp` with safe permissions.
3. **L1** — add allocation caps (matters most on untrusted networks).
4. **L3** — gitignore `*.log` and remove the stray log file.
5. **L2, L4, I1** — hardening / documentation as convenient.

See `REMEDIATION_PLAN.md` for the step-by-step implementation.
