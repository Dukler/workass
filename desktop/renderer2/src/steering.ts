export type SteeringBehavior = 'codex-live' | 'claude-live' | 'capability';
export type SteeringDestination = 'transcript' | 'queue';

export interface SteeringReplyLike {
  ok?: boolean;
  strategy?: string;
  receipt?: boolean;
  daemonQueued?: boolean;
}

export interface PendingSteerLike {
  id: string;
  status: string;
  steerState?: 'sending' | 'accepted' | 'applied' | 'uncertain';
}

export interface ChronologicalMessageLike extends PendingSteerLike {
  role: 'user' | 'assistant';
  id: string;
  content: string;
  result?: string;
  events: unknown[];
  at?: string | null;
  jobId?: string;
  turnRootId?: string;
  turnTerminal?: boolean;
  // Native Codex keeps an admitted steer out of transcript history until the
  // core commits its userMessage item between sampling steps. Workass persists
  // this waiting pair immediately, but hides it from transcript rendering and
  // leaves the current assistant segment live until that receipt arrives.
  steerBoundary?: 'waiting';
  steerContinuationId?: string;
  steerContinuationFor?: string;
  permission?: unknown;
  interrupted?: boolean;
  retryPrompt?: string;
}

function isEmptyAssistant(message: ChronologicalMessageLike): boolean {
  return message.role === 'assistant'
    && message.content.length === 0
    && (message.result ?? '').length === 0
    && message.events.length === 0
    && !message.permission;
}

export function isWaitingSteerBoundary(message: ChronologicalMessageLike): boolean {
  return message.steerBoundary === 'waiting';
}

// Match Codex TUI: submission is durable and visible in the pending-input
// preview immediately, but it does not split a streaming assistant message.
// The pre-created continuation is also durable so a receipt can atomically
// reveal user+continuation without manufacturing identity after provider I/O.
export function stageChronologicalSteer<T extends ChronologicalMessageLike>(
  messages: T[],
  steer: T,
  continuation: T,
): { steer: T; continuation: T; rootId: string } | undefined {
  let activeIndex = -1;
  for (let index = messages.length - 1; index >= 0; index--) {
    const candidate = messages[index];
    if (candidate.role === 'assistant'
        && candidate.status === 'running'
        && candidate.steerBoundary !== 'waiting') {
      activeIndex = index;
      break;
    }
  }
  if (activeIndex < 0) return undefined;

  const active = messages[activeIndex];
  const rootId = active.turnRootId ?? active.id;
  steer.status = 'pending';
  steer.steerState = 'sending';
  steer.turnRootId = rootId;
  steer.steerBoundary = 'waiting';
  steer.steerContinuationId = continuation.id;

  continuation.role = 'assistant';
  continuation.status = 'pending';
  continuation.at = null;
  continuation.jobId = active.jobId ?? continuation.jobId;
  continuation.turnRootId = rootId;
  continuation.turnTerminal = true;
  continuation.steerBoundary = 'waiting';
  continuation.steerContinuationFor = steer.id;

  let insertAt = activeIndex + 1;
  while (insertAt < messages.length
      && messages[insertAt].steerBoundary === 'waiting'
      && messages[insertAt].turnRootId === rootId) insertAt++;
  messages.splice(insertAt, 0, steer, continuation);
  return { steer, continuation, rootId };
}

function commitOneStagedSteer<T extends ChronologicalMessageLike>(
  messages: T[],
  steerId: string,
): { steer: T; continuation: T; rootId: string } | undefined {
  const steerIndex = messages.findIndex((message) => message.id === steerId);
  if (steerIndex < 0) return undefined;
  const steer = messages[steerIndex];
  if (steer.role !== 'user' || steer.steerBoundary !== 'waiting') return undefined;
  const continuationIndex = messages.findIndex((message) => (
    message.id === steer.steerContinuationId
    && message.role === 'assistant'
    && message.steerBoundary === 'waiting'
    && message.steerContinuationFor === steer.id
  ));
  if (continuationIndex < 0) return undefined;
  const continuation = messages[continuationIndex];
  const rootId = steer.turnRootId ?? continuation.turnRootId ?? steer.id;

  let activeIndex = -1;
  for (let index = steerIndex - 1; index >= 0; index--) {
    const candidate = messages[index];
    if (candidate.role === 'assistant'
        && candidate.status === 'running'
        && candidate.steerBoundary !== 'waiting') {
      activeIndex = index;
      break;
    }
  }
  if (activeIndex < 0) return undefined;
  const active = messages[activeIndex];
  const activeJobId = active.jobId;
  // Rapid receipts with no intervening provider output should produce adjacent
  // user rows, not an invisible empty assistant segment.
  if (active.id !== rootId
      && active.turnRootId === rootId
      && active.turnTerminal === true
      && isEmptyAssistant(active)) {
    messages.splice(activeIndex, 1);
  } else {
    active.turnRootId = rootId;
    active.turnTerminal = false;
    active.status = 'done';
    active.at = null;
    active.permission = undefined;
    active.interrupted = false;
    active.retryPrompt = undefined;
  }

  delete steer.steerBoundary;
  delete steer.steerContinuationId;
  continuation.status = 'running';
  continuation.jobId = activeJobId ?? continuation.jobId;
  continuation.turnRootId = rootId;
  continuation.turnTerminal = true;
  delete continuation.steerBoundary;
  delete continuation.steerContinuationFor;
  return { steer, continuation, rootId };
}

