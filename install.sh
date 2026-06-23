#!/bin/bash
set -e

BINARY=dist/samsung-scan-macos
DEST=/usr/local/bin/samsung-scan
PLIST=launchd/com.local.samsung-scan.plist
PLIST_DEST="$HOME/Library/LaunchAgents/com.local.samsung-scan.plist"
DEFAULT_IP="192.168.1.128"
GITHUB_REPO="karol/samsung-scan"
RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/latest/download/samsung-scan-macos"
TMP_BINARY=/tmp/samsung-scan-download

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

# Install the binary only if it is not already present.
# On a multi-user Mac the first user installs it; subsequent users skip this.
if [ ! -f "$DEST" ]; then
    if curl -fsSL "$RELEASE_URL" -o "$TMP_BINARY" 2>/dev/null; then
        echo "Downloaded pre-built binary from GitHub Releases"
        chmod +x "$TMP_BINARY"
        sudo mkdir -p "$(dirname "$DEST")"
        sudo install -m 755 "$TMP_BINARY" "$DEST"
        rm -f "$TMP_BINARY"
    else
        echo "No pre-built release found — building from source (Go required)."
        echo "If Go is not installed, download the binary from:"
        echo "  https://github.com/${GITHUB_REPO}/releases"
        make build-mac
        echo "Installing binary to $DEST"
        sudo mkdir -p "$(dirname "$DEST")"
        sudo cp "$BINARY" "$DEST"
    fi
else
    echo "Binary already installed at $DEST — skipping"
fi

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

# Unload any previous version before (re)loading
launchctl bootout "gui/$(id -u)/com.local.samsung-scan" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST_DEST"

echo ""
echo "Agent loaded."
echo "Printer IP:       $PRINTER_IP"
echo "Output:           ${OUTPUT_DIR:-~/Desktop (default)}"
echo "To view logs:     tail -f ~/Library/Logs/samsung-scan.log"
echo "To stop:          launchctl bootout gui/\$(id -u)/com.local.samsung-scan"
