import { useState } from 'react';
import type { Chat } from '../store/types';
import { store } from '../store/store';
import { nodeState, nodeDuration, ToolDetail } from './messages';
import { subagentActivity, type SubagentNode } from '../subagent-layout';
import { spawnedWorkActivity } from '../tool-names';
import { ActionGlyph, IcStopSquare, ModelIcon } from '../icons';

// One subagent as a compact disclosure: model icon + label + elapsed on top, then
// model id + a human live activity. Long labels and model ids ellipsize; the
// elapsed is never pushed off. Exact calls stay one tap away in the body.
export function SubagentRow({ n, nowMs, chat }: { n: SubagentNode; nowMs: number; chat?: Chat | null }) {
  const st = nodeState(n);
  const running = st === 'running';
  const failed = st === 'failed';
  const dur = nodeDuration(n, nowMs);
  // The row says what it did, not how it ended: no outcome word, no red (user,
  // 2026-07-25). A settled subagent reads the same whatever its exit was.
  const activity = running ? (n.calls.some((call) => ['in_progress', 'pending', 'running'].includes(call.status ?? ''))
    ? subagentActivity(n) : (n.work ? spawnedWorkActivity(n.work) : null) ?? subagentActivity(n)) : null;
  const [stopping, setStopping] = useState(false);
  const canStop = running && n.work?.kind === 'subagent' && !!chat && store.canStopSpawnedWork();
  const stop = async () => {
    if (stopping || !canStop || !chat || !n.work) return;
    setStopping(true);
    try {
      const result = await store.stopSpawnedWork(chat, n.work.id);
      if (result && !result.ok) store.addToast('No se pudo detener', result.error || 'El daemon rechazó la parada.');
    } finally {
      setStopping(false);
    }
  };
  const ownership = n.work?.kind === 'agent' ? 'Subagente nativo · solo lectura'
    : n.work?.kind === 'subagent' ? 'Subagente de Workass' : undefined;
  const settled = `${n.calls.length} ${n.calls.length === 1 ? 'llamada' : 'llamadas'}`;
  return (
    <details className="r-sa" data-status={st}>
      <summary>
        {/* No chevron: the model icon is the row's anchor, and it reads bigger
            without one (user, 2026-07-24). Hover + open body carry the affordance. */}
        <span className="r-mi" data-p={n.provider ?? undefined} title={ownership}><ModelIcon provider={n.provider} /></span>
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
        {running && <span className="r-sa-controls">
          <span className="r-pulse" aria-label="En curso" />
          {canStop && <button type="button" className="bgr-stop" title={stopping ? 'Deteniendo…' : 'Detener'}
            aria-label={`Detener ${n.label}`} disabled={stopping}
            onClick={(event) => { event.preventDefault(); event.stopPropagation(); void stop(); }}>
            <IcStopSquare />
          </button>}
        </span>}
      </summary>
      {(n.calls.length > 0 || n.header?.output || n.work?.summary) && (
        <div className="r-sa-b">
          {n.calls.map((t) => <ToolDetail key={t.key} t={t} />)}
          {(n.header?.output || n.work?.summary) && <pre className="bgr-tail">{n.header?.output || n.work?.summary}</pre>}
        </div>
      )}
    </details>
  );
}
