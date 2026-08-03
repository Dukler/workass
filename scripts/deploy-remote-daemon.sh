#!/bin/sh
# Put a workass daemon on another machine and keep it there.
#
# The remote daemon was hand-provisioned: a cross-compile, an scp, and a nohup
# started over SSH. Everything about that survives exactly until the machine
# reboots, and none of it is repeatable by anyone who was not watching. This
# script is the repeatable version, and the unit it installs is the difference
# between "a process I started" and "a machine that is part of your fleet".
#
# Deliberately NOT here:
#   - No privileged port. A remote machine is one you reach INTO, never one that
#     dials out (D10), so nothing has to listen below 1024 and nothing needs
#     setcap, pf, or root.
#   - No fleet key on the command line. `ps` is world-readable and a shell keeps
#     history; the key is read on stdin, exactly as `workass fleet join` does.
#
# usage:
#   scripts/deploy-remote-daemon.sh user@192.168.1.50
#   scripts/deploy-remote-daemon.sh user@host --port 18788 --arch arm64
#   workass fleet key | scripts/deploy-remote-daemon.sh user@host --join
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)

target=""
port=18788
arch=amd64
remote_dir=""
join=0
beacon=false

while [ $# -gt 0 ]; do
  case "$1" in
    --port) port=$2; shift 2 ;;
    --arch) arch=$2; shift 2 ;;
    --dir) remote_dir=$2; shift 2 ;;
    --join) join=1; shift ;;
    # A machine on your own LAN can announce itself so the app finds it without
    # anyone typing an address. Off by default: announcing is a deliberate act,
    # and some networks are not yours to announce on.
    --beacon) beacon=true; shift ;;
    -h|--help) sed -n '2,26p' "$0"; exit 0 ;;
    *) target=$1; shift ;;
  esac
done

if [ -z "$target" ]; then
  echo "usage: scripts/deploy-remote-daemon.sh <user@host> [--port N] [--arch amd64|arm64] [--join]" >&2
  exit 2
fi

remote_user=${target%@*}
[ "$remote_user" = "$target" ] && remote_user=""
[ -n "$remote_dir" ] || remote_dir="workass"

key=""
if [ "$join" = 1 ]; then
  # Read before anything long-running, so a missing key fails in a second
  # rather than after a cross-compile and an upload.
  key=$(cat)
  case "$key" in
    wf-*) ;;
    *) echo "that does not look like a fleet key (expected wf-…)" >&2; exit 2 ;;
  esac
fi

echo "==> building linux/$arch"
staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT
(cd "$repo_root" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$staging/workass" ./cmd/workass)

echo "==> uploading to $target:$remote_dir"
# Every remote command goes through `sh -s` on stdin rather than being pasted at
# the login shell: the account on the other end may well run fish or zsh, and a
# POSIX fragment sent to fish fails in the middle of a deploy with a syntax
# error about your own script.
printf 'mkdir -p %s %s/state\n' "$remote_dir" "$remote_dir" | ssh "$target" sh -s
# Upload beside the running binary and move it into place: replacing a file that
# is currently executing fails on Linux, and a half-written binary would come
# back as a daemon that cannot start.
scp -q "$staging/workass" "$target:$remote_dir/workass.new"
printf 'mv -f %s/workass.new %s/workass && chmod +x %s/workass\n' "$remote_dir" "$remote_dir" "$remote_dir" | ssh "$target" sh -s

if [ "$join" = 1 ]; then
  echo "==> joining the fleet"
  # The key travels on the same stdin the remote sh is reading, so it is never an
  # argument: `ps` is world-readable and a shell keeps history.
  { printf 'cd %s && ./workass fleet join --state-dir %s/state <<KEY\n' "$remote_dir" "$remote_dir"
    printf '%s\nKEY\n' "$key"
  } | ssh "$target" sh -s
fi

echo "==> installing the service"
# systemd USER unit plus lingering: a user service normally dies when the last
# session ends, which for a headless box means the daemon lives exactly as long
# as your SSH connection. Lingering is what makes it survive logout and boot.
#
# PATH is explicit because a login shell's PATH is not a service's. Under nohup
# the daemon inherited `~/.local/bin` and found `claude` there; a unit inherits
# a minimal PATH instead, and the failure is a remote chat that starts no engine
# and says nothing about why.
cat > "$staging/workass.service" <<UNIT
[Unit]
Description=workass daemon
Documentation=https://github.com/dukler/workass
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h/$remote_dir
Environment=PATH=%h/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ExecStart=%h/$remote_dir/workass --state-dir %h/$remote_dir/state --port $port --bind lan --trust-localhost=false --beacon=$beacon
Restart=always
RestartSec=3
# Engines are children of the daemon. Without this an agent survives a restart
# holding a repo, and the next turn meets a lock nobody owns.
KillMode=control-group
TimeoutStopSec=20

[Install]
WantedBy=default.target
UNIT

scp -q "$staging/workass.service" "$target:workass.service.new"
ssh "$target" sh -s <<REMOTE
set -eu
mkdir -p ~/.config/systemd/user
mv -f ~/workass.service.new ~/.config/systemd/user/workass.service
loginctl enable-linger "\$(id -un)" 2>/dev/null || sudo loginctl enable-linger "\$(id -un)"
systemctl --user daemon-reload
systemctl --user enable workass.service
# A daemon already started by hand still owns the port, and Restart=always would
# spin against it forever. Stop the unit, clear anything left on that state dir,
# then start clean.
systemctl --user stop workass.service 2>/dev/null || true
pkill -f "workass --state-dir \$HOME/$remote_dir/state" 2>/dev/null || true
sleep 1
systemctl --user start workass.service
REMOTE

echo "==> health"
# The daemon binds before it is ready to answer; a couple of polls beats a sleep
# that is either too short to work or too long to sit through.
ssh "$target" sh -s <<REMOTE
for i in 1 2 3 4 5 6 7 8 9 10; do
  out=\$(curl -s --max-time 3 http://127.0.0.1:$port/workass/health || true)
  case "\$out" in ?*) printf '%s\n' "\$out" | head -c 400; echo; exit 0 ;; esac
  sleep 1
done
echo 'no health response after 10s' >&2
systemctl --user status workass.service --no-pager | head -20
exit 1
REMOTE

echo
echo "done. it now starts on boot and restarts on crash:"
echo "  ssh $target systemctl --user status workass"
echo "  ssh $target journalctl --user -u workass -f"
