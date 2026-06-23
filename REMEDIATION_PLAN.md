# Remediation Plan — samsung-scan

Step-by-step implementation of the recommendations in `SECURITY_AUDIT.md`. Read
that document first for the findings and their rationale; this plan covers *how*
to fix them.

## Context and requirements

These fixes are written against the project's deployment requirements:

1. **Single-user Mac is the primary target, but fast user switching must work.**
   When more than one user is logged in at once, the scan must go to the **active
   (foreground) console user only**, and the printer LCD must show a **single**
   scan target ("My Mac").
2. **Headless / not-logged-in delivery is *not* required.** Scans are only
   delivered while a user is logged in at the GUI.
3. Delivered files must be owned by the receiving user and openable in Finder with
   no authentication prompt.

The anchor change is migrating from a root `LaunchDaemon` to a **per-user
`LaunchAgent`**, which, combined with active-console-user gating, satisfies all
three requirements and by construction also resolves findings M2 and M3 and the
worst of H2.

Each phase is independently shippable. Ordering follows the audit's remediation
priority.

| Phase | Findings closed | Risk if skipped |
|-------|-----------------|-----------------|
| 1. LaunchAgent migration + active-user gating | H1, M2, M3 (+ enables H2) | parsing stack stays root; wrong user receives scan |
| 2. Log path | H2 | symlink redirection / info disclosure |
| 3. Allocation caps | L1 | OOM from a hostile responder |
| 4. Hygiene | L3 | log leakage into version control |
| 5. Optional hardening | L2, L4, I1, network guard | defense-in-depth |

---

## Phase 1 ✅ — Per-user LaunchAgent with active-console-user gating (H1, M2, M3)

**Goal:** the daemon runs **as the logged-in user, inside their GUI session**, never
as root; and when several users are logged in, only the foreground user's instance
registers with the printer and receives the scan.

### 1a. Deployment artifacts

**`launchd/com.local.samsung-scan.plist`**
- Add `LimitLoadToSessionType` = `Aqua` (only loads in a GUI login session).
- Point `StandardOutPath` / `StandardErrorPath` at the user's own log (see Phase 2).
- Keep `RunAtLoad`, `KeepAlive`, `ThrottleInterval`.
- The plist no longer belongs in `/Library/LaunchDaemons` (root). Install it to
  `~/Library/LaunchAgents/` (this user) or `/Library/LaunchAgents/` (all users —
  still runs per-user, not root).

**`install.sh`**
- Change `PLIST_DEST` to `"$HOME/Library/LaunchAgents/com.local.samsung-scan.plist"`.
- Load with the per-user form, without `sudo`:
  ```bash
  launchctl bootstrap gui/$(id -u) "$PLIST_DEST"               # load
  launchctl bootout   gui/$(id -u)/com.local.samsung-scan      # unload
  ```
- Copying the binary to `/usr/local/bin` still needs one `sudo cp` (or relocate it
  to a user-writable directory such as `~/.local/bin`). The binary's location does
  not affect privilege — only the uid launchd runs it under does.
- Update the printed "to stop / view logs" hints (new log path and `bootout`
  command).

**Migrating an already-installed root daemon.** Document these steps in `install.sh`
output (or a README) so existing installs are cleaned up:
```bash
sudo launchctl unload /Library/LaunchDaemons/com.local.samsung-scan.plist
sudo rm /Library/LaunchDaemons/com.local.samsung-scan.plist
# then run ./install.sh as the normal (non-root) user
```

### 1b. Code changes — `cmd/samsung-scan/main.go`

- **`activeUserDesktop()`**: the process now *is* the target user, so replace the
  `consoleUser()` → `user.Lookup()` path with `os.UserHomeDir()`:
  ```go
  home, err := os.UserHomeDir()
  // ... fallback handling ...
  return filepath.Join(home, "Desktop")
  ```
- **`downloadAndSave()`**: delete the `os.Stat(output)` + `os.Chown(...)` block
  (lines ~208-218). The file is written by the user into the user's own Desktop,
  already correctly owned — this closes **M3** (no cross-user write, no TOCTOU).
