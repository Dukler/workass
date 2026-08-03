// Workspace browser — the folder picker behind "Añadir carpeta".
//
// It browses the SERVER's filesystem over the frozen fs:list-dir channel, so an
// Electron window on the daemon host and a phone on the LAN see the same
// folders. The native Electron dialog and the "type an absolute path" prompt
// were both rejected (user, 2026-07-12); do not bring them back. Nothing is
// added to the store until the user confirms with "Usar esta carpeta".

import { useCallback, useEffect, useId, useRef, useState } from 'react';
import type { KeyboardEvent as ReactKeyboardEvent, MouseEvent as ReactMouseEvent } from 'react';
import { createPortal } from 'react-dom';
import type { DirListing } from '../wire/types';
import { callThrow, has } from '../wire/api';
import {
  canSelectFolder, errorListing, folderCountLabel, isFolderClickThrough, isServerRoots, normalizeListing, pathLabel,
  type FolderPointerNavigation,
} from '../workspace-picker';
import { IcFolder } from '../icons';

// Left-to-right mark: keeps the path rendering LTR inside the RTL box that
// truncates it from the start (see .wbpath).
const LTR_MARK = String.fromCharCode(0x200e);

const IcBack = () => (
  <svg viewBox="0 0 16 16" fill="none" stroke="currentColor">
    <path d="M10 3.5L5.5 8l4.5 4.5" strokeLinecap="round" strokeLinejoin="round" />
  </svg>
);
const IcUp = () => (
  <svg viewBox="0 0 16 16" fill="none" stroke="currentColor">
    <path d="M8 12.5V4M4.5 7.5L8 4l3.5 3.5" strokeLinecap="round" strokeLinejoin="round" />
  </svg>
);
// Server roots: a two-unit rack, to say "the machine that runs Workass".
const IcServer = () => (
  <svg viewBox="0 0 16 16" fill="none" stroke="currentColor">
    <rect x="2.5" y="3" width="11" height="4" rx="1.2" />
    <rect x="2.5" y="9" width="11" height="4" rx="1.2" />
    <path d="M4.8 5h.01M4.8 11h.01" strokeLinecap="round" />
  </svg>
);

interface WorkspaceBrowserProps {
  onSelect: (path: string) => void;
  onClose: () => void;
}

