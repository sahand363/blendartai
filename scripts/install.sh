#!/usr/bin/env bash
# Blendartai installer and updater.
#   bash <(curl -fsSL https://raw.githubusercontent.com/sahand363/blendartai/main/scripts/install.sh)
# Re-running it updates the app and keeps your data and settings.
# Options: --version vX.Y.Z   install that release instead of the latest
#          --yes              no questions, use defaults (or existing settings)
set -euo pipefail

REPO="sahand363/blendartai"
BIN=/usr/local/bin/blendartai-server
MENU=/usr/local/bin/blendartai
ETC=/etc/blendartai
ENVF=$ETC/blendartai.env
DATA=/var/lib/blendartai
UNIT=/etc/systemd/system/blendartai.service

VERSION=""; YES=0
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --yes|-y) YES=1; shift ;;
    *) echo "unknown option $1"; exit 1 ;;
  esac
done

c_ok=$'\e[32m'; c_warn=$'\e[33m'; c_err=$'\e[31m'; c_b=$'\e[1m'; c_0=$'\e[0m'
say()  { echo "${c_b}==>${c_0} $*"; }
warn() { echo "${c_warn}!!${c_0} $*"; }
die()  { echo "${c_err}xx${c_0} $*" >&2; exit 1; }
ask()  { local q="$1" d="$2" a; if [ "$YES" = 1 ]; then echo "$d"; return; fi; read -r -p "$q [$d]: " a </dev/tty || true; echo "${a:-$d}"; }

[ "$(id -u)" = 0 ] || die "run as root (sudo)"
command -v systemctl >/dev/null || die "systemd is required"
for t in curl sha256sum; do command -v $t >/dev/null || die "$t is required"; done

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU $(uname -m) (amd64 and arm64 only)" ;;
esac

if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | grep -m1 '"tag_name"' | cut -d'"' -f4) || true
  [ -n "$VERSION" ] || die "could not find the latest release"
fi
URL="https://github.com/$REPO/releases/download/$VERSION"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

say "Downloading Blendartai $VERSION ($ARCH)"
curl -fsSL -o "$TMP/blendartai-linux-$ARCH" "$URL/blendartai-linux-$ARCH"
curl -fsSL -o "$TMP/blendartai.sh" "$URL/blendartai.sh"
curl -fsSL -o "$TMP/SHA256SUMS" "$URL/SHA256SUMS"
( cd "$TMP" && grep -E " (blendartai-linux-$ARCH|blendartai.sh)\$" SHA256SUMS | sha256sum -c --quiet - ) || die "checksum mismatch, aborting"

port_owner() { ss -Hltnp "sport = :$1" 2>/dev/null | sed -n 's/.*users:(("\([^"]*\)".*/\1/p' | head -1; }
port_busy()  { ss -Hltn "sport = :$1" 2>/dev/null | grep -q .; }

FRESH=0
if [ ! -f "$ENVF" ]; then
  FRESH=1
  say "First install"
  PORT=$(ask "Panel port" 2096)
  while port_busy "$PORT"; do
    warn "port $PORT is used by $(port_owner "$PORT" || echo another program)"
    [ "$YES" = 1 ] && die "port $PORT is busy"
    PORT=$(ask "Pick another port" $((PORT+1)))
  done
  # Plain HTTP on first install. Add SSL later from the menu: sudo blendartai -> SSL / domain.
  SSL=off; DOMAIN=""; EMAIL=""; CERT=""; KEY=""
  mkdir -p "$ETC"; chmod 700 "$ETC"
  cat > "$ENVF" <<ENV
# Blendartai server settings. Edit with: sudo blendartai
BLENDARTAI_PORT=$PORT
BLENDARTAI_BIND=
BLENDARTAI_DATA=$DATA
BLENDARTAI_BASE_PATH=
BLENDARTAI_SSL=$SSL
BLENDARTAI_DOMAIN=$DOMAIN
BLENDARTAI_EMAIL=$EMAIL
BLENDARTAI_CERT=$CERT
BLENDARTAI_KEY=$KEY
ENV
  chmod 600 "$ENVF"
else
  say "Updating, your data and settings are kept"
fi

mkdir -p "$DATA"; chmod 700 "$DATA"
[ -f "$BIN" ] && cp -f "$BIN" "$BIN.prev"
[ -f "$MENU" ] && cp -f "$MENU" "$MENU.prev"
install -m 755 "$TMP/blendartai-linux-$ARCH" "$BIN"
install -m 755 "$TMP/blendartai.sh" "$MENU"

cat > "$UNIT" <<UNIT
[Unit]
Description=Blendartai image mixer panel
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=$ENVF
ExecStart=$BIN serve
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=full
PrivateTmp=true
ReadWritePaths=$DATA $ETC
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable blendartai >/dev/null 2>&1
systemctl restart blendartai
sleep 2
systemctl is-active --quiet blendartai || { journalctl -u blendartai -n 30 --no-pager; die "service did not start"; }

echo
echo "${c_ok}Blendartai $VERSION is running.${c_0}"
BLENDARTAI_FROM_INSTALL=1 "$MENU" info || true
echo
echo "Manage it any time with: ${c_b}sudo blendartai${c_0}  (SSL / domain is option 7)"
# Interactive installs land in the menu. Updates from the menu (--yes) go back to it on their own.
if [ "$YES" = 0 ] && ( : </dev/tty ) 2>/dev/null; then
  sleep 1
  exec "$MENU" </dev/tty >/dev/tty 2>&1
fi
