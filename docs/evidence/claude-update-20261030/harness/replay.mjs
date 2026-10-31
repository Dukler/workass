// Read-only: what a fresh client is replayed about provider updates.
process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const port = process.argv[2];
const ws = new WebSocket(`wss://127.0.0.1:${port}/ws`, { headers: { Origin: `https://127.0.0.1:${port}` } });
const want = new Set(['providers:updates', 'providers:update-progress']);
ws.onmessage = (e) => { const m = JSON.parse(e.data); if (want.has(m.channel)) console.log(JSON.stringify(m)); };
ws.onerror = (e) => { console.error('socket', e.message); process.exit(1); };
setTimeout(() => process.exit(0), 1200);
