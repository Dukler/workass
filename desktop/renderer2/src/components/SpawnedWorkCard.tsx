import { useEffect, useState } from 'react';
import { subscribeSecond } from '../second-tick';
import type { Chat } from '../store/types';
import type { SpawnedWorkItem } from '../wire/types';
import { store, useSpawnedWork } from '../store/store';
import { fmtDur } from './messages';
import { ActionGlyph, IcCheck, IcChevDown, IcChevron, IcMachineLocal, IcMachineServer, IcStopSquare, IcTerminal, ModelIcon } from '../icons';
import { spawnedWorkActivity, spawnedWorkKindWord } from '../tool-names';
import { displayDetail } from '../tool-display';
import { startSerialPoll } from '../serial-poll';
import { workHostName, canStopSpawnedWorkItem, currentTurnMessages, extractSubagents, isAgentWork, nestSpawnedWork, nodeForSpawnedWork, subagentWorkId } from '../subagent-layout';
import {
  finishedToday, fmtSpan, fmtWait, isQueuedWork, nextInQueue, queueGroups, workLabel, workMachine, workRunMs, workWaitMs,
  type MachineContext,
} from '../rail-work';
import { railQueueOpen, setRailQueueOpen } from '../rail-view';
import { SubagentRow } from './SubagentRow';
import type { ToolEvent } from '../store/types';

function itemDuration(item: SpawnedWorkItem, nowMs: number): string {
  const end = item.finishedAt ? Date.parse(item.finishedAt) : nowMs;
  if (isQueuedWork(item)) {
    const waited = Date.parse(item.queuedAt!);
    return Number.isFinite(waited) ? `en cola ${fmtDur(Math.max(0, nowMs - waited))}` : 'en cola';
  }
  // The run alone. A supervised job that never ran has no run time to show.
  if (item.runMs) return fmtDur(item.runMs);
  if (item.queuedAt && !item.execStartedAt) return '';
  const start = Date.parse(item.execStartedAt || item.startedAt);
  return Number.isFinite(start) && Number.isFinite(end) ? fmtDur(Math.max(0, end - start)) : '';
}

// A glyph only when it depicts the work: a terminal for a shell, the brand mark
// for an agent. Anything else gets NO icon rather than a catch-all squiggle
// (user, 2026-07-25) — the row's own kind line already names it.
function KindIcon({ item }: { item: SpawnedWorkItem }) {
  if (item.kind === 'bash') return <IcTerminal />;
  // A tracked subagent's live row is now the ONLY row it gets (TareasCard drops
  // the duplicate node), so it carries the registration-projected brand mark.
  if (item.kind === 'agent' || item.kind === 'subagent') {
    return item.assistantBrand ? <ModelIcon provider={item.assistantBrand} /> : null;
  }
  return null;
}

export function MachineGlyph({ local }: { local: boolean }) {
  return <span className="wmg" aria-hidden="true">{local ? <IcMachineLocal /> : <IcMachineServer />}</span>;
}

// The live output tail of one row, read only while it is open.
function useWorkTail(chat: Chat, item: SpawnedWorkItem) {
  const [tailOpen, setTailOpen] = useState(false);
  const [tail, setTail] = useState('');
  const [tailLimited, setTailLimited] = useState(false);
  const [tailError, setTailError] = useState('');
  const tabId = chat.id;
  const chatId = chat.chatId;

  useEffect(() => {
    if (!tailOpen || !item.outputFile) return;
    let alive = true;
    const refresh = async () => {
      const result = await store.readSpawnedWork({ id: tabId, chatId }, item.id);
      if (!alive || !result) return;
      if (!result.ok) {
        setTailError(result.error || 'Salida no disponible.');
        return;
      }
      setTail(result.tail || '');
      setTailLimited(!!result.tailLimited);
      setTailError('');
    };
    const stop = startSerialPoll(refresh, 2000);
    return () => { alive = false; stop(); };
  }, [tabId, chatId, item.id, item.outputFile, tailOpen]);

  return { tailOpen, toggleTail: () => setTailOpen((open) => !open), tail, tailLimited, tailError };
}

