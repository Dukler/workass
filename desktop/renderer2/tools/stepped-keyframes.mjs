// Stepped keyframes for the looping status animations (energy lane, 2026-11-12).
//
// A smooth CSS animation makes the compositor draw and present a new frame on
// every display refresh (120 Hz on this Mac) for as long as it runs, even for a
// 16 px icon. These are the same curves sampled at about 30 frames a second and
// held between samples (`animation-timing-function: steps(1, end)` on each
// interval), so the GPU process and WindowServer draw a quarter of the frames.
// The motion keeps its shape, period and easing; samples where a layer is
// invisible (opacity 0) keep the previous transform, so they draw nothing.
// Transform and opacity only, so the compositor runs them.
//
// app.css carries the output between the stepped-keyframes markers;
// tests/stepped-keyframes.test.ts fails when they drift apart.
// Regenerate: node desktop/renderer2/tools/stepped-keyframes.mjs

export const FPS = 30;

// CSS cubic-bezier(x1, y1, x2, y2) as a function of progress.
export function cubicBezier(x1, y1, x2, y2) {
  const bx = (t) => 3 * x1 * t * (1 - t) ** 2 + 3 * x2 * t * t * (1 - t) + t ** 3;
  const by = (t) => 3 * y1 * t * (1 - t) ** 2 + 3 * y2 * t * t * (1 - t) + t ** 3;
  return (x) => {
    if (x <= 0) return 0;
    if (x >= 1) return 1;
    let lo = 0, hi = 1, t = x;
    for (let i = 0; i < 60; i++) {
      t = (lo + hi) / 2;
      if (bx(t) < x) lo = t; else hi = t;
    }
    return by(t);
  };
}
export const EASE = {
  linear: (x) => x,
  ease: cubicBezier(0.25, 0.1, 0.25, 1),
  'ease-in': cubicBezier(0.42, 0, 1, 1),
  'ease-out': cubicBezier(0, 0, 0.58, 1),
  'ease-in-out': cubicBezier(0.42, 0, 0.58, 1),
};

const round = (v) => {
  const r = Math.round(v * 1000) / 1000;
  return Object.is(r, -0) ? 0 : r;
};
const pct = (p) => `${round(p * 100)}%`;

// Value of every property at cycle progress p (0..1): the keyframe interval it
// falls in, eased the way CSS eases that interval.
export function valueAt(spec, p) {
  const stops = spec.stops;
  let i = 0;
  while (i < stops.length - 2 && p >= stops[i + 1].at) i++;
  const a = stops[i], b = stops[i + 1];
  const span = b.at - a.at;
  const local = span > 0 ? Math.min(1, Math.max(0, (p - a.at) / span)) : 1;
  const pick = a.ease ?? spec.ease ?? 'ease';
  const ease = typeof pick === 'function' ? pick : EASE[pick];
  const e = ease(local);
  const out = {};
  for (const key of Object.keys(a.v)) out[key] = a.v[key] + (b.v[key] - a.v[key]) * e;
  return out;
}

// Keyframes on a fixed grid of `count` samples a cycle, the curve shifted by
// `delay` (a fraction of the cycle) instead of an animation-delay: every layer
// of the forge then changes on the same frames, so the icon draws ~30 frames a
// second in all, not 30 for each of its fourteen staggered layers. Invisible
// samples keep the previous transform, so a hidden layer draws nothing.
export function gridSample(spec, count, delay = 0, cycles = 1) {
  const frames = [];
  let prev = null;
  // `count` may be fractional (a 280 ms fade holds 8.4 thirtieths): the last
  // sample then holds until the end.
  for (let n = 0; n < count - 1e-9; n++) {
    let v = valueAt(spec, ((((n / count) * cycles - delay) % 1) + 1) % 1);
    if (prev && spec.hiddenWhen && spec.hiddenWhen(v) && spec.hiddenWhen(prev)) v = prev;
    const css = spec.css(Object.fromEntries(Object.entries(v).map(([k, x]) => [k, round(x)])));
    prev = v;
    if (frames.length && frames[frames.length - 1].css === css) continue;
    frames.push({ at: n / count, css });
  }
  frames.push({ at: 1, css: spec.end ? spec.css(spec.end) : frames[frames.length - 1].css });
  return frames;
}

export function keyframesCss(name, frames) {
  const lines = frames.map((f) => `  ${pct(f.at)} { ${f.css} }`);
  return `@keyframes ${name} {\n${lines.join('\n')}\n}`;
}

const rotate = ({ r }) => `transform: rotate(${r}deg);`;
const fly = ({ o, k, g }) => `opacity: ${o}; transform: translate(calc(var(--dx) * ${k}), calc(var(--dy) * ${k} + ${round(6 * g)}%));`;
const flash = ({ o, s }) => `opacity: ${o}; transform: scale(${s});`;
const ring = ({ o, s }) => `opacity: ${o}; transform: scale(${s});`;
const fade = ({ o }) => `opacity: ${o};`;
const invisible = (v) => v.o === 0;

