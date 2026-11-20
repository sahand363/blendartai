#!/usr/bin/env bash
# Blendartai SSH menu. Run: sudo blendartai   (or: sudo blendartai info|restart|logs|update)
set -uo pipefail

REPO="sahand363/blendartai"
BIN=/usr/local/bin/blendartai-server
MENU=/usr/local/bin/blendartai
ETC=/etc/blendartai
ENVF=$ETC/blendartai.env
UNIT=/etc/systemd/system/blendartai.service
SVC=blendartai

c_ok=$'\e[32m'; c_warn=$'\e[33m'; c_err=$'\e[31m'; c_b=$'\e[1m'; c_d=$'\e[2m'; c_0=$'\e[0m'

[ "$(id -u)" = 0 ] || { echo "Run with sudo: sudo blendartai"; exit 1; }
[ -f "$ENVF" ] || { echo "Blendartai is not installed ($ENVF missing)."; exit 1; }

getenv() { grep -E "^$1=" "$ENVF" | tail -1 | cut -d= -f2-; }
setenv() {
  if grep -qE "^$1=" "$ENVF"; then sed -i "s|^$1=.*|$1=${2//|/\\|}|" "$ENVF"; else echo "$1=$2" >> "$ENVF"; fi
}
srv() { set -a; . "$ENVF"; set +a; "$BIN" "$@"; }
pause() { read -r -p "Press Enter to go back…" _ </dev/tty || true; }
port_busy() { ss -Hltn "sport = :$1" 2>/dev/null | grep -q .; }
port_owner() { ss -Hltnp "sport = :$1" 2>/dev/null | sed -n 's/.*users:(("\([^"]*\)".*/\1/p' | head -1; }

panel_url() {
  local ssl port host bp scheme=http
  ssl=$(getenv BLENDARTAI_SSL); port=$(getenv BLENDARTAI_PORT); host=$(getenv BLENDARTAI_DOMAIN); bp=$(getenv BLENDARTAI_BASE_PATH)
  case "$ssl" in selfsigned|letsencrypt|files) scheme=https ;; esac
  if [ "$ssl" = proxy ]; then echo "behind your reverse proxy (local: http://127.0.0.1:$port$bp/)"; return; fi
  [ -n "$host" ] || host=$(curl -fsS4 --max-time 4 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')
  echo "$scheme://$host:$port$bp/"
}

header() {
  clear 2>/dev/null || true
  local st ver
  if systemctl is-active --quiet $SVC; then st="${c_ok}running${c_0}"; else st="${c_err}stopped${c_0}"; fi
  ver=$("$BIN" version 2>/dev/null || echo "?")
  echo "${c_b}Blendartai${c_0} ${c_d}v$ver${c_0}   status: $st"
  echo "Panel: $(panel_url)"
  echo "SSL:   $(getenv BLENDARTAI_SSL)"
  local code; code=$(srv setup-code 2>/dev/null)
  if [ -n "$code" ]; then echo "${c_warn}No admin yet. Setup code: ${c_b}$code${c_0}"; fi
  echo "────────────────────────────────────────"
}

reset_pw() {
  local u p1 p2 cur; cur=$(srv info 2>/dev/null | sed -n 's/^Login user: *//p'); case "$cur" in none*|"") cur=admin ;; esac
  read -r -p "Username [$cur]: " u </dev/tty; u=${u:-$cur}
  read -r -s -p "New password (10+ characters): " p1 </dev/tty; echo
  read -r -s -p "Repeat password: " p2 </dev/tty; echo
  printf '%s\n%s\n%s\n' "$u" "$p1" "$p2" | srv reset-password >/dev/null && echo "${c_ok}Login set for $u. Everyone was logged out.${c_0}"
}

restart() { systemctl restart $SVC; sleep 1; systemctl is-active --quiet $SVC && echo "${c_ok}Restarted.${c_0}" || { echo "${c_err}It failed to start:${c_0}"; journalctl -u $SVC -n 20 --no-pager; }; }

change_port() {
  local p; read -r -p "New port: " p </dev/tty
  [[ "$p" =~ ^[0-9]+$ ]] && [ "$p" -ge 1 ] && [ "$p" -le 65535 ] || { echo "Not a valid port."; return; }
  [ "$p" = "$(getenv BLENDARTAI_PORT)" ] && { echo "That's already the port."; return; }
  if port_busy "$p"; then echo "${c_err}Port $p is used by $(port_owner "$p" || echo another program).${c_0}"; return; fi
  setenv BLENDARTAI_PORT "$p"; restart
  command -v ufw >/dev/null && ufw status | grep -q active && ufw allow "$p/tcp" >/dev/null && echo "Opened $p in ufw."
}

