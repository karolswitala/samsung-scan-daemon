#!/bin/bash
set -e

LABEL=com.local.samsung-scan
DEST="$HOME/.local/bin/samsung-scan"
PLIST_DEST="$HOME/Library/LaunchAgents/$LABEL.plist"
LEGACY_DEST=/usr/local/bin/samsung-scan
LEGACY_DAEMON=/Library/LaunchDaemons/$LABEL.plist

# Read the printer IP from the installed plist (the argument following --ip)
PRINTER_IP=""
if [ -f "$PLIST_DEST" ]; then
    PRINTER_IP=$(/usr/libexec/PlistBuddy -c "Print :ProgramArguments" "$PLIST_DEST" 2>/dev/null \
        | awk '{$1=$1} prev=="--ip" {print; exit} {prev=$0}')
fi

echo "Stopping agent"
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true

# The agent deregisters on SIGTERM; --cleanup also covers a previously crashed run.
if [ -x "$DEST" ] && [ -n "$PRINTER_IP" ]; then
    echo "Removing printer registration at $PRINTER_IP"
    "$DEST" --ip "$PRINTER_IP" --cleanup || true
fi

for f in "$PLIST_DEST" "$DEST"; do
    if [ -e "$f" ]; then
        rm -f "$f"
        echo "Removed $f"
    fi
done

echo ""
echo "Uninstalled. Log file kept at ~/Library/Logs/samsung-scan.log"

if [ -e "$LEGACY_DAEMON" ] || [ -e "$LEGACY_DEST" ]; then
    echo ""
    echo "Leftovers from an older root-owned install remain. Remove them with:"
    if [ -e "$LEGACY_DAEMON" ]; then
        echo "  sudo launchctl unload $LEGACY_DAEMON && sudo rm $LEGACY_DAEMON"
    fi
    if [ -e "$LEGACY_DEST" ]; then
        echo "  sudo rm $LEGACY_DEST"
    fi
fi