// Ending the row is the one thing this rail could show forever without ever
// offering: a lane whose process died without a done-file reads "running"
// until someone settles it, and a dev server nobody needs any more keeps its
// chat's rail busy. The daemon does both; this is the button.
function useWorkStop(chat: Chat, item: SpawnedWorkItem) {
  const [stopping, setStopping] = useState(false);
  const canStop = store.canStopSpawnedWork() && canStopSpawnedWorkItem(item);
  const stop = async () => {
    if (stopping) return;
    setStopping(true);
    try {
      const result = await store.stopSpawnedWork(chat, item.id);
      // A refusal is the only thing worth interrupting for. Success removes the
      // row from this zone on the daemon's own event, which is the receipt.
      if (result && !result.ok) store.addToast('No se pudo detener', result.error || 'El daemon rechazó la parada.');
    } finally {
      setStopping(false);
    }
  };
  return { canStop, stopping, stop };
}

// One RUNNING spawned process an agent started: an inline flat row under that
// agent. Title + elapsed, a mono meta line, the daemon's live summary, and a
// per-row output toggle.
function RunningRow({ chat, item, nowMs }: { chat: Chat; item: SpawnedWorkItem; nowMs: number }) {
  const { tailOpen, toggleTail, tail, tailLimited, tailError } = useWorkTail(chat, item);
  const { canStop, stopping, stop } = useWorkStop(chat, item);

  const meta = [
    spawnedWorkKindWord(item.kind),
    item.kind === 'agent' ? item.providerId : '',
    item.modelLabel || '',
    item.pid ? `pid ${item.pid}` : '',
    workHostName(item),
  ].filter(Boolean).join(' · ');
  // The live line is an ACTION like every other row in the rail: glyph + human
  // name + the exact evidence (approved mock rail-actions, 2026-07-27). It used
  // to print the daemon's raw phase and the provider's raw tool id — «subagent»
  // / «Read». Text we could not classify still renders verbatim and, honestly,
  // with no glyph.
  const act = spawnedWorkActivity(item);
  const evidence = act?.evidence ? displayDetail(act.evidence, chat.cwd ?? null) : '';
  const live = act?.label ?? '';
  const duration = itemDuration(item, nowMs);
  return (
    <div className="bgr" data-kind={item.kind} data-work-id={item.id}>
      <span className="bgr-ic" aria-hidden="true"><KindIcon item={item} /></span>
      <span className="bgr-info">
        <span className="bgr-top">
          <span className="bgr-n" title={item.label || item.taskId}>{item.label || item.taskId}</span>
          {duration && <span className="bgr-el">{duration}</span>}
        </span>
        <span className="bgr-meta">{meta}</span>
        {live && (act?.unclassified
          ? <span className="bgr-sum" title={live}>{live}</span>
          : (
            <span className="bgr-act" title={act?.evidence || act?.raw || live}>
              <span className="a-ic" aria-hidden="true"><ActionGlyph icon={act!.icon} /></span>
              <span className="a-n">{live}</span>
              {act?.tag && <span className="a-tag">· {act.tag}</span>}
              {evidence && <span className="a-ev mono">{evidence}</span>}
            </span>
          ))}
        {item.outputFile && (
          <button type="button" className="bgr-out" onClick={toggleTail}>
            {tailOpen ? 'ocultar salida' : 'ver salida'}
          </button>
        )}
        {tailOpen && (
          <pre className="bgr-tail">
            {tailError || tail || 'Sin salida todavía.'}{tailLimited ? '\n… salida recortada' : ''}
          </pre>
        )}
      </span>
      <span className="bgr-end">
        {!isQueuedWork(item) && <span className="r-pulse" aria-label="En curso" />}
        {canStop && (
          <button
            type="button"
            className="bgr-stop"
            title={stopping ? 'Deteniendo…' : 'Detener'}
            aria-label="Detener"
            disabled={stopping}
            onClick={() => void stop()}
          >
            <IcStopSquare />
          </button>
        )}
      </span>
    </div>
  );
}

// What a background row is doing, in the words the rest of the rail uses: the
// daemon's live activity, else the command a supervised job runs, shortened
// against the chat's folder.
function jobDescription(item: SpawnedWorkItem, cwd: string | null): string {
  const act = spawnedWorkActivity(item);
  if (act) {
    if (act.unclassified) return displayDetail(act.label, cwd);
    return [act.label, act.evidence ? displayDetail(act.evidence, cwd) : ''].filter(Boolean).join(' ');
  }
  const word = spawnedWorkKindWord(item.kind);
  return word !== item.kind ? word : '';
}

