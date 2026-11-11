// Ajustes · Conectores (owner 2026-11-10, desktop/docs/mocks/connectors-alerts/
// mock.html section 1 with the owner's amendments; docs/specs/connectors-alerts.md).
// A connector brings alerts in from outside, and nothing more: no option here
// makes an agent act by itself. Grafana first; any Grafana, any rule.
//
// Two panes, like Agentes y modelos: every connector of every connected
// machine on the left (the one that listens is chosen when adding), the
// chosen one on the right. Flat rows; a control that cannot act says why.

import { useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import { store, useApp } from '../../store/store';
import type { Connector, ConnectorNotify, ConnectorRule, ConnectorSetArgs, ConnectorTarget, ConnectorTestResult } from '../../wire/types';
import {
  connectorEntries, listeningMachines, removeConnector, rotateSecret, saveConnector, setArgsOf, testConnector,
  useAlertsRefresh, webhookURL, alertEntries, moveConnectorTo, type ConnectorEntry,
} from '../../alerts';
import { agoText, isFiring, moveFailureText, severityOf, shortFolder } from '../../alerts-model';
import { buildWorkspaceGroups, normalizeWorkspacePath } from '../../workspaces';

const SEV_CLASS = { critical: 'c', warning: 'w', info: 'i' } as const;
const ADD = '+add';

function Row({ name, detail, children, className }: { name: ReactNode; detail?: ReactNode; children?: ReactNode; className?: string }) {
  return (
    <div className={`set${className ? ` ${className}` : ''}`}>
      <div className="lbl">
        <div className="n">{name}</div>
        {detail && <div className="d">{detail}</div>}
      </div>
      {children && <div className="ctl">{children}</div>}
    </div>
  );
}

function Toggle({ on, label, onChange, disabled }: { on: boolean; label: string; onChange: (on: boolean) => void; disabled?: boolean }) {
  return (
    <button className={`toggle ${on ? 'on' : ''}`} role="switch" aria-checked={on} aria-label={label} disabled={disabled} onClick={() => onChange(!on)}>
      <span className="knob" />
    </button>
  );
}

function Seg<T extends string>({ value, options, onChange, label, disabled }: {
  value: T; options: Array<[T, string]>; onChange: (v: T) => void; label: string; disabled?: boolean;
}) {
  return (
    <div className="seg" role="group" aria-label={label}>
      {options.map(([v, text]) => (
        <button key={v} className={v === value ? 'on' : ''} disabled={disabled} onClick={() => { if (v !== value) onChange(v); }}>{text}</button>
      ))}
    </div>
  );
}

/** A text field that saves on Enter or when it loses focus. */
function Field({ value, onSave, placeholder, label, mono = true, type = 'text', width }: {
  value: string; onSave: (v: string) => void; placeholder?: string; label: string; mono?: boolean; type?: string; width?: number;
}) {
  const [text, setText] = useState(value);
  useEffect(() => { setText(value); }, [value]);
  const commit = () => { if (text.trim() !== value) onSave(text.trim()); };
  return (
    <input className={`inp${mono ? ' mono' : ''}`} type={type} value={text} placeholder={placeholder} aria-label={label}
      spellCheck={false} style={width ? { width } : undefined}
      onChange={(e) => setText(e.target.value)} onBlur={commit}
      onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); commit(); } if (e.key === 'Escape') setText(value); }} />
  );
}

function IcGrafana() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.3" aria-hidden="true">
      <circle cx="8" cy="8" r="5.5" /><circle cx="8" cy="8" r="2" /><path d="M8 2.5V1M13.5 8H15" />
    </svg>
  );
}
function IcPlus() {
  return <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true"><path d="M8 3v10M3 8h10" /></svg>;
}

function copy(text: string, what: string) {
  try {
    const p = navigator.clipboard?.writeText(text);
    if (p && typeof p.then === 'function') { p.then(() => store.addToast(`${what} copiado`, '')).catch(() => store.addToast(what, text)); return; }
  } catch { /* falls through */ }
  store.addToast(what, text);
}

function errText(err: unknown): string {
  return String((err as Error)?.message || err);
}

// ---- machines ------------------------------------------------------------------

