import { useEffect, useRef, useState, type CSSProperties, type ImgHTMLAttributes, type ReactNode } from 'react';
import type { MessageImage } from '../store/types';
import { messageImageSrc } from '../image-drafts';
import { call } from '../wire/api';
import { transcriptImageGate } from '../transcript-images';
import { chatImageURL } from '../chat-image-url';

// A message image the daemon sent by reference (archive reads with
// deferImages, 2026-10-13): the chat's text paints first, and each image's
// bytes load once, when it comes into view. Loaded images stay cached, so
// going back to a chat never fetches them again. Tool images share it, and a
// windowed transcript remounts rows as the reader scrolls, so it holds more.
const loaded = new Map<string, string>();
const CACHE_LIMIT = 48;

function remember(ref: string, src: string) {
  loaded.delete(ref);
  loaded.set(ref, src);
  while (loaded.size > CACHE_LIMIT) loaded.delete(loaded.keys().next().value as string);
}

// Images served by URL (chat-image-url.ts) are cached by the browser, keyed by
// URL. This set only remembers which URLs have already loaded, so a remounted
// row paints its image at once instead of fading it in again. A URL that
// failed (an expired key, an image the endpoint will not serve) is read over
// the socket instead, once per session.
const loadedURLs = new Set<string>();
const brokenURLs = new Set<string>();
const URL_MEMORY = 4096;

function rememberURL(url: string) {
  loadedURLs.delete(url);
  loadedURLs.add(url);
  while (loadedURLs.size > URL_MEMORY) loadedURLs.delete(loadedURLs.values().next().value as string);
}

function imageURL(tabId: string, image: MessageImage): string {
  const url = chatImageURL(tabId, image);
  return url && !brokenURLs.has(url) ? url : '';
}

export function cachedImageSrc(image: MessageImage, tabId?: string): string {
  if (!image.deferredImageRef) return messageImageSrc(image);
  const bytes = loaded.get(image.deferredImageRef);
  if (bytes) return bytes;
  const url = tabId === undefined ? '' : imageURL(tabId, image);
  return url && loadedURLs.has(url) ? url : '';
}

// Image reads share the daemon's history lane with history pages, one at a
// time and in arrival order. A reader scrolling a screenshot-heavy chat
// mounted thousands of tool images on the way and queued a read for each, so
// the next history page waited behind all of them (a 225 MB chat: ~6,000
// queued reads, auto-loading stalled and fell back to "Ver anteriores"). So a
// few reads are on the wire at once, the newest request goes first (what is
// on screen now), and a read whose image has left the transcript before its
// turn is dropped; it is asked for again if the image comes back.
export const IMAGE_READS_AT_ONCE = 3;

type ImageRead = {
  ref: string;
  tabId: string;
  mimeType: string;
  /** Mounted images still waiting for these bytes. */
  holders: number;
  /** An explicit request (opening the image) that must run regardless. */
  pinned: boolean;
  started: boolean;
  promise: Promise<string | null>;
  settle: (src: string | null) => void;
};

const reads = new Map<string, ImageRead>();
const waiting: ImageRead[] = []; // newest last
let reading = 0;

function startImageReads() {
  while (reading < IMAGE_READS_AT_ONCE) {
    const next = waiting.pop();
    if (!next) return;
    if (!next.pinned && next.holders <= 0) {
      reads.delete(next.ref);
      next.settle(null);
      continue;
    }
    next.started = true;
    reading++;
    call('archiveLoad', next.tabId, { imageRef: next.ref }).then((reply) => {
      const data = Array.isArray(reply) && reply[0] && typeof reply[0] === 'object'
        ? (reply[0] as { data?: unknown }).data : null;
      if (typeof data !== 'string' || !data) return null;
      const src = data.startsWith('data:') ? data : `data:${next.mimeType};base64,${data}`;
      remember(next.ref, src);
      return src;
    }).catch(() => null).then((src) => {
      reading--;
      reads.delete(next.ref);
      next.settle(src);
      startImageReads();
    });
  }
}

function queueImageRead(tabId: string, image: MessageImage, pinned: boolean): ImageRead {
  const ref = image.deferredImageRef!;
  let read = reads.get(ref);
  if (!read) {
    let settle: (src: string | null) => void = () => {};
    const promise = new Promise<string | null>((resolve) => { settle = resolve; });
    read = { ref, tabId, mimeType: image.mimeType, holders: 0, pinned: false, started: false, promise, settle };
    reads.set(ref, read);
    waiting.push(read);
  } else if (!read.started) {
    // Wanted again: it moves to the front of the line.
    const at = waiting.indexOf(read);
    if (at >= 0) waiting.splice(at, 1);
    waiting.push(read);
  }
  if (pinned) read.pinned = true;
  return read;
}

