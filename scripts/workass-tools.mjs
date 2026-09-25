#!/usr/bin/env node
// Windows Workass tools client. Invoked through workass-tools.cmd with the
// bundled, OpenJS-signed Node runtime. It mirrors internal/toolcommand and
// internal/toolcli: one authenticated request over pinned loopback HTTPS.
import { Buffer } from 'node:buffer';
import { randomBytes } from 'node:crypto';
import { closeSync, lstatSync, openSync, readFileSync, readSync, realpathSync, writeFileSync } from 'node:fs';
import { request as httpsRequest } from 'node:https';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

const MAX_ARGUMENT_BYTES = 4 * 1024 * 1024;
const MAX_CONTEXT_BYTES = 64 * 1024;
export const MAX_RESPONSE_BYTES = 32 * 1024 * 1024;
const CONNECT_TIMEOUT_MS = 10_000;
const ENDPOINT_RE = /^https:\/\/tools\.localhost:([0-9]{1,5})\/workass\/tools$/;
const SECRET_TEXT_RE = /(bearer\s+)[A-Za-z0-9._~+/=-]+|((?:api[_-]?key|token|secret|password|credential)\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}]+)/gi;
const USAGE = 'workass tools --context FILE list [NAME]\nworkass tools --context FILE call NAME [--input FILE]\nCall arguments are one JSON object read from stdin, or --input FILE. Mutations require operation_id; retry only with the same id and arguments.';

export class ToolError extends Error {}

export function redactSensitiveText(text) {
  return String(text).replace(SECRET_TEXT_RE, (match, bearerPrefix, secretPrefix) => {
    if (bearerPrefix) return `${bearerPrefix}[redacted]`;
    if (secretPrefix) return `${secretPrefix}[redacted]`;
    return '[redacted]';
  });
}

// Go's flag package accepts -name value, --name value, and equals forms.
function parseFlags(args, known) {
  const values = {};
  let index = 0;
  while (index < args.length) {
    const arg = args[index];
    if (arg === '--') { index += 1; break; }
    if (!arg.startsWith('-') || arg === '-') break;
    const body = arg.replace(/^--?/, '');
    if (body === 'h' || body === 'help') return { help: true };
    const equals = body.indexOf('=');
    const name = equals >= 0 ? body.slice(0, equals) : body;
    if (!known.includes(name)) throw new ToolError(`flag provided but not defined: -${name}`);
    if (equals >= 0) { values[name] = body.slice(equals + 1); index += 1; }
    else {
      if (index + 1 >= args.length) throw new ToolError(`flag needs an argument: -${name}`);
      values[name] = args[index + 1]; index += 2;
    }
  }
  return { values, rest: args.slice(index) };
}

function readBounded(fd, limit) {
  const chunks = [];
  let total = 0;
  const chunk = Buffer.allocUnsafe(64 * 1024);
  for (;;) {
    const count = readSync(fd, chunk, 0, chunk.length, null);
    if (count === 0) break;
    total += count;
    if (total > limit) throw new ToolError('tool arguments are unreadable or exceed 4 MiB');
    chunks.push(Buffer.from(chunk.subarray(0, count)));
  }
  return Buffer.concat(chunks, total);
}

export function parseArguments(raw) {
  if (raw.length > MAX_ARGUMENT_BYTES) throw new ToolError('tool arguments are unreadable or exceed 4 MiB');
  const bytes = raw.subarray(0, 3).equals(Buffer.from([0xef, 0xbb, 0xbf])) ? raw.subarray(3) : raw;
  let value;
  try { value = JSON.parse(bytes.toString('utf8')); } catch {
    throw new ToolError('tool arguments must be a JSON object (use {} for no arguments)');
  }
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new ToolError('tool arguments must be a JSON object (use {} for no arguments)');
  }
  return value;
}

function rawArgumentObject(raw) {
  let start = raw.subarray(0, 3).equals(Buffer.from([0xef, 0xbb, 0xbf])) ? 3 : 0;
  let end = raw.length;
  const whitespace = (byte) => byte === 0x20 || byte === 0x09 || byte === 0x0a || byte === 0x0d;
  while (start < end && whitespace(raw[start])) start += 1;
  while (end > start && whitespace(raw[end - 1])) end -= 1;
  return raw.subarray(start, end);
}

export function readConfig(path) {
  let info;
  try { info = lstatSync(path); } catch { info = null; }
  if (!info || !info.isFile() || info.size > MAX_CONTEXT_BYTES) throw new ToolError('Workass context file is unavailable or invalid');
  let config;
  try { config = JSON.parse(readFileSync(path, 'utf8')); } catch { throw new ToolError('invalid Workass context file'); }
  if (config === null || typeof config !== 'object' || Array.isArray(config)) throw new ToolError('invalid Workass context file');
  const field = (name) => (typeof config[name] === 'string' ? config[name] : '');
  return { endpoint: field('endpoint'), caFile: field('ca_file'), credential: field('credential'), chatID: field('chat_id'), tabID: field('tab_id') };
}

