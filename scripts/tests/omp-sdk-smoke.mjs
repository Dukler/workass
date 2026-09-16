#!/usr/bin/env node
// Real OMP SDK smoke probe. It creates no model turns and isolates all state.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

if (!process.argv[2]) throw new Error('usage: scripts/tests/omp-sdk-smoke.mjs <frontier-hosts-target-dir>');
const bundle = path.resolve(process.argv[2]);
const bun = process.execPath;
const sdk = path.join(bundle, 'node_modules/@oh-my-pi/pi-coding-agent/src/index.ts');
const temp = await fs.mkdtemp(path.join(os.tmpdir(), 'workass-omp-smoke-'));
const agentDir = path.join(temp, 'agent');
const cwd = path.join(temp, 'workspace');
await fs.mkdir(cwd);
process.env.HOME = temp;
process.env.USERPROFILE = temp;
process.env.PI_CODING_AGENT_DIR = agentDir;
process.env.OMP_AUTH_BROKER_URL = '';

process.env.WORKASS_OMP_SDK_MODULE = sdk;
const { createAgentSession, AgentRegistry, SessionManager } =
  await import(`file://${sdk}`);
const options = (dir, appendSystemPrompt) => ({
  cwd, agentDir: dir, appendSystemPrompt, agentRegistry: new AgentRegistry(), disableExtensionDiscovery: true,
  enableMCP: false, enableLsp: false, enableIrc: false,
});

const first = await createAgentSession(options(path.join(temp, 'one'), 'WORKASS_WORKASS_APPEND_PROBE'));
const second = await createAgentSession(options(path.join(temp, 'two'), 'WORKASS_SECOND_APPEND_PROBE'));
assert.equal(first.session.systemPrompt.some((block) => block.includes('WORKASS_WORKASS_APPEND_PROBE')), true);
assert.equal(second.session.systemPrompt.some((block) => block.includes('WORKASS_SECOND_APPEND_PROBE')), true);
assert.ok(first.session.modelRegistry.getAll().length >= 1, 'model catalog getAll is empty');
assert.ok(Array.isArray(first.session.modelRegistry.getAvailable()), 'model catalog getAvailable failed');
for (const mode of ['always-ask','write','yolo']) {
  first.session.settings.override('tools.approvalMode', mode);
  assert.equal(first.session.settings.get('tools.approvalMode'),mode);
  assert.equal(first.session.settings.isConfigured('tools.approvalMode'),true);
  first.session.setClientBridge({capabilities:{requestPermission:true},requestPermission:async()=>({outcome:'cancelled'})});
}
first.session.settings.clearOverride('tools.approvalMode');
assert.equal(typeof first.session.preparePlanForReview,'function');
assert.ok(first.session.getAllToolNames().includes('task'),'native subagent task tool missing');


const registry = new AgentRegistry();
registry.register({ id: 'smoke-one', displayName: 'Smoke One', kind: 'main', session: first.session, sessionFile: first.session.sessionFile });
registry.register({ id: 'smoke-two', displayName: 'Smoke Two', kind: 'sub', session: second.session, sessionFile: second.session.sessionFile });
assert.equal(registry.list().length, 2);

// Execute harmless tools through the REAL SDK approval wrappers, without a model.
const {OmpSession} = await import('../omp-native-host.mjs');
const host = new OmpSession('',cwd,false,{});
await host.start();
let approvals = 0;
host.ui.select = async () => { approvals++; return 'Approve'; };
const native = host.native;
const context = {settings:native.settings};
const writeTool = native.getToolByName('write');
assert.ok(writeTool,'native write tool missing');
for (const [mode,expected] of [['always-ask',1],['write',0],['yolo',0]]) {
  native.settings.override('tools.approvalMode',mode);
  approvals=0;
  await writeTool.execute('smoke-write',{path:path.join(cwd,mode+'.txt'),content:'fixture'},undefined,undefined,context);
  assert.equal(approvals,expected,'native approval mismatch for '+mode);
}
native.settings.override('tools.approvalMode','yolo');
native.settings.override('tools.approval',{write:'deny'});
await assert.rejects(writeTool.execute('smoke-deny',{path:path.join(cwd,'denied.txt'),content:'fixture'},undefined,undefined,context));
assert.equal(await fs.access(path.join(cwd,'denied.txt')).then(()=>true,()=>false),false);
native.settings.override('tools.approval',{write:'prompt'});
approvals=0;
await writeTool.execute('smoke-prompt',{path:path.join(cwd,'prompt.txt'),content:'fixture'},undefined,undefined,context);
assert.equal(approvals,1,'explicit native prompt policy lost under yolo');
native.settings.override('tools.approval',{});
const bashTool=native.getToolByName('bash');
assert.ok(bashTool,'native bash tool missing');
native.settings.override('bash.patterns',[{match:'printf *',approval:'deny'}]);
await assert.rejects(bashTool.execute('smoke-bash-deny',{command:'printf fixture'},undefined,undefined,context));
native.settings.override('bash.patterns',[{match:'printf *',approval:'prompt'}]);
approvals=0;
await bashTool.execute('smoke-bash-prompt',{command:'printf fixture'},undefined,undefined,context);
assert.equal(approvals,1,'bash prompt pattern lost under yolo');
native.settings.override('tools.approvalMode','always-ask');
native.settings.override('bash.patterns',[{match:'printf *',approval:'allow'}]);
approvals=0;
await bashTool.execute('smoke-bash-allow',{command:'printf fixture'},undefined,undefined,context);
assert.equal(approvals,0,'bash allow pattern ignored');
await host.close();

await first.session.sessionManager.ensureOnDisk();
const exact = first.session.sessionFile;
assert.ok(exact);
await first.session.dispose();
const resumed = await createAgentSession({ ...options(path.join(temp, 'resume')), sessionManager: await SessionManager.open(exact) });
assert.equal(resumed.session.sessionFile, exact, 'resume changed exact session id');
const missing = path.join(temp, 'does-not-exist.jsonl');
// The raw SDK creates a journal for a missing path. The native host must
// stat/reject a missing exact resume ID before reaching this API.
const recreated = await createAgentSession({ ...options(path.join(temp, 'missing')), sessionManager: await SessionManager.open(missing) });
assert.equal(await fs.access(missing).then(() => true, () => false), true,
  'SDK missing-session behavior changed; host guard must be re-audited');
await recreated.session.dispose();
await second.session.dispose();
await resumed.session.dispose();
console.log(JSON.stringify({ ok: true, bun, sdk, exactSession: true, sdkMissingCreates: true,
  sessions: 2, models: first.session.modelRegistry.getAll().length }));
await fs.rm(temp, { recursive: true, force: true });
