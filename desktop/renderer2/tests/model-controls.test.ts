import assert from 'node:assert/strict';
import test from 'node:test';
import {
  compatibleEffortId, compatibleModeId, composeModelSelection,
  imageDraftCapability,
  modelControlsChangedDuringInit, nextModelControlRevision,
  normalizeModelControlMemory, rememberModelControls, rememberedModelControls,
  permissionIntentForMode,
  restoredProviderBinding,
  restoredControlSelection,
  type ModelControlMemory,
} from '../src/model-controls.ts';
import type { ModeOption, ModelOption, PermissionIntent } from '../src/wire/types.ts';

const codexModel: ModelOption = { modelId: 'gpt-5.6-sol', name: 'GPT-5.6 Sol', efforts: ['low', 'high', 'xhigh', 'max'] };
const claudeModel: ModelOption = { modelId: 'opus[1m]', name: 'Opus 4.8', efforts: ['low', 'medium', 'high', 'xhigh', 'max'] };
const codexModes: ModeOption[] = [
  { id: 'read-only', name: 'Read only' }, { id: 'agent', name: 'Agent' }, { id: 'agent-full-access', name: 'Full access' },
];
const claudeModes: ModeOption[] = [
  { id: 'default', name: 'Default' }, { id: 'plan', name: 'Plan' }, { id: 'bypassPermissions', name: 'Bypass' },
];
const codexIntents = {
  read: 'read-only', edit: 'agent', full: 'agent-full-access',
} satisfies Partial<Record<PermissionIntent, string>>;
const claudeIntents = {
  read: 'plan', edit: 'default', full: 'bypassPermissions',
} satisfies Partial<Record<PermissionIntent, string>>;

test('one chat restores the last permission and effort for each provider/model', () => {
  let memory: ModelControlMemory | undefined;
  memory = rememberModelControls(memory, 'codex', codexModel.modelId, { effort: 'xhigh', modeId: 'agent-full-access' });

  // Provider-native ids translate only through typed adapter intent metadata.
  assert.equal(compatibleModeId(
    'agent-full-access', claudeModes, 'default',
    permissionIntentForMode(codexIntents, 'agent-full-access'), claudeIntents,
  ), 'bypassPermissions');
  assert.equal(compatibleEffortId(undefined, claudeModel, 'xhigh'), 'xhigh');
  memory = rememberModelControls(memory, 'claude', claudeModel.modelId, { effort: 'high', modeId: 'bypassPermissions' });

  const codex = rememberedModelControls(memory, 'codex', codexModel.modelId);
  const claude = rememberedModelControls(memory, 'claude', claudeModel.modelId);
  assert.deepEqual(codex, { effort: 'xhigh', modeId: 'agent-full-access' });
  assert.deepEqual(claude, { effort: 'high', modeId: 'bypassPermissions' });
  assert.equal(composeModelSelection(codexModel.modelId, codex?.effort ?? null), 'gpt-5.6-sol[xhigh]');
  assert.equal(composeModelSelection(claudeModel.modelId, claude?.effort ?? null), 'opus[1m][high]');
});

test('model control memory is isolated per chat', () => {
  const chatA = rememberModelControls(undefined, 'codex', codexModel.modelId, { effort: 'low', modeId: 'agent' });
  const chatB = rememberModelControls(undefined, 'codex', codexModel.modelId, { effort: 'max', modeId: 'agent-full-access' });
  assert.deepEqual(rememberedModelControls(chatA, 'codex', codexModel.modelId), { effort: 'low', modeId: 'agent' });
  assert.deepEqual(rememberedModelControls(chatB, 'codex', codexModel.modelId), { effort: 'max', modeId: 'agent-full-access' });
});

test('model control memory survives the JSON reload boundary', () => {
  const memory = rememberModelControls(undefined, 'codex', codexModel.modelId, { effort: 'xhigh', modeId: 'agent-full-access' });
  const restored = normalizeModelControlMemory(JSON.parse(JSON.stringify(memory)));
  assert.deepEqual(restored, memory);
});

test('reload uses live ACP controls instead of stale persisted provider controls', () => {
  assert.deepEqual(
    restoredControlSelection('gpt-5.6-sol[xhigh]', 'bypassPermissions', {
      modelId: 'gpt-5.6-sol[high]', modeId: 'agent-full-access',
    }),
    { modelId: 'gpt-5.6-sol[high]', modeId: 'agent-full-access' },
  );
  assert.deepEqual(
    restoredControlSelection('gpt-5.6-sol[xhigh]', 'agent-full-access'),
    { modelId: 'gpt-5.6-sol[xhigh]', modeId: 'agent-full-access' },
    'after a daemon restart there is no live binding, so the chat cache survives',
  );
  assert.deepEqual(
    restoredControlSelection('stale-model', 'bypassPermissions', { modelId: null, modeId: null }),
    { modelId: null, modeId: null },
    'a live adapter that exposes no controls must not inherit stale persisted ids',
  );
});

test('permission translation is driven by typed adapter intent metadata', () => {
  assert.equal(compatibleModeId(
    'bypassPermissions', codexModes, 'agent',
    permissionIntentForMode(claudeIntents, 'bypassPermissions'), codexIntents,
  ), 'agent-full-access');
  assert.equal(compatibleModeId(
    'plan', codexModes, 'agent',
    permissionIntentForMode(claudeIntents, 'plan'), codexIntents,
  ), 'read-only');
  assert.equal(compatibleModeId('agent-full-access', codexModes, 'agent'), 'agent-full-access');
  assert.equal(compatibleModeId('unknown-old-mode', codexModes, 'agent'), 'agent');
});

test('invalid remembered effort falls back inside the selected model vocabulary', () => {
  assert.equal(compatibleEffortId('ultra', codexModel, 'xhigh'), 'xhigh');
  assert.equal(compatibleEffortId('ultra', codexModel), 'high');
});

test('an explicit picker change wins over an in-flight inherited-provider initialization', () => {
  const initRevision = 0;
  const pickedRevision = nextModelControlRevision(initRevision);
  assert.equal(modelControlsChangedDuringInit(initRevision, pickedRevision), true);
  assert.equal(modelControlsChangedDuringInit(pickedRevision, pickedRevision), false);
});

test('a stale provider session never turns unresolved image support into a rejection', () => {
  assert.equal(imageDraftCapability(null, null, 'claude', false), 'unknown',
    'a brand-new chat may accept the image while its selected provider initializes');
  assert.equal(imageDraftCapability('codex-session', 'codex', 'claude', false), 'unknown',
    'the previous provider capability says nothing about the newly selected provider');
  assert.equal(imageDraftCapability('codex-session', 'codex', 'codex', undefined), 'unknown',
    'a matching session without an additive capability field is still unresolved');
  assert.equal(imageDraftCapability('claude-session', 'claude', 'claude', true), 'supported');
  assert.equal(imageDraftCapability('claude-session', 'claude', 'claude', false), 'unsupported');
});

test('reload keeps a staged provider pick separate from the provider owning the live session', () => {
  assert.deepEqual(restoredProviderBinding('claude', 'codex'), {
    selectedProviderId: 'claude',
    sessionProviderId: 'codex',
    useLiveControls: false,
  });
  assert.deepEqual(restoredProviderBinding(null, 'codex'), {
    selectedProviderId: 'codex',
    sessionProviderId: 'codex',
    useLiveControls: true,
  });
  assert.deepEqual(restoredProviderBinding('claude', 'claude'), {
    selectedProviderId: 'claude',
    sessionProviderId: 'claude',
    useLiveControls: true,
  });
});
