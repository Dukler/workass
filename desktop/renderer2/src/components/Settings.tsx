// Settings — full-view takeover, ported from the approved mock
// desktop/docs/mocks/p4/designs/settings.html. Five sections: Agentes,
// Dispositivos, Engines, Apariencia, Atajos.
//
// REAL data only, feature-detected against the daemon LAN bridge:
//  - Dispositivos: lan:devices + lan:access-request/decide + lan:revoke (live).
//  - Agentes: live providers:list/catalog state with explicit detect and
//    enable/disable controls; the legacy config:get fallback remains for older
//    daemons that do not expose the provider registry.
//  - Engines: live fleet from proc:list; lifecycle values are daemon CLI-flag
//    defaults shown read-only (no channel exposes them — no fake persistence).
//  - Apariencia: theme + density, persisted locally.

import { useEffect, useState, type ReactNode } from 'react';
import type { MachineView, SettingsSection } from '../store/types';
import type { LanDevice, AccessRequest, ProcessSummary, ProviderRecord } from '../wire/types';
import { store, useApp, useProc, type ProviderSettingsMachine } from '../store/store';
import { call, has } from '../wire/api';
import { normalizeMachineNickname } from '../machine-nickname';
import { hasModelIcon, ModelIcon, providerIconBrand } from '../icons';
import { chatTitleAgents, resolveChatTitleChoice } from '../chat-titles';

const SECTION_NAME: Record<SettingsSection, string> = {
  agentes: 'Agentes', maquinas: 'Conectar Workass', dispositivos: 'Acceso aprobado', engines: 'Engines',
  apariencia: 'Apariencia', atajos: 'Atajos',
};

function Svg({ children }: { children: ReactNode }) {
  return <svg viewBox="0 0 16 16" fill="none" stroke="currentColor">{children}</svg>;
}
const NavIcon = {
  agentes: <Svg><rect x="2.5" y="3" width="11" height="8" rx="2" /><circle cx="6" cy="7" r="1" /><circle cx="10" cy="7" r="1" /><path d="M8 11v2M5 14h6" /></Svg>,
  maquinas: <Svg><rect x="1.8" y="3" width="12.4" height="8" rx="1.6" /><path d="M5.5 13.5h5M8 11v2.5" /></Svg>,
  dispositivos: <Svg><rect x="2" y="3.5" width="9" height="6.5" rx="1.5" /><path d="M4 12.5h5" /><rect x="11.5" y="6" width="3" height="7" rx="1" /></Svg>,
  engines: <Svg><rect x="2.5" y="2.5" width="11" height="4" rx="1.2" /><rect x="2.5" y="9.5" width="11" height="4" rx="1.2" /><circle cx="5" cy="4.5" r=".6" fill="currentColor" /><circle cx="5" cy="11.5" r=".6" fill="currentColor" /></Svg>,
  apariencia: <Svg><circle cx="8" cy="8" r="5.5" /><path d="M8 2.5v11" /><path d="M8 8l4.8-2.7M8 8l-4.8 2.7" /></Svg>,
  atajos: <Svg><rect x="1.5" y="4" width="13" height="8" rx="1.8" /><path d="M4 6.5h.01M6.5 6.5h.01M9 6.5h.01M11.5 6.5h.01M4.5 9.5h7" /></Svg>,
};

