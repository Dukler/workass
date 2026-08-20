import assert from 'node:assert/strict';
import test from 'node:test';
import type { CatalogGroup, ModelOption, ProviderRecord } from '../src/wire/types.ts';
import {
  clampScore, clearAllModelScores, clearModelScore, countScoredModels,
  getModelScore, groupModelsForScoring, isEmptyScore,
  MODEL_SCORES_SETTINGS_KEY, modelScoresFromSettings,
  NOTE_MAX, normalizeNote, parseModelScores, sanitizeScore, SCORE_DIMENSIONS, SCORE_MAX, SCORE_MIN,
  serializeModelScores, settingsFromReply, withModelScoresInSettings, withNoteValue, withScoreValue,
  type ModelScores,
} from '../src/model-scores.ts';

// ---- grouping -------------------------------------------------------------

const claudeModels: ModelOption[] = [
  { modelId: 'opus[1m]', name: 'Opus 4.8' },
  { modelId: 'sonnet', name: 'Sonnet 5' },
  { modelId: 'opus[1m]', name: 'Opus 4.8 (dup)' }, // duplicate id
];
const codexModels: ModelOption[] = [{ modelId: 'gpt-5.6-sol', name: 'GPT-5.6 Sol' }];

test('grouping prefers provider groups, dedups models, and resolves provider names', () => {
  const groups: CatalogGroup[] = [
    { providerId: 'claude', providerName: 'Claude', models: claudeModels, modes: [] },
    { providerId: 'codex', providerName: '', models: codexModels, modes: [] },
  ];
  const providers: ProviderRecord[] = [{ id: 'codex', name: 'Codex ACP', enabled: true, status: 'ready' }];
  const out = groupModelsForScoring(groups, providers);
  assert.equal(out.length, 2);
  assert.deepEqual(out[0], {
    providerId: 'claude', providerName: 'Claude',
    models: [{ modelId: 'opus[1m]', name: 'Opus 4.8' }, { modelId: 'sonnet', name: 'Sonnet 5' }],
  });
  // Empty group providerName falls back to the providers-list name.
  assert.equal(out[1].providerName, 'Codex ACP');
});

test('grouping drops empty groups and models missing an id', () => {
  const groups: CatalogGroup[] = [
    { providerId: 'empty', providerName: 'Empty', models: [], modes: [] },
    { providerId: 'x', providerName: 'X', models: [{ modelId: '', name: 'nameless' } as ModelOption, { modelId: 'ok', name: 'Ok' }], modes: [] },
  ];
  const out = groupModelsForScoring(groups);
  assert.equal(out.length, 1);
  assert.deepEqual(out[0].models, [{ modelId: 'ok', name: 'Ok' }]);
});

test('model preference surfaces trust the authoritative grouped catalog', () => {
  const out = groupModelsForScoring([
    { providerId: 'mock', providerName: 'Mock', models: [{ modelId: 'mock-deterministic', name: 'Mock deterministic' }], modes: [] },
    { providerId: 'qwen', providerName: 'Qwen', models: [
      { modelId: '$runtime|openai|workass-dev(openai)', name: 'workass-dev' },
      { modelId: 'qwen3-coder', name: 'Qwen3 Coder' },
    ], modes: [] },
  ]);
  assert.deepEqual(out.map((group) => [group.providerId, group.models.map((model) => model.modelId)]), [
    ['mock', ['mock-deterministic']], ['qwen', ['$runtime|openai|workass-dev(openai)', 'qwen3-coder']],
  ]);
});

test('grouping returns nothing when there is no catalog', () => {
  assert.deepEqual(groupModelsForScoring([]), []);
});

// ---- validation / clamping ------------------------------------------------

test('clampScore rounds and clamps to the whole-number 1..10 scale', () => {
  assert.equal(SCORE_MIN, 1);
  assert.equal(SCORE_MAX, 10);
  assert.equal(clampScore(7), 7);
  assert.equal(clampScore('8'), 8);
  assert.equal(clampScore(6.4), 6);
  assert.equal(clampScore(6.5), 7);
  assert.equal(clampScore(-3), SCORE_MIN);
  assert.equal(clampScore(999), SCORE_MAX);
  // 0 is below the 1..10 rubric — it clamps UP to the minimum, never stays 0.
  assert.equal(clampScore(0), SCORE_MIN);
  assert.equal(clampScore(1), 1);
  assert.equal(clampScore(10), 10);
});

