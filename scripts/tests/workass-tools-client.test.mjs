import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { parseCallFlags, run, ToolError } from '../workass-tools.mjs';

const NOT_LAUNCHED_MESSAGE = 'WORKASS_TOOL_CONTEXT is not set; this process was not launched by a Workass chat engine';
const MISSING_MESSAGE = 'Workass context file does not exist; the chat that launched this process was deleted, or the daemon cleaned up a context left over from before a restart. For delayed work, ask the chat to use workass_schedule_message instead of a detached helper';
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

// Requests a stubbed daemon saw, so a test can read what the client asked for.
function recordingReply(requests, reply = { result: { ok: true } }) {
  return (options, callback) => {
    const delivered = {
      statusCode: 200,
      headers: {},
      on(event, handler) {
        if (event === 'data') handler(Buffer.from(JSON.stringify(reply)));
        if (event === 'end') handler();
        return this;
      },
      resume() { return this; },
      destroy() {},
    };
    return {
      on() { return this; },
      end(body) {
        requests.push({ path: options.path, method: options.method, body: body ? body.toString('utf8') : '' });
        setImmediate(() => callback(delivered));
      },
    };
  };
}

function capture() {
  const out = [];
  const err = [];
  return { stdout: { write: (text) => out.push(String(text)) }, stderr: { write: (text) => err.push(String(text)) }, readStdin: () => { throw new Error('stdin was read'); }, out, err };
}

test('the Go client and this one read a command line the same way', async () => {
  const vectors = JSON.parse(await readFile(new URL('./fixtures/tools-call-flags.json', import.meta.url), 'utf8'));
  for (const vector of vectors) {
    if (vector.error) {
      assert.throws(() => parseCallFlags(vector.args), (err) => err instanceof ToolError && err.message.includes(vector.error), JSON.stringify(vector.args));
      continue;
    }
    const got = parseCallFlags(vector.args);
    assert.equal(got.input, vector.want.input ?? '', JSON.stringify(vector.args));
    assert.equal(got.inline, vector.want.inline ?? '', JSON.stringify(vector.args));
    assert.equal(got.help, vector.want.help ?? false, JSON.stringify(vector.args));
    assert.deepEqual(got.pairs, vector.want.pairs ?? {}, JSON.stringify(vector.args));
  }
});

test('usage never advertises --context, and the flag still works', async () => {
  const output = capture();
  await run(['--help'], { env: {}, ...output });
  assert.equal(output.err.join('').includes('--context'), false);
  assert.equal(output.err.join('').includes('--full'), true);
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-context-flag-'));
  const { contextFile } = await fixtureContextFile(root);
  const requests = [];
  await run(['--context', contextFile, 'list'], { env: {}, ...capture(), requestImpl: recordingReply(requests) });
  assert.equal(requests.length, 1);
});

test('guide prints the guide whatever follows it', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-guide-'));
  const guide = path.join(root, 'guide.md');
  await writeFile(guide, 'the guide');
  const output = capture();
  await run(['guide', 'workass_read_chat'], { env: { WORKASS_TOOLS_GUIDE: guide }, ...output });
  assert.equal(output.out.join(''), 'the guide');
  assert.equal(output.err.join('').includes('tools list'), true);
});

test('list asks the daemon for the short listing, one schema, or every schema', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-list-'));
  const { contextFile } = await fixtureContextFile(root);
  for (const [args, want] of [
    [['list'], '/workass/tools'],
    [['list', '--full'], '/workass/tools?full=1'],
    [['list', '-full'], '/workass/tools?full=1'],
    [['list', 'workass_read_chat'], '/workass/tools?name=workass_read_chat'],
  ]) {
    const requests = [];
    await run(args, { env: { WORKASS_TOOL_CONTEXT: contextFile }, ...capture(), requestImpl: recordingReply(requests) });
    assert.equal(requests[0].path, want);
    assert.equal(requests[0].method, 'GET');
  }
  await assert.rejects(run(['list', 'a', 'b'], { env: { WORKASS_TOOL_CONTEXT: contextFile }, ...capture() }), /at most one/);
  await assert.rejects(run(['list', '--verbose'], { env: { WORKASS_TOOL_CONTEXT: contextFile }, ...capture() }), /--full/);
});

