import { useEffect, useRef, useState, type ReactNode } from 'react';
import type { MessageImage } from '../store/types';
import { messageImageSrc } from '../image-drafts';
import { call } from '../wire/api';
import { observeTranscriptImageOnce } from '../transcript-images';

// A message image the daemon sent by reference (archive reads with
// deferImages, 2026-10-13): the chat's text paints first, and each image's
// bytes load once, when it comes into view. Loaded images stay cached, so
// going back to a chat never fetches them again.
const loaded = new Map<string, string>();
const pending = new Map<string, Promise<string | null>>();
const CACHE_LIMIT = 24;

function remember(ref: string, src: string) {
  loaded.delete(ref);
  loaded.set(ref, src);
  while (loaded.size > CACHE_LIMIT) loaded.delete(loaded.keys().next().value as string);
}

export function cachedImageSrc(image: MessageImage): string {
  if (!image.deferredImageRef) return messageImageSrc(image);
  return loaded.get(image.deferredImageRef) ?? '';
}

export function loadImageSrc(tabId: string, image: MessageImage): Promise<string | null> {
  const ref = image.deferredImageRef;
  if (!ref) return Promise.resolve(messageImageSrc(image));
  const hit = loaded.get(ref);
  if (hit) { remember(ref, hit); return Promise.resolve(hit); }
  let request = pending.get(ref);
  if (!request) {
    request = call('archiveLoad', tabId, { imageRef: ref }).then((reply) => {
      const data = Array.isArray(reply) && reply[0] && typeof reply[0] === 'object'
        ? (reply[0] as { data?: unknown }).data : null;
      if (typeof data !== 'string' || !data) return null;
      const src = data.startsWith('data:') ? data : `data:${image.mimeType};base64,${data}`;
      remember(ref, src);
      return src;
    }).catch(() => null).finally(() => { pending.delete(ref); });
    pending.set(ref, request);
  }
  return request;
}

// The image's src: immediate for inline bytes or a cached reference, loaded
// once the element scrolls into view otherwise.
export function useMessageImageSrc(tabId: string, image: MessageImage) {
  const target = useRef<HTMLElement | null>(null);
  const [src, setSrc] = useState(() => cachedImageSrc(image));
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    const now = cachedImageSrc(image);
    setSrc(now);
    setFailed(false);
    if (now || !image.deferredImageRef || !target.current) return;
    let current = true;
    const stop = observeTranscriptImageOnce(target.current, () => {
      void loadImageSrc(tabId, image).then((next) => {
        if (!current) return;
        if (next) setSrc(next); else setFailed(true);
      });
    });
    return () => { current = false; stop(); };
  }, [tabId, image]);
  return { target, src, failed };
}

// One clickable image of a message: the reply gallery, an inline Markdown
// image, a user's attachment. Clicking opens the loaded image.
export function MessageImageButton({ tabId, image, alt, className, onOpen, children }: {
  tabId: string;
  image: MessageImage;
  alt: string;
  className: string;
  onOpen: (src: string) => void;
  children?: ReactNode;
}) {
  const { target, src, failed } = useMessageImageSrc(tabId, image);
  return (
    <button
      ref={(element) => { target.current = element; }}
      type="button"
      className={className}
      title={src ? 'Ampliar' : alt}
      disabled={!src}
      onClick={() => { if (src) onOpen(src); }}
    >
      {src
        ? <img src={src} alt={alt} decoding="async" />
        : <span className="assistant-image-pending">{failed ? 'Imagen no disponible' : 'Cargando imagen…'}</span>}
      {children}
    </button>
  );
}
