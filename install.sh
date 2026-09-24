#!/usr/bin/env bash
# Roundhouse installer: dependencies, Go, the rh binary, and a system service
# that runs the daemon (API + dashboard) on boot.
#
#   sudo ./install.sh          dashboard on http://127.0.0.1:7070 (this machine only)
#   sudo ./install.sh --lan    dashboard on port 7070 of every interface, with a login token
#                              (use this in a VM you open from your laptop's browser)
#   sudo ./install.sh --uninstall
#
# Safe to run again: it upgrades rh in place and restarts the service.
set -euo pipefail

GO_VERSION="1.24.7"
LISTEN="127.0.0.1:7070"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

say() { printf '\033[1;35m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

case "${1:-}" in
  --lan) LISTEN="0.0.0.0:7070" ;;
  --uninstall)
    [ "$(id -u)" -eq 0 ] || die "run with sudo"
    systemctl disable --now roundhouse 2>/dev/null || true
    rm -f /etc/systemd/system/roundhouse.service /etc/default/roundhouse /usr/local/bin/rh
    systemctl daemon-reload 2>/dev/null || true
    say "Removed the service and binary. Containers, images and volumes remain in /var/lib/roundhouse."
    say "Stop them with 'sudo rh ps' / 'sudo rh rm -f <id>', then 'sudo rm -rf /var/lib/roundhouse' to remove everything."
    exit 0 ;;
  "" ) ;;
  -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
  *) die "unknown option $1 (try --help)" ;;
esac

[ "$(id -u)" -eq 0 ] || die "run with sudo: sudo ./install.sh"
[ "$(uname -s)" = "Linux" ] || die "Roundhouse needs Linux (containers are a Linux kernel feature); see docs/foundations/f0-setup.md"
[ -f "$REPO_DIR/go.mod" ] || die "run this from inside the roundhouse repository"

say "Installing system packages (compiler, git, networking tools)"
if command -v apt-get >/dev/null; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq build-essential git curl ca-certificates iproute2 iptables >/dev/null
else
  say "not a Debian/Ubuntu system: make sure gcc, git, curl and iptables are installed"
fi

need_go=1
if command -v go >/dev/null || [ -x /usr/local/go/bin/go ]; then
  GOBIN="$(command -v go || echo /usr/local/go/bin/go)"
  have="$("$GOBIN" env GOVERSION 2>/dev/null | sed 's/^go//')"
  # Go 1.24 or newer is fine.
  if [ -n "$have" ] && [ "$(printf '%s\n' 1.24 "$have" | sort -V | head -1)" = "1.24" ]; then need_go=0; fi
fi
if [ "$need_go" -eq 1 ]; then
  arch="$(uname -m)"; case "$arch" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die "unsupported CPU architecture $arch" ;; esac
  say "Installing Go $GO_VERSION ($arch)"
  rm -rf /usr/local/go
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" | tar -C /usr/local -xz
  grep -q /usr/local/go/bin /etc/profile.d/go.sh 2>/dev/null || echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
fi
export PATH="$PATH:/usr/local/go/bin"

say "Building rh"
cd "$REPO_DIR"
CGO_ENABLED=1 GOFLAGS=-buildvcs=false go build -o /tmp/rh.new ./cmd/rh
# Rename rather than overwrite: running shims still execute the old binary
# ("text file busy"), and a rename leaves their copy intact.
install -m 0755 /tmp/rh.new /usr/local/bin/rh.new && mv -f /usr/local/bin/rh.new /usr/local/bin/rh
rm -f /tmp/rh.new

cat > /etc/default/roundhouse <<EOF
# Read by roundhouse.service. Change RH_HTTP and run: sudo systemctl restart roundhouse
RH_HTTP=$LISTEN
EOF

if [ -d /run/systemd/system ]; then
  say "Installing the roundhouse service"
  cat > /etc/systemd/system/roundhouse.service <<'EOF'
[Unit]
Description=Roundhouse daemon (provisioning engine, edge proxy, private DNS, dashboard)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/default/roundhouse
ExecStart=/usr/local/bin/rh daemon --http ${RH_HTTP}
Restart=always
RestartSec=2
# Stop only the daemon. Containers belong to their shims and keep running
# across daemon restarts and upgrades; the daemon adopts them when it comes
# back (see docs/08-provisioning-engine.md, "Failure modes").
KillMode=process

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable roundhouse >/dev/null 2>&1
  systemctl restart roundhouse
  for _ in $(seq 1 50); do [ -S /run/roundhouse.sock ] && break; sleep 0.2; done
  [ -S /run/roundhouse.sock ] || { journalctl -u roundhouse -n 30 --no-pager; die "the daemon did not start"; }
else
  say "No systemd here (a container or WSL without systemd): starting the daemon in the background"
  pkill -f '^/usr/local/bin/rh daemon' 2>/dev/null || true
  nohup /usr/local/bin/rh daemon --http "$LISTEN" >/var/log/roundhouse.log 2>&1 &
  for _ in $(seq 1 50); do [ -S /run/roundhouse.sock ] && break; sleep 0.2; done
  [ -S /run/roundhouse.sock ] || { tail -30 /var/log/roundhouse.log; die "the daemon did not start"; }
  say "Logs: /var/log/roundhouse.log (it will not restart on reboot without systemd)"
fi

echo
say "Roundhouse is running."
/usr/local/bin/rh dashboard --http "$LISTEN"
if [ "$LISTEN" = "127.0.0.1:7070" ]; then
  cat <<'EOF'

  Opening it from another computer (e.g. your laptop, with Roundhouse in a VM):
    - reinstall with:  sudo ./install.sh --lan      (token-protected), or
    - tunnel it:       ssh -L 7070:localhost:7070 <user>@<vm-ip>   then open http://localhost:7070
EOF
fi
echo
say "Try it:  open the dashboard, click New → Template → Static site"
say "CLI:     sudo rh svc ls · sudo rh ps · sudo rh --help"
