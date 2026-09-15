#!/bin/bash
set -e

cd "$(dirname "$0")"

BINARY=dist/samsung-scan-macos
DEST="$HOME/.local/bin/samsung-scan"
LEGACY_DEST=/usr/local/bin/samsung-scan
PLIST=launchd/com.local.samsung-scan.plist
PLIST_DEST="$HOME/Library/LaunchAgents/com.local.samsung-scan.plist"
DEFAULT_IP="192.168.1.128"
GITHUB_REPO="karolswitala/samsung-scan-daemon"
RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/latest/download/samsung-scan-macos"

# If upgrading from the old root LaunchDaemon, remove it first:
#   sudo launchctl unload /Library/LaunchDaemons/com.local.samsung-scan.plist
#   sudo rm /Library/LaunchDaemons/com.local.samsung-scan.plist

# Accept printer IP as first argument, otherwise prompt
if [ -n "$1" ]; then
    PRINTER_IP="$1"
else
    read -rp "Printer IP address [$DEFAULT_IP]: " PRINTER_IP
    PRINTER_IP="${PRINTER_IP:-$DEFAULT_IP}"
fi

# Prompt for output directory (relative to home folder; Enter = Desktop)
read -rp "Output directory relative to home [Desktop]: " OUTPUT_SUBDIR
if [ -n "$OUTPUT_SUBDIR" ]; then
    OUTPUT_SUBDIR="${OUTPUT_SUBDIR#/}"   # strip any accidental leading slash
    OUTPUT_DIR="$HOME/$OUTPUT_SUBDIR"
    mkdir -p "$OUTPUT_DIR"
    echo "Output directory: $OUTPUT_DIR"
else
    OUTPUT_DIR=""
fi

# Accept MAC as second argument, otherwise auto-discover via ARP (no sudo needed)
if [ -n "$2" ]; then
    PRINTER_MAC="$2"
    echo "Using provided MAC: $PRINTER_MAC"
else
    ARP_OUT=$(/usr/sbin/arp -n "$PRINTER_IP" 2>/dev/null || true)
    PRINTER_MAC=$(echo "$ARP_OUT" | awk '{for(i=1;i<=NF;i++) if($i=="at") {print $(i+1); exit}}')
fi

# Always (re)install the binary so it matches the plist written below.
# Build from this checkout when Go is available; otherwise use the latest release.
if command -v go >/dev/null 2>&1; then
    echo "Building from source"
    make build-mac
    SRC="$BINARY"
else
    TMP_BINARY=$(mktemp -t samsung-scan)
    trap 'rm -f "$TMP_BINARY"' EXIT
    echo "Go not found — downloading latest release from GitHub"
    if ! curl -fsSL "$RELEASE_URL" -o "$TMP_BINARY"; then
        echo "Download failed. Install Go (brew install go) and re-run, or get the binary from:" >&2
        echo "  https://github.com/${GITHUB_REPO}/releases" >&2
        exit 1
    fi
    SRC="$TMP_BINARY"
fi

# Stop any previous version (it deregisters from the printer on SIGTERM)
# before replacing its binary.
launchctl bootout "gui/$(id -u)/com.local.samsung-scan" 2>/dev/null || true

echo "Installing binary to $DEST"
mkdir -p "$(dirname "$DEST")"
install -m 755 "$SRC" "$DEST"

echo "Installing LaunchAgent plist to $PLIST_DEST"
mkdir -p "$(dirname "$PLIST_DEST")"
cp "$PLIST" "$PLIST_DEST"
sed -i '' "s|__HOME__|$HOME|g" "$PLIST_DEST"
sed -i '' "s|192.168.1.128|$PRINTER_IP|g" "$PLIST_DEST"

# Inject output directory into plist if specified
if [ -n "$OUTPUT_DIR" ]; then
    /usr/libexec/PlistBuddy -c "Add :ProgramArguments: string --output" "$PLIST_DEST"
    /usr/libexec/PlistBuddy -c "Add :ProgramArguments: string $OUTPUT_DIR" "$PLIST_DEST"
fi

# Inject network guard MAC into plist if discovered (macOS-only; skipped automatically on Linux/Docker)
if [ -n "$PRINTER_MAC" ]; then
    /usr/libexec/PlistBuddy -c "Add :ProgramArguments: string --enable-network-guard" "$PLIST_DEST"
    /usr/libexec/PlistBuddy -c "Add :ProgramArguments: string $PRINTER_MAC" "$PLIST_DEST"
    echo "Network guard enabled — printer MAC: $PRINTER_MAC"
else
    echo "Printer not reachable at $PRINTER_IP — network guard skipped."
    echo "Re-run ./install.sh when the printer is online to enable it, or pass the MAC as a second argument:"
    echo "  ./install.sh $PRINTER_IP <mac>"
fi

launchctl bootstrap "gui/$(id -u)" "$PLIST_DEST"

echo ""
echo "Agent loaded."
echo "Binary:           $DEST"
echo "Printer IP:       $PRINTER_IP"
echo "Output:           ${OUTPUT_DIR:-~/Desktop (default)}"
echo "To view logs:     tail -f ~/Library/Logs/samsung-scan.log"
echo "To stop:          launchctl bootout gui/\$(id -u)/com.local.samsung-scan"
echo "To uninstall:     ./uninstall.sh"

if [ -e "$LEGACY_DEST" ]; then
    echo ""
    echo "Note: $LEGACY_DEST is from an older install and is no longer used."
    echo "Remove it once with:  sudo rm $LEGACY_DEST"
fi
