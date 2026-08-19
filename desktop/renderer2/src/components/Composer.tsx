import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { Chat, AppState } from '../store/types';
import type { CatalogGroup, CatalogCommand, PlanUsageSnapshot, PlanUsageEntry } from '../wire/types';
import type { ModelFavorite } from '../model-favorites';
import { store, useApp } from '../store/store';
import { has } from '../wire/api';
import { IcPlus, IcMic, IcGauge, IcStar } from '../icons';
import { modelContextQualifier, resolveModelSelection } from '../model-selection';
import { favoriteCatalogModels, isModelFavorite } from '../model-favorites';
import { userFacingCatalogGroups, userFacingFlatModels, type WorkassRuntimeProfile } from '../model-catalog';
import { attachmentWorkBoundary, clipboardImageFiles, createDraftImages, draftImagePayloads, withoutDraftImages } from '../image-drafts';
import { QueueList } from './QueueList';
import { steeringBehavior } from '../steering';
import { composerSubmitIntent, type ComposerSubmitIntent } from '../composer-submit';
import { insertAtCaret, startRecording, transcribe, voiceStatus, type Recorder, type VoiceState } from '../voice';
import { clampPlanUsagePercent, formatCountdown, formatPlanUsagePercent, isExpiredPlanReset, isHotRateLimit, isLiveReset, rateLimitLabel, relativePlanReset } from '../plan-usage';
import { imageDraftCapability } from '../model-controls';
import { autosizeComposerTextarea, observeComposerTextareaWidth, syncComposerTextareaFade } from '../composer-autosize';
import {
  compactContextTokens,
  contextUsageForProvider,
  contextUsagePercent,
  type ContextUsageSnapshot,
} from '../context-usage';

const MODE_LABEL: Record<string, string> = { ask: 'Preguntar permisos', bypass: 'Permitir todo', bypassPermissions: 'Permitir todo', acceptEdits: 'Aceptar ediciones' };

// Fuzzy subsequence match for the commands/agents popup (approved mock,
// 2026-07-28): returns the matched character indices for highlighting, or null
// when the query is not a subsequence. Empty query matches with no highlights.
function fuzzyIndices(query: string, target: string): number[] | null {
  if (!query) return [];
  const lowered = target.toLowerCase();
  const out: number[] = [];
  let from = 0;
  for (const ch of query.toLowerCase()) {
    const idx = lowered.indexOf(ch, from);
    if (idx < 0) return null;
    out.push(idx);
    from = idx + 1;
  }
  return out;
}

// Order: prefix hits first, then tighter spans, then name. The matched chars
// are the ONLY green in the popup (accent law).
function fuzzyRank(a: { name: string; hit: number[] }, b: { name: string; hit: number[] }): number {
  const aFirst = a.hit[0] ?? 0; const bFirst = b.hit[0] ?? 0;
  if (aFirst !== bFirst) return aFirst - bFirst;
  const aSpan = (a.hit.at(-1) ?? 0) - aFirst; const bSpan = (b.hit.at(-1) ?? 0) - bFirst;
  if (aSpan !== bSpan) return aSpan - bSpan;
  return a.name.localeCompare(b.name);
}

function FuzzyName({ name, hit }: { name: string; hit: number[] }) {
  const marks = new Set(hit);
  return <>{name.split('').map((ch, i) => marks.has(i) ? <span key={i} className="fzhit">{ch}</span> : ch)}</>;
}

// The middle-ish sensible default: prefer "high", else the last stop before any
// "max"/"ultra", else the first. Never invents a name — only picks from the list.
function defaultEffort(efforts: string[]): string {
  if (efforts.includes('high')) return 'high';
  const heavy = new Set(['max', 'ultra']);
  const light = efforts.filter((e) => !heavy.has(e));
  return (light.length ? light[light.length - 1] : efforts[0]);
}
// Display-only capitalization for the header readout ("high" → "High"); the
// committed effort stays verbatim lowercase (onPick sends efforts[c] as-is).
const cap = (s: string) => (s ? s.charAt(0).toUpperCase() + s.slice(1) : s);

// Effort control: Claude Code's /model effort popover style. A full-width rounded
// FILL STRIP (~28px tall, quiet ink-over-panel fill) with round DOT stops evenly
// spaced inside — one per effort level, faint by default, inking to --muted as the
// thumb passes and the TOP stop tinted --acc (accent-only). The thumb is a vertical
// rounded PILL that slides over the active stop. The header reads "Esfuerzo <Value>"
// (label muted, value in ink, capitalized for display) and updates LIVE during drag;
// endpoint labels "Más rápido" / "Más inteligente" sit above the strip.
//
// Interaction is unchanged from the prior control: pointer drag with snap,
// click-track-to-jump, ArrowLeft/Right/Down/Up + Home/End, role="slider" ARIA.
// The visual preview (thumb/dots/readout) tracks the pointer live, but effort is
// COMMITTED only on release / click / key — never per drag-pixel — because onPick
// routes through store.setModel (a network round-trip). Strip width scales with the
// stop count (~200px for 2 stops, ~300px for 6). THUMB_W is the pill width and drives
// the half-offset + travel math so the pill never overflows the strip at the extremes.
const THUMB_W = 14;  // .sl-cc .sl-thumb pill width — drives half-offset + travel math

