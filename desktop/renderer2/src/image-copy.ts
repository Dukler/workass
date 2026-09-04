export interface ImageClipboardBridge {
  supported: boolean;
  copyText?: (text: string) => Promise<boolean>;
  copyImageAt: (payload: { x: number; y: number }) => Promise<boolean>;
  openImageExternal?: (payload: { bytes: ArrayBuffer; mimeType: string }) => Promise<boolean>;
}

declare global {
  interface Window { workassClipboard?: ImageClipboardBridge; }
}

interface KeyLike {
  key: string;
  metaKey: boolean;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
}

interface ClickLike {
  button: number;
  metaKey: boolean;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
}

interface RectLike {
  left: number;
  top: number;
  right: number;
  bottom: number;
  width: number;
  height: number;
}

interface SelectionLike {
  readonly isCollapsed: boolean;
  toString(): string;
}

interface ActiveSelectionLike {
  readonly tagName?: string;
  readonly selectionStart?: number | null;
  readonly selectionEnd?: number | null;
}

export function isImageCopyShortcut(event: KeyLike): boolean {
  return (event.metaKey || event.ctrlKey) && !event.altKey && !event.shiftKey && event.key.toLowerCase() === 'c';
}

// Clicking a focusable image and then dragging across transcript text leaves
// the image as document.activeElement in Chromium. Native text selection must
// therefore win explicitly; focus alone is not proof that Cmd/Ctrl+C targets
// the image. Input/textarea selections are not always exposed by getSelection,
// so preserve those through their own selection range as well.
export function imageCopyTargetForShortcut(
  image: HTMLImageElement | null,
  selection: SelectionLike | null,
  activeElement: ActiveSelectionLike | null,
): HTMLImageElement | null {
  if (!image) return null;
  if (selection && !selection.isCollapsed && selection.toString().length > 0) return null;
  const tagName = String(activeElement?.tagName || '').toUpperCase();
  if (tagName === 'INPUT' || tagName === 'TEXTAREA') {
    const start = activeElement?.selectionStart;
    const end = activeElement?.selectionEnd;
    if (typeof start === 'number' && typeof end === 'number' && end > start) return null;
  }
  return image;
}

export function isImageExternalOpenGesture(event: ClickLike, platform: string): boolean {
  if (event.button !== 0 || event.altKey || event.shiftKey) return false;
  const mac = platform.toLowerCase().includes('mac');
  return mac ? event.metaKey && !event.ctrlKey : event.ctrlKey && !event.metaKey;
}

export function imageCopyPoint(rect: RectLike, viewportWidth: number, viewportHeight: number): { x: number; y: number } | null {
  const left = Math.max(0, rect.left);
  const top = Math.max(0, rect.top);
  const right = Math.min(viewportWidth, rect.right);
  const bottom = Math.min(viewportHeight, rect.bottom);
  if (right <= left || bottom <= top || rect.width <= 0 || rect.height <= 0) return null;
  return { x: Math.round((left + right) / 2), y: Math.round((top + bottom) / 2) };
}

async function sourceAsPNG(src: string): Promise<Blob> {
  const response = await fetch(src);
  if (!response.ok) throw new Error(`image fetch failed (${response.status})`);
  const blob = await response.blob();
  if (blob.type === 'image/png') return blob;
  const bitmap = await createImageBitmap(blob);
  try {
    const canvas = document.createElement('canvas');
    canvas.width = bitmap.width;
    canvas.height = bitmap.height;
    const context = canvas.getContext('2d');
    if (!context) throw new Error('image canvas is unavailable');
    context.drawImage(bitmap, 0, 0);
    return await new Promise<Blob>((resolve, reject) => canvas.toBlob((png) => {
      if (png) resolve(png); else reject(new Error('image conversion failed'));
    }, 'image/png'));
  } finally {
    bitmap.close();
  }
}

export async function copyImageElement(image: HTMLImageElement): Promise<boolean> {
  const point = imageCopyPoint(image.getBoundingClientRect(), window.innerWidth, window.innerHeight);
  if (!point) return false;
  const bridge = window.workassClipboard;
  if (bridge?.supported) return bridge.copyImageAt(point).catch(() => false);
  if (!navigator.clipboard?.write || typeof ClipboardItem === 'undefined') return false;
  const src = image.currentSrc || image.src;
  if (!src) return false;
  try {
    // ClipboardItem accepts a promise, keeping the write inside the keyboard
    // user-activation window while a hosted/JPEG/WebP image converts to PNG.
    await navigator.clipboard.write([new ClipboardItem({ 'image/png': sourceAsPNG(src) })]);
    return true;
  } catch {
    return false;
  }
}

const MAX_EXTERNAL_IMAGE_BYTES = 16 * 1024 * 1024;

export async function openImageElementExternally(image: HTMLImageElement): Promise<boolean> {
  const bridge = window.workassClipboard;
  if (!bridge?.supported || typeof bridge.openImageExternal !== 'function') return false;
  const src = image.currentSrc || image.src;
  if (!src) return false;
  try {
    const response = await fetch(src);
    if (!response.ok && response.status !== 0) return false;
    const blob = await response.blob();
    if (blob.size <= 0 || blob.size > MAX_EXTERNAL_IMAGE_BYTES) return false;
    return await bridge.openImageExternal({ bytes: await blob.arrayBuffer(), mimeType: blob.type || 'application/octet-stream' });
  } catch {
    return false;
  }
}