// A later receipt cannot overtake an earlier rapid steer. If a provider ever
// reports a later client id first, commit every earlier waiting row for the same
// native turn in FIFO order before revealing the requested one.
export function commitChronologicalSteer<T extends ChronologicalMessageLike>(
  messages: T[],
  steerId: string,
): { steer: T; continuation: T; rootId: string } | undefined {
  const target = messages.find((message) => message.id === steerId);
  if (!target || target.role !== 'user' || target.steerBoundary !== 'waiting') return undefined;
  const rootId = target.turnRootId;
  const waitingIds: string[] = [];
  for (const message of messages) {
    if (message.role === 'user' && message.steerBoundary === 'waiting' && message.turnRootId === rootId) {
      waitingIds.push(message.id);
    }
    if (message.id === steerId) break;
  }
  let committed: { steer: T; continuation: T; rootId: string } | undefined;
  for (const id of waitingIds) {
    const result = commitOneStagedSteer(messages, id);
    if (!result) return undefined;
    if (id === steerId) committed = result;
  }
  return committed;
}

// A turn can end after acknowledgement but before a receipt reaches the
// client. Reveal those durable user rows after the completed assistant message,
// remove their never-activated continuation placeholders, and never replay.
export function settleStagedSteersAtTurnEnd<T extends ChronologicalMessageLike>(messages: T[]): T[] {
  const settled: T[] = [];
  const waitingContinuationIds = new Set(messages
    .filter((message) => message.role === 'user' && message.steerBoundary === 'waiting')
    .map((message) => message.steerContinuationId)
    .filter((id): id is string => !!id));
  for (let index = messages.length - 1; index >= 0; index--) {
    const message = messages[index];
    if (message.role === 'assistant'
        && message.steerBoundary === 'waiting'
        && waitingContinuationIds.has(message.id)) {
      messages.splice(index, 1);
    }
  }
  for (const message of messages) {
    if (message.role !== 'user' || message.steerBoundary !== 'waiting') continue;
    delete message.steerBoundary;
    delete message.steerContinuationId;
    if (message.steerState === 'sending') message.steerState = 'uncertain';
    message.status = 'done';
    settled.push(message);
  }
  return settled;
}

// A sent steer is a real transcript message, not presentation metadata. Split
// the currently-running assistant into a frozen prefix and a new running tail,
// then insert the user row between them in the canonical array. Every later
// chunk/result/tool is routed to the tail, so ordinary array order is the full
// rendering and persistence contract.
export function insertChronologicalSteer<T extends ChronologicalMessageLike>(
  messages: T[],
  steer: T,
  continuation: T,
): { steer: T; continuation: T; rootId: string } | undefined {
  let activeIndex = -1;
  for (let index = messages.length - 1; index >= 0; index--) {
    const candidate = messages[index];
    if (candidate.role === 'assistant' && candidate.status === 'running') {
      activeIndex = index;
      break;
    }
  }
  if (activeIndex < 0) return undefined;

  let active = messages[activeIndex];
  const rootId = active.turnRootId ?? active.id;
  // Two rapid steers with no intervening output should be adjacent user rows,
  // not separated by an invisible empty assistant segment.
  if (active.turnRootId === rootId && active.turnTerminal === true && isEmptyAssistant(active)) {
    messages.splice(activeIndex, 1);
    activeIndex--;
    active = messages[activeIndex];
  }
  if (active?.role === 'assistant') {
    active.turnRootId = rootId;
    active.turnTerminal = false;
    active.status = 'done';
    active.at = null;
    active.permission = undefined;
    active.interrupted = false;
    active.retryPrompt = undefined;
  }

  steer.status = 'pending';
  steer.steerState = 'sending';
  steer.turnRootId = rootId;

  continuation.role = 'assistant';
  continuation.status = 'running';
  continuation.at = null;
  continuation.jobId = active?.jobId ?? continuation.jobId;
  continuation.turnRootId = rootId;
  continuation.turnTerminal = true;
  messages.splice(activeIndex + 1, 0, steer, continuation);
  return { steer, continuation, rootId };
}