- **Replace `consoleUser()`** (the `exec.Command("stat", …)` shell-out) with the
  non-root, in-process active-user self-check described in 1c — closing **M2**.
- **Imports**: drop the now-unused `os/exec` and `os/user`. Keep `syscall` (used by
  the `Stat_t` console-owner read and by the existing `SIGINT`/`SIGTERM` handling).
- Update the package doc comment (`main.go:18-21`) that describes the "single root
  daemon serving multiple users" model — it no longer applies.

### 1c. Active-console-user gating (requirement 1)

Under fast user switching, launchd runs **one agent per logged-in user at the same
time**. To deliver only to the active user and show a single LCD target, each agent
serves only while it owns the console, and all agents share one printer identity so
there is exactly one printer slot.

- **Active-user self-check** (replaces `consoleUser()`; no subprocess):
  ```go
  func isActiveConsoleUser() bool {
      info, err := os.Stat("/dev/console")
      if err != nil {
          return false
      }
      st, ok := info.Sys().(*syscall.Stat_t)
      return ok && int(st.Uid) == os.Getuid()
  }
  ```
  `/dev/console` is owned by the current foreground user and updates on fast user
  switching (the same property the old `stat -f %Su /dev/console` relied on). Here
  it is a privilege-free comparison against the agent's *own* uid, so finding M2
  does not reapply.

- **Shared printer identity.** Keep `userID = "My Mac"` and the existing
  hostname-derived `uniqueID()` for every user. An identical `UniqueID` means a
  single registration slot on the printer, hence one LCD target. Do **not** make
  the identity per-user: that would surface two simultaneous targets and could
  route a scan to a background user — the opposite of requirement 1.

- **Gate the main loop on active-ness**, re-checking each poll tick:
  - *inactive and not registered* → stay idle; only re-evaluate `isActiveConsoleUser()`.
  - *becomes active* → `Register` (the existing DELETE-then-ADD pattern claims or
    refreshes the single slot) + `PostAppList`, then run the SNMP poll / fetch loop.
  - *becomes inactive* (user switched away) → `Deregister`, return to idle.
  - *on shutdown* → `Deregister` (existing behaviour).

- **Handoff race (documented, accepted).** During a switch there is a brief window
  where the outgoing agent may not have deregistered before the incoming one
  registers. Because both use the same `UniqueID`, the incoming `Register`
  (DELETE+ADD) cleanly reclaims the single slot. Worst case: a scan triggered
  mid-switch lands with whichever agent currently holds the registration. This is
  rare and acceptable — no cross-user file write can occur, since each agent only
  ever writes to its own Desktop as itself. (If this window must be closed, add an
  inter-agent lock; out of scope for this plan.)

### 1d. Verification
- `make build-mac && make test` — compiles, no unused-import errors.
- `grep -rn "Chown\|exec.Command\|\"os/exec\"\|\"os/user\"" cmd/ internal/` returns
  nothing (the `chown` and the subprocess are gone; `os.Stat("/dev/console")` is
  intentionally kept).
- After install, `ps -o user= -p "$(pgrep -f samsung-scan)"` shows the logged-in
  **user**, not `root`.
- Trigger a scan; the file lands on the Desktop owned by you (`ls -l`), with no
  Finder authentication prompt.
- **Fast-user-switching test:** log in as a second user and switch to them. Confirm
  (a) the printer LCD shows exactly **one** "My Mac" target; (b) a scan started
  while user B is active lands on **B's** Desktop; (c) switching back to A
  re-registers A, and a scan then lands on **A's** Desktop. The inactive user's
  agent logs show it idle / deregistered.

---

## Phase 2 ✅ — Move logs off `/tmp` (H2)

After Phase 1 the daemon is non-root, which already removes the symlink-redirection
primitive. Finish by writing to a user-owned, non-shared location.

