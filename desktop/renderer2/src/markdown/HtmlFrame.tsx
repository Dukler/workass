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
        const applied = guard.current.next(reported);
        setHeight(applied);
        setSized(true);
        if (blockKey) rememberHeight(blockKey, applied);
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
    // A page that never reports a size is still shown, so it is never invisible.
    if (fallback.current !== null) clearTimeout(fallback.current);
    fallback.current = setTimeout(() => {
      fallback.current = null;
      if (sizedRef.current) return;
      setHeight((value) => value ?? HTML_RENDER_FALLBACK_HEIGHT);
      setSized(true);
    }, 3000);
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