export function WorkspaceBrowser({ onSelect, onClose }: WorkspaceBrowserProps) {
  const titleId = useId();
  const subId = useId();
  const panelRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  // Only the newest navigation may paint: a slow parent listing must not land on
  // top of the child the user already clicked into.
  const request = useRef(0);
  const focusList = useRef(false);
  const lastPointerNavigation = useRef<FolderPointerNavigation | null>(null);

  const [supported] = useState(() => has('listDir'));
  const [listing, setListing] = useState<DirListing | null>(null);
  const [loading, setLoading] = useState(supported);
  const [trail, setTrail] = useState<Array<string | null>>([]);

  const load = useCallback(async (target: string | null) => {
    const seq = ++request.current;
    setLoading(true);
    let next: DirListing;
    try {
      next = normalizeListing(await callThrow('listDir', target), target);
    } catch (err) {
      next = errorListing(target, err);
    }
    if (seq !== request.current) return;
    setListing(next);
    setLoading(false);
  }, []);

  // Open on the server roots; an older bridge without fs:list-dir says so instead.
  useEffect(() => { if (supported) void load(null); }, [supported, load]);

  const open = (target: string | null, event?: ReactMouseEvent<HTMLElement>) => {
    if (loading) return;
    if (event && event.detail > 0) {
      const gesture = { x: event.clientX, y: event.clientY, at: event.timeStamp };
      if (isFolderClickThrough(lastPointerNavigation.current, gesture)) return;
      lastPointerNavigation.current = gesture;
    }
    focusList.current = true;
    setTrail((prev) => [...prev, listing?.path ?? null]);
    void load(target);
  };
  const back = () => {
    if (trail.length === 0) return;
    focusList.current = true;
    const previous = trail[trail.length - 1] ?? null;
    setTrail((prev) => prev.slice(0, -1));
    void load(previous);
  };

  const atRoots = isServerRoots(listing);
  const selectable = canSelectFolder(listing) && !loading;
  const currentPath = pathLabel(listing);

  // Real modality: the app behind the dialog goes inert (unfocusable and hidden
  // from assistive tech) while it is open — the dialog is portaled to <body>, so
  // it stays interactive. Focus lands on the dialog and returns to "Añadir
  // carpeta" on close; inert is dropped FIRST so that focus can actually land.
  useEffect(() => {
    const root = document.getElementById('root');
    const restore = document.activeElement as HTMLElement | null;
    root?.setAttribute('inert', '');
    panelRef.current?.focus();
    return () => {
      root?.removeAttribute('inert');
      restore?.focus?.();
    };
  }, []);

  // Esc closes. Capture it so the dialog wins over App.tsx's layered Esc handler,
  // which would otherwise cancel a running turn or open the rewind menu.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      e.preventDefault();
      e.stopPropagation();
      onClose();
    };
    document.addEventListener('keydown', onKey, true);
    return () => document.removeEventListener('keydown', onKey, true);
  }, [onClose]);

  // After a navigation the row that was clicked no longer exists, so move focus
  // to the top of the new listing (or back to the dialog when it has no rows).
  useEffect(() => {
    if (loading || !focusList.current) return;
    focusList.current = false;
    const first = listRef.current?.querySelector<HTMLButtonElement>('.wbrow');
    (first ?? panelRef.current)?.focus();
  }, [loading, listing]);

  // Keep Tab inside the dialog (aria-modal is a promise to assistive tech, not a trap).
  const onPanelKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'Tab') return;
    const stops = panelRef.current?.querySelectorAll<HTMLElement>('button:not([disabled])');
    if (!stops || stops.length === 0) return;
    const first = stops[0];
    const last = stops[stops.length - 1];
    const active = document.activeElement;
    if (e.shiftKey && (active === first || active === panelRef.current)) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && active === last) { e.preventDefault(); first.focus(); }
  };

  const onListKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
    const rows = Array.from(listRef.current?.querySelectorAll<HTMLButtonElement>('.wbrow') ?? []);
    if (rows.length === 0) return;
    e.preventDefault();
    const at = rows.indexOf(document.activeElement as HTMLButtonElement);
    const next = e.key === 'ArrowDown' ? rows[Math.min(rows.length - 1, at + 1)] : rows[Math.max(0, at - 1)];
    next?.focus();
  };

  return createPortal(
    <div className="ovl" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose(); }}>
      <div
        ref={panelRef}
        className="wbrowse"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={subId}
        aria-busy={loading}
        tabIndex={-1}
        onKeyDown={onPanelKeyDown}
      >
        <div className="wbhead">
          <b id={titleId}>Elegir carpeta del servidor</b>
          <span className="sp" />
          <button className="tico" type="button" title="Cerrar · Esc" aria-label="Cerrar" onClick={onClose}>✕</button>
        </div>
        <div className="wbsub" id={subId}>
          Estas son las carpetas del equipo donde corre Workass, no las de este dispositivo.
        </div>

        <div className="wbbar">
          <button
            className="wbnav" type="button" title="Atrás" aria-label="Atrás"
            disabled={trail.length === 0 || loading} onClick={back}
          ><IcBack /></button>
          <button
            className="wbnav" type="button" title="Subir un nivel" aria-label="Subir un nivel"
            disabled={!listing?.parent || loading} onClick={(event) => open(listing?.parent ?? null, event)}
          ><IcUp /></button>
          <button
            className="wbnav" type="button" title="Carpetas del servidor" aria-label="Carpetas del servidor"
            disabled={!supported || atRoots || loading} onClick={(event) => open(null, event)}
          ><IcServer /></button>
          <div className="wbpath mono" title={currentPath} aria-live="polite">{LTR_MARK + currentPath}</div>
        </div>

        {!supported ? (
          <div className="wberr" role="alert">
            <b>Este servidor no expone el explorador de carpetas.</b>
            <span>Actualizá Workass en el servidor para elegir una carpeta desde este cliente.</span>
          </div>
        ) : loading ? (
          <div className="wbempty">Cargando carpetas…</div>
        ) : listing?.error ? (
          <div className="wberr" role="alert">
            <b>No pude abrir esta carpeta.</b>
            <code>{listing.error}</code>
            <button className="btn" type="button" onClick={() => void load(listing.path)}>Reintentar</button>
          </div>
        ) : listing && listing.entries.length === 0 ? (
          <div className="wbempty">
            {atRoots ? 'El servidor no ofreció ninguna carpeta.' : 'Esta carpeta no tiene subcarpetas.'}
          </div>
        ) : (
          <div className="wblist" ref={listRef} onKeyDown={onListKeyDown}>
            {listing?.entries.map((entry) => (
              <button
                key={entry.path}
                className="wbrow"
                type="button"
                title={entry.path}
                aria-label={`Abrir ${entry.name}`}
                onClick={(event) => open(entry.path, event)}
                onDoubleClick={(event) => event.preventDefault()}
              >
                <span className="wbico" aria-hidden="true"><IcFolder /></span>
                <span className="wbname">{entry.name}</span>
                <span className="wbchev" aria-hidden="true">›</span>
              </button>
            ))}
          </div>
        )}

        <div className="wbfoot">
          <span className="wbcount">
            {loading ? 'Cargando…' : !supported || listing?.error ? '' : folderCountLabel(listing)}
          </span>
          <span className="sp" />
          <button className="btn" type="button" onClick={onClose}>Cancelar</button>
          <button
            className="btn pri"
            type="button"
            disabled={!selectable}
            title={selectable ? `Usar ${listing?.path}` : 'Entrá en una carpeta del servidor para elegirla'}
            onClick={() => { if (listing?.path) onSelect(listing.path); }}
          >Usar esta carpeta</button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