function EffortPopover({ efforts, current, onPick, onClose }: {
  efforts: string[]; current: string; onPick: (effort: string) => void; onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const trackRef = useRef<HTMLDivElement>(null);
  const n = efforts.length;
  const idx = Math.max(0, efforts.indexOf(current));
  const [val, setVal] = useState(idx);        // visual preview index (may lead the commit)
  const [dragging, setDragging] = useState(false);
  const [trackW, setTrackW] = useState(0);
  useEffect(() => { setVal(Math.max(0, efforts.indexOf(current))); }, [current, efforts]);
  useEffect(() => {
    const down = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) onClose(); };
    const key = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    document.addEventListener('mousedown', down);
    document.addEventListener('keydown', key);
    return () => { document.removeEventListener('mousedown', down); document.removeEventListener('keydown', key); };
  }, [onClose]);
  // Measure the track so tick fractions + thumb travel are pixel-exact; a
  // ResizeObserver keeps it honest across theme/font settle and rail-width changes.
  useEffect(() => {
    const el = trackRef.current; if (!el) return;
    const measure = () => setTrackW(el.clientWidth);
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const clampV = (i: number) => Math.max(0, Math.min(n - 1, i));
  const railW = Math.round(150 + 25 * n);     // 2 stops → 200px, 6 stops → 300px (mock metrics)
  const half = THUMB_W / 2;
  const travel = Math.max(0, trackW - THUMB_W);
  const center = (i: number) => half + (n < 2 ? 0 : (i / (n - 1)) * travel);
  const pct = n < 2 ? 0 : val / (n - 1);
  const thumbX = pct * travel;

  // Snap a client X (from the track's left edge) to the nearest stop index.
  const valueFromX = (clientX: number) => {
    const el = trackRef.current; if (!el) return val;
    const r = el.getBoundingClientRect();
    const denom = r.width - THUMB_W;
    let p = denom <= 0 ? 0 : (clientX - r.left - half) / denom;
    p = Math.max(0, Math.min(1, p));
    return Math.round(p * (n - 1));
  };
  const commit = (i: number) => { const c = clampV(i); setVal(c); onPick(efforts[c]); };

  const onDown = (e: React.PointerEvent) => {
    e.preventDefault();
    setDragging(true);
    (e.currentTarget as HTMLElement).focus();
    try { (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId); } catch { /* noop */ }
    setVal(valueFromX(e.clientX));             // visual preview only — commit waits for release
  };
  const onMove = (e: React.PointerEvent) => { if (dragging) setVal(valueFromX(e.clientX)); };
  const onUp = (e: React.PointerEvent) => {
    if (!dragging) return;
    setDragging(false);
    try { (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId); } catch { /* noop */ }
    commit(valueFromX(e.clientX));             // release / click-to-jump commits here
  };
  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowRight' || e.key === 'ArrowUp') { e.preventDefault(); commit(val + 1); }
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowDown') { e.preventDefault(); commit(val - 1); }
    else if (e.key === 'Home') { e.preventDefault(); commit(0); }
    else if (e.key === 'End') { e.preventDefault(); commit(n - 1); }
  };

  return (
    <div className="pop pop-effort" ref={ref} style={{ bottom: '130%', right: 0 }}>
      <div className="pcaprow">
        <span className="sl-lab">Esfuerzo</span>
        <span className="sl-value">{cap(efforts[val])}</span>
        <span className="sp" />
        <span className="sl-help" title="Más esfuerzo: respuestas más cuidadas pero más lentas. Menos: más rápidas.">?</span>
      </div>
      <div className="sl-ends" aria-hidden>
        <span>Más rápido</span>
        <span>Más inteligente</span>
      </div>
      <div className={`sl sl-cc ${dragging ? 'dragging' : ''}`} role="slider" tabIndex={0}
        aria-valuemin={0} aria-valuemax={n - 1} aria-valuenow={val} aria-valuetext={efforts[val]}
        aria-label="Esfuerzo del modelo" style={{ width: railW }}
        onKeyDown={onKey} onPointerDown={onDown} onPointerMove={onMove}
        onPointerUp={onUp} onPointerCancel={onUp}>
        <div className="sl-track" ref={trackRef}>
          {efforts.map((e, i) => (
            <div key={e} className={`dot ${i <= val ? 'past' : ''} ${i === n - 1 ? 'top' : ''}`} aria-hidden style={{ left: center(i) }} />
          ))}
          <div className="sl-thumb" aria-hidden style={{ left: thumbX }} />
        </div>
      </div>
    </div>
  );
}

function Popover({ items, current, onPick, onClose, cap }: {
  items: Array<{ id: string; name: string }>; current: string | null;
  onPick: (id: string) => void; onClose: () => void; cap: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const h = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) onClose(); };
    document.addEventListener('mousedown', h);
    return () => document.removeEventListener('mousedown', h);
  }, [onClose]);
  return (
    <div className="pop" ref={ref} style={{ bottom: '130%', right: 0 }}>
      <div className="pcap">{cap}</div>
      {items.length === 0 && <div className="pitem" style={{ color: 'var(--faint)' }}>Sin opciones</div>}
      {items.map((it) => (
        <button key={it.id} className={`pitem ${it.id === current ? 'on' : ''}`} onClick={() => { onPick(it.id); onClose(); }}>
          {it.name}{it.id === current && <span className="ck">✓</span>}
        </button>
      ))}
    </div>
  );
}

