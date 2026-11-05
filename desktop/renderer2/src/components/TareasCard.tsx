import { useEffect, useRef, useState } from 'react';
import type { Chat, ToolEvent } from '../store/types';
import { store, useApp, useActivity, useSpawnedWork } from '../store/store';
import { extractSubagents, nodeState } from './messages';
import { currentTurnMessages, nestSpawnedWork, reconcileSubagentWork, type SubagentNode } from '../subagent-layout';
import { backgroundWork, loadSegments, machineLoads, workTotals, type MachineContext } from '../rail-work';
import { SubagentRow } from './SubagentRow';
import { BackgroundJobs, FinishedWork, MachineGlyph, SpawnedWorkDoneKids, SpawnedWorkKids, WorkQueue } from './SpawnedWorkCard';

// Everything the Trabajos tab draws, and its count: the turn's subagents (the
// shared subagent rows), the chat's running background work and what waits.
export function trabajosModel(chat: Chat | null) {
  const tools = currentTurnMessages(chat).flatMap((message) => message.events)
    .filter((event): event is ToolEvent => event.kind === 'tool');
  const grouped = extractSubagents(tools);
  // Tracked child lifetime survives foreground settlement. While the child is
  // live its spawned-work row owns activity and elapsed time; after completion
  // the grouped tool row uses the durable child result.
  const work = chat ? store.spawnedWork(chat) : [];
  const { nodes } = reconcileSubagentWork(grouped.nodes, work);
  const { byParent } = nestSpawnedWork(work);
  const runningNodes = nodes.filter((n) => nodeState(n) === 'running');
  // A cancelled child leaves the rail; its card stays in the conversation.
  const doneNodes = nodes.filter((n) => nodeState(n) !== 'running' && nodeState(n) !== 'cancelled');
  const { running, queued } = backgroundWork(work);
  return { runningNodes, doneNodes, byParent, running, queued, count: runningNodes.length + running.length + queued.length };
}

export function machineContextFor(chat: Chat | null): MachineContext {
  return { chatMachineId: chat?.machineId, localMachineId: store.localMachineId(), names: store.machineNames() };
}

// «Trabajos» — the activity pane's first tab (approved mock rail-v7,
// 2026-11-05): totals and per-machine load, the turn's subagents, the running
// background work, the queue, and what finished today. The live turn itself
// (intent, plan, current call, calls) is the chat's to show, not this tab's.
export function TareasCard() {
  const app = useApp();
  useActivity(); // re-render on live tool events (subagent calls)
  useSpawnedWork(); // …and when spawned work appears/settles
  const [nowMs, setNowMs] = useState(Date.now());
  const cardRef = useRef<HTMLElement>(null);

  const chat = store.active();
  const { runningNodes, doneNodes, byParent, running, queued } = trabajosModel(chat);
  const context = machineContextFor(chat);
  const kidsOf = (n: SubagentNode) => chat && n.work ? <SpawnedWorkKids chat={chat} items={byParent.get(n.work.taskId)} nowMs={nowMs} /> : null;
  const doneKidsOf = (n: SubagentNode) => n.work ? <SpawnedWorkDoneKids items={byParent.get(n.work.taskId)} nowMs={nowMs} calls={n.calls} /> : null;
  const live = runningNodes.length + running.length + queued.length > 0;
  const totals = workTotals(runningNodes.length, running.length, queued.length);
  const loads = machineLoads(running, queued, context);
  const empty = runningNodes.length + doneNodes.length + running.length + queued.length === 0;

  useEffect(() => {
    if (!live) return;
    const iv = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(iv);
  }, [live]);
  useEffect(() => {
    if (app.flashTareas) cardRef.current?.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  }, [app.flashTareas]);

  return (
    <section className={`rp-work${app.flashTareas ? ' flash' : ''}`} ref={cardRef}>
      {empty && <div className="rp-empty">Nada corriendo</div>}
      {totals.length > 0 && (
        <div className="wtotals">
          {totals.map((part, index) => (
            <span key={part.word} className="wtotal">
              {index > 0 && <span className="wtotal-sep" aria-hidden="true">·</span>}
              <b>{part.count}</b> {part.word}
            </span>
          ))}
        </div>
      )}
      {loads.length > 0 && (
        <div className="wload">
          {loads.map((load) => {
            const segments = loadSegments(load.running, load.waiting);
            return (
              <div key={load.machine.key || 'local'} className="wload-r" data-local={load.machine.local ? '' : undefined}>
                <span className="wload-who" title={load.machine.name}><MachineGlyph local={load.machine.local} /><span className="wload-nm">{load.machine.name}</span></span>
                <span className="wseg" aria-hidden="true">
                  {Array.from({ length: segments.running }, (_, i) => <i key={`r${i}`} className="r" />)}
                  {Array.from({ length: segments.waiting }, (_, i) => <i key={`w${i}`} />)}
                </span>
                <span className="wload-n" aria-label={`${load.running} corriendo, ${load.waiting} en espera`}>{load.running} + {load.waiting}</span>
              </div>
            );
          })}
        </div>
      )}
      {/* EVERY subagent of this turn is one flat list — a finished one is not
          worth a fold inside a fold inside a row (user, 2026-07-25). Running
          first, then settled; each still opens to its own calls. */}
      {(runningNodes.length > 0 || doneNodes.length > 0) && (
        <>
          <div className="wsec">Subagentes{runningNodes.length > 0 && <span className="wsec-h">{runningNodes.length} trabajando</span>}</div>
          <div className="r-subs">
            {runningNodes.map((n) => <SubagentRow key={n.id} n={n} nowMs={nowMs} chat={chat} kids={kidsOf(n)} doneKids={doneKidsOf(n)} />)}
            {doneNodes.map((n) => <SubagentRow key={n.id} n={n} nowMs={nowMs} chat={chat} kids={kidsOf(n)} doneKids={doneKidsOf(n)} />)}
          </div>
        </>
      )}
      {chat && <BackgroundJobs chat={chat} running={running} nowMs={nowMs} context={context} />}
      {chat && <WorkQueue chat={chat} queued={queued} nowMs={nowMs} context={context} />}
      {chat && <FinishedWork chat={chat} />}
    </section>
  );
}