// Explicit provider rejection is the only path that may remove a submitted
// transcript steer. Join only the assistant segments that become adjacent;
// rapid later steers remain in place and preserve their own ownership.
export function rejectChronologicalSteer<T extends ChronologicalMessageLike>(messages: T[], steerId: string): T | undefined {
  const index = messages.findIndex((message) => message.id === steerId);
  if (index < 0) return undefined;
  const steer = messages[index];
  if (steer.status !== 'pending' && steer.steerState !== 'uncertain') return undefined;
  if (steer.steerState === 'accepted' || steer.steerState === 'applied') return undefined;
  if (steer.steerBoundary === 'waiting') {
    const continuationId = steer.steerContinuationId;
    messages.splice(index, 1);
    const continuationIndex = messages.findIndex((message) => message.id === continuationId
      && message.steerBoundary === 'waiting'
      && message.steerContinuationFor === steer.id);
    if (continuationIndex >= 0) messages.splice(continuationIndex, 1);
    return steer;
  }
  messages.splice(index, 1);

  const left = messages[index - 1];
  const right = messages[index];
  if (left?.role === 'assistant'
      && right?.role === 'assistant'
      && left.turnRootId
      && left.turnRootId === right.turnRootId) {
    const contentOffset = left.content.length;
    left.content += right.content;
    left.result = `${left.result ?? ''}${right.result ?? ''}` || undefined;
    left.events.push(...right.events.map((raw) => {
      if (!raw || typeof raw !== 'object') return raw;
      const event = raw as Record<string, unknown>;
      const at = Number(event.at);
      return Number.isFinite(at) ? { ...event, at: at + contentOffset } : { ...event };
    }));
    left.status = right.status;
    left.at = right.at;
    left.jobId = right.jobId ?? left.jobId;
    left.turnTerminal = right.turnTerminal;
    left.permission = right.permission;
    left.interrupted = right.interrupted;
    left.retryPrompt = right.retryPrompt;
    messages.splice(index, 1);
  }
  return steer;
}

export function steerStatusLabel(
  state: 'sending' | 'accepted' | 'applied' | 'uncertain',
  waitingForConsumption = false,
): string {
  switch (state) {
    case 'sending': return 'Steering…';
    case 'accepted': return waitingForConsumption ? 'Steering…' : 'Steered';
    case 'applied': return 'Steered';
    case 'uncertain': return 'Steer unconfirmed';
  }
}

// One FIFO lane per chat keeps rapid explicit steering deterministic without
// delaying the synchronous bubble paint. Failures are absorbed only by the
// lane tail; each caller still receives its own result/rejection.
export class SteeringDispatchLane {
  private tails = new Map<string, Promise<void>>();

  run<T>(chatId: string, task: () => Promise<T>): Promise<T> {
    const previous = this.tails.get(chatId) ?? Promise.resolve();
    const run = previous.then(task);
    const settled = run.then(() => undefined, () => undefined);
    this.tails.set(chatId, settled);
    void settled.then(() => {
      if (this.tails.get(chatId) === settled) this.tails.delete(chatId);
    });
    return run;
  }
}

// Codex app-server accepts same-turn input through turn/steer; the packaged
// Claude host accepts it through the Agent SDK's live streaming input. Both
// acknowledge on admission and confirm consumption later, so both are live.
// Every other ACP agent keeps capability-driven behavior (`_session/steer` when
// advertised, local queue otherwise). An adapter too old for the native request
// still reports its own `interrupt-queue` disposition, which is the ONLY thing
// that may move a submitted steer into the FIFO.
export function steeringBehavior(providerId: string | null | undefined): SteeringBehavior {
  const id = String(providerId ?? '').trim().toLowerCase();
  if (id === 'codex' || id === 'codex-acp' || id.includes('codex')) return 'codex-live';
  if (id === 'claude' || id === 'claude-agent-acp' || id === 'claude-code-acp' || id.includes('claude')) return 'claude-live';
  return 'capability';
}

// Native frontier hosts commit the user row at their own canonical boundary
// (Codex between sampling steps; Claude at the terminal result that closes the
// pre-steer segment, since the SDK answers the direction in the next turn of
// the same query), so the pair is persisted immediately but stays hidden until
// the receipt arrives. Generic capability steering has no such receipt and
// keeps the immediate-split contract.
export function steeringStagesBoundary(behavior: SteeringBehavior): boolean {
  return behavior === 'codex-live' || behavior === 'claude-live';
}