// Grouped model picker (R1): one scrollable list, a small provider-section
// divider per group, pick anything. Selecting binds the chat to the group's
// provider (at creation) and sets the model.
function GroupedModelPopover({ groups, profile, currentProvider, current, favorites, onPick, onToggleFavorite, onClose }: {
  groups: CatalogGroup[]; currentProvider: string | null; current: string | null; favorites: readonly ModelFavorite[];
  profile: WorkassRuntimeProfile;
  onPick: (providerId: string, modelId: string) => void;
  onToggleFavorite: (providerId: string, modelId: string) => void;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const h = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) onClose(); };
    document.addEventListener('mousedown', h);
    return () => document.removeEventListener('mousedown', h);
  }, [onClose]);
  const usable = userFacingCatalogGroups(groups, profile);
  const favoriteModels = favoriteCatalogModels(usable, favorites, profile);
  const modelRow = (providerId: string, providerName: string, model: { modelId: string; name: string }, showProvider = false) => {
    const active = providerId === currentProvider && model.modelId === current;
    const favorite = isModelFavorite(favorites, providerId, model.modelId);
    const qualifier = modelContextQualifier(model.modelId);
    return (
      <div className={`pmodelrow ${active ? 'on' : ''}`} key={`${showProvider ? 'favorite:' : ''}${providerId}:${model.modelId}`}>
        <button className="pmodelpick" onClick={() => { onPick(providerId, model.modelId); onClose(); }}>
          <span className="pmodelname">{model.name}</span>
          {qualifier && <span className="pmodelqual">{qualifier}</span>}
          {showProvider && <span className="pmodelprov">{providerName}</span>}
          {active && <span className="ck">✓</span>}
        </button>
        <button
          className={`pstar ${favorite ? 'on' : ''}`}
          aria-label={`${favorite ? 'Quitar de' : 'Agregar a'} favoritos: ${model.name}`}
          aria-pressed={favorite}
          title={favorite ? 'Quitar de favoritos' : 'Agregar a favoritos'}
          onClick={() => onToggleFavorite(providerId, model.modelId)}
        ><IcStar /></button>
      </div>
    );
  };
  return (
    <div className="pop pop-grouped" ref={ref} style={{ bottom: '130%', right: 0 }}>
      {usable.length === 0 && <div className="pitem" style={{ color: 'var(--faint)' }}>Sin modelos</div>}
      {favoriteModels.length > 0 && (
        <div className="pgroup pfavorites">
          <div className="pcap pcap-prov"><span>Favoritos</span></div>
          {favoriteModels.map((favorite) => modelRow(favorite.providerId, favorite.providerName, favorite.model, true))}
        </div>
      )}
      {usable.map((g) => (
        <div className="pgroup" key={g.providerId}>
          <div className="pcap pcap-prov">
            <span>{g.providerName || g.providerId}</span>
            {g.badge && <span className="pbadge">{g.badge}</span>}
          </div>
          {g.models.map((m) => modelRow(g.providerId, g.providerName || g.providerId, m))}
        </div>
      ))}
    </div>
  );
}

function resolveModelName(app: AppState, modelId: string | null | undefined): string | null {
  if (!modelId) return null;
  for (const g of app.groups) { const m = g.models.find((x) => x.modelId === modelId); if (m) return m.name; }
  return app.models.find((m) => m.modelId === modelId)?.name ?? null;
}
// ---- plan-usage rendering (provider-aware; WIRE-CONTRACT chat:plan-usage) ----
// Billed cost: "$0.06 USD" style — 2–4 decimals, trailing zeros beyond 2 trimmed.
function formatCost(amount: number | undefined, currency: string | undefined): string | null {
  const n = typeof amount === 'number' ? amount : Number(amount);
  if (!Number.isFinite(n)) return null;
  let s = n.toFixed(4).replace(/0+$/, '');
  const dot = s.indexOf('.');
  if (dot === -1) s += '.00';
  else { const dec = s.length - dot - 1; if (dec < 2) s += '0'.repeat(2 - dec); }
  return `$${s}${currency ? ` ${currency}` : ''}`;
}

// A clock that re-renders once a second while `active`. Only mounts inside the
// open context popover, so it never ticks in the background.
function useNow(active: boolean, intervalMs: number = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [active, intervalMs]);
  return now;
}

// The "Plan usage limits" section under the context window, scoped to the ACTIVE
// chat's provider. Rate-limit bars use only provider-reported utilization; a
// missing percentage stays empty rather than becoming an invented state bar.
function PlanSection({ providerLabel, snapshot, loading }: {
  providerLabel: string | null;
  snapshot?: PlanUsageSnapshot;
  loading?: boolean;
}) {
  const entries = snapshot?.entries ?? [];
  // Tick every second only if some reset is close enough to warrant a live clock.
  const hasLive = entries.some((e) => e.kind === 'rate-limit' && isLiveReset(e.resetsAt));
  const now = useNow(hasLive);
  const rows: React.ReactNode[] = [];
  entries.forEach((e: PlanUsageEntry, i) => {
    if (e.kind === 'rate-limit') {
      const expired = isExpiredPlanReset(e.resetsAt, now);
      const hot = !expired && isHotRateLimit(e);
      // Percentages are scoped to the provider's old window. At the reset
      // boundary, suppress them until the metadata-only refresh returns rather
      // than displaying a stale 83%/100% as current usage.
      const percent = expired ? null : clampPlanUsagePercent(e.usedPercent);
      const pctText = expired ? null : formatPlanUsagePercent(e.usedPercent);
      const reset = expired ? '' : relativePlanReset(e.resetsAt, now);
      // Under 24h the coarse "en 3 h" becomes a live H:MM:SS countdown that ticks
      // down each second; a day or more away stays the static relative label.
      const live = isLiveReset(e.resetsAt, now);
      const countdown = live ? formatCountdown(Date.parse(e.resetsAt!) - now) : null;
      // Label (truncates), the percentage pinned right (never shrinks), the bar,
      // and the reset time as a small caption underneath — so a long provider
      // label + reset string can never push text out of the popover box.
      rows.push(
        <div className="planrow" key={`rl-${e.id ?? i}`}>
          <div className="plantop">
            <span className={`planlab ${hot ? 'hot' : ''}`}>{rateLimitLabel(e.id, e.limitName)}</span>
            <span className={`planpct ${hot ? 'hot' : ''}`}>{pctText != null ? `${pctText}%` : '—'}</span>
          </div>
          <div className="planbar"><div className={`planbarfill ${hot ? 'hot' : ''}`} style={{ width: `${percent ?? 0}%` }} /></div>
          {(expired || reset || e.isUsingOverage) && (
            <div className="plansub">
              {expired ? (
                <span className="planreset">Actualizando límites…</span>
              ) : reset ? (
                <span className="planreset">
                  {live
                    ? <>Se reinicia en <span className={`plantick ${hot ? 'hot' : ''}`}>{countdown}</span></>
                    : `Se reinicia ${reset}`}
                </span>
              ) : null}
              {e.isUsingOverage ? <span className="planover">usando excedente</span> : null}
            </div>
          )}
        </div>,
      );
    } else if (e.kind === 'cost') {
      const cost = formatCost(e.amount, e.currency);
      if (cost) rows.push(
        <div className="planrow plancost" key={`cost-${i}`}>
          <div className="plantop"><span className="planlab">Uso facturado</span><span className="planval">{cost}</span></div>
        </div>,
      );
    } else if (e.kind === 'quota') {
      (e.perModel ?? []).forEach((pm, j) => {
        if (typeof pm.totalTokens !== 'number') return;
        rows.push(
          <div className="planrow" key={`q-${i}-${pm.model ?? j}`}>
          <div className="plantop"><span className="planlab">{pm.model || 'Modelo'}</span><span className="planval">{compactContextTokens(pm.totalTokens)} tokens</span></div>
          </div>,
        );
      });
    }
  });
  return (
    <div className="planlim">
      <div className="planhead">Límites del plan{providerLabel ? ` · ${providerLabel}` : ''}</div>
      {rows.length
        ? rows
        : <div className="planempty">{loading ? 'Cargando límites del plan…' : 'No hay límites del plan disponibles.'}</div>}
    </div>
  );
}

