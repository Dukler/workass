import test from 'node:test';
import assert from 'node:assert/strict';
import { brandForProvider, updateChainAfterCheck } from '../src/update-card.ts';

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
