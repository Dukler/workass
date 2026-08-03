import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';

// Approved mock f-bg2 (2026-07-23): RUNNING spawned work = inline .bgr rows,
// visible without opening any fold; FINISHED spawned work = ONE flat
// "Segundo plano" fold — no fold nested inside a fold.
test('running spawned work is inline; finished work is a single flat fold', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(async () => { await server.close(); });
  const loaded = await server.ssrLoadModule('/src/components/SpawnedWorkCard.tsx') as {
    SpawnedWorkCard: React.ComponentType<{ chat: unknown }>;
    SpawnedWorkLive: React.ComponentType<{ chat: unknown }>;
  };
  const storeModule = await server.ssrLoadModule('/src/store/store.ts') as { store: { state: Record<string, unknown> } };
  const chat = { id: 'tab-1', chatId: 'chat-1', messages: [] };
  storeModule.store.state.hasSpawnedWorkChannels = false;
  storeModule.store.state.spawnedWorkByChat = {
    ['tab-1\u0000chat-1']: [
      {
        id: 'bash-1', taskId: 'bash-1', tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'bash', status: 'running',
        label: 'Serve the complete interactive comparison gallery without truncating this title',
        startedAt: new Date(Date.now() - 5000).toISOString(), updatedAt: new Date().toISOString(), pid: 321,
        outputFile: '/private/tmp/claude-501/project/session/tasks/bash-1.output',
        summary: 'Reempaquetando la app y esperando el gate.',
      },
      {
        id: 'agent-1', taskId: 'agent-1', tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'agent', status: 'exited',
        label: 'Review the UI', startedAt: new Date(Date.now() - 9000).toISOString(), updatedAt: new Date().toISOString(), finishedAt: new Date().toISOString(),
      },
      {
        id: 'bash-2', taskId: 'bash-2', tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'bash', status: 'failed',
        label: 'notarize.sh', startedAt: new Date(Date.now() - 8000).toISOString(), updatedAt: new Date().toISOString(), finishedAt: new Date(Date.now() - 2000).toISOString(),
        exitCode: 1,
      },
    ],
  };

  // Running rows: inline, flat, no <details> wrapper, pulse + live summary visible.
  const live = renderToStaticMarkup(React.createElement(loaded.SpawnedWorkLive, { chat }));
  assert.match(live, /class="bglive"/);
  assert.match(live, /Serve the complete interactive comparison gallery without truncating this title/);
  // the kind reads as a word, not the wire enum (mock rail-actions, 2026-07-27)
  assert.match(live, /comando · pid 321/);
  // daemon prose we cannot classify stays verbatim — and gets NO action glyph
  assert.match(live, /<span class="bgr-sum"[^>]*>Reempaquetando la app y esperando el gate\.<\/span>/);
  assert.doesNotMatch(live, /class="bgr-act"/);
  assert.match(live, /ver salida/);
  assert.match(live, /r-pulse/);
  assert.doesNotMatch(live, /<details/);
  assert.doesNotMatch(live, /Review the UI/); // finished items never render inline

  // Finished fold: one details, flat r-dline rows; running item and the old
  // nested "trabajos terminados" fold are gone. The row says WHAT ran, never how
  // it ended — the fold's old « · 1 falló» count and every red cue were removed
  // on the user's call (2026-07-25), so this pins their absence.
  const card = renderToStaticMarkup(React.createElement(loaded.SpawnedWorkCard, { chat }));
  assert.match(card, /Segundo plano/);
  assert.match(card, /<span class="r-count">2<\/span>/);
  assert.match(card, /Review the UI/);
  assert.match(card, /notarize\.sh/);
  assert.doesNotMatch(card, /falló|fracas|error/i);
  assert.match(card, /r-dline/);
  assert.doesNotMatch(card, /trabajos terminados/);
  assert.doesNotMatch(card, /Serve the complete interactive comparison gallery/);
  assert.equal((card.match(/<details/g) ?? []).length, 1, 'exactly one fold — no nested compaction');
});

test('spawned-work rail CSS clamps titles and bounds output tails', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  const title = css.match(/\.bgr-n\s*\{([^}]*)\}/)?.[1] ?? '';
  const tail = css.match(/\.bgr-tail\s*\{([^}]*)\}/)?.[1] ?? '';
  assert.match(title, /overflow-wrap:\s*anywhere/);
  assert.match(title, /-webkit-line-clamp:\s*2/);
  assert.match(tail, /max-height:\s*180px/);
  assert.match(tail, /overflow:\s*auto/);
  // Fold bodies scroll INSIDE the fold — opening a long list must never spawn
  // a pane-wide scrollbar on the rail (user 2026-07-24).
  const foldBody = css.match(/\.r-meta-b\s*\{([^}]*)\}/)?.[1] ?? '';
  assert.match(foldBody, /max-height:/);
  assert.match(foldBody, /overflow-y:\s*auto/);
  assert.match(foldBody, /overscroll-behavior:\s*contain/);
  // No lingering styles from the removed nested-fold design.
  assert.doesNotMatch(css, /bgwdone|\.bgcard/);
});
