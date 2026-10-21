import { useEffect, useRef, useState, type CSSProperties, type ImgHTMLAttributes, type ReactNode } from 'react';
import type { MessageImage } from '../store/types';
import { messageImageSrc } from '../image-drafts';
import { call } from '../wire/api';
import { observeTranscriptImageOnce } from '../transcript-images';

// A message image the daemon sent by reference (archive reads with
// deferImages, 2026-10-13): the chat's text paints first, and each image's
// bytes load once, when it comes into view. Loaded images stay cached, so
// going back to a chat never fetches them again. Tool images share it, and a
// windowed transcript remounts rows as the reader scrolls, so it holds more.
const loaded = new Map<string, string>();
const pending = new Map<string, Promise<string | null>>();
const CACHE_LIMIT = 48;

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

// ── Reserved image frame (approved mock image-placeholder, variant C) ──────
// The frame owns the image's size from the first paint: --iw/--ih are its
// pixel size from the wire (daemon `width`/`height`), each surface keeps
// today's caps in CSS, and the media box carries the exact aspect ratio, so
// swapping the placeholder for the image changes no layout. Without a wire
// size (an older daemon) the frame uses one default per surface and corrects
// once, to the image's natural size, when it loads.
export type ImageSurface = 'user' | 'inline' | 'gallery' | 'tool';
export type ImageFrameState = 'pending' | 'loaded' | 'failed';

export const IMAGE_FRAME_FALLBACK: Record<ImageSurface, readonly [number, number]> = {
  user: [640, 480], inline: [1600, 900], gallery: [1200, 900], tool: [1200, 900],
};

function pixelSize(width: unknown, height: unknown): [number, number] | null {
  return typeof width === 'number' && typeof height === 'number'
    && Number.isFinite(width) && Number.isFinite(height) && width > 0 && height > 0
    ? [width, height] : null;
}

// The CSS variables that size an image's frame: its natural size once known,
// else its wire size, else the surface default.
export function imageFrameStyle(image: MessageImage, surface: ImageSurface, natural?: readonly [number, number] | null): CSSProperties {
  const [width, height] = natural ?? pixelSize(image.width, image.height) ?? IMAGE_FRAME_FALLBACK[surface];
  return { '--iw': width, '--ih': height } as CSSProperties;
}

// Natural sizes measured on load, so a remounted row (the windowed transcript
// remounts as the reader scrolls) paints its final frame at once even when the
// wire carried no size.
const naturalByRef = new Map<string, readonly [number, number]>();
const naturalByImage = new WeakMap<MessageImage, readonly [number, number]>();
const NATURAL_LIMIT = 1024;

function knownNaturalSize(image: MessageImage): readonly [number, number] | null {
  return (image.deferredImageRef ? naturalByRef.get(image.deferredImageRef) : naturalByImage.get(image)) ?? null;
}

function rememberNaturalSize(image: MessageImage, size: readonly [number, number]) {
  if (!image.deferredImageRef) { naturalByImage.set(image, size); return; }
  naturalByRef.delete(image.deferredImageRef);
  naturalByRef.set(image.deferredImageRef, size);
  while (naturalByRef.size > NATURAL_LIMIT) naturalByRef.delete(naturalByRef.keys().next().value as string);
}

// failed: the read by reference failed, or the browser could not decode the
// bytes; loaded: the <img> for the current source has loaded (or the source
// was in hand on the first render); pending otherwise.
export function reservedImageState(readFailed: boolean, src: string, readySrc: string, brokenSrc: string): ImageFrameState {
  if (readFailed || (!!src && brokenSrc === src)) return 'failed';
  return src && readySrc === src ? 'loaded' : 'pending';
}

