import type { MachineRegistry } from './wire/machineRegistry.ts';
import type { WorkassApi } from './wire/types.ts';
import type { MachineSocket } from './wire/machineSocket.ts';

export interface ArtifactRequest {
  op: 'open' | 'read' | 'close';
  requestId: string;
  machineId: string;
  path?: string;
  method?: 'GET' | 'HEAD';
  headers?: Record<string, string>;
  transferId?: string;
}
export interface ArtifactReply {
  requestId: string;
  ok: boolean;
  error?: string;
  errorCode?: 'unsupported';
  transferId?: string;
  status?: number;
  headers?: Record<string, string>;
  bodyBase64?: string;
  eof?: boolean;
}
export interface WorkassArtifactsBridge {
  supported: boolean;
  onRequest(cb: (request: ArtifactRequest) => void): () => void;
  onCancel(cb: (request: { requestId: string }) => void): () => void;
  reply(payload: ArtifactReply): Promise<unknown>;
}

const PREFIX = '/workass/artifacts/';
const LEGACY_PREFIX = '/workass/connected-artifacts/';
const SAFE_HEADERS = new Set(['range', 'if-range', 'if-none-match', 'if-modified-since']);
function validPath(path: string): boolean {
  const pathname = path.split('?')[0];
  return pathname.startsWith(PREFIX) && !/[\\\r\n\0]/u.test(pathname)
    && !pathname.split('/').some((part) => part === '.' || part === '..')
    && !/%(?:2f|2e|5c)/iu.test(pathname);
}
function validPart(value: string): boolean {
  return /^[A-Za-z0-9._~-]{1,200}$/u.test(value) && value !== '.' && value !== '..';
}
function artifactLinkPath(target: string): string | undefined {
  const raw = String(target ?? '').trim();
  const path = raw.startsWith('/') ? raw.split(/[?#]/u)[0]
    : raw.match(/^https?:\/\/[^/?#]+(\/[^?#]*)/iu)?.[1];
  return path?.startsWith(PREFIX) || path?.startsWith(LEGACY_PREFIX) ? path : undefined;
}
export const isArtifactLink = (target: string): boolean => artifactLinkPath(target) !== undefined;
export interface ParsedArtifactLink { machineId?: string; artifactId: string; path: string; query: string; hash: string; }
/** Validate before URL normalization can erase traversal or change ownership. */
export function parseArtifactLink(target: string): ParsedArtifactLink | undefined {
  const raw = String(target ?? '').trim();
  const rawPath = artifactLinkPath(raw);
  if (!rawPath || /[\\\r\n\0]/u.test(rawPath) || /%(?:2e|2f|5c)/iu.test(rawPath)
    || rawPath.split('/').some((part) => part === '.' || part === '..')) return undefined;
  let url: URL;
  try { url = new URL(raw, 'http://workass.invalid'); } catch { return undefined; }
  const alias = rawPath.startsWith(LEGACY_PREFIX);
  const parts = rawPath.slice((alias ? LEGACY_PREFIX : PREFIX).length).split('/');
  let machineId: string | undefined;
  if (alias) machineId = parts.shift();
  else if (parts[0]?.startsWith('@')) machineId = parts.shift()!.slice(1);
  const artifactId = parts.shift() ?? '';
  if ((machineId !== undefined && !validPart(machineId)) || !validPart(artifactId)
    || parts.some((part, index) => !part && index !== parts.length - 1)) return undefined;
  return { machineId, artifactId, path: parts.length ? `/${parts.join('/')}` : '', query: url.search, hash: url.hash };
}
export function artifactOwner(target: string, fallbackMachineId = ''): string {
  return parseArtifactLink(target)?.machineId ?? String(fallbackMachineId ?? '').trim();
}
function safeRequest(request: ArtifactRequest) {
  const path = String(request.path ?? '');
  const method = request.method ?? 'GET';
  if (!validPath(path)) throw new Error('invalid artifact path');
  if (method !== 'GET' && method !== 'HEAD') throw new Error('invalid artifact method');
  const headers: Record<string, string> = {};
  for (const [key, value] of Object.entries(request.headers ?? {})) {
    if (SAFE_HEADERS.has(key.toLowerCase()) && typeof value === 'string'
      && value.length <= 4096 && !/[\r\n\0]/u.test(value)) headers[key] = value;
  }
  return { path, method, headers };
}

export function connectedArtifactURL(
  machineId: string, target: string,
  origin = typeof window !== 'undefined' ? window.location.origin : '',
  supported = typeof window !== 'undefined' && window.workassArtifacts?.supported === true,
  localMachineId = '',
): string {
  const raw = String(target ?? '').trim();
  const parsed = parseArtifactLink(raw);
  if (!parsed) return isArtifactLink(raw) ? '' : raw;
  const owner = parsed.machineId ?? String(machineId ?? '').trim();
  if (!owner) return raw;
  if (!validPart(owner) || (!supported && owner !== localMachineId)) return '';
  const base = String(origin ?? '').trim().replace(/\/+$/, '');
  return `${base}${PREFIX}@${owner}/${parsed.artifactId}${parsed.path}${parsed.query}${parsed.hash}`;
}

type Registry = Pick<MachineRegistry, 'linkFor' | 'ownsLink'> & {
  local?(): WorkassApi | undefined;
  localMachineId?(): string;
};
type Transfer = { machineId: string; link?: MachineSocket; api?: WorkassApi; generation?: number; transferId: string };
type Pending = { cancelled: boolean; transfer?: Transfer };
const transferKey = (machineId: string, transferId: string) => JSON.stringify([machineId, transferId]);

export function installWorkassArtifactsBridge(registry: Registry, bridge?: WorkassArtifactsBridge): () => void {
  const api = bridge ?? (typeof window !== 'undefined' ? window.workassArtifacts : undefined);
  if (!api?.supported) return () => {};
  const pending = new Map<string, Pending>();
  const transfers = new Map<string, Transfer>();
  let disposed = false;
  const release = (transfer: Transfer) => {
    transfers.delete(transferKey(transfer.machineId, transfer.transferId));
    // Never replay a transfer identifier against a replacement connection.
    if (transfer.link && registry.ownsLink(transfer.machineId, transfer.link)) {
      void transfer.link.invoke('artifact:close', { transferId: transfer.transferId }).catch(() => {});
    } else if (transfer.api && transfer.machineId === String(registry.localMachineId?.() ?? '').trim()
      && transfer.api === registry.local?.()
      && transfer.api.artifactConnectionGeneration?.() === transfer.generation
      && transfer.api.artifactClose) {
      void transfer.api.artifactClose({ transferId: transfer.transferId }).catch(() => {});
    }
  };
  const send = (requestId: string, payload: Omit<ArtifactReply, 'requestId'>) => {
    if (!disposed) void api.reply({ ...payload, requestId }).catch(() => {});
  };
  const onRequest = async (request: ArtifactRequest) => {
    const machineId = String(request.machineId ?? '').trim();
    const requestId = String(request.requestId ?? '').trim();
    if (disposed || !machineId || !requestId || pending.has(requestId)) return;
    const entry: Pending = { cancelled: false };
    pending.set(requestId, entry);
    try {
      if (!['open', 'read', 'close'].includes(request.op)) throw new Error('unsupported artifact operation');
      const key = transferKey(machineId, String(request.transferId ?? ''));
      const transfer = request.op === 'open' ? undefined : transfers.get(key);
      entry.transfer = transfer;
      const local = machineId === String(registry.localMachineId?.() ?? '').trim();
      const api = local ? registry.local?.() : undefined;
      const generation = api?.artifactConnectionGeneration?.() ?? 0;
      const link = request.op === 'open' ? (local ? undefined : registry.linkFor(machineId)) : transfer?.link;
      const localTransfer = request.op === 'open' ? undefined : transfer?.api;
      if (request.op === 'close' && !transfer) {
        send(requestId, { ok: true });
        return;
      }
      if (local) {
        if (!api || generation <= 0 || !api.artifactOpen || !api.artifactRead || !api.artifactClose) throw new Error('artifact machine unavailable or stale');
      } else if (!link || !registry.ownsLink(machineId, link)) throw new Error('artifact machine unavailable or stale');
      if (local && request.op !== 'open' && (!localTransfer || localTransfer !== api
        || transfer?.generation !== generation || transfer?.machineId !== machineId)) throw new Error('artifact machine unavailable or stale');
      const result = (local
        ? await (request.op === 'open' ? api!.artifactOpen!(safeRequest(request)) : request.op === 'read' ? api!.artifactRead!({ transferId: request.transferId! }) : api!.artifactClose!({ transferId: request.transferId! }))
        : await link!.invoke<Record<string, unknown>>(`artifact:${request.op}`, request.op === 'open' ? safeRequest(request) : { transferId: request.transferId })) as Record<string, unknown>;
      if (request.op === 'open' && typeof result?.transferId === 'string') {
        entry.transfer = local ? { machineId, api, generation, transferId: result.transferId } : { machineId, link, transferId: result.transferId };
      }
      if (local) {
        if (machineId !== String(registry.localMachineId?.() ?? '').trim()
          || api !== registry.local?.() || api!.artifactConnectionGeneration?.() !== generation || generation <= 0) throw new Error('artifact machine unavailable or stale');
      } else if (!link || !registry.ownsLink(machineId, link)) throw new Error('artifact machine unavailable or stale');
      if (entry.cancelled || disposed) {
        if (entry.transfer) release(entry.transfer);
        return;
      }
      if (request.op === 'open') {
        if (!entry.transfer || entry.transfer.transferId.length > 200
          || !Number.isInteger(result.status) || Number(result.status) < 200 || Number(result.status) > 599
          || !result.headers || typeof result.headers !== 'object' || Array.isArray(result.headers)) throw new Error('invalid artifact response');
        transfers.set(transferKey(machineId, entry.transfer.transferId), entry.transfer);
        send(requestId, { ok: true, transferId: entry.transfer.transferId, status: Number(result.status), headers: result.headers as Record<string, string> });
      } else if (request.op === 'read') {
        if (typeof result.bodyBase64 !== 'string' || result.bodyBase64.length > 174764
          || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/u.test(result.bodyBase64)
          || typeof result.eof !== 'boolean') throw new Error('invalid artifact chunk');
        send(requestId, { ok: true, bodyBase64: result.bodyBase64, eof: result.eof });
        if (result.eof) transfers.delete(key);
      } else {
        transfers.delete(key);
        send(requestId, { ok: true });
      }
    } catch (error) {
      if (entry.transfer) release(entry.transfer);
      if (!entry.cancelled) send(requestId, {
        ok: false, error: 'artifact machine unavailable or response invalid',
        ...(error instanceof Error && error.message.startsWith('unknown channel: artifact:') ? { errorCode: 'unsupported' as const } : {}),
      });
    } finally {
      pending.delete(requestId);
    }
  };
  const offRequest = api.onRequest(onRequest);
  const offCancel = api.onCancel(({ requestId }) => {
    const entry = pending.get(requestId);
    if (!entry) return;
    entry.cancelled = true;
    if (entry.transfer) release(entry.transfer);
  });
  return () => {
    disposed = true;
    offRequest();
    offCancel();
    for (const entry of pending.values()) entry.cancelled = true;
    for (const transfer of transfers.values()) release(transfer);
    transfers.clear();
  };
}

declare global { interface Window { workassArtifacts?: WorkassArtifactsBridge; } }
