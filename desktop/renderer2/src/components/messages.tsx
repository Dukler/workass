import { memo, useEffect, useState, type ReactNode } from 'react';
import type { ThinkingEvent, PlanEvent, ToolEvent, PermissionState, RestoredEvent, BgProcEvent, MessageImage, SteerState } from '../store/types';
import { renderInline } from '../markdown/inline';
import { IcShield, IcBgProc, ModelIcon, ActionGlyph } from '../icons';
import { store } from '../store/store';
import { messageImageSrc } from '../image-drafts';
import { isSubagentHeader, type SubagentNode } from '../subagent-layout';
import { steerStatusLabel } from '../steering';
import { displayDetail, splitTail } from '../tool-display';
import { toolPresentation } from '../tool-names';

export { extractSubagents } from '../subagent-layout';
export type { SubagentNode } from '../subagent-layout';

export const UserPill = memo(function UserPill({ text, images, steerState }: { text: string; images?: MessageImage[]; steerState?: SteerState }) {
  return (
    <div className={`userpill${steerState ? ` steer-${steerState}` : ''}`}>
      {!!images?.length && (
        <div className="user-images">
          {images.map((image, index) => {
            const src = messageImageSrc(image);
            const alt = image.name || `Imagen adjunta ${index + 1}`;
            // Click to open full-size in the lightbox.
            return (
              <img
                key={`${image.mimeType}-${index}`}
                src={src}
                alt={alt}
                className="zoomable"
                title="Ampliar"
                tabIndex={0}
                aria-keyshortcuts="Meta+C Control+C"
                onClick={() => store.openImageLightbox(src, alt)}
              />
            );
          })}
        </div>
      )}
      {!!text.trim() && <div className="userpill-text">{renderInline(text)}</div>}
      {steerState && (
        <div className={`steerstate ${steerState}`} role="status" aria-live="polite">
          {(steerState === 'sending' || steerState === 'uncertain') && (
            <span className="steerstate-mark" aria-hidden="true">{steerState === 'uncertain' ? '!' : '·'}</span>
          )}
          {steerStatusLabel(steerState)}
        </div>
      )}
    </div>
  );
});

// Reasoning arrives from BOTH providers as short bold step titles
// ("**Parsing process tree with ps and awk**"). The old row glued them onto one
// line and sliced it at 90 characters, which is where the mid-word cuts came
// from. Only titles closed with ** are promoted, so a title still streaming in
// is never shown half-written — and nothing is ever sliced by length.
function stepTitles(text: string): string[] {
  const out: string[] = [];
  for (const m of text.matchAll(/\*\*([^*]+)\*\*/g)) {
    const title = m[1].trim();
    if (title && out[out.length - 1] !== title) out.push(title);
  }
  return out;
}

// Each word fades and sharpens into place, 34ms after the one before. Opacity
// and blur only — nothing moves — so it stays appropriate when the OS asks for
// reduced motion. Remounting on `text` is what replays it per title.
function StepWords({ text }: { text: string }) {
  return (
    <span className="stwords">
      {text.split(' ').map((word, i) => (
        <span key={`${i}-${word}`} className="stword" style={{ animationDelay: `${i * 34}ms` }}>{word} </span>
      ))}
    </span>
  );
}

export { StepWords };

export function StepRow({ ev, live = false, fallbackLabel }: { ev: ThinkingEvent; live?: boolean; fallbackLabel?: string }) {
  const [open, setOpen] = useState(false);
  const titles = stepTitles(ev.text);

  // Live: the step it is on right now. Nothing closed yet falls back to the
  // turn's whimsy word (Claude Code's spinner idiom) rather than a fragment.
  // Rendered pinned at the tail of the turn by AssistantMessage, never inline,
  // so a growing answer can't scroll it away.
  if (live) {
    const current = titles[titles.length - 1] ?? fallbackLabel ?? 'Pensando';
    return (
      <div className="steprow steplive" role="status" aria-live="polite">
        <span className="stmark"><span className="ping" aria-hidden="true" /></span>
        <span className="stbody"><StepWords key={current} text={current} /></span>
      </div>
    );
  }

  // Settled: one quiet line. It opens the trail of steps — no numbers, no
  // marks, so it never reads as a second copy of the rail's plan.
  return (
    <button className="steprow" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
      <span className="stbody">
        <span className="stlbl">Pensó</span>
        {open && <div className="stexpand">{(titles.length ? titles : [ev.text]).join('\n')}</div>}
      </span>
    </button>
  );
}

