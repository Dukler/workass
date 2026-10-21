import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { PAGE_COV, PAGE_EVIL } from '../../desktop/acp/html-pages.mjs';

// The tools CLI the daemon hands a provider, faked: it records the page each
// call delivers (inline, or read from the path it was given, while the call
// runs) and fails while a `fail` file sits beside it.
const fakeTools = `#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
const here = path.dirname(new URL(import.meta.url).pathname);
const argv = process.argv.slice(2);
const at = argv.indexOf('--input');
const args = JSON.parse(at >= 0 ? fs.readFileSync(argv[at + 1], 'utf8') : fs.readFileSync(0, 'utf8'));
const page = typeof args.html === 'string' ? args.html : fs.readFileSync(args.path, 'utf8');
fs.appendFileSync(path.join(here, 'calls.jsonl'), JSON.stringify({ argv, title: args.title, operation: args.operation_id, page }) + '\\n');
if (fs.existsSync(path.join(here, 'fail'))) {
  process.stderr.write('render refused\\n');
  process.exit(1);
}
process.stdout.write(JSON.stringify({ htmlRender: { id: 'page-' + args.operation_id, title: args.title } }) + '\\n');
`;

test('mock render-html publishes its page and leaves nothing in the chat working directory', { skip: process.platform === 'win32' && 'the fake tools CLI is a shebang script' }, async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), 'workass-mock-render-html-test-'));
  const workingDir = path.join(temporary, 'chat');
  await fs.mkdir(workingDir);
  const tools = path.join(temporary, 'workass-tools.mjs');
  await fs.writeFile(tools, fakeTools, { mode: 0o755 });

  const child = spawn(process.execPath, [fileURLToPath(new URL('../../desktop/acp/mock-server.mjs', import.meta.url))], {
    cwd: temporary,
    env: { ...process.env, WORKASS_MOCK_ACP_DELAY_MS: '0', WORKASS_TOOLS_COMMAND: tools },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  const exited = once(child, 'exit');
  child.stderr.resume();
  const lines = createInterface({ input: child.stdout });
  const pending = new Map();
  const updates = [];
  let sequence = 0;
  lines.on('line', line => {
    const frame = JSON.parse(line);
    if (frame.method === 'session/update') {
      updates.push(frame.params.update);
      return;
    }
    const waiter = pending.get(frame.id);
    if (!waiter) return;
    pending.delete(frame.id);
    clearTimeout(waiter.timer);
    if (frame.error) waiter.reject(new Error(frame.error.message)); else waiter.resolve(frame.result);
  });
  const request = (method, params) => new Promise((resolve, reject) => {
    const id = ++sequence;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(`${method} did not settle`)); }, 10_000);
    pending.set(id, { resolve, reject, timer });
    child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', id, method, params })}\n`);
  });
  const calls = async () => (await fs.readFile(path.join(temporary, 'calls.jsonl'), 'utf8').catch(() => ''))
    .split('\n').filter(Boolean).map(line => JSON.parse(line));
  const renderRow = () => updates.filter(update => update.sessionUpdate === 'tool_call_update' && String(update.toolCallId).endsWith('-render')).at(-1);

  try {
    await request('initialize', { protocolVersion: 1, clientCapabilities: {} });
    const session = await request('session/new', { cwd: workingDir });
    const turn = async (text) => {
      const result = await request('session/prompt', { sessionId: session.sessionId, prompt: [{ type: 'text', text }] });
      assert.equal(result.stopReason, 'end_turn');
      assert.deepEqual(await fs.readdir(workingDir), [], `${text} left files in the chat working directory`);
    };

    await turn('[mock:render-html] publish the coverage');
    assert.equal(renderRow().status, 'completed');
    let recorded = await calls();
    assert.equal(recorded.length, 1);
    assert.equal(recorded[0].title, 'Cobertura por paquete');
    assert.equal(recorded[0].page, PAGE_COV);
    assert.ok(recorded[0].operation);

    await turn('[mock:render-html] [mock:hostile] publish the hostile page');
    recorded = await calls();
    assert.equal(recorded.length, 2);
    assert.equal(recorded[1].page, PAGE_EVIL);

    // A call that fails leaves nothing behind either.
    await fs.writeFile(path.join(temporary, 'fail'), '');
    await turn('[mock:render-html] publish again');
    assert.equal(renderRow().status, 'failed');
    assert.equal((await calls()).length, 3);
  } finally {
    for (const waiter of pending.values()) clearTimeout(waiter.timer);
    child.kill();
    await exited;
    lines.close();
    await fs.rm(temporary, { recursive: true, force: true });
  }
});
