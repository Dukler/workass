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
// internal/toolcommand keeps the same string; a Go test holds them equal.
const USAGE = "workass tools guide\nworkass tools list [NAME | --full]\nworkass tools call NAME [--input FILE]\nlist shows each tool in one line, list NAME its schema, list --full every schema.\nCall arguments are one JSON object on stdin, or --input FILE, or inline JSON. Simple values also work as --key value. Mutations require operation_id; retry only with the same id and arguments.";
const FLAG_NAME_RE = /^[A-Za-z][A-Za-z0-9_-]*$/;

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

// A token after a flag is the next flag unless it is a negative number, which
// is a value: timeout_ms takes -1.
function looksLikeFlag(token) {
  return token.startsWith('--') || /^-[A-Za-z]/.test(token);
}

function looksLikeInlineJSON(value) {
  return /^[ \t\r\n]*\{/.test(value);
}

// Mirrors internal/toolcommand.parseCallFlags, held to it by
// tests/fixtures/tools-call-flags.json. --input reads as it always did; any other
// flag is one argument for the daemon to type against the tool's schema.
export function parseCallFlags(tokens) {
  const parsed = { input: '', inline: '', pairs: {}, help: false };
  const positional = [];
  let hasInput = false;
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (token === '--') { positional.push(...tokens.slice(index + 1)); break; }
    if (!token.startsWith('-') || token === '-') { positional.push(token); continue; }
    const body = token.replace(/^--?/, '');
    const equals = body.indexOf('=');
    const name = equals >= 0 ? body.slice(0, equals) : body;
    let value = equals >= 0 ? body.slice(equals + 1) : '';
    if (name === 'h' || name === 'help') { parsed.help = true; return parsed; }
    if (!FLAG_NAME_RE.test(name)) throw new ToolError('that is not a valid flag');
    const key = name.replaceAll('-', '_');
    if (key === 'context') throw new ToolError('--context is not needed: the process environment already names the context');
    if (key === 'input') {
      if (equals < 0) {
        if (index + 1 >= tokens.length) throw new ToolError('--input needs an argument');
        index += 1;
        value = tokens[index];
      }
      parsed.input = value;
      hasInput = true;
      continue;
    }
    if (equals < 0) {
      value = 'true';
      if (index + 1 < tokens.length && !looksLikeFlag(tokens[index + 1])) { index += 1; value = tokens[index]; }
    }
    if (Object.hasOwn(parsed.pairs, key)) throw new ToolError(`--${key} was passed more than once`);
    parsed.pairs[key] = value;
  }
  if (positional.length > 1 || (positional.length === 1 && !looksLikeInlineJSON(positional[0]))) throw new ToolError('unexpected call argument');
  if (positional.length === 1) parsed.inline = positional[0];
  if ([hasInput, parsed.inline !== '', Object.keys(parsed.pairs).length > 0].filter(Boolean).length > 1) {
    throw new ToolError('use one of --input, inline JSON or --key value, not both');
  }
  return parsed;
}

function callSyntaxHelp(name) {
  return `pass arguments as one JSON object: on stdin, with --input FILE, or inline as tools call ${name} --input '{"key":"value"}'; tools list ${name} shows its arguments`;
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
  try { info = lstatSync(path); } catch (err) {
    // Mirror internal/toolcli.ReadConfig: a nonexistent file means the chat
    // that launched this process is gone, which callers must state truthfully.
    if (err?.code === 'ENOENT') throw new ToolError('Workass context file does not exist');
    info = null;
  }
  if (!info || !info.isFile() || info.size > MAX_CONTEXT_BYTES) throw new ToolError('Workass context file is unavailable or invalid');
  let config;
  try { config = JSON.parse(readFileSync(path, 'utf8')); } catch { throw new ToolError('invalid Workass context file'); }
  if (config === null || typeof config !== 'object' || Array.isArray(config)) throw new ToolError('invalid Workass context file');
  const field = (name) => (typeof config[name] === 'string' ? config[name] : '');
  return { endpoint: field('endpoint'), caFile: field('ca_file'), credential: field('credential'), chatID: field('chat_id'), tabID: field('tab_id') };
}

export function doRequest(config, name, call, { signal, requestImpl = httpsRequest, query = {} } = {}) {
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
    // Flag values are the text the caller typed; the daemon holds the schema and
    // gives each its type. A daemon that predates this reads an ordinary call.
    if (call.fromFlags) requestPath += '?flags=1';
  }
  else {
    // Sorted like Go's url.Values.Encode, so both clients send the same request.
    const parameters = new URLSearchParams({ ...(name ? { name } : {}), ...query });
    parameters.sort();
    const encoded = parameters.toString();
    if (encoded) requestPath += `?${encoded}`;
  }
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
        if (reply.statusCode < 200 || reply.statusCode >= 300) {
          // Mirrors internal/toolcli.Response.Status: lets callers classify a
          // credential rejection without matching error-message text.
          response.status = reply.statusCode;
          if (!response.error) response.error = 'Workass tool request rejected';
        }
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

