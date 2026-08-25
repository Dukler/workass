import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';
import type { ToolEvent } from '../src/store/types.ts';

test('a running subagent defaults closed with model and live activity in its two-line summary', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(async () => { await server.close(); });

  const loaded = await server.ssrLoadModule('/src/components/TareasCard.tsx') as {
    TareasCard: React.ComponentType;
  };
  const storeModule = await server.ssrLoadModule('/src/store/store.ts') as {
    store: { state: Record<string, unknown> };
  };
  const events: ToolEvent[] = [
    {
      key: 'subagent-1', at: 0, kind: 'tool', id: 'subagent-1', toolKind: 'agent',
      title: 'Opus · Entorno files UI', status: 'in_progress', command: null, location: null,
      terminalId: null, input: null, output: null, subagentId: 'subagent-1',
      subagentHeader: true, subagentProvider: 'claude', subagentModel: 'Opus4.8-max',
      startedAt: 1_000,
    },
    {
      key: 'subagent-1:read', at: 1, kind: 'tool', id: 'subagent-1:read', toolKind: 'read',
      title: 'Read File', status: 'in_progress', command: null, location: 'AGENTS.md',
      terminalId: null, input: null, output: null, subagentId: 'subagent-1',
      subagentProvider: 'claude', subagentModel: 'Opus4.8-max', startedAt: 2_000,
    },
  ];
  storeModule.store.state.meta = { profile: 'dev' };
  storeModule.store.state.activeId = 'tab-1';
  storeModule.store.state.chats = [{
    id: 'tab-1', cwd: null, messages: [{
      id: 'assistant-1', role: 'assistant', content: '', status: 'running', at: null, events,
    }],
  }];

  const html = renderToStaticMarkup(React.createElement(loaded.TareasCard));
  assert.match(html, /<details class="r-sa" data-status="running">/);
  assert.doesNotMatch(html, /<details class="r-sa" data-status="running" open=""/);
  assert.match(html, /class="r-mdl">Opus4\.8-max<\/span>/);
  // The live activity is named by the shared action vocabulary and carries the
  // action's own glyph (mock rail-actions, 2026-07-27) — the rail's old private
  // gerund («Leyendo un archivo») disagreed with the row right below it.
  assert.match(html, /class="a-n">Leer un archivo<\/span>/);
  assert.match(html, /class="r-act"><span class="a-ic"/);
  assert.match(html, /class="r-sa-b"/);
  assert.match(html, /Read File/); // the raw tool id survives in the row tooltip
});

test('steering keeps every subagent from the same logical turn visible', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(async () => { await server.close(); });

  const loaded = await server.ssrLoadModule('/src/components/TareasCard.tsx') as {
    TareasCard: React.ComponentType;
  };
  const storeModule = await server.ssrLoadModule('/src/store/store.ts') as {
    store: { state: Record<string, unknown> };
  };
  const events: ToolEvent[] = [
    {
      key: 'subagent-1', at: 0, kind: 'tool', id: 'subagent-1', toolKind: 'agent',
      title: 'Inspect renderer state', status: 'in_progress', command: null, location: null,
      terminalId: null, input: null, output: null, subagentId: 'subagent-1',
      subagentHeader: true, subagentProvider: 'codex', subagentModel: 'gpt-5.6-sol[max]',
      startedAt: 1_000,
    },
    {
      key: 'subagent-2', at: 0, kind: 'tool', id: 'subagent-2', toolKind: 'agent',
      title: 'Inspect daemon state', status: 'in_progress', command: null, location: null,
      terminalId: null, input: null, output: null, subagentId: 'subagent-2',
      subagentHeader: true, subagentProvider: 'codex', subagentModel: 'gpt-5.6-sol[high]',
      startedAt: 1_100,
    },
  ];
  storeModule.store.state.meta = { profile: 'dev' };
  storeModule.store.state.activeId = 'tab-1';
  storeModule.store.state.chats = [{
    id: 'tab-1', cwd: null, messages: [
      {
        id: 'assistant-root', role: 'assistant', content: 'Working', status: 'done', at: null,
        events, turnRootId: 'assistant-root', turnTerminal: false,
      },
      {
        id: 'steer-1', role: 'user', content: 'First direction', status: 'done', at: null,
        events: [], turnRootId: 'assistant-root', steerState: 'applied',
      },
      {
        id: 'assistant-tail-1', role: 'assistant', content: '', status: 'done', at: null,
        events: [], turnRootId: 'assistant-root', turnTerminal: false,
      },
      {
        id: 'steer-2', role: 'user', content: 'Second direction', status: 'done', at: null,
        events: [], turnRootId: 'assistant-root', steerState: 'applied',
      },
      {
        id: 'assistant-tail-2', role: 'assistant', content: '', status: 'running', at: null,
        events: [], turnRootId: 'assistant-root', turnTerminal: true,
      },
    ],
  }];

  const html = renderToStaticMarkup(React.createElement(loaded.TareasCard));
  assert.match(html, /Inspect renderer state/);
  assert.match(html, /Inspect daemon state/);
  assert.equal((html.match(/class="r-sa"/g) ?? []).length, 2, 'each child remains one Turnos row');
  assert.match(html, /class="r-lbl">Ahora<\/span>/, 'the steered logical turn remains live');

  // Native receipt-bearing steering first reserves an empty continuation but
  // deliberately leaves the original assistant row running and unsplit. That
  // acknowledgement window must preserve the same projection too.
  storeModule.store.state.chats = [{
    id: 'tab-1', cwd: null, messages: [
      {
        id: 'assistant-root', role: 'assistant', content: 'Working', status: 'running', at: null,
        events,
      },
      {
        id: 'steer-staged', role: 'user', content: 'Pending direction', status: 'pending', at: null,
        events: [], turnRootId: 'assistant-root', steerState: 'accepted', steerBoundary: 'waiting',
        steerContinuationId: 'assistant-staged',
      },
      {
        id: 'assistant-staged', role: 'assistant', content: '', status: 'pending', at: null,
        events: [], turnRootId: 'assistant-root', turnTerminal: true, steerBoundary: 'waiting',
        steerContinuationFor: 'steer-staged',
      },
    ],
  }];
  const stagedHtml = renderToStaticMarkup(React.createElement(loaded.TareasCard));
  assert.match(stagedHtml, /Inspect renderer state/);
  assert.match(stagedHtml, /Inspect daemon state/);
  assert.equal((stagedHtml.match(/class="r-sa"/g) ?? []).length, 2);
  assert.match(stagedHtml, /class="r-lbl">Ahora<\/span>/);
});
