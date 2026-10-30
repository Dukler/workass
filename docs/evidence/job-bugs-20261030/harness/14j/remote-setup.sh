# On duklerver. A scratch daemon: its own unit, port and state, stock KillMode, nothing from the real install.
set -eu
sudo -n mkdir -p /data/workass-scratch; sudo -n chown "$(id -un)" /data/workass-scratch
cd /data/workass-scratch; mkdir -p runtime state codex/bin bundle
tar -xzf codex.tgz -C codex/bin; tar -xzf host.tgz -C codex/bin
mv -f codex/bin/codex-x86_64-unknown-linux-musl codex/bin/codex; mv -f codex/bin/codex-code-mode-host-x86_64-unknown-linux-musl codex/bin/codex-code-mode-host; chmod +x codex/bin/*
tar -xzf rel.tgz -C bundle --strip-components=1; cp -a bundle/. runtime/
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/workass-scratch.service <<UNIT
[Unit]
Description=Workass scratch daemon (orphaned-row and status proof)
[Service]
Type=simple
WorkingDirectory=/data/workass-scratch
ExecStart=/data/workass-scratch/runtime/workass -headless -bind lan -port 18788 -state-dir /data/workass-scratch/state
Environment=PATH=/data/workass-scratch/runtime/node/linux-amd64/bin:/data/workass-scratch/codex/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Environment=WORKASS_NODE=/data/workass-scratch/runtime/node/linux-amd64/bin/node
Environment=WORKASS_CODEX=/data/workass-scratch/codex/bin/codex
Restart=on-failure
RestartSec=2
[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
