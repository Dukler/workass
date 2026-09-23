import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import {
  BROWSER_VIEWPORT_PRESETS, browserApi, browserViewportInputError, browserViewportPreset, localBrowserOwnsChat, sameBrowserBounds,
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
  const [customWidth, setCustomWidth] = useState('1440');
  const [customHeight, setCustomHeight] = useState('900');
  const [customMode, setCustomMode] = useState(false);
  const [viewportActionError, setViewportActionError] = useState<string | null>(null);
  const viewportRequestSequence = useRef(0);
  const customDraftDirty = useRef(false);
  const visibleChatId = useRef(chatId);
  const visibleMachineId = useRef(machineId);
  visibleChatId.current = chatId;
  visibleMachineId.current = machineId;

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
    viewportRequestSequence.current += 1;
    setState(initialState(chatId));
    setAddress('');
    setCustomWidth('1440');
    setCustomHeight('900');
    setCustomMode(false);
    customDraftDirty.current = false;
    setViewportActionError(null);
    return () => {
      if (visibleChatId.current === chatId) viewportRequestSequence.current += 1;
    };
  }, [chatId, machineId]);

  useEffect(() => {
    if (!state.viewport || customDraftDirty.current) return;
    setCustomWidth(String(state.viewport.width));
    setCustomHeight(String(state.viewport.height));
    setCustomMode(browserViewportPreset(state.viewport.width, state.viewport.height) === 'custom');
  }, [state.viewport?.width, state.viewport?.height]);

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
    let frame = 0;
    let lastBounds: WorkassBrowserBounds | null = null;
    const sync = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        if (disposed || !el.isConnected) return;
        const bounds = boundsFor(el);
        if (sameBrowserBounds(lastBounds, bounds)) return;
        lastBounds = bounds;
        void api.resize({ chatId, bounds });
      });
    };
    const observer = new ResizeObserver(sync);
    observer.observe(el);
    const root = document.getElementById('root');
    const layoutObserver = new MutationObserver(sync);
    if (root) layoutObserver.observe(root, { attributes: true, attributeFilter: ['class', 'style'] });
    addEventListener('resize', sync);
    const bounds = boundsFor(el);
    lastBounds = bounds;
    void api.activate({ chatId, conversationId, bounds }).then((next) => {
      if (disposed) return;
      setState(next);
      setAddress(next.url === 'about:blank' ? '' : next.url);
    });
    return () => {
      disposed = true;
      cancelAnimationFrame(frame);
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

  const viewportInputError = customMode ? browserViewportInputError(customWidth, customHeight) : null;
  const runViewportRequest = (request: () => Promise<WorkassBrowserState>) => {
    const requestSequence = ++viewportRequestSequence.current;
    const requestChatId = chatId;
    const requestMachineId = machineId;
    setViewportActionError(null);
    void request().then((next) => {
      if (visibleChatId.current !== requestChatId || visibleMachineId.current !== requestMachineId || viewportRequestSequence.current !== requestSequence || next.chatId !== requestChatId) return;
      setViewportActionError(null);
      setState((current) => current.chatId === requestChatId ? {
        ...current,
        viewport: next.viewport,
        effectiveViewport: next.effectiveViewport,
        viewportGeneration: next.viewportGeneration,
      } : current);
      if (next.viewport) {
        customDraftDirty.current = false;
        setCustomWidth(String(next.viewport.width));
        setCustomHeight(String(next.viewport.height));
      }
      setCustomMode(browserViewportPreset(next.viewport?.width, next.viewport?.height) === 'custom');
    }).catch((error: unknown) => {
      if (visibleChatId.current !== requestChatId || visibleMachineId.current !== requestMachineId || viewportRequestSequence.current !== requestSequence) return;
      setViewportActionError(error instanceof Error ? error.message : String(error));
    });
  };
  const applyViewport = (width: number, height: number) => {
    runViewportRequest(() => api.setViewport(chatId, width, height));
  };
  const chooseViewport = (value: string) => {
    if (value === 'custom') {
      viewportRequestSequence.current += 1;
      customDraftDirty.current = false;
      setCustomWidth(String(state.viewport?.width ?? 1440));
      setCustomHeight(String(state.viewport?.height ?? 900));
      setCustomMode(true);
      return;
    }
    customDraftDirty.current = false;
    setCustomMode(false);
    const preset = BROWSER_VIEWPORT_PRESETS[value as keyof typeof BROWSER_VIEWPORT_PRESETS];
    if (preset) applyViewport(preset.width, preset.height);
  };
  const applyCustomViewport = () => {
    if (viewportInputError) return;
    applyViewport(Number(customWidth), Number(customHeight));
  };
  const customWidthInvalid = !!viewportInputError && (
    !/^\d+$/.test(customWidth.trim()) || Number(customWidth) < 320 || Number(customWidth) > 3840
  );
  const customHeightInvalid = !!viewportInputError && (
    !/^\d+$/.test(customHeight.trim()) || Number(customHeight) < 240 || Number(customHeight) > 2160
  );
  const resetViewport = () => {
    runViewportRequest(() => api.resetViewport(chatId));
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
      <div className="brw2viewportbar">
        <label className="brw2viewport-preset">
          <span>Tamaño</span>
          <select aria-label="Tamaño del navegador" value={customMode ? 'custom' : browserViewportPreset(state.viewport?.width, state.viewport?.height)} onChange={(event) => chooseViewport(event.target.value)}>
            <option value="desktop">Escritorio 1440×900</option>
            <option value="laptop">Portátil 1280×800</option>
            <option value="narrow">Estrecho 390×844</option>
            <option value="custom">Personalizado</option>
          </select>
        </label>
        {customMode && (
          <div className="brw2viewport-custom">
            <label>
              <span>Ancho</span>
              <input
                aria-label="Ancho"
                aria-invalid={customWidthInvalid}
                aria-describedby={viewportInputError ? 'brw2viewport-error' : undefined}
                type="text"
                inputMode="numeric"
                value={customWidth}
                onChange={(event) => { viewportRequestSequence.current += 1; customDraftDirty.current = true; setCustomWidth(event.target.value); }}
              />
            </label>
            <span aria-hidden="true">×</span>
            <label>
              <span>Alto</span>
              <input
                aria-label="Alto"
                aria-invalid={customHeightInvalid}
                aria-describedby={viewportInputError ? 'brw2viewport-error' : undefined}
                type="text"
                inputMode="numeric"
                value={customHeight}
                onChange={(event) => { viewportRequestSequence.current += 1; customDraftDirty.current = true; setCustomHeight(event.target.value); }}
              />
            </label>
            <button type="button" className="brw2viewport-action" onClick={applyCustomViewport} disabled={!!viewportInputError}>Aplicar</button>
          </div>
        )}
        <span className="brw2viewport-effective" aria-live="polite">
          {state.effectiveViewport
            ? `Actual: ${state.effectiveViewport.width} × ${state.effectiveViewport.height}`
            : 'Cargando tamaño…'}
        </span>
        <button type="button" className="brw2viewport-action" onClick={resetViewport} disabled={!customMode && browserViewportPreset(state.viewport?.width, state.viewport?.height) === 'desktop'}>Restablecer</button>
      </div>
      {(viewportInputError || viewportActionError || state.error) && <div id="brw2viewport-error" className="brw2viewport-error" role="alert">{viewportInputError || viewportActionError || state.error}</div>}
      <div className="brw2view" ref={viewport}>
        {blank && (
          <div className="brw2empty"><IcBrowser /><span>Ingresá una URL para navegar.</span></div>
        )}
      </div>
    </div>
  );
}