// One running background job (mock rail-v7): a live dot, the name on one line,
// its run time, where it runs and what it runs. Its two actions show on hover
// (always on a touch screen, and while its output is open).
function JobRow({ chat, item, nowMs, context }: { chat: Chat; item: SpawnedWorkItem; nowMs: number; context: MachineContext }) {
  const { tailOpen, toggleTail, tail, tailLimited, tailError } = useWorkTail(chat, item);
  const { canStop, stopping, stop } = useWorkStop(chat, item);
  const machine = workMachine(item, context);
  const label = workLabel(item);
  const description = jobDescription(item, chat.cwd ?? null);
  const meta = [machine.name, description].filter(Boolean).join(' · ');
  const elapsed = fmtSpan(workRunMs(item, nowMs));
  const hasActions = !!item.outputFile || canStop;
  return (
    <div className={`wjob${tailOpen ? ' tail-open' : ''}`} data-kind={item.kind} data-work-id={item.id}>
      <span className="wdot" aria-label="En curso" />
      <span className="wjob-n" title={label}>{label}</span>
      <span className="wjob-t">{elapsed}</span>
      <span className="wjob-m" title={[meta, item.pid ? `pid ${item.pid}` : ''].filter(Boolean).join(' · ')}>
        <MachineGlyph local={machine.local} />
        <span className="wjob-mt">{meta}</span>
      </span>
      {hasActions && (
        <span className="wjob-acts">
          {item.outputFile && (
            <button type="button" className="wbtn" aria-expanded={tailOpen} onClick={toggleTail}>
              {tailOpen ? 'Ocultar salida' : 'Ver salida'}
            </button>
          )}
          {canStop && (
            <button type="button" className="wbtn stop" aria-label={`Detener ${label}`} disabled={stopping} onClick={() => void stop()}>
              {stopping ? 'Deteniendo…' : 'Detener'}
            </button>
          )}
        </span>
      )}
      {tailOpen && (
        <pre className="bgr-tail wjob-tail">
          {tailError || tail || 'Sin salida todavía.'}{tailLimited ? '\n… salida recortada' : ''}
        </pre>
      )}
    </div>
  );
}

// At most this many running rows; the rest fold into one line that opens in place.
export const RUNNING_ROWS_SHOWN = 4;

// «Trabajos en segundo plano»: the chat's RUNNING background work, inline flat
// rows that outlive turns (daemon spawned-work registry). What waits is the
// queue's, below.
export function BackgroundJobs({ chat, running, nowMs, context }: { chat: Chat; running: readonly SpawnedWorkItem[]; nowMs: number; context: MachineContext }) {
  const [all, setAll] = useState(false);
  if (running.length === 0) return null;
  const shown = all ? running : running.slice(0, RUNNING_ROWS_SHOWN);
  const rest = running.length - RUNNING_ROWS_SHOWN;
  return (
    <div className="wbg">
      <div className="wsec">Trabajos en segundo plano<span className="wsec-h">{running.length} corriendo</span></div>
      {shown.map((item) => <JobRow key={item.id} chat={chat} item={item} nowMs={nowMs} context={context} />)}
      {rest > 0 && (
        <button type="button" className="wmore" aria-expanded={all} onClick={() => setAll((open) => !open)}>
          {all ? 'Mostrar menos' : `+${rest} más corriendo`}
        </button>
      )}
    </div>
  );
}

// Always-on services (role "service"): supervised like any job but not work, so
// they are not counted above and sit in ONE closed line. Open, the same rows as
// any job (output, stop). Opening ticks its own clock; closed, nothing ticks.
export function ServiceJobs({ chat, services, context }: { chat: Chat; services: readonly SpawnedWorkItem[]; context: MachineContext }) {
  const [open, setOpen] = useState(false);
  const [nowMs, setNowMs] = useState(Date.now());
  useEffect(() => {
    if (!open) return;
    setNowMs(Date.now());
    return subscribeSecond(() => setNowMs(Date.now()));
  }, [open]);
  if (services.length === 0) return null;
  return (
    <div className={`wq wsv${open ? ' open' : ''}`}>
      <button type="button" className="wq-sum" aria-expanded={open} onClick={() => setOpen((value) => !value)}>
        <span className="wq-chev" aria-hidden="true">{open ? <IcChevDown /> : <IcChevron />}</span>
        <span className="wq-lead">Servicios · {services.length}</span>
        <span className="wq-nx">{open ? 'siempre activos' : 'siempre activos, supervisados'}</span>
      </button>
      {open && services.map((item) => <JobRow key={item.id} chat={chat} item={item} nowMs={nowMs} context={context} />)}
    </div>
  );
}

