#!/usr/bin/env node
// Call one channel on a running Workass daemon over the frozen wire protocol
// (docs/PORT-SPEC.md §2) and print the reply's result as JSON.
//
//   node scripts/wire-call.mjs --url wss://127.0.0.1:PORT [--insecure] CHANNEL [JSON_ARGS]
//
// JSON_ARGS is a JSON array, spread into the handler. Off loopback a device token
// goes in the URL: wss://host:PORT/?deviceToken=... Exit 0 on a reply without
// error, 1 on an error reply, a timeout or a lost connection, 2 on bad usage.
import crypto from 'node:crypto';
import net from 'node:net';
import tls from 'node:tls';
import { fileURLToPath } from 'node:url';
import fs from 'node:fs';

const DEFAULT_TIMEOUT_MS = 30_000;
const MAX_MESSAGE_BYTES = 512 * 1024 * 1024; // above the daemon's own frame limit
const USAGE = 'usage: node scripts/wire-call.mjs --url ws(s)://HOST:PORT [--insecure] [--timeout-ms N] CHANNEL [JSON_ARGS]';
const OP = { continuation: 0, text: 1, close: 8, ping: 9, pong: 10 };

class UsageError extends Error {}

export function parseArgs(argv) {
  const options = { insecure: false, timeoutMs: DEFAULT_TIMEOUT_MS, args: [] };
  const positional = [];
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === '--insecure') options.insecure = true;
    else if (arg === '--url') options.url = argv[++index];
    else if (arg === '--timeout-ms') options.timeoutMs = Number(argv[++index]);
    else if (arg.startsWith('--')) throw new UsageError(`unknown option: ${arg}`);
    else positional.push(arg);
  }
  if (!options.url) throw new UsageError('--url is required');
  let parsed;
  try { parsed = new URL(options.url); } catch { throw new UsageError('--url is not a valid URL'); }
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') throw new UsageError('--url must be ws:// or wss://');
  options.url = parsed;
  if (!Number.isInteger(options.timeoutMs) || options.timeoutMs < 1) throw new UsageError('--timeout-ms must be a positive integer');
  if (positional.length < 1 || positional.length > 2) throw new UsageError('expected CHANNEL and optional JSON_ARGS');
  [options.channel] = positional;
  if (positional[1] !== undefined) {
    try { options.args = JSON.parse(positional[1]); } catch { throw new UsageError('JSON_ARGS is not valid JSON'); }
    if (!Array.isArray(options.args)) throw new UsageError('JSON_ARGS must be a JSON array: args are spread into the handler');
  }
  return options;
}

function clientFrame(opcode, payload) {
  const length = payload.length;
  const header = length < 126 ? Buffer.from([0x80 | opcode, 0x80 | length])
    : length < 65536 ? Buffer.from([0x80 | opcode, 0x80 | 126, length >> 8, length & 255])
      : (() => { const big = Buffer.alloc(10); big[0] = 0x80 | opcode; big[1] = 0x80 | 127; big.writeBigUInt64BE(BigInt(length), 2); return big; })();
  const mask = crypto.randomBytes(4);
  const masked = Buffer.from(payload);
  for (let index = 0; index < masked.length; index += 1) masked[index] ^= mask[index % 4];
  return Buffer.concat([header, mask, masked]);
}

// Accumulates chunks and flattens only when enough bytes have arrived, so a very
// large session reply stays linear.
function frameReader(onFrame) {
  let chunks = [];
  let size = 0;
  let needed = 2;
  let header = null;
  const take = count => {
    const all = chunks.length === 1 ? chunks[0] : Buffer.concat(chunks, size);
    const head = all.subarray(0, count);
    const rest = all.subarray(count);
    chunks = rest.length ? [rest] : [];
    size = rest.length;
    return head;
  };
  const peek = () => { if (chunks.length > 1) chunks = [Buffer.concat(chunks, size)]; return chunks[0]; };
  return chunk => {
    chunks.push(chunk);
    size += chunk.length;
    while (size >= needed) {
      if (!header) {
        const bytes = peek();
        const lengthCode = bytes[1] & 0x7f;
        const masked = (bytes[1] & 0x80) !== 0;
        const extra = lengthCode === 126 ? 2 : lengthCode === 127 ? 8 : 0;
        if (size < 2 + extra) { needed = 2 + extra; return; }
        const length = extra === 2 ? bytes.readUInt16BE(2) : extra === 8 ? Number(bytes.readBigUInt64BE(2)) : lengthCode;
        if (masked) throw new Error('server sent a masked frame');
        if (bytes[0] & 0x70) throw new Error('server sent a compressed or reserved frame that was never negotiated');
        if (length > MAX_MESSAGE_BYTES) throw new Error(`frame of ${length} bytes is over the client limit`);
        header = { fin: (bytes[0] & 0x80) !== 0, opcode: bytes[0] & 0x0f, length };
        take(2 + extra);
        needed = length;
      }
      if (size < needed) return;
      const payload = header.length ? Buffer.from(take(header.length)) : Buffer.alloc(0);
      const frame = { ...header, payload };
      header = null;
      needed = 2;
      onFrame(frame);
    }
  };
}

