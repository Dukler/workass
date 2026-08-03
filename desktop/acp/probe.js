'use strict';

const { spawn } = require('node:child_process');

function stopChild(child) {
  if (!child || child.killed) return;
  try { child.kill(); } catch { /* ignore */ }
}

function probeAcpServer({ command, args = [], cwd, env = {}, shell = false, timeoutMs = 5000, protocolVersion = 1 } = {}) {
  if (!command) return Promise.resolve({ ok: false, error: 'ACP probe requires a command.' });
  const startedAt = Date.now();

  return new Promise((resolve) => {
    let settled = false;
    let buffer = '';
    let stderr = '';
    let child;

    const finish = (result) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      stopChild(child);
      resolve({
        command,
        args: args.map(String),
        latencyMs: Date.now() - startedAt,
        ...result,
        ...(stderr ? { stderr: stderr.slice(-4000) } : {}),
      });
    };

    const timer = setTimeout(() => finish({ ok: false, error: `ACP initialize timed out after ${timeoutMs}ms.` }), timeoutMs);
    if (timer.unref) timer.unref();

    try {
      child = spawn(command, args.map(String), {
        cwd,
        env: { ...process.env, ...env },
        windowsHide: true,
        shell: !!shell,
        stdio: ['pipe', 'pipe', 'pipe'],
      });
    } catch (err) {
      finish({ ok: false, error: err.message || String(err) });
      return;
    }

    child.on('error', (err) => finish({ ok: false, error: err.message || String(err) }));
    child.on('exit', (code, signal) => {
      if (!settled) finish({ ok: false, error: `ACP process exited before initialize (${signal || code}).` });
    });
    child.stderr.on('data', (chunk) => { stderr = (stderr + chunk.toString()).slice(-4000); });
    child.stdout.on('data', (chunk) => {
      buffer += chunk.toString();
      let newline;
      while ((newline = buffer.indexOf('\n')) !== -1) {
        const line = buffer.slice(0, newline).trim();
        buffer = buffer.slice(newline + 1);
        if (!line) continue;
        let message;
        try { message = JSON.parse(line); } catch { continue; }
        if (message.id !== 1) continue;
        if (message.error) {
          finish({ ok: false, error: message.error.message || `ACP error ${message.error.code}` });
          return;
        }
        const result = message.result || {};
        finish({
          ok: true,
          protocolVersion: result.protocolVersion ?? protocolVersion,
          agentInfo: result.agentInfo || null,
          agentCapabilities: result.agentCapabilities || {},
          authMethods: Array.isArray(result.authMethods) ? result.authMethods : [],
        });
        return;
      }
    });

    try {
      child.stdin.write(`${JSON.stringify({
        jsonrpc: '2.0',
        id: 1,
        method: 'initialize',
        params: {
          protocolVersion,
          clientInfo: { name: 'Workass ACP Probe', version: '0.1.0' },
          clientCapabilities: { auth: { terminal: false }, fs: { readTextFile: false, writeTextFile: false }, terminal: false },
        },
      })}\n`);
    } catch (err) {
      finish({ ok: false, error: err.message || String(err) });
    }
  });
}

module.exports = { probeAcpServer };
