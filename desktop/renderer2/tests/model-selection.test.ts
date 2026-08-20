import assert from 'node:assert/strict';
import test from 'node:test';
import {
  modelContextQualifier,
  resolveModelSelection,
} from '../src/model-selection.ts';
import type { CatalogGroup, ModelOption } from '../src/wire/types.ts';

const claudeModels: ModelOption[] = [
  { modelId: 'claude-fable-5[1m]', name: 'Fable 5' },
  { modelId: 'opus[1m]', name: 'Opus 4.8' },
  { modelId: 'sonnet', name: 'Sonnet 5' },
  { modelId: 'haiku', name: 'Haiku 4.5' },
];
const groups: CatalogGroup[] = [{
  providerId: 'claude', providerName: 'Claude', status: 'ready', models: claudeModels, modes: [],
}];

test('keeps Claude Fable literal [1m] model id intact', () => {
  const selected = resolveModelSelection(groups, [], 'claude-fable-5[1m]');
  assert.deepEqual(selected, { base: 'claude-fable-5[1m]', effort: null, model: claudeModels[0] });
});

test('keeps Claude Opus literal [1m] model id intact', () => {
  const selected = resolveModelSelection(groups, [], 'opus[1m]');
  assert.equal(selected.base, 'opus[1m]');
  assert.equal(selected.effort, null);
  assert.equal(selected.model?.name, 'Opus 4.8');
});

test('parses a suffix only when the base model advertises that effort', () => {
  const codex: ModelOption = { modelId: 'gpt-5.6-sol', name: 'GPT-5.6 Sol', efforts: ['low', 'high', 'xhigh'] };
  const selected = resolveModelSelection([], [codex], 'gpt-5.6-sol[xhigh]');
  assert.deepEqual(selected, { base: 'gpt-5.6-sol', effort: 'xhigh', model: codex });
});

test('an unadvertised effort is not guessed into a provider model', () => {
  const selected = resolveModelSelection(groups, [], 'haiku[low]');
  assert.deepEqual(selected, { base: 'haiku[low]', effort: null, model: null });
});

test('does not corrupt an unknown bracketed provider model id', () => {
  const selected = resolveModelSelection([], [], 'vendor-model[long-context]');
  assert.deepEqual(selected, { base: 'vendor-model[long-context]', effort: null, model: null });
});

test('exposes literal context qualifiers without mistaking effort for context', () => {
  assert.equal(modelContextQualifier('claude-fable-5[1m]'), '1M');
  assert.equal(modelContextQualifier('vendor-model[long-context]'), 'LONG-CONTEXT');
  assert.equal(modelContextQualifier('gpt-5.6-sol[xhigh]'), null);
  assert.equal(modelContextQualifier('sonnet'), null);
});
