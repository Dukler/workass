import type { DraftImage, MessageImage, QueuedMsg } from './store/types';

// Chromium normally mirrors clipboard images into DataTransfer.files, but some
// screenshot sources expose them only as DataTransferItem entries. Prefer the
// direct files to avoid duplicates; fall back to getAsFile when needed.
export function clipboardImageFiles(data: Pick<DataTransfer, 'files' | 'items'>): File[] {
  const direct = Array.from(data.files ?? []).filter((file) => file.type.startsWith('image/'));
  if (direct.length) return direct;
  return Array.from(data.items ?? [])
    .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
    .map((item) => item.getAsFile())
    .filter((file): file is File => !!file && file.type.startsWith('image/'));
}

export function imageBase64(dataURL: string): string {
  const comma = dataURL.indexOf(',');
  return comma >= 0 ? dataURL.slice(comma + 1) : dataURL;
}

// These limits mirror the daemon's provider boundary. The renderer must fit
// ordinary pasted/chosen images before it creates a visible turn; the daemon
// repeats the check as defense in depth for remote and older clients.
export const MAX_ATTACHED_IMAGES = 6;
export const MAX_ATTACHMENT_IMAGE_BASE64_BYTES = 8 * 1024 * 1024;
export const MAX_ATTACHMENT_TOTAL_BASE64_BYTES = 16 * 1024 * 1024;

const MAX_NORMALIZED_IMAGE_EDGE = 4096;
const MIN_NORMALIZED_IMAGE_EDGE = 320;
const SAFE_RASTER_MIME = new Set(['image/png', 'image/jpeg', 'image/webp', 'image/gif']);
const NORMALIZED_WEBP_QUALITIES = [0.92, 0.82, 0.72, 0.62, 0.5];

export interface PreparedImagePayload {
  mimeType: string;
  data: string;
  name?: string;
}

export type DraftImageNormalizer = (draft: DraftImage, maxBase64Bytes: number) => Promise<PreparedImagePayload>;

type ObjectURLFactory = (blob: Blob) => string;
type ObjectURLRevoker = (url: string) => void;

// Attachment selection must stay a cheap synchronous UI operation. Object URLs
// point at the browser-owned File bytes without cloning or base64 expansion, so
// previews can render in the same commit even for several large screenshots.
export function createDraftImages(files: File[], createObjectURL: ObjectURLFactory = (blob) => URL.createObjectURL(blob)): DraftImage[] {
  const now = Date.now();
  const drafts: DraftImage[] = [];
  for (const [index, file] of files.entries()) {
    if (!file.type.startsWith('image/')) continue;
    try {
      drafts.push({
        id: `${now}-${index}-${Math.random().toString(36).slice(2, 8)}`,
        name: file.name || 'imagen',
        mimeType: file.type,
        file,
        url: createObjectURL(file),
      });
    } catch {
      // One unreadable item must not prevent the remaining previews appearing.
    }
  }
  return drafts;
}

export function releaseDraftImages(images: DraftImage[], revokeObjectURL: ObjectURLRevoker = (url) => URL.revokeObjectURL(url)) {
  for (const image of images) {
    if (!image.url.startsWith('blob:')) continue;
    try { revokeObjectURL(image.url); } catch { /* already released */ }
  }
}

function readFileDataURL(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result ?? ''));
    reader.onerror = () => reject(reader.error ?? new Error('No se pudo leer la imagen.'));
    reader.onabort = () => reject(new Error('Se canceló la lectura de la imagen.'));
    reader.readAsDataURL(file);
  });
}

function base64LengthForBytes(bytes: number): number {
  return Math.ceil(Math.max(0, bytes) / 3) * 4;
}

function estimatedDraftBase64Length(draft: DraftImage): number {
  if (draft.data) return draft.data.length;
  return draft.file ? base64LengthForBytes(draft.file.size) : 0;
}

function readBlobDataURL(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result ?? ''));
    reader.onerror = () => reject(reader.error ?? new Error('No se pudo codificar la imagen reducida.'));
    reader.onabort = () => reject(new Error('Se canceló la codificación de la imagen reducida.'));
    reader.readAsDataURL(blob);
  });
}

async function draftBlob(draft: DraftImage): Promise<Blob> {
  if (draft.file) return draft.file;
  if (!draft.data) throw new Error(`No se pudo leer ${draft.name || 'la imagen'}.`);
  const response = await fetch(`data:${draft.mimeType};base64,${draft.data}`);
  if (!response.ok) throw new Error(`No se pudo decodificar ${draft.name || 'la imagen'}.`);
  return response.blob();
}

