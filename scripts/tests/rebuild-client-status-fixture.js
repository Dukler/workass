'use strict';

// Minimal loopback-only shell receipt used by the isolated daemon handoff test.
// Each catalog read advances its timestamp, modeling the fresh chat:catalog
// replay that the real Electron shell receives after a WebSocket reconnect.
const http = require('node:http');

const port = Number(process.argv[2]);
if (!Number.isInteger(port) || port < 1 || port > 65535) {
  process.stderr.write('usage: node rebuild-client-status-fixture.js PORT\n');
  process.exit(2);
}

let sequence = 0;
const server = http.createServer((req, res) => {
  if (req.method !== 'GET' || req.url !== '/__workass-shell/status') {
    res.writeHead(404);
    res.end();
    return;
  }
  sequence += 1;
  const body = JSON.stringify({
    controller: true,
    reportedAt: `controller-${sequence}`,
    catalog: {
      reportedAt: `catalog-${sequence}`,
      readyModelCount: 1,
      groups: [{ providerId: 'mock', status: 'ready', models: [{ modelId: 'mock', name: 'Mock' }] }],
    },
  });
  res.writeHead(200, {
    'Cache-Control': 'no-store',
    'Content-Type': 'application/json',
    'Content-Length': Buffer.byteLength(body),
  });
  res.end(body);
});

server.listen(port, '127.0.0.1', () => process.stdout.write(`READY ${port}\n`));

const close = () => server.close(() => process.exit(0));
process.on('SIGTERM', close);
process.on('SIGINT', close);
