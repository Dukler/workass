import { useAutomations } from '../automations';
import { useEffect, useState } from 'react';
import type { Chat } from '../store/types';
import { store, useApp, useActivity, useSpawnedWork } from '../store/store';
import { chatPane, PANE_SLIDE_MS } from '../store/right-pane';
import { IcExpand, IcClose } from '../icons';
import { TareasCard, trabajosModel } from './TareasCard';
import { BrowserPanel } from './BrowserPanel';
import { localBrowserOwnsChat } from '../browser';
import { CambiosTab, cambiosCount } from './CambiosTab';
import { useChangesState } from '../changes-store';
import { SalidasTab } from './SalidasTab';
import { chatOutputs } from '../chat-outputs';
import { RAIL_TABS, railTab, setRailTab, type RailTab } from '../rail-view';

export function RightRail({ chat }: { chat: Chat | null }) {
  const app = useApp();
  const pane = chatPane(chat);
  // Closing the rail slides its column shut (App: slidePane), so the rail
  // stays drawn while it slides out instead of leaving an empty band. Decided
  // while rendering, so the rail is never unmounted and mounted again in the
  // slide's first frame (its tab, scroll and subscriptions all restarted).
  // This component is keyed per chat, so a chat switch never holds it.
  const [shownPane, setShownPane] = useState(pane);
  const [sliding, setSliding] = useState(false);
  if (shownPane !== pane) {
    setShownPane(pane);
    setSliding(shownPane === 'rail' && pane === null);
  }
  useEffect(() => {
    if (!sliding) return;
    const timer = setTimeout(() => setSliding(false), PANE_SLIDE_MS);
    return () => clearTimeout(timer);
  }, [sliding]);

  // A closed per-chat pane owns no hidden work. Keeping the rail mounted behind
  // CSS left its activity subscription, tool sorting and one-second timers live
  // while the user could not see any of it.
  if (!pane && !sliding) return null;

  // Browser mode (own titlebar toggle, 2026-07-12): the right column becomes the
  // live browser — its own frame, no rail chrome. Electron-only; a client without
  // the browser bridge can't turn this on (the toggle is hidden there). Per-chat,
  // so it only shows for the chat that actually opened a browser.
  if (pane === 'browser') {
    if (!app.hasBrowserChannels || !chat || !localBrowserOwnsChat(chat.id, chat.machineId)) return null;
    return (
      <aside className="rail browser-col">
        <BrowserPanel
          chatId={store.browserPageFor(chat.id)}
          ownerChatId={chat.id}
          conversationId={chat.chatId}
          machineId={chat.machineId}
          artifactOrigin={store.browserArtifactMachineId(chat.machineId)}
          onClose={() => store.toggleBrowser()}
        />
      </aside>
    );
  }

  return <ActivityPane chat={chat} />;
}

const TAB_LABEL: Record<RailTab, string> = { trabajos: 'Trabajos', cambios: 'Cambios', salidas: 'Salidas' };

// «Actividad» (approved mock rail-v7, 2026-11-05): one flat column against the
// chat, three text tabs. Trabajos is what runs and waits, Cambios what the
// owner's chats changed in their repos (mock rail-cambios-v2), Salidas what the
// chat produced to open. The live turn's intent,
// plan, current call and calls are no longer drawn here (owner, 2026-11-05).
// The open tab is kept per chat.
function ActivityPane({ chat }: { chat: Chat | null }) {
  const app = useApp();
  useActivity();
  useSpawnedWork();
  // Programado (Trabajos) reads what is scheduled while the pane is open.
  useAutomations();
  // Cambios's count is the last snapshot's: the daemon only looks while the
  // tab is open, so it is not refreshed for the other tabs.
  const changes = useChangesState();
  const chatKey = chat?.id ?? '';
  const [tab, setTab] = useState<RailTab>(() => railTab(chatKey));
  const choose = (next: RailTab) => {
    setRailTab(chatKey, next);
    setTab(next);
  };

  // The titlebar's background-work chip reveals this pane at Trabajos.
  useEffect(() => { if (app.flashTareas) choose('trabajos'); }, [app.flashTareas]); // eslint-disable-line react-hooks/exhaustive-deps

  const outputs = chatOutputs(chat?.messages);
  const counts: Record<RailTab, number> = {
    trabajos: trabajosModel(chat).count,
    cambios: cambiosCount(changes, chat?.chatId ?? ''),
    salidas: outputs.length,
  };

  return (
    <aside className="rail">
      <div className="railhead">
        <span className="rp-title">Actividad</span>
        <button className="tico" title="Expandir panel" onClick={() => store.toggleRailWide()}><IcExpand /></button>
        <button className="tico" title="Cerrar panel" onClick={() => store.closeRail()}><IcClose /></button>
      </div>
      <div className="rp-tabs" role="tablist" aria-label="Actividad">
        {RAIL_TABS.map((key) => (
          <button key={key} type="button" role="tab" id={`rp-tab-${key}`} aria-selected={tab === key}
            className={`rp-tab${tab === key ? ' on' : ''}`} onClick={() => choose(key)}>
            {TAB_LABEL[key]}
            {/* A zero count is left out, like the zero parts of the totals line. */}
            {counts[key] > 0 && <span className="rp-tab-c">{counts[key]}</span>}
          </button>
        ))}
      </div>
      <div className="rp-body" role="tabpanel" aria-labelledby={`rp-tab-${tab}`} data-tab={tab}>
        {tab === 'trabajos' && <TareasCard />}
        {tab === 'cambios' && <CambiosTab chat={chat} />}
        {tab === 'salidas' && (chat ? <SalidasTab chat={chat} outputs={outputs} /> : <div className="rp-empty">Nada todavía</div>)}
      </div>
    </aside>
  );
}
