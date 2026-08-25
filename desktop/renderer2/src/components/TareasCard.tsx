import { useEffect, useRef, useState } from 'react';
import type { ToolEvent, PlanEntry } from '../store/types';
import { store, useApp, useActivity, useSpawnedWork } from '../store/store';
import { toolState, fmtDur, extractSubagents, nodeState, nodeDuration, ToolDetail } from './messages';
import { subagentActivity, type SubagentNode } from '../subagent-layout';
import { SpawnedWorkLive } from './SpawnedWorkCard';
import { renderInline } from '../markdown/inline';
import { IcActivity, ActionGlyph, ModelIcon } from '../icons';
import { toolPresentation } from '../tool-names';
import { displayDetail } from '../tool-display';

// The action slot's icon matches WHAT the call is — a pencil for an edit, a
// document for a read, a terminal for a command — instead of one generic glyph
// for everything (mock b3). It reads from the SAME action vocabulary as the
// transcript rows (tool-names.ts), so the rail and the chat never disagree
// about what a call was.
function toolIconFor(t: ToolEvent) {
  return <ActionGlyph icon={toolPresentation(t).icon} />;
}

// The row's action + the exact evidence behind it, shortened against the chat
// cwd exactly like the transcript does. Only the icon used to come from the
// action vocabulary here; the name stayed the provider's raw id («Read»,
// «/bin/zsh -lc …») until the rail-actions mock (2026-07-27).
function railAction(t: ToolEvent) {
  const act = toolPresentation(t);
  const raw = t.command || t.location || act.evidence || null;
  return { act, raw, detail: raw ? displayDetail(raw, store.active()?.cwd ?? null) : null };
}

// "Turnos" — the live turn of the chat you're watching (approved mocks b3/b4).
// The rail's step-by-step is the agent's REAL plan and nothing else: dots collapse
// the provider's plan entries, tapping expands the exact checklist, and the hero
// is the entry the provider marked in_progress — verbatim.
//
// NOTHING here may be synthesized. An earlier version fabricated "steps" from
// tool-call kinds and used tool titles / streamed thought fragments as the title;
// that was rejected outright. If the provider sent no plan, the rail says so and
// shows no dots — it never invents a step and never infers a current one.

const PLAN_MARK: Record<string, string> = { completed: '✓', in_progress: '◐', pending: '○' };

// Progress as dots only: done = filled, in_progress = ring, pending = hollow.
function PlanDots({ entries }: { entries: readonly PlanEntry[] }) {
  return (
    <span className="r-pdots" aria-hidden="true">
      {entries.map((e, i) => (
        <i key={i} className={e.status === 'completed' ? 'done' : e.status === 'in_progress' ? 'now' : ''} />
      ))}
    </span>
  );
}

// The current action — a FIXED-HEIGHT slot that always holds the most recent
// main-thread call: live (pulse) while it runs, then held in a calm done state
// until the next call replaces it, so the rail never bounces as calls come and
// go. The status light just stops pulsing when the call finishes.
function CurrentCall({ t, nowMs }: { t: ToolEvent; nowMs: number }) {
  const st = toolState(t.status);
  const running = st === 'running';
  const dur = running
    ? (typeof t.startedAt === 'number' ? fmtDur(nowMs - t.startedAt) : '')
    : doneDur(t);
  // No outcome word for a failure and no red — the rail states what ran, not how
  // it ended (user, 2026-07-25). A failed call shows its duration alone; "listo"
  // stays reserved for calls that actually succeeded, so nothing lies.
  const sub = running
    ? `en curso${dur ? ` · ${dur}` : ''}`
    : st === 'failed'
      ? dur
      : st === 'cancelled'
        ? 'cancelado'
        : (dur ? `listo · ${dur}` : 'listo');
  const { act, raw, detail } = railAction(t);
  return (
    <div className="r-live-call" data-status={st}>
      <span className="r-lc-ic">{toolIconFor(t)}</span>
      <span className="r-lc-info">
        <span className="r-lc-n" title={raw || act.raw}>
          {act.label}
          {act.tag && <span className="r-lc-tag"> · {act.tag}</span>}
          {detail && <span className="r-lc-loc"> · {detail}</span>}
        </span>
        <span className="r-lc-s">{sub}</span>
      </span>
      <span className="r-lc-end" aria-hidden="true">
        {running ? <span className="r-pulse" /> : <span className="r-lc-dot" />}
      </span>
    </div>
  );
}