// Context is passive application state: the ring updates without requiring a
// model turn. Exact token and percentage details remain in the click-open
// provider-scoped popover instead of adding permanent text to the controls row.
// Missing usage draws an empty ring, never a fabricated zero-percent reading.
function ContextRing({ usage, planBound, planProvider, planUsage, planUsageLoading, onOpen }: {
  usage?: ContextUsageSnapshot;
  planBound?: boolean; planProvider?: string | null; planUsage?: PlanUsageSnapshot;
  planUsageLoading?: boolean;
  onOpen?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const down = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false); };
    const key = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', down);
    document.addEventListener('keydown', key);
    return () => { document.removeEventListener('mousedown', down); document.removeEventListener('keydown', key); };
  }, [open]);
  const pct = contextUsagePercent(usage); // null when there's no usable usage data
  const shown = pct ?? 0;             // ring draws empty (0%) when data is absent
  const hot = shown > 80;
  const R = 6;                        // 16px viewBox, 2px stroke → r6 keeps the ring inside
  const C = 2 * Math.PI * R;
  const off = C * (1 - shown / 100);
  const arc = hot ? 'var(--amber)' : 'var(--acc)';
  const title = pct != null && usage
    ? `Uso del contexto: ${compactContextTokens(usage.used)} / ${compactContextTokens(usage.size)} (${pct}%)`
    : 'Uso del contexto: sin datos todavía';
  return (
    <div className="ctxring-wrap" ref={ref} style={{ position: 'relative' }}>
      <button className="ctxring" onClick={() => setOpen((v) => { if (!v) onOpen?.(); return !v; })} title={title}
        aria-label={title} aria-haspopup="dialog" aria-expanded={open}>
        <svg viewBox="0 0 16 16" width="17" height="17" aria-hidden>
          <circle cx="8" cy="8" r={R} fill="none" stroke="var(--line2)" strokeWidth="2" />
          <circle cx="8" cy="8" r={R} fill="none" stroke={arc} strokeWidth="2" strokeLinecap="round"
            strokeDasharray={C} strokeDashoffset={off} transform="rotate(-90 8 8)" />
        </svg>
      </button>
      {open && (
        <div className="pop pop-ctx" style={{ bottom: '150%', right: 0 }} role="dialog" aria-label="Detalles de uso de contexto">
          {pct != null && usage ? (
            <>
              <div className="ctxrow">
                <span className="ctxlab">Ventana de contexto</span>
                <span className="ctxval">{compactContextTokens(usage.used)} / {compactContextTokens(usage.size)} ({pct}%)</span>
              </div>
              <div className="ctxbar"><div className="ctxbarfill" style={{ width: `${pct}%`, background: hot ? 'var(--amber)' : 'var(--acc)' }} /></div>
            </>
          ) : (
            <div className="ctxempty">Sin datos de contexto todavía.</div>
          )}
          {planBound && <PlanSection providerLabel={planProvider ?? null} snapshot={planUsage} loading={planUsageLoading} />}
        </div>
      )}
    </div>
  );
}

/** Names this chat is likely to hear, as decoding bias.
 *
 *  whisper renders an unknown identifier phonetically — "lambbridge.go" for
 *  lan_bridge.go, "usefleet" for useFleet — and returns both exactly when the
 *  same strings precede the audio as context. These are the ones available for
 *  free; identifiers harvested from the transcript's tool calls would be a
 *  richer source and are a follow-up, not a blocker. */
