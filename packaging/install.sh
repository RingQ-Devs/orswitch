#!/usr/bin/env bash
# Install ORswitch as a systemd service.
#
#   From the source tree:            sudo make install   (builds first)
#   From the prebuilt binary package: sudo bash packaging/install.sh
#
# Layout:
#   /usr/local/bin/orswitch, /usr/local/bin/or_cli
#   /usr/local/share/orswitch/install-transcription.sh (Call transcription)
#   /etc/orswitch/            configuration (kept if it already exists)
#   /var/lib/orswitch/        log/cdr.csv, recordings/, sounds/
#   /etc/systemd/system/orswitch.service
set -euo pipefail

PREFIX=/usr/local
CONF=/etc/orswitch
DATA=/var/lib/orswitch
SRC="$(cd "$(dirname "$0")/.." && pwd)"

if [ "$(id -u)" -ne 0 ]; then
  echo "error: run as root (sudo bash $0)" >&2
  exit 1
fi

# The installer writes the live configuration to $CONF and the data to
# $DATA, so the source/package tree must live somewhere else.
case "$SRC/" in
  "$CONF"/*|"$DATA"/*)
    echo "error: the source tree is inside $CONF or $DATA, which the installer manages." >&2
    echo "       Move it first, e.g.:  mv $SRC /opt/orswitch-src && cd /opt/orswitch-src" >&2
    exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) ARCH="$(uname -m)" ;;
esac

BIN=""
for dir in "$SRC/bin" "$SRC/bin/linux-$ARCH"; do
  if [ -x "$dir/orswitch" ] && [ -x "$dir/or_cli" ]; then BIN="$dir"; break; fi
done
if [ -z "$BIN" ]; then
  echo "error: binaries not found. Run 'make build' first (or use the prebuilt package for $ARCH)." >&2
  exit 1
fi
echo "==> using binaries from $BIN"

if ! id ringq >/dev/null 2>&1; then
  echo "==> creating system user 'ringq'"
  useradd --system --home-dir "$DATA" --no-create-home --shell /usr/sbin/nologin ringq
fi

echo "==> installing binaries to $PREFIX/bin"
install -m 0755 "$BIN/orswitch" "$PREFIX/bin/orswitch"
install -m 0755 "$BIN/or_cli" "$PREFIX/bin/or_cli"
# RingQ Core runs "fs_cli -x ..." for many Global Settings; or_cli answers it (as fs_cli it
# falls back to FreeSWITCH's default event socket password for users who cannot read the config).
if [ ! -e /usr/bin/fs_cli ] || [ -L /usr/bin/fs_cli ]; then ln -sf "$PREFIX/bin/or_cli" /usr/bin/fs_cli; fi
# The Call transcription installer, at a path the web interface can show.
if [ -f "$SRC/packaging/install-transcription.sh" ]; then
  install -d -m 0755 "$PREFIX/share/orswitch"
  install -m 0755 "$SRC/packaging/install-transcription.sh" "$PREFIX/share/orswitch/install-transcription.sh"
fi

echo "==> creating $DATA"
mkdir -p "$DATA/log" "$DATA/recordings" "$DATA/sounds/music/default" "$DATA/sounds/prompts" "$DATA/voicemail" "$DATA/fax"
if [ -f "$SRC/sounds/README.md" ] && [ ! -f "$DATA/sounds/README.md" ]; then
  cp "$SRC/sounds/README.md" "$DATA/sounds/"
fi
chown -R ringq:ringq "$DATA"

if [ -f "$CONF/orswitch.xml" ]; then
  echo "==> keeping existing configuration in $CONF"
else
  echo "==> installing configuration to $CONF"
  mkdir -p "$CONF"
  cp -r "$SRC/conf/." "$CONF/"
  # The sample uses paths relative to the conf dir; point them at $DATA.
  sed -i -e "s#\"../log/cdr.csv\"#\"$DATA/log/cdr.csv\"#" \
         -e "s#\"../sounds\"#\"$DATA/sounds\"#" \
         -e "s#\"../recordings\"#\"$DATA/recordings\"#" \
         -e "s#\"../voicemail\"#\"$DATA/voicemail\"#" "$CONF/orswitch.xml"
  # Config holds passwords: readable by the service user only.
  chown -R root:ringq "$CONF"
  find "$CONF" -type d -exec chmod 0750 {} +
  find "$CONF" -type f -exec chmod 0640 {} +
fi

if [ -d /run/systemd/system ]; then
  echo "==> installing systemd unit"
  install -m 0644 "$SRC/packaging/orswitch.service" /etc/systemd/system/orswitch.service
  systemctl daemon-reload
else
  echo "==> systemd not found; start manually: sudo -u ringq $PREFIX/bin/orswitch -conf $CONF"
fi

echo "==> validating configuration"
if "$PREFIX/bin/orswitch" -conf "$CONF" -check; then
  CHECK=ok
else
  CHECK=failed
fi

if grep -q '<param name="dsn" value="postgres' "$CONF/orswitch.xml" 2>/dev/null; then
  cat <<EOF

ORswitch installed (database mode: $CONF/orswitch.xml has a database address).
EOF
  if [ "$CHECK" = failed ]; then
    cat <<EOF
The check above could not use that database. On a TEST server, create a test
database and connect ORswitch to it with one command:
    sudo bash packaging/setup-test-db.sh
Step-by-step guide: docs/STEP-BY-STEP.md
EOF
  else
    cat <<EOF
Next: sudo systemctl enable --now orswitch && sudo journalctl -u orswitch -f
EOF
  fi
  exit 0
fi

cat <<EOF

ORswitch installed. Next steps:
  1. Edit $CONF/orswitch.xml:
       - default_password and event-socket-password (do not keep ChangeMe-*)
       - ext-sip-ip / ext-rtp-ip = this server's PUBLIC IP if it is behind NAT (all clouds)
  2. sudo orswitch -conf $CONF -check
  3. sudo systemctl enable --now orswitch
  4. sudo journalctl -u orswitch -f
  5. sudo or_cli -x status   (sudo or_cli: the live console)
To use a PostgreSQL database instead of XML files (test database in one
command: sudo bash packaging/setup-test-db.sh), see docs/STEP-BY-STEP.md.
EOF