function doneDur(t: ToolEvent): string {
  if (typeof t.startedAt === 'number' && typeof t.endedAt === 'number') return fmtDur(t.endedAt - t.startedAt);
  return '';
}

// Coarse "hace N …" for the settled-turn recap header.
function relTime(ms: number): string {
  const s = Math.round(ms / 1000);
  if (s < 45) return 'recién';
  const m = Math.floor(s / 60);
  if (m < 60) return `hace ${Math.max(1, m)} min`;
  const h = Math.floor(m / 60);
  if (h < 24) return `hace ${h} h`;
  return `hace ${Math.floor(h / 24)} d`;
}

// One subagent as a compact disclosure: model icon + label + elapsed on top, then
// model id + a human live activity. Long labels and model ids ellipsize; the
// elapsed is never pushed off. Exact calls stay one tap away in the body.
function SubagentRow({ n, nowMs }: { n: SubagentNode; nowMs: number }) {
  const st = nodeState(n);
  const running = st === 'running';
  const failed = st === 'failed';
  const dur = nodeDuration(n, nowMs);
  // The row says what it did, not how it ended: no outcome word, no red (user,
  // 2026-07-25). A settled subagent reads the same whatever its exit was.
  const activity = running ? subagentActivity(n) : null;
  const settled = `${n.calls.length} ${n.calls.length === 1 ? 'llamada' : 'llamadas'}`;
  return (
    <details className="r-sa" data-status={st}>
      <summary>
        {/* No chevron: the model icon is the row's anchor, and it reads bigger
            without one (user, 2026-07-24). Hover + open body carry the affordance. */}
        <span className="r-mi" data-p={n.provider ?? undefined}><ModelIcon provider={n.provider} /></span>
        <span className="r-said">
          <span className="r-satop">
            <span className="r-nm" title={n.label}>{n.label}</span>
            {dur && <span className="r-el">{dur}</span>}
          </span>
          <span className="r-sasub">
            {n.model && <span className="r-mdl">{n.model}</span>}
            {n.model && <span className="r-sep" aria-hidden="true">·</span>}
            {activity
              ? (
                <span className="r-act">
                  <span className="a-ic" aria-hidden="true"><ActionGlyph icon={activity.icon} /></span>
                  <span className="a-n">{activity.label}</span>
                </span>
              )
              : (
                <span className="r-act">
                  <span className="a-n">{settled}</span>
                  {failed && <span className="dc-fail"> · falló</span>}
                </span>
              )}
          </span>
        </span>
      </summary>
      {n.calls.length > 0 && (
        <div className="r-sa-b">
          {n.calls.map((t) => <ToolDetail key={t.key} t={t} />)}
        </div>
      )}
    </details>
  );
}

