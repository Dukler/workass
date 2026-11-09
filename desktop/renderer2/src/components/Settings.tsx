// Settings — full-view takeover. Owner-approved rework (2026-11-08, mock
// .dev/mocks/settings-v2.html): grouped navigation, flat rows with no cards,
// the section name as the title bar and no page heading. Every page shows
// only what Workass really knows or enforces; nothing is a placeholder:
//  - General (settings/GeneralPage): new-chat defaults, Enter while a turn
//    runs, notices and their sound, dictation language.
//  - Apariencia: theme and density, kept on this device.
//  - Agentes y modelos (settings/AgentsPage): each machine's agents, with
//    detect, enable/disable and CLI updates.
//  - Permisos (settings/PermisosPage): each level by every agent's own mode
//    name, who may raise subagents, the agent browser's rules.
//  - Herramientas (settings/HerramientasPage): Workass tool groups and their
//    token cost; each agent's MCP servers, read-only.
//  - Máquinas y acceso: this machine's name, other Workass, the phone, and
//    the clients that keep access (lan:devices / access-request / revoke).
//  - Trabajos (settings/TrabajosPage): job notices and resource holders.
//  - Actualizaciones (settings/ActualizacionesPage): this Mac, "Instalar
//    solo", paired machines and agent CLIs, all on the one updater.
//  - Atajos: the keyboard shortcuts.
//  - Mantenimiento (settings/MantenimientoPage): disk use, when quiet chats
//    file themselves away, running agents, diagnostics export, privacy.
// Accounts (plans, usage pace, resets) are their own page: AccountsPage.tsx,
// opened from the left panel's foot.

import { useEffect, useState, type ReactNode } from 'react';
import type { MachineView, SettingsSection } from '../store/types';
import type { LanDevice, AccessRequest } from '../wire/types';
import { store, useApp, useProc } from '../store/store';
import { has } from '../wire/api';
import { normalizeMachineNickname } from '../machine-nickname';
import { AgentsPage } from './settings/AgentsPage';
import { GeneralPage } from './settings/GeneralPage';
import { PermisosPage } from './settings/PermisosPage';
import { HerramientasPage } from './settings/HerramientasPage';
import { TrabajosPage } from './settings/TrabajosPage';
import { ActualizacionesPage } from './settings/ActualizacionesPage';
import { MantenimientoPage } from './settings/MantenimientoPage';
import { fleetLabel } from './settings/fleet';

const SECTION_NAME: Record<SettingsSection, string> = {
  general: 'General', apariencia: 'Apariencia', agentes: 'Agentes y modelos', permisos: 'Permisos',
  herramientas: 'Herramientas', maquinas: 'Máquinas y acceso', trabajos: 'Trabajos',
  actualizaciones: 'Actualizaciones', atajos: 'Atajos', mantenimiento: 'Mantenimiento',
};

function Svg({ children }: { children: ReactNode }) {
  return <svg viewBox="0 0 16 16" fill="none" stroke="currentColor">{children}</svg>;
}
const NavIcon: Record<SettingsSection, ReactNode> = {
  general: <Svg><path d="M3 4.5h10M3 8h10M3 11.5h6" /></Svg>,
  permisos: <Svg><path d="M8 1.8 13 4v4c0 3-2.2 5.2-5 6.2C5.2 13.2 3 11 3 8V4z" /><path d="M6 8.2 7.5 9.7 10.2 6.8" /></Svg>,
  herramientas: <Svg><path d="M10.5 2.5a3 3 0 0 0-3.4 4L2.8 10.8a1.3 1.3 0 0 0 1.9 1.9L9 8.4a3 3 0 0 0 4-3.4l-1.8 1.8-1.7-.4-.4-1.7z" /></Svg>,
  trabajos: <Svg><path d="M4 2.5h8M4 13.5h8M5 2.5c0 3 6 3.5 6 5.5s-6 2.5-6 5.5M11 2.5c0 3-6 3.5-6 5.5" /></Svg>,
  actualizaciones: <Svg><path d="M8 2.5v7M5 6.5l3 3 3-3M3 12.5h10" /></Svg>,
  agentes: <Svg><rect x="2.5" y="3" width="11" height="8" rx="2" /><circle cx="6" cy="7" r="1" /><circle cx="10" cy="7" r="1" /><path d="M8 11v2M5 14h6" /></Svg>,
  maquinas: <Svg><rect x="1.8" y="3" width="12.4" height="8" rx="1.6" /><path d="M5.5 13.5h5M8 11v2.5" /></Svg>,
  mantenimiento: <Svg><rect x="2.5" y="2.5" width="11" height="4" rx="1.2" /><rect x="2.5" y="9.5" width="11" height="4" rx="1.2" /><circle cx="5" cy="4.5" r=".6" fill="currentColor" /><circle cx="5" cy="11.5" r=".6" fill="currentColor" /></Svg>,
  apariencia: <Svg><circle cx="8" cy="8" r="5.5" /><path d="M8 2.5v11" /><path d="M8 8l4.8-2.7M8 8l-4.8 2.7" /></Svg>,
  atajos: <Svg><rect x="1.5" y="4" width="13" height="8" rx="1.8" /><path d="M4 6.5h.01M6.5 6.5h.01M9 6.5h.01M11.5 6.5h.01M4.5 9.5h7" /></Svg>,
};

