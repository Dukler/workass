import test from 'node:test';
import assert from 'node:assert/strict';
import { nextUpdatePhase, brandForProvider, updateFailureTitle, updateFailureReason, updateChainAfterCheck, DONE_HOLD_MS, EXIT_MS } from '../src/update-card.ts';

test('provider ids resolve to the same brand marks the daemon uses', () => {
  const providers = [
    { id: 'alpha', name: 'Alpha', assistantBrand: 'gpt', enabled: true, status: 'ready' },
    { id: 'beta', name: 'Beta', assistantBrand: 'claude', enabled: true, status: 'ready' },
  ];
  const groups = [
    { providerId: 'gamma', providerName: 'Gamma', assistantBrand: 'qwen', models: [], modes: [] },
  ];
  assert.equal(brandForProvider('alpha', providers, groups), 'gpt');
  assert.equal(brandForProvider('beta', providers, groups), 'claude');
  assert.equal(brandForProvider('gamma', providers, groups), 'qwen');
  assert.equal(brandForProvider('missing', providers, groups), '');
  assert.equal(brandForProvider(null), '');
});

test('an in-flight update shows the running card from any prior phase', () => {
  assert.equal(nextUpdatePhase('hidden', { active: true, pending: 1 }), 'running');
  assert.equal(nextUpdatePhase('resting', { active: true, pending: 2 }), 'running');
  assert.equal(nextUpdatePhase('done', { active: true, pending: 0 }), 'running');
  assert.equal(nextUpdatePhase('exiting', { active: true, pending: 0 }), 'running');
});

test('finishing the last update seals the card into done', () => {
  assert.equal(nextUpdatePhase('running', { active: false, pending: 0 }), 'done');
});

test('leaving an update with work still pending falls back to resting, not done', () => {
  // A failed CLI stays advertised as updateable, so the success seal must not fire.
  assert.equal(nextUpdatePhase('running', { active: false, pending: 1 }), 'resting');
});

test('done and exiting are held until the component timers advance them', () => {
  assert.equal(nextUpdatePhase('done', { active: false, pending: 0 }), 'done');
  assert.equal(nextUpdatePhase('exiting', { active: false, pending: 0 }), 'exiting');
});

test('resting appears only while updates remain, else hidden', () => {
  assert.equal(nextUpdatePhase('hidden', { active: false, pending: 3 }), 'resting');
  assert.equal(nextUpdatePhase('resting', { active: false, pending: 1 }), 'resting');
  assert.equal(nextUpdatePhase('hidden', { active: false, pending: 0 }), 'hidden');
  assert.equal(nextUpdatePhase('resting', { active: false, pending: 0 }), 'hidden');
});

test('timing constants keep the seal hold and slide-out distinct and short', () => {
  assert.equal(DONE_HOLD_MS, 500);
  assert.ok(EXIT_MS > 0 && EXIT_MS < DONE_HOLD_MS);
});

test('a failed update says why, and an exit status only when one means something', () => {
  const noChange = { providerId: 'claude', status: 'failed', startedAt: '', tail: '', exitCode: 0, error: 'la actualización terminó sin instalar claude' };
  assert.equal(updateFailureTitle('claude', noChange), 'claude: error');
  assert.equal(updateFailureReason(noChange, undefined), 'la actualización terminó sin instalar claude');
  const killed = { ...noChange, exitCode: -1, error: 'signal: killed' };
  assert.equal(updateFailureTitle('claude', killed), 'claude: error (exit -1)');
  assert.equal(updateFailureTitle('claude', undefined), 'claude: error');
  const update = { providerId: 'claude', cli: 'claude', installed: '1', latest: '2', updateAvailable: true, lastError: 'desde el daemon' };
  assert.equal(updateFailureReason(undefined, update), 'desde el daemon');
});

test('a failed step leaves the card once the provider no longer offers an update', () => {
  const chain = { ids: ['claude'], index: 1, failedId: 'claude' };
  const offer = { providerId: 'claude', cli: 'claude', installed: '1', latest: '2', updateAvailable: true };
  assert.equal(updateChainAfterCheck(chain, [offer]), chain);
  assert.equal(updateChainAfterCheck(chain, [{ ...offer, updateAvailable: false }]), undefined);
  assert.equal(updateChainAfterCheck(chain, []), undefined);
  const walking = { ids: ['claude'], index: 1, failedId: null };
  assert.equal(updateChainAfterCheck(walking, []), walking);
  assert.equal(updateChainAfterCheck(undefined, [offer]), undefined);
});
