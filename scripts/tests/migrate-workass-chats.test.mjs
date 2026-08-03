import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const script = path.join(root, 'scripts', 'migrate-workass-chats.mjs');

function fixture() {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-chat-migration-'));
  const source = path.join(temp, 'source');
  const dest = path.join(temp, 'prod');
  fs.mkdirSync(path.join(source, 'chat-archive'), { recursive: true });
  fs.mkdirSync(path.join(source, 'checkpoints'), { recursive: true });
  fs.writeFileSync(path.join(source, 'session-state.json'), JSON.stringify({
    v: 1, activeId: 'tab-a', chats: [{ id: 'tab-a', chatId: 'chat-a', providerId: 'codex', messages: [
      { id: 'u-1', role: 'user', content: 'hello' }, { id: 'a-1', role: 'assistant', content: 'hi' },
    ] }],
  }));
  fs.writeFileSync(path.join(source, 'native-sessions.json'), JSON.stringify({ v: 2, bindings: [
    { tabId: 'tab-a', chatId: 'chat-a', providerId: 'codex', sessionId: 'native-secret', generation: 2 },
  ] }));
  fs.writeFileSync(path.join(source, 'chat-archive', 'tab-a.jsonl'), `${JSON.stringify({ id: 'u-1', role: 'user', content: 'hello' })}\n`);
  fs.writeFileSync(path.join(source, 'checkpoints', 'chat-a.json'), '{}');
  fs.writeFileSync(path.join(source, 'checkpoints', 'test-fixture.json'), '{}');
  fs.writeFileSync(path.join(source, 'agent-control.json'), '{"must":"not migrate"}');
  return { temp, source, dest };
}

test('migrates only canonical chat/session data with hashed native identities', (t) => {
  const f = fixture();
  t.after(() => fs.rmSync(f.temp, { recursive: true, force: true }));
  const output = execFileSync(process.execPath, [script, '--source-state', f.source, '--dest-root', f.dest], { encoding: 'utf8' });
  assert.match(output, /CHAT_MIGRATION_COMPLETE/);
  assert.ok(fs.existsSync(path.join(f.dest, 'state', 'session-state.json')));
  assert.ok(fs.existsSync(path.join(f.dest, 'state', 'native-sessions.json')));
  assert.ok(fs.existsSync(path.join(f.dest, 'state', 'chat-archive', 'tab-a.jsonl')));
  assert.ok(fs.existsSync(path.join(f.dest, 'state', 'checkpoints', 'chat-a.json')));
  assert.ok(!fs.existsSync(path.join(f.dest, 'state', 'checkpoints', 'test-fixture.json')));
  assert.ok(!fs.existsSync(path.join(f.dest, 'state', 'agent-control.json')));
  const manifest = JSON.parse(fs.readFileSync(path.join(f.dest, 'state', 'migration-manifest.json')));
  assert.equal(manifest.chatCount, 1);
  assert.equal(manifest.nativeBindingCount, 1);
  assert.doesNotMatch(JSON.stringify(manifest), /native-secret/);
});

test('rejects a provider-native session owned by the wrong chat', (t) => {
  const f = fixture();
  t.after(() => fs.rmSync(f.temp, { recursive: true, force: true }));
  const ledger = JSON.parse(fs.readFileSync(path.join(f.source, 'native-sessions.json')));
  ledger.bindings[0].chatId = 'chat-b';
  fs.writeFileSync(path.join(f.source, 'native-sessions.json'), JSON.stringify(ledger));
  const result = spawnSync(process.execPath, [script, '--source-state', f.source, '--dest-root', f.dest], { encoding: 'utf8' });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /does not belong to authoritative chat/);
  assert.ok(!fs.existsSync(path.join(f.dest, 'state')));
});
