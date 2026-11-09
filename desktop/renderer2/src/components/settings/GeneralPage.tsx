// Ajustes · General (Settings rework, mock .dev/mocks/settings-v2.html page 1):
// how a new chat starts and is named, what happens to a message written while
// an agent works, and when Workass tells you something.

import { store, useApp } from '../../store/store';
import { has } from '../../wire/api';
import { chatTitleAgents, resolveChatTitleChoice } from '../../chat-titles';

// How new chats are named. "Primer mensaje" is the old behavior; "Un agente"
// also sends the first message once to the chosen agent for a short title
// (daemon app-chat:auto-title). A rename typed by hand always wins. The agents
// and models listed are the ones this machine's providers offer for one-shot
// prompts.
function ChatTitlesGroup() {
  const app = useApp();
  if (!has('appChatAutoTitle') || !has('setSettings')) return null;
  const choice = app.chatTitles;
  const agents = chatTitleAgents(app.providers);
  const pick = resolveChatTitleChoice(choice, agents);
  const byAgent = choice.mode === 'agent';
  return (
    <>
      <div className="gtitle">Títulos de los chats</div>
      <div className="group">
        <div className="set">
          <div className="lbl">
            <div className="n">Nombrar chats nuevos</div>
            <div className="d">{!byAgent
              ? 'El título son las primeras palabras del primer mensaje.'
              : pick
                ? `${pick.agent.name} lee el primer mensaje una sola vez, sin herramientas ni proyecto, y le pone un título corto. Si lo renombrás vos, queda tu nombre.`
                : 'Ningún agente activo en esta computadora puede nombrar chats: se usa el primer mensaje.'}</div>
          </div>
          <div className="ctl"><div className="seg">
            <button className={!byAgent ? 'on' : ''} onClick={() => store.setChatTitles({ ...choice, mode: 'prompt' })}>Primer mensaje</button>
            <button className={byAgent ? 'on' : ''} disabled={!pick && !byAgent} title={!pick ? 'Ningún agente activo puede nombrar chats' : undefined}
              onClick={() => store.setChatTitles({ ...choice, mode: 'agent' })}>Un agente</button>
          </div></div>
        </div>
        {byAgent && pick && agents.length > 1 && (
          <div className="set">
            <div className="lbl"><div className="n">Agente</div><div className="d">Cuál lee el primer mensaje.</div></div>
            <div className="ctl"><div className="seg">
              {agents.map((a) => (
                <button key={a.providerId} className={pick.agent.providerId === a.providerId ? 'on' : ''}
                  onClick={() => store.setChatTitles({ ...choice, providerId: a.providerId, model: a.models[0].id })}>{a.name}</button>
              ))}
            </div></div>
          </div>
        )}
        {byAgent && pick && (
          <div className="set">
            <div className="lbl"><div className="n">Modelo</div><div className="d">{`${pick.agent.name} · ${pick.model.name}. El primero es el más rápido y barato.`}</div></div>
            <div className="ctl"><div className="seg">
              {pick.agent.models.map((m) => (
                <button key={m.id} className={pick.model.id === m.id ? 'on' : ''}
                  onClick={() => store.setChatTitles({ ...choice, providerId: pick.agent.providerId, model: m.id })}>{m.name}</button>
              ))}
            </div></div>
          </div>
        )}
      </div>
    </>
  );
}

export function GeneralPage() {
  const app = useApp();
  return (
    <section className="stgs-panel">
      <ChatTitlesGroup />
      <div className="gtitle">Avisos</div>
      <div className="group">
        <div className="set">
          <div className="lbl">
            <div className="n">Avisos de escritorio</div>
            <div className="d">{notifDetail(app.notifEnabled, app.notifPermission)}</div>
          </div>
          <div className="ctl">
            <button className={`toggle ${app.notifEnabled ? 'on' : ''}`} role="switch" aria-checked={app.notifEnabled}
              aria-label="Avisos de escritorio" onClick={() => void store.setNotifEnabled(!app.notifEnabled)}>
              <span className="knob" />
            </button>
          </div>
        </div>
      </div>
    </section>
  );
}

function notifDetail(enabled: boolean, perm: NotificationPermission | 'unsupported'): string {
  if (perm === 'unsupported') return 'Tu navegador no soporta notificaciones — se usarán avisos dentro de la app.';
  if (!enabled) return 'Desactivados. Activalos para recibir avisos cuando la pestaña esté en segundo plano.';
  if (perm === 'granted') return 'Activos. Se muestran como notificaciones del sistema.';
  if (perm === 'denied') return 'El navegador bloqueó los permisos — se usarán avisos dentro de la app.';
  return 'Activados. Concedé el permiso del navegador para verlos en el sistema.';
}