export function TareasCard() {
  const app = useApp();
  useActivity(); // re-render on live tool/plan events
  useSpawnedWork(); // …and when spawned work appears/settles, for the dedupe below
  const [nowMs, setNowMs] = useState(Date.now());
  const cardRef = useRef<HTMLElement>(null);

  const chat = store.active();
  const msg = chat ? [...chat.messages].reverse().find((m) => m.role === 'assistant') ?? null : null;
  // Steering splits one provider turn into assistant continuations separated by
  // canonical user rows. The newest continuation may still be empty (or only a
  // staged placeholder), while every subagent/tool event seen so far remains on
  // an earlier segment. Turnos projects the whole logical turn, not whichever
  // transcript segment happens to be last, so steering cannot make live child
  // work disappear or make the still-running turn look settled.
  const turnRootId = msg?.turnRootId?.trim();
  const turnMessages = !msg
    ? []
    : !turnRootId
      ? [msg]
      : (chat?.messages.filter((message) => message.role === 'assistant' && (
          message.id === turnRootId || message.turnRootId?.trim() === turnRootId
        )) ?? [msg]);
  const running = turnMessages.some((message) => message.status === 'running');
  const events = turnMessages.flatMap((message) => message.events);
  const tools = events.filter((e): e is ToolEvent => e.kind === 'tool');
  const { nodes, mainTools } = extractSubagents(tools);
  // ONE row per subagent. A tracked subagent is registered as spawned work under
  // the same id the header event carries (daemon: registerSubagentSpawnedWork
  // uses run.ID, emitSubagentHeader emits subagentId=run.ID), so while it is
  // live BOTH surfaces would draw it. The live row wins that overlap: it carries
  // the true elapsed and current activity, while the node has been observed
  // reading as settled ("2 llamadas") for a child still working. When the child
  // really ends, the item leaves `running` and the node takes over with its calls.
  const liveSubagentIds = new Set(
    (chat ? store.spawnedWork(chat) : [])
      .filter((item) => item.status === 'running' && item.kind === 'subagent')
      .map((item) => item.id),
  );
  const runningNodes = nodes.filter((n) => nodeState(n) === 'running' && !liveSubagentIds.has(n.id));
  const doneNodes = nodes.filter((n) => nodeState(n) !== 'running' && !liveSubagentIds.has(n.id));
  const runningTools = mainTools.filter((t) => toolState(t.status) === 'running');
  const doneTools = mainTools.filter((t) => toolState(t.status) !== 'running');
  // The most recent main-thread call: the running one if any, else the last
  // finished one — held so the action slot never empties between calls.
  const currentTool = running ? (runningTools[runningTools.length - 1] ?? doneTools[doneTools.length - 1]) : undefined;

  // Actor projection owns the current plan, including an explicit empty plan.
  const planEntries: readonly PlanEntry[] = chat?.planLatest ?? [];
  const hasPlan = planEntries.length > 0;
  const doneCount = planEntries.filter((e) => e.status === 'completed').length;

  // The hero is the provider's in_progress entry, verbatim. Never inferred: no
  // in_progress entry means no sentence.
  const currentStep = planEntries.find((e) => e.status === 'in_progress')?.content ?? null;
  const planLabel = !hasPlan
    ? ''
    : doneCount >= planEntries.length ? 'Plan completado' : `Plan · ${doneCount} de ${planEntries.length}`;

  const starts = tools.map((t) => t.startedAt).filter((x): x is number => typeof x === 'number');
  const turnElapsed = running && starts.length ? fmtDur(nowMs - Math.min(...starts)) : '';
  const ends = tools.map((t) => t.endedAt).filter((x): x is number => typeof x === 'number');
  const lastEnd = ends.length ? Math.max(...ends) : null;
  const when = !running && lastEnd ? relTime(Date.now() - lastEnd) : '';

  const hasContent = hasPlan || nodes.length > 0 || mainTools.length > 0;

  useEffect(() => {
    if (!running) return;
    const iv = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(iv);
  }, [running]);
  useEffect(() => {
    if (app.flashTareas) cardRef.current?.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  }, [app.flashTareas]);

  return (
    <section className={`r-turno${app.flashTareas ? ' flash' : ''}`} ref={cardRef}>
      {running ? (
        <>
          <div className="r-ahora">
            <span className="r-lbl">Ahora</span>
            {turnElapsed && <span className="r-live"><span className="r-pulse" />{turnElapsed}</span>}
          </div>
          {currentStep && <div className="r-say">{renderInline(currentStep)}</div>}
        </>
      ) : hasContent ? (
        <>
          <div className="r-rechead">
            <span className="r-lbl">Último turno</span>
            {when && <span className="r-when">{when}</span>}
          </div>
          {currentStep && <div className="r-recsay">{renderInline(currentStep)}</div>}
        </>
      ) : (
        <div className="r-empty">
          <span className="r-empty-g"><IcActivity /></span>
          <span className="r-empty-t">Sin turno activo</span>
          <span className="r-empty-d">Cuando el agente trabaje, vas a ver acá su rumbo, sus archivos y sus tareas — en vivo.</span>
        </div>
      )}

      {hasPlan ? (
        <details className="r-plan-d">
          <summary>
            <PlanDots entries={planEntries} />
            {!running && planLabel && <span className="r-plabel">{planLabel}</span>}
          </summary>
          <div className="r-plan">
            {planEntries.map((e, i) => (
              <div key={i} className={`r-prow ${e.status === 'completed' ? 'done' : e.status === 'in_progress' ? 'now' : 'todo'}`}>
                <span className="r-mk">{PLAN_MARK[e.status] ?? '•'}</span>
                <span className="r-tx">{renderInline(e.content)}</span>
              </div>
            ))}
          </div>
        </details>
      ) : hasContent ? (
        <div className="r-noplan">Sin plan del agente.</div>
      ) : null}

      {running && <div className="r-hair" />}
      {running && (currentTool
        ? <CurrentCall t={currentTool} nowMs={nowMs} />
        // Reserve the slot before the first call so its arrival doesn't jump.
        // NO placeholder glyph: there is no action yet, so there is nothing for
        // an icon to depict — the squiggle was standing in for one (user,
        // 2026-07-25). The pulsing light already says "live".
        : <div className="r-live-call r-live-idle" data-status="running" aria-hidden="true">
            <span className="r-lc-info"><span className="r-lc-s">en curso</span></span>
            <span className="r-lc-end"><span className="r-pulse" /></span>
          </div>)}
      {/* EVERY subagent of this turn is one flat list — a finished one is not
          worth a fold inside a fold inside a row (user, 2026-07-25). Running
          first, then settled; each still opens to its own calls. */}
      {(runningNodes.length > 0 || doneNodes.length > 0) && (
        <div className="r-subs">
          {runningNodes.map((n) => <SubagentRow key={n.id} n={n} nowMs={nowMs} />)}
          {doneNodes.map((n) => <SubagentRow key={n.id} n={n} nowMs={nowMs} />)}
        </div>
      )}
      {/* Running background processes live HERE — inline with the live call and
          subagents, never behind a fold (approved mock f-bg2). They outlive
          turns, so they render in the recap/empty states too. */}
      {chat && <SpawnedWorkLive chat={chat} />}

      {/* Llamadas now holds ONLY the main thread's own calls; subagents left it. */}
      {doneTools.length > 0 && (
        <details className="r-meta">
          <summary>
            <span className="r-ml">Llamadas</span>
            <span className="r-count">{doneTools.length}</span>
          </summary>
          <div className="r-meta-b">
            {doneTools.map((t) => {
              const st = toolState(t.status);
              const { act, raw, detail } = railAction(t);
              return (
                <div key={t.key} className="r-dline r-dcall" data-status={st}>
                  {st === 'cancelled' && <span className="r-g">–</span>}
                  <span className="r-dic" aria-hidden="true"><ActionGlyph icon={act.icon} /></span>
                  <span className="r-dt" title={raw || act.raw}>{act.label}</span>
                  {/* the evidence column: the exact path/command, or the MCP
                      server when the call had none — mono, ellipsized, never
                      pushing the duration off the row */}
                  {detail
                    ? <span className="r-dev">{detail}</span>
                    : act.tag && <span className="r-dev">· {act.tag}</span>}
                  {doneDur(t) && <span className="r-dl">{doneDur(t)}</span>}
                </div>
              );
            })}
          </div>
        </details>
      )}
    </section>
  );
}
