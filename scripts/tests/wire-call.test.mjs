import test from 'node:test';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import os from 'node:os';
import path from 'node:path';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const tool = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../wire-call.mjs');

function serverFrame(opcode, payload, { fin = true } = {}) {
  const length = payload.length;
  const first = (fin ? 0x80 : 0) | opcode;
  const header = length < 126 ? Buffer.from([first, length])
    : length < 65536 ? Buffer.from([first, 126, length >> 8, length & 255])
      : (() => { const big = Buffer.alloc(10); big[0] = first; big[1] = 127; big.writeBigUInt64BE(BigInt(length), 2); return big; })();
  return Buffer.concat([header, payload]);
}

function readClientFrame(buffer) {
  if (buffer.length < 6) return null;
  const opcode = buffer[0] & 0x0f;
  let length = buffer[1] & 0x7f;
  let offset = 2;
  assert.ok(buffer[1] & 0x80, 'a client frame must be masked');
  if (length === 126) { if (buffer.length < 4) return null; length = buffer.readUInt16BE(2); offset = 4; }
  else if (length === 127) { if (buffer.length < 10) return null; length = Number(buffer.readBigUInt64BE(2)); offset = 10; }
  if (buffer.length < offset + 4 + length) return null;
  const mask = buffer.subarray(offset, offset + 4);
  const payload = Buffer.from(buffer.subarray(offset + 4, offset + 4 + length));
  for (let index = 0; index < payload.length; index += 1) payload[index] ^= mask[index % 4];
  return { opcode, payload, rest: buffer.subarray(offset + 4 + length) };
}

// The tiny server speaks exactly the frozen frames; `respond` sees each invoke
// and each other client frame.
async function server(t, respond, { create = http.createServer, options } = {}) {
  const frames = [];
  const upgraded = new Set();
  const httpServer = options ? create(options) : create();
  httpServer.on('upgrade', (request, socket) => {
    upgraded.add(socket);
    const accept = crypto.createHash('sha1').update(`${request.headers['sec-websocket-key']}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest('base64');
    socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
    let buffered = Buffer.alloc(0);
    socket.on('error', () => {});
    socket.on('data', chunk => {
      buffered = Buffer.concat([buffered, chunk]);
      for (let frame = readClientFrame(buffered); frame; frame = readClientFrame(buffered)) {
        buffered = frame.rest;
        frames.push(frame);
        if (frame.opcode === 1) respond(JSON.parse(frame.payload.toString()), socket, request);
      }
    });
  });
  await new Promise(resolve => httpServer.listen(0, '127.0.0.1', resolve));
  t.after(() => {
    for (const socket of upgraded) socket.destroy();
    return new Promise(resolve => httpServer.close(resolve));
  });
  return { port: httpServer.address().port, frames };
}

function call(args) {
  return new Promise(resolve => {
    const child = spawn(process.execPath, [tool, ...args], { stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '';
    let stderr = '';
    child.stdout.on('data', chunk => { stdout += chunk; });
    child.stderr.on('data', chunk => { stderr += chunk; });
    child.on('close', code => resolve({ code, stdout, stderr }));
  });
}

const json = value => Buffer.from(JSON.stringify(value));

test('sends one frozen invoke frame and prints only the matching reply as JSON', async t => {
  let invoke;
  const { port } = await server(t, (frame, socket) => {
    invoke = frame;
    socket.write(serverFrame(1, json({ t: 'event', channel: 'job:event', payload: { noise: true } })));
    socket.write(serverFrame(1, json({ t: 'reply', id: frame.id + 1, result: 'someone else', error: null })));
    socket.write(serverFrame(1, json({ t: 'reply', id: frame.id, result: { ok: true, list: [1, 2] }, error: null })));
  });
  const result = await call(['--url', `ws://127.0.0.1:${port}`, 'chat:list', '["tab-1",2]']);
  assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), { ok: true, list: [1, 2] });
  assert.deepEqual(Object.keys(invoke).sort(), ['args', 'channel', 'id', 't']);
  assert.equal(invoke.t, 'invoke');
  assert.equal(invoke.channel, 'chat:list');
  assert.deepEqual(invoke.args, ['tab-1', 2]);
});

test('args default to an empty array and a null result prints as null', async t => {
  let invoke;
  const { port } = await server(t, (frame, socket) => {
    invoke = frame;
    socket.write(serverFrame(1, json({ t: 'reply', id: frame.id, result: null, error: null })));
  });
  const result = await call(['--url', `ws://127.0.0.1:${port}`, 'app:ping']);
  assert.equal(result.code, 0, result.stderr);
  assert.equal(result.stdout.trim(), 'null');
  assert.deepEqual(invoke.args, []);
});