/** The machine book, for where a chat opens: this one and every paired one. */
function bookMachines(): Array<{ id: string; name: string; ready: boolean }> {
  const self = store.localMachineId();
  return [
    { id: self, name: store.localMachineName(), ready: true },
    ...store.state.machines.filter((m) => m.paired && m.machineId !== self)
      .map((m) => ({ id: m.machineId, name: m.name || m.reportedName || m.machineId, ready: m.link === 'ready' })),
  ];
}

function sameMachine(a: string, b: string): boolean {
  const self = store.localMachineId();
  const norm = (x: string) => (!x || x === self ? '' : x);
  return norm(a) === norm(b);
}

function knownFolders(machineId: string): string[] {
  const app = store.state;
  const self = store.localMachineId();
  const key = !machineId || machineId === self ? '' : machineId;
  return buildWorkspaceGroups(app.workspaces, app.chats, app.removedWorkspaces, app.deferredChats?.cwds ?? [])
    .filter((g) => (g.machineId ?? '') === key && normalizeWorkspacePath(g.path)).map((g) => g.path);
}

function TargetControls({ target, onChange, label }: { target: ConnectorTarget; onChange: (t: ConnectorTarget) => void; label: string }) {
  const machines = bookMachines();
  const current = machines.find((m) => sameMachine(m.id, target.machineId));
  const listId = `cn-folders-${label.replace(/\W+/g, '-')}`;
  return (
    <>
      <select className="inp" aria-label={`Máquina · ${label}`} value={current?.id ?? target.machineId}
        onChange={(e) => onChange({ machineId: e.target.value, cwd: sameMachine(e.target.value, target.machineId) ? target.cwd : '' })}>
        {machines.map((m) => <option key={m.id || 'local'} value={m.id}>{m.name}{m.ready ? '' : ' · sin conexión'}</option>)}
        {!current && target.machineId && <option value={target.machineId}>{target.machineId}</option>}
      </select>
      <input className="inp mono cn-folder" list={listId} defaultValue={target.cwd} key={`${target.machineId}|${target.cwd}`}
        placeholder="carpeta del chat" aria-label={`Carpeta · ${label}`} spellCheck={false} title={target.cwd}
        onBlur={(e) => { const v = e.target.value.trim(); if (v !== target.cwd) onChange({ ...target, cwd: v }); }}
        onKeyDown={(e) => { if (e.key === 'Enter') (e.target as HTMLInputElement).blur(); }} />
      <datalist id={listId}>{knownFolders(target.machineId).map((p) => <option key={p} value={p}>{shortFolder(p)}</option>)}</datalist>
    </>
  );
}

// ---- the page ------------------------------------------------------------------------

function statusLine(entry: ConnectorEntry, now: Date, showMachine: boolean): { tone: 'ok' | 'err' | 'idle'; text: string } {
  const c = entry.connector;
  const st = c.status?.state ?? 'idle';
  const last = c.status?.lastOkAt ? new Date(c.status.lastOkAt) : null;
  const when = last && !Number.isNaN(last.getTime()) ? ` · ${agoText(last, now)}` : '';
  const where = showMachine ? ` · ${entry.machineName}` : '';
  if (st === 'error') return { tone: 'err', text: `${c.status?.error || 'no responde'}${where}` };
  if (st === 'ok') return { tone: 'ok', text: `${c.mode === 'webhook' ? 'recibe' : 'conectado'}${when}${where}` };
  return { tone: 'idle', text: `${c.mode === 'webhook' ? 'esperando a Grafana' : 'todavía sin consultar'}${where}` };
}

