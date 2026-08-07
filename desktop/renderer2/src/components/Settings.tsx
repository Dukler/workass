// Settings — full-view takeover, ported from the approved mock
// desktop/docs/mocks/p4/designs/settings.html. Five sections: Agentes,
// Dispositivos, Engines, Apariencia, Atajos.
//
// REAL data only, feature-detected against the daemon LAN bridge:
//  - Dispositivos: lan:devices + lan:access-request/decide + lan:revoke (live).
//  - Agentes: the single ACP provider from config:get.acp (no providers:list on
//    the daemon today); Probar is hidden unless app-chat:detect-acp is exposed.
//  - Engines: live fleet from proc:list; lifecycle values are daemon CLI-flag
//    defaults shown read-only (no channel exposes them — no fake persistence).
//  - Apariencia: theme + density, persisted locally.

import type { ReactNode } from 'react';
import type { MachineView, SettingsSection } from '../store/types';
import type { LanDevice, AccessRequest, ProcessSummary, ProviderRecord, ProviderUpdate } from '../wire/types';
import { store, useApp, useProc } from '../store/store';
import { call, has } from '../wire/api';
import {
  countScoredModels, getModelScore, groupModelsForScoring, isEmptyScore,
  normalizeNote, NOTE_MAX, SCORE_DIMENSIONS, SCORE_MAX, SCORE_MIN,
  type ModelScore, type ScoreableModel,
} from '../model-scores';

const SECTION_NAME: Record<SettingsSection, string> = {
  agentes: 'Agentes', modelos: 'Modelos', maquinas: 'Conectar Workass', dispositivos: 'Acceso aprobado', engines: 'Engines',
  apariencia: 'Apariencia', atajos: 'Atajos',
};

function Svg({ children }: { children: ReactNode }) {
  return <svg viewBox="0 0 16 16" fill="none" stroke="currentColor">{children}</svg>;
}
const NavIcon = {
  agentes: <Svg><rect x="2.5" y="3" width="11" height="8" rx="2" /><circle cx="6" cy="7" r="1" /><circle cx="10" cy="7" r="1" /><path d="M8 11v2M5 14h6" /></Svg>,
  modelos: <Svg><path d="M3 5h5M12 5h1.5M3 11h1.5M8 11h5" /><circle cx="10" cy="5" r="1.6" /><circle cx="5.5" cy="11" r="1.6" /></Svg>,
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

// Copy an upgrade command to the clipboard — notify-only, workass never runs it.
function copyHint(hint: string) {
  const done = () => store.addToast('Comando copiado', hint);
  try {
    const p = navigator.clipboard?.writeText(hint);
    if (p && typeof p.then === 'function') { p.then(done).catch(() => store.addToast('Comando', hint)); }
    else done();
  } catch { store.addToast('Comando', hint); }
}
// Last non-empty line of a redacted updater tail (single truncated line under
// the row while running / on failure).
function tailLine(tail: string | undefined): string {
  if (!tail) return '';
  const lines = tail.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
  return lines.length ? lines[lines.length - 1] : '';
}
// Secondary copy-command chip — manual fallback; workass never runs this itself.
function CopyChip({ hint }: { hint: string }) {
  return (
    <code className="cli-hint" role="button" tabIndex={0} title="Copiar comando"
      onClick={() => copyHint(hint)}
      onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); copyHint(hint); } }}>{hint}</code>
  );
}
function CliUpdateRow({ u }: { u: ProviderUpdate }) {
  const app = useApp();
  const prog = app.updateProgress[u.providerId];
  const running = prog?.status === 'running';
  // Failure surfaces from the live 'failed' snapshot, or — after a reload where
  // the progress event was missed — from the entry's replayed failure fields.
  const failed = prog?.status === 'failed' || (!prog && !!u.lastError);
  const errText = prog?.error ?? u.lastError;
  const exitCode = prog?.exitCode ?? u.exitCode;
  const tail = tailLine(running ? prog?.tail : (prog?.tail ?? u.tail));
  const canUpdate = has('providersUpdate');
  const connected = app.connection === 'connected';
  const disabled = running || !connected;

  return (
    <div className="lrow cli-lrow">
      <div className="ic mono">{(u.cli || '?').charAt(0)}</div>
      <div className="body">
        <div className="nm">{u.cli}{u.updateAvailable && <span className="badge acc">Actualización</span>}</div>
        <div className="mt">CLI {u.installed}{u.updateAvailable && <span className="upd-to"> → {u.latest}</span>}</div>
        {running && <div className="cli-prog mono">{tail || 'iniciando…'}</div>}
        {failed && !running && (
          <div className="cli-err">
            No se pudo actualizar{typeof exitCode === 'number' ? ` (código ${exitCode})` : ''}
            {errText ? ` · ${errText}` : ''}
            {tail && <span className="cli-err-tail mono"> — {tail}</span>}
          </div>
        )}
      </div>
      <div className="act cli-act">
        {u.updateAvailable && canUpdate ? (
          <>
            <button
              className="btn sm accq"
              disabled={disabled}
              title={connected ? undefined : 'Sin conexión con el daemon'}
              onClick={() => void store.updateProvider(u.providerId)}
            >
              {running ? <><span className="btnspin" aria-hidden="true" />Actualizando…</> : failed ? 'Reintentar' : 'Actualizar'}
            </button>
            {!running && u.hint && <CopyChip hint={u.hint} />}
          </>
        ) : u.updateAvailable && u.hint ? (
          // Older bridge without providers:update → the copy command stays primary.
          <CopyChip hint={u.hint} />
        ) : (
          <span className="stat"><span className="dot run" />al día</span>
        )}
      </div>
    </div>
  );
}