// Quiet in-transcript marker: the daemon auto-compacted the context (D1/R3).
export function CompactionRow() {
  return (
    <div className="steprow compactrow" style={{ cursor: 'default' }}>
      <span className="chev">›</span>
      <span className="stbody">Contexto compactado</span>
    </div>
  );
}

// Quiet step row dropped after a successful rewind (R4/D3).
export function RestoredRow({ ev }: { ev: RestoredEvent }) {
  return (
    <div className="steprow compactrow" style={{ cursor: 'default' }}>
      <span className="chev">↺</span>
      <span className="stbody">Estado restaurado a antes del turno {ev.turnSeq} ›</span>
    </div>
  );
}

// Live inline row for a background process tied to this chat (its ACP engine /
// spawned aux processes). Ticks elapsed while running; settles with a gentle
// fade when the process ends. No controls here — stopping stays in the Tareas
// card. Reduced-motion is honored in CSS.
function bgDuration(startIso: string, endMs: number): string {
  const start = new Date(startIso).getTime();
  if (Number.isNaN(start)) return '';
  let s = Math.max(0, Math.round((endMs - start) / 1000));
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}
export function BgProcRow({ ev }: { ev: BgProcEvent }) {
  const running = ev.status === 'running';
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!running) return;
    const iv = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(iv);
  }, [running]);
  const endMs = running ? now : (ev.endedAt ? new Date(ev.endedAt).getTime() : now);
  const dur = bgDuration(ev.startedAt, endMs);
  const cls = running ? 'run' : ev.status === 'failed' ? 'fail' : 'done';
  return (
    <div className={`bgprocrow ${cls}`}>
      <span className="bgic"><IcBgProc /></span>
      <span className="bgtext">
        Proceso en segundo plano: <b>{ev.label}</b><span className="bgsep"> · </span>
        {running
          ? <>en curso · {dur}</>
          : ev.status === 'failed'
            ? <span className="bgfail">falló{ev.code != null ? ` · código ${ev.code}` : ''} · {dur}</span>
            : <>terminó · {dur}</>}
      </span>
      {running && <span className="bgdot" aria-hidden="true" />}
    </div>
  );
}

const PLAN_MARK: Record<string, string> = { completed: '✓', in_progress: '◐', pending: '○' };
export function PlanView({ ev }: { ev: PlanEvent }) {
  return (
    <div className="plan">
      {ev.entries.map((e, i) => (
        <div key={i} className={`prow ${e.status === 'completed' ? 'done' : e.status === 'in_progress' ? 'run' : ''}`}>
          <span className="pmark">{PLAN_MARK[e.status] ?? '•'}</span>
          <span>{renderInline(e.content)}</span>
        </div>
      ))}
    </div>
  );
}