// A steering message must have exactly one visible owner. Workass creates that
// owner synchronously, then either settles the SAME transcript row or transfers
// it once to the FIFO after an explicit rejection. It never paints a second
// optimistic row or bounces an acknowledged steer through the queue.
export function steeringDestination(reply: SteeringReplyLike | null | undefined): SteeringDestination {
  const strategy = String(reply?.strategy ?? '').trim().toLowerCase();
  if (strategy === 'uncertain') return 'transcript';
  if (!reply || reply.ok === false || strategy === 'queue' || strategy === 'interrupt-queue') return 'queue';
  return 'transcript';
}

// Initialize one stable pending owner. Provider-specific placement is handled
// separately: Codex stages it in the pending preview, while generic live ACP
// steering may place it directly in transcript history.
export function beginPendingSteer<T extends PendingSteerLike>(messages: T[], message: T): T {
  message.status = 'pending';
  message.steerState = 'sending';
  messages.push(message);
  return message;
}

// A successful turn/steer response proves admission to the active regular turn.
// It settles delivery feedback but does not choose the transcript boundary;
// userMessage.clientId does that for receipt-capable Codex adapters.
export function acceptPendingSteer<T extends PendingSteerLike>(messages: T[], pendingId: string): T | undefined {
  const message = messages.find((candidate) => candidate.id === pendingId);
  if (!message || message.steerState === 'applied') return undefined;
  if (message.status !== 'pending' && message.steerState !== 'uncertain') return undefined;
  message.status = 'done';
  message.steerState = 'accepted';
  return message;
}

// A transport timeout is not a rejection: app-server may have accepted the
// already-written request. Settle the stable row honestly and wait for an
// optional canonical receipt; automatic replay here could duplicate direction.
export function markPendingSteerUncertain<T extends PendingSteerLike>(messages: T[], pendingId: string): T | undefined {
  const message = messages.find((candidate) => candidate.id === pendingId);
  if (!message || message.steerState === 'applied' || message.steerState === 'accepted') return undefined;
  if (message.status !== 'pending' && message.steerState !== 'uncertain') return undefined;
  message.status = 'done';
  message.steerState = 'uncertain';
  return message;
}

// A renderer disconnect can orphan the JavaScript promise that was awaiting
// the acknowledgement. At the native turn boundary, convert only genuinely
// in-flight rows to an explicit uncertainty state. Accepted/applied rows stay
// delivered, and explicit rejection can still transfer an uncertain row once.
export function settleSendingSteersAtTurnEnd<T extends PendingSteerLike>(messages: T[]): T[] {
  const settled: T[] = [];
  for (const message of messages) {
    if (message.status !== 'pending' || message.steerState !== 'sending') continue;
    message.status = 'done';
    message.steerState = 'uncertain';
    settled.push(message);
  }
  return settled;
}

// A consumed live steer stays in the transcript. Only an explicit queue
// disposition removes a still-pending bubble before its FIFO row is created.
export function settlePendingSteer<T extends PendingSteerLike>(
  messages: T[],
  pendingId: string,
  destination: SteeringDestination,
): T | undefined {
  const index = messages.findIndex((message) => message.id === pendingId);
  if (index < 0) return undefined;
  const message = messages[index];
  if (destination === 'queue') {
    // Only an unresolved/transport-uncertain steer can be explicitly rejected.
    // A receipt or acknowledgement owns the transcript row permanently; a late
    // response cannot steal it after it became a normal/accepted message.
    if (message.status !== 'pending' && message.steerState !== 'uncertain') return undefined;
    if (message.steerState === 'accepted' || message.steerState === 'applied') return undefined;
    messages.splice(index, 1);
    return message;
  }
  // Canonical consumption may arrive before OR after the request response. It
  // upgrades sending/accepted/uncertain in place and is idempotent by message id.
  if (!message.steerState || message.steerState === 'applied') return undefined;
  message.status = 'done';
  message.steerState = 'applied';
  return message;
}

// Receipt capability controls whether Workass waits for the host's canonical
// transcript boundary (Codex's between-sampling-step userMessage, Claude's
// pre-steer terminal result). Older adapters commit at their successful
// acknowledgement instead.
export function hasSteerConsumptionReceipt(reply: SteeringReplyLike | null | undefined): boolean {
  const strategy = String(reply?.strategy ?? '').trim().toLowerCase();
  return reply?.receipt === true
    && (strategy === 'receipt-live' || strategy === 'codex-live' || strategy === 'claude-live' || strategy === 'uncertain');
}
