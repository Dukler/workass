// An HTML page in the chat, as approved in desktop/docs/mocks/html-render
// (2026-10-21): Marco B (borderless, a slim always-visible icon row: code ·
// copy · expand), the page first with a code switch, full height (the frame
// fits the page up to 20,000px), one quiet line while it is written, a zoom
// dialog, and a link that opens only after a confirmation showing its URL.
import { useContext, useEffect, useRef, useState, type Ref } from 'react';
import { createPortal } from 'react-dom';
import { findMatchOffsets } from '../chat-find';
import { IcCheck, IcCodeView, IcExpand, IcPageView, IcStampCopy } from '../icons';
import { CodeBlock, copyCodeText } from './CodeBlock';
import { ChatFindQueryContext } from './find-context';
import { HTML_RENDER_MAX_SOURCE, htmlBlockIdentity, htmlPageTitle, htmlStreamSize } from './html-render';
import { claimLinkAsk, pushEscapeLayer, rememberView, rememberedView } from './html-frames';
import { HtmlFrame, useLatest } from './HtmlFrame';
import { HtmlRenderDialog } from './HtmlRenderDialog';

function HtmlCopyButton({ raw }: { raw: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => { if (timer.current !== null) clearTimeout(timer.current); }, []);
  const copy = async () => {
    const copied = await copyCodeText(raw);
    setState(copied ? 'copied' : 'failed');
    if (timer.current !== null) clearTimeout(timer.current);
    timer.current = setTimeout(() => setState('idle'), 1600);
  };
  const label = state === 'copied' ? 'Código copiado' : state === 'failed' ? 'No se pudo copiar el código' : 'Copiar código';
  return (
    <button type="button" className="hbtn" data-state={state} title={label} aria-label={label} onClick={() => { void copy(); }}>
      {state === 'copied' ? <IcCheck /> : <IcStampCopy />}
    </button>
  );
}

type WindowOpener = (url: string, target: string, features: string) => unknown;

/** «Abrir»: the only path from a page's link to the system browser. */
export function openConfirmedLink(url: string, opener?: WindowOpener): void {
  // The shell's window-open handler hands it to the system browser; noopener
  // keeps the new window from reaching back into Workass.
  const open: WindowOpener = opener ?? ((target, name, features) => window.open(target, name, features));
  open(url, '_blank', 'noopener,noreferrer');
}

export function HtmlLinkAskView({ url, onCancel, onOpen, goRef }: {
  url: string;
  onCancel: () => void;
  onOpen: () => void;
  goRef?: Ref<HTMLButtonElement>;
}) {
  return (
    <div className="hblink" role="alertdialog" aria-label="Abrir un enlace de la página">
      <span className="t">¿Abrir este enlace en el navegador?<span className="u">{url}</span></span>
      <button type="button" onClick={onCancel}>Cancelar</button>
      <button type="button" className="go" ref={goRef} onClick={onOpen}>Abrir</button>
    </div>
  );
}

/**
 * The confirmation a link inside a page asks for. It shows the whole URL (a
 * long one wraps instead of being cut), and only «Abrir» opens it, through the
 * shell's window-open handler, in the system browser.
 */
export function HtmlLinkAsk({ url, onClose }: { url: string; onClose: () => void }) {
  const go = useRef<HTMLButtonElement>(null);
  const close = useLatest(onClose);
  useEffect(() => {
    const done = () => close.current();
    const release = claimLinkAsk(done);
    const pop = pushEscapeLayer(done);
    return () => { pop(); release(); };
  }, [close]);
  useEffect(() => { go.current?.focus(); }, [url]);
  if (typeof document === 'undefined') return null;
  return createPortal(
    <HtmlLinkAskView url={url} goRef={go} onCancel={onClose} onOpen={() => { onClose(); openConfirmedLink(url); }} />,
    document.body,
  );
}

function HtmlPageView({ raw, title, blockKey, onCode }: { raw: string; title: string; blockKey: string; onCode: () => void }) {
  const [expanded, setExpanded] = useState(false);
  const [askUrl, setAskUrl] = useState<string | null>(null);
  const expand = useRef<HTMLButtonElement>(null);
  return (
    <div className="hb hb-b" data-html-page="">
      <div className="hb-row">
        <button type="button" className="hbtn" title="Ver el código" aria-label="Ver el código" onClick={onCode}><IcCodeView /></button>
        <HtmlCopyButton raw={raw} />
        <button type="button" className="hbtn" title="Ampliar" aria-label="Ampliar" ref={expand} onClick={() => setExpanded(true)}><IcExpand /></button>
      </div>
      <div className="hb-clip">
        <HtmlFrame raw={raw} title={title} blockKey={blockKey} onLink={setAskUrl} />
      </div>
      {expanded && (
        <HtmlRenderDialog
          raw={raw}
          title={title}
          onLink={setAskUrl}
          onClose={() => {
            setExpanded(false);
            expand.current?.focus();
          }}
        />
      )}
      {askUrl && <HtmlLinkAsk url={askUrl} onClose={() => setAskUrl(null)} />}
    </div>
  );
}

// While the fence is still being written: one quiet line with its size so far,
// and a peek at the code. The page renders when the fence closes.
function HtmlStreamLine({ raw }: { raw: string }) {
  const [peek, setPeek] = useState(false);
  return (
    <div className="hb-stream">
      <div className="hb-line">
        <span className="hb-orbit" aria-hidden="true" />
        <span className="t">Escribiendo página</span>
        <span className="m">{htmlStreamSize(raw)}</span>
        <span className="sp" />
        <button type="button" className="code-copy" aria-expanded={peek} onClick={() => setPeek((open) => !open)}>
          <IcCodeView />
          <span>{peek ? 'Ocultar código' : 'Ver código'}</span>
        </button>
      </div>
      {peek && <CodeBlock raw={raw} language="html" closed={false} className="hb-code hb-line-peek" />}
    </div>
  );
}

/**
 * A ```html block, or a page the agent published. `closed` is the fence's own
 * state; `streaming` is the reply's. The page renders once the fence closes or
 * the reply stops (the Mermaid rule, t3code ChatMarkdown.tsx:1531).
 */
export function HtmlBlock({ raw, closed, streaming = false, title }: { raw: string; closed: boolean; streaming?: boolean; title?: string }) {
  const findQuery = useContext(ChatFindQueryContext);
  const key = htmlBlockIdentity(raw);
  const [view, setViewState] = useState<'page' | 'code'>(() => rememberedView(key));
  const setView = (next: 'page' | 'code') => {
    rememberView(key, next);
    setViewState(next);
  };

  if (!closed && streaming) return <HtmlStreamLine raw={raw} />;
  // Over t3code's input cap the block stays code.
  if (raw.length > HTML_RENDER_MAX_SOURCE) return <CodeBlock raw={raw} language="html" closed />;
  // An open find whose query is inside the source shows the source, so the
  // match can be highlighted; closing find returns to the reader's choice.
  const revealed = !!findQuery && findMatchOffsets(raw, findQuery).length > 0;
  if (revealed || view === 'code') {
    return (
      <CodeBlock
        raw={raw}
        language="html"
        closed
        className="hb-code"
        actions={revealed ? undefined : (
          <button type="button" className="code-copy" title="Ver la página" aria-label="Ver la página" onClick={() => setView('page')}>
            <IcPageView />
            <span>Página</span>
          </button>
        )}
      />
    );
  }
  return <HtmlPageView raw={raw} title={title || htmlPageTitle(raw)} blockKey={key} onCode={() => setView('code')} />;
}