// ── Grouped, collapsible tool runs ───────────────────────────────────────────
// Restored from the old renderer (desktop/renderer/chat-render.js): a run of
// consecutive tool calls fired at the same text offset (no assistant prose
// between them) collapses into ONE expandable line instead of a card per call.
// Done groups fold; the in-progress group auto-opens with a live dot. This is
// the "colapsing tasks" behavior the new renderer regressed away.
export const TOOL_STATUS: Record<string, string> = { in_progress: 'en curso', pending: 'pendiente', completed: 'listo', failed: 'falló', error: 'falló', cancelled: 'cancelado', canceled: 'cancelado' };
export const isToolDone = (s: string) => s === 'completed' || s === 'success';
export const isToolFailed = (s: string) => s === 'failed' || s === 'error';
export const isToolCancelled = (s: string) => s === 'cancelled' || s === 'canceled';
export type ToolState = 'done' | 'failed' | 'cancelled' | 'running';
export function toolState(s: string): ToolState {
	return isToolFailed(s) ? 'failed' : isToolCancelled(s) ? 'cancelled' : isToolDone(s) ? 'done' : 'running';
}
export function groupState(tools: ToolEvent[]): ToolState {
	let failed = false, cancelled = false, allSettled = true;
	for (const t of tools) {
		const state = toolState(t.status);
		if (state === 'failed') failed = true;
		if (state === 'cancelled') cancelled = true;
		if (state === 'running') allSettled = false;
	}
	return failed ? 'failed' : allSettled ? (cancelled ? 'cancelled' : 'done') : 'running';
}
const GLYPH: Record<ToolState, string> = { done: '✓', failed: '✕', cancelled: '–', running: '◌' };

// Spanish count-noun per ACP tool category — used to summarize a multi-call run.
const KIND_NOUN: Record<string, [string, string]> = {
  execute: ['comando', 'comandos'], read: ['lectura', 'lecturas'], edit: ['edición', 'ediciones'],
  delete: ['borrado', 'borrados'], move: ['movimiento', 'movimientos'], search: ['búsqueda', 'búsquedas'],
  fetch: ['descarga', 'descargas'], agent: ['subagente', 'subagentes'], think: ['razonamiento', 'razonamientos'],
  other: ['herramienta', 'herramientas'],
};
const KIND_ORDER = ['execute', 'read', 'edit', 'delete', 'move', 'search', 'fetch', 'agent', 'think', 'other'];
// Prefer the real ACP kind; else sniff the title (covers reloaded history that
// predates toolKind). Unknown → 'other'.
function toolKindOf(t: ToolEvent): string {
  if (isSubagentHeader(t)) return 'agent';
  const k = String(t.toolKind || '').toLowerCase();
  if (KIND_NOUN[k]) return k;
  const title = String(t.title || '').toLowerCase();
  if (/^\s*\$|(^|\b)(ran|run|exec|command|terminal|shell|bash|npm|git)/.test(title)) return 'execute';
  if (/(^|\b)(read|reading|view|cat|open|le[ií])/.test(title)) return 'read';
  if (/(^|\b)(edit|wrote|write|creat|modif|patch|append|replac|escrib)/.test(title)) return 'edit';
  if (/(^|\b)(search|grep|find|glob|busc)/.test(title)) return 'search';
  if (/(^|\b)(fetch|download|http|web|url|curl|descarg)/.test(title)) return 'fetch';
  if (/(^|\b)(delet|remov|rm|borr)/.test(title)) return 'delete';
  if (/(^|\b)(move|mv|rename|mov)/.test(title)) return 'move';
  return 'other';
}
// A single call keeps its specific title; a run summarizes by kind: "3 lecturas · 1 búsqueda".
function groupSummary(tools: ToolEvent[]): string {
  if (tools.length === 1) {
    const t = tools[0];
    if (isSubagentHeader(t)) return `Subagente · ${t.title || 'tarea'}`;
    return t.title || 'herramienta';
  }
  const counts = new Map<string, number>();
  for (const t of tools) { const k = toolKindOf(t); counts.set(k, (counts.get(k) ?? 0) + 1); }
  const frags = KIND_ORDER.filter((k) => counts.has(k)).map((k) => {
    const n = counts.get(k)!; return `${n} ${KIND_NOUN[k][n > 1 ? 1 : 0]}`;
  });
  return frags.join(' · ') || 'herramientas';
}
export function fmtDur(ms: number): string {
  if (!(ms >= 0)) return '';
  const s = ms / 1000;
  if (s < 10) return `${s.toFixed(1)}s`;
  if (s < 60) return `${Math.round(s)}s`;
  const m = Math.floor(s / 60); const rem = Math.round(s - m * 60);
  return rem ? `${m}m ${rem}s` : `${m}m`;
}
// Real observed duration from the stamped startedAt/endedAt (absent on old
// history → ''). Live groups tick from the earliest start to now.
export function groupDuration(tools: ToolEvent[], nowMs: number, running: boolean): string {
  const starts = tools.map((t) => t.startedAt).filter((x): x is number => typeof x === 'number');
  if (!starts.length) return '';
  const start = Math.min(...starts);
  if (running) return fmtDur(nowMs - start);
  const ends = tools.map((t) => t.endedAt).filter((x): x is number => typeof x === 'number');
  if (!ends.length) return '';
  return fmtDur(Math.max(...ends) - start);
}

