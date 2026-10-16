// Subsessions nested under their parent in SidebarV2 (mock
// desktop/docs/mocks/forks/sidebar-nested.html, as amended by the user on
// 2026-10-16): the parent's title line carries a neutral count chip + chevron;
// expanded, one quiet indent level holds the single-line subsession rows —
// the threads its agents spawned AND the forked ones, the same line for both
// ("un hijo es un hijo": no fork glyph is allowed on any parent-side row).
// The ⑂ fork icon lives only inside the child's own view.

import { useEffect, type KeyboardEvent } from 'react';
import type { Chat } from '../store/types';
import { useClock, store } from '../store/store';
import { forkStore } from '../store/forks';
import { IcActAgent, IcChevron, ModelIcon } from '../icons';
import { isAgentWork } from '../subagent-layout';
import { chatPane } from '../store/right-pane';
import { forkChildLabel } from '../fork-format';
import {
  forkFlatKids, forkGroups, forkPage, forkSubsessionRow, formatForkShortDuration,
  mergeSubsessionRows, spawnSubsessionRow, type ForkKidRow, type SubsessionRow,
} from '../fork-sidebar';

/** The parent row's disclosure: a neutral children glyph, the count, the sidebar's own chevron. */
export function ForkKidsToggle({ count, open, onToggle }: { count: number; open: boolean; onToggle: () => void }) {
  return (
    <button
      type="button"
      className={`sv2-kids${open ? ' open' : ''}`}
      title={open ? 'Ocultar subsesiones' : 'Mostrar subsesiones'}
      aria-label={`${open ? 'Ocultar' : 'Mostrar'} ${count} ${count === 1 ? 'subsesión' : 'subsesiones'}`}
      aria-expanded={open}
      draggable={false}
      onClick={(event) => { event.stopPropagation(); onToggle(); }}
      onDoubleClick={(event) => event.stopPropagation()}
    >
      <span className="kg" aria-hidden="true"><IcActAgent /></span>
      {count > 0 && <span className="kn">{count}</span>}
      <span className="kc" aria-hidden="true"><IcChevron /></span>
    </button>
  );
}

// Only a running row ticks, and only its own time span repaints.
function LiveWall({ startedMs }: { startedMs: number }) {
  useClock();
  if (!startedMs) return null;
  return <span className="kt">{formatForkShortDuration(Math.max(0, Date.now() - startedMs))}</span>;
}

function KidDot({ state }: { state: string }) {
  if (state === 'running') return <i className="fk-pulse" aria-label="En curso" />;
  if (state === 'queued') return <i className="fk-ring" aria-label="En cola" />;
  return <i className="fk-dot" aria-hidden="true" />;
}

function KidRow({ chat, chatId, row }: { chat: Chat; chatId: string; row: SubsessionRow }) {
  const selected = !!(row.fork && forkStore.isSelected(chat.id, row.fork.runId, row.fork.rootId));
  const open = () => {
    if (row.fork) {
      forkStore.select({ tabId: chat.id, chatId, runId: row.fork.runId, rootId: row.fork.rootId });
      return;
    }
    // A spawned thread has no separate view of its own: its live row lives in
    // the right rail (TareasCard stamps data-work-id anchors), so this brings
    // it forward — open the column when the user had it closed, then look for
    // the row two frames out (chat switch and pane opening both land on a
    // task). No fork styling anywhere on this path (user 2026-10-16).
    if (store.state.activeId !== chat.id) store.switchChat(chat.id);
    if (chatPane(chat) === null && typeof document !== 'undefined') store.toggleRail();
    if (typeof document === 'undefined') return;
    requestAnimationFrame(() => requestAnimationFrame(() => {
      const id = row.key.slice('spawn:'.length);
      document.querySelector(`[data-work-id="${CSS.escape(id)}"]`)?.scrollIntoView({ block: 'nearest' });
    }));
  };
  const onKey = (event: KeyboardEvent) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    open();
  };
  return (
    <div
      className={`sv2-kid${selected ? ' on' : ''}`} data-st={row.state} role="button" tabIndex={0}
      aria-current={selected ? 'true' : undefined} title={row.label}
      data-sv2-kid={row.key}
      onClick={open} onKeyDown={onKey}
    >
      <span className="ks"><KidDot state={row.state} /></span>
      <span className="kl">{row.label}</span>
      {row.word && <span className="kf">{row.word}</span>}
      {row.brand && <span className="kmi" aria-hidden="true"><ModelIcon provider={row.brand} /></span>}
      {row.state === 'running' ? <LiveWall startedMs={row.startedMs} /> : row.wallMs ? <span className="kt">{formatForkShortDuration(row.wallMs)}</span> : null}
    </div>
  );
}

/** The expanded nest under a parent row. Fetches the full fork list on open. */
export function ForkNest({ chat }: { chat: Chat }) {
  const chatId = chat.chatId ?? '';
  useEffect(() => { forkStore.ensure({ tabId: chat.id, chatId }); }, [chat.id, chatId]);
  const nowMs = Date.now();
  const forkRows = forkFlatKids(forkGroups(forkStore.chat(chat.id)))
    .map((kid: ForkKidRow & { point: { providerId: string } }) => forkSubsessionRow(
      kid, { label: forkChildLabel(kid.child), brand: store.providerBrand(kid.point.providerId, chat) }, nowMs,
    ));
  // One undifferentiated family under the parent: spawned agent threads ride
  // beside the forked ones, the same single line for both (user 2026-10-16).
  const rows = mergeSubsessionRows(
    forkRows,
    store.spawnedWork(chat).filter(isAgentWork).map((item) => spawnSubsessionRow(item)),
  );
  if (!rows.length) return null;
  const { visible, hidden } = forkPage(rows, forkStore.allShown(chat.id));
  return (
    <div className="sv2-nest" role="group" aria-label={`Subsesiones de ${chat.title}`}>
      {visible.map((row) => <KidRow key={row.key} chat={chat} chatId={chatId} row={row} />)}
      {hidden > 0 && (
        <button type="button" className="sv2-more" onClick={() => forkStore.showAll(chat.id)}>Mostrar {hidden} más</button>
      )}
    </div>
  );
}