test('call flags travel as strings for the daemon to type, and JSON stays exact', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-flags-'));
  const { contextFile } = await fixtureContextFile(root);
  const env = { WORKASS_TOOL_CONTEXT: contextFile };
  let requests = [];
  await run(['call', 'tool', '--limit', '10', '--fork'], { env, ...capture(), requestImpl: recordingReply(requests) });
  assert.equal(requests[0].path, '/workass/tools?flags=1');
  assert.deepEqual(JSON.parse(requests[0].body), { name: 'tool', arguments: { limit: '10', fork: 'true' } });

  requests = [];
  const big = '{"limit":90071992547409931}';
  await run(['call', 'tool'], { env, ...capture(), readStdin: () => Buffer.from(big), requestImpl: recordingReply(requests) });
  assert.equal(requests[0].path, '/workass/tools');
  assert.equal(requests[0].body, `{"name":"tool","arguments":${big}}`);

  for (const args of [['call', 'tool', '--input', big], ['call', 'tool', big]]) {
    requests = [];
    await run(args, { env, ...capture(), requestImpl: recordingReply(requests) });
    assert.equal(requests[0].path, '/workass/tools');
    assert.equal(requests[0].body, `{"name":"tool","arguments":${big}}`);
  }
  requests = [];
  await run(['call', 'tool', '--input', '{}'], { env, ...capture(), requestImpl: recordingReply(requests) });
  assert.equal(requests[0].body, '{"name":"tool","arguments":{}}');

  requests = [];
  await run(['call', 'tool', '--input', '-'], { env, ...capture(), readStdin: () => Buffer.from('{"a":1}'), requestImpl: recordingReply(requests) });
  assert.equal(requests[0].body, '{"name":"tool","arguments":{"a":1}}');

  await assert.rejects(run(['call', 'tool', '--input', '/no/such/file.json'], { env, ...capture() }),
    (err) => err instanceof ToolError && err.message.includes('cannot open tool arguments file') && err.message.includes('inline'));

  // A real file wins, whatever it is called.
  const odd = path.join(root, '{odd}.json');
  await writeFile(odd, '{"from":"file"}');
  requests = [];
  await run(['call', 'tool', '--input', odd], { env, ...capture(), requestImpl: recordingReply(requests) });
  assert.equal(requests[0].body, '{"name":"tool","arguments":{"from":"file"}}');
});

test('a flag before the tool name is named as that mistake', async () => {
  await assert.rejects(run(['call', '--title', 'x', 'some_tool'], { env: {}, ...capture() }), /tool name first/);
});

test('a call syntax mistake asks for that tool\'s example, and is explained without a context', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-tools-syntax-'));
  const { contextFile } = await fixtureContextFile(root);
  const requests = [];
  await assert.rejects(
    run(['call', 'some_tool', 'stray'], { env: { WORKASS_TOOL_CONTEXT: contextFile }, ...capture(), requestImpl: recordingReply(requests, { result: { usage: 'the example for this tool' } }) }),
    (err) => err instanceof ToolError && err.message.includes('unexpected') && err.message.includes('the example for this tool'),
  );
  assert.equal(requests.length, 1);
  assert.equal(requests[0].method, 'GET');
  assert.equal(requests[0].path, '/workass/tools?name=some_tool&usage=1');
  await assert.rejects(
    run(['call', 'some_tool', 'stray'], { env: {}, ...capture() }),
    (err) => err instanceof ToolError && err.message.includes('unexpected') && err.message.includes('--input'),
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
