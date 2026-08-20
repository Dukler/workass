import type { Chat } from '../store/types';
import { store, useApp } from '../store/store';
import { chatPane } from '../store/right-pane';
import { IcExpand, IcClose } from '../icons';
import { TareasCard } from './TareasCard';
import { BrowserPanel } from './BrowserPanel';
import { localBrowserOwnsChat } from '../browser';
import { SpawnedWorkCard } from './SpawnedWorkCard';
import { EnvCard } from './EnvCard';

export function RightRail({ chat }: { chat: Chat | null }) {
  const app = useApp();
  const pane = chatPane(chat);

  // A closed per-chat pane owns no hidden work. Keeping the rail mounted behind
  // CSS left its activity subscription, tool sorting and one-second timers live
  // while the user could not see any of it.
  if (!pane) return null;

  // Browser mode (own titlebar toggle, 2026-07-12): the right column becomes the
  // live browser — its own frame, no rail chrome. Electron-only; a client without
  // the browser bridge can't turn this on (the toggle is hidden there). Per-chat,
  // so it only shows for the chat that actually opened a browser.
  if (pane === 'browser') {
    if (!app.hasBrowserChannels || !chat || !localBrowserOwnsChat(chat.id, chat.machineId)) return null;
    return (
      <aside className="rail browser-col">
        <BrowserPanel
          chatId={chat.id}
          conversationId={chat.chatId}
          machineId={chat.machineId}
          onClose={() => store.toggleBrowser()}
        />
      </aside>
    );
  }

  // Turno-first, de-carded (mock b3, 2026-07-22): the rail is one flush column.
  // The live turn (intent + dots + live call + subagents) is the hero at the top;
  // Archivos and Segundo plano are flush count rows beneath, each opening on tap.
  return (
    <aside className="rail">
      <div className="railhead">
        <button className="tico" title="Expandir panel" onClick={() => store.toggleRailWide()}><IcExpand /></button>
        <button className="tico" title="Cerrar panel" onClick={() => store.closeRail()}><IcClose /></button>
      </div>

      <TareasCard />
      {chat && <EnvCard chat={chat} />}
      {chat && <SpawnedWorkCard chat={chat} />}
    </aside>
  );
}
