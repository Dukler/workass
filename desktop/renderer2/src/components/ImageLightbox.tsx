import { useEffect } from 'react';
import { createPortal } from 'react-dom';
import { store, useApp } from '../store/store';
import { IcClose } from '../icons';
import { pushEscapeLayer } from '../markdown/html-frames';

// Full-size image viewer. One instance mounted at the app root; opens when
// store.imageLightbox is set (click any chat image). Backdrop click, Esc, or the
// close button dismiss; clicking the image itself is swallowed so it stays open
// while you inspect it. Same .ovl-family backdrop as the other overlays.
export function ImageLightbox() {
  const app = useApp();
  const box = app.imageLightbox;
  // Esc belongs to the viewer while it is open: closing it must never also
  // reach the app's Esc, which cancels a running turn.
  useEffect(() => {
    if (!box) return;
    return pushEscapeLayer(() => store.closeImageLightbox());
  }, [box]);
  if (!box) return null;
  return createPortal(
    <div
      className="imglb-ovl"
      onClick={() => store.closeImageLightbox()}
      role="dialog"
      aria-modal="true"
      aria-label={box.alt || 'Imagen'}
    >
      <button className="imglb-close" title="Cerrar · Esc" aria-label="Cerrar" onClick={() => store.closeImageLightbox()}>
        <IcClose />
      </button>
      <img className="imglb-img" src={box.src} alt={box.alt} tabIndex={0} aria-keyshortcuts="Meta+C Control+C" onClick={(e) => e.stopPropagation()} />
    </div>,
    document.body,
  );
}
