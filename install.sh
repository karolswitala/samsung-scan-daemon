#!/bin/bash
set -e

BINARY=dist/samsung-scan-macos
DEST=/usr/local/bin/samsung-scan
PLIST=launchd/com.local.samsung-scan.plist
PLIST_DEST="$HOME/Library/LaunchAgents/com.local.samsung-scan.plist"
DEFAULT_IP="192.168.1.128"

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

# Build and install the binary only if it is not already present.
# On a multi-user Mac the first user installs it; subsequent users skip this.
if [ ! -f "$DEST" ]; then
    echo "Building..."
    make build-mac
    echo "Installing binary to $DEST"
    sudo mkdir -p "$(dirname "$DEST")"
    sudo cp "$BINARY" "$DEST"
else
    echo "Binary already installed at $DEST — skipping build"
fi

echo "Installing LaunchAgent plist to $PLIST_DEST"
mkdir -p "$(dirname "$PLIST_DEST")"
cp "$PLIST" "$PLIST_DEST"
sed -i '' "s|__HOME__|$HOME|g" "$PLIST_DEST"
sed -i '' "s|192.168.1.128|$PRINTER_IP|g" "$PLIST_DEST"

# Unload any previous version before (re)loading
launchctl bootout "gui/$(id -u)/com.local.samsung-scan" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST_DEST"

echo ""
echo "Agent loaded. Printer IP: $PRINTER_IP"
echo "To view logs:  tail -f ~/Library/Logs/samsung-scan.log"
echo "To stop:       launchctl bootout gui/\$(id -u)/com.local.samsung-scan"