export function ConectoresPage() {
  useApp();
  useAlertsRefresh();
  const [now, setNow] = useState(() => new Date());
  useEffect(() => { const t = window.setInterval(() => setNow(new Date()), 15_000); return () => window.clearInterval(t); }, []);
  const entries = connectorEntries().sort((a, b) => a.connector.name.localeCompare(b.connector.name));
  const listening = listeningMachines();
  const [selected, setSelected] = useState('');
  // A secret the daemon shows once (first webhook save or a rotation).
  const [secret, setSecret] = useState<{ key: string; value: string } | null>(null);
  const keyOf = (e: ConnectorEntry) => `${e.machineKey}\u0000${e.connector.id}`;
  const chosen = selected === ADD ? null : entries.find((e) => keyOf(e) === selected) ?? entries[0] ?? null;
  const adding = selected === ADD || (!chosen && listening.length > 0);
  const showMachine = new Set(entries.map((e) => e.machineKey)).size > 1 || listening.length > 1;

  if (listening.length === 0 && entries.length === 0) {
    return (
      <section className="stgs-panel">
        <div className="group"><div className="lrow"><div className="body">
          <span className="empty">Ninguna máquina conectada tiene conectores todavía (connectors:list). Actualizá Workass en la máquina que va a escuchar a Grafana.</span>
        </div></div></div>
      </section>
    );
  }

  return (
    <section className="stgs-panel">
      <div className="ag-two cn-two">
        <div className="ag-list" role="list" aria-label="Conectores">
          {entries.map((e) => {
            const st = statusLine(e, now, showMachine);
            const on = !adding && chosen && keyOf(chosen) === keyOf(e);
            return (
              <div key={keyOf(e)} role="listitem" tabIndex={0} className={`ag-item cn-item${on ? ' on' : ''}`}
                onClick={() => setSelected(keyOf(e))}
                onKeyDown={(ev) => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); setSelected(keyOf(e)); } }}>
                <span className="cn-logo"><IcGrafana /></span>
                <span className="ag-tx">
                  <span className="ag-n">Grafana · {e.connector.name}</span>
                  <span className="ag-s"><span className={`cn-st ${st.tone}`}>●</span> {st.text}</span>
                </span>
              </div>
            );
          })}
          <div role="listitem" tabIndex={0} className={`ag-item cn-item cn-add${adding ? ' on' : ''}`}
            title={listening.length ? undefined : 'Ninguna máquina conectada tiene conectores.'}
            onClick={() => { if (listening.length) setSelected(ADD); }}
            onKeyDown={(ev) => { if ((ev.key === 'Enter' || ev.key === ' ') && listening.length) { ev.preventDefault(); setSelected(ADD); } }}>
            <span className="cn-logo"><IcPlus /></span>
            <span className="ag-tx"><span className="ag-n">Añadir conector</span><span className="ag-s">Grafana</span></span>
          </div>
        </div>
        {adding
          ? <AddConnector key="add" listening={listening} onAdded={(machineKey, c, webhookSecret) => {
              const key = `${machineKey}\u0000${c.id}`;
              setSelected(key);
              if (webhookSecret) setSecret({ key, value: webhookSecret });
            }} />
          : chosen && <ConnectorDetail key={keyOf(chosen)} entry={chosen}
              secret={secret?.key === keyOf(chosen) ? secret.value : ''}
              onSecret={(value) => setSecret({ key: keyOf(chosen), value })}
              onRemoved={() => setSelected('')}
              onMoved={(machineKey) => setSelected(`${machineKey}\u0000${chosen.connector.id}`)} />}
      </div>
    </section>
  );
}

// ---- adding ------------------------------------------------------------------------------

