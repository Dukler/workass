import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import {
  browserApi, browserViewportForBounds, localBrowserOwnsChat, sameBrowserBounds,
  type WorkassBrowserApi, type WorkassBrowserBounds, type WorkassBrowserState,
} from '../browser';
import { connectedArtifactURL } from '../connected-artifacts';
import { store, useApp } from '../store/store';
import { IcBrowser } from '../icons';

function boundsFor(el: HTMLElement): WorkassBrowserBounds {
  const rect = el.getBoundingClientRect();
  return {
    x: Math.round(rect.left),
    y: Math.round(rect.top),
    width: Math.max(1, Math.round(rect.width)),
    height: Math.max(1, Math.round(rect.height)),
  };
}

function initialState(chatId: string): WorkassBrowserState {
  return {
    chatId, url: 'about:blank', title: '', loading: false, error: null,
    canGoBack: false, canGoForward: false, cdpAttached: false, persistent: false,
  };
}

// Minimal browser chrome (2026-07-12 redesign): one clean frame, a single nav
// row (‹ › ↻ + URL pill + close), and the web viewport. No title bar, no footer,
// no SSO / CDP / "Sesión persistente" labels — the persistent session and CDP
// control still work underneath; they just stop shouting. The persistent-session
// profile and CDP are daemon/shell concerns, not UI chrome.
export function BrowserPanel(props: {
  chatId: string;
  conversationId?: string;
  machineId?: string;
  artifactOrigin?: string;
  onClose: () => void;
}) {
  const api = browserApi();
  if (!api?.supported || !localBrowserOwnsChat(props.chatId, props.machineId)) return null;
  return <LocalBrowserPanel {...props} api={api} />;
}