/** Reads an image's bytes now (opening it); shares any read already queued. */
export function loadImageSrc(tabId: string, image: MessageImage): Promise<string | null> {
  const ref = image.deferredImageRef;
  if (!ref) return Promise.resolve(messageImageSrc(image));
  const hit = loaded.get(ref);
  if (hit) { remember(ref, hit); return Promise.resolve(hit); }
  // By URL the viewer's own <img> fetches it (from the cache when it is shown).
  const url = imageURL(tabId, image);
  if (url) return Promise.resolve(url);
  const read = queueImageRead(tabId, image, true);
  startImageReads();
  return read.promise;
}

/**
 * Reads an image's bytes for a mounted image. release() says it no longer
 * needs them; a read nobody needs any more is dropped before it starts.
 */
export function requestImageSrc(tabId: string, image: MessageImage): { promise: Promise<string | null>; release: () => void } {
  const ref = image.deferredImageRef;
  if (!ref) return { promise: Promise.resolve(messageImageSrc(image)), release: () => {} };
  const hit = loaded.get(ref);
  if (hit) { remember(ref, hit); return { promise: Promise.resolve(hit), release: () => {} }; }
  const read = queueImageRead(tabId, image, false);
  read.holders++;
  let released = false;
  startImageReads();
  return { promise: read.promise, release: () => { if (!released) { released = true; read.holders--; } } };
}

/** Image reads queued or on the wire (for tests and diagnostics). */
export function imageReadsPending(): { waiting: number; reading: number } {
  return { waiting: waiting.length, reading };
}

// The image's src: immediate for inline bytes or a cached reference;
// otherwise it starts loading once the element is near the viewport and the
// scroll has settled (transcript-images.ts), and stops if it leaves first.
// By URL the <img> itself loads it; an older daemon's image is read over the
// socket. onLoaded/onBroken report what the <img> did with the source.
export function useMessageImageSrc(tabId: string, image: MessageImage) {
  const target = useRef<HTMLElement | null>(null);
  const [src, setSrc] = useState(() => cachedImageSrc(image, tabId));
  const [failed, setFailed] = useState(false);
  // Bumped when a URL fails, so the image is asked for again over the socket.
  const [attempt, setAttempt] = useState(0);
  const loadedRef = useRef<() => void>(() => {});
  useEffect(() => {
    const now = cachedImageSrc(image, tabId);
    setSrc(now);
    setFailed(false);
    loadedRef.current = () => {};
    if (now || !image.deferredImageRef || !target.current) return;
    let current = true;
    let request: ReturnType<typeof requestImageSrc> | null = null;
    const url = imageURL(tabId, image);
    const gate = transcriptImageGate().watch(target.current, {
      start: () => {
        if (!current) return;
        if (url) { setSrc(url); return; }
        const mine = requestImageSrc(tabId, image);
        request = mine;
        void mine.promise.then((next) => {
          // A read given up on (the image left first) settles without bytes;
          // that is not a failure. Bytes it did bring are cached for next time.
          if (!current || request !== mine) return;
          if (next) { setSrc(next); gate.loaded(); } else setFailed(true);
        });
      },
      cancel: () => {
        if (!current) return;
        // An <img> that loses its src aborts its fetch; the frame keeps its size.
        if (url) setSrc((value) => (value === url ? '' : value));
        request?.release();
        request = null;
      },
    });
    loadedRef.current = gate.loaded;
    return () => { current = false; gate.stop(); request?.release(); };
  }, [tabId, image, attempt]);
  const onLoaded = (loadedSrc: string) => {
    if (loadedSrc && !loadedSrc.startsWith('data:')) rememberURL(loadedSrc);
    loadedRef.current();
  };
  // A URL the <img> could not load (an expired key, an image the endpoint will
  // not serve) is read over the socket instead: true means that is under way.
  const onBroken = (brokenSrc: string) => {
    if (!brokenSrc || brokenSrc.startsWith('data:') || brokenURLs.has(brokenSrc)) return false;
    brokenURLs.add(brokenSrc);
    loadedURLs.delete(brokenSrc);
    setAttempt((value) => value + 1);
    return true;
  };
  return { target, src, failed, onLoaded, onBroken };
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
  const { target, src, failed, onLoaded, onBroken } = useMessageImageSrc(tabId, image);
  const [readySrc, setReadySrc] = useState(() => cachedImageSrc(image, tabId));
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
    onLoaded(loadedSrc);
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
        {...(src.startsWith('data:') ? {} : { loading: 'lazy' as const })}
        onLoad={(event) => settle(event.currentTarget, src)}
        onError={() => { if (!onBroken(src)) setBrokenSrc(src); }}
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
