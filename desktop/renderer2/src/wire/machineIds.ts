// Ids are namespaced at the socket boundary, and nowhere else.
//
// remote-plan E3: the frozen wire contract does not grow a `machineId` argument
// on any channel. Instead every id crossing INTO the client from a remote
// machine is tagged, and every id crossing back OUT is stripped. A daemon
// therefore never sees an id it did not mint, and the store can hold chats from
// three machines in one list without any of them colliding on `chat-1`.
//
// The local machine is deliberately NOT tagged. Its ids stay byte-identical to
// what they are today, so every persisted draft, queue entry, archive filename
// and scroll position keeps matching after this lands.

export const MACHINE_ID_PREFIX = 'M~';
const SEPARATOR = '~';

/** Tag a remote id. A local id (empty machineId) is returned untouched. */
export function tagId(machineId: string, id: string): string {
  const machine = String(machineId ?? '').trim();
  const local = String(id ?? '');
  if (!machine || !local) return local;
  if (isTagged(local)) return local;
  return MACHINE_ID_PREFIX + machine + SEPARATOR + local;
}

/** Split a possibly-tagged id. An untagged id belongs to the local machine. */
export function splitId(id: string): { machineId: string; id: string } {
  const value = String(id ?? '');
  if (!value.startsWith(MACHINE_ID_PREFIX)) return { machineId: '', id: value };
  const rest = value.slice(MACHINE_ID_PREFIX.length);
  const cut = rest.indexOf(SEPARATOR);
  if (cut <= 0) return { machineId: '', id: value };
  return { machineId: rest.slice(0, cut), id: rest.slice(cut + 1) };
}

export function isTagged(id: string): boolean {
  return splitId(id).machineId !== '';
}

/** Which machine an id belongs to; '' means this one. */
export function machineOf(id: string): string {
  return splitId(id).machineId;
}

/** The id as its owning daemon knows it — what goes back out on the wire. */
export function localId(id: string): string {
  return splitId(id).id;
}

/**
 * Retag every id-shaped value in a payload arriving from `machineId`.
 *
 * Which keys are ids is a list rather than a heuristic: a heuristic that guessed
 * would eventually tag a model id or a file path, and the failure would be a
 * chat that silently addresses the wrong machine. Anything not named here
 * travels unchanged.
 */
const ID_KEYS = new Set([
  'id', 'chatId', 'tabId', 'conversationId', 'parentChatId', 'parentTabId',
  'jobId', 'workId', 'subagentId', 'sessionId', 'requestId',
  'userMessageId', 'assistantMessageId', 'messageId', 'clientUserMessageId',
  'continuationAssistantMessageId', 'queueId', 'planLatestMessageId',
  'steerContinuationId', 'steerContinuationFor', 'turnRootId', 'runningJobId',
  'lastMessageId', 'queueHeadId',
]);

const ID_ARRAY_KEYS = new Set([
  'consumedSteerIds', 'pendingPermissionIds',
]);

export function tagPayload<T>(machineId: string, value: T): T {
  const machine = String(machineId ?? '').trim();
  if (!machine) return value;
  return walk(value, (id) => tagId(machine, id)) as T;
}

export function untagPayload<T>(value: T): T {
  return walk(value, (id) => localId(id)) as T;
}

/**
 * Strip the tags of ONE machine, wherever they appear — including a bare
 * positional argument, which `untagPayload` cannot reach because it has no key
 * to recognise it by (`archiveLoad(tabId)`, `cancelJob(id)`).
 *
 * Matching the machine exactly is what makes this safe to run over everything,
 * message text included: a string only changes if it is tagged with the very
 * machine this call is already being routed to, which no human types by
 * accident. The inbound direction cannot work this way — deciding which strings
 * BECOME ids needs the key list, since a heuristic would eventually tag a model
 * id or a path.
 */
export function untagFor<T>(machineId: string, value: T): T {
  const machine = String(machineId ?? '').trim();
  if (!machine) return value;
  return deepMap(value, (text) => (machineOf(text) === machine ? localId(text) : text)) as T;
}

function deepMap(value: unknown, map: (text: string) => string): unknown {
  if (typeof value === 'string') return map(value);
  if (Array.isArray(value)) return value.map((item) => deepMap(item, map));
  if (!value || typeof value !== 'object') return value;
  const source = value as Record<string, unknown>;
  const out: Record<string, unknown> = {};
  for (const key of Object.keys(source)) out[key] = deepMap(source[key], map);
  return out;
}

function walk(value: unknown, map: (id: string) => string, ownerKey = ''): unknown {
  if (Array.isArray(value)) {
    return value.map((item) => (
      typeof item === 'string' && ID_ARRAY_KEYS.has(ownerKey)
        ? map(item)
        : walk(item, map)
    ));
  }
  if (!value || typeof value !== 'object') return value;
  const source = value as Record<string, unknown>;
  const out: Record<string, unknown> = {};
  for (const key of Object.keys(source)) {
    const item = source[key];
    if (typeof item === 'string' && ID_KEYS.has(key)) out[key] = map(item);
    else out[key] = walk(item, map, key);
  }
  return out;
}