function canvasBlob(canvas: HTMLCanvasElement, mimeType: string, quality: number): Promise<Blob> {
  return new Promise((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (blob) resolve(blob);
      else reject(new Error('El navegador no pudo comprimir la imagen.'));
    }, mimeType, quality);
  });
}

// Oversized camera photos and lossless screenshots are normalized locally so
// the user-visible attachment and the provider input have one successful
// ownership boundary. The full-resolution File remains behind the object URL
// until admission; only the bounded WebP crosses renderer/daemon/ACP JSON.
export async function normalizeDraftImage(
  draft: DraftImage,
  maxBase64Bytes: number,
): Promise<PreparedImagePayload> {
  if (maxBase64Bytes <= 0 || typeof document === 'undefined' || typeof createImageBitmap !== 'function') {
    throw new Error(`La imagen ${draft.name || ''} supera el límite seguro y no se pudo reducir.`.trim());
  }
  const bitmap = await createImageBitmap(await draftBlob(draft));
  try {
    const longest = Math.max(bitmap.width, bitmap.height);
    if (!Number.isFinite(longest) || longest <= 0) throw new Error('La imagen no tiene dimensiones válidas.');
    let scale = Math.min(1, MAX_NORMALIZED_IMAGE_EDGE / longest);
    const canvas = document.createElement('canvas');
    const context = canvas.getContext('2d', { alpha: true });
    if (!context) throw new Error('El navegador no pudo preparar la imagen.');

    while (Math.max(1, Math.round(longest * scale)) >= MIN_NORMALIZED_IMAGE_EDGE) {
      canvas.width = Math.max(1, Math.round(bitmap.width * scale));
      canvas.height = Math.max(1, Math.round(bitmap.height * scale));
      context.clearRect(0, 0, canvas.width, canvas.height);
      context.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
      for (const quality of NORMALIZED_WEBP_QUALITIES) {
        const blob = await canvasBlob(canvas, 'image/webp', quality);
        if (base64LengthForBytes(blob.size) > maxBase64Bytes) continue;
        const data = imageBase64(await readBlobDataURL(blob));
        if (data && data.length <= maxBase64Bytes) {
          return { mimeType: 'image/webp', data, name: draft.name };
        }
      }
      scale *= 0.75;
    }
  } finally {
    bitmap.close();
  }
  throw new Error(`La imagen ${draft.name || ''} supera el límite seguro incluso después de reducirla.`.trim());
}

// Encoding is deliberately a send-boundary concern. Read sequentially to avoid
// the old Promise.all peak where every expanded base64 result landed together.
// The injected reader keeps the transform deterministic in the Node test suite.
export async function draftImagePayloads(
  drafts: DraftImage[],
  readDataURL: (file: File) => Promise<string> = readFileDataURL,
  yieldBetween: () => Promise<void> = async () => {},
  normalize: DraftImageNormalizer = normalizeDraftImage,
): Promise<PreparedImagePayload[]> {
  if (drafts.length > MAX_ATTACHED_IMAGES) {
    throw new Error(`Podés adjuntar hasta ${MAX_ATTACHED_IMAGES} imágenes por mensaje.`);
  }
  const estimates = drafts.map(estimatedDraftBase64Length);
  const batchNeedsNormalization = estimates.some((size) => size > MAX_ATTACHMENT_IMAGE_BASE64_BYTES)
    || estimates.reduce((total, size) => total + size, 0) > MAX_ATTACHMENT_TOTAL_BASE64_BYTES;
  const payloads: PreparedImagePayload[] = [];
  let total = 0;
  for (const [index, draft] of drafts.entries()) {
    const remaining = drafts.length - index;
    const remainingBudget = MAX_ATTACHMENT_TOTAL_BASE64_BYTES - total;
    const fairBatchBudget = batchNeedsNormalization ? Math.floor(remainingBudget / remaining) : MAX_ATTACHMENT_IMAGE_BASE64_BYTES;
    const imageBudget = Math.min(MAX_ATTACHMENT_IMAGE_BASE64_BYTES, fairBatchBudget);
    if (imageBudget <= 0) throw new Error('Las imágenes adjuntas superan el límite seguro de Workass.');

    let data = draft.data ?? '';
    let mimeType = draft.mimeType.toLowerCase();
    const mustNormalize = !SAFE_RASTER_MIME.has(mimeType)
      || estimatedDraftBase64Length(draft) > imageBudget;
    if (mustNormalize) {
      ({ data, mimeType } = await normalize(draft, imageBudget));
      mimeType = mimeType.toLowerCase();
    } else if (!data && draft.file) {
      data = imageBase64(await readDataURL(draft.file));
    }
    if (!SAFE_RASTER_MIME.has(mimeType) || !data) {
      throw new Error(`No se pudo preparar ${draft.name || 'una de las imágenes'} como PNG, JPEG, WebP o GIF.`);
    }
    if (data.length > imageBudget || total + data.length > MAX_ATTACHMENT_TOTAL_BASE64_BYTES) {
      const normalized = await normalize(draft, imageBudget);
      mimeType = normalized.mimeType.toLowerCase();
      data = normalized.data;
    }
    if (!SAFE_RASTER_MIME.has(mimeType) || !data || data.length > imageBudget
      || total + data.length > MAX_ATTACHMENT_TOTAL_BASE64_BYTES) {
      throw new Error(`La imagen ${draft.name || ''} supera el límite seguro de Workass.`.trim());
    }
    payloads.push({ mimeType, data, name: draft.name });
    total += data.length;
    await yieldBetween();
  }
  return payloads;
}