// `standalone` renders the row as its OWN status line (a single-call group has no
// wrapper — see ToolGroupView); `trail` is the right-aligned duration/live-dot the
// group summary used to carry, now folded into this one line.
export function ToolDetail({ t, standalone, trail }: { t: ToolEvent; standalone?: boolean; trail?: ReactNode }) {
  const [showOut, setShowOut] = useState(false);
  const st = toolState(t.status);
  // A subagent header rendered inline as a task row: model brand instead of the
  // status glyph, a "Subagente ·" prefix, and a link to its detail in the rail
  // (its own tool-calls live only in Turnos). No inline expansion here.
  if (isSubagentHeader(t)) {
    return (
      <div className="evt evt-sub" data-status={st}>
        <div className="evt-head">
          <span className="mico"><ModelIcon provider={t.subagentProvider} /></span>
          <span className="evt-title">Subagente · {t.title}</span>
          <span className="evt-st">{TOOL_STATUS[t.status] ?? t.status}</span>
          <button className="evt-turnos" title="Ver su detalle en el panel Turnos" onClick={() => store.focusTareas()}>Ver en Turnos ›</button>
        </div>
      </div>
    );
  }
  // Success is SILENT (no ✓ — in a settled list it says nothing); only
  // failed/cancelled/running rows get a glyph. The tool name never shrinks;
  // the detail takes every remaining px: relative/~ paths, and a lone path
  // splits head/tail so the MIDDLE ellipsizes and the filename always shows.
  // No «salida» button — the row itself toggles the output (approved mock
  // 2026-07-15, toolrow-redesign v2).
  //
  // The row leads with the ACTION, not the provider's tool id (approved mock
  // toolrow-names, 2026-07-25): «Ejecutar un comando» + terminal glyph instead
  // of «Bash», «Cancelar un subagente · workass» instead of
  // «mcp__workass-agent__workass_cancel_subagent». When the title WAS the
  // evidence (Codex sends the whole command line), it falls through to the
  // detail column, which ellipsizes properly instead of eating the row.
  const act = toolPresentation(t);
  const raw = t.command || t.location || act.evidence || null;
  const detail = raw ? displayDetail(raw, store.active()?.cwd ?? null) : null;
  const split = detail && !t.command ? splitTail(detail) : null;
  const dur = typeof t.startedAt === 'number' && typeof t.endedAt === 'number' ? fmtDur(t.endedAt - t.startedAt) : '';
  // Success is silent. Failure is carried by a red command/path (see .evt-loc
  // failed rule), or a quiet trailing status when no detail exists — never a
  // ✕ + red title (user report 2026-07-16). Cancelled still shows its faint «–»;
  // a standalone running row shows the live dot in its trail so it drops the
  // leading ◌.
  const showGlyph = st === 'cancelled' || (st === 'running' && !standalone);
  return (
    <div
      className={`evt${t.output ? ' hasout' : ''}${standalone ? ' evt-solo' : ''}`}
      data-status={st}
      onClick={t.output ? () => setShowOut((v) => !v) : undefined}
      title={raw && raw !== detail ? raw : act.label !== act.raw ? act.raw : undefined}
    >
      <div className="evt-head">
        {showGlyph && <span className={`evt-ic ${st}`}>{GLYPH[st]}</span>}
        <span className="evt-kic" aria-hidden="true"><ActionGlyph icon={act.icon} /></span>
        <span className="evt-title">{act.label}</span>
        {act.tag && <span className="evt-tag">· {act.tag}</span>}
        {detail && (split
          ? <span className="evt-loc mono"><span className="lh">{split.head}</span><span className="lt">{split.tail}</span></span>
          : <span className="evt-loc mono">{detail}</span>)}
        {st === 'failed' && !detail && <span className="evt-fail">{TOOL_STATUS[t.status] ?? t.status}</span>}
        {trail}
      </div>
      {showOut && t.output && (
        <div className="evt-out">
          {dur && <div className="evt-dur">{dur}</div>}
          {/* the provider's own id, so a renamed row never hides WHICH tool ran */}
          {act.raw && act.raw !== raw && act.raw !== act.label && <div className="evt-raw">{act.raw}</div>}
          {raw && raw !== detail && <div className="evt-full">{raw}</div>}
          {t.output}
        </div>
      )}
    </div>
  );
}

