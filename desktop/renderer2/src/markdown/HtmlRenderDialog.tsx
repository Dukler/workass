// The page, expanded over the chat (t3code HtmlRenderDialog.tsx:27-78, styled
// as in the approved mock): 92vw × min(86vh, 100vh − 160px), a close button
// pinned top-right, the title and zoom (50–200% in 25% steps) below. It mounts
// its own frame with a fresh copy of the page, as t3code does.
import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { IcClose, IcMinus, IcPlus } from '../icons';
import { pushEscapeLayer } from './html-frames';
import { HtmlFrame } from './HtmlFrame';

export const HTML_DIALOG_MIN_ZOOM = 0.5;
export const HTML_DIALOG_MAX_ZOOM = 2;

export function stepZoom(zoom: number, delta: number): number {
  const next = Math.round((zoom + delta) * 4) / 4;
  return Math.min(HTML_DIALOG_MAX_ZOOM, Math.max(HTML_DIALOG_MIN_ZOOM, next));
}

export function HtmlRenderDialog({ raw, title, onClose, onLink }: {
  raw: string;
  title: string;
  onClose: () => void;
  onLink?: (url: string) => void;
}) {
  const close = useRef<HTMLButtonElement>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  const [zoom, setZoom] = useState(1);
  useEffect(() => pushEscapeLayer(() => onCloseRef.current()), []);
  // Focus starts on the close button; the opener gets it back on close.
  useEffect(() => { close.current?.focus(); }, []);
  if (typeof document === 'undefined') return null;
  return createPortal(
    <div
      className="hbdlg"
      role="dialog"
      aria-modal="true"
      aria-label={`${title}, ampliada`}
      onClick={(event) => { if (event.target === event.currentTarget) onClose(); }}
    >
      <button type="button" className="hbdlg-close" ref={close} title="Cerrar" aria-label="Cerrar" onClick={onClose}>
        <IcClose />
      </button>
      <div className="hbdlg-box">
        <div className="hbdlg-panel">
          <div className="hbdlg-zoom" style={{ zoom }}>
            <HtmlFrame raw={raw} title={title} fill onLink={onLink} />
          </div>
        </div>
        <div className="hbdlg-foot">
          <span className="hbdlg-title">{title}</span>
          <span className="hbdlg-zoomctl">
            <button type="button" title="Alejar" aria-label="Alejar" disabled={zoom <= HTML_DIALOG_MIN_ZOOM} onClick={() => setZoom((value) => stepZoom(value, -0.25))}>
              <IcMinus />
            </button>
            <button type="button" title="Tamaño real" aria-label={`Zoom ${Math.round(zoom * 100)} %, volver al tamaño real`} onClick={() => setZoom(1)}>
              {Math.round(zoom * 100)} %
            </button>
            <button type="button" title="Acercar" aria-label="Acercar" disabled={zoom >= HTML_DIALOG_MAX_ZOOM} onClick={() => setZoom((value) => stepZoom(value, 0.25))}>
              <IcPlus />
            </button>
          </span>
        </div>
      </div>
    </div>,
    document.body,
  );
}