function AddConnector({ listening, onAdded }: {
  listening: Array<{ machineKey: string; machineName: string }>;
  onAdded: (machineKey: string, c: Connector, webhookSecret?: string) => void;
}) {
  const [machineKey, setMachineKey] = useState(listening[0]?.machineKey ?? '');
  const [name, setName] = useState('');
  const [url, setUrl] = useState('');
  const [mode, setMode] = useState<'poll' | 'webhook'>('poll');
  const [token, setToken] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [test, setTest] = useState<ConnectorTestResult | null>(null);
  const why = !name.trim() ? 'Falta el nombre.' : !/^https?:\/\/\S+$/.test(url.trim()) ? 'La dirección empieza con http:// o https://.' : '';
  const add = async () => {
    if (busy || why) return;
    setBusy(true);
    setError('');
    try {
      const args: ConnectorSetArgs = {
        name: name.trim(), url: url.trim(), mode, notify: 'critical',
        target: { machineId: store.localMachineId(), cwd: '' }, ruleTargets: {}, guidePattern: '',
        ...(mode === 'poll' && token.trim() ? { token: token.trim() } : {}),
      };
      const reply = await saveConnector(machineKey, args);
      if (reply?.connector) onAdded(machineKey, reply.connector, reply.webhookSecret);
      else setError('El daemon no devolvió el conector.');
    } catch (err) {
      setError(`No se pudo añadir: ${errText(err)}`);
    } finally {
      setBusy(false);
    }
  };
  const probe = async () => {
    setTest(null);
    try { setTest(await testConnector(machineKey, { url: url.trim(), token: token.trim() })); }
    catch (err) { setTest({ ok: false, error: errText(err) }); }
  };
  return (
    <div className="ag-det cn-det">
      <div className="cn-ttl"><span>Nuevo conector · Grafana</span></div>
      <div className="group">
        <Row name="Máquina que escucha" detail="La que habla con Grafana. Las alertas se ven en todos tus clientes.">
          <select className="inp" aria-label="Máquina que escucha" value={machineKey} onChange={(e) => setMachineKey(e.target.value)}>
            {listening.map((m) => <option key={m.machineKey || 'local'} value={m.machineKey}>{m.machineName}</option>)}
          </select>
        </Row>
        <Row name="Nombre" detail="Cómo se llama este Grafana en Workass.">
          <input className="inp" value={name} placeholder="fiumba" aria-label="Nombre" spellCheck={false} onChange={(e) => setName(e.target.value)} />
        </Row>
        <Row name="Dirección" detail={test ? <TestLine test={test} /> : 'El Grafana que Workass lee.'}>
          <input className="inp mono" value={url} placeholder="http://127.0.0.1:3000" aria-label="Dirección" spellCheck={false} onChange={(e) => { setUrl(e.target.value); setTest(null); }} />
          <button className="btn sm" disabled={!!why && !url.trim()} title={url.trim() ? undefined : 'Escribí la dirección primero.'} onClick={() => void probe()}>Probar</button>
        </Row>
        <Row name="Cómo llegan" detail="Consultar: Workass pregunta a Grafana cada 30 s, sin tocar Grafana. Recibir: Grafana empuja a un punto de contacto que Workass te da.">
          <Seg label="Cómo llegan" value={mode} options={[['poll', 'Consultar'], ['webhook', 'Recibir']]} onChange={setMode} />
        </Row>
        {mode === 'poll' && (
          <Row name="Token de cuenta de servicio" detail="Opcional si un proxy ya autentica. Solo lectura (rol Viewer); se guarda y no se vuelve a mostrar.">
            <input className="inp mono" type="password" value={token} placeholder="glsa_…" aria-label="Token" autoComplete="off" onChange={(e) => setToken(e.target.value)} />
          </Row>
        )}
        <Row name={<span className="cn-addrow">Añadir</span>} detail={error ? <span className="cn-err">{error}</span> : (why || (mode === 'webhook' ? 'Después te muestra la dirección y el secreto para Grafana.' : 'Empieza a consultar enseguida.'))}>
          <button className="btn acc" disabled={busy || !!why} title={why || undefined} onClick={() => void add()}>{busy ? 'Añadiendo…' : 'Añadir'}</button>
        </Row>
      </div>
    </div>
  );
}

function TestLine({ test }: { test: ConnectorTestResult }) {
  if (!test.ok) return <span className="cn-err">No responde: {test.error || 'sin detalle'}</span>;
  const rules = test.rules ?? 0;
  const firing = test.firing ?? 0;
  return <span className="cn-ok">Responde: {rules} regla{rules === 1 ? '' : 's'}, {firing} disparando.</span>;
}

// ---- one connector ------------------------------------------------------------------------