function ToolImageGallery({ tools }: { tools: ToolEvent[] }) {
  const images = tools.flatMap((tool) => (tool.images ?? []).map((image) => ({ image, tool })));
  if (!images.length) return null;
  return (
    <div className="tool-images" aria-label="Imágenes devueltas por herramientas">
      {images.map(({ image, tool }, index) => {
        const src = messageImageSrc(image);
        const alt = image.name || `${tool.title || 'Herramienta'} · imagen ${index + 1}`;
        return (
          <button className="tool-image" key={`${tool.key}-${image.mimeType}-${index}`} onClick={() => store.openImageLightbox(src, alt)} title="Ampliar">
            <img src={src} alt={alt} />
          </button>
        );
      })}
    </div>
  );
}

interface ToolGroupProps { tools: ToolEvent[]; revision?: readonly unknown[] }

function ToolGroupView({ tools }: ToolGroupProps) {
  const st = groupState(tools);
  const running = st === 'running';
  // Always start collapsed — one clean line whether running or done. No
  // auto-open (a running call is NOT a 2-line drawer) and no auto-collapse
  // ("out of nowhere" on completion, user report 2026-07-12). Click to expand.
  const [open, setOpen] = useState(false);
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!running) return;
    const iv = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(iv);
  }, [running]);
  const dur = groupDuration(tools, now, running);

  // One call → no group layer. Wrapping a single call in a summary duplicated its
  // title (summary + detail both showed it) and forced two opens to reach the
  // output (user report 2026-07-15). Render the call as its own status line: one
  // title, the path on the same line, one click to the output. The heavy output
  // stays lazy (behind the row's own toggle); only the small head mounts eagerly.
  if (tools.length === 1) {
    const trail = running
      ? <span className="evt-run" aria-hidden="true" />
      : dur ? <span className="evt-dur-head">{dur}</span> : null;
    return (
      <>
        <ToolImageGallery tools={tools} />
        <div className="toolsolo" data-status={st}>
          <ToolDetail t={tools[0]} standalone trail={trail} />
        </div>
      </>
    );
  }

  return (
    <>
      <ToolImageGallery tools={tools} />
      <div className="toolgroup" data-status={st}>
        <button
          type="button"
          className="tg-summary"
          aria-expanded={open}
          onClick={() => setOpen((v) => !v)}
        >
          {/* no caret — hover highlight signals clickability; open state shows itself.
              success silent: the ✓ said nothing; running has its own live dot.
              failed drops the ✕ too — the expanded failed row carries the red cue */}
          {st === 'cancelled' && <span className={`tg-ic ${st}`}>{GLYPH[st]}</span>}
          <span className="tg-title">
            {groupSummary(tools)}
            {tools.length > 1 && <span className="tg-count"> · {tools.length} llamadas</span>}
          </span>
          {running ? <span className="tg-run" aria-hidden="true" /> : dur && <span className="tg-dur">{dur}</span>}
        </button>
        {open && (
          <div className="tg-body">
            {tools.map((t) => <ToolDetail key={t.key} t={t} />)}
          </div>
        )}
      </div>
    </>
  );
}

