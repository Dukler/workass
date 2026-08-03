import { useEffect } from 'react';
import { copyImageElement, imageCopyTargetForShortcut, isImageCopyShortcut, isImageExternalOpenGesture, openImageElementExternally } from '../image-copy';
import { store } from '../store/store';

function imageInside(element: Element | null): HTMLImageElement | null {
  if (element instanceof HTMLImageElement) return element;
  return element?.querySelector?.('img') ?? null;
}

function chatImageFromTarget(target: EventTarget | null): HTMLImageElement | null {
  const image = imageInside(target instanceof Element ? target : null);
  if (!image) return null;
  const copyable = image.hasAttribute('aria-keyshortcuts')
    || !!image.closest('.assistant-inline-image, .tool-image, .imglb-ovl');
  return copyable ? image : null;
}

// One controller covers every image surface (assistant media, tool results,
// queued/draft thumbnails, user attachments, and the lightbox). It only claims
// Cmd/Ctrl+C when an image is actually focused or the lightbox is open, leaving
// normal transcript/text copy untouched.
export function ImageClipboardController() {
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (!isImageCopyShortcut(event)) return;
      const lightbox = document.querySelector<HTMLImageElement>('.imglb-img');
      const focused = imageInside(document.activeElement);
      const image = imageCopyTargetForShortcut(lightbox ?? focused, window.getSelection(), document.activeElement);
      if (!image) return;
      event.preventDefault();
      void copyImageElement(image);
    };
    const onClickCapture = (event: MouseEvent) => {
      const bridge = window.workassClipboard;
      if (!bridge?.supported || typeof bridge.openImageExternal !== 'function') return;
      if (!isImageExternalOpenGesture(event, navigator.platform || '')) return;
      const image = chatImageFromTarget(event.target);
      if (!image) return;
      event.preventDefault();
      event.stopPropagation();
      void openImageElementExternally(image).then((opened) => {
        if (!opened) store.openImageLightbox(image.currentSrc || image.src, image.alt || 'Imagen');
      });
    };
    document.addEventListener('keydown', onKeyDown);
    document.addEventListener('click', onClickCapture, true);
    return () => {
      document.removeEventListener('keydown', onKeyDown);
      document.removeEventListener('click', onClickCapture, true);
    };
  }, []);
  return null;
}