test('clampScore treats blank/invalid input as unscored (undefined), never 0', () => {
  assert.equal(clampScore(''), undefined);
  assert.equal(clampScore('   '), undefined);
  assert.equal(clampScore(null), undefined);
  assert.equal(clampScore(undefined), undefined);
  assert.equal(clampScore('abc'), undefined);
  assert.equal(clampScore(NaN), undefined);
  assert.equal(clampScore(Infinity), undefined);
});

test('normalizeNote trims, caps length at 500, and drops empties', () => {
  assert.equal(NOTE_MAX, 500);
  assert.equal(normalizeNote('  fast for edits  '), 'fast for edits');
  assert.equal(normalizeNote('   '), undefined);
  assert.equal(normalizeNote(42), undefined);
  assert.equal(normalizeNote('x'.repeat(NOTE_MAX + 50))?.length, NOTE_MAX);
});

test('the numeric dimensions are exactly the daemon wire keys', () => {
  assert.deepEqual(SCORE_DIMENSIONS.map((d) => d.key), ['intelligence', 'taste', 'cost']);
});

test('sanitizeScore clamps every dimension, keeps the note, and drops empty', () => {
  assert.equal(sanitizeScore({}), undefined);
  assert.equal(sanitizeScore({ intelligence: '', note: '   ' }), undefined);
  // 12 clamps down to 10; -1 clamps up to the 1 minimum; unknown keys dropped.
  assert.deepEqual(sanitizeScore({ intelligence: 12, taste: -1, bogus: 5, note: ' n ' }), { intelligence: 10, taste: 1, note: 'n' });
  assert.equal(sanitizeScore('nope'), undefined);
});

test('isEmptyScore recognizes the unscored state', () => {
  assert.equal(isEmptyScore(undefined), true);
  assert.equal(isEmptyScore({}), true);
  assert.equal(isEmptyScore({ note: '' } as never), true);
  assert.equal(isEmptyScore({ cost: 1 }), false); // any set dimension is a real score
  assert.equal(isEmptyScore({ note: 'x' }), false);
});

// ---- immutable updates ----------------------------------------------------

test('withScoreValue sets, clamps, and prunes down to unscored', () => {
  let scores: ModelScores = {};
  scores = withScoreValue(scores, 'claude', 'opus[1m]', 'cost', 12);
  assert.deepEqual(getModelScore(scores, 'claude', 'opus[1m]'), { cost: 10 });
  scores = withScoreValue(scores, 'claude', 'opus[1m]', 'intelligence', 8);
  assert.deepEqual(getModelScore(scores, 'claude', 'opus[1m]'), { cost: 10, intelligence: 8 });
  // Clearing one dimension keeps the rest.
  scores = withScoreValue(scores, 'claude', 'opus[1m]', 'cost', '');
  assert.deepEqual(getModelScore(scores, 'claude', 'opus[1m]'), { intelligence: 8 });
  // Clearing the last dimension prunes the model AND the now-empty provider.
  scores = withScoreValue(scores, 'claude', 'opus[1m]', 'intelligence', '');
  assert.deepEqual(scores, {});
});

test('withNoteValue sets and clears the freeform note', () => {
  let scores: ModelScores = withScoreValue({}, 'codex', 'gpt-5.6-sol', 'taste', 9);
  scores = withNoteValue(scores, 'codex', 'gpt-5.6-sol', '  tight-spec local smart  ');
  assert.deepEqual(getModelScore(scores, 'codex', 'gpt-5.6-sol'), { taste: 9, note: '  tight-spec local smart  ' });
  scores = withNoteValue(scores, 'codex', 'gpt-5.6-sol', '');
  assert.deepEqual(getModelScore(scores, 'codex', 'gpt-5.6-sol'), { taste: 9 });
});

test('controlled note typing preserves a space before the following character', () => {
  let scores: ModelScores = {};
  const type = (char: string) => {
    const current = getModelScore(scores, 'claude', 'fable')?.note ?? '';
    scores = withNoteValue(scores, 'claude', 'fable', current + char);
  };
  for (const char of 'never use as subagent') type(char);
  assert.equal(getModelScore(scores, 'claude', 'fable')?.note, 'never use as subagent');
  // Draft whitespace is normalized only at the persistence boundary.
  assert.equal(serializeModelScores(withNoteValue(scores, 'claude', 'fable', '  never use as subagent  ')).claude.fable.note, 'never use as subagent');
});

