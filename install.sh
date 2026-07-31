#!/bin/bash
set -e

# Do NOT run this under sudo. It is a per-user LaunchAgent installer and must run
# as your normal user so the plist under ~/Library/LaunchAgents is owned by you.
# It self-elevates with sudo only for the binary copy to /usr/local/bin. Running
# the whole script as root creates a root-owned plist that launchd rejects.
if [ "$(id -u)" -eq 0 ]; then
    echo "Error: run install.sh WITHOUT sudo — it prompts for sudo only when needed." >&2
    exit 1
fi

BINARY=dist/samsung-scan-macos
DEST=/usr/local/bin/samsung-scan
PLIST=launchd/com.local.samsung-scan.plist
LABEL=com.local.samsung-scan
PLIST_DEST="$HOME/Library/LaunchAgents/${LABEL}.plist"
OLD_DAEMON="/Library/LaunchDaemons/${LABEL}.plist"
LOG_DIR="$HOME/Library/Logs"

# Migration cleanup: earlier versions installed a root system LaunchDaemon. Leaving
# it in place means a stale root daemon keeps running and shadows this per-user
# agent, so remove it before installing.
if [ -e "$OLD_DAEMON" ]; then
    echo "Removing old root system LaunchDaemon ($OLD_DAEMON)"
    sudo launchctl bootout "system/${LABEL}" 2>/dev/null \
        || sudo launchctl unload "$OLD_DAEMON" 2>/dev/null \
        || true
    sudo rm -f "$OLD_DAEMON"
fi

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