export function doRequest(config, name, call, { signal, requestImpl = httpsRequest } = {}) {
  const match = ENDPOINT_RE.exec(config.endpoint);
  const port = match ? Number(match[1]) : 0;
  if (!match || port < 1 || port > 65535) return Promise.reject(new ToolError('invalid Workass tool endpoint'));
  if (!config.credential || /[\r\n]/.test(config.credential) || !config.chatID || !config.tabID) return Promise.reject(new ToolError('Workass session context is incomplete'));
  let ca;
  try { ca = readFileSync(config.caFile, 'utf8'); } catch { return Promise.reject(new ToolError('cannot read Workass public certificate')); }
  if (!/-----BEGIN CERTIFICATE-----/.test(ca)) return Promise.reject(new ToolError('invalid Workass public certificate'));
  let requestPath = '/workass/tools';
  let body = null;
  if (call) {
    // Keep JSON number lexemes intact. JSON.parse rounds integers above 2^53,
    // while the Go client preserves them with json.Number.
    const argumentsJSON = Buffer.isBuffer(call.rawArguments)
      ? call.rawArguments
      : Buffer.from(JSON.stringify(call.arguments), 'utf8');
    body = Buffer.concat([
      Buffer.from(`{"name":${JSON.stringify(call.name)},"arguments":`, 'utf8'),
      argumentsJSON,
      Buffer.from('}', 'utf8'),
    ]);
  }
  else if (name) requestPath += `?${new URLSearchParams({ name }).toString()}`;
  const headers = {
    Host: `tools.localhost:${port}`,
    Authorization: `Bearer ${config.credential}`,
    'X-Workass-Chat-ID': config.chatID,
    'X-Workass-Tab-ID': config.tabID,
    'Content-Type': 'application/json',
  };
  if (body) headers['Content-Length'] = String(body.length);
  return new Promise((resolve, reject) => {
    let settled = false;
    let connectTimer;
    const fail = (message) => {
      if (settled) return;
      settled = true;
      clearTimeout(connectTimer);
      reject(new ToolError(message));
    };
    const req = requestImpl({
      host: '127.0.0.1', port, path: requestPath, method: body ? 'POST' : 'GET', headers,
      servername: 'tools.localhost', ca, minVersion: 'TLSv1.3', agent: false, signal,
    }, (reply) => {
      clearTimeout(connectTimer);
      if (reply.statusCode >= 300 && reply.statusCode < 400) {
        reply.resume(); fail('Workass tool connection failed or was cancelled'); return;
      }
      const chunks = [];
      let total = 0;
      reply.on('data', (chunk) => {
        total += chunk.length;
        if (total > MAX_RESPONSE_BYTES) { reply.destroy(); fail('Workass tool response is unreadable or too large'); return; }
        chunks.push(chunk);
      });
      reply.on('error', () => fail('Workass tool response is unreadable or too large'));
      reply.on('end', () => {
        if (settled) return;
        let response;
        try { response = JSON.parse(Buffer.concat(chunks, total).toString('utf8')); } catch { response = undefined; }
        if (response === null || typeof response !== 'object' || Array.isArray(response)) { fail('invalid Workass tool response'); return; }
        if (typeof response.error !== 'string') response.error = '';
        if ((reply.statusCode < 200 || reply.statusCode >= 300) && !response.error) response.error = 'Workass tool request rejected';
        settled = true;
        resolve(response);
      });
    });
    connectTimer = setTimeout(() => { req.destroy(); fail('Workass tool connection failed or was cancelled'); }, CONNECT_TIMEOUT_MS);
    req.on('socket', (socket) => socket.once('secureConnect', () => clearTimeout(connectTimer)));
    req.on('error', () => fail('Workass tool connection failed or was cancelled'));
    req.end(body || undefined);
  });
}