// ---- shared helpers ------------------------------------------------------
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

      <div className="gtitle" title="Cada cliente aprobado recibe su propio acceso. Revocar elimina únicamente ese cliente.">Clientes con acceso</div>
      <div className="group">
        {devices.length === 0
          ? (app.devicesLoaded === false ? null : <div className="lrow"><div className="body"><span className="empty">Sin dispositivos aprobados todavía.</span></div></div>)
          : devices.map((d) => <DeviceRow key={d.deviceId} d={d} />)}
      </div>
    </section>
  );
}

// ---- 5 · Apariencia ------------------------------------------------------
function AparienciaPanel() {
  const app = useApp();
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

    </section>
  );
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
        <Kb desc="Abrir la caja de comandos" keys={['⌘', ',']} />
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

      {/* Requests show only on the Workass that has to grant them, and only
          while one is waiting. */}
      {incoming.length > 0 && (
        <>
          <div className="gtitle">Solicitudes recibidas</div>
          <div className="group">{incoming.map((r) => <RequestRow key={r.requestId} r={r} />)}</div>
        </>
      )}

      <div className="gtitle" title="Descubrir no da acceso: «Solicitar conexión» pide una aprobación cifrada en la otra Workass.">Workass descubiertas en esta red</div>
      <div className="group">
        {nearby.length === 0
          ? <div className="lrow"><div className="body"><span className="empty">No se detectó otra Workass en esta red.</span></div></div>
          : nearby.map((m) => <MachineRow key={m.machineId} m={m} />)}
      </div>

      <div className="gtitle">Workass conectadas</div>
      <div className="group">
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

function MaquinasYAccesoPanel() {
  return <><MaquinasPanel /><DispositivosPanel /></>;
}

// ---- shell ---------------------------------------------------------------
const PANELS: Record<SettingsSection, () => ReactNode> = {
  general: GeneralPage, apariencia: AparienciaPanel, agentes: () => <AgentsPage />,
  permisos: PermisosPage, herramientas: HerramientasPage, maquinas: MaquinasYAccesoPanel,
  trabajos: TrabajosPage, actualizaciones: ActualizacionesPage, atajos: AtajosPanel, mantenimiento: MantenimientoPage,
};
// The mock's grouped navigation.
const NAV: ReadonlyArray<{ label?: string; items: SettingsSection[] }> = [
  { items: ['general', 'apariencia'] },
  { label: 'Agentes', items: ['agentes', 'permisos', 'herramientas'] },
  { label: 'Tus máquinas', items: ['maquinas', 'trabajos'] },
  { label: 'Sistema', items: ['actualizaciones', 'atajos', 'mantenimiento'] },
];

export function Settings() {
  const app = useApp();
  useProc();
  // A renderer replacement can reconnect while an older in-memory shell still
  // names a section removed by the new bundle. Never let that stale value blank
  // the Settings surface; the canonical default is General.
  const sec: SettingsSection = Object.prototype.hasOwnProperty.call(PANELS, app.settingsSection)
    ? app.settingsSection
    : 'general';
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
        {NAV.map((group, index) => (
          <div key={group.label ?? index} className="ngroup">
            {group.label && <div className="ngrp">{group.label}</div>}
            {group.items.map((s) => (
              <div key={s} className={`nrow ${s === sec ? 'on' : ''}`} role="button" tabIndex={0}
                onClick={() => store.setSettingsSection(s)}
                onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); store.setSettingsSection(s); } }}>
                {NavIcon[s]}{SECTION_NAME[s]}
              </div>
            ))}
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