// Mirrors internal/toolcommand/run.go: state what actually happened to this
// process's context, without ever echoing the private context file path.
function readContext(contextFile) {
  if (!contextFile || !String(contextFile).trim()) throw new ToolError('WORKASS_TOOL_CONTEXT is not set; this process was not launched by a Workass chat engine');
  try { return readConfig(contextFile); }
  catch (err) {
    if (err.message === 'Workass context file does not exist') {
      throw new ToolError('Workass context file does not exist; the chat that launched this process was deleted, the daemon cleaned up a context left over from before a restart, or a command deleted the file (for example a one-shot run credential). For delayed work, ask the chat to use workass_schedule_message instead of a detached helper');
    }
    throw new ToolError(`${err.message}; use the current process WORKASS_TOOL_CONTEXT, not a context path copied from conversation history`);
  }
}

// A command line that could not be read: the error carries a working example for
// that tool, which only the daemon can write. This is the one request this run
// makes, and nothing about the call has been sent.
async function explainCallSyntax(contextFile, name, mistake, options) {
  let help = callSyntaxHelp(name);
  try {
    const response = await doRequest(readContext(contextFile), name, null, { ...options, query: { usage: '1' } });
    if (!response.error && typeof response.result?.usage === 'string' && response.result.usage) help = response.result.usage;
  } catch { /* the plain explanation stands */ }
  return new ToolError(redactSensitiveText(`${mistake.message}. ${help}`));
}

function readCallBytes(callFlags, readStdin) {
  if (callFlags.inline) return Buffer.from(callFlags.inline, 'utf8');
  if (callFlags.input) {
    let fd;
    try { fd = openSync(callFlags.input, 'r'); }
    catch {
      if (looksLikeInlineJSON(callFlags.input)) return Buffer.from(callFlags.input, 'utf8');
      if (callFlags.input !== '-') throw new ToolError('cannot open tool arguments file; --input takes a file path or one inline JSON object, and stdin works too');
    }
    if (fd !== undefined) {
      try { return readBounded(fd, MAX_ARGUMENT_BYTES); }
      catch { throw new ToolError('tool arguments are unreadable or exceed 4 MiB'); }
      finally { closeSync(fd); }
    }
  }
  try { return readStdin(); } catch { throw new ToolError('tool arguments are unreadable or exceed 4 MiB'); }
}

export async function run(args, { env = process.env, stdout = process.stdout, stderr = process.stderr, readStdin = () => readBounded(0, MAX_ARGUMENT_BYTES), signal, requestImpl } = {}) {
  const parsed = parseFlags(args, ['context']);
  if (parsed.help) { stderr.write(`${USAGE}\n`); return; }
  const contextFile = parsed.values.context ?? env.WORKASS_TOOL_CONTEXT ?? '';
  const rest = parsed.rest;
  if (rest.length === 0) { stderr.write(`${USAGE}\n`); throw new ToolError('a tools command is required'); }
  let name = '';
  let call = null;
  const query = {};
  switch (rest[0]) {
    case 'guide': {
      let guide;
      try { guide = readFileSync(String(env.WORKASS_TOOLS_GUIDE || '')); } catch { throw new ToolError('Workass guide is unavailable in this process'); }
      // The guide is what was asked for, whatever else was typed after it.
      if (rest.length > 1) stderr.write("guide takes no arguments; tools list NAME shows one tool's schema\n");
      stdout.write(guide); return;
    }
    case 'list':
      for (const argument of rest.slice(1)) {
        if (argument === '--full' || argument === '-full') query.full = '1';
        else if (argument.startsWith('-')) throw new ToolError('list accepts one exact tool name or --full');
        else if (name) throw new ToolError('list accepts at most one exact tool name');
        else name = argument;
      }
      if (name) delete query.full;
      break;
    case 'call': {
      if (rest.length < 2) throw new ToolError('call requires an exact tool name');
      name = rest[1];
      if (name.startsWith('-')) throw new ToolError('call takes the exact tool name first: tools call NAME --key value');
      let callFlags;
      try { callFlags = parseCallFlags(rest.slice(2)); }
      catch (err) {
        if (!(err instanceof ToolError)) throw err;
        throw await explainCallSyntax(contextFile, name, err, { signal, requestImpl });
      }
      if (callFlags.help) { stderr.write(`${USAGE}\n`); return; }
      if (Object.keys(callFlags.pairs).length > 0) {
        call = { name, arguments: callFlags.pairs, fromFlags: true };
        break;
      }
      const raw = readCallBytes(callFlags, readStdin);
      call = { name, arguments: parseArguments(raw), rawArguments: rawArgumentObject(raw) };
      break;
    }
    default: throw new ToolError('unknown tools command: use guide, list or call');
  }
  const config = readContext(contextFile);
  const response = await doRequest(config, name, call, { signal, requestImpl, query });
  if (response.error) {
    if (response.status === 401) {
      // A 401 means either the chat was deleted or this context's credential
      // belonged to an engine (or an old pre-chat-token context) that a spare
      // engine or daemon restart has since stopped.
      throw new ToolError('Workass rejected this context\'s credential: the chat that launched this process was deleted, or the context was issued by an engine that has since stopped. For delayed work, ask the chat to use workass_schedule_message instead of a detached helper');
    }
    throw new ToolError(redactSensitiveText(response.error));
  }
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