- In the plist, set both log paths to the user's log directory. launchd plists
  require an **absolute** path (no `~`), so `install.sh` substitutes `$HOME` at
  install time:
  ```
  StandardOutPath / StandardErrorPath = /Users/<user>/Library/Logs/samsung-scan.log
  ```
  A simple approach: ship the plist with a `__HOME__` placeholder and have
  `install.sh` `sed`-substitute the real `$HOME` when copying it into place.
- Update the "view logs" hint in `install.sh` to the new path.
- Verify: `ls -l ~/Library/Logs/samsung-scan.log` is owned by you after install,
  and `grep -rn /tmp launchd/ install.sh` returns nothing.

---

## Phase 3 ✅ — Allocation caps against a hostile responder (L1)

These caps defend the parsing stack regardless of network trust (relevant to the
public-WiFi / IP-collision scenario in the audit Appendix). Choose limits well
above a real A4 @ 300 DPI colour scan (~25 MB raw) but far below memory-exhaustion
territory. Suggested starting values below; adjust to observed scan sizes.

- **`internal/tcp/tcp.go`**
  - `readJPEGStrip` (~line 143): cap accumulated bytes per strip
    (e.g. `maxStripBytes = 16 << 20`); return an error instead of growing the
    buffer without bound.
  - Download loop (~lines 350, 387): cap strip count, page count
    (`maxStrips`, `maxPages`), and total accumulated bytes
    (e.g. `maxTotalBytes = 200 << 20`).
- **`internal/imageutil/assemble.go`**
  - `AssembleStrips` (~lines 54, 58): before `image.NewRGBA`, reject the scan if
    `width` or `totalH` exceeds a maximum dimension, or if `width * totalH` exceeds
    a pixel budget (e.g. `maxCanvasPixels = 100_000_000`). These dimensions come
    from decoded JPEG headers, i.e. attacker-influenced data.

Each cap should return a clear error that aborts the scan cleanly. (The existing
network deadlines already bound *time*; these caps bound *volume*.)

**Verification:** add unit tests that feed oversized synthetic strips / headers and
assert the error path — the `internal/tcp` and `internal/imageutil` packages already
have `_test.go` harnesses to extend. `make test` stays green.

---

## Phase 4 ✅ — Hygiene (L3)

- Add `*.log` to `.gitignore`.
- Remove the stray `log_1633_20062026.log` from the working tree.
- Verify `git status` shows no `.log` files tracked or staged.

---

## Phase 5 (ongoing) — Optional hardening (L2, L4, I1, network guard)

Do these as convenient; none block the single-user deployment.

- **L2** — if the XML payloads ever gain dynamic (user- or printer-derived) input,
  switch `Register` / `Deregister` / `PostAppList` from `fmt.Sprintf` to
  `encoding/xml` marshalling or `xml.EscapeText`. Currently safe; this is
  future-proofing.
- **L4 / network guard** — add an optional startup check that only proceeds when the
  expected home network is present (gateway IP, Wi-Fi SSID, or the printer's MAC via
  ARP), so the agent no-ops on public WiFi instead of dialing `--ip` at a stranger's
  device. This is the cheapest effective control for the untrusted-network scenario.
- **I1** — if the Docker path is used, add a non-root `USER` to the `Dockerfile`.

> **Do not change the printer identity to be per-user.** Keep the shared
> `userID` / `uniqueID` (Phase 1c). Per-user identities would create two
> simultaneous "My Mac" targets under fast user switching and could deliver a scan
> to a background user. Shared identity + active-console gating is what satisfies
> requirement 1 ("only the active user gets the scan").

---

## Suggested commit sequence

1. `phase1: per-user LaunchAgent; drop root + chown, gate serving on active console user (H1/M2/M3)`
2. `phase2: write logs to ~/Library/Logs, off /tmp (H2)`
3. `phase3: bound strip/page/canvas allocation from printer data (L1)`
4. `phase4: gitignore *.log and remove stray log (L3)`
5. `phase5: optional hardening (L2/L4/I1) — as needed`

After each phase, run `make build-mac && make test`, then perform a live scan
against the real printer to confirm no protocol regression. The next-page check
loop and the unquoted multipart boundary (documented in `CLAUDE.md`) are the usual
breakage points.