// One image's frame state. Bytes already in hand on the first render (inline
// data, a cached reference) paint as loaded, exactly as before; bytes that
// arrive later fade in over the reserved frame.
export function useReservedImage(tabId: string, image: MessageImage, surface: ImageSurface) {
  const { target, src, failed } = useMessageImageSrc(tabId, image);
  const [readySrc, setReadySrc] = useState(() => cachedImageSrc(image));
  const [brokenSrc, setBrokenSrc] = useState('');
  const [measured, setMeasured] = useState<{ src: string; size: readonly [number, number] } | null>(null);
  const media = useRef<HTMLImageElement | null>(null);
  const natural = (measured && measured.src === src ? measured.size : null) ?? knownNaturalSize(image);
  const settle = (element: HTMLImageElement, loadedSrc: string) => {
    const size = pixelSize(element.naturalWidth, element.naturalHeight);
    if (size) {
      rememberNaturalSize(image, size);
      setMeasured((current) => current && current.src === loadedSrc && current.size[0] === size[0] && current.size[1] === size[1]
        ? current : { src: loadedSrc, size });
    }
    setReadySrc(loadedSrc);
  };
  // A load that finished before its listener was attached still settles.
  useEffect(() => {
    const element = media.current;
    if (src && readySrc !== src && element?.complete && element.naturalWidth > 0) settle(element, src);
  });
  const state = reservedImageState(failed, src, readySrc, brokenSrc);
  const img = (props: ImgHTMLAttributes<HTMLImageElement>) => src && state !== 'failed'
    ? (
      <img
        {...props}
        ref={media}
        src={src}
        decoding="async"
        onLoad={(event) => settle(event.currentTarget, src)}
        onError={() => setBrokenSrc(src)}
      />
    )
    : null;
  return { target, src, state, style: imageFrameStyle(image, surface, natural), img };
}

const IMAGE_GLYPH = (
  <svg viewBox="0 0 16 16" aria-hidden="true"><rect x="2" y="3" width="12" height="10" rx="1.6" /><circle cx="5.8" cy="6.4" r="1.1" /><path d="M2.6 11.6 6.4 8.4l2.4 2 1.8-1.4 2.8 2.6" /></svg>
);
const BROKEN_GLYPH = (
  <svg viewBox="0 0 16 16" aria-hidden="true"><rect x="2" y="3" width="12" height="10" rx="1.6" /><path d="M2.5 2.5 13.5 13.5" /></svg>
);

// The placeholder that fills the frame until the image is drawn. It keeps
// today's copy and the pending classes as aliases.
export function ImagePlaceholder({ state, alias }: { state: ImageFrameState; alias: string }) {
  const failed = state === 'failed';
  const label = failed ? 'Imagen no disponible' : 'Cargando imagen…';
  return (
    <span
      className={`ri-ph ${alias}`}
      {...(state === 'loaded' ? { 'aria-hidden': true } : { role: 'img', 'aria-label': failed ? 'Imagen no disponible' : 'Cargando imagen' })}
    >
      {failed ? BROKEN_GLYPH : IMAGE_GLYPH}
      <span className="cap">{label}</span>
    </span>
  );
}

// One clickable image of a message: the reply gallery or an inline Markdown
// image. Clicking opens the loaded image.
export function MessageImageButton({ tabId, image, alt, className, onOpen, surface = 'inline', children }: {
  tabId: string;
  image: MessageImage;
  alt: string;
  className: string;
  onOpen: (src: string) => void;
  surface?: 'inline' | 'gallery';
  children?: ReactNode;
}) {
  const { target, src, state, style, img } = useReservedImage(tabId, image, surface);
  return (
    <button
      ref={(element) => { target.current = element; }}
      type="button"
      className={`${className} ri ri-${surface}`}
      data-state={state}
      style={style}
      title={src ? 'Ampliar' : alt}
      disabled={!src}
      onClick={() => { if (src) onOpen(src); }}
    >
      <span className="ri-media">
        {img({ alt })}
        <ImagePlaceholder state={state} alias="assistant-image-pending" />
      </span>
      {children}
    </button>
  );
}