// ---- 1 · Agentes ---------------------------------------------------------
function providerStatusLabel(status: string): string {
  switch (status) {
    case 'ready': return 'Listo';
    case 'needs-login': return 'Requiere login';
    case 'not-found': return 'No instalado';
    case 'error': return 'Error';
    case 'inactive': return 'Inactivo';
    default: return status || 'Desconocido';
  }
}
function AgentProviderRow({ provider }: { provider: ProviderRecord }) {
  const app = useApp();
  const group = app.groups.find((g) => g.providerId === provider.id);
  const modelCount = group?.models?.length ?? 0;
  const ready = provider.status === 'ready';
  const retryable = provider.status === 'needs-login' || provider.status === 'error' || provider.status === 'inactive';
  const detail = provider.fixHint || provider.error || provider.message || provider.resolvedCommand || '—';
  return (
    <div className="lrow">
      <div className="ic mono">{(provider.name || provider.id || '?').charAt(0)}</div>
      <div className="body">
        <div className="nm">{provider.name || provider.id}{provider.badge && <span className="badge">{provider.badge}</span>}</div>
        <div className={`mt ${provider.status === 'needs-login' || provider.status === 'error' ? 'cli-err' : ''}`}>{detail}</div>
      </div>
      <div className="act">
        <span className="stat"><span className={`dot ${ready ? 'run' : ''}`} />{providerStatusLabel(provider.status)}</span>
        {modelCount > 0 && <span className="badge">{modelCount} {modelCount === 1 ? 'modelo' : 'modelos'}</span>}
        {retryable && has('providersDetect') && (
          <button className="btn sm" onClick={() => void store.detectProvider(provider.id)}>Comprobar de nuevo</button>
        )}
      </div>
    </div>
  );
}
function AgentesPanel() {
  const app = useApp();
  const updates = app.providersUpdates;
  const acp = app.daemonConfig?.acp;
  const provider = (acp?.provider || '').trim();
  const command = [acp?.command, ...(Array.isArray(acp?.args) ? (acp!.args as unknown[]).map(String) : [])]
    .filter(Boolean).join(' ');
  const name = provider ? provider.charAt(0).toUpperCase() + provider.slice(1) : 'Agente ACP';
  const connected = app.chats.some((c) => c.sessionId);
  const models = app.models.length;
  const canProbe = has('appChatDetectAcp');

  return (
    <section className="stgs-panel">
      <div className="phead">
        <h2>Agentes</h2>
        <p>workass se conecta a los agentes ACP instalados en tu máquina; cada CLI conserva su propia sesión y autenticación.</p>
      </div>

      <div className="gtitle">Agente conectado</div>
      {app.providers.length > 0 ? (
        <>
          <div className="group">
            {app.providers.map((p) => <AgentProviderRow key={p.id} provider={p} />)}
          </div>
          <div className="ghint">
            El login siempre se hace en el CLI oficial. Si un agente indica “Requiere login”, seguí la instrucción mostrada y después tocá <b>Comprobar de nuevo</b>.
          </div>
        </>
      ) : app.hasConfigChannel && acp ? (
        <>
          <div className="group">
            <div className="lrow">
              <div className="ic mono">{name.charAt(0)}</div>
              <div className="body">
                <div className="nm">{name}{provider === 'mock' && <span className="badge acc">Oráculo de pruebas</span>}</div>
                <div className="mt">{command || '—'}</div>
              </div>
              <div className="act">
                <span className="stat"><span className={`dot ${connected ? 'run' : ''}`} />{connected ? 'Conectado' : 'Configurado'}</span>
                {models > 0 && <span className="badge">{models} {models === 1 ? 'modelo' : 'modelos'}</span>}
                {canProbe && <button className="btn sm" onClick={() => void call('appChatDetectAcp', acp ? { command: acp.command, args: acp.args, provider: acp.provider } : {})}>Probar</button>}
              </div>
            </div>
          </div>
          <div className="ghint">Los modelos del agente aparecen en el selector del compositor.</div>
        </>
      ) : (
        <div className="group"><div className="lrow"><div className="body"><span className="empty">El daemon no expone la configuración de agentes (config:get).</span></div></div></div>
      )}

      {updates.length > 0 && (
        <>
          <div className="gtitle">Versiones de CLIs</div>
          <div className="group">
            {updates.map((u) => <CliUpdateRow key={u.providerId} u={u} />)}
          </div>
          <div className="ghint">
            workass compara la versión instalada de cada CLI con la última publicada. Tocá <b>Actualizar</b> y el daemon corre el
            actualizador oficial del CLI en el acto; el progreso aparece acá mismo. El comando queda como alternativa manual.
          </div>
        </>
      )}
    </section>
  );
}

