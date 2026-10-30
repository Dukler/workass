set -eu
cd /data/workass-scratch
V=0.1.901; B=Workass-$V-linux-amd64
rm -rf feed/*; mkdir -p feed/$B; cp -a bundle/. feed/$B/; cp workass-linux-901 feed/$B/workass
printf '{"schemaVersion":2,"platform":"linux","arch":"amd64","version":"%s","revision":"scratch","headless":true,"electron":false}\n' $V > feed/$B/manifest.json
tar -C feed -czf feed/$B.tar.gz $B; rm -rf feed/$B
SHA=$(sha256sum feed/$B.tar.gz | cut -d' ' -f1); SIZE=$(stat -c%s feed/$B.tar.gz)
cat > feed/workass-linux-amd64-release.json <<J
{"schemaVersion":1,"product":"Workass","version":"$V","platform":"linux","arch":"amd64","headless":true,"artifacts":{"update":{"name":"$B.tar.gz","url":"http://127.0.0.1:18791/$B.tar.gz","sha256":"$SHA","size":$SIZE}}}
J
systemctl --user stop workass-scratch-feed 2>/dev/null || true
systemd-run --user --unit workass-scratch-feed --working-directory=/data/workass-scratch/feed --collect /usr/bin/python3 -m http.server 18791 --bind 127.0.0.1 2>&1 | tail -1
sleep 1; curl -s http://127.0.0.1:18791/workass-linux-amd64-release.json | cut -c1-120
