import assert from 'node:assert/strict';
import test from 'node:test';

import { AtlassianMcpClient } from '../atlassian-mcp.mjs';

test('Atlassian client uses only stateless MCP 2026-07-28 requests', async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  const calls = [];
  globalThis.fetch = async (_url, options) => {
    calls.push(options);
    const body = JSON.parse(options.body);
    if (body.method === 'server/discover') {
      return new Response(JSON.stringify({
        jsonrpc: '2.0', id: body.id, result: {
          resultType: 'complete', supportedVersions: ['2026-07-28'], capabilities: { tools: {} },
          ttlMs: 300000, cacheScope: 'private',
        },
      }), { status: 200, headers: { 'content-type': 'application/json' } });
    }
    return new Response(`data: ${JSON.stringify({
      jsonrpc: '2.0', id: body.id, result: {
        resultType: 'complete', content: [{ type: 'text', text: '{"ok":true}' }],
      },
    })}\n\n`, { status: 200, headers: { 'content-type': 'text/event-stream' } });
  };

  const client = new AtlassianMcpClient({ url: 'https://mcp.example.invalid/rpc', access_token: 'fixture-token' });
  await client.initialize();
  assert.deepEqual(await client.callTool('fixture_tool', { key: 'value' }), { ok: true });
  assert.equal(calls.length, 2);
  for (const call of calls) {
    const body = JSON.parse(call.body);
    assert.equal(call.headers['mcp-protocol-version'], '2026-07-28');
    assert.equal(call.headers['mcp-method'], body.method);
    assert.equal(call.headers['mcp-session-id'], undefined);
    assert.equal(body.params._meta['io.modelcontextprotocol/protocolVersion'], '2026-07-28');
    assert.deepEqual(body.params._meta['io.modelcontextprotocol/clientCapabilities'], {});
    assert.notEqual(body.method, 'initialize');
  }
  assert.equal(calls[1].headers['mcp-name'], 'fixture_tool');
});

test('Atlassian client does not fall back to a legacy initialize handshake', async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  const methods = [];
  globalThis.fetch = async (_url, options) => {
    const body = JSON.parse(options.body);
    methods.push(body.method);
    return new Response(JSON.stringify({
      jsonrpc: '2.0', id: body.id, error: {
        code: -32022, message: 'unsupported', data: { requested: '2026-07-28', supported: ['2025-11-25'] },
      },
    }), { status: 400, headers: { 'content-type': 'application/json' } });
  };

  const client = new AtlassianMcpClient({ url: 'https://mcp.example.invalid/rpc', access_token: 'fixture-token' });
  await assert.rejects(client.initialize(), /MCP HTTP 400/u);
  assert.deepEqual(methods, ['server/discover']);
});
