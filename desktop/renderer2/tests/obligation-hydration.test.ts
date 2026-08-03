import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

// The obligation reaches the sidebar through spawned-work:changed, which only
// fires when background state changes. A chat that owes something but is
// running nothing quiet never emits it again, so a renderer that reloads —
// which is every promotion — would drop its pill until unrelated work moved.
// The refresh path is the only hydration this client has; it must carry the
// obligation, not just the items.
test('refreshing spawned work hydrates the obligation, not only the items', () => {
  const source = readFileSync(new URL('../src/store/store.ts', import.meta.url), 'utf8');
  const refresh = source.match(/async refreshSpawnedWork\([\s\S]*?\n  \}\n/)?.[0] ?? '';

  assert.match(refresh, /call\('spawnedWorkList'/);
  assert.match(refresh, /obligation: result\.obligation/);
});

// Additive-only: an older daemon answers spawned-work:list with items alone,
// and the ingest must treat that as "nothing new to say" rather than clearing
// a state it already holds.
test('an absent obligation never erases the one already held', () => {
  const source = readFileSync(new URL('../src/store/store.ts', import.meta.url), 'utf8');
  const ingest = source.match(/private onSpawnedWorkChanged\([\s\S]*?\n  \}\n/)?.[0] ?? '';

  assert.match(ingest, /if \(s\.obligation\?\.state\)/);
  assert.doesNotMatch(ingest, /delete this\.state\.obligationByChat/);
});
