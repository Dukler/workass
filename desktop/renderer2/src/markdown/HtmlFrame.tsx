// One sandboxed frame for an HTML page in the chat (approved mock
// desktop/docs/mocks/html-render). The security strings live in html-render.ts;
// the shared listener, theme and lazy mounting in html-frames.ts.
import { useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react';
import {
  HTML_RENDER_FALLBACK_HEIGHT,
  HTML_RENDER_SANDBOX,
  HTML_RENDER_STATIC_HEIGHT,
  HTML_RENDER_STATIC_SANDBOX,
  buildHtmlSrcdoc,
  createGrowthGuard,
} from './html-render';
import {
  currentScheme,
  currentThemeVars,
  lazyMountSupported,
  markFrameLoaded,
  observeFrameMount,
  registerHtmlFrame,
  rememberHeight,
  rememberedHeight,
  renderFramesScripted,
  subscribeTheme,
  themeSnapshot,
  type FrameHandlers,
} from './html-frames';

/** Whether the frame is inside its scroller's visible box (or the window's). */
export function frameOnScreen(frame: HTMLIFrameElement | null): boolean {
  if (!frame || !frame.isConnected) return false;
  const box = frame.getBoundingClientRect();
  const view = frame.closest('.scroll')?.getBoundingClientRect() ?? { top: 0, bottom: window.innerHeight };
  return box.bottom > view.top && box.top < view.bottom;
}

// How long a smaller reported height must hold before the frame shrinks.
export const SHRINK_SETTLE_MS = 150;

export function useLatest<T>(value: T) {
  const ref = useRef(value);
  ref.current = value;
  return ref;
}

/**
 * One frame. `fill` frames (the zoom dialog) take their box's height and
 * ignore size reports; every other frame fits its page.
 */
export function HtmlFrame({ raw, title, blockKey, fill = false, onLink }: {
  raw: string;
  title: string;
  blockKey?: string;
  fill?: boolean;
  onLink?: (url: string) => void;
}) {
  const scripted = renderFramesScripted();
  const themeVersion = useSyncExternalStore(subscribeTheme, themeSnapshot, () => 0);
  // A scripted page follows the theme by message and never reloads. A static
  // page cannot listen, and has no state to lose, so it is rebuilt instead.
  const staticTheme = scripted ? 0 : themeVersion;
  const srcdoc = useMemo(
    () => buildHtmlSrcdoc(raw, { vars: currentThemeVars(), scheme: currentScheme(), mode: scripted ? 'scripted' : 'static' }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [raw, scripted, staticTheme],
  );
  const holder = useRef<HTMLDivElement>(null);
  const frame = useRef<HTMLIFrameElement>(null);
  const handlers = useRef<FrameHandlers | null>(null);
  const onLinkRef = useLatest(onLink);
  const [mounted, setMounted] = useState(() => fill || !lazyMountSupported());
  const [height, setHeight] = useState<number | undefined>(() => (fill ? undefined : scripted ? (blockKey ? rememberedHeight(blockKey) : undefined) : HTML_RENDER_STATIC_HEIGHT));
  // The frame stays invisible until its page first reports a height, so the
  // transcript never shows a 24px sliver (not gated on reduced motion: the
  // fade is a dissolve, and Reduce Motion is on where this ships).
  const [sized, setSized] = useState(fill);
  const sizedRef = useLatest(sized);
  const heightRef = useLatest(height);
  const shrinkTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => { if (shrinkTimer.current !== null) clearTimeout(shrinkTimer.current); }, []);
  const guard = useRef(createGrowthGuard());

  useEffect(() => {
    if (fill || !holder.current) return;
    return observeFrameMount(holder.current, setMounted);
  }, [fill]);

  useLayoutEffect(() => {
    const iframe = frame.current;
    if (!mounted || !iframe) return;
    if (!fill) setSized(false);
    const entry: FrameHandlers = {
      iframe, fill, scripted,
      onSize: (reported) => {
        // A frame changing height moves everything under it, which reads as
        // the chat jumping while someone scrolls (user 2026-11-08: pages
        // reported 0 and then their height every time they loaded, a whole
        // page's worth of jump each time). So nothing below 1 px is a height,
        // a page grows at once, and it only shrinks once the smaller height
        // has held for SHRINK_SETTLE_MS: a drop and a pop back never move it.
        if (!(reported >= 1)) return;
        const apply = (value: number) => {
          const applied = guard.current.next(value);
          setHeight(applied);
          setSized(true);
          if (blockKey) rememberHeight(blockKey, applied);
        };
        if (shrinkTimer.current !== null) { clearTimeout(shrinkTimer.current); shrinkTimer.current = null; }
        const current = heightRef.current;
        if (current !== undefined && reported < current && sizedRef.current) {
          shrinkTimer.current = setTimeout(() => { shrinkTimer.current = null; apply(reported); }, SHRINK_SETTLE_MS);
          return;
        }
        apply(reported);
      },
      onLink: (url) => onLinkRef.current?.(url),
    };
    handlers.current = entry;
    const unregister = registerHtmlFrame(entry);
    return () => {
      unregister();
      if (handlers.current === entry) handlers.current = null;
    };
  }, [mounted, fill, scripted, blockKey, srcdoc, onLinkRef]);

  const fallback = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => { if (fallback.current !== null) clearTimeout(fallback.current); }, []);
  const onLoad = () => {
    if (handlers.current) markFrameLoaded(handlers.current);
    if (fill || !scripted) { setSized(true); return; }
    // A page whose height is already known shows at once; it only reports
    // once it has settled (htmlFrameBootstrap), which may be a moment later.
    if (heightRef.current !== undefined) setSized(true);
    // A page that never reports a size is still shown, so it is never invisible.
    if (fallback.current !== null) clearTimeout(fallback.current);
    // Off screen a page does not report until it is near (it is not laid out
    // there), so the fallback waits until the frame is on screen.
    const arm = () => {
      fallback.current = setTimeout(() => {
        fallback.current = null;
        if (sizedRef.current) return;
        if (!frameOnScreen(frame.current)) { arm(); return; }
        setHeight((value) => value ?? HTML_RENDER_FALLBACK_HEIGHT);
        setSized(true);
      }, 3000);
    };
    arm();
  };

  const shown = fill ? undefined : (height ?? 24);
  return (
    <div className="hb-holder" ref={holder} style={!fill && !mounted ? { height: shown } : undefined}>
      {mounted && (
        <iframe
          ref={frame}
          className={`hb-frame${sized ? ' sized' : ''}`}
          title={title}
          // Never allow-same-origin, allow-popups, allow-modals,
          // allow-top-navigation* or allow-downloads, and no allow= features.
          sandbox={scripted ? HTML_RENDER_SANDBOX : HTML_RENDER_STATIC_SANDBOX}
          referrerPolicy="no-referrer"
          srcDoc={srcdoc}
          style={shown === undefined ? undefined : { height: shown }}
          onLoad={onLoad}
          data-static={scripted ? undefined : ''}
        />
      )}
    </div>
  );
}
