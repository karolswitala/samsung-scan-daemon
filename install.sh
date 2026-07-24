#!/bin/bash
set -e

BINARY=dist/samsung-scan-macos
DEST=/usr/local/bin/samsung-scan
PLIST=launchd/com.local.samsung-scan.plist
PLIST_DEST="$HOME/Library/LaunchAgents/com.local.samsung-scan.plist"
LOG_DIR="$HOME/Library/Logs"

echo "Building..."
make build-mac

echo "Installing binary to $DEST (may prompt for sudo)"
sudo mkdir -p "$(dirname "$DEST")"
sudo cp "$BINARY" "$DEST"

echo "Installing LaunchAgent plist to $PLIST_DEST"
mkdir -p "$HOME/Library/LaunchAgents" "$LOG_DIR"
# launchd does not expand ~, so bake the real home directory into the log paths.
sed "s|__HOME__|$HOME|g" "$PLIST" > "$PLIST_DEST"

echo ""
echo "Edit $PLIST_DEST to set your printer IP, then run:"
echo "  launchctl load $PLIST_DEST"
echo ""
echo "To view logs:  tail -f $LOG_DIR/samsung-scan.log"
echo "To stop:       launchctl unload $PLIST_DEST"
