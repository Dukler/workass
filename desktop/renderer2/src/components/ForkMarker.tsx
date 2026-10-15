// The parent transcript's ONE quiet line where it forked (approved mock
// sidebar-nested.html, surface 3): "⑂ Punto de bifurcación · name ——— 3 hijos ·
// 89 % caché". Accent on the glyph only. Clicking opens the parent's group in
// the sidebar and selects nothing; the children themselves never render here.

import type { Chat } from '../store/types';
import type { ForkPointView } from '../wire/types';
import { forkStore, useForks } from '../store/forks';
import { IcFork } from '../icons';
import { forkGroups, forkMarkerMeta } from '../fork-sidebar';

export function ForkMarker({ chat, point }: { chat: Chat; point: ForkPointView }) {
  useForks();
  const group = forkGroups(forkStore.chat(chat.id)).find((candidate) => candidate.point.forkPointId === point.forkPointId)
    ?? { point, runs: [], kids: [] };
  return (
    <button
      type="button" className="fkmark" data-fork-point={point.forkPointId}
      title="Mostrar los hijos en la barra lateral"
      onClick={() => forkStore.reveal({ tabId: chat.id, chatId: chat.chatId ?? point.chatId })}
    >
      <span className="fg" aria-hidden="true"><IcFork /></span>
      <span className="ft">Punto de bifurcación · <span className="fn">{point.name}</span></span>
      <span className="rule" aria-hidden="true" />
      <span className="fm">{forkMarkerMeta(group)}</span>
    </button>
  );
}
