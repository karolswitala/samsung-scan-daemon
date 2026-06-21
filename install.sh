#!/bin/bash
set -e

BINARY=dist/samsung-scan-macos
DEST=/usr/local/bin/samsung-scan
PLIST=launchd/com.local.samsung-scan.plist
PLIST_DEST="/Library/LaunchDaemons/com.local.samsung-scan.plist"

echo "Building..."
make build-mac

echo "Installing binary to $DEST"
mkdir -p "$(dirname "$DEST")"
cp "$BINARY" "$DEST"

echo "Installing LaunchAgent plist"
cp "$PLIST" "$PLIST_DEST"

echo ""
echo "Edit $PLIST_DEST to set your printer IP, then run:"
echo "  sudo launchctl load $PLIST_DEST"
echo ""
echo "To view logs:  tail -f /tmp/samsung-scan.log"
echo "To stop:       sudo launchctl unload $PLIST_DEST"
