# On duklerver. A scratch daemon: its own unit, port and state, stock KillMode, nothing from the real install.
set -eu
sudo -n mkdir -p /data/workass-scratch; sudo -n chown "$(id -un)" /data/workass-scratch
cd /data/workass-scratch; mkdir -p runtime state
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/workass-scratch.service <<UNIT
[Unit]
Description=Workass scratch daemon (job-bugs proof)
[Service]
Type=simple
WorkingDirectory=/data/workass-scratch
ExecStart=/data/workass-scratch/runtime/workass -headless -bind lan -port 18788 -state-dir /data/workass-scratch/state
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Restart=on-failure
RestartSec=2
[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