test('an error reply exits nonzero with the error on stderr', async t => {
  const { port } = await server(t, (frame, socket) => {
    socket.write(serverFrame(1, json({ t: 'reply', id: frame.id, result: null, error: 'no such channel' })));
  });
  const result = await call(['--url', `ws://127.0.0.1:${port}`, 'nope']);
  assert.equal(result.code, 1);
  assert.equal(result.stdout, '');
  assert.match(result.stderr, /no such channel/);
});

test('a silent server times out nonzero', async t => {
  const { port } = await server(t, () => {});
  const result = await call(['--url', `ws://127.0.0.1:${port}/?deviceToken=abc123secret`, '--timeout-ms', '100', 'chat:list']);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /no reply to chat:list within 100 ms/);
  assert.doesNotMatch(result.stderr, /abc123secret/, 'errors never echo the token');
});

test('handles a fragmented large reply split across writes, and answers a ping', async t => {
  const large = 'x'.repeat(200_000);
  const { port, frames } = await server(t, (frame, socket) => {
    const body = json({ t: 'reply', id: frame.id, result: large, error: null });
    const whole = Buffer.concat([
      serverFrame(9, Buffer.from('still there')),
      serverFrame(1, body.subarray(0, 70_000), { fin: false }),
      serverFrame(0, body.subarray(70_000), { fin: true }),
    ]);
    for (let start = 0; start < whole.length; start += 32_768) socket.write(whole.subarray(start, start + 32_768));
  });
  const result = await call(['--url', `ws://127.0.0.1:${port}`, 'session:get']);
  assert.equal(result.code, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout), large);
  assert.ok(frames.some(frame => frame.opcode === 10 && frame.payload.toString() === 'still there'));
});

test('a refused upgrade, a server close and a dead port all exit nonzero', async t => {
  const refusing = http.createServer((_request, response) => { response.statusCode = 403; response.end(); });
  await new Promise(resolve => refusing.listen(0, '127.0.0.1', resolve));
  const refusingPort = refusing.address().port;
  const refused = await call(['--url', `ws://127.0.0.1:${refusingPort}`, 'chat:list']);
  await new Promise(resolve => refusing.close(resolve));
  assert.equal(refused.code, 1);
  assert.match(refused.stderr, /upgrade refused.*403/);
  const dead = await call(['--url', `ws://127.0.0.1:${refusingPort}`, 'chat:list']);
  assert.equal(dead.code, 1);

  const { port } = await server(t, (_frame, socket) => { socket.write(serverFrame(8, Buffer.alloc(0))); });
  const closed = await call(['--url', `ws://127.0.0.1:${port}`, 'chat:list']);
  assert.equal(closed.code, 1);
  assert.match(closed.stderr, /closed/);
});

test('bad usage exits 2 before connecting', async () => {
  for (const args of [[], ['chat:list'], ['--url', 'http://127.0.0.1:1', 'chat:list'], ['--url', 'ws://127.0.0.1:1'], ['--url', 'ws://127.0.0.1:1', 'chat:list', '{"not":"array"}'], ['--url', 'ws://127.0.0.1:1', 'chat:list', 'not json'], ['--url', 'ws://127.0.0.1:1', '--timeout-ms', '0', 'chat:list'], ['--url', 'ws://127.0.0.1:1', '--bogus', 'chat:list']]) {
    const result = await call(args);
    assert.equal(result.code, 2, args.join(' '));
    assert.match(result.stderr, /usage:/);
  }
});

test('wss verifies the certificate unless told --insecure', async t => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-wire-call-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const keyPath = path.join(dir, 'fixture-key');
  const certPath = path.join(dir, 'fixture-cert');
  const made = spawnSync('openssl', ['req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1', '-nodes', '-keyout', keyPath, '-out', certPath, '-days', '1', '-subj', '/CN=127.0.0.1'], { encoding: 'utf8' });
  if (made.status !== 0) { t.skip('openssl is not available'); return; }
  const { port } = await server(t, (frame, socket) => {
    socket.write(serverFrame(1, json({ t: 'reply', id: frame.id, result: 'over tls', error: null })));
  }, { create: https.createServer, options: { key: fs.readFileSync(keyPath), cert: fs.readFileSync(certPath) } });
  const refused = await call(['--url', `wss://127.0.0.1:${port}`, 'chat:list']);
  assert.equal(refused.code, 1);
  assert.match(refused.stderr, /--insecure/);
  const accepted = await call(['--url', `wss://127.0.0.1:${port}`, '--insecure', 'chat:list']);
  assert.equal(accepted.code, 0, accepted.stderr);
  assert.equal(JSON.parse(accepted.stdout), 'over tls');
});
