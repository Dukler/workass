import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import {
  contextUsageForProvider,
  contextUsageIdentityMatches,
  contextUsagePercent,
  normalizeContextUsageByProvider,
  withContextUsage,
} from '../src/context-usage.ts';

test('context usage remains provider-scoped across model handovers', () => {
  let usage = withContextUsage(undefined, 'codex', { used: 128_800, size: 258_400, updatedAt: '2026-07-22T19:37:00Z' });
  usage = withContextUsage(usage, 'claude', { used: 40_000, size: 200_000, updatedAt: '2026-07-22T19:38:00Z' });

  assert.deepEqual(contextUsageForProvider(usage, 'codex'), {
    used: 128_800,
    size: 258_400,
    updatedAt: '2026-07-22T19:37:00Z',
  });
  assert.deepEqual(contextUsageForProvider(usage, 'claude'), {
    used: 40_000,
    size: 200_000,
    updatedAt: '2026-07-22T19:38:00Z',
  });
  assert.equal(contextUsageForProvider(usage, 'mock'), null);
  assert.equal(contextUsageForProvider(usage, 'mock'), null);
});

test('context percentage never turns missing data into zero percent', () => {
  const snapshot = { used: 128_800, size: 258_400 };
  assert.equal(contextUsagePercent(snapshot), 50);
  assert.equal(contextUsagePercent(undefined), null);
});

test('persisted context usage accepts only bounded real provider snapshots', () => {
  assert.deepEqual(normalizeContextUsageByProvider({
    codex: { used: 128_800, size: 258_400, updatedAt: '2026-07-22T19:37:00Z' },
    broken: { used: -1, size: 0 },
  }), {
    codex: { used: 128_800, size: 258_400, updatedAt: '2026-07-22T19:37:00Z' },
  });
});

test('late usage events require the complete stable chat identity', () => {
  const chat = { id: 'tab-a', chatId: 'chat-a', sessionId: 'session-a' };
  assert.equal(contextUsageIdentityMatches({ tabId: 'tab-a', chatId: 'chat-a', sessionId: 'old-session' }, chat), true);
  assert.equal(contextUsageIdentityMatches({ tabId: 'tab-a', chatId: 'replacement-chat' }, chat), false);
  assert.equal(contextUsageIdentityMatches({ tabId: 'replacement-tab', chatId: 'chat-a' }, chat), false);
  assert.equal(contextUsageIdentityMatches({ sessionId: 'session-a' }, chat), true);
});

test('composer keeps context passive and exposes exact details only in its popover', () => {
  const composer = readFileSync(new URL('../src/components/Composer.tsx', import.meta.url), 'utf8');
  const styles = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  const persistence = readFileSync(new URL('../src/store/persistence.ts', import.meta.url), 'utf8');
  const store = readFileSync(new URL('../src/store/store.ts', import.meta.url), 'utf8');

  assert.doesNotMatch(composer, /className="ctxreadout"/);
  assert.doesNotMatch(composer, /Contexto ·/);
  assert.doesNotMatch(styles, /\.ctxreadout\s*\{/);
  assert.match(composer, /className="pop pop-ctx"/);
  assert.match(composer, /compactContextTokens\(usage\.used\)/);
  assert.match(persistence, /contextUsageByProvider\?:/);
  assert.doesNotMatch(store, /\busage:\s*legacy|c\.usage/);
});
