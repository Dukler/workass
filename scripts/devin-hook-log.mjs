#!/usr/bin/env node
/* Log real Devin lifecycle/tool hook events for the Work Assistant console.
   The target log file is provided per spawned Devin process via ASSISTANT_DEVIN_HOOK_LOG. */

import fs from 'node:fs';

const logPath = process.env.ASSISTANT_DEVIN_HOOK_LOG;
if (!logPath) process.exit(0);

function readStdin() {
  return new Promise((resolve) => {
    let body = '';
    process.stdin.setEncoding('utf8');
    process.stdin.on('data', (chunk) => { body += chunk; });
    process.stdin.on('end', () => resolve(body));
  });
}

function redact(value) {
  return String(value || '')
    .replace(/Bearer\s+[A-Za-z0-9._~+/=-]+/gi, 'Bearer [REDACTED]')
    .replace(/(authorization|cookie|token|password|secret|api[-_]?key)\s*[:=]\s*[^\s"']+/gi, '$1=[REDACTED]')
    .replace(/\s+/g, ' ')
    .trim();
}

function short(value, max = 180) {
  const text = redact(value);
  return text.length > max ? `${text.slice(0, max - 1)}...` : text;
}

function describeToolInput(toolName, input = {}) {
  if (!input || typeof input !== 'object') return '';
  if (input.command) return short(input.command);
  if (input.file_path) return short(input.file_path);
  if (input.path) return short(input.path);
  if (input.pattern) return short(input.pattern);
  if (input.query) return short(input.query);
  if (input.server_name || input.tool_name) return short(`${input.server_name || ''}${input.tool_name ? `.${input.tool_name}` : ''}`);
  if (toolName?.startsWith('mcp__')) return short(JSON.stringify(input));
  const keys = Object.keys(input).slice(0, 4);
  return keys.length ? short(keys.map((key) => `${key}=${JSON.stringify(input[key])}`).join(' ')) : '';
}

function responseExcerpt(response = {}) {
  if (!response || typeof response !== 'object') return '';
  const raw = response.error || response.output || response.stdout || response.stderr || response.text || response.result;
  if (raw == null) return '';
  let text = typeof raw === 'string' ? raw : JSON.stringify(raw);
  text = text
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .slice(0, 3)
    .join(' | ');
  return short(text, 320);
}

function lineFor(event) {
  const name = event.hook_event_name || event.event || 'Hook';
  const at = new Date().toLocaleTimeString('es-AR', { hour12: false });
  if (name === 'SessionStart') return `[${at}] Devin session started${event.source ? ` (${event.source})` : ''}`;
  if (name === 'UserPromptSubmit') return `[${at}] Prompt submitted to Devin`;
  if (name === 'PermissionRequest') {
    const detail = describeToolInput(event.tool_name, event.tool_input);
    return `[${at}] Permission requested: ${event.tool_name || 'tool'}${detail ? ` - ${detail}` : ''}`;
  }
  if (name === 'PreToolUse') {
    const detail = describeToolInput(event.tool_name, event.tool_input);
    return `[${at}] Tool start: ${event.tool_name || 'tool'}${detail ? ` - ${detail}` : ''}`;
  }
  if (name === 'PostToolUse') {
    const response = event.tool_response || {};
    const status = response.success === false ? 'failed' : 'done';
    const excerpt = responseExcerpt(response);
    const extra = excerpt ? ` - result: ${excerpt}` : '';
    return `[${at}] Tool ${status}: ${event.tool_name || 'tool'}${extra}`;
  }
  if (name === 'Stop') return `[${at}] Devin is preparing final response`;
  if (name === 'SessionEnd') return `[${at}] Devin session ended${event.reason ? ` (${event.reason})` : ''}`;
  return `[${at}] ${name}`;
}

try {
  const raw = await readStdin();
  const event = raw.trim() ? JSON.parse(raw) : {};
  fs.appendFileSync(logPath, `${lineFor(event)}\n`, 'utf8');
} catch (error) {
  try {
    fs.appendFileSync(logPath, `[${new Date().toLocaleTimeString('es-AR', { hour12: false })}] Hook log error: ${short(error.message || error)}\n`, 'utf8');
  } catch {
    // Never fail the Devin hook because telemetry logging failed.
  }
}