// The forge hammer swing (sidebar working icon), 1.15 s.
export const SPECS = {
  'fg-swing': {
    duration: 1.15, css: rotate, stops: [
      { at: 0, v: { r: -30 }, ease: 'ease-in-out' },
      { at: 0.48, v: { r: -34 }, ease: cubicBezier(0.6, 0, 1, 0.6) },
      { at: 0.60, v: { r: 0 }, ease: 'ease-out' },
      { at: 0.66, v: { r: -6 }, ease: 'ease-in' },
      { at: 0.71, v: { r: 0 }, ease: 'ease-in-out' },
      { at: 1, v: { r: -30 } },
    ],
  },
  // A spark of the first blow, 2.3 s (the second blow is the same 50% later).
  'fg-fly-a': sparkSpec(0),
  'fg-fly-b': sparkSpec(0.5),
  'fg-flash': {
    duration: 2.3, css: flash, ease: 'ease', hiddenWhen: invisible, stops: [
      { at: 0, v: { o: 0, s: 0.4 } },
      { at: 0.299, v: { o: 0, s: 0.4 } },
      { at: 0.303, v: { o: 1, s: 1.1 } },
      { at: 0.35, v: { o: 0, s: 0.7 } },
      { at: 0.799, v: { o: 0, s: 0.4 } },
      { at: 0.803, v: { o: 1, s: 1.1 } },
      { at: 0.85, v: { o: 0, s: 0.7 } },
      { at: 1, v: { o: 0, s: 0.7 } },
    ],
  },
  // The background-work dot's halo: a 3 px ring at 16% that breathes out to
  // 5 px at 5% and back (was a box-shadow, repainted every frame): a 14 px disc
  // under the 8 px dot, scaled to 18 px and faded to 5/16.
  'wdot-ring': {
    duration: 2, css: ring, ease: 'ease-in-out', stops: [
      { at: 0, v: { o: 1, s: 1 } },
      { at: 0.5, v: { o: 0.3125, s: 18 / 14 } },
      { at: 1, v: { o: 1, s: 1 } },
    ],
  },
  // The fork row's ping, 1.3 s.
  'fk-ping': {
    duration: 1.3, css: flash, ease: cubicBezier(0, 0, 0.2, 1), hiddenWhen: invisible, stops: [
      { at: 0, v: { o: 0.85, s: 1 } },
      { at: 0.7, v: { o: 0, s: 2.6 } },
      { at: 1, v: { o: 0, s: 2.6 } },
    ],
  },
  // The streaming reveal: each new block, list item, row and sentence fades in
  // once over 280 ms, ease-out (stream-reveal.ts). Ends fully opaque.
  'mdfresh': {
    duration: 0.28, css: fade, ease: 'ease-out', end: { o: 1 }, stops: [
      { at: 0, v: { o: 0 } },
      { at: 1, v: { o: 1 } },
    ],
  },
  // The branch chip while the Cambios tab is busy, 1.6 s.
  'cb-pulse': {
    duration: 1.6, css: fade, ease: 'ease-in-out', stops: [
      { at: 0, v: { o: 1 } },
      { at: 0.5, v: { o: 0.45 } },
      { at: 1, v: { o: 1 } },
    ],
  },
};

function sparkSpec(shift) {
  const s = (at) => at + shift;
  return {
    duration: 2.3, css: fly, ease: 'ease', hiddenWhen: invisible, stops: [
      { at: 0, v: { o: 0, k: 0, g: 0 } },
      ...(shift ? [{ at: s(-0.5), v: { o: 0, k: 0, g: 0 } }] : []),
      { at: s(0.299), v: { o: 0, k: 0, g: 0 } },
      { at: s(0.303), v: { o: 1, k: 0.18, g: 0 } },
      { at: s(0.38), v: { o: 1, k: 0.8, g: 0 } },
      { at: s(0.46), v: { o: 0, k: 1, g: 1 } },
      { at: 1, v: { o: 0, k: 1, g: 1 } },
    ].filter((stop, index, all) => index === 0 || stop.at > all[index - 1].at),
  };
}

// The forge's spark stagger (ForgeIcon.tsx DELAYS, ms): baked into one
// keyframes per delay. Every layer runs 2.3 s (the hammer: two 1.15 s blows) in
// 69 samples, so the whole icon changes on one 1/30 s grid.
export const FORGE_DELAYS = [0, 15, 25, 40, 55, 70];
const FORGE = new Set(['fg-swing', 'fg-flash', 'fg-fly-a', 'fg-fly-b']);
// Every stepped loop changes only at multiples of 1/30 s from its start
// (frame-grid.ts lines their starts up on that grid too).
const gridCount = (spec) => spec.duration * FPS;

export function generate() {
  const out = [
    keyframesCss('fg-swing', gridSample(SPECS['fg-swing'], 69, 0, 2)),
    keyframesCss('fg-flash', gridSample(SPECS['fg-flash'], 69)),
  ];
  for (const set of ['a', 'b']) {
    for (const delay of FORGE_DELAYS) {
      out.push(keyframesCss(`fg-fly-${set}-${delay}`, gridSample(SPECS[`fg-fly-${set}`], 69, delay / 2300)));
    }
  }
  for (const [name, spec] of Object.entries(SPECS)) if (!FORGE.has(name)) out.push(keyframesCss(name, gridSample(spec, gridCount(spec))));
  return out.join('\n');
}

if (import.meta.url === `file://${process.argv[1]}`) process.stdout.write(`${generate()}\n`);