export function wireCall({ url, insecure = false, channel, args = [], timeoutMs = DEFAULT_TIMEOUT_MS }) {
  return new Promise((resolve, reject) => {
    const secure = url.protocol === 'wss:';
    const port = Number(url.port) || (secure ? 443 : 80);
    const host = url.hostname.replace(/^\[|\]$/g, '');
    const socket = secure
      ? tls.connect({ host, port, rejectUnauthorized: !insecure, ...(net.isIP(host) ? {} : { servername: host }) })
      : net.connect({ host, port });
    const key = crypto.randomBytes(16).toString('base64');
    const id = 1;
    let settled = false;
    let upgraded = false;
    let handshake = Buffer.alloc(0);
    let message = [];
    let messageBytes = 0;
    const finish = (error, value) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      if (error) { socket.destroy(); reject(error); return; }
      socket.end(clientFrame(OP.close, Buffer.alloc(0)), () => socket.destroy());
      resolve(value);
    };
    const timer = setTimeout(() => finish(new Error(`no reply to ${channel} within ${timeoutMs} ms (an unapproved device waits for approval, pass ?deviceToken= off loopback)`)), timeoutMs);
    const onMessage = text => {
      let frame;
      try { frame = JSON.parse(text); } catch { finish(new Error('server sent a frame that is not JSON')); return; }
      if (frame.t !== 'reply' || frame.id !== id) return; // pushed events and other traffic
      if (frame.error) finish(new Error(String(frame.error)));
      else finish(null, frame.result ?? null);
    };
    const read = frameReader(frame => {
      if (frame.opcode === OP.ping) socket.write(clientFrame(OP.pong, frame.payload));
      else if (frame.opcode === OP.close) finish(new Error('server closed the connection before replying'));
      else if (frame.opcode === OP.text || frame.opcode === OP.continuation) {
        message.push(frame.payload);
        messageBytes += frame.payload.length;
        if (messageBytes > MAX_MESSAGE_BYTES) { finish(new Error('reply is over the client limit')); return; }
        if (frame.fin) {
          const text = Buffer.concat(message, messageBytes).toString('utf8');
          message = [];
          messageBytes = 0;
          onMessage(text);
        }
      }
    });
    const feed = chunk => {
      try { read(chunk); } catch (error) { finish(error); }
    };
    socket.on(secure ? 'secureConnect' : 'connect', () => {
      socket.write([
        `GET ${url.pathname || '/'}${url.search} HTTP/1.1`,
        `Host: ${url.host}`,
        'Upgrade: websocket',
        'Connection: Upgrade',
        `Sec-WebSocket-Key: ${key}`,
        'Sec-WebSocket-Version: 13',
        '', '',
      ].join('\r\n'));
    });
    socket.on('data', chunk => {
      if (upgraded) { feed(chunk); return; }
      handshake = Buffer.concat([handshake, chunk]);
      const end = handshake.indexOf('\r\n\r\n');
      if (end < 0) return;
      const head = handshake.subarray(0, end).toString('latin1');
      const status = head.split('\r\n')[0];
      if (!/^HTTP\/1\.1 101\b/.test(status)) { finish(new Error(`websocket upgrade refused: ${status}`)); return; }
      const expected = crypto.createHash('sha1').update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest('base64');
      if (!new RegExp(`^sec-websocket-accept:\\s*${expected.replace(/[+/=]/g, '\\$&')}\\s*$`, 'im').test(head)) { finish(new Error('websocket upgrade had a wrong accept key')); return; }
      upgraded = true;
      socket.write(clientFrame(OP.text, Buffer.from(JSON.stringify({ t: 'invoke', id, channel, args }))));
      const rest = handshake.subarray(end + 4);
      handshake = null;
      if (rest.length) feed(rest);
    });
    socket.on('error', error => finish(new Error(error.code === 'DEPTH_ZERO_SELF_SIGNED_CERT' || error.code === 'SELF_SIGNED_CERT_IN_CHAIN' ? `${error.message} (a daemon's own certificate needs --insecure, loopback checks only)` : error.message)));
    socket.on('close', () => finish(new Error('connection closed before a reply')));
  });
}

async function main() {
  let options;
  try {
    options = parseArgs(process.argv.slice(2));
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    process.stderr.write(`${error.message}\n${USAGE}\n`);
    process.exitCode = 2;
    return;
  }
  try {
    const result = await wireCall(options);
    process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
  } catch (error) {
    // Messages never echo the URL, which can carry a device token.
    process.stderr.write(`wire-call: ${error.message}\n`);
    process.exitCode = 1;
  }
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === fs.realpathSync(fileURLToPath(import.meta.url))) await main();
