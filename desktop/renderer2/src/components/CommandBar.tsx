// ⌘, command box. Built after a daemon promotion left the app running but not
// the controller — no models, every send refused — with no way out except a
// terminal. The first command in the list is the way out; everything else is an
// action that already existed elsewhere in the UI, newly reachable by keyboard.
//
// Deliberately dumb: no async load, no capability probe, no dependency on being
// hydrated or being the controller. A recovery surface that needs the daemon to
// be healthy is not a recovery surface.

import { useEffect, useMemo, useRef, useState } from 'react';
import { store, useApp } from '../store/store';
import { filterCommands, forceReconnect, type Command } from '../store/commands';

export function CommandBar() {
  const app = useApp();
  const open = app.commandBarOpen;
  const [q, setQ] = useState('');
  const [sel, setSel] = useState(0);
  const [busy, setBusy] = useState('');
  const inputRef = useRef<HTMLInputElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);

  const commands = useMemo<Command[]>(() => [
    {
      id: 'reload',
      title: 'Recargar y reconectar',
      detail: 'Rehace la conexión con el daemon, recupera el control y recarga la ventana.',
      keywords: 'reload reconectar reconnect refrescar refresh arreglar reparar control controller atascado colgado stuck',
      run: async () => {
        setBusy('Reconectando…');
        await forceReconnect();
      },
    },
    {
      id: 'settings',
      title: 'Ajustes',
      keywords: 'settings preferencias configuracion',
      run: () => { store.closeCommandBar(); store.openSettings(); },
    },
    {
      id: 'machines',
      title: 'Máquinas',
      detail: 'Agregar una máquina por dirección, o pegar la clave de la flota.',
      keywords: 'machines remoto remote builder flota fleet emparejar clave key',
      run: () => { store.closeCommandBar(); store.openSettings('maquinas'); },
    },
    {
      id: 'devices',
      title: 'Dispositivos',
      detail: 'Qué clientes tienen acceso a este daemon, y revocarlos.',
      keywords: 'devices telefono phone acceso revocar',
      run: () => { store.closeCommandBar(); store.openSettings('dispositivos'); },
    },
    {
      id: 'new-chat',
      title: 'Nueva conversación',
      keywords: 'new chat nuevo crear',
      run: () => { store.closeCommandBar(); store.newChat(); },
    },
    {
      id: 'toggle-side',
      title: app.panes.side ? 'Colapsar panel de conversaciones' : 'Mostrar panel de conversaciones',
      hint: '⌘B',
      keywords: 'sidebar panel lateral ocultar mostrar',
      run: () => { store.closeCommandBar(); store.toggleSide(); },
    },
  ], [app.panes.side]);

  const shown = useMemo(() => filterCommands(commands, q), [commands, q]);

  // Every open starts clean: an old query surviving would put a different
  // command under Enter than the one the muscle expects.
  useEffect(() => {
    if (!open) return;
    setQ(''); setSel(0); setBusy('');
    const t = setTimeout(() => inputRef.current?.focus(), 0);
    return () => clearTimeout(t);
  }, [open]);

  useEffect(() => { setSel(0); }, [q]);

  // Keep the selected row in view when arrowing past the fold.
  useEffect(() => {
    if (!open) return;
    const el = listRef.current?.querySelector<HTMLElement>('[data-cmd-sel="1"]');
    el?.scrollIntoView({ block: 'nearest' });
  }, [sel, open]);

  if (!open) return null;

  const run = (cmd: Command | undefined) => {
    if (!cmd || busy) return;
    void Promise.resolve(cmd.run()).catch((err) => {
      console.warn('[commandbar] command failed', cmd.id, err);
      setBusy('');
    });
  };

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); store.closeCommandBar(); return; }
    if (e.key === 'ArrowDown') { e.preventDefault(); setSel((s) => Math.min(s + 1, Math.max(0, shown.length - 1))); return; }
    if (e.key === 'ArrowUp') { e.preventDefault(); setSel((s) => Math.max(0, s - 1)); return; }
    if (e.key === 'Enter') { e.preventDefault(); run(shown[sel]); }
  };

  return (
    <div className="ovl cmd-ovl" onMouseDown={(e) => { if (e.target === e.currentTarget) store.closeCommandBar(); }}>
      <div className="cmdbar" role="dialog" aria-label="Comandos" onKeyDown={onKey}>
        <input
          ref={inputRef}
          className="cmdq"
          value={q}
          disabled={!!busy}
          placeholder="Escribí un comando…"
          aria-label="Buscar comando"
          onChange={(e) => setQ(e.target.value)}
          spellCheck={false}
        />
        {busy ? (
          <div className="cmdbusy">{busy}</div>
        ) : (
          <div className="cmdlist" ref={listRef} role="listbox">
            {shown.length === 0 && <div className="cmdempty">Ningún comando coincide.</div>}
            {shown.map((cmd, i) => (
              <button
                key={cmd.id}
                type="button"
                role="option"
                aria-selected={i === sel}
                data-cmd={cmd.id}
                data-cmd-sel={i === sel ? '1' : '0'}
                className={`cmdrow${i === sel ? ' on' : ''}`}
                onMouseEnter={() => setSel(i)}
                onClick={() => run(cmd)}
              >
                <span className="cmdtxt">
                  <span className="cmdtitle">{cmd.title}</span>
                  {cmd.detail && <span className="cmddetail">{cmd.detail}</span>}
                </span>
                {cmd.hint && <kbd className="cmdhint">{cmd.hint}</kbd>}
              </button>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
