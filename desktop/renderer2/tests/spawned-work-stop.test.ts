import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const card = readFileSync(new URL('../src/components/SpawnedWorkCard.tsx', import.meta.url), 'utf8');
const styles = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
const store = readFileSync(new URL('../src/store/store.ts', import.meta.url), 'utf8');
const router = readFileSync(new URL('../src/wire/machineRouter.ts', import.meta.url), 'utf8');
const bridge = readFileSync(new URL('../../../internal/httpserve/lan_bridge.go', import.meta.url), 'utf8');
const mutating = readFileSync(new URL('../../../internal/wire/wire.go', import.meta.url), 'utf8');

test('a running background row carries a stop square, and only a running one', () => {
  const running = card.slice(card.indexOf('function RunningRow'), card.indexOf('export function SpawnedWorkLive'));
  assert.match(running, /className="bgr-stop"/);
  assert.match(running, /<IcStopSquare \/>/);
  assert.match(running, /store\.stopSpawnedWork\(chat, item\.id\)/);
  // The finished fold is flat lines of title + duration. Nothing there is
  // running, so nothing there may offer to stop it.
  const finished = card.slice(card.indexOf('export function SpawnedWorkCard'));
  assert.doesNotMatch(finished, /bgr-stop/);
});

test('the stop square is feature-detected, so an older daemon draws no dead button', () => {
  assert.match(card, /const canStop = store\.canStopSpawnedWork\(\);/);
  assert.match(card, /\{canStop && \(/);
  assert.match(store, /canStopSpawnedWork\(\): boolean \{ return has\('spawnedWorkStop'\); \}/);
});

test('a second press cannot land while the first is in flight', () => {
  assert.match(card, /if \(stopping\) return;/);
  assert.match(card, /disabled=\{stopping\}/);
});

test('the stop square is neutral: no red, no failure wording on this card', () => {
  const rule = styles.slice(styles.indexOf('.bgr-stop {'), styles.indexOf('.bgr-stop svg'));
  assert.doesNotMatch(rule, /--(err|danger|red|warn)/);
  assert.doesNotMatch(rule, /#[0-9a-f]{3,6}/i);
  assert.match(styles, /\.bgr-stop:disabled \{[^}]*cursor: default;/);
});

test('stopping is routable to the machine the process actually runs on', () => {
  assert.match(router, /\['spawnedWorkStop', 'spawned-work:stop'/);
  assert.match(bridge, /spawnedWorkStop: \(tabId, chatId, id\) => invoke\('spawned-work:stop'/);
});

test('killing a process needs the controller lease, like every other kill channel', () => {
  const table = mutating.slice(mutating.indexOf('var mutatingChannels'), mutating.indexOf('var controllerOnlyEventChannels'));
  assert.match(table, /"spawned-work:stop":\s*\{\},/);
  // The read channels stay open to any approved device — a watching phone must
  // keep seeing what is running while another device holds the lease.
  assert.doesNotMatch(table, /"spawned-work:(list|read)"/);
});