test('updates never mutate the previous scores object (immutability)', () => {
  const before: ModelScores = { claude: { 'opus[1m]': { intelligence: 5 } } };
  const after = withScoreValue(before, 'claude', 'opus[1m]', 'intelligence', 9);
  assert.deepEqual(before, { claude: { 'opus[1m]': { intelligence: 5 } } });
  assert.deepEqual(after, { claude: { 'opus[1m]': { intelligence: 9 } } });
  assert.notEqual(before, after);
});

test('an invalid id is rejected without cloning; clearing an unset dimension is harmless', () => {
  const scores: ModelScores = { claude: { sonnet: { intelligence: 7 } } };
  // A blank provider/model id is rejected and returns the same reference.
  assert.equal(withScoreValue(scores, '', 'sonnet', 'intelligence', 5), scores);
  assert.equal(withScoreValue(scores, 'claude', '', 'intelligence', 5), scores);
  // Clearing a dimension that was never set leaves the structure unchanged.
  assert.deepEqual(withScoreValue(scores, 'claude', 'sonnet', 'cost', ''), scores);
});

// ---- reset ----------------------------------------------------------------

test('clearModelScore resets one model and prunes an emptied provider', () => {
  const scores: ModelScores = { claude: { 'opus[1m]': { intelligence: 8 }, sonnet: { taste: 6 } }, codex: { 'gpt-5.6-sol': { cost: 10 } } };
  const afterOne = clearModelScore(scores, 'claude', 'opus[1m]');
  assert.deepEqual(afterOne, { claude: { sonnet: { taste: 6 } }, codex: { 'gpt-5.6-sol': { cost: 10 } } });
  const afterLast = clearModelScore(afterOne, 'claude', 'sonnet');
  assert.deepEqual(afterLast, { codex: { 'gpt-5.6-sol': { cost: 10 } } }); // empty 'claude' pruned
  // Resetting a model that has no score is a no-op (same reference).
  assert.equal(clearModelScore(afterLast, 'claude', 'sonnet'), afterLast);
});

test('clearAllModelScores wipes everything', () => {
  assert.deepEqual(clearAllModelScores(), {});
});

test('countScoredModels counts models across providers', () => {
  assert.equal(countScoredModels({}), 0);
  assert.equal(countScoredModels({ claude: { 'opus[1m]': { intelligence: 8 }, sonnet: { taste: 6 } }, codex: { 'gpt-5.6-sol': { cost: 10 } } }), 3);
});

// ---- serialization --------------------------------------------------------

test('parseModelScores sanitizes, clamps, and drops empty entries/providers', () => {
  const raw = {
    claude: {
      'opus[1m]': { intelligence: 12, taste: -4, bogus: 3, note: '  n  ' },
      sonnet: { intelligence: '', note: '   ' }, // fully empty → dropped
    },
    empty: {}, // no models → dropped
    bad: 'not-an-object', // ignored
    '': { m: { intelligence: 5 } }, // blank provider id → ignored
  };
  // 12 → 10, -4 → 1 (min of the 1..10 rubric), note trimmed.
  assert.deepEqual(parseModelScores(raw), { claude: { 'opus[1m]': { intelligence: 10, taste: 1, note: 'n' } } });
  assert.deepEqual(parseModelScores(null), {});
  assert.deepEqual(parseModelScores('x'), {});
});

test('serializeModelScores round-trips a clean map and is idempotent', () => {
  const clean: ModelScores = { claude: { 'opus[1m]': { intelligence: 9, cost: 8, note: 'daily driver' } } };
  const once = serializeModelScores(clean);
  assert.deepEqual(once, clean);
  // Serialize → JSON → parse survives the daemon settings boundary intact, and
  // the on-the-wire keys are exactly the daemon contract (intelligence/taste/cost).
  const json = JSON.parse(JSON.stringify(once));
  assert.deepEqual(Object.keys(json.claude['opus[1m]']).sort(), ['cost', 'intelligence', 'note']);
  const roundTrip = parseModelScores(json);
  assert.deepEqual(roundTrip, clean);
  // Idempotent.
  assert.deepEqual(serializeModelScores(roundTrip), clean);
});