function sameRevision(previous: readonly unknown[] | undefined, next: readonly unknown[] | undefined): boolean {
  if (!previous || !next || previous.length !== next.length) return false;
  for (let index = 0; index < previous.length; index++) if (previous[index] !== next[index]) return false;
  return true;
}

export const ToolGroup = memo(ToolGroupView, (previous, next) => sameRevision(previous.revision, next.revision));

// ── Subagent tracking (rail-owned) ───────────────────────────────────────────
// When the agent spawns Task subagents, the daemon stamps each nested tool call
// with subagentId (the spawning call's id), subagentLabel, subagentProvider and
// subagentModel. In the transcript a subagent is just one task row (its header,
// folded into the normal tool group); ITS OWN tool-calls are shown only in the
// Turnos rail, one expandable node per subagent (approved mock, 2026-07-12).
export function nodeState(n: SubagentNode): ToolState {
  if (n.header) return toolState(n.header.status);
  return groupState(n.calls);
}
export function nodeDuration(n: SubagentNode, nowMs: number): string {
  const evs = n.header ? [n.header, ...n.calls] : n.calls;
  return groupDuration(evs, nowMs, nodeState(n) === 'running');
}
const PERM_LABEL: Record<string, string> = {
  allow_once: 'Permitir una vez', allow_always: 'Permitir siempre', allow: 'Permitir',
  reject_once: 'Rechazar', reject_always: 'Rechazar siempre', reject: 'Rechazar', cancel: 'Cancelar',
};
export function PermCard({ perm, tabId, msgId }: { perm: PermissionState; tabId: string; msgId: string }) {
  const decide = (optionId: string) => { if (!perm.resolved) void store.decidePermission(tabId, msgId, perm.id, optionId); };
  // A question carries its own answers: `options` holds one entry per choice
  // (kind 'answer') plus the escape hatch, so they are answered here rather than
  // allowed/rejected. Falls back to the permission card if the daemon predates
  // the question field.
  if (perm.question) {
    const answers = perm.options.filter((o) => o.kind === 'answer');
    const skip = perm.options.find((o) => o.kind !== 'answer');
    return (
      <div className="permcard ask">
        {perm.question.header && <div className="askhead">{perm.question.header}</div>}
        <div className="askq">{perm.question.question}</div>
        <div className="askopts">
          {answers.map((o, i) => (
            <button
              key={o.optionId}
              className={`askopt ${perm.resolved === o.optionId ? 'on' : ''}`}
              disabled={!!perm.resolved}
              onClick={() => decide(o.optionId)}
            >
              <div className="asklabel">{o.name}</div>
              {perm.question?.options[i]?.description && <div className="askdesc">{perm.question.options[i].description}</div>}
            </button>
          ))}
        </div>
        {skip && (
          <button className="askskip" disabled={!!perm.resolved} onClick={() => decide(skip.optionId)}>{skip.name}</button>
        )}
      </div>
    );
  }
  return (
    <div className="permcard">
      <div className="wicon"><IcShield /></div>
      <div className="ptext">
        Quiere ejecutar <code>{perm.title}</code>
        <small>Corre en tu máquina con el modo de permisos actual.</small>
      </div>
      <span className="sp" />
      <div className="btns">
        {perm.options.map((o) => {
          const label = PERM_LABEL[o.kind] ?? o.name ?? o.kind;
          const isAllow = /allow/i.test(o.kind) || /allow/i.test(o.name || '');
          const chosen = perm.resolved === o.optionId;
          return (
            <button
              key={o.optionId}
              className={`btn ${isAllow ? 'pri' : ''}`}
              disabled={!!perm.resolved}
              style={chosen ? { borderColor: 'var(--acc)', color: 'var(--acc)' } : undefined}
              onClick={() => decide(o.optionId)}
            >{label}</button>
          );
        })}
      </div>
    </div>
  );
}