// ---- shared helpers ------------------------------------------------------
function liveEngines(procs: ProcessSummary[]): ProcessSummary[] {
  return procs.filter((p) => p.engine && p.status === 'running');
}
function fmtRss(kb: number): string {
  if (!kb) return '—';
  const mb = kb / 1024;
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${Math.round(mb)} MB`;
}
function fleetLabel(procs: ProcessSummary[]): { count: number; rss: string } {
  const eng = liveEngines(procs);
  const kb = eng.reduce((s, p) => s + (p.rssKb ?? 0), 0);
  return { count: eng.length, rss: fmtRss(kb) };
}
function fmtSeen(iso?: string): string {
  if (!iso) return 'visto —';
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return 'visto —';
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (s < 60) return 'visto ahora';
  if (s < 3600) return `visto hace ${Math.floor(s / 60)} min`;
  if (s < 86400) return `visto hace ${Math.floor(s / 3600)} h`;
  return `visto ${new Date(iso).toLocaleDateString('es', { day: 'numeric', month: 'short' })}`;
}
function accentHex(): string {
  if (typeof getComputedStyle === 'undefined') return '';
  return getComputedStyle(document.documentElement).getPropertyValue('--acc').trim() || '#4fa583';
}

// ---- 1 · Agentes ---------------------------------------------------------
function providerStatusLabel(provider: ProviderRecord): string {
  if (provider.disabledByUser) return 'Desactivado';
  const status = provider.status;
  switch (status) {
    case 'ready': return 'Listo';
    case 'needs-login': return 'Requiere login';
    case 'not-found': return 'No instalado';
    case 'error': return 'Error';
    case 'inactive': return 'Inactivo';
    default: return status || 'Desconocido';
  }
}
function AgentProviderRow({ provider, machine }: { provider: ProviderRecord; machine: ProviderSettingsMachine }) {
  const app = useApp();
  const [saving, setSaving] = useState(false);
  const group = machine.remote ? undefined : app.groups.find((g) => g.providerId === provider.id);
  const ready = provider.status === 'ready';
  const active = !provider.disabledByUser;
  const retryable = active && (provider.status === 'needs-login' || provider.status === 'error' || provider.status === 'inactive');
  const brand = providerIconBrand(provider.id, provider.assistantBrand || group?.assistantBrand);
  const hasBrandIcon = hasModelIcon(brand);
  const label = provider.name || provider.id;
  const statusLabel = providerStatusLabel(provider);
  const issue = active && !ready && provider.status !== 'inactive'
    ? provider.fixHint || provider.error || provider.message
      || (provider.status === 'not-found' ? `CLI no encontrado en ${machine.name}.` : statusLabel)
    : '';
  const statusClass = !active ? 'off' : ready ? 'run' : provider.status === 'error' ? 'error' : provider.status === 'needs-login' || provider.status === 'not-found' ? 'warn' : 'off';
  const canToggle = has('providersToggle') && machine.connected;
  const toggle = async () => {
    if (!canToggle || saving) return;
    setSaving(true);
    try { await store.toggleProvider(provider.id, !active, machine.machineId); }
    finally { setSaving(false); }
  };
  return (
    <div className={`lrow agent-provider-row ${provider.disabledByUser ? 'disabled' : ''} ${ready ? 'ready' : ''}`} role="listitem">
      <div className="agent-provider-icon-wrap" role="img" aria-label={`${label} en ${machine.name}: ${statusLabel}`} title={statusLabel}>
        <div className={`ic agent-provider-icon ${hasBrandIcon ? 'brand' : 'mono'}`}>
          {hasBrandIcon ? <ModelIcon provider={brand} /> : label.charAt(0)}
        </div>
        <span className={`agent-status-dot ${statusClass}`} aria-hidden="true" />
      </div>
      <div className="body">
        <div className="nm">{label}</div>
        <div
          className={`agent-provider-location ${machine.remote ? 'remote' : 'local'}`}
          title={`${machine.remote ? 'Agente remoto' : 'Agente en este equipo'} · ${machine.name}${machine.connected ? '' : ' · sin conexión'}`}
        >
          <span className="agent-provider-location-icon" aria-hidden="true">
            {machine.remote
              ? <Svg><path d="M6.5 5.2l1.2-1.2a2.3 2.3 0 013.3 3.3L9.8 8.5M9.5 10.8 8.3 12a2.3 2.3 0 01-3.3-3.3l1.2-1.2M6.4 9.6l3.2-3.2" /></Svg>
              : NavIcon.maquinas}
          </span>
          <span className="agent-provider-location-kind">{machine.remote ? 'Remoto' : 'Este equipo'}</span>
          <span className="agent-provider-location-separator" aria-hidden="true">·</span>
          <span className="agent-provider-location-machine">{machine.name}</span>
          {!machine.connected && <span className="agent-provider-location-offline">sin conexión</span>}
        </div>
        {issue && <div className={`agent-provider-issue ${provider.status === 'error' ? 'error' : ''}`}>{issue}</div>}
      </div>
      <div className="act agent-provider-actions">
        {retryable && has('providersDetect') && machine.connected && (
          <button className="btn sm" onClick={() => void store.detectProvider(provider.id, machine.machineId)}>Comprobar de nuevo</button>
        )}
        {canToggle && (
          <button
            className={`agent-switch ${active ? 'on' : ''}`}
            role="switch"
            aria-checked={active}
            aria-label={`${active ? 'Desactivar' : 'Activar'} ${provider.name || provider.id} en ${machine.name}`}
            title={active ? 'Ocultar agente' : 'Mostrar agente'}
            disabled={saving}
            aria-busy={saving}
            onClick={() => void toggle()}
          >
            <span className="agent-switch-track" aria-hidden="true"><span /></span>
          </button>
        )}
      </div>
    </div>
  );
}
function AgentesPanel() {
  const app = useApp();
  const machines = store.providerSettingsMachines();
  const agentRows = machines.flatMap((machine) => machine.providers.map((provider) => ({ machine, provider })));
  const acp = app.daemonConfig?.acp;
  const provider = (acp?.provider || '').trim();
  const command = [acp?.command, ...(Array.isArray(acp?.args) ? (acp!.args as unknown[]).map(String) : [])]
    .filter(Boolean).join(' ');
  const name = provider ? provider.charAt(0).toUpperCase() + provider.slice(1) : 'Agente ACP';
  const connected = app.chats.some((c) => c.sessionId);
  const canProbe = has('appChatDetectAcp');
  const hasProviderRegistry = has('providersList') || app.providers.length > 0 || app.machines.length > 0;

  useEffect(() => { void store.refreshProviderSettings(); }, []);

  return (
    <section className="stgs-panel">

      {hasProviderRegistry ? (
        <div className="agent-provider-group" role="list" aria-label="Agentes por ubicación">
          {agentRows.length > 0 ? agentRows.map(({ machine, provider }) => (
            <AgentProviderRow key={`${machine.machineId || 'local'}:${provider.id}`} provider={provider} machine={machine} />
          )) : (
            <div className="agent-provider-empty">Sin agentes detectados</div>
          )}
        </div>
      ) : app.hasConfigChannel && acp ? (
        <>
          <div className="group">
            <div className="lrow">
              <div className="ic mono">{name.charAt(0)}</div>
              <div className="body">
                <div className="nm">{name}</div>
                <div className="mt">{command || '—'}</div>
              </div>
              <div className="act">
                <span className={`agent-status-dot legacy ${connected ? 'run' : 'off'}`} role="img" aria-label={connected ? 'Conectado' : 'Configurado'} />
                {canProbe && <button className="btn sm" onClick={() => void call('appChatDetectAcp', acp ? { command: acp.command, args: acp.args, provider: acp.provider } : {})}>Probar</button>}
              </div>
            </div>
          </div>
          <div className="ghint">Los modelos del agente aparecen en el selector del compositor.</div>
        </>
      ) : (
        <div className="group"><div className="lrow"><div className="body"><span className="empty">El daemon no expone la configuración de agentes (config:get).</span></div></div></div>
      )}

      <ChatTitlesGroup />
    </section>
  );
}

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

// ---- 3 · Dispositivos ----------------------------------------------------
function platformBadge(d: LanDevice): string {
  const n = (d.name || '').toLowerCase();
  if (n.includes('win')) return 'Windows';
  if (n.includes('mac') || n.includes('darwin')) return 'macOS';
  if (n.includes('iphone') || n.includes('ipad') || n.includes('ios')) return 'iOS';
  if (n.includes('linux')) return 'Linux';
  return 'LAN';
}
function DeviceRow({ d }: { d: LanDevice }) {
  return (
    <div className="lrow">
      <div className="ic"><Svg><rect x="2" y="3" width="12" height="8" rx="1.5" /><path d="M5.5 13.5h5M8 11v2.5" /></Svg></div>
      <div className="body">
        <div className="nm">{d.name || 'Dispositivo'}{d.controller ? <span className="badge acc">Controlador</span> : <span className="badge">{platformBadge(d)}</span>}</div>
        <div className="mt">{d.ip || '—'} · {fmtSeen(d.lastSeen)}</div>
      </div>
      <div className="act">
        {d.controller
          ? <span className="stat"><span className="dot run" />Activo</span>
          : <><span className="stat"><span className="dot" />Aprobado</span><button className="btn sm danger" onClick={() => void store.revokeDevice(d.deviceId)}>Revocar</button></>}
      </div>
    </div>
  );
}
function RequestRow({ r }: { r: AccessRequest }) {
  return (
    <div className="lrow">
      <div className="ic"><Svg><rect x="3" y="2.5" width="10" height="11" rx="1.5" /><path d="M6.5 12h3" /></Svg></div>
      <div className="body">
        <div className="nm"><span className="dot warn" />{r.deviceName || 'Dispositivo desconocido'} <span className="badge">LAN</span></div>
        <div className="mt">{r.ip || '—'} · solicitó acceso{r.requestedAt ? ` ${fmtSeen(r.requestedAt).replace('visto ', '')}` : ''}</div>
      </div>
      <div className="act">
        <button className="btn sm acc" onClick={() => void store.approveAccess(r.requestId)}>Aprobar</button>
        <button className="btn sm danger" onClick={() => void store.rejectAccess(r.requestId)}>Rechazar</button>
      </div>
    </div>
  );
}
function DispositivosPanel() {
  const app = useApp();
  useProc();
  if (!app.hasDeviceChannels) {
    return (
      <section className="stgs-panel">
        
        <div className="group"><div className="lrow"><div className="body"><span className="empty">Este bridge no expone los canales de dispositivos (lan:devices).</span></div></div></div>
      </section>
    );
  }
  const { devices } = app;
  return (
    <section className="stgs-panel">

      <div className="gtitle">Clientes con acceso</div>
      <div className="group">
        {devices.length === 0
          ? (app.devicesLoaded === false ? null : <div className="lrow"><div className="body"><span className="empty">Sin dispositivos aprobados todavía.</span></div></div>)
          : devices.map((d) => <DeviceRow key={d.deviceId} d={d} />)}
      </div>
      <div className="ghint">Cada cliente aprobado recibe su propio acceso. Revocar elimina únicamente ese cliente; no se administran usuarios, contraseñas ni direcciones manuales.</div>
    </section>
  );
}

// ---- 4 · Engines ---------------------------------------------------------
function EngineDefault({ n, d, value, suf }: { n: string; d: string; value: string; suf: string }) {
  return (
    <div className="set">
      <div className="lbl"><div className="n">{n}</div><div className="d">{d}</div></div>
      <div className="ctl"><input className="inp num" value={value} disabled readOnly /><span className="suf">{suf}</span></div>
    </div>
  );
}
function EnginesPanel() {
  const app = useApp();
  useProc();
  const fleet = fleetLabel(app.processes);
  return (
    <section className="stgs-panel">

      <div className="gtitle">Flota</div>
      <div className="group">
        <div className="set">
          <div className="lbl"><div className="n">Estado actual</div><div className="d">Engines vivos y memoria residente agregada.</div></div>
          <div className="ctl">
            {app.hasProcChannels
              ? <span className="stat"><span className={`dot ${fleet.count ? 'run' : ''}`} />{fleet.count} {fleet.count === 1 ? 'engine' : 'engines'} · <span className="mono" style={{ color: 'var(--ink2)' }}>{fleet.rss}</span></span>
              : <span className="stat"><span className="dot" />sin datos de proceso</span>}
          </div>
        </div>
      </div>

      <div className="gtitle">Ciclo de vida <span className="gtag">valores por defecto del daemon · solo lectura</span></div>
      <div className="group">
        <EngineDefault n="TTL de hibernación" d="Tiempo inactivo antes de que un chat suspenda su engine. Los prompts en vuelo se anclan." value="20" suf="min" />
        <EngineDefault n="Umbral de reciclado de RAM" d="Cuando un engine supera este RSS, se recicla en el próximo momento ocioso." value="4" suf="GB" />
        <EngineDefault n="Reserva templada" d="Inactiva: cada chat abre su sesión en su propio directorio, así que un engine pre-calentado nunca se adopta." value="0" suf="engines" />
        <EngineDefault n="Intervalo de muestreo" d="Cada cuánto el reaper muestrea RSS y barre las sesiones inactivas." value="30" suf="s" />
      </div>
      <div className="ghint">Estos valores son flags del daemon (<code>--hibernate-ttl</code>, <code>--engine-max-rss-kb</code>, <code>--spare-sessions</code>, <code>--rss-sample-interval</code>). Ningún canal los edita todavía; se muestran para referencia.</div>
    </section>
  );
}

// ---- 5 · Apariencia ------------------------------------------------------
function AparienciaPanel() {
  const app = useApp();
  const hex = accentHex();
  return (
    <section className="stgs-panel">

      <div className="gtitle">Tema</div>
      <div className="group">
        <div className="set">
          <div className="lbl"><div className="n">Modo de color</div><div className="d">Claro y oscuro con contraste AA. «Sistema» sigue a macOS.</div></div>
          <div className="ctl"><div className="seg">
            <button className={app.themePref === 'system' ? 'on' : ''} onClick={() => store.setThemePref('system')}>Sistema</button>
            <button className={app.themePref === 'light' ? 'on' : ''} onClick={() => store.setThemePref('light')}>Claro</button>
            <button className={app.themePref === 'dark' ? 'on' : ''} onClick={() => store.setThemePref('dark')}>Oscuro</button>
          </div></div>
        </div>
        <div className="set">
          <div className="lbl"><div className="n">Densidad</div><div className="d">Ritmo vertical del texto y las listas.</div></div>
          <div className="ctl"><div className="seg">
            <button className={app.density === 'compact' ? 'on' : ''} onClick={() => store.setDensity('compact')}>Compacta</button>
            <button className={app.density === 'comfortable' ? 'on' : ''} onClick={() => store.setDensity('comfortable')}>Cómoda</button>
          </div></div>
        </div>
      </div>

      <div className="gtitle">Acento</div>
      <div className="group">
        <div className="set">
          <div className="lbl"><div className="n">Color de marca</div><div className="d">Verde, fijo. Se usa solo como acento — nunca en texto de cuerpo ni en fondos grandes.</div></div>
          <div className="ctl"><span className="swatch" /><span className="stat mono">{hex}</span></div>
        </div>
      </div>

      <div className="gtitle">Notificaciones</div>
      <div className="group">
        <div className="set">
          <div className="lbl">
            <div className="n">Avisos de escritorio</div>
            <div className="d">{notifDetail(app.notifEnabled, app.notifPermission)}</div>
          </div>
          <div className="ctl">
            <button className={`toggle ${app.notifEnabled ? 'on' : ''}`} role="switch" aria-checked={app.notifEnabled}
              onClick={() => void store.setNotifEnabled(!app.notifEnabled)}>
              <span className="knob" />
            </button>
          </div>
        </div>
      </div>
      <div className="ghint">Los avisos cubren notificaciones explícitas del agente y solicitudes de permisos. Las actualizaciones aparecen una sola vez en el panel izquierdo; terminar un turno no genera ningún aviso.</div>
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

// ---- 6 · Atajos ----------------------------------------------------------
function Kb({ desc, keys }: { desc: string; keys: string[] }) {
  return (
    <div className="kb"><span className="desc">{desc}</span>{keys.map((k, i) => <kbd key={i}>{k}</kbd>)}</div>
  );
}
function AtajosPanel() {
  return (
    <section className="stgs-panel">
      <div className="gtitle">Teclado</div>
      <div className="group">
        <Kb desc="Enviar prompt al agente" keys={['↵']} />
        <Kb desc="Nueva línea en el compositor" keys={['⇧', '↵']} />
        <Kb desc="Mostrar / ocultar conversaciones" keys={['⌘', 'B']} />
        <Kb desc="Cancelar turno · cerrar panel" keys={['Esc']} />
        <Kb desc="Volver a un punto anterior (menú)" keys={['Esc', 'Esc']} />
        <Kb desc="Abrir / cerrar ajustes" keys={['⌘', ',']} />
      </div>
    </section>
  );
}


// ---- máquinas (remote-plan E3) -------------------------------------------
const MACHINE_SCAN_MS = 30_000;
// One visible ceremony only: discovery finds the other Workass, this client
// requests access, and the controller on the other machine approves it. The
// compatibility wire still understands manual/fleet enrolment, but ordinary
// users never need to choose between those implementation paths.
function MaquinasPanel() {
  const app = useApp();
  const [showPhoneQR, setShowPhoneQR] = useState(false);
  const [phoneQRError, setPhoneQRError] = useState(false);
  const [phoneQRNonce, setPhoneQRNonce] = useState(0);
  const paired = app.machines.filter((machine) => machine.paired);
  const nearby = app.machines.filter((machine) => !machine.paired);
  const incoming = app.accessRequests;

  // On a network with none of this user's machines the daemon does not sweep by
  // itself, so an open pane is what asks. Refresh keeps a machine that just came
  // up from waiting for the pane to be reopened.
  useEffect(() => {
    void store.scanForMachines();
    const timer = window.setInterval(() => { void store.scanForMachines(); }, MACHINE_SCAN_MS);
    return () => window.clearInterval(timer);
  }, []);

  const togglePhoneQR = () => {
    if (!app.fleetCanReveal) return;
    setPhoneQRError(false);
    setPhoneQRNonce((value) => value + 1);
    setShowPhoneQR((value) => !value);
  };

  if (!app.hasMachineChannels) {
    return (
      <section className="stgs-panel">
        
        <div className="group"><div className="lrow"><div className="body">
          <span className="empty">Este daemon todavía no tiene libreta de máquinas (machines:list).</span>
        </div></div></div>
      </section>
    );
  }

  return (
    <section className="stgs-panel">

      <div className="group phone-pairing">
        <div className="lrow">
          <div className="ic"><Svg><rect x="4" y="1.5" width="8" height="13" rx="1.6" /><path d="M6.5 4.5h3M7.5 12h1" /></Svg></div>
          <div className="body">
            <div className="nm">Vincular teléfono</div>
            <div className="mt">Escaneá el código desde Workass Mobile</div>
          </div>
          <div className="act">
            <button
              type="button"
              className="btn sm acc"
              disabled={!app.fleetCanReveal}
              title={app.fleetCanReveal ? undefined : 'El QR solo puede mostrarse desde la computadora que guarda la clave'}
              aria-expanded={showPhoneQR}
              onClick={togglePhoneQR}
            >
              {showPhoneQR ? 'Ocultar QR' : 'Mostrar QR'}
            </button>
          </div>
        </div>
        {showPhoneQR && (
          <div className="phone-qr-sheet">
            {phoneQRError ? (
              <div className="phone-qr-error">
                <span>No se pudo generar el QR.</span>
                <button type="button" className="btn sm" onClick={() => { setPhoneQRError(false); setPhoneQRNonce((value) => value + 1); }}>Reintentar</button>
              </div>
            ) : (
              <img
                src={`/workass/fleet-qr.svg?v=${phoneQRNonce}`}
                alt="Código QR para vincular Workass Mobile"
                onError={() => setPhoneQRError(true)}
              />
            )}
            <p>El código contiene la clave de acceso. Mostralo solo al teléfono que querés vincular.</p>
          </div>
        )}
      </div>

      <div className="pairing-flow" aria-label="Flujo para conectar otra Workass">
        <div className="pairing-step"><span>1</span><b>Descubrir</b><small>Ambas Workass abiertas en la misma red</small></div>
        <div className="pairing-arrow" aria-hidden>→</div>
        <div className="pairing-step"><span>2</span><b>Solicitar</b><small>Un clic desde la lista de abajo</small></div>
        <div className="pairing-arrow" aria-hidden>→</div>
        <div className="pairing-step"><span>3</span><b>Aprobar</b><small>Confirmar en la otra Workass</small></div>
      </div>

      <div className="gtitle">Solicitudes recibidas</div>
      <div className="group">
        {incoming.length === 0
          ? <div className="lrow"><div className="body"><span className="empty">Ninguna solicitud esperando aprobación.</span></div></div>
          : incoming.map((r) => <RequestRow key={r.requestId} r={r} />)}
      </div>
      <div className="ghint">Una solicitud aparece acá únicamente en la Workass que debe conceder el acceso.</div>

      <div className="gtitle">Workass descubiertas en esta red</div>
      <div className="group">
        {nearby.length === 0
          ? <div className="lrow"><div className="body"><span className="empty">No se detectó otra Workass. Abrila en la otra máquina y verificá que ambas estén en la misma red.</span></div></div>
          : nearby.map((m) => <MachineRow key={m.machineId} m={m} />)}
      </div>
      <div className="ghint">El descubrimiento no concede acceso por sí solo. “Solicitar conexión” abre una única aprobación cifrada en la otra Workass.</div>

      <div className="gtitle">Workass conectadas</div>
      <div className="group">
        <div className="lrow">
          <div className="ic"><Svg><rect x="3" y="2" width="10" height="12" rx="1.5" /><path d="M5.5 5h5M5.5 8h5M5.5 11h3" /></Svg></div>
          <div className="body">
            <div className="nm">Esta Workass</div>
            <div className="mt">el cliente que estás mirando</div>
          </div>
        </div>
        {paired.length === 0
          ? <div className="lrow"><div className="body"><span className="empty">Ninguna otra Workass conectada.</span></div></div>
          : paired.map((m) => <MachineRow key={m.machineId} m={m} />)}
      </div>
    </section>
  );
}

function MachineRow({ m }: { m: MachineView }) {
  const live = m.link === 'ready';
  const dot = live ? 'ok' : (m.paired ? 'warn' : '');
  const canNickname = m.paired && has('machinesNickname');
  const [editingNickname, setEditingNickname] = useState(false);
  const [nickname, setNickname] = useState('');
  const [savingNickname, setSavingNickname] = useState(false);
  const [nicknameError, setNicknameError] = useState('');
  const beginNickname = () => {
    setNickname(m.nickname);
    setNicknameError('');
    setEditingNickname(true);
  };
  const cancelNickname = () => {
    setEditingNickname(false);
    setNicknameError('');
  };
  const saveNickname = async () => {
    if (savingNickname) return;
    const normalized = normalizeMachineNickname(nickname);
    if (normalized.error) { setNicknameError(normalized.error); return; }
    setSavingNickname(true);
    setNicknameError('');
    const result = await store.setMachineNickname(m.machineId, normalized.nickname);
    setSavingNickname(false);
    if (!result.ok) { setNicknameError(result.error || 'No se pudo guardar el apodo'); return; }
    setEditingNickname(false);
  };
  return (
    <div className="lrow">
      <div className="ic"><Svg><rect x="1.8" y="3" width="12.4" height="8" rx="1.6" /><path d="M5.5 13.5h5M8 11v2.5" /></Svg></div>
      <div className="body">
        {canNickname && editingNickname ? (
          <input
            className="inp machine-nickname-input"
            value={nickname}
            placeholder={m.reportedName}
            aria-label={`Apodo para ${m.reportedName}`}
            autoFocus
            onChange={(event) => { setNickname(event.target.value); setNicknameError(''); }}
            onKeyDown={(event) => {
              if (event.key === 'Enter') { event.preventDefault(); void saveNickname(); }
              if (event.key === 'Escape') { event.preventDefault(); cancelNickname(); }
            }}
          />
        ) : (
          <div className="nm"><span className={`dot ${dot}`} />{m.name}
            {!m.secure && <span className="badge">insegura</span>}</div>
        )}
        <div className="mt">{m.nickname ? `${m.reportedName} · ` : ''}{m.address || '—'}{m.reason ? ` · ${m.reason}` : (live ? ' · conectada' : m.requested ? ' · esperando aprobación' : ' · disponible en la red')}</div>
        {nicknameError && <div className="machine-nickname-error" role="alert">{nicknameError}</div>}
      </div>
      <div className="act">
        {!m.paired && !m.requested && <button className="btn sm acc" disabled={!m.secure || !m.reachable} onClick={() => void store.requestMachineAccess(m.machineId)}>Solicitar conexión</button>}
        {!m.paired && m.requested && <span className="badge">solicitud enviada</span>}
        {canNickname && editingNickname ? (
          <>
            <button className="btn sm acc" disabled={savingNickname} onClick={() => void saveNickname()}>{savingNickname ? 'Guardando…' : 'Guardar'}</button>
            <button className="btn sm" disabled={savingNickname} onClick={cancelNickname}>Cancelar</button>
          </>
        ) : (
          <>
            {canNickname && <button className="btn sm" onClick={beginNickname}>{m.nickname ? 'Editar apodo' : 'Cambiar apodo'}</button>}
            {m.paired && <button className="btn sm danger" onClick={() => void store.forgetMachine(m.machineId)}>Desconectar</button>}
          </>
        )}
      </div>
    </div>
  );
}

// ---- shell ---------------------------------------------------------------
const PANELS: Record<SettingsSection, () => ReactNode> = {
  agentes: AgentesPanel, maquinas: MaquinasPanel, dispositivos: DispositivosPanel,
  engines: EnginesPanel, apariencia: AparienciaPanel, atajos: AtajosPanel,
};
const ORDER: SettingsSection[] = ['agentes', 'maquinas', 'dispositivos', 'engines', 'apariencia', 'atajos'];

export function Settings() {
  const app = useApp();
  useProc();
  // A renderer replacement can reconnect while an older in-memory shell still
  // names a section removed by the new bundle. Never let that stale value blank
  // the Settings surface; the canonical default remains Agentes.
  const sec: SettingsSection = Object.prototype.hasOwnProperty.call(PANELS, app.settingsSection)
    ? app.settingsSection
    : 'agentes';
  const Panel = PANELS[sec];
  const fleet = fleetLabel(app.processes);

  return (
    <div className="stgs">
      <aside className="stgs-nav">
        <div className="titlebar"><span className="tl"><span /><span /><span /></span></div>
        <div className="navhead">
          <Svg><circle cx="8" cy="8" r="2.4" /><path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.3 3.3l1.4 1.4M11.3 11.3l1.4 1.4M12.7 3.3l-1.4 1.4M4.7 11.3l-1.4 1.4" /></Svg>
          <b>Ajustes</b>
        </div>
        {ORDER.map((s) => (
          <div key={s} className={`nrow ${s === sec ? 'on' : ''}`} role="button" tabIndex={0} onClick={() => store.setSettingsSection(s)}>
            {NavIcon[s]}{SECTION_NAME[s]}
          </div>
        ))}
        <div className="foot">
          {app.hasProcChannels && (
            <div className="fleet"><span className="fd" />{fleet.count} {fleet.count === 1 ? 'engine' : 'engines'}<span className="mono">· {fleet.rss}</span></div>
          )}
        </div>
      </aside>

      <main className="stgs-main">
        <div className="tbar">
          <span className="crumb"><b>{SECTION_NAME[sec]}</b></span>
          <span className="sp" />
          <button className="tico" title="Cerrar · Esc" aria-label="Cerrar" onClick={() => store.closeSettings()}>
            <Svg><path d="M4 4l8 8M12 4l-8 8" /></Svg>
          </button>
        </div>
        <div className="scroll"><div className="stgs-wrap">
          <Panel />
        </div></div>
      </main>
    </div>
  );
}
