import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { ToolEvent } from '../src/store/types.ts';

// The rail's step-by-step is the agent's REAL plan and nothing else. An earlier
// version fabricated "steps" from tool-call kinds ("Ejecutando comandos") and
// used tool titles / streamed thought fragments as the hero; that was rejected.
// These assertions fail if any of it comes back.

const doneTool: ToolEvent = {
  key: 'bash-1', at: 1, kind: 'tool', id: 'bash-1', toolKind: 'execute',
  title: 'Bash', status: 'completed', command: 'go test ./cmd/workass/...',
  location: 'go test ./cmd/workass/…', terminalId: null, input: null, output: null,
  startedAt: 1_000, endedAt: 3_000,
};

const plan = (entries: Array<{ status: string; content: string }>, key = 'plan-1') =>
  ({ key, at: 0, kind: 'plan', entries });

let server: ViteDevServer;
let TareasCard: React.ComponentType;
let appStore: { state: Record<string, unknown> };

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  ({ TareasCard } = await server.ssrLoadModule('/src/components/TareasCard.tsx') as {
    TareasCard: React.ComponentType;
  });
  appStore = (await server.ssrLoadModule('/src/store/store.ts') as {
    store: { state: Record<string, unknown> };
  }).store;
});

after(async () => { await server.close(); });

function renderRail(
  messages: unknown[],
  planLatest: Array<{ status: string; content: string }> = [],
) {
  appStore.state.meta = { profile: 'dev' };
  appStore.state.activeId = 'tab-1';
  appStore.state.chats = [{ id: 'tab-1', cwd: null, messages, planLatest }];
  return renderToStaticMarkup(React.createElement(TareasCard));
}

const running = (events: unknown[], id = 'a-1') =>
  ({ id, role: 'assistant', content: '', status: 'running', at: null, events });
const settled = (events: unknown[], id = 'a-0') =>
  ({ id, role: 'assistant', content: '', status: 'done', at: null, events });

test('the hero is the provider in_progress entry verbatim, and the dots match the plan exactly', () => {
  const entries = [
    { status: 'completed', content: 'Leer el panel' },
    { status: 'in_progress', content: 'Rediseñar la densidad del panel' },
    { status: 'pending', content: 'Verificar en dev' },
  ];
  const html = renderRail([running([doneTool])], entries);
  assert.match(html, /class="r-say">Rediseñar la densidad del panel</);
  // one dot per provider entry, in provider order and status
  assert.match(html, /class="r-pdots"[^>]*>(<i class="done"><\/i>|<i class="done">)/);
  const dots = html.match(/<i class="(done|now)?"><\/i>/g) ?? [];
  assert.equal(dots.length, entries.length, 'one dot per plan entry');
  // the expanded checklist carries every entry, unchanged
  for (const entry of entries) assert.ok(html.includes(entry.content), `missing entry: ${entry.content}`);
  // never a synthesized label or a tool title as plan content
  assert.doesNotMatch(html, /Ejecutando comandos|Leyendo archivos|Buscando en el código/);
  assert.doesNotMatch(html, /class="r-say">Bash/);
});

test('a turn with no plan says so and invents nothing', () => {
  const html = renderRail([running([doneTool])]);
  assert.match(html, /Sin plan del agente\./);
  assert.doesNotMatch(html, /class="r-pdots"/, 'no dots without a real plan');
  assert.doesNotMatch(html, /Ejecutando comandos|Leyendo archivos|Trabajando…/);
  assert.doesNotMatch(html, /class="r-say">/, 'no hero without an in_progress entry');
});

test('a plan with no in_progress entry shows the dots but never infers a current step', () => {
  const entries = [
    { status: 'completed', content: 'Paso uno' },
    { status: 'pending', content: 'Paso dos' },
  ];
  const html = renderRail([running([])], entries);
  assert.match(html, /class="r-pdots"/);
  assert.doesNotMatch(html, /class="r-say">/, 'no hero is inferred from pending/completed entries');
});

test('an incomplete plan from an earlier turn carries forward', () => {
  const entries = [
    { status: 'completed', content: 'Paso hecho' },
    { status: 'in_progress', content: 'Paso en curso' },
  ];
  const html = renderRail([settled([]), running([doneTool], 'a-2')], entries);
  assert.match(html, /class="r-say">Paso en curso</);
  assert.match(html, /class="r-pdots"/);
});

test('a completed plan from an earlier turn does not linger', () => {
  const entries = [
    { status: 'completed', content: 'Todo listo uno' },
    { status: 'completed', content: 'Todo listo dos' },
  ];
  const html = renderRail([settled([plan(entries)]), running([doneTool], 'a-2')], []);
  assert.doesNotMatch(html, /Todo listo uno/, 'a finished plan must not resurface on a later turn');
  assert.doesNotMatch(html, /class="r-pdots"/);
  assert.match(html, /Sin plan del agente\./);
});
