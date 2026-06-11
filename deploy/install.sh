#!/bin/sh
# install.sh — set up the argus group, socket directory and systemd unit.
# Run as root on the target host (e.g. Raspberry Pi 5).
set -eu

BIN_DIR="${BIN_DIR:-/usr/local/bin}"
RUN_DIR=/run/argus
UNIT_SRC="$(dirname "$0")/argusd.service"
UNIT_DST=/etc/systemd/system/argusd.service

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must run as root" >&2
    exit 1
fi

# Group for unprivileged clients that may talk to argusd.
if ! getent group argus >/dev/null; then
    groupadd --system argus
    echo "created group: argus"
fi

# Socket directory (systemd recreates it via RuntimeDirectory= on every start;
# this covers manual runs of argusd outside systemd).
mkdir -p "$RUN_DIR"
chown root:argus "$RUN_DIR"
chmod 0750 "$RUN_DIR"

# Install binaries if they were built next to this script's repo (optional).
for bin in argusd argus argus-mcp; do
    if [ -f "$(dirname "$0")/../bin/linux-arm64/$bin" ]; then
        install -m 0755 "$(dirname "$0")/../bin/linux-arm64/$bin" "$BIN_DIR/$bin"
        echo "installed $BIN_DIR/$bin"
    fi
done

install -m 0644 "$UNIT_SRC" "$UNIT_DST"
systemctl daemon-reload
echo "installed $UNIT_DST"
echo "enable + start with: systemctl enable --now argusd"
echo "add users to the argus group with: usermod -aG argus <user>"
