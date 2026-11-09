// Ajustes · Permisos (Settings rework, mock .dev/mocks/settings-v2.html page 4).
//  - Niveles: one name per level for every agent, each enabled agent's own mode
//    under it, from the daemon's catalog (permissionLevels). New chats take the
//    open chat's mode, so there is no default level to pick here.
//  - Subagentes: whether an agent may raise the chats and subagents it starts
//    or changes up to its own level; off, every raise is a card for the owner.
//  - Navegador del agente: allow / ask / block for opening pages and uploads,
//    allow / block for downloads (nothing can stop to ask for a download), and
//    the browser profile's sessions, which the agent browses with.
// The daemon enforces every rule (cmd/workass/agent_permissions.go).

import { useEffect, useState } from 'react';
import { store, useApp } from '../../store/store';
import { has } from '../../wire/api';
import {
  PERMISSION_LEVELS, browserProfileApi, permissionLevelMappings,
  type AgentPermissionSettings, type BrowserRule, type DownloadRule,
} from '../../agent-permissions';

const RULE_NAMES: Record<BrowserRule, string> = { allow: 'Permitir', ask: 'Preguntar', block: 'Bloquear' };

function RuleSeg<T extends BrowserRule>({ value, options, label, onPick }: { value: T; options: readonly T[]; label: string; onPick: (next: T) => void }) {
  return (
    <div className="seg" role="radiogroup" aria-label={label}>
      {options.map((option) => (
        <button key={option} role="radio" aria-checked={value === option} className={value === option ? 'on' : ''}
          onClick={() => onPick(option)}>{RULE_NAMES[option]}</button>
      ))}
    </div>
  );
}

function LevelsGroup() {
  const app = useApp();
  const levels = permissionLevelMappings(app.groups);
  if (!levels.length) return null;
  return (
    <>
      <div className="gtitle">Niveles</div>
      <div className="group">
        {levels.map(({ level, agents }) => {
          const meta = PERMISSION_LEVELS.find((l) => l.id === level)!;
          return (
            <div key={level} className="set">
              <div className="lbl">
                <div className="n">{meta.name}</div>
                <div className="d">{meta.detail} <span className="why">{agents.map((a) => `${a.agent}: ${a.mode}`).join(' · ')}</span></div>
              </div>
            </div>
          );
        })}
      </div>
    </>
  );
}

function BrowserSessionsRow() {
  const api = browserProfileApi();
  const [sites, setSites] = useState<string[] | null>(null);
  const [open, setOpen] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let live = true;
    if (api) void api.profileSites().then((reply) => { if (live) setSites(reply?.sites ?? null); }).catch(() => {});
    return () => { live = false; };
  }, [api]);
  if (!api || !sites) return null;
  const closeAll = async () => {
    setBusy(true);
    try {
      const reply = await api.clearSessions();
      setSites(reply?.sites ?? []);
      setConfirming(false);
      setOpen(false);
    } finally { setBusy(false); }
  };
  const count = sites.length;
  return (
    <>
      <div className="set">
        <div className="lbl">
          <div className="n">Sitios con sesión guardada</div>
          <div className="d">{count === 0
            ? 'Ninguno.'
            : `${count} ${count === 1 ? 'sitio' : 'sitios'}. El agente navega con esas sesiones, sin que se vea.`}</div>
        </div>
        {count > 0 && (
          <div className="ctl">
            {confirming ? (
              <>
                <button className="btn sm" disabled={busy} onClick={() => setConfirming(false)}>Cancelar</button>
                <button className="btn sm danger" disabled={busy} onClick={() => void closeAll()}>
                  {busy ? 'Cerrando…' : `Cerrar ${count === 1 ? 'la sesión' : `las ${count}`}`}
                </button>
              </>
            ) : (
              <>
                <button className="btn sm" aria-expanded={open} onClick={() => setOpen((v) => !v)}>{open ? 'Ocultar' : 'Ver'}</button>
                <button className="btn sm danger" onClick={() => setConfirming(true)}>Cerrar todas</button>
              </>
            )}
          </div>
        )}
      </div>
      {open && count > 0 && (
        <div className="set"><div className="lbl"><div className="d ag-path">{sites.join(' · ')}</div></div></div>
      )}
    </>
  );
}

export function PermisosPage() {
  const app = useApp();
  const canSave = has('getSettings') && has('setSettings');
  const rules = app.agentPermissions;
  const set = (patch: Partial<AgentPermissionSettings>) => store.setAgentPermissions({ ...rules, ...patch });
  const browser = canSave || !!browserProfileApi();
  return (
    <section className="stgs-panel">
      <LevelsGroup />
      {canSave && (
        <>
          <div className="gtitle">Subagentes</div>
          <div className="group">
            <div className="set">
              <div className="lbl">
                <div className="n">Un agente puede subir los permisos de sus subagentes</div>
                <div className="d">Nunca por encima del suyo. Si lo apagás, solo vos podés subirlos.</div>
              </div>
              <div className="ctl">
                <button className={`toggle ${rules.subagentRaise ? 'on' : ''}`} role="switch" aria-checked={rules.subagentRaise}
                  aria-label="Un agente puede subir los permisos de sus subagentes"
                  onClick={() => set({ subagentRaise: !rules.subagentRaise })}>
                  <span className="knob" />
                </button>
              </div>
            </div>
          </div>
        </>
      )}
      {browser && <div className="gtitle">Navegador del agente</div>}
      {browser && <div className="group">
        {canSave && (
          <>
            <div className="set">
              <div className="lbl"><div className="n">Navegar</div><div className="d">Abrir páginas y leerlas. Preguntar: una vez por sitio en cada chat.</div></div>
              <div className="ctl"><RuleSeg label="Navegar" value={rules.browserNavigate} options={['allow', 'ask', 'block'] as const}
                onPick={(browserNavigate) => set({ browserNavigate })} /></div>
            </div>
            <div className="set">
              <div className="lbl"><div className="n">Descargar archivos</div><div className="d">Guardar algo de una página en tu disco.</div></div>
              <div className="ctl"><RuleSeg<DownloadRule> label="Descargar archivos" value={rules.browserDownload} options={['allow', 'block'] as const}
                onPick={(browserDownload) => set({ browserDownload })} /></div>
            </div>
            <div className="set">
              <div className="lbl"><div className="n">Subir archivos</div><div className="d">Mandar un archivo tuyo a una página.</div></div>
              <div className="ctl"><RuleSeg label="Subir archivos" value={rules.browserUpload} options={['allow', 'ask', 'block'] as const}
                onPick={(browserUpload) => set({ browserUpload })} /></div>
            </div>
          </>
        )}
        <BrowserSessionsRow />
      </div>}
    </section>
  );
}
