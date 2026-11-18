#!/usr/bin/env bash
# Use the server's free (ACME) certificate for the ORswitch web interface.
#
#   sudo bash packaging/use-certificate.sh pbx.example.com
#       Uses /etc/letsencrypt/live/pbx.example.com/{fullchain,privkey}.pem
#       (made by the server's certificate client, e.g. for another web
#       server). The files are copied to /etc/orswitch/tls/ (readable by
#       the ringq service user; the original folder is not changed),
#       orswitch.xml is pointed at them, and a renewal hook copies
#       renewed certificates, which ORswitch picks up by itself within a
#       minute.
#
#   sudo bash packaging/use-certificate.sh pbx.example.com --port 443
#       Also moves the web interface to port 443 (only when nothing else,
#       such as another web server, uses it).
#
# Afterwards open https://pbx.example.com:8443/ (or https://pbx.example.com/
# with --port 443): no certificate warning, and the dialer and meetings work
# in every browser.
set -euo pipefail

DOMAIN="${1:-}"
PORT=""
[ "${2:-}" = "--port" ] && PORT="${3:-}"
CONF=/etc/orswitch
CFG="$CONF/orswitch.xml"

die() { echo "error: $*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "run as root: sudo bash $0 <domain>"
[[ "$DOMAIN" =~ ^[A-Za-z0-9.-]+$ ]] || die "usage: sudo bash $0 <domain> [--port 443]"
[ -z "$PORT" ] || [[ "$PORT" =~ ^[0-9]+$ ]] || die "--port: a port number"
LIVE="/etc/letsencrypt/live/$DOMAIN"
[ -f "$LIVE/fullchain.pem" ] && [ -f "$LIVE/privkey.pem" ] || die "$LIVE/fullchain.pem and privkey.pem not found (get the certificate for $DOMAIN first)"
[ -f "$CFG" ] || die "$CFG not found"
grep -q '<web>' "$CFG" || die "$CFG has no <web> section (run packaging/setup-test-db.sh, or add it from conf/examples/ringq-postgres.xml)"

DEST="$CONF/tls/$DOMAIN"
copy() {
  mkdir -p "$DEST"
  install -m 0644 -o root -g root "$LIVE/fullchain.pem" "$DEST/fullchain.pem.new"
  if id ringq >/dev/null 2>&1; then
    install -m 0640 -o root -g ringq "$LIVE/privkey.pem" "$DEST/privkey.pem.new"
  else
    install -m 0600 -o root -g root "$LIVE/privkey.pem" "$DEST/privkey.pem.new"
  fi
  # Key first, then the certificate: ORswitch reloads when the certificate changes.
  mv -f "$DEST/privkey.pem.new" "$DEST/privkey.pem"
  mv -f "$DEST/fullchain.pem.new" "$DEST/fullchain.pem"
  if id ringq >/dev/null 2>&1; then chgrp ringq "$CONF/tls" "$DEST"; chmod 0750 "$CONF/tls" "$DEST"; fi
}
copy
echo "certificate copied to $DEST"

cp -p "$CFG" "$CFG.before-certificate.$(date +%Y%m%d-%H%M%S)"
TLS="$DEST/fullchain.pem,$DEST/privkey.pem"
if grep -q '<param name="tls"' "$CFG"; then
  sed -i "s|<param name=\"tls\" value=\"[^\"]*\"/>|<param name=\"tls\" value=\"$TLS\"/>|" "$CFG"
else
  sed -i "s|<web>|<web>\n    <param name=\"tls\" value=\"$TLS\"/>|" "$CFG"
fi
echo "orswitch.xml: tls = $TLS"

if [ -n "$PORT" ]; then
  if ss -ltnH "sport = :$PORT" 2>/dev/null | grep -q .; then
    echo "WARNING: something already listens on TCP port $PORT:"
    ss -ltnp "sport = :$PORT" | sed 's/^/   /'
    echo "Keeping the current port. (With another web server on 443, put ORswitch behind it: docs/WEB-UI.md, section 11.3.)"
    PORT=""
  else
    sed -i "s|<param name=\"listen\" value=\"\\([0-9.]*\\):[0-9]*\"/>|<param name=\"listen\" value=\"\\1:$PORT\"/>|" "$CFG"
    if [ "$PORT" -lt 1024 ] && [ -d /run/systemd/system ]; then
      mkdir -p /etc/systemd/system/orswitch.service.d
      printf '[Service]\nAmbientCapabilities=CAP_NET_BIND_SERVICE\n' > /etc/systemd/system/orswitch.service.d/port.conf
      systemctl daemon-reload
    fi
    echo "web interface moved to port $PORT"
  fi
fi

HOOK=/etc/letsencrypt/renewal-hooks/deploy/orswitch-$DOMAIN.sh
mkdir -p "$(dirname "$HOOK")"
cat > "$HOOK" <<EOF
#!/bin/sh
# Copies the renewed certificate for ORswitch (installed by use-certificate.sh).
case "\$RENEWED_LINEAGE" in */$DOMAIN) ;; *) exit 0 ;; esac
mkdir -p "$DEST"
install -m 0640 -o root -g \$(id -gn ringq 2>/dev/null || echo root) "$LIVE/privkey.pem" "$DEST/privkey.pem.new"
install -m 0644 "$LIVE/fullchain.pem" "$DEST/fullchain.pem.new"
mv -f "$DEST/privkey.pem.new" "$DEST/privkey.pem"
mv -f "$DEST/fullchain.pem.new" "$DEST/fullchain.pem"
EOF
chmod 0755 "$HOOK"
echo "renewal hook: $HOOK"

if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
  WEBPORT=$(grep -o 'name="listen" value="[^"]*"' "$CFG" | head -1 | sed 's/.*://; s/"$//')
  RTCPORT=$(grep -o 'name="webrtc-port" value="[0-9]*"' "$CFG" | grep -o '[0-9]*$' || echo 8443)
  [ -n "$WEBPORT" ] && ufw allow "$WEBPORT/tcp" >/dev/null && echo "firewall: $WEBPORT/tcp open"
  [ "$RTCPORT" != "0" ] && ufw allow "$RTCPORT/udp" >/dev/null && echo "firewall: $RTCPORT/udp open (dialer and meetings audio/video)"
fi

orswitch -conf "$CONF" -check >/dev/null || die "the configuration check failed: run orswitch -conf $CONF -check"
if [ -d /run/systemd/system ]; then
  systemctl restart orswitch
  sleep 2
  systemctl is-active --quiet orswitch || { journalctl -u orswitch -n 20 --no-pager; die "ORswitch did not start"; }
fi
P=$(grep -o 'name="listen" value="[^"]*"' "$CFG" | head -1 | sed 's/.*://; s/"$//')
echo
echo "Done. Open: https://$DOMAIN$([ "$P" = 443 ] || echo ":$P")/"
