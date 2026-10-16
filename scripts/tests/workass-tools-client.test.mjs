import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { run, ToolError } from '../workass-tools.mjs';

const NOT_LAUNCHED_MESSAGE = 'WORKASS_TOOL_CONTEXT is not set; this process was not launched by a Workass chat engine';
const MISSING_MESSAGE = 'Workass context file does not exist; the chat that launched this process was deleted, or the daemon cleaned up a context left over from before a restart; long-running helpers should not poll — ask the chat to use workass_schedule_message';
const UNAUTHORIZED_MESSAGE = 'Workass rejected this context\'s credential: the chat that launched this process was deleted, or the context was issued by an engine that has since stopped. For delayed work, ask the chat to use workass_schedule_message instead of a detached helper';

function io() {
  return { stdout: { write: () => {} }, stderr: { write: () => {} }, readStdin: () => Buffer.from('{}') };
}

// fixtureContextFile mirrors internal/toolcommand/run_test.go: it writes a
// valid private context file so ReadConfig succeeds and the request runs.
async function fixtureContextFile(root) {
  const ca = path.join(root, 'ca.pem');
  await writeFile(ca, '-----BEGIN CERTIFICATE----- FIXTURE -----END CERTIFICATE-----');
  const config = {
    endpoint: 'https://tools.localhost:8443/workass/tools',
    ca_file: ca,
    credential: 'wct_private_fixture_capability',
    chat_id: 'chat-fixture',
    tab_id: 'tab-fixture',
  };
  const contextFile = path.join(root, 'context.json');
  await writeFile(contextFile, JSON.stringify(config));
  return { config, contextFile };
}

// fixtureReply stubs one HTTPS response for doRequest's injected requestImpl.
function fixtureReply(status, body) {
  return (options, callback) => {
    void options;
    const reply = {
      statusCode: status,
      headers: {},
      on(event, handler) {
        if (event === 'data' && body !== undefined) handler(Buffer.from(JSON.stringify(body)));
        if (event === 'end') handler();
        return this;
      },
      resume() { return this; },
      destroy() {},
    };
    setImmediate(() => callback(reply));
    return { on() { return this; }, end() {} };
  };
}

test('unset WORKASS_TOOL_CONTEXT states the process was not launched by a Workass chat engine', async () => {
  for (const env of [{ WORKASS_TOOL_CONTEXT: '' }, {}]) {
    await assert.rejects(
      run(['list'], { env, ...io(), requestImpl: () => { throw new Error('no request expected'); } }),
      (err) => err instanceof ToolError && err.message === NOT_LAUNCHED_MESSAGE,
    );
  }
});

test('missing context file states the chat was deleted and never leaks the private path', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-missing-'));
  const stale = path.join(root, 'state', 'tool-bootstrap', 'bridge-0001', 'context.json');
  await assert.rejects(
    run(['list'], { env: { WORKASS_TOOL_CONTEXT: stale }, ...io(), requestImpl: () => { throw new Error('no request expected'); } }),
    (err) => {
      assert.ok(err instanceof ToolError);
      assert.equal(err.message, MISSING_MESSAGE);
      // The context path is a private detail; repeating it is exactly how
      // stale context paths spread through conversation history.
      assert.equal(err.message.includes('context.json'), false);
      assert.equal(err.message.includes('bridge-0001'), false);
      return true;
    },
  );
});

test('unusable context file keeps the existing wording, also without the path', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-invalid-'));
  await assert.rejects(
    run(['list'], { env: { WORKASS_TOOL_CONTEXT: root }, ...io(), requestImpl: () => { throw new Error('no request expected'); } }),
    (err) => {
      assert.ok(err instanceof ToolError);
      assert.equal(err.message, 'Workass context file is unavailable or invalid; use the current process WORKASS_TOOL_CONTEXT, not a context path copied from conversation history');
      assert.equal(err.message.includes(root.startsWith('/private') ? root.slice('/private'.length) : root), false);
      return true;
    },
  );
});

test('HTTP 401 states both deleted chat and stopped engine without credential or path', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-unauthorized-'));
  const { config, contextFile } = await fixtureContextFile(root);
  await assert.rejects(
    run(['list'], { env: { WORKASS_TOOL_CONTEXT: contextFile }, ...io(), requestImpl: fixtureReply(401, { error: 'unauthorized' }) }),
    (err) => {
      assert.ok(err instanceof ToolError);
      assert.equal(err.message, UNAUTHORIZED_MESSAGE);
      // Mirror Go's classification: the daemon's own error body must not leak,
      // and neither the credential nor the private context path may appear.
      assert.equal(err.message.includes('unauthorized'), false);
      assert.equal(err.message.includes(config.credential), false);
      assert.equal(err.message.includes('context.json'), false);
      return true;
    },
  );
});

test('401 without a body still classifies, mirroring a bare daemon rejection', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-unauth-plain-'));
  const { contextFile } = await fixtureContextFile(root);
  await assert.rejects(
    run(['list'], { env: { WORKASS_TOOL_CONTEXT: contextFile }, ...io(), requestImpl: fixtureReply(401, {}) }),
    (err) => err instanceof ToolError && err.message === UNAUTHORIZED_MESSAGE,
  );
});

test('other HTTP rejections surface the daemon error text unchanged', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-forbidden-'));
  const { contextFile } = await fixtureContextFile(root);
  await assert.rejects(
    run(['list'], { env: { WORKASS_TOOL_CONTEXT: contextFile }, ...io(), requestImpl: fixtureReply(403, { error: 'forbidden' }) }),
    (err) => err instanceof ToolError && err.message === 'forbidden',
  );
});
