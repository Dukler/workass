set -eu
cd /data/workass-scratch
tar -xzf codex.tgz -C codex/bin && mv codex/bin/codex-x86_64-unknown-linux-musl codex/bin/codex && chmod +x codex/bin/codex
rm -rf runtime state bundle; mkdir -p runtime state feed
tar -xzf rel.tgz -C /data/workass-scratch && mv Workass-0.1.244-linux-amd64 bundle
cp -a bundle/. runtime/
chmod +x workass-linux-900 workass-linux-901
cp workass-linux-900 runtime/workass
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/workass-scratch.service <<UNIT
[Unit]
Description=Workass scratch daemon, stock KillMode (remote-rail proof)
[Service]
Type=simple
WorkingDirectory=/data/workass-scratch
ExecStart=/data/workass-scratch/runtime/workass -headless -bind lan -port 18788 -state-dir /data/workass-scratch/state
Environment=PATH=/data/workass-scratch/runtime/node/linux-amd64/bin:/data/workass-scratch/codex/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Environment=WORKASS_NODE=/data/workass-scratch/runtime/node/linux-amd64/bin/node
Environment=WORKASS_CODEX=/data/workass-scratch/codex/bin/codex
Environment=WORKASS_UPDATE_FEED_URL=http://127.0.0.1:18791/workass-linux-amd64-release.json
Restart=on-failure
RestartSec=2
[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user start workass-scratch.service
sleep 3
systemctl --user is-active workass-scratch.service
systemctl --user show workass-scratch.service -p KillMode -p Restart --value
curl -ks --max-time 3 https://127.0.0.1:18788/workass/health | head -c 300; echo