ssl_menu() {
  echo "1) Domain with HTTPS (reuse a certificate on this server, or Let's Encrypt)"
  echo "2) Self-signed HTTPS (works on an IP, browser shows a warning)"
  echo "3) Behind a reverse proxy (nginx, Caddy, CorText): plain HTTP on 127.0.0.1"
  echo "4) Certificate files I give"
  echo "5) Off (plain HTTP, not recommended)"
  local c d e crt key; read -r -p "Choose: " c </dev/tty
  case "$c" in
    1) read -r -p "Domain: " d </dev/tty; [ -n "$d" ] || return
       crt=""; key=""
       for dir in "/etc/letsencrypt/live/$d" "/root/cert/$d"; do
         [ -f "$dir/fullchain.pem" ] && [ -f "$dir/privkey.pem" ] && { crt="$dir/fullchain.pem"; key="$dir/privkey.pem"; break; }
       done
       setenv BLENDARTAI_DOMAIN "$d"; setenv BLENDARTAI_BIND ""
       if [ -n "$crt" ]; then
         echo "Found a certificate in $(dirname "$crt"), using it."
         setenv BLENDARTAI_SSL files; setenv BLENDARTAI_CERT "$crt"; setenv BLENDARTAI_KEY "$key"
       elif [ "$(getenv BLENDARTAI_PORT)" = 443 ] || ! port_busy 80; then
         read -r -p "Email for Let's Encrypt (optional): " e </dev/tty
         setenv BLENDARTAI_SSL letsencrypt; setenv BLENDARTAI_EMAIL "$e"
         echo "Let's Encrypt will issue the certificate on the first visit."
       else
         echo "${c_warn}Port 80 belongs to $(port_owner 80 || echo another program), and I won't touch it.${c_0}"
         echo "Get a certificate with that program (or certbot), then pick option 4, or move the panel to 443."
         return
       fi ;;
    2) setenv BLENDARTAI_SSL selfsigned; setenv BLENDARTAI_BIND "" ;;
    3) setenv BLENDARTAI_SSL proxy; setenv BLENDARTAI_BIND 127.0.0.1
       read -r -p "URL path the proxy uses (empty for /): " d </dev/tty; setenv BLENDARTAI_BASE_PATH "$d"
       echo "Point your proxy at http://127.0.0.1:$(getenv BLENDARTAI_PORT)${d}/ and pass X-Forwarded-Proto." ;;
    4) read -r -p "Certificate (fullchain) path: " crt </dev/tty; read -r -p "Key path: " key </dev/tty
       [ -f "$crt" ] && [ -f "$key" ] || { echo "File not found."; return; }
       setenv BLENDARTAI_SSL files; setenv BLENDARTAI_CERT "$crt"; setenv BLENDARTAI_KEY "$key"; setenv BLENDARTAI_BIND "" ;;
    5) setenv BLENDARTAI_SSL off; setenv BLENDARTAI_BIND "" ;;
    *) return ;;
  esac
  restart
  echo "Panel: $(panel_url)"
}

update() {
  local tmp; tmp=$(mktemp)
  curl -fsSL -o "$tmp" "https://raw.githubusercontent.com/$REPO/main/scripts/install.sh" || { echo "Download failed."; rm -f "$tmp"; return; }
  bash "$tmp" --yes "$@"; rm -f "$tmp"
}

rollback() {
  [ -f "$BIN.prev" ] || { echo "No previous version saved."; return; }
  echo "Roll back from $("$BIN" version) to $("$BIN.prev" version)?"
  read -r -p "[y/N]: " a </dev/tty; [[ "$a" =~ ^[Yy] ]] || return
  cp -f "$BIN" "$BIN.next"; cp -f "$BIN.prev" "$BIN"; mv -f "$BIN.next" "$BIN.prev"
  [ -f "$MENU.prev" ] && { cp -f "$MENU" "$MENU.next"; cp -f "$MENU.prev" "$MENU"; mv -f "$MENU.next" "$MENU.prev"; }
  restart
}

uninstall() {
  echo "${c_err}This removes Blendartai from this server.${c_0}"
  read -r -p "Type REMOVE to continue: " a </dev/tty; [ "$a" = REMOVE ] || { echo "Cancelled."; return; }
  local data; data=$(getenv BLENDARTAI_DATA); data=${data:-/var/lib/blendartai}
  read -r -p "Also delete your images, providers and keys in $data? [y/N]: " d </dev/tty
  systemctl disable --now $SVC >/dev/null 2>&1
  rm -f "$UNIT" "$BIN" "$BIN.prev" "$MENU.prev"; systemctl daemon-reload
  [[ "$d" =~ ^[Yy] ]] && rm -rf "$data" "$ETC" && echo "Data deleted." || echo "Data kept in $data."
  rm -f "$MENU"; echo "Blendartai removed."; exit 0
}

info() {
  echo "Version:      $("$BIN" version)"
  echo "Panel:        $(panel_url)"
  srv info
  [ "$(getenv BLENDARTAI_SSL)" = selfsigned ] && echo "${c_d}Self-signed: your browser will warn once, that's expected.${c_0}"
}

case "${1:-}" in
  info) info; exit ;;
  start|stop|restart|status) systemctl "$1" $SVC; exit ;;
  logs) journalctl -u $SVC -f; exit ;;
  update) shift; update "$@"; exit ;;
  rollback) rollback; exit ;;
  reset-password) reset_pw; exit ;;
  version) "$BIN" version; exit ;;
  "") ;;
  *) echo "usage: sudo blendartai [info|start|stop|restart|status|logs|update|rollback|reset-password|version]"; exit 1 ;;
esac

while true; do
  header
  cat <<M
 1) Start            2) Stop            3) Restart
 4) Panel login info 5) Reset admin password
 6) Change port      7) SSL / domain
 8) Live logs        9) Update         10) Roll back
11) Uninstall        0) Exit
M
  read -r -p "Choose: " c </dev/tty || exit 0
  echo
  case "$c" in
    1) systemctl start $SVC && echo "Started." ;;
    2) systemctl stop $SVC && echo "Stopped." ;;
    3) restart ;;
    4) info ;;
    5) reset_pw ;;
    6) change_port ;;
    7) ssl_menu ;;
    8) echo "Ctrl+C to go back."; journalctl -u $SVC -f -n 50 || true ;;
    9) update ;;
    10) rollback ;;
    11) uninstall ;;
    0|q) exit 0 ;;
    *) continue ;;
  esac
  pause
done
