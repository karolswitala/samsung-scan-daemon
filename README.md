# samsung-scan

Samsung M2070W Scan-to-PC daemon — lightweight background service for macOS and Linux/Docker.

## Overview

Implements the reverse-engineered Samsung Scan-to-PC protocol:
- **SNMP** (UDP 161): polls printer state every 3 seconds
- **HTTP** (TCP 80): registers this machine and announces scan profiles
- **Binary TCP** (TCP 9400): downloads JPEG strips and reassembles them

Output: timestamped JPEG or multi-page PDF saved to the output directory.

## Build

```bash
# macOS ARM64 (M-series)
make build-mac

# Linux AMD64 (Docker host)
make build-linux

# Docker multi-arch image
make docker
```

## Usage

```bash
./dist/samsung-scan-macos --ip 192.168.1.128 [--output ~/Desktop] [--resolution 300] [--log-level info]
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--ip` | (required) | Printer IP address |
| `--output` | `~/Desktop` | Directory for saved scans |
| `--resolution` | `300` | DPI: 75/150/300/600/1200 |
| `--color` | `COLOR_TRUE` | Color/Grayscale/BlackWhite |
| `--format` | `FORMAT_M_PDF` | Hint: FORMAT_M_PDF or FORMAT_JPEG (printer selection overrides) |
| `--poll` | `3s` | SNMP poll interval |
| `--cleanup` | false | Deregister stale entries and exit |
| `--log-level` | `info` | debug/info/warn/error |

## macOS Install (launchd)

```bash
# Build and install
make build-mac
cp dist/samsung-scan-macos /usr/local/bin/samsung-scan

# Edit printer IP in plist, then:
cp launchd/com.local.samsung-scan.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.local.samsung-scan.plist

# View logs
tail -f /tmp/samsung-scan.log

# Stop
launchctl unload ~/Library/LaunchAgents/com.local.samsung-scan.plist
```

## Docker

```bash
docker run --rm samsung-scan:latest --ip 192.168.1.128 --output /scans
```

## Protocol

See [PROTOCOL.md](../PROTOCOL.md) for the full wire-level specification.