function LocalBrowserPanel({
  api, chatId, conversationId, machineId, artifactOrigin, onClose,
}: {
  api: WorkassBrowserApi;
  chatId: string;
  conversationId?: string;
  machineId?: string;
  artifactOrigin?: string;
  onClose: () => void;
}) {
  const app = useApp();
  const viewport = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<WorkassBrowserState>(() => initialState(chatId));
  const [address, setAddress] = useState('');

  // The browser is a native WebContentsView that paints ABOVE all HTML, so an
  // HTML overlay (the image lightbox) can't cover it — it would show through the
  // right pane (user, 2026-07-23). No z-index fixes a native layer: detach the
  // view while the lightbox is open, and re-attach it (same live page) on close.
  const overlayOpen = !!app.imageLightbox;
  useEffect(() => {
    if (!api || !overlayOpen) return;
    void api.hide(chatId);
    return () => {
      const el = viewport.current;
      if (el && el.isConnected) void api.activate({ chatId, conversationId, bounds: boundsFor(el) });
    };
  }, [api, chatId, conversationId, overlayOpen]);

  useEffect(() => {
    setState(initialState(chatId));
    setAddress('');
  }, [chatId, machineId]);

  useEffect(() => {
    if (!api) return;
    return api.onState((next) => {
      if (next.chatId !== chatId) return;
      setState((current) => {
        if (current.viewportGeneration != null && next.viewportGeneration != null && next.viewportGeneration < current.viewportGeneration) {
          return {
            ...next,
            viewport: current.viewport,
            effectiveViewport: current.effectiveViewport,
            viewportGeneration: current.viewportGeneration,
          };
        }
        return next;
      });
      if (document.activeElement?.getAttribute('data-browser-address') !== chatId) {
        setAddress(next.url === 'about:blank' ? '' : next.url);
      }
    });
  }, [api, chatId]);

  useLayoutEffect(() => {
    const el = viewport.current;
    if (!api || !el) return;
    let disposed = false;
    let activated = false;
    let syncing = false;
    let pendingBounds: WorkassBrowserBounds | null = null;
    let lastBounds: WorkassBrowserBounds | null = null;
    const flushBounds = async () => {
      if (!activated || syncing) return;
      syncing = true;
      try {
        while (!disposed && pendingBounds) {
          const bounds = pendingBounds;
          pendingBounds = null;
          if (sameBrowserBounds(lastBounds, bounds)) continue;
          await api.resize({ chatId, bounds });
          if (disposed) return;
          const size = browserViewportForBounds(bounds);
          await api.setViewport(chatId, size.width, size.height);
          lastBounds = bounds;
        }
      } catch (error) {
        if (!disposed) setState((current) => ({ ...current, error: error instanceof Error ? error.message : String(error) }));
      } finally {
        syncing = false;
        if (!disposed && pendingBounds) void flushBounds();
      }
    };
    const sync = () => {
      if (disposed || !el.isConnected) return;
      const rect = el.getBoundingClientRect();
      if (rect.width <= 0 || rect.height <= 0) return;
      const bounds = boundsFor(el);
      if (sameBrowserBounds(lastBounds, bounds)) return;
      pendingBounds = bounds;
      void flushBounds();
    };
    const observer = new ResizeObserver(sync);
    observer.observe(el);
    const root = document.getElementById('root');
    const layoutObserver = new MutationObserver(sync);
    if (root) layoutObserver.observe(root, { attributes: true, attributeFilter: ['class', 'style'] });
    addEventListener('resize', sync);
    const bounds = boundsFor(el);
    void api.activate({ chatId, conversationId, bounds }).then((next) => {
      if (disposed) return;
      setState(next);
      setAddress(next.url === 'about:blank' ? '' : next.url);
      activated = true;
      sync();
    });
    return () => {
      disposed = true;
      observer.disconnect();
      layoutObserver.disconnect();
      removeEventListener('resize', sync);
      void api.hide(chatId);
    };
  }, [api, chatId, machineId, conversationId]);

  const run = (command: 'back' | 'forward' | 'reload' | 'stop') => {
    if (!api) return;
    void api.command(chatId, command).then(setState);
  };
  const navigate = (event: FormEvent) => {
    event.preventDefault();
    if (!api || !address.trim()) return;
    const target = connectedArtifactURL(artifactOrigin ?? '', address, undefined, undefined, store.localMachineId());
    if (!target) {
      setState((current) => ({ ...current, error: 'No se encontró el origen de la máquina remota.' }));
      return;
    }
    void api.command(chatId, 'navigate', target).then((next) => {
      setState(next);
      setAddress(next.url === 'about:blank' ? '' : next.url);
    });
  };

  const blank = state.url === 'about:blank' && !state.loading;

  return (
    <div className="brw2 live-browser">
      <form className="brw2bar" onSubmit={navigate}>
        <button type="button" className="brw2nav" title="Atrás" disabled={!state.canGoBack} onClick={() => run('back')} aria-label="Atrás">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6"><path d="M10 3L5 8l5 5" /></svg>
        </button>
        <button type="button" className="brw2nav" title="Adelante" disabled={!state.canGoForward} onClick={() => run('forward')} aria-label="Adelante">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6"><path d="M6 3l5 5-5 5" /></svg>
        </button>
        <button type="button" className={`brw2nav ${state.loading ? 'loading' : ''}`} title={state.loading ? 'Detener' : 'Recargar'} onClick={() => run(state.loading ? 'stop' : 'reload')} aria-label={state.loading ? 'Detener' : 'Recargar'}>
          {state.loading
            ? <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6"><path d="M4 4l8 8M12 4l-8 8" /></svg>
            : <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M13 8a5 5 0 1 1-1.5-3.5M13 2v3h-3" /></svg>}
        </button>
        <input
          className="brw2url"
          data-browser-address={chatId}
          value={address}
          onChange={(event) => setAddress(event.target.value)}
          placeholder="URL o búsqueda"
          aria-label="Dirección del navegador"
          spellCheck={false}
        />
        <button type="button" className="brw2close" title="Cerrar navegador" onClick={onClose} aria-label="Cerrar navegador">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6"><path d="M4 4l8 8M12 4l-8 8" /></svg>
        </button>
      </form>
      {state.error && <div className="brw2viewport-error" role="alert">{state.error}</div>}
      <div className="brw2view" ref={viewport}>
        {blank && (
          <div className="brw2empty"><IcBrowser /><span>Ingresá una URL para navegar.</span></div>
        )}
      </div>
    </div>
  );
}