// Let the preview/queue commit paint before starting base64 work, then yield
// between files. FileReader itself is asynchronous, but allocating a very large
// result and updating React can otherwise monopolize consecutive frames.
export function attachmentWorkBoundary(): Promise<void> {
  return new Promise((resolve) => {
    if (typeof requestAnimationFrame === 'function') {
      requestAnimationFrame(() => setTimeout(resolve, 0));
      return;
    }
    setTimeout(resolve, 0);
  });
}

export function appendDraftImages(current: DraftImage[] | undefined, added: DraftImage[]): DraftImage[] {
  return [...(current ?? []), ...added];
}

export function withoutDraftImages(current: DraftImage[] | undefined, ids: Iterable<string>): DraftImage[] {
  const remove = new Set(ids);
  return (current ?? []).filter((image) => !remove.has(image.id));
}

// Copy the exact accepted payload into transcript-safe records. Invalid/empty
// items never create broken image elements, and no object is shared with the
// mutable draft array.
export function messageImages(images: Array<{ mimeType: string; data: string; name?: string; source?: string }> | undefined): MessageImage[] | undefined {
  const accepted = (images ?? [])
    .filter((image) => image.mimeType.startsWith('image/') && image.data.length > 0)
    .map((image) => ({
      mimeType: image.mimeType,
      data: image.data,
      ...(image.name ? { name: image.name } : {}),
      ...(image.source ? { source: image.source } : {}),
    }));
  return accepted.length ? accepted : undefined;
}

export function mergeMessageImages(
  current: MessageImage[] | undefined,
  added: Array<{ mimeType: string; data: string; name?: string; source?: string }> | undefined,
): MessageImage[] | undefined {
  const accepted = messageImages([...(current ?? []), ...(added ?? [])]);
  if (!accepted) return undefined;
  const seen = new Set<string>();
  return accepted.filter((image) => {
    const key = image.source || `${image.mimeType}\u0000${image.data}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

export function messageImageSrc(image: MessageImage): string {
  return `data:${image.mimeType};base64,${image.data}`;
}

// Queue attachment lifecycle. Keeping these transforms pure makes it explicit
// that the image payload stays bound to one queue item and is transferred with
// that item into the canonical transcript turn in one ownership change.
export function queuedMessage(id: string, text: string, images?: MessageImage[]): QueuedMsg {
  const accepted = messageImages(images);
  return { id, text, images: accepted, attachmentState: accepted?.length ? 'ready' : undefined };
}

export function queuedDraftMessage(id: string, text: string, drafts: DraftImage[]): QueuedMsg {
  return {
    id,
    text,
    draftImages: drafts,
    attachmentNames: drafts.map((draft) => draft.name),
    attachmentState: drafts.length ? 'preparing' : undefined,
  };
}

export function queuedAttachmentsReady(item: QueuedMsg): boolean {
  return item.attachmentState !== 'preparing' && item.attachmentState !== 'failed';
}

// Renderer-owned follow-ups can outlive the job:end event that normally drains
// them (for example when an ACP child dies while the view is reconnecting).
// Recheck that head after hydration/reconnect, but never steal daemon-owned
// agent/host rows or dispatch an attachment that is not ready yet.
export function shouldDrainRecoveredQueue(queue: QueuedMsg[] | undefined, running: boolean): boolean {
  if (running) return false;
  const head = queue?.[0];
  if (!head || head.source === 'agent' || head.source === 'host') return false;
  return queuedAttachmentsReady(head);
}

export function queuedJob(item: QueuedMsg): { prompt: string; images?: MessageImage[] } {
  return { prompt: item.text, images: item.images };
}

export function afterQueuedAcceptance(queue: QueuedMsg[], id: string, accepted: boolean): QueuedMsg[] {
  return accepted ? queue.filter((item) => item.id !== id) : queue;
}
