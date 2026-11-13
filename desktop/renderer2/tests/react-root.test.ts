import assert from 'node:assert/strict';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { SKIPPED_ROOT_EVENTS, withoutRootListeners } from '../src/react-root.ts';

// The app's React roots skip the animationiteration listener so looping CSS
// indicators stay on the compositor (energy lane, 2026-11-13).
test('only the listeners added while the root is created skip animationiteration', () => {
  const added: string[] = [];
  const proto = { addEventListener(type: string) { added.push(type); } } as unknown as EventTarget;
  const original = proto.addEventListener;
  const result = withoutRootListeners(proto, () => {
    for (const type of ['click', 'animationiteration', 'animationstart', 'animationend', 'keydown']) proto.addEventListener(type, null);
    return 'root';
  });
  assert.equal(result, 'root');
  assert.deepEqual(added, ['click', 'animationstart', 'animationend', 'keydown']);
  assert.equal(proto.addEventListener, original, 'restored after the root exists');
  proto.addEventListener('animationiteration', null);
  assert.deepEqual(added.at(-1), 'animationiteration', 'later listeners are untouched');
});

test('the prototype is restored when creating the root throws', () => {
  const proto = { addEventListener() {} } as unknown as EventTarget;
  const original = proto.addEventListener;
  assert.throws(() => withoutRootListeners(proto, () => { throw new Error('boom'); }), /boom/);
  assert.equal(proto.addEventListener, original);
});

test('no component relies on a skipped root event', () => {
  const props: Record<string, string> = { animationiteration: 'onAnimationIteration' };
  const files: string[] = [];
  const walk = (dir: string) => { for (const name of readdirSync(dir)) { const p = join(dir, name); if (statSync(p).isDirectory()) walk(p); else if (/\.(tsx?|jsx?)$/.test(name) && name !== 'react-root.ts') files.push(p); } };
  walk(new URL('../src', import.meta.url).pathname);
  for (const event of SKIPPED_ROOT_EVENTS) {
    const prop = props[event];
    assert.ok(prop, `map ${event} to its React prop`);
    for (const file of files) assert.ok(!readFileSync(file, 'utf8').includes(prop), `${file} uses ${prop}; React will never call it`);
  }
});
