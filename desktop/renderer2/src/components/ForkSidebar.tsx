// Fork children as subsessions nested under their parent in SidebarV2
// (approved mock desktop/docs/mocks/forks/sidebar-nested.html): the parent's
// title line carries "⑂ N" + chevron; expanded, one quiet indent level holds
// the single-line child rows directly (no per-fork-point header).

import { useEffect, type KeyboardEvent } from 'react';
import type { Chat } from '../store/types';
import type { ForkChildView } from '../wire/types';
import { useClock } from '../store/store';
import { forkStore } from '../store/forks';
import { IcChevron, IcFork, ModelIcon } from '../icons';
import { store } from '../store/store';
import { forkChildElapsedMs, forkChildLabel } from '../fork-format';
import {
  forkFlatKids, forkGroups, forkKidMeta, forkPage, formatForkShortDuration, type ForkKidRow,
} from '../fork-sidebar';

/** The parent row's disclosure: fork glyph, child count, the sidebar's own chevron. */
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
      <span className="kg" aria-hidden="true"><IcFork /></span>
      {count > 0 && <span className="kn">{count}</span>}
      <span className="kc" aria-hidden="true"><IcChevron /></span>
    </button>
  );
}

// Only a running row ticks, and only its own time span repaints.
function LiveTime({ child }: { child: ForkChildView }) {
  useClock();
  return <span className="kt">{formatForkShortDuration(forkChildElapsedMs(child, Date.now()))}</span>;
}

function KidDot({ state }: { state: string }) {
  if (state === 'running') return <i className="fk-pulse" aria-label="En curso" />;
  if (state === 'queued') return <i className="fk-ring" aria-label="En cola" />;
  return <i className="fk-dot" aria-hidden="true" />;
}

function KidRow({ chat, chatId, kid, brand }: { chat: Chat; chatId: string; kid: ForkKidRow; brand: string }) {
  const label = forkChildLabel(kid.child);
  const meta = forkKidMeta(kid.child, Date.now());
  const selected = forkStore.isSelected(chat.id, kid.runId, kid.rootId);
  const open = () => forkStore.select({ tabId: chat.id, chatId, runId: kid.runId, rootId: kid.rootId });
  const onKey = (event: KeyboardEvent) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    open();
  };
  return (
    <div
      className={`sv2-kid${selected ? ' on' : ''}`} data-st={meta.state} role="button" tabIndex={0}
      aria-current={selected ? 'true' : undefined} title={label}
      data-sv2-kid={`${kid.runId}:${kid.rootId}`}
      onClick={open} onKeyDown={onKey}
    >
      <span className="ks"><KidDot state={meta.state} /></span>
      <span className="kl">{label}</span>
      {meta.word && <span className="kf">{meta.word}</span>}
      {brand && <span className="kmi" aria-hidden="true"><ModelIcon provider={brand} /></span>}
      {meta.state === 'running' ? <LiveTime child={kid.child} /> : meta.time ? <span className="kt">{meta.time}</span> : null}
    </div>
  );
}

/** The expanded nest under a parent row. Fetches the chat's full fork list on open. */
export function ForkNest({ chat }: { chat: Chat }) {
  const chatId = chat.chatId ?? '';
  useEffect(() => { forkStore.ensure({ tabId: chat.id, chatId }); }, [chat.id, chatId]);
  const kids = forkFlatKids(forkGroups(forkStore.chat(chat.id)));
  if (!kids.length) return null;
  const { visible, hidden } = forkPage(kids, forkStore.allShown(chat.id));
  return (
    <div className="sv2-nest" role="group" aria-label={`Subsesiones de ${chat.title}`}>
      {visible.map((kid) => (
        // Every child runs on its fork point's provider.
        <KidRow key={`${kid.runId}:${kid.rootId}`} chat={chat} chatId={chatId} kid={kid} brand={store.providerBrand(kid.point.providerId, chat)} />
      ))}
      {hidden > 0 && (
        <button type="button" className="sv2-more" onClick={() => forkStore.showAll(chat.id)}>Mostrar {hidden} más</button>
      )}
    </div>
  );
}
