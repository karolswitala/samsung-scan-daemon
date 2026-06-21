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

# Accept MAC as second argument, otherwise auto-discover via ARP (no sudo needed)
if [ -n "$2" ]; then
    PRINTER_MAC="$2"
    echo "Using provided MAC: $PRINTER_MAC"
else
    ARP_OUT=$(/usr/sbin/arp -n "$PRINTER_IP" 2>/dev/null || true)
    PRINTER_MAC=$(echo "$ARP_OUT" | awk '{for(i=1;i<=NF;i++) if($i=="at") {print $(i+1); exit}}')
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
echo "Agent loaded. Printer IP: $PRINTER_IP"
echo "To view logs:  tail -f ~/Library/Logs/samsung-scan.log"
echo "To stop:       launchctl bootout gui/\$(id -u)/com.local.samsung-scan"