function QueueRow({ chat, item, position, nowMs }: { chat: Chat; item: SpawnedWorkItem; position: number; nowMs: number }) {
  const { canStop, stopping, stop } = useWorkStop(chat, item);
  const label = workLabel(item);
  const waited = fmtWait(workWaitMs(item, nowMs));
  return (
    <div className="wq-row" data-work-id={item.id}>
      <span className="wq-p">{position}</span>
      <span className="wq-t" title={item.resource ? `${label} · espera ${item.resource}` : label}>{label}</span>
      {waited && <span className="wq-w">espera {waited}</span>}
      {canStop && (
        <span className="wq-acts">
          <button type="button" className="wbtn stop wq-stop" aria-label={`Detener ${label}`} disabled={stopping} onClick={() => void stop()}>
            {stopping ? 'Deteniendo…' : 'Detener'}
          </button>
        </span>
      )}
    </div>
  );
}

function NextNames({ names }: { names: readonly string[] }) {
  return (
    <>
      {names.map((name, index) => (
        <span key={index}>
          {index > 0 && (index === names.length - 1 ? ' y ' : ', ')}
          <b>{name}</b>
        </span>
      ))}
    </>
  );
}

// The queue: ONE row while closed — how many wait and what runs next on each
// machine (mock rail-v6). Open, it lists every waiting job in order, grouped by
// machine, in a box of its own fixed height that scrolls by itself, so the
// pane around it never grows a scroll bar. Open or closed is kept per chat.
export function WorkQueue({ chat, queued, nowMs, context }: { chat: Chat; queued: readonly SpawnedWorkItem[]; nowMs: number; context: MachineContext }) {
  const [open, setOpen] = useState(() => railQueueOpen(chat.id));
  if (queued.length === 0) return null;
  const groups = queueGroups(queued, context);
  const next = nextInQueue(groups);
  const toggle = () => {
    const value = !open;
    setRailQueueOpen(chat.id, value);
    setOpen(value);
  };
  return (
    <div className={`wq${open ? ' open' : ''}`}>
      <button type="button" className="wq-sum" aria-expanded={open} onClick={toggle}>
        <span className="wq-chev" aria-hidden="true">{open ? <IcChevDown /> : <IcChevron />}</span>
        <span className="wq-lead">En espera · {queued.length}</span>
        {open
          ? <span className="wq-nx">en orden, por máquina</span>
          : next.length > 0 && <span className="wq-nx" title={`sigue ${next.join(' y ')}`}>sigue <NextNames names={next} /></span>}
      </button>
      {open && (
        <div className="wq-list">
          {groups.map((group) => (
            <div key={group.machine.key || 'local'} className="wq-g">
              <div className="wq-mh"><MachineGlyph local={group.machine.local} /><span className="wq-mn" title={group.machine.name}>{group.machine.name}</span> · {group.items.length}</div>
              {group.items.map((item, index) => <QueueRow key={item.id} chat={chat} item={item} position={index + 1} nowMs={nowMs} />)}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// The work one agent started, under that agent's row. RUNNING items are full
// live rows (output, stop) rendered as a sibling of the agent's <details>, so
// they show without opening the agent. Finished ones go INSIDE the fold
// (SpawnedWorkDoneKids): Claude Code reports every command an agent runs as a
// task, so a long agent left hundreds of flat lines outside its row that no
// click could collapse (user, 2026-10-26).
export function SpawnedWorkKids({ chat, items, nowMs }: { chat: Chat; items: readonly SpawnedWorkItem[] | undefined; nowMs: number }) {
  const running = items?.filter((item) => item.status === 'running') ?? [];
  if (running.length === 0) return null;
  return (
    <div className="r-sa-kids">
      {running.map((item) => <RunningRow key={item.id} chat={chat} item={item} nowMs={nowMs} />)}
    </div>
  );
}

// The agent's finished work, inside its fold. A task whose launching call is
// already one of the agent's listed calls (a plain command) is that call:
// drawn once, as the call.
export function SpawnedWorkDoneKids({ items, nowMs, calls }: { items: readonly SpawnedWorkItem[] | undefined; nowMs: number; calls?: readonly ToolEvent[] }) {
  const listed = new Set((calls ?? []).map((call) => call.id).filter(Boolean));
  const finished = items?.filter((item) => item.status !== 'running' && !(item.toolCallId && listed.has(item.toolCallId))) ?? [];
  if (finished.length === 0) return null;
  return (
    <div className="r-sa-kids">
      {finished.map((item) => (
        <div key={item.id} className="r-dline" data-status={item.status}>
          <span className="r-dt" title={item.label || item.taskId}>{item.label || item.taskId}</span>
          {itemDuration(item, nowMs) && <span className="r-dl">{itemDuration(item, nowMs)}</span>}
        </div>
      ))}
    </div>
  );
}

// FINISHED spawned work — the pane's footer line «Terminados hoy · N», which
// opens the flat history in place: title + duration rows, and the shared
// subagent row for an agent. Work finished on an earlier day folds once more,
// so nothing is out of reach and nothing old reads as today's. NO failure
// wording and no red anywhere here: the row says what ran, not how it ended
// (user, 2026-07-25) — same rule the subagent rows follow.
export function FinishedWork({ chat }: { chat: Chat }) {
  useSpawnedWork();
  const currentNodes = extractSubagents(currentTurnMessages(chat).flatMap((message) => message.events)
    .filter((event): event is ToolEvent => event.kind === 'tool')).nodes;
  const represented = new Set(currentNodes.map((node) => node.id));
  const work = store.spawnedWork(chat);
  const { byParent, nested } = nestSpawnedWork(work);
  for (const item of work) if (isAgentWork(item) && item.status === 'running') represented.add(subagentWorkId(item));
  const finished = work.filter((item) => item.status !== 'running' && !nested.has(item.id)
    && (!isAgentWork(item) || !represented.has(subagentWorkId(item))));
  const [historyOpen, setHistoryOpen] = useState(false);
  const historicalNodes = new Map((historyOpen ? extractSubagents(chat.messages.flatMap((message) => message.events)
    .filter((event): event is ToolEvent => event.kind === 'tool')).nodes : []).map((node) => [node.id, node]));

  useEffect(() => { void store.refreshSpawnedWork(chat); }, [chat]);
  // The history shows older finished work whole, with the calls of the
  // subagents of loaded turns whose calls came as a summary.
  useEffect(() => {
    if (!historyOpen) return;
    store.wantFullSpawnedWork(chat);
    for (const message of chat.messages) if ((message.workSummary?.subagentCalls ?? 0) > 0) void store.loadWorkGroup(chat.id, message);
  }, [historyOpen, chat]);

  if (finished.length === 0) return null;
  const nowMs = Date.now();
  const today = finished.filter((item) => finishedToday(item, nowMs));
  const earlier = finished.filter((item) => !finishedToday(item, nowMs));
  const row = (item: SpawnedWorkItem) => isAgentWork(item)
    ? (() => {
      const n = nodeForSpawnedWork(item, historicalNodes.get(subagentWorkId(item)));
      return <SubagentRow key={item.id} chat={chat} n={n} nowMs={nowMs}
        kids={<SpawnedWorkKids chat={chat} items={byParent.get(item.taskId)} nowMs={nowMs} />}
        doneKids={<SpawnedWorkDoneKids items={byParent.get(item.taskId)} nowMs={nowMs} calls={n.calls} />} />;
    })()
    : (
      <div key={item.id} className="r-dline" data-status={item.status}>
        <span className="r-dt" title={item.label || item.taskId}>{item.label || item.taskId}</span>
        {itemDuration(item, nowMs) && <span className="r-dl">{itemDuration(item, nowMs)}</span>}
      </div>
    );
  return (
    <details className="wdone" onToggle={(event) => { if (event.target === event.currentTarget) setHistoryOpen(event.currentTarget.open); }}>
      <summary>
        <span className="wdone-ic" aria-hidden="true"><IcCheck /></span>
        <span className="wdone-l">Terminados hoy</span>
        <span className="wdone-n">{today.length}</span>
      </summary>
      <div className="wdone-b">
        {today.map(row)}
        {earlier.length > 0 && (
          <details className="wdone-old">
            <summary>{earlier.length} {earlier.length === 1 ? 'anterior' : 'anteriores'}</summary>
            {earlier.map(row)}
          </details>
        )}
      </div>
    </details>
  );
}
