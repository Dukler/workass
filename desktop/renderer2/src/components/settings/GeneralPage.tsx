// Ajustes · General (Settings rework, mock .dev/mocks/settings-v2.html page 1):
// how a new chat starts and is named, what happens to a message written while
// an agent works, when Workass tells you something, and the dictation
// language. Everything here lives in the daemon's app settings
// (general-settings.ts, chat-titles.ts) except "Avisos de escritorio", which
// is this device's own permission.

import type { ReactNode } from 'react';
import { store, useApp } from '../../store/store';
import { has } from '../../wire/api';
import { chatTitleAgents, resolveChatTitleChoice } from '../../chat-titles';
import {
  EFFORT_WORD, NEW_CHAT_FOLDER_ACTIVE, NEW_CHAT_FOLDER_LAST, newChatAgents, newChatModel,
  type GeneralSettings, type NewChatDefaults, type NoticeSettings,
} from '../../general-settings';
import { defaultEffortId } from '../../model-controls';
import { buildWorkspaceGroups, normalizeWorkspacePath } from '../../workspaces';
import { canDictate } from '../../voice';
import { MODE_LABEL } from '../Composer';

function Row({ name, detail, children }: { name: string; detail?: ReactNode; children: ReactNode }) {
  return (
    <div className="set">
      <div className="lbl">
        <div className="n">{name}</div>
        {detail && <div className="d">{detail}</div>}
      </div>
      <div className="ctl">{children}</div>
    </div>
  );
}

function Toggle({ on, label, onChange }: { on: boolean; label: string; onChange: (on: boolean) => void }) {
  return (
    <button className={`toggle ${on ? 'on' : ''}`} role="switch" aria-checked={on} aria-label={label} onClick={() => onChange(!on)}>
      <span className="knob" />
    </button>
  );
}

function setGeneral(app: { general: GeneralSettings }, patch: Partial<GeneralSettings>) {
  store.setGeneral({ ...app.general, ...patch });
}

function effortWord(effort: string): string {
  return EFFORT_WORD[effort] ?? effort.charAt(0).toUpperCase() + effort.slice(1);
}

// "Chats nuevos": the agent, model, effort, permissions and folder a chat
// started here begins with. With no agent chosen a new chat copies the open
// one (what Workass always did); the agents offered are the ready ones on this
// machine.
function NewChatRows() {
  const app = useApp();
  const defaults = app.general.newChat;
  const setNewChat = (patch: Partial<NewChatDefaults>) => setGeneral(app, { newChat: { ...defaults, ...patch } });
  const agents = newChatAgents(app.groups);
  const group = defaults.providerId ? app.groups.find((g) => g.providerId === defaults.providerId) : undefined;
  const ready = !!group && agents.includes(group);
  const model = group ? newChatModel(defaults, group) : null;
  const efforts = model?.efforts ?? [];
  const effort = efforts.includes(defaults.effort) && defaults.model === model?.modelId ? defaults.effort : defaultEffortId(efforts);
  const modes = group?.modes ?? [];
  const mode = modes.find((m) => m.id === defaults.modeId) ?? modes[0];
  const groups = buildWorkspaceGroups(app.workspaces, app.chats, app.removedWorkspaces, app.deferredChats?.cwds ?? [])
    .filter((g) => normalizeWorkspacePath(g.path));
  const fixedListed = groups.some((g) => normalizeWorkspacePath(g.path) === normalizeWorkspacePath(defaults.folder));
  const folder = defaults.folder === NEW_CHAT_FOLDER_ACTIVE || fixedListed ? defaults.folder : NEW_CHAT_FOLDER_LAST;

  return (
    <>
      <Row name="Agente" detail={!defaults.providerId
        ? 'Un chat nuevo copia el agente, el modelo y los permisos del chat abierto.'
        : ready ? 'El de un chat nuevo. Cada chat puede cambiarlo.'
        : 'No está listo en esta computadora: un chat nuevo copia el chat abierto.'}>
        <select className="inp" aria-label="Agente de un chat nuevo" value={defaults.providerId}
          onChange={(e) => setNewChat({ providerId: e.target.value, model: '', effort: '', modeId: '' })}>
          <option value="">Como el chat abierto</option>
          {agents.map((g) => <option key={g.providerId} value={g.providerId}>{g.providerName || g.providerId}</option>)}
          {group && !ready && <option value={group.providerId}>{group.providerName || group.providerId}</option>}
          {defaults.providerId && !group && <option value={defaults.providerId}>{defaults.providerId}</option>}
        </select>
      </Row>
      {ready && group && model && (
        <Row name="Modelo y esfuerzo" detail="El modelo y cuánto razona.">
          <select className="inp" aria-label="Modelo de un chat nuevo" value={model.modelId}
            onChange={(e) => setNewChat({ model: e.target.value, effort: '' })}>
            {group.models.map((m) => <option key={m.modelId} value={m.modelId}>{m.name}</option>)}
          </select>
          {efforts.length > 1 && (
            <div className="seg" role="group" aria-label="Esfuerzo">
              {efforts.map((value) => (
                <button key={value} className={value === effort ? 'on' : ''}
                  onClick={() => setNewChat({ model: model.modelId, effort: value })}>{effortWord(value)}</button>
              ))}
            </div>
          )}
        </Row>
      )}
      {ready && mode && modes.length > 1 && (
        <Row name="Permisos" detail="Con qué permisos arranca.">
          <select className="inp" aria-label="Permisos de un chat nuevo" value={mode.id}
            onChange={(e) => setNewChat({ modeId: e.target.value })}>
            {modes.map((m) => <option key={m.id} value={m.id}>{MODE_LABEL[m.id] ?? m.name}</option>)}
          </select>
        </Row>
      )}
      <Row name="Carpeta" detail="Dónde abre un chat nuevo si no elegís otra.">
        <select className="inp" aria-label="Carpeta de un chat nuevo" value={folder}
          onChange={(e) => setNewChat({ folder: e.target.value })}>
          <option value={NEW_CHAT_FOLDER_LAST}>La última usada</option>
          <option value={NEW_CHAT_FOLDER_ACTIVE}>La del chat abierto</option>
          {groups.map((g) => <option key={g.path} value={g.path} title={g.path}>{g.name}</option>)}
        </select>
      </Row>
    </>
  );
}