test('a below-range score clamps up to the minimum rather than persisting 0', () => {
  // 0 is outside the 1..10 rubric — it is lifted to SCORE_MIN, never stored as 0.
  const parsed = parseModelScores({ codex: { 'gpt-5.6-sol': { taste: 0 } } });
  assert.deepEqual(parsed, { codex: { 'gpt-5.6-sol': { taste: SCORE_MIN } } });
  // A min-boundary rating survives serialization unchanged.
  const scores: ModelScores = { codex: { 'gpt-5.6-sol': { taste: 1 } } };
  assert.deepEqual(parseModelScores(JSON.parse(JSON.stringify(scores))), scores);
});

// ---- settings blob boundary (settings:get / settings:set) -----------------

test('settingsFromReply unwraps the settings:get envelope and tolerates junk', () => {
  // settings:get replies wrap the blob as { settings, models, modes, taskKinds }.
  const inner = { chatMode: 'chat', modelScores: {} };
  assert.equal(settingsFromReply({ settings: inner, models: [], modes: [], taskKinds: [] }), inner);
  // Missing/malformed replies degrade to {} — a read never throws.
  assert.deepEqual(settingsFromReply(null), {});
  assert.deepEqual(settingsFromReply('nope'), {});
  assert.deepEqual(settingsFromReply({ settings: 'bad' }), {});
  assert.deepEqual(settingsFromReply({}), {});
});

test('modelScoresFromSettings reads + sanitizes scores out of a settings:get reply', () => {
  const reply = {
    settings: {
      chatMode: 'chat',
      modelScores: {
        claude: { 'opus[1m]': { intelligence: 12, taste: 7, note: '  daily  ' } },
        gone: { m: { note: '   ' } }, // empty → dropped
      },
    },
    models: [], modes: [], taskKinds: [],
  };
  assert.deepEqual(modelScoresFromSettings(reply), {
    claude: { 'opus[1m]': { intelligence: 10, taste: 7, note: 'daily' } },
  });
  // No scores present, or a malformed reply → empty map (hydrate to unscored).
  assert.deepEqual(modelScoresFromSettings({ settings: { chatMode: 'chat' } }), {});
  assert.deepEqual(modelScoresFromSettings(undefined), {});
});

test('withModelScoresInSettings preserves every other key and only replaces modelScores', () => {
  // Round-tripping the WHOLE blob is what prevents settings:set (which REPLACES,
  // not merges) from wiping the keys other surfaces own — plus any unknown key.
  const current = {
    version: 1,
    models: { chat: 'opus' },
    permissionModes: { review: 'auto' },
    chatMode: 'chat',
    prApprover: 'someone',
    futureUnknownKey: { keep: 'me' },
    modelScores: { claude: { sonnet: { taste: 3 } } }, // will be overwritten
  };
  const scores: ModelScores = { codex: { 'gpt-5.6-sol': { intelligence: 9, cost: 8 } } };
  const out = withModelScoresInSettings(current, scores);
  assert.deepEqual(out, {
    version: 1,
    models: { chat: 'opus' },
    permissionModes: { review: 'auto' },
    chatMode: 'chat',
    prApprover: 'someone',
    futureUnknownKey: { keep: 'me' },
    modelScores: { codex: { 'gpt-5.6-sol': { intelligence: 9, cost: 8 } } },
  });
  // The scores slice is sanitized on the way out, and lives under the wire key.
  const sanitized = withModelScoresInSettings({}, { codex: { 'gpt-5.6-sol': { cost: 42 } } });
  assert.deepEqual(sanitized[MODEL_SCORES_SETTINGS_KEY], { codex: { 'gpt-5.6-sol': { cost: 10 } } });
  // A null/undefined current still yields a well-formed blob.
  assert.deepEqual(withModelScoresInSettings(null, {}), { modelScores: {} });
});

test('clearing every score still writes an explicit empty modelScores (never omitted)', () => {
  // A full reset must persist {} so the daemon clears the stored scores; it must
  // not silently drop the key and leave the old scores behind.
  const out = withModelScoresInSettings({ chatMode: 'chat' }, clearAllModelScores());
  assert.deepEqual(out, { chatMode: 'chat', modelScores: {} });
});
