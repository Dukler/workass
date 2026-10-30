// Talk to the scratch daemon's own wire from its own machine, the way a client
// does: node scratch-wire.mjs CHANNEL 'JSON-ARG'
process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const [channel, arg] = process.argv.slice(2);
const ws = new WebSocket('wss://127.0.0.1:18788/ws', { headers: { Origin: 'https://127.0.0.1:18788' } });
const pending = new Map(); let n = 0;
const invoke = (c, ...args) => new Promise((resolve, reject) => { const id = 'x' + (++n); pending.set(id, { resolve, reject }); ws.send(JSON.stringify({ t: 'invoke', id, channel: c, args })); });
ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.t === 'reply' && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result ?? m.value ?? m); } };
ws.onerror = (e) => { console.error('socket', e.message); process.exit(1); };
ws.onopen = async () => {
  await new Promise((r) => setTimeout(r, 300));
  try { await invoke('lan:take-control', {}).catch(() => {}); console.log(JSON.stringify(await invoke(channel, JSON.parse(arg || '{}')))); }
  catch (error) { console.log('ERROR', error.message); }
  process.exit(0);
};
