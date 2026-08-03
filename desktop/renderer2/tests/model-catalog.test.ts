import assert from 'node:assert/strict';
import test from 'node:test';
import { isUserFacingModel, isUserFacingSubagentIdentity, userFacingCatalogGroups, userFacingFlatModels, userFacingProviders } from '../src/model-catalog.ts';
import type { CatalogGroup, ProviderRecord } from '../src/wire/types.ts';

const groups: CatalogGroup[] = [
  { providerId: 'mock', providerName: 'Workass Mock ACP', status: 'ready', modes: [], models: [
    { modelId: 'mock-deterministic', name: 'Mock deterministic' },
  ] },
  { providerId: 'qwen', providerName: 'Qwen Code ACP', status: 'ready', modes: [], models: [
    { modelId: 'coder-model(qwen-oauth)', name: 'coder-model' },
    { modelId: '$runtime|openai|workass-dev(openai)', name: 'workass-dev' },
    { modelId: 'qwen3-coder', name: 'Qwen3 Coder' },
  ] },
  { providerId: 'local-lmstudio', providerName: 'LM Studio', status: 'ready', modes: [], models: [
    { modelId: 'workass-dev', name: 'workass-dev' },
    { modelId: 'user-model', name: 'My local model' },
  ] },
  { providerId: 'codex', providerName: 'Codex', status: 'ready', modes: [], models: [
    { modelId: 'gpt-preview', name: 'GPT Preview' },
  ] },
];

test('production removes deterministic mock and local smoke-test models from user-facing groups', () => {
  const visible = userFacingCatalogGroups(groups, 'prod');
  assert.deepEqual(visible.map((group) => [group.providerId, group.models.map((model) => model.modelId)]), [
    ['qwen', ['qwen3-coder']],
    ['local-lmstudio', ['user-model']],
    ['codex', ['gpt-preview']],
  ]);
});

test('development and test profiles expose the complete fixture catalog', () => {
  for (const profile of ['dev', 'test'] as const) {
    assert.deepEqual(userFacingCatalogGroups(groups, profile), groups);
    assert.deepEqual(userFacingFlatModels(groups.flatMap((group) => group.models), profile), groups.flatMap((group) => group.models));
  }
});

test('does not use broad preview or local-provider heuristics', () => {
  assert.equal(isUserFacingModel('codex', { modelId: 'gpt-preview', name: 'GPT Preview' }), true);
  assert.equal(isUserFacingModel('local-lmstudio', { modelId: 'user-model', name: 'My local model' }), true);
});

test('production flat fallback removes identity-known fixture models without guessing a provider', () => {
  assert.deepEqual(userFacingFlatModels(groups.flatMap((group) => group.models), 'prod').map((model) => model.modelId), [
    'qwen3-coder', 'user-model', 'gpt-preview',
  ]);
});

test('production hides fixture providers and legacy fixture subagent labels from every visible surface', () => {
  const providers: ProviderRecord[] = [
    { id: 'mock', name: 'Workass Mock ACP', enabled: true, status: 'ready' },
    { id: 'codex', name: 'Codex ACP', enabled: true, status: 'ready' },
  ];
  assert.deepEqual(userFacingProviders(providers, 'prod').map((provider) => provider.id), ['codex']);
  assert.deepEqual(userFacingProviders(providers, 'dev').map((provider) => provider.id), ['mock', 'codex']);
  assert.equal(isUserFacingSubagentIdentity('', 'Mockdeterministic-low', 'prod'), false);
  assert.equal(isUserFacingSubagentIdentity('qwen', 'coder-model', 'prod'), false);
  assert.equal(isUserFacingSubagentIdentity('gpt', 'GPT-5.6-Sol-xhigh', 'prod'), true);
  assert.equal(isUserFacingSubagentIdentity('', 'Mockdeterministic-low', 'dev'), true);
});