const IMAGE_EXTENSIONS = { 'image/png': '.png', 'image/jpeg': '.jpg', 'image/webp': '.webp' };
export function materializeImages(result, images, contextFile) {
  const saved = [];
  for (const image of images) {
    const ext = IMAGE_EXTENSIONS[image?.mime_type];
    if (!ext) throw new ToolError('unsupported Workass image type');
    if (typeof image.data !== 'string' || !/^[A-Za-z0-9+/]*={0,2}$/.test(image.data) || image.data.length % 4 !== 0) throw new ToolError('invalid Workass image bytes');
    const data = Buffer.from(image.data, 'base64');
    let file = '';
    for (let attempt = 0; attempt < 16 && !file; attempt += 1) {
      const candidate = join(dirname(contextFile), `tool-image-${randomBytes(6).readUIntBE(0, 6)}${ext}`);
      try { writeFileSync(candidate, data, { flag: 'wx', mode: 0o600 }); file = candidate; }
      catch (err) { if (err?.code !== 'EEXIST') throw new ToolError('cannot save Workass tool image'); }
    }
    if (!file) throw new ToolError('cannot save Workass tool image');
    saved.push({ mime_type: image.mime_type, path: file });
  }
  return { images: saved, result };
}

export async function run(args, { env = process.env, stdout = process.stdout, stderr = process.stderr, readStdin = () => readBounded(0, MAX_ARGUMENT_BYTES), signal, requestImpl } = {}) {
  const parsed = parseFlags(args, ['context']);
  if (parsed.help) { stderr.write(`${USAGE}\n`); return; }
  const contextFile = parsed.values.context ?? env.WORKASS_TOOL_CONTEXT ?? '';
  const rest = parsed.rest;
  if (rest.length === 0) { stderr.write(`${USAGE}\n`); throw new ToolError('a tools command is required'); }
  let name = '';
  let call = null;
  switch (rest[0]) {
    case 'guide': {
      if (rest.length !== 1) throw new ToolError('guide accepts no arguments');
      let guide;
      try { guide = readFileSync(String(env.WORKASS_TOOLS_GUIDE || '')); } catch { throw new ToolError('Workass guide is unavailable in this process'); }
      stdout.write(guide); return;
    }
    case 'list':
      if (rest.length > 2) throw new ToolError('list accepts at most one exact tool name');
      if (rest.length === 2) name = rest[1];
      break;
    case 'call': {
      if (rest.length < 2) throw new ToolError('call requires an exact tool name');
      name = rest[1];
      const callFlags = parseFlags(rest.slice(2), ['input']);
      if (callFlags.help) { stderr.write('Usage of call:\n  -input string\n    \tJSON arguments file; stdin by default\n'); return; }
      if (callFlags.rest.length !== 0) throw new ToolError('unexpected call argument');
      let raw;
      if (callFlags.values.input) {
        let fd;
        try { fd = openSync(callFlags.values.input, 'r'); } catch { throw new ToolError('cannot open tool arguments file'); }
        try { raw = readBounded(fd, MAX_ARGUMENT_BYTES); }
        catch { throw new ToolError('tool arguments are unreadable or exceed 4 MiB'); }
        finally { closeSync(fd); }
      } else {
        try { raw = readStdin(); } catch { throw new ToolError('tool arguments are unreadable or exceed 4 MiB'); }
      }
      call = { name, arguments: parseArguments(raw), rawArguments: rawArgumentObject(raw) };
      break;
    }
    default: throw new ToolError('unknown tools command: use list or call');
  }
  let config;
  try { config = readConfig(contextFile); }
  catch (err) { throw new ToolError(`${err.message}; use the current process WORKASS_TOOL_CONTEXT, not a context path copied from conversation history`); }
  const response = await doRequest(config, name, call, { signal, requestImpl });
  if (response.error) throw new ToolError(redactSensitiveText(response.error));
  let result = response.result ?? null;
  if (Array.isArray(response.images) && response.images.length > 0) result = materializeImages(result, response.images, contextFile);
  stdout.write(`${JSON.stringify(result)}\n`);
}

async function main() {
  const argv = process.argv.slice(2);
  if (argv[0] !== 'tools') {
    process.stderr.write(`${JSON.stringify({ error: 'a leading tools command is required' })}\n`);
    process.exitCode = 2; return;
  }
  const controller = new AbortController();
  const abort = () => controller.abort();
  process.once('SIGINT', abort);
  process.once('SIGTERM', abort);
  try { await run(argv.slice(1), { signal: controller.signal }); }
  catch (err) {
    const message = err instanceof ToolError ? err.message : 'Workass tools client failed';
    process.stderr.write(`${JSON.stringify({ error: redactSensitiveText(message) })}\n`);
    process.exitCode = 1;
  } finally {
    process.removeListener('SIGINT', abort);
    process.removeListener('SIGTERM', abort);
  }
}
const invokedDirectly = (() => {
  try {
    const canonical = (file) => { const resolved = realpathSync(file); return process.platform === 'win32' ? resolved.toLowerCase() : resolved; };
    return Boolean(process.argv[1]) && canonical(process.argv[1]) === canonical(fileURLToPath(import.meta.url));
  } catch { return false; }
})();
if (invokedDirectly) await main();