// "Máquina que escucha" (owner 2026-11-10): one click moves the connector to
// another connected machine. Picking one shows a single confirm row with the
// address as that machine reaches Grafana (another machine may need another
// one) and, for "Recibir", the new contact point Grafana must use.
function MoveRow({ entry, onMoved }: { entry: ConnectorEntry; onMoved: (machineKey: string) => void }) {
  const c = entry.connector;
  const others = listeningMachines().filter((m) => m.machineKey !== entry.machineKey);
  const [to, setTo] = useState<string | null>(null);
  const [url, setUrl] = useState(c.url);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const toName = others.find((m) => m.machineKey === to)?.machineName ?? '';
  const why = others.length ? '' : 'No hay otra máquina conectada con conectores.';
  const move = async () => {
    if (to === null || busy) return;
    setBusy(true);
    setError('');
    const result = await moveConnectorTo(entry.machineKey, to, c.id, url);
    setBusy(false);
    if (result.ok) { setTo(null); onMoved(to); return; }
    setError(moveFailureText(result, entry.machineName, toName));
    if (result.stage === 'remove') onMoved(to);
  };
  const newURL = to !== null ? webhookURL(to, c) : '';
  return (
    <>
      <Row name="Máquina que escucha" detail={error ? <span className="cn-err" role="alert">{error}</span> : 'La que habla con Grafana. Las alertas se ven en todos tus clientes.'}>
        <span className="cn-fld">{entry.machineName}</span>
        <select className="inp cn-change" aria-label="Cambiar la máquina que escucha" value="" disabled={!!why || busy} title={why || undefined}
          onChange={(e) => { if (e.target.value !== '') { setTo(e.target.value === '\u0000local' ? '' : e.target.value); setUrl(c.url); setError(''); } }}>
          <option value="" disabled>Cambiar</option>
          {others.map((m) => <option key={m.machineKey || 'local'} value={m.machineKey || '\u0000local'}>{m.machineName}</option>)}
        </select>
      </Row>
      {to !== null && (
        <Row className="cn-over cn-move" name={`Dirección desde ${toName}`}
          detail={c.mode === 'webhook'
            ? <>Grafana tiene que mandar al punto de contacto nuevo: <span className="mono">{newURL}</span> <button className="cn-link" onClick={() => copy(newURL, 'Dirección')}>Copiar</button></>
            : 'Puede ser otra: esa máquina llega a Grafana por su propio camino.'}>
          <input className="inp mono" value={url} aria-label={`Dirección desde ${toName}`} spellCheck={false} onChange={(e) => setUrl(e.target.value)} />
          <button className="btn sm acc" disabled={busy || !url.trim()} title={url.trim() ? undefined : 'Escribí la dirección.'} onClick={() => void move()}>{busy ? 'Moviendo…' : 'Mover'}</button>
          <button className="btn sm" disabled={busy} onClick={() => setTo(null)}>Cancelar</button>
        </Row>
      )}
    </>
  );
}

