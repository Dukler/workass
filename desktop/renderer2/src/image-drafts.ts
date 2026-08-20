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

// Encoding is deliberately a send-boundary concern. Read sequentially to avoid
// the old Promise.all peak where every expanded base64 result landed together.
// The injected reader keeps the transform deterministic in the Node test suite.
export async function draftImagePayloads(
  drafts: DraftImage[],
  readDataURL: (file: File) => Promise<string> = readFileDataURL,
  yieldBetween: () => Promise<void> = async () => {},
): Promise<Array<{ mimeType: string; data: string; name?: string }>> {
  const payloads: Array<{ mimeType: string; data: string; name?: string }> = [];
  for (const draft of drafts) {
    let data = draft.data ?? '';
    if (!data && draft.file) data = imageBase64(await readDataURL(draft.file));
    if (!draft.mimeType.startsWith('image/') || !data) continue;
    payloads.push({ mimeType: draft.mimeType, data, name: draft.name });
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