// ---- 2 · Modelos ---------------------------------------------------------
// User-authored scoring: the user rates every catalog model against THEIR own
// priorities (Inteligencia / Gusto / Costo, 1–10; Costo higher = more expensive)
// so the future agent-facing catalog can route on those preferences. Neutral
// surfaces; the accent is a focus ring only. Scores persist through the daemon
// app-settings blob (settings:get / settings:set) — never localStorage, never
// the session mirror.
function ModelScoreCard({ providerId, model, score }: { providerId: string; model: ScoreableModel; score: ModelScore | undefined }) {
  const empty = isEmptyScore(score);
  return (
    <div className="mscore">
      <div className="mscore-head">
        <div className="mscore-nm">{model.name}<code className="mscore-id" title={model.modelId}>{model.modelId}</code></div>
        <button className="btn sm" disabled={empty} title={empty ? 'Sin puntuar' : 'Volver a sin puntuar'}
          onClick={() => store.resetModelScore(providerId, model.modelId)}>Restablecer</button>
      </div>
      <div className="mscore-grid">
        {SCORE_DIMENSIONS.map((d) => {
          const value = score?.[d.key];
          return (
            <label key={d.key} className="mscore-dim">
              <span className="mscore-dlabel">{d.label}{d.key === 'cost' && <span className="mscore-dcue"> · 10 = caro</span>}</span>
              <input
                className="inp num mscore-inp"
                type="number" inputMode="numeric"
                min={SCORE_MIN} max={SCORE_MAX} step={1}
                value={value === undefined ? '' : String(value)}
                placeholder="—"
                aria-label={`${d.label} — ${model.name} (${SCORE_MIN} a ${SCORE_MAX})`}
                onChange={(e) => store.setModelScore(providerId, model.modelId, d.key, e.target.value)}
              />
            </label>
          );
        })}
      </div>
      <input
        className="inp mscore-note"
        type="text" maxLength={NOTE_MAX}
        value={score?.note ?? ''}
        placeholder="Nota (opcional) — para qué te sirve este modelo, cuándo lo preferís…"
        aria-label={`Nota — ${model.name}`}
        onChange={(e) => store.setModelNote(providerId, model.modelId, e.target.value)}
        onBlur={(e) => store.setModelNote(providerId, model.modelId, normalizeNote(e.currentTarget.value) ?? '')}
      />
    </div>
  );
}
function ModelosPanel() {
  const app = useApp();
  const groups = groupModelsForScoring(app.groups, app.models, app.providers, app.meta?.profile ?? 'prod');
  const scored = countScoredModels(app.modelScores);
  return (
    <section className="stgs-panel">
      <div className="phead">
        <h2>Modelos</h2>
        <p>Puntuá cada modelo del catálogo según <b>tus</b> prioridades: <b>Inteligencia</b>, <b>Gusto</b> y <b>Costo</b>. Son preferencias tuyas —no afirmaciones del proveedor— y alimentan el catálogo que orientará a los agentes. Escala {SCORE_MIN}–{SCORE_MAX}; en <b>Costo</b>, {SCORE_MAX} = más caro. Dejá una casilla vacía para no puntuar esa dimensión.</p>
      </div>

      {groups.length === 0 ? (
        <div className="group"><div className="lrow"><div className="body"><span className="empty">Todavía no hay modelos en el catálogo. Conectá un agente y volvé acá.</span></div></div></div>
      ) : (
        <>
          <div className="gtitle">
            Puntajes
            <span className="gtag">{scored === 0 ? 'sin puntuar' : `${scored} ${scored === 1 ? 'modelo puntuado' : 'modelos puntuados'}`}</span>
            <button className="btn sm stg-reset" disabled={scored === 0}
              onClick={() => store.resetAllModelScores()}>Restablecer todo</button>
          </div>
          {groups.map((g) => (
            <div key={g.providerId || g.providerName} className="mscore-provider">
              <div className="gtitle mscore-gtitle">{g.providerName}<span className="gtag">{g.models.length} {g.models.length === 1 ? 'modelo' : 'modelos'}</span></div>
              <div className="group mscore-group">
                {g.models.map((m) => (
                  <ModelScoreCard key={m.modelId} providerId={g.providerId} model={m}
                    score={getModelScore(app.modelScores, g.providerId, m.modelId)} />
                ))}
              </div>
            </div>
          ))}
          <div className="ghint">Los puntajes se guardan en la configuración del daemon (settings) —la misma fuente que consultará el catálogo de los agentes—, no en este dispositivo. <b>Restablecer</b> borra el puntaje de un modelo; <b>Restablecer todo</b> los borra todos.</div>
        </>
      )}
    </section>
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
        <div className="phead"><h2>Acceso aprobado</h2><p>Clientes que ya pueden abrir esta Workass.</p></div>
        <div className="group"><div className="lrow"><div className="body"><span className="empty">Este bridge no expone los canales de dispositivos (lan:devices).</span></div></div></div>
      </section>
    );
  }
  const { devices } = app;
  return (
    <section className="stgs-panel">
      <div className="phead">
        <h2>Acceso aprobado</h2>
        <p>Clientes que ya pueden abrir esta Workass. Las solicitudes nuevas se aceptan desde <b>Conectar Workass</b>; acá solo administrás quién conserva acceso.</p>
      </div>

      <div className="gtitle">Clientes con acceso</div>
      <div className="group">
        {devices.length === 0
          ? <div className="lrow"><div className="body"><span className="empty">Sin dispositivos aprobados todavía.</span></div></div>
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
  const spare = app.daemonConfig?.chat?.spareSessions;
  return (
    <section className="stgs-panel">
      <div className="phead">
        <h2>Engines</h2>
        <p>Ciclo de vida de los motores ACP: hibernación de chats inactivos, reciclado por memoria y una reserva templada para arranques instantáneos.</p>
      </div>

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
        <EngineDefault n="Reserva templada" d="Engines pre-calentados listos para el próximo chat sin costo de arranque frío." value={String(spare ?? 0)} suf={(spare ?? 0) === 1 ? 'engine' : 'engines'} />
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
      <div className="phead">
        <h2>Apariencia</h2>
        <p>Tema, densidad y acento. workass sigue el lenguaje visual moderno-minimal de las apps de Claude Code y Codex.</p>
      </div>

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
      <div className="ghint">Los avisos cubren actualizaciones de agentes, notificaciones explícitas del agente y solicitudes de permisos. Terminar un turno no genera ningún aviso.</div>
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
      <div className="phead">
        <h2>Atajos</h2>
        <p>Teclas rápidas de workass. Se muestran junto a sus acciones en toda la interfaz.</p>
      </div>
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
// One visible ceremony only: discovery finds the other Workass, this client
// requests access, and the controller on the other machine approves it. The
// compatibility wire still understands manual/fleet enrolment, but ordinary
// users never need to choose between those implementation paths.
function MaquinasPanel() {
  const app = useApp();
  const paired = app.machines.filter((machine) => machine.paired);
  const nearby = app.machines.filter((machine) => !machine.paired);
  const incoming = app.accessRequests;

  if (!app.hasMachineChannels) {
    return (
      <section className="stgs-panel">
        <div className="phead"><h2>Conectar Workass</h2><p>Tus daemons, en una sola lista de conversaciones.</p></div>
        <div className="group"><div className="lrow"><div className="body">
          <span className="empty">Este daemon todavía no tiene libreta de máquinas (machines:list).</span>
        </div></div></div>
      </section>
    );
  }

  return (
    <section className="stgs-panel">
      <div className="phead">
        <h2>Conectar Workass</h2>
        <p>Una sola forma de vincularlas: Workass las descubre en la red, enviás la solicitud y la aprobás en la otra máquina. Sin direcciones, PIN ni claves.</p>
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
  return (
    <div className="lrow">
      <div className="ic"><Svg><rect x="1.8" y="3" width="12.4" height="8" rx="1.6" /><path d="M5.5 13.5h5M8 11v2.5" /></Svg></div>
      <div className="body">
        <div className="nm"><span className={`dot ${dot}`} />{m.name}
          {!m.secure && <span className="badge">insegura</span>}</div>
        <div className="mt">{m.address || '—'}{m.reason ? ` · ${m.reason}` : (live ? ' · conectada' : m.requested ? ' · esperando aprobación' : ' · disponible en la red')}</div>
      </div>
      <div className="act">
		{!m.paired && !m.requested && <button className="btn sm acc" disabled={!m.secure || !m.reachable} onClick={() => void store.requestMachineAccess(m.machineId)}>Solicitar conexión</button>}
        {!m.paired && m.requested && <span className="badge">solicitud enviada</span>}
        {m.paired && <button className="btn sm danger" onClick={() => void store.forgetMachine(m.machineId)}>Desconectar</button>}
      </div>
    </div>
  );
}

// ---- shell ---------------------------------------------------------------
const PANELS: Record<SettingsSection, () => ReactNode> = {
  agentes: AgentesPanel, modelos: ModelosPanel, maquinas: MaquinasPanel, dispositivos: DispositivosPanel,
  engines: EnginesPanel, apariencia: AparienciaPanel, atajos: AtajosPanel,
};
const ORDER: SettingsSection[] = ['agentes', 'modelos', 'maquinas', 'dispositivos', 'engines', 'apariencia', 'atajos'];

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
          <span className="crumb">Ajustes / <b>{SECTION_NAME[sec]}</b></span>
          <span className="sp" />
          <span className="tclose">Cerrar <kbd>Esc</kbd></span>
          <button className="tico" title="Volver a los chats" onClick={() => store.closeSettings()}>
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