function ConnectorDetail({ entry, secret, onSecret, onRemoved, onMoved }: {
  entry: ConnectorEntry; secret: string; onSecret: (value: string) => void; onRemoved: () => void; onMoved: (machineKey: string) => void;
}) {
  const app = useApp();
  const c = entry.connector;
  const [error, setError] = useState('');
  const [test, setTest] = useState<ConnectorTestResult | null>(null);
  const [editingToken, setEditingToken] = useState(false);
  const [token, setToken] = useState('');
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [adding, setAdding] = useState(false);
  useEffect(() => {
    if (!confirmRemove) return undefined;
    const t = window.setTimeout(() => setConfirmRemove(false), 4000);
    return () => window.clearTimeout(t);
  }, [confirmRemove]);

  const save = async (patch: Partial<ConnectorSetArgs>) => {
    setError('');
    try {
      const reply = await saveConnector(entry.machineKey, setArgsOf(c, patch));
      if (reply?.webhookSecret) onSecret(reply.webhookSecret);
    } catch (err) {
      setError(`No se pudo guardar: ${errText(err)}`);
    }
  };
  const probe = async () => {
    setTest(null);
    try { setTest(await testConnector(entry.machineKey, { id: c.id })); }
    catch (err) { setTest({ ok: false, error: errText(err) }); }
  };
  const rotate = async () => {
    try { const value = await rotateSecret(entry.machineKey, c.id); if (value) onSecret(value); }
    catch (err) { setError(`No se pudo rotar: ${errText(err)}`); }
  };
  const remove = async () => {
    if (!confirmRemove) { setConfirmRemove(true); return; }
    try { await removeConnector(entry.machineKey, c.id); onRemoved(); }
    catch (err) { setError(`No se pudo quitar: ${errText(err)}`); }
  };

  const rules = c.rules ?? [];
  const shownRules = rules.filter((r) => r.enabled).length;
  const active = alertEntries().filter((e) => e.machineKey === entry.machineKey && e.alert.connectorId === c.id && isFiring(e.alert)).length;
  const toggleRule = (rule: ConnectorRule, enabled: boolean) =>
    void save({ rules: rules.map((r) => ({ key: r.key, enabled: r.key === rule.key ? enabled : r.enabled })) });
  const target = c.target ?? { machineId: '', cwd: '' };
  const overrides = Object.entries(c.ruleTargets ?? {});
  const ruleByKey = new Map(rules.map((r) => [r.key, r]));
  const notOverridden = rules.filter((r) => !(c.ruleTargets ?? {})[r.key]);
  const setOverride = (key: string, t: ConnectorTarget | null) => {
    const next = { ...(c.ruleTargets ?? {}) };
    if (t) next[key] = t; else delete next[key];
    void save({ ruleTargets: next });
  };
  const notifOff = !app.notifEnabled || app.notifPermission !== 'granted';

  return (
    <div className="ag-det cn-det">
      <div className="cn-ttl">
        <span>Grafana · {c.name}</span>
        {active > 0 && <span className="cn-pill">{active} activa{active === 1 ? '' : 's'}</span>}
        <button className={`btn q cn-remove${confirmRemove ? ' armed' : ''}`} onClick={() => void remove()}>{confirmRemove ? '¿Quitar? Tocá de nuevo' : 'Quitar'}</button>
      </div>
      {error && <div className="cn-err cn-errline" role="alert">{error}</div>}
      <div className="group cn-first">
        <Row name="Dirección" detail={test ? <TestLine test={test} /> : c.status?.state === 'error' && c.status.error ? <span className="cn-err">{c.status.error}</span> : 'El Grafana que Workass lee.'}>
          <Field value={c.url} label="Dirección" placeholder="http://127.0.0.1:3000" onSave={(url) => void save({ url })} />
          <button className="btn sm" onClick={() => void probe()}>Probar</button>
        </Row>
        <Row name="Cómo llegan" detail="Consultar: Workass pregunta a Grafana cada 30 s, sin tocar Grafana. Recibir: Grafana empuja a un punto de contacto que Workass te da.">
          <Seg label="Cómo llegan" value={c.mode} options={[['poll', 'Consultar'], ['webhook', 'Recibir']]} onChange={(mode) => void save({ mode })} />
        </Row>
        {c.mode === 'poll' && (
          <Row name="Token de cuenta de servicio" detail="Opcional si un proxy ya autentica. Solo lectura (rol Viewer); se guarda y no se vuelve a mostrar.">
            {editingToken ? (
              <>
                <input className="inp mono" type="password" value={token} placeholder="glsa_…" aria-label="Token nuevo" autoComplete="off" autoFocus
                  onChange={(e) => setToken(e.target.value)}
                  onKeyDown={(e) => { if (e.key === 'Escape') { setEditingToken(false); setToken(''); } }} />
                <button className="btn sm acc" disabled={!token.trim()} title={token.trim() ? undefined : 'Pegá el token primero.'}
                  onClick={() => { void save({ token: token.trim() }); setEditingToken(false); setToken(''); }}>Guardar</button>
                <button className="btn sm" onClick={() => { setEditingToken(false); setToken(''); }}>Cancelar</button>
              </>
            ) : (
              <>
                <span className="cn-fld mono">{c.hasToken ? `•••••••• ${c.tokenHint ?? ''}`.trim() : 'sin token'}</span>
                <button className="btn sm" onClick={() => setEditingToken(true)}>{c.hasToken ? 'Cambiar' : 'Poner'}</button>
              </>
            )}
          </Row>
        )}
        {c.mode === 'webhook' && (
          <>
            <Row name="Punto de contacto" detail="En Grafana: un punto de contacto Webhook con esta dirección, y el encabezado Authorization: Bearer y el secreto.">
              <span className="cn-fld mono cn-url" title={webhookURL(entry.machineKey, c)}>{webhookURL(entry.machineKey, c)}</span>
              <button className="btn sm" onClick={() => copy(webhookURL(entry.machineKey, c), 'Dirección')}>Copiar</button>
            </Row>
            <Row name="Secreto" detail={secret ? 'Se muestra esta vez sola: copialo ahora.' : 'Guardado. Rotarlo deja de aceptar el anterior.'}>
              {secret
                ? <><span className="cn-fld mono cn-url" title={secret}>{secret}</span><button className="btn sm acc" onClick={() => copy(secret, 'Secreto')}>Copiar</button></>
                : <span className="cn-fld mono">{c.hasWebhookSecret ? '••••••••' : 'sin secreto'}</span>}
              <button className="btn sm" onClick={() => void rotate()}>{c.hasWebhookSecret || secret ? 'Rotar' : 'Crear'}</button>
            </Row>
          </>
        )}
        <MoveRow entry={entry} onMoved={onMoved} />
      </div>

      <div className="gtitle">Reglas <span className="gtag cn-r">{rules.length ? `${shownRules} de ${rules.length} se muestran` : ''}</span></div>
      <div className="group">
        {rules.length === 0 && (
          <div className="set"><div className="lbl"><div className="d">
            {c.mode === 'webhook' ? 'Las reglas aparecen cuando Grafana manda su primera alerta.' : 'Las reglas aparecen con la primera consulta.'}
          </div></div></div>
        )}
        {rules.map((rule) => (
          <div key={rule.key} className="set cn-rule">
            <span className={`cn-sev ${SEV_CLASS[severityOf({ severity: rule.severity ?? 'warning' })]}`} aria-hidden="true" />
            <div className="lbl"><div className="n">{rule.title}</div>{rule.folder && <div className="d">{rule.folder}</div>}</div>
            <div className="ctl"><Toggle on={rule.enabled} label={`Mostrar ${rule.title}`} onChange={(on) => toggleRule(rule, on)} /></div>
          </div>
        ))}
      </div>

      <div className="gtitle">Avisos</div>
      <div className="group">
        <Row name="Notificación del sistema"
          detail={<>Un aviso por alerta nueva. Las repeticiones de Grafana no avisan.{notifOff && <> Los avisos de escritorio están apagados en <button className="cn-link" onClick={() => store.setSettingsSection('general')}>Ajustes · General</button>.</>}</>}>
          <Seg<ConnectorNotify> label="Notificación del sistema" value={c.notify ?? 'critical'}
            options={[['never', 'Nunca'], ['critical', 'Críticas'], ['all', 'Todas']]} onChange={(notify) => void save({ notify })} />
        </Row>
      </div>

      <div className="gtitle">Al dárselas a un agente</div>
      <div className="group">
        <Row name="Dónde se abre el chat" detail="Máquina y carpeta del chat nuevo. El agente es el de Ajustes · General.">
          <TargetControls label="conector" target={target} onChange={(t) => void save({ target: t })} />
        </Row>
        <Row name="Por regla" detail="Una regla puede abrir en otro lado. Si no, usa la de arriba." />
        {overrides.map(([key, t]) => {
          const rule = ruleByKey.get(key);
          return (
            <div key={key} className="set cn-rule cn-over">
              <span className={`cn-sev ${SEV_CLASS[severityOf({ severity: rule?.severity ?? 'warning' })]}`} aria-hidden="true" />
              <div className="lbl"><div className="n">{rule?.title ?? key.replace(/^title:/, '')}</div></div>
              <div className="ctl">
                <TargetControls label={rule?.title ?? key} target={t} onChange={(next) => setOverride(key, next)} />
                <button className="btn q cn-reset" title="Usar la de arriba" aria-label="Usar la de arriba" onClick={() => setOverride(key, null)}>↺</button>
              </div>
            </div>
          );
        })}
        <div className="set cn-rule cn-over cn-addrule">
          <span className="cn-sev none" aria-hidden="true" />
          <div className="lbl">
            {adding && notOverridden.length ? (
              <select className="inp" autoFocus aria-label="Regla" defaultValue=""
                onChange={(e) => { if (e.target.value) setOverride(e.target.value, { ...target }); setAdding(false); }}
                onBlur={() => setAdding(false)}>
                <option value="" disabled>Elegí una regla…</option>
                {notOverridden.map((r) => <option key={r.key} value={r.key}>{r.title}</option>)}
              </select>
            ) : (
              <button className="cn-link mut" disabled={notOverridden.length === 0}
                title={notOverridden.length ? undefined : rules.length ? 'Todas las reglas tienen su lugar.' : 'Todavía no hay reglas.'}
                onClick={() => setAdding(true)}>Añadir una regla…</button>
            )}
          </div>
        </div>
        <Row name="Guía por regla" detail={<>Un archivo tuyo que se adjunta al mensaje, si existe. <span className="mono">&lt;regla&gt;</span> es la etiqueta alert_family, o el nombre de la regla.</>}>
          <Field value={c.guidePattern ?? ''} label="Guía por regla" placeholder="ops/runbooks/<regla>.md" onSave={(guidePattern) => void save({ guidePattern })} />
        </Row>
      </div>
    </div>
  );
}
