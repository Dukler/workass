import { useEffect, useState } from 'react';
import type { Chat } from '../store/types';
import type { SpawnedWorkItem } from '../wire/types';
import { store, useSpawnedWork } from '../store/store';
import { fmtDur } from './messages';
import { ActionGlyph, IcStopSquare, IcTerminal, ModelIcon } from '../icons';
import { spawnedWorkActivity, spawnedWorkKindWord } from '../tool-names';
import { displayDetail } from '../tool-display';

function itemDuration(item: SpawnedWorkItem, nowMs: number): string {
  const start = Date.parse(item.startedAt);
  const end = item.finishedAt ? Date.parse(item.finishedAt) : nowMs;
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

// One RUNNING spawned process: an inline flat row, always visible — the same
// family as the rail's live call and subagent rows. Title + elapsed, a mono
// meta line, the daemon's live summary, and a per-row output toggle.
function RunningRow({ chat, item, nowMs }: { chat: Chat; item: SpawnedWorkItem; nowMs: number }) {
  const [tailOpen, setTailOpen] = useState(false);
  const [tail, setTail] = useState('');
  const [tailLimited, setTailLimited] = useState(false);
  const [tailError, setTailError] = useState('');
  const [stopping, setStopping] = useState(false);

  useEffect(() => {
    if (!tailOpen || !item.outputFile) return;
    let alive = true;
    const refresh = async () => {
      const result = await store.readSpawnedWork(chat, item.id);
      if (!alive || !result) return;
      if (!result.ok) {
        setTailError(result.error || 'Salida no disponible.');
        return;
      }
      setTail(result.tail || '');
      setTailLimited(!!result.tailLimited);
      setTailError('');
    };
    void refresh();
    const timer = setInterval(refresh, 2000);
    return () => { alive = false; clearInterval(timer); };
  }, [chat, item.id, item.outputFile, tailOpen]);

  const meta = [
    spawnedWorkKindWord(item.kind),
    item.kind === 'agent' ? item.providerId : '',
    item.pid ? `pid ${item.pid}` : '',
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
  // Ending the row is the one thing this rail could show forever without ever
  // offering: a lane whose process died without a done-file reads "running"
  // until someone settles it, and a dev server nobody needs any more keeps its
  // chat's rail busy. The daemon does both; this is the button.
  const canStop = store.canStopSpawnedWork();
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
  return (
    <div className="bgr" data-kind={item.kind}>
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
          <button type="button" className="bgr-out" onClick={() => setTailOpen((open) => !open)}>
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
        <span className="r-pulse" aria-label="En curso" />
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

// RUNNING spawned work — inline rows in the rail's live zone (approved mock
// f-bg2, 2026-07-23): visible without opening anything, several stack, each
// stays compact. Rendered by TareasCard next to the live call and subagents;
// never behind a fold. Work here outlives one turn (daemon spawned-work
// registry), so it renders whether or not a turn is streaming.
export function SpawnedWorkLive({ chat }: { chat: Chat }) {
  useSpawnedWork();
  const running = store.spawnedWork(chat).filter((item) => item.status === 'running');
  const [nowMs, setNowMs] = useState(Date.now());

  useEffect(() => {
    if (running.length === 0) return;
    const timer = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [running.length]);

  if (running.length === 0) return null;
  return (
    <div className="bglive">
      {running.map((item) => <RunningRow key={item.id} chat={chat} item={item} nowMs={nowMs} />)}
    </div>
  );
}

// FINISHED spawned work — ONE "Segundo plano" fold of flat lines (approved mock
// f-bg2): a single tap opens title + duration rows. The old fold-inside-a-fold
// ("N trabajos terminados" inside "Segundo plano") is gone, and running work
// never renders here — it lives inline via SpawnedWorkLive. NO failure wording
// and no red anywhere in this card: the row says what ran, not how it ended
// (user, 2026-07-25) — same rule the subagent rows follow.
export function SpawnedWorkCard({ chat }: { chat: Chat }) {
  useSpawnedWork();
  const finished = store.spawnedWork(chat).filter((item) => item.status !== 'running');

  useEffect(() => { void store.refreshSpawnedWork(chat); }, [chat]);

  if (finished.length === 0) return null;
  const nowMs = Date.now();
  return (
    <details className="r-meta">
      <summary>
        <span className="r-ml">Segundo plano</span>
        <span className="r-count">{finished.length}</span>
      </summary>
      <div className="r-meta-b">
        {finished.map((item) => (
          <div key={item.id} className="r-dline" data-status={item.status}>
            <span className="r-dt" title={item.label || item.taskId}>{item.label || item.taskId}</span>
            {itemDuration(item, nowMs) && <span className="r-dl">{itemDuration(item, nowMs)}</span>}
          </div>
        ))}
      </div>
    </details>
  );
}
