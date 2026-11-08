// «Trabajando» in the chat list: a blacksmith's hammer strikes an anvil and
// throws sparks of light (approved mock sidebar-forge v5, 2026-11-08).
//
// Every moving part is its own layer that only moves and fades (styles in
// app.css, .sv2-forge), so the GPU animates it and the page is never touched:
// 16 working rows measured 0.4 ms of CPU a second, against 45 ms when the
// parts moved inside one SVG (style and layout 60 times a second).
import { memo, useId, type CSSProperties } from 'react';

// The drawing sits in a 24 box; this shift centers its mass there.
const SHIFT = 'translate(1.4 -0.6)';
// Where the hammer's face meets the anvil, after the shift.
const IMPACT_X = 16.4;
const IMPACT_Y = 12.15;
const SPARK_SCALE = 1.35;

const ANVIL = 'M1.6 13c1.6 0 3 0 4.1 0H19.6v1.3c0 1-.8 1.6-1.9 1.6h-2.5l.6 2.5h1.9c.6 0 1 .4 1 1V21H5.3v-1.6c0-.6.4-1 1-1h1.9l.6-2.5H7.4C4.6 15.9 2.6 14.8 1.6 13z';
// A real hammer: the handle enters the head in the middle; flat face down, peen up.
const HANDLE = 'M2.6 9.4 12.2 9.35';
const HEAD = 'M12.45 5.9H13.55L14.3 7.8V10.45H14.75V12.4Q14.75 12.95 14.2 12.95H11.8Q11.25 12.95 11.25 12.4V10.45H11.7V7.8Z';

type Spark = { set: 'a' | 'b'; angle: number; distance: number; size: number; delay: number };

// Two patterns that alternate blow by blow, at irregular distances and with a
// small stagger, so a burst scatters instead of drawing an arc.
const DELAYS = [0, 40, 15, 70, 25, 55];
const SPARKS: Spark[] = [
  ...[[-128, 4.8, .9], [-98, 8.4, 1], [-72, 6.2, 1.1], [-48, 8.8, 1], [-26, 5.4, .9], [-6, 6.8, .8]]
    .map(([angle, distance, size], i): Spark => ({ set: 'a', angle, distance, size, delay: DELAYS[i] })),
  ...[[-142, 7, .8], [-112, 5, 1], [-86, 9, 1.1], [-60, 6, 1], [-36, 8.2, .95], [-16, 4.6, .85]]
    .map(([angle, distance, size], i): Spark => ({ set: 'b', angle, distance, size, delay: DELAYS[i] })),
];

const stop = (offset: string, color: string, opacity?: number) => (
  <stop offset={offset} style={{ stopColor: `var(${color})`, ...(opacity === undefined ? {} : { stopOpacity: opacity }) }} />
);

function SparkLayer({ spark, uid }: { spark: Spark; uid: string }) {
  const a = (spark.angle * Math.PI) / 180;
  const s = spark.size * SPARK_SCALE;
  // A short straight trail behind a glowing point, drawn at the impact; the
  // layer carries it out along its angle.
  const tx = IMPACT_X - 2.6 * s * Math.cos(a);
  const ty = IMPACT_Y - 2.6 * s * Math.sin(a);
  const style = {
    '--dx': `${((spark.distance * Math.cos(a)) / 24) * 100}%`,
    '--dy': `${((spark.distance * Math.sin(a)) / 24) * 100}%`,
    animationDelay: `${spark.delay}ms`,
  } as CSSProperties;
  return (
    <span className={`fg-spark ${spark.set}`} style={style}>
      <svg viewBox="0 0 24 24">
        <defs>
          <linearGradient id={`${uid}t`} gradientUnits="userSpaceOnUse" x1={tx} y1={ty} x2={IMPACT_X} y2={IMPACT_Y}>
            {stop('0', '--fg-ember', 0)}{stop('1', '--fg-spark', 0.95)}
          </linearGradient>
          <radialGradient id={`${uid}g`}>
            {stop('0', '--fg-core')}{stop('.3', '--fg-hot')}{stop('.6', '--fg-spark', 0.8)}{stop('1', '--fg-ember', 0)}
          </radialGradient>
        </defs>
        <line x1={tx} y1={ty} x2={IMPACT_X} y2={IMPACT_Y} stroke={`url(#${uid}t)`} strokeWidth={0.55 * s} strokeLinecap="round" />
        <circle cx={IMPACT_X} cy={IMPACT_Y} r={1.15 * s} fill={`url(#${uid}g)`} />
      </svg>
    </span>
  );
}

export const ForgeIcon = memo(function ForgeIcon() {
  const uid = `fg${useId().replace(/[^a-zA-Z0-9]/g, '')}`;
  return (
    <span className="sv2-forge" aria-hidden="true">
      <svg viewBox="0 0 24 24"><g transform={SHIFT}><path className="fg-anvil" d={ANVIL} /></g></svg>
      <span className="fg-hammer">
        <svg viewBox="0 0 24 24"><g transform={SHIFT}><path className="fg-handle" d={HANDLE} /><path className="fg-head" d={HEAD} /></g></svg>
      </span>
      <span className="fg-flash">
        <svg viewBox="0 0 24 24">
          <defs>
            <radialGradient id={`${uid}f`}>
              {stop('0', '--fg-core')}{stop('.22', '--fg-hot')}{stop('.5', '--fg-spark', 0.75)}{stop('1', '--fg-ember', 0)}
            </radialGradient>
          </defs>
          <circle cx={IMPACT_X} cy={IMPACT_Y} r={3.8} fill={`url(#${uid}f)`} />
        </svg>
      </span>
      {SPARKS.map((spark, index) => <SparkLayer key={index} spark={spark} uid={`${uid}s${index}`} />)}
    </span>
  );
});
