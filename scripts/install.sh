#!/bin/sh
# Install or update the Enphase dashboard as a systemd service on Linux
# (Raspberry Pi OS, Debian, Ubuntu, ...). Run as root; re-run to update.
#
#   curl -fsSL https://raw.githubusercontent.com/cdonner/enphase-dashboard/main/scripts/install.sh | sudo ENVOY_HOST=192.168.1.50 sh
#
# Optional: VERSION=v1.2.0 to pin a release (default: latest).
set -eu

REPO=cdonner/enphase-dashboard
VERSION=${VERSION:-latest}
BIN=/usr/local/bin/enphase-dashboard
DATA=/var/lib/enphase-dashboard
CONF=/etc/enphase-dashboard.env
UNIT=/etc/systemd/system/enphase-dashboard.service
USER_NAME=enphase

die() { echo "Error: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
command -v systemctl >/dev/null || die "systemd is required"

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  armv7l | armv8l) ARCH=armv7 ;;
  armv6l) ARCH=armv6 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

if [ "$VERSION" = latest ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi
ASSET="enphase-dashboard-linux-$ARCH"

download() {
  if command -v curl >/dev/null; then
    curl -fsSL -o "$2" "$1"
  elif command -v wget >/dev/null; then
    wget -qO "$2" "$1"
  else
    die "curl or wget is required"
  fi
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "Downloading $ASSET ($VERSION)..."
download "$BASE/$ASSET" "$TMP/$ASSET" || die "download failed: $BASE/$ASSET"
if download "$BASE/SHA256SUMS" "$TMP/SHA256SUMS" 2>/dev/null && command -v sha256sum >/dev/null; then
  (cd "$TMP" && grep " $ASSET\$" SHA256SUMS | sha256sum -c -) >/dev/null || die "checksum mismatch"
  echo "Checksum OK"
fi
install -m 755 "$TMP/$ASSET" "$BIN"

if ! id "$USER_NAME" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir "$DATA" --shell /usr/sbin/nologin "$USER_NAME"
fi
install -d -m 750 -o "$USER_NAME" -g "$USER_NAME" "$DATA"

if [ ! -f "$CONF" ]; then
  TZ_NAME=$(timedatectl show -p Timezone --value 2>/dev/null || cat /etc/timezone 2>/dev/null || echo UTC)
  cat >"$CONF" <<CONF
# Enphase dashboard settings. After editing: sudo systemctl restart enphase-dashboard
ENVOY_HOST=${ENVOY_HOST:-envoy.local}
TZ=$TZ_NAME
HOST=0.0.0.0
PORT=3001
DATA_DIR=$DATA
# DASHBOARD_PASSWORD=
# ENPHASE_API_KEY=
# ENPHASE_CLIENT_ID=
# ENPHASE_CLIENT_SECRET=
# ANTHROPIC_API_KEY=
# GEMINI_API_KEY=
# MQTT_URL=mqtt://homeassistant.local:1883
# MQTT_USERNAME=
# MQTT_PASSWORD=
CONF
  chmod 600 "$CONF"
  echo "Created $CONF"
fi

cat >"$UNIT" <<UNIT
[Unit]
Description=Enphase dashboard
Wants=network-online.target
After=network-online.target

[Service]
EnvironmentFile=$CONF
ExecStart=$BIN
WorkingDirectory=$DATA
User=$USER_NAME
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DATA

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable enphase-dashboard >/dev/null 2>&1
systemctl restart enphase-dashboard

PORT_NUM=$(sed -n 's/^PORT=//p' "$CONF")
IP=$(hostname -I 2>/dev/null | awk '{print $1}')
echo "Enphase dashboard is running: http://${IP:-<this-host>}:${PORT_NUM:-3001}"
echo "Settings: $CONF   Logs: journalctl -u enphase-dashboard -f"