function dictationVocab(chat: Chat | null): string[] {
  if (!chat) return [];
  const terms: string[] = [];
  const push = (value?: string | null) => { if (value) terms.push(value); };

  push(chat.title);
  push(chat.providerName);
  push(chat.currentModelId?.replace(/\[.*$/, ''));
  // The working directory's own name: the folder is what most paths start with.
  const cwd = chat.cwd?.replace(/[\\/]+$/, '');
  if (cwd) push(cwd.split(/[\\/]/).pop());
  return terms;
}

export function Composer({ chat }: { chat: Chat | null }) {
  const app = useApp();
  const contextUsage = contextUsageForProvider(chat?.contextUsageByProvider, chat?.providerId);
  const taRef = useRef<HTMLTextAreaElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const [text, setText] = useState(chat?.draft ?? '');
  const [modelOpen, setModelOpen] = useState(false);
  const [modeOpen, setModeOpen] = useState(false);
  const [effortOpen, setEffortOpen] = useState(false);
  const [preparingImages, setPreparingImages] = useState(false);
  // Dictation. `level` drives the meter: it is the only proof the microphone is
  // hearing anything, and the common failure is recording happily from a muted
  // or wrong input device.
  const [voice, setVoice] = useState<VoiceState>('idle');
  const [level, setLevel] = useState(0);
  const recorderRef = useRef<Recorder | null>(null);
  // Once a steer has synchronously acquired its pending transcript/queue owner,
  // that surface paints the encoded images. Hide only those exact draft
  // previews from the composer so ownership moves in one commit without a
  // duplicate or a blank acknowledgement gap. A later paste remains visible.
  const [transferredSteerImageIDs, setTransferredSteerImageIDs] = useState<string[]>([]);

  // Load this chat's saved draft when the active chat changes. useLayoutEffect so
  // the swap commits BEFORE paint — no flash of the previous tab's text.
  useLayoutEffect(() => { setText(chat?.draft ?? ''); }, [chat?.id]);
  // Single source of truth for the box height: re-measure whenever the COMMITTED
  // value changes — typing, tab switch, or send-clear. Keying on `text` (not a
  // rAF that races the value commit) guarantees we measure the value React
  // actually rendered, so the box never collapses on a multi-line draft nor
  // balloons on an empty one when switching tabs.
  useLayoutEffect(() => { autosize(); }, [text]);
  // The selected chat can change the per-chat right column after this keyed
  // composer mounts. Re-measure when that layout change alters the actual text
  // width, otherwise wrapped drafts keep the previous chat's too-short height.
  useLayoutEffect(() => {
    const el = taRef.current;
    if (!el) return;
    return observeComposerTextareaWidth(el, autosize);
  }, []);

  // The popup's data: a chat without a catalog asks the daemon once when the
  // composer lands on it (boot never fires switchChat for the already-active
  // chat, and the catalog is daemon-memory-only so hydration can't carry it).
  // Keyed on the connection too: the mount fetch can fire before the wire is
  // open, and nothing else would ever retry it.
  useEffect(() => {
    if (chat?.id && app.connection === 'connected') store.requestCommandCatalog(chat.id);
  }, [chat?.id, chat?.providerId, chat?.sessionId, app.connection]);

  // R6 image attach (feature-detected on the session's promptCaps.image) and
  // agent-advertised slash-command autocomplete (only when the session exposes
  // a catalog). The mock reports neither, so both degrade to nothing.
  const imageCapability = imageDraftCapability(chat?.sessionId, chat?.sessionProviderId, chat?.providerId, chat?.imageSupport);
  const canChooseImage = !!chat && imageCapability !== 'unsupported';
  const atts = chat?.draftImages ?? [];
  const visibleAtts = withoutDraftImages(atts, transferredSteerImageIDs);
  // Rich per-session catalog when the provider reports one; the flat
  // session `commands` list stays as the fallback for older daemons.
  const catalog = chat?.commandCatalog;
  const catalogCommands: CatalogCommand[] = catalog?.commands?.length ? catalog.commands : (chat?.commands ?? []);
  const catalogAgents = catalog?.agents ?? [];
  // Triggers (approved composer-skills mock): "/" at draft start opens
  // Comandos; "@" at a word boundary opens Agentes. Esc latches per token —
  // the popup stays away until the token itself changes.
  const slashQuery = text.startsWith('/') && !text.includes('\n') ? text.slice(1).split(/\s/)[0] : null;
  const atQuery = (() => {
    const match = /(?:^|\s)@([\w./-]*)$/.exec(text);
    return match ? match[1] : null;
  })();
  const popToken = slashQuery !== null ? `/${slashQuery}` : atQuery !== null ? `@${atQuery}` : null;
  const [popDismissed, setPopDismissed] = useState<string | null>(null);
  const [popIndex, setPopIndex] = useState(0);
  const popView: 'commands' | 'agents' | null = popToken === null || popDismissed === popToken
    ? null
    : slashQuery !== null ? 'commands' : 'agents';
  const commandMatches = popView === 'commands'
    ? catalogCommands
      .map((c) => ({ c, name: c.name, hit: fuzzyIndices(slashQuery ?? '', c.name) }))
      .filter((m): m is { c: typeof m.c; name: string; hit: number[] } => m.hit !== null)
      .sort(fuzzyRank)
    : [];
  const agentMatches = popView === 'agents'
    ? catalogAgents
      .map((a) => ({ a, name: a.name, hit: fuzzyIndices(atQuery ?? '', a.name) }))
      .filter((m): m is { a: typeof m.a; name: string; hit: number[] } => m.hit !== null)
      .sort(fuzzyRank)
    : [];
  const popCount = popView === 'commands' ? commandMatches.length : agentMatches.length;
  const popOpen = popView !== null && popCount > 0;
  const popActive = Math.min(popIndex, Math.max(0, popCount - 1));
  // New token → first row active again.
  useEffect(() => { setPopIndex(0); }, [popToken]);

  async function addFiles(chatId: string, files: FileList | File[]) {
    const imgs = Array.from(files).filter((f) => f.type.startsWith('image/'));
    if (imgs.length === 0) return;
    const added = createDraftImages(imgs);
    if (added.length === 0) return;
    // Paint previews before a cold ACP engine/session is started. Capability
    // validation may take seconds; it must not hold the user's files hostage.
    store.addDraftImages(chatId, added);
    if (!await store.ensureImageDraftSupport(chatId)) {
      store.removeDraftImages(chatId, added.map((image) => image.id));
      store.addToast('Imágenes no compatibles', 'El agente seleccionado no acepta imágenes.');
    }
  }
  function onPaste(e: React.ClipboardEvent) {
    if (!chat) return;
    const files = clipboardImageFiles(e.clipboardData);
    if (files.length) { e.preventDefault(); void addFiles(chat.id, files); }
  }

  const running = store.isChatRunning(chat?.id ?? null);
  const grouped = app.groups.length > 0;
  // The persisted id may be suffixed (`base[effort]`). Split it so the picker shows
  // the base name/selection and the effort control reflects the chosen stop.
  const modelSelection = resolveModelSelection(app.groups, app.models, chat?.currentModelId);
  const { base: modelBase, effort: modelEffort } = modelSelection;
  const modelName = modelSelection.model?.name ?? resolveModelName(app, modelBase) ?? app.models[0]?.name ?? 'Modelo';
  const efforts = modelSelection.model?.efforts ?? [];
  const curEffort = efforts.length
    ? (modelEffort && efforts.includes(modelEffort) ? modelEffort : defaultEffort(efforts))
    : null;
  // Permission modes are provider-specific. The old global list represents
  // only the default provider and becomes empty or wrong after a
  // reconnect/provider switch, which made this control look dead.
  const providerModes = app.groups.find((g) => g.providerId === chat?.providerId)?.modes;
  const modes = providerModes?.length ? providerModes : app.modes;
  const modeId = chat?.currentModeId ?? modes[0]?.id ?? null;
  const modeName = modes.find((m) => m.id === modeId)?.name ?? '';
  const permLabel = (modeId && MODE_LABEL[modeId]) || (modeName ? modeName : 'Preguntar permisos');
  const steerAvail = has('appChatSteer');
  const steerBehavior = steeringBehavior(chat?.providerId);

  function autosize() {
    const el = taRef.current; if (!el) return;
    autosizeComposerTextarea(el);
  }
  // The composer draws no scrollbar, so the top fade is the ONLY sign that text
  // is hidden above the edge — it has to follow the real scroll position, not
  // sit there permanently dimming the first line of a short draft. Toggled on
  // the DOM node directly: this fires on every scroll tick and must not re-render.
  function fade() {
    const el = taRef.current; if (!el) return;
    syncComposerTextareaFade(el);
  }
  // Height is handled by the useLayoutEffect on `text` above — just update state.
  function change(v: string) { setText(v); if (chat) store.setDraft(chat.id, v); }
  // Tap to open the microphone, tap to close it. Nothing is sent: the text is
  // inserted at the caret for the user to read and fix, and they press send.
  //
  // There is no live transcript on purpose — whisper decodes a whole segment,
  // so "live" text is the same audio re-decoded through a sliding window, which
  // rewrites itself mid-sentence and cannot be edited while it does.
  async function toggleVoice() {
    if (voice === 'transcribing') return;

    if (voice === 'recording') {
      const recorder = recorderRef.current;
      recorderRef.current = null;
      setVoice('transcribing');
      setLevel(0);
      try {
        const wav = await recorder!.stop();
        // Bias decoding toward names this chat actually uses. Without it
        // whisper returns "lambbridge.go" for lan_bridge.go; with it, exactly.
        const said = await transcribe(wav, undefined, dictationVocab(chat));
        if (said) {
          const el = taRef.current;
          const start = el?.selectionStart ?? text.length;
          const end = el?.selectionEnd ?? start;
          const { text: next, caret } = insertAtCaret(text, said, start, end);
          change(next);
          requestAnimationFrame(() => {
            taRef.current?.focus();
            taRef.current?.setSelectionRange(caret, caret);
          });
        }
      } catch (error) {
        store.addToast('No se pudo transcribir', String((error as Error)?.message ?? error));
      } finally {
        setVoice('idle');
      }
      return;
    }

    // Ask before opening the microphone: a missing engine discovered at the end
    // of a spoken sentence is the worst possible time to find out.
    const status = await voiceStatus();
    if (!status.available) {
      store.addToast('Dictado no disponible', status.hint ?? 'no hay motor de voz en esta máquina');
      return;
    }
    try {
      recorderRef.current = await startRecording({
        onLevel: setLevel,
        maxSeconds: 120,
        onAutoStop: () => { void toggleVoice(); },
      });
      setVoice('recording');
    } catch (error) {
      // Denied permission and "no input device" both land here, and both need
      // saying out loud — a mic button that does nothing reads as broken.
      store.addToast('Sin micrófono', String((error as Error)?.message ?? error));
    }
  }

  // Release the device if the composer unmounts mid-recording; the OS recording
  // indicator would otherwise stay lit with nothing listening.
  useEffect(() => () => { recorderRef.current?.cancel(); recorderRef.current = null; }, []);

  function pickSlash(name: string) {
    const v = `/${name} `;
    change(v);
    requestAnimationFrame(() => { taRef.current?.focus(); const el = taRef.current; if (el) el.setSelectionRange(v.length, v.length); });
  }
  // Inserts, never sends: the trailing @token becomes the canonical agent
  // mention and the caret lands after the inserted space.
  function pickAgent(name: string) {
    const v = text.replace(/(^|\s)@[\w./-]*$/, `$1@${name} `);
    change(v);
    requestAnimationFrame(() => { taRef.current?.focus(); const el = taRef.current; if (el) el.setSelectionRange(v.length, v.length); });
  }
  function pickPopRow(index: number) {
    if (popView === 'commands') { const m = commandMatches[index]; if (m) pickSlash(m.c.name); return; }
    if (popView === 'agents') { const m = agentMatches[index]; if (m) pickAgent(m.a.name); }
  }
  async function submit(intent: ComposerSubmitIntent = running ? 'steer' : 'send') {
    // Blocked while offline (mirrors the disabled send button); the banner above
    // the composer explains why. Enter must not silently drop into a dead socket.
    if (app.connection !== 'connected') return;
    if (!running && chat?.queuePaused) {
      const resumeOnly = !!chat.queue?.length;
      const resumed = await store.resumeQueued(chat.id);
      if (!resumed || resumeOnly) return;
    }
    const submittedDraft = text;
    const t = text.trim();
    if (running && !t && intent === 'steer') {
      if (chat) store.cancelChatTurn(chat.id);
      return;
    }
    if (!t || !chat || preparingImages) return;
    if (running && intent === 'queue') {
      if (store.queueDraftMessage(chat.id, t, atts)) setText('');
      return;
    }
    setPreparingImages(true);
    let images: Awaited<ReturnType<typeof draftImagePayloads>> | undefined;
    try {
      // Commit the preparing state before touching bytes, then yield between
      // files so typing/scrolling can paint even for a large idle send or steer.
      if (atts.length) await attachmentWorkBoundary();
      images = atts.length ? await draftImagePayloads(atts, undefined, attachmentWorkBoundary) : undefined;
    } catch (error) {
      store.addToast('No se pudo adjuntar', error instanceof Error ? error.message : 'No se pudo leer una de las imágenes.');
      setPreparingImages(false);
      return;
    }
    const sentChatID = chat?.id;
    const sentImageIDs = atts.map((image) => image.id);
    if (running && intent === 'steer') {
      if (t && chat) {
        const delivery = store.steerOrQueue(chat.id, t, images);
        setTransferredSteerImageIDs(sentImageIDs);
        // A native acknowledgement can arrive seconds after Codex has already
        // received the direction. Release the composer as soon as the store has
        // synchronously created its pending transcript owner; do not make the
        // text look stuck at the bottom or keep the composer send-locked.
        store.setDraft(chat.id, '');
        setText('');
        // Text-only steering can accept the next draft immediately. Attached
        // files keep the send gate until acknowledgement so the still-owned
        // previews cannot be submitted a second time during the receipt gap.
        if (sentImageIDs.length === 0) setPreparingImages(false);
        void delivery.then((accepted) => {
          if (accepted && sentChatID) {
            store.removeDraftImages(sentChatID, sentImageIDs);
            return;
          }
          // The dispatch boundary rejected the direction before it acquired a
          // visible owner. Restore only into an untouched empty composer; never
          // overwrite text the user typed while the receipt was in flight.
          if (sentChatID) setText((current) => {
            if (current) return current;
            store.setDraft(sentChatID, submittedDraft);
            return submittedDraft;
          });
        }).finally(() => {
          setTransferredSteerImageIDs([]);
          setPreparingImages(false);
        });
        return;
      }
      setPreparingImages(false);
      return;
    }
    void store.sendTo(chat.id, t, images, submittedDraft).then((accepted) => {
      if (accepted && sentChatID) store.removeDraftImages(sentChatID, sentImageIDs);
    }).finally(() => setPreparingImages(false));
    setText('');
  }
  function keydown(e: React.KeyboardEvent) {
    // While the catalog popup is open it owns ↑↓/Enter/Tab/Esc: picking
    // INSERTS and never sends (approved composer-skills mock, 2026-07-28).
    if (popOpen) {
      if (e.key === 'ArrowDown') { e.preventDefault(); setPopIndex((i) => Math.min(i + 1, popCount - 1)); return; }
      if (e.key === 'ArrowUp') { e.preventDefault(); setPopIndex((i) => Math.max(i - 1, 0)); return; }
      if (e.key === 'Enter' || e.key === 'Tab') { e.preventDefault(); pickPopRow(popActive); return; }
      if (e.key === 'Escape') { e.preventDefault(); setPopDismissed(popToken); return; }
    }
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      void submit(composerSubmitIntent(running, e.metaKey));
    }
  }

  const hasText = text.trim().length > 0;
  const steerMode = running && hasText;      // typing while running → steer/queue
  const pausedQueue = !running && chat?.queuePaused === true && !!chat.queue?.length;
  // While the daemon socket is down, sending would vanish into a dead queue —
  // block it honestly (the banner above the composer explains why).
  const offline = app.connection !== 'connected';
  const canSend = (running || hasText || pausedQueue) && !offline && !preparingImages;
  const sendGlyph = pausedQueue ? '▶' : steerMode ? '⤴' : running ? '■' : '↑';
  const sendTitle = offline
    ? 'Sin conexión con el daemon'
    : pausedQueue
      ? 'Continuar mensajes en cola'
    : steerMode
      ? (steerAvail || steerBehavior !== 'capability'
        ? 'Dirigir el turno en curso · ⌘Enter'
        : 'Encolar · se envía al terminar el turno')
      : running ? 'Detener' : 'Enviar';

  return (
    <div className="compwrap">
      {popOpen && (
        <div className="slashpop">
          <div className="ptabs">
            <span className={popView === 'commands' ? 'ptab on' : 'ptab'}>Comandos</span>
            {catalogAgents.length > 0 && <span className={popView === 'agents' ? 'ptab on' : 'ptab'}>Agentes</span>}
            {(catalog?.commandsTruncated ?? 0) > 0 && popView === 'commands' && (
              <span className="ptrunc" title="El proveedor reportó más comandos que el tope del catálogo">lista recortada</span>
            )}
          </div>
          <div className="slist" role="listbox">
            {popView === 'commands' && commandMatches.map((m, index) => (
              <button
                key={m.c.name}
                className={index === popActive ? 'pitem on' : 'pitem'}
                role="option"
                aria-selected={index === popActive}
                onMouseDown={(e) => { e.preventDefault(); pickSlash(m.c.name); }}
              >
                <span className="scmd mono"><span className="sigil">/</span><FuzzyName name={m.c.name} hit={m.hit} /></span>
                {m.c.argumentHint && <span className="shint mono">{m.c.argumentHint}</span>}
                {m.c.description && <span className="sdesc">{m.c.description}</span>}
              </button>
            ))}
            {popView === 'agents' && agentMatches.map((m, index) => (
              <button
                key={m.a.name}
                className={index === popActive ? 'pitem on' : 'pitem'}
                role="option"
                aria-selected={index === popActive}
                onMouseDown={(e) => { e.preventDefault(); pickAgent(m.a.name); }}
              >
                <span className="scmd mono"><span className="sigil">@</span><FuzzyName name={m.a.name} hit={m.hit} /></span>
                {m.a.description && <span className="sdesc">{m.a.description}</span>}
                {m.a.model && <span className="pmodel mono">{m.a.model}</span>}
              </button>
            ))}
          </div>
          <div className="pfoot">↑↓ navegar · ⏎ usar · ⇥ completar · esc cerrar</div>
        </div>
      )}
      {chat && <QueueList chat={chat} />}
      {visibleAtts.length > 0 && (
        <div className="attrow">
          {visibleAtts.map((a) => (
            <span key={a.id} className="attchip" title={a.name}>
              <img src={a.url} alt={a.name} tabIndex={0} aria-keyshortcuts="Meta+C Control+C" />
              <button className="attx" title="Quitar" onClick={() => chat && store.removeDraftImages(chat.id, [a.id])}>✕</button>
            </span>
          ))}
        </div>
      )}
      {/* The box now holds ONLY the textarea + the round send/stop button pinned at
          its bottom-right edge (Claude-Code style). Every control moved to the
          external .comprow below, outside the box border. */}
      <div className="comp">
        <textarea
          ref={taRef} rows={1} value={text} placeholder="Contale a tu agente qué sigue…"
          onChange={(e) => change(e.target.value)} onKeyDown={keydown} onPaste={onPaste} onScroll={fade}
          aria-keyshortcuts="Enter Meta+Enter Shift+Enter"
          title={running ? 'Enter: encolar · ⌘Enter: dirigir el turno · ⇧Enter: nueva línea' : 'Enter: enviar · ⇧Enter: nueva línea'}
        />
        <button className={`send ${running && !steerMode ? 'stop' : ''} ${steerMode ? 'steer' : ''}`} disabled={!canSend} onClick={() => void submit(running ? 'steer' : 'send')} title={sendTitle}>
          {running && !steerMode
            // Real centered SVG square — the `■` glyph's font bounding box is
            // asymmetric and renders visibly off-center (user report 2026-07-12).
            ? <svg className="stopsq" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><rect x="3.5" y="3.5" width="9" height="9" rx="2" /></svg>
            : sendGlyph}
        </button>
      </div>
      <div className="comprow">
        <div style={{ position: 'relative' }}>
          <button className="permchip" onClick={() => setModeOpen((v) => !v)} disabled={modes.length === 0}>{permLabel}</button>
          {modeOpen && (
            <Popover cap="Modo de permisos" items={modes.map((m) => ({ id: m.id, name: MODE_LABEL[m.id] ?? m.name }))}
              current={modeId} onPick={(id) => chat && void store.setModeSel(chat.id, id)} onClose={() => setModeOpen(false)} />
          )}
        </div>
        <input ref={fileRef} type="file" accept="image/*" multiple hidden
          onChange={(e) => { if (chat && e.target.files) void addFiles(chat.id, e.target.files); e.target.value = ''; }} />
        <button className="cicon" title={canChooseImage ? 'Adjuntar imagen' : 'Este agente no acepta imágenes'}
          disabled={!canChooseImage} onClick={() => fileRef.current?.click()}><IcPlus /></button>
        <button className={`cicon ${voice === 'idle' ? '' : 'rec'}`} onClick={() => void toggleVoice()}
          disabled={voice === 'transcribing'} aria-pressed={voice === 'recording'}
          title={voice === 'recording' ? 'Tocá para cerrar el micrófono' : voice === 'transcribing' ? 'Transcribiendo…' : 'Voz'}><IcMic /></button>
        {/* Four bars, no text. They prove the microphone is hearing something —
            recording from a muted or wrong input device looks identical without
            them. Hidden while idle so the resting row is unchanged. */}
        {voice !== 'idle' && (
          <span className={`vmeter ${voice === 'transcribing' ? 'busy' : ''}`} aria-hidden="true">
            {[0, 1, 2, 3].map((i) => (
              <i key={i} style={voice === 'recording'
                ? { height: `${Math.max(3, Math.min(13, 3 + level * 34 * (i % 2 ? 1.35 : 0.8)))}px` }
                : undefined} />
            ))}
          </span>
        )}
        <span className="sp" />
        <div className="selectorcluster">
          <div className="selectoranchor">
            <button className="modelsel" onClick={() => setModelOpen((v) => !v)}>{modelName}</button>
            {modelOpen && (grouped
              ? <GroupedModelPopover groups={app.groups} profile={app.meta?.profile ?? 'prod'} currentProvider={chat?.providerId ?? null} current={modelBase || null}
                  favorites={app.modelFavorites}
                  onPick={(providerId, modelId) => chat && void store.pickModel(chat.id, providerId, modelId)}
                  onToggleFavorite={(providerId, modelId) => store.toggleModelFavorite(providerId, modelId)}
                  onClose={() => setModelOpen(false)} />
              : <Popover cap="Modelo" items={userFacingFlatModels(app.models, app.meta?.profile ?? 'prod').map((m) => ({ id: m.modelId, name: m.name }))}
                  current={modelBase || null} onPick={(id) => chat && void store.setModel(chat.id, id)} onClose={() => setModelOpen(false)} />
            )}
          </div>
          {curEffort && (
            <div className="selectoranchor">
              <button className="effortsel" onClick={() => setEffortOpen((v) => !v)} title="Esfuerzo del modelo">
                {/* Fixed label slot sized to the model's longest effort name so
                    changing effort never reflows the row (the model button
                    beside this must not move). */}
                <span className="egauge"><IcGauge /></span>
                <span className="elabel" style={{ minWidth: `${Math.max(...efforts.map((e) => e.length))}ch` }}>{curEffort}</span>
              </button>
              {effortOpen && (
                <EffortPopover efforts={efforts} current={curEffort}
                  onPick={(eff) => chat && void store.setModel(chat.id, `${modelBase}[${eff}]`)}
                  onClose={() => setEffortOpen(false)} />
              )}
            </div>
          )}
          {/* One shared-height cluster keeps model, effort and context on the
              same centerline; their labels remain the full click targets. */}
          <ContextRing usage={contextUsage ?? undefined}
            planBound={!!chat?.providerId}
            planProvider={chat?.providerName ?? chat?.providerId ?? null}
            planUsage={chat?.providerId ? app.planUsageByProvider[chat.providerId] : undefined}
            planUsageLoading={chat?.providerId ? app.planUsageLoadingByProvider[chat.providerId] : false}
            onOpen={() => { if (chat) store.refreshPlanUsage(chat.id); }} />
        </div>
      </div>
    </div>
  );
}
