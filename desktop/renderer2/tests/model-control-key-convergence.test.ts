// The daemon and the renderer must key per-model memory identically.
//
// They did not. `cmd/workass/session_store.go` rewrites `gpt-5.6-sol[xhigh]` to
// `gpt-5.6-sol` on every save and marks the session changed; the renderer read
// the key back unchanged, kept it through `preserveNewerLocalControls`, and
// wrote it out again. Measured on the running daemon 2026-07-26: one session
// rewrite every 1.25s, indefinitely — and each one restores the server's
// `activeId`, so the chat the user had selected kept getting yanked away.
import assert from 'node:assert/strict';
import test from 'node:test';
import { normalizeModelControlMemory } from '../src/model-controls.ts';
import { canonicalModelControlKey } from '../src/model-selection.ts';

test('a canonical effort suffix is peeled from a memory key, as the daemon peels it', () => {
  const memory = normalizeModelControlMemory({ codex: { 'gpt-5.6-sol[xhigh]': { effort: 'xhigh' } } });
  assert.deepEqual(memory, { codex: { 'gpt-5.6-sol': { effort: 'xhigh' } } });
});

test('normalizing is idempotent, which is what ends the loop', () => {
  const once = normalizeModelControlMemory({ codex: { 'gpt-5.6-sol[xhigh]': { effort: 'xhigh' } } });
  assert.deepEqual(normalizeModelControlMemory(once), once);
});

test('an entry already stored under the base wins, matching the daemon migration', () => {
  const memory = normalizeModelControlMemory({
    codex: { 'gpt-5.6-sol': { effort: 'high' }, 'gpt-5.6-sol[xhigh]': { effort: 'xhigh' } },
  });
  assert.deepEqual(memory, { codex: { 'gpt-5.6-sol': { effort: 'high' } } });
});

test('a literal bracket id is not an effort and is left alone', () => {
  // Claude ships `opus[1m]` as a real model id. Peeling it would silently
  // retarget the chat to a different model.
  assert.equal(canonicalModelControlKey('opus[1m]'), 'opus[1m]');
  const memory = normalizeModelControlMemory({ claude: { 'opus[1m]': { effort: 'max' } } });
  assert.deepEqual(memory, { claude: { 'opus[1m]': { effort: 'max' } } });
});

test('the renderer effort set matches the daemon canonicalEffortOrder', () => {
  for (const effort of ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra']) {
    assert.equal(canonicalModelControlKey(`m[${effort}]`), 'm', `${effort} should be peeled`);
  }
  assert.equal(canonicalModelControlKey('m[1m]'), 'm[1m]');
  assert.equal(canonicalModelControlKey('m[thinking]'), 'm[thinking]');
});

// The actual loop, reproduced: a model that has LEFT the catalog cannot be split
// by resolveModelSelection, which returns the whole suffixed id as the base. The
// reconcile pass then wrote that back as a memory key on every pass, the daemon
// stripped it on every save, and neither side ever converged.
test('a model missing from the catalog still writes a canonical memory key', async () => {
  const { rememberModelControls, rememberedModelControls } = await import('../src/model-controls.ts');
  const { resolveModelSelection } = await import('../src/model-selection.ts');

  const emptyCatalog = resolveModelSelection([], [], 'gpt-5.6-sol[xhigh]');
  assert.equal(emptyCatalog.base, 'gpt-5.6-sol[xhigh]', 'precondition: an absent model yields the whole id');

  const memory = rememberModelControls(undefined, 'codex', emptyCatalog.base, { effort: 'xhigh' });
  assert.deepEqual(memory, { codex: { 'gpt-5.6-sol': { effort: 'xhigh' } } });
  // Written canonically, so it must also be readable by the id the chat holds.
  assert.deepEqual(rememberedModelControls(memory, 'codex', 'gpt-5.6-sol[xhigh]'), { effort: 'xhigh' });
});
