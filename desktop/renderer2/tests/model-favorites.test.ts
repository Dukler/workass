import assert from 'node:assert/strict';
import test from 'node:test';
import {
  MODEL_FAVORITES_SETTINGS_KEY, favoriteCatalogModels, isModelFavorite,
  modelFavoritesFromSettings, parseModelFavorites, toggleModelFavorite,
  withModelFavoritesInSettings,
} from '../src/model-favorites.ts';
import type { CatalogGroup } from '../src/wire/types.ts';

const groups: CatalogGroup[] = [
  { providerId: 'mock', providerName: 'Mock', status: 'ready', modes: [], models: [{ modelId: 'mock-deterministic', name: 'Mock' }] },
  { providerId: 'claude', providerName: 'Claude', status: 'ready', modes: [], models: [{ modelId: 'opus[1m]', name: 'Opus 4.8' }] },
  { providerId: 'codex', providerName: 'Codex', status: 'ready', modes: [], models: [{ modelId: 'gpt-5.6-sol', name: 'GPT-5.6-Sol' }] },
];

test('favorites parse, deduplicate, and survive the settings envelope', () => {
  const raw = [
    { providerId: 'codex', modelId: 'gpt-5.6-sol' },
    { providerId: 'codex', modelId: 'gpt-5.6-sol' },
    { providerId: ' claude ', modelId: ' opus[1m] ' },
    { providerId: '', modelId: 'bad' },
    null,
  ];
  const parsed = parseModelFavorites(raw);
  assert.deepEqual(parsed, [
    { providerId: 'codex', modelId: 'gpt-5.6-sol' },
    { providerId: 'claude', modelId: 'opus[1m]' },
  ]);
  assert.deepEqual(modelFavoritesFromSettings({ settings: { [MODEL_FAVORITES_SETTINGS_KEY]: raw } }), parsed);
});

test('toggle adds at the end and removes the exact provider plus model pair', () => {
  const first = toggleModelFavorite([], 'codex', 'gpt-5.6-sol');
  const second = toggleModelFavorite(first, 'claude', 'opus[1m]');
  assert.equal(isModelFavorite(second, 'codex', 'gpt-5.6-sol'), true);
  assert.deepEqual(toggleModelFavorite(second, 'codex', 'gpt-5.6-sol'), [{ providerId: 'claude', modelId: 'opus[1m]' }]);
});

test('favorites resolve only against the authoritative grouped catalog', () => {
  const resolved = favoriteCatalogModels(groups, [
    { providerId: 'codex', modelId: 'gpt-5.6-sol' },
    { providerId: 'missing', modelId: 'gone' },
    { providerId: 'mock', modelId: 'mock-deterministic' },
    { providerId: 'claude', modelId: 'opus[1m]' },
  ]);
  assert.deepEqual(resolved.map(({ providerId, modelId }) => [providerId, modelId]), [
    ['codex', 'gpt-5.6-sol'], ['mock', 'mock-deterministic'], ['claude', 'opus[1m]'],
  ]);
});

test('settings write preserves unrelated fields and replaces only favorites', () => {
  assert.deepEqual(withModelFavoritesInSettings(
    { chatMode: 'ask', modelScores: { codex: {} }, modelFavorites: [{ providerId: 'old', modelId: 'old' }] },
    [{ providerId: 'codex', modelId: 'gpt-5.6-sol' }],
  ), {
    chatMode: 'ask', modelScores: { codex: {} }, modelFavorites: [{ providerId: 'codex', modelId: 'gpt-5.6-sol' }],
  });
});
