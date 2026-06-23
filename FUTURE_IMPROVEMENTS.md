# Potential improvements

## 1. Native Linux deployment (systemd user unit)

Linux is currently supported only via Docker. A systemd user unit would allow native
installation without a container runtime.

A `~/.config/systemd/user/samsung-scan.service` alongside an updated `install.sh`
that detects Linux and uses `systemctl --user` would mirror the macOS LaunchAgent
model closely. The daemon already builds a fully static Linux binary (`make build-linux`),
so no code changes are required — only a service unit file and installer support.

## 2. Linux feature parity: network guard and active-console-user gating

Two macOS-specific safety features silently no-op on Linux:

- **Network guard** (`--enable-network-guard`) — uses `/usr/sbin/arp` to verify the
  printer's MAC before each registration. On Linux the equivalent is
  `ip neigh show <ip>` from `iproute2`, which is available on all mainstream distros.
- **Active-console-user gating** — reads `/dev/console` ownership, a macOS-only
  mechanism. On a single-user Linux machine this is irrelevant, but on a multi-seat
  setup (or inside a container on a shared host) there is no equivalent guard and the
  daemon registers unconditionally.

The network guard is the more actionable of the two: switching to `ip neigh` for Linux
would give Docker and native Linux deployments the same printer-impersonation protection
as macOS.

## 3. Lossless scan output

Multi-strip JPEG scans are currently reassembled by decoding each strip to RGBA and
re-encoding the combined image at Q95 (`internal/imageutil/assemble.go`). This
introduces a second lossy generation for what was already a lossy JPEG.

Two directions:

- **Lossless JPEG join** — if all strips share the same quantisation tables and
  dimensions, the strips can be spliced at the entropy-coded data level without
  decoding (similar to what `jpegtran -crop` does). This preserves the original quality
  exactly. It is non-trivial to implement in pure Go but eliminates the re-encoding loss.
- **Q100 re-encoding** — a simpler interim step: raise the re-encode quality to 100.
  It does not recover the lost information from the first encode but stops adding
  further generation loss. One-line change in `assemble.go`.

For typical 300 DPI document scans Q95 is visually transparent, so this is a
quality-of-life improvement rather than a functional gap.

## 4. Paper size flexibility

The PDF page dimensions are hardcoded to A4 in `internal/imageutil/assemble.go`.
The printer's `UserSelect.xml` response already contains the selected paper size
(`FORMAT_S_PDF`, `FORMAT_M_PDF`, etc.) but does not expose an explicit size field in
the current parsing — the daemon downloads whatever pixel data arrives.

If Letter or other sizes are ever needed, the assembler could derive page dimensions
from the downloaded image's pixel dimensions and the resolution reported by
`GetUserSelect`, rather than assuming A4. This would make the output PDF dimensions
match the physical paper size the user selected on the printer LCD.
