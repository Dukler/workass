import type { MachineRegistry } from './wire/machineRegistry.ts';
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
const SAFE_HEADERS = new Set(['range', 'if-range', 'if-none-match', 'if-modified-since']);
function validPath(path: string): boolean {
  const pathname = path.split('?')[0];
  return pathname.startsWith(PREFIX) && !/[\\\r\n\0]/u.test(pathname)
    && !pathname.split('/').some((part) => part === '.' || part === '..')
    && !/%(?:2f|2e|5c)/iu.test(pathname);
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
): string {
  const id = String(machineId ?? '').trim();
  const raw = String(target ?? '').trim();
  if (raw.startsWith(PREFIX) && !validPath(raw)) return '';
  if (!id) return raw;
  let url: URL;
  try { url = new URL(raw, 'http://workass.invalid'); } catch { return ''; }
  if (!url.pathname.startsWith(PREFIX)) return raw;
  if (!supported || !validPath(url.pathname)) return '';
  const rest = url.pathname.slice(PREFIX.length);
  const slash = rest.indexOf('/');
  const artifact = slash < 0 ? rest : rest.slice(0, slash);
  if (!/^[A-Za-z0-9._~-]{1,200}$/u.test(artifact) || artifact === '.' || artifact === '..') return '';
  const tail = slash < 0 ? '' : rest.slice(slash + 1);
  const base = String(origin ?? '').trim().replace(/\/+$/, '');
  return `${base}/workass/connected-artifacts/${encodeURIComponent(id)}/${artifact}/${tail}${url.search}${url.hash}`;
}

type Registry = Pick<MachineRegistry, 'linkFor' | 'ownsLink'>;
type Transfer = { machineId: string; link: MachineSocket; transferId: string };
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
    if (registry.ownsLink(transfer.machineId, transfer.link)) {
      void transfer.link.invoke('artifact:close', { transferId: transfer.transferId }).catch(() => {});
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
      const link = request.op === 'open' ? registry.linkFor(machineId) : transfer?.link;
      if (request.op === 'close' && !transfer) {
        send(requestId, { ok: true });
        return;
      }
      if (!link || !registry.ownsLink(machineId, link)) throw new Error('artifact machine unavailable or stale');
      entry.transfer = transfer;
      const result = await link.invoke<Record<string, unknown>>(`artifact:${request.op}`,
        request.op === 'open' ? safeRequest(request) : { transferId: request.transferId });
      if (request.op === 'open' && typeof result?.transferId === 'string') {
        entry.transfer = { machineId, link, transferId: result.transferId };
      }
      if (!registry.ownsLink(machineId, link)) throw new Error('artifact machine unavailable or stale');
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
