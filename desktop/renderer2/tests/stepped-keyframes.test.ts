import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
// @ts-expect-error plain .mjs tool without types
import { FORGE_DELAYS, FPS, SPECS, generate, gridSample, valueAt } from '../tools/stepped-keyframes.mjs';

// The looping status animations are sampled at 30 fps (energy lane,
// 2026-11-12). app.css must carry exactly what the generator prints, and every
// sample must sit on the original curve, so the motion keeps its shape.
const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');

test('app.css carries the generated stepped keyframes verbatim', () => {
  const begin = css.indexOf('/* stepped-keyframes:begin */\n');
  const end = css.indexOf('\n/* stepped-keyframes:end */');
  assert.ok(begin > 0 && end > begin, 'markers present');
  assert.equal(css.slice(begin + '/* stepped-keyframes:begin */\n'.length, end), generate());
});

test('each looping animation that uses them holds its samples (steps(1, end))', () => {
  for (const rule of [
    /\.fg-hammer \{[^}]*animation: fg-swing 2\.3s steps\(1, end\) infinite/,
    /\.fg-spark \{[^}]*animation: 2\.3s steps\(1, end\) infinite/,
    /\.fg-flash \{[^}]*animation: fg-flash 2\.3s steps\(1, end\) infinite/,
    /\.wdot::before \{[^}]*animation: wdot-ring 2s steps\(1, end\) infinite/,
    /\.fk-pulse::after \{[^}]*animation: fk-ping 1\.3s steps\(1, end\) infinite/,
    /\.cb-br\.pulse \{ animation: cb-pulse 1\.6s steps\(1, end\) infinite/,
    /\.workorbit::after \{[^}]*animation: workorbit 1s steps\(30\) infinite/,
  ]) assert.match(css, rule);
  // No box-shadow or content animation is left: those repaint on the main thread every frame.
  const bodyOf = (name: string) => {
    const start = css.indexOf(`@keyframes ${name} {`);
    let depth = 0;
    for (let i = start; i < css.length; i++) {
      if (css[i] === '{') depth++;
      else if (css[i] === '}' && --depth === 0) return css.slice(start, i + 1);
    }
    return '';
  };
  for (const name of [...css.matchAll(/@keyframes ([\w-]+) \{/g)].map((m) => m[1])) {
    if (/(content|box-shadow)\s*:/.test(bodyOf(name))) {
      const looping = new RegExp(`animation:[^;]*\\b${name}\\b[^;]*infinite`).test(css);
      assert.ok(!looping, `${name} loops while animating content or box-shadow`);
    }
  }
});

test('every spark stagger in ForgeIcon has its keyframes', () => {
  const source = readFileSync(new URL('../src/components/ForgeIcon.tsx', import.meta.url), 'utf8');
  const delays = JSON.parse(/const DELAYS = (\[[^\]]*\])/.exec(source)![1]) as number[];
  assert.deepEqual([...new Set(delays)].sort((a, b) => a - b), [...FORGE_DELAYS].sort((a, b) => a - b));
  for (const set of ['a', 'b']) for (const delay of delays) assert.match(css, new RegExp(`@keyframes fg-fly-${set}-${delay} \\{`));
  assert.match(source, /animationName: `fg-fly-\$\{spark\.set\}-\$\{spark\.delay\}`/);
});

test('samples lie on the original curves, on one 1/30 s grid', () => {
  const runs: [string, number, number, number, number][] = [
    // name, samples, duration (s), stagger (fraction), cycles per animation
    ['fg-swing', 69, 2.3, 0, 2], ['fg-flash', 69, 2.3, 0, 1], ['fg-fly-a', 69, 2.3, 40 / 2300, 1], ['fg-fly-b', 69, 2.3, 15 / 2300, 1],
    ...(['wdot-ring', 'fk-ping', 'cb-pulse'].map((name) => [name, Math.round((SPECS as any)[name].duration * FPS), (SPECS as any)[name].duration, 0, 1]) as [string, number, number, number, number][]),
  ];
  for (const [name, count, duration, delay, cycles] of runs) {
    const spec = (SPECS as any)[name];
    // one sample per 1/30 s of the animation's duration
    assert.ok(Math.abs(duration * FPS - count) < 1e-9, `${name}: ${count} samples for ${duration}s`);
    for (const frame of gridSample(spec, count, delay, cycles).slice(0, -1)) {
      assert.ok(Math.abs(frame.at * count - Math.round(frame.at * count)) < 1e-6, `${name} off the grid at ${frame.at}`);
      const v = valueAt(spec, ((((frame.at * cycles) - delay) % 1) + 1) % 1);
      if (spec.hiddenWhen && spec.hiddenWhen(v)) continue;
      const expected = spec.css(Object.fromEntries(Object.entries(v).map(([k, x]) => [k, Math.round((x as number) * 1000) / 1000 || 0])));
      assert.equal(frame.css, expected, `${name} at ${frame.at}`);
    }
  }
  // The hammer strike keeps its timing: up at 48%, down on the anvil at 60%.
  assert.equal(valueAt(SPECS['fg-swing'], 0.48).r, -34);
  assert.equal(valueAt(SPECS['fg-swing'], 0.6).r, 0);
});
