// Persisted per-model memory uses base model ids as keys. Composite effort ids
// are selections, not keys, and are not accepted from the persisted shape.
import assert from 'node:assert/strict';
import test from 'node:test';
import { normalizeModelControlMemory } from '../src/model-controls.ts';
import { canonicalModelControlKey } from '../src/model-selection.ts';

test('a noncanonical effort-suffixed key is ignored', () => {
  const memory = normalizeModelControlMemory({ codex: { 'gpt-5.6-sol[xhigh]': { effort: 'xhigh' } } });
  assert.equal(memory, undefined);
});

test('canonical entries survive normalization unchanged', () => {
  const memory = normalizeModelControlMemory({
    codex: { 'gpt-5.6-sol': { effort: 'high' } },
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

// A model that has LEFT the catalog cannot be split by resolveModelSelection,
// which returns the whole suffixed id as the base. The write path still
// canonicalizes that selected base before storing controls.
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