// How new chats are named. "Primer mensaje" is the old behavior; "Un agente"
// also sends the first message once to the chosen agent for a short title
// (daemon app-chat:auto-title). A rename typed by hand always wins. The agents
// and models listed are the ones this machine's providers offer for one-shot
// prompts.
function ChatTitleRow() {
  const app = useApp();
  if (!has('appChatAutoTitle') || !has('setSettings')) return null;
  const choice = app.chatTitles;
  const agents = chatTitleAgents(app.providers);
  const pick = resolveChatTitleChoice(choice, agents);
  const byAgent = choice.mode === 'agent';
  const options = agents.flatMap((a) => a.models.map((m) => ({ key: `${a.providerId}\u0000${m.id}`, agent: a, model: m })));
  return (
    <Row name="Título" detail={!byAgent
      ? 'El título son las primeras palabras del primer mensaje.'
      : pick
        ? `${pick.agent.name} lee el primer mensaje una sola vez, sin herramientas ni proyecto, y le pone un título corto. Si lo renombrás vos, queda tu nombre.`
        : 'Ningún agente activo en esta computadora puede nombrar chats: se usa el primer mensaje.'}>
      <div className="seg">
        <button className={!byAgent ? 'on' : ''} onClick={() => store.setChatTitles({ ...choice, mode: 'prompt' })}>Primer mensaje</button>
        <button className={byAgent ? 'on' : ''} disabled={!pick && !byAgent} title={!pick ? 'Ningún agente activo puede nombrar chats' : undefined}
          onClick={() => store.setChatTitles({ ...choice, mode: 'agent' })}>Un agente</button>
      </div>
      {byAgent && pick && options.length > 1 && (
        <select className="inp" aria-label="Agente y modelo que nombran los chats" value={`${pick.agent.providerId}\u0000${pick.model.id}`}
          onChange={(e) => {
            const next = options.find((o) => o.key === e.target.value);
            if (next) store.setChatTitles({ ...choice, providerId: next.agent.providerId, model: next.model.id });
          }}>
          {options.map((o) => <option key={o.key} value={o.key}>{`${o.agent.name} · ${o.model.name}`}</option>)}
        </select>
      )}
    </Row>
  );
}

const NOTICE_ROWS: { key: Exclude<keyof NoticeSettings, 'sound'>; name: string; detail: ReactNode }[] = [
  { key: 'needsYou', name: 'Cuando un agente necesita algo de vos', detail: 'Un permiso o una pregunta, aunque estés en otra app.' },
  { key: 'turnEnded', name: 'Cuando termina un turno', detail: 'Solo si no estás mirando ese chat.' },
  { key: 'jobs', name: 'Cuando termina o falla un trabajo en segundo plano', detail: <>Entrenamientos, servidores, <span className="mono">workass run</span>.</> },
];

const DICTATION_LANGS: { lang: string; name: string }[] = [
  { lang: 'es', name: 'Español' }, { lang: 'en', name: 'Inglés' }, { lang: 'auto', name: 'Detectar' },
];

export function GeneralPage() {
  const app = useApp();
  const general = app.general;
  const saved = has('setSettings') && has('getSettings');
  const notices = general.notices;
  const setNotices = (patch: Partial<NoticeSettings>) => setGeneral(app, { notices: { ...notices, ...patch } });
  const modifiedEnter = typeof window !== 'undefined' && window.workassWindow?.platform === 'darwin' ? '⌘Enter' : 'Ctrl+Enter';
  return (
    <section className="stgs-panel">
      {saved && (
        <>
          <div className="gtitle">Chats nuevos</div>
          <div className="group">
            <NewChatRows />
            <ChatTitleRow />
          </div>
          <div className="gtitle">Si escribís mientras trabaja</div>
          <div className="group">
            <Row name="Un mensaje nuevo" detail={`«En cola» espera a que termine el turno; «Dirigir» se lo da al agente en el momento. ${modifiedEnter} hace lo otro.`}>
              <div className="seg">
                <button className={general.whileWorking === 'queue' ? 'on' : ''} onClick={() => setGeneral(app, { whileWorking: 'queue' })}>En cola</button>
                <button className={general.whileWorking === 'steer' ? 'on' : ''} onClick={() => setGeneral(app, { whileWorking: 'steer' })}>Dirigir</button>
              </div>
            </Row>
          </div>
        </>
      )}
      <div className="gtitle">Avisos</div>
      <div className="group">
        <Row name="Avisos de escritorio" detail={notifDetail(app.notifEnabled, app.notifPermission)}>
          <Toggle on={app.notifEnabled} label="Avisos de escritorio" onChange={(on) => void store.setNotifEnabled(on)} />
        </Row>
        {saved && NOTICE_ROWS.map((row) => (
          <Row key={row.key} name={row.name} detail={row.detail}>
            <Toggle on={notices[row.key]} label={row.name} onChange={(on) => setNotices({ [row.key]: on })} />
          </Row>
        ))}
        {saved && (
          <Row name="Sonido" detail="Un sonido corto con cada aviso.">
            <Toggle on={notices.sound} label="Sonido" onChange={(on) => {
              setNotices({ sound: on });
              if (on) store.playNoticeSound();
            }} />
          </Row>
        )}
      </div>
      {saved && canDictate() && (
        <>
          <div className="gtitle">Dictado</div>
          <div className="group">
            <Row name="Idioma" detail="Se transcribe en esta máquina; nada sale a internet.">
              <div className="seg">
                {DICTATION_LANGS.map((option) => (
                  <button key={option.lang} className={general.dictation.lang === option.lang ? 'on' : ''}
                    onClick={() => setGeneral(app, { dictation: { lang: option.lang } })}>{option.name}</button>
                ))}
              </div>
            </Row>
          </div>
        </>
      )}
    </section>
  );
}

function notifDetail(enabled: boolean, perm: NotificationPermission | 'unsupported'): string {
  if (perm === 'unsupported') return 'Tu navegador no soporta notificaciones: los avisos se ven dentro de la app.';
  if (!enabled) return 'Apagados: los avisos se ven solo dentro de la app.';
  if (perm === 'granted') return 'Se muestran como notificaciones del sistema.';
  if (perm === 'denied') return 'El navegador bloqueó los permisos: los avisos se ven dentro de la app.';
  return 'Activados. Concedé el permiso del navegador para verlos en el sistema.';
}
