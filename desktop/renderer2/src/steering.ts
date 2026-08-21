import type { DeliveryCapabilities } from './wire/types';

export type SteeringDestination = 'transcript' | 'rejected';

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
  // A receipt-bearing lane persists both identities immediately. Once accepted,
  // the user steer is transcript-visible at its canonical position; only the
  // reserved continuation stays hidden until the core commits the negotiated
  // consumption boundary.
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

// Submission is durable and visible in the pending-input preview immediately,
// but it does not split a streaming assistant message.
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
    // A provider cannot consume a later FIFO steer before every earlier staged
    // direction. A receipt for the later client id therefore applies each row
    // this loop commits, not only the requested target; leaving an earlier row
    // sending/uncertain would strand it in the Steering tray after its semantic
    // boundary had already been committed.
    result.steer.status = 'done';
    result.steer.steerState = 'applied';
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
  _waitingForConsumption = false,
): string {
  switch (state) {
    case 'sending': return 'Steering…';
    case 'accepted': return 'Steered';
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

export function normalizeDeliveryCapabilities(value: unknown): DeliveryCapabilities | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined;
  const capabilities = value as Partial<DeliveryCapabilities>;
  return {
    stableInputIdentity: capabilities.stableInputIdentity === true,
    liveSteer: capabilities.liveSteer === true,
    steerConsumptionReceipt: capabilities.steerConsumptionReceipt === true,
    consumptionReceipt: capabilities.consumptionReceipt === true,
    turnReadback: capabilities.turnReadback === true,
  };
}

export function liveSteeringSupported(capabilities: DeliveryCapabilities | null | undefined): boolean {
  return capabilities?.liveSteer === true;
}

// A later, stable steer-consumption receipt is the only reason to stage the
// pending row outside transcript chronology. Generic live steering has an
// admission acknowledgement but no later semantic receipt, so it keeps the
// pending transcript row. Provider branding never enters this decision.
export function steeringStagesBoundary(capabilities: DeliveryCapabilities | null | undefined): boolean {
  return liveSteeringSupported(capabilities) && capabilities?.steerConsumptionReceipt === true;
}

// A steering message must have exactly one visible owner. Workass creates that
// owner synchronously, then either settles that same transcript row or rejects
// it back to the untouched composer. FIFO ownership exists only for a separate,
// explicit queue intent.
export function steeringDestination(reply: SteeringReplyLike | null | undefined): SteeringDestination {
  const strategy = String(reply?.strategy ?? '').trim().toLowerCase();
  if (strategy === 'uncertain') return 'transcript';
  if (!reply || reply.ok === false || strategy === 'rejected' || strategy === 'unsupported'
      || strategy === 'queue' || strategy === 'interrupt-queue') return 'rejected';
  return 'transcript';
}

// A successful turn/steer response proves admission to the active regular turn.
// It settles delivery feedback but does not choose the transcript boundary;
// the typed steer-consumption receipt does that for receipt-capable lanes.
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

// A consumed live steer stays in the transcript. A definite rejection removes
// only the temporary row so the composer can reclaim the untouched input.
export function settlePendingSteer<T extends PendingSteerLike>(
  messages: T[],
  pendingId: string,
  destination: SteeringDestination,
): T | undefined {
  const index = messages.findIndex((message) => message.id === pendingId);
  if (index < 0) return undefined;
  const message = messages[index];
  if (destination === 'rejected') {
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

// The attached lane capability has already selected the staged path. The
// operation reply now needs only its typed receipt fact; provider-branded
// strategy labels are transport diagnostics and never semantic switches.
export function hasSteerConsumptionReceipt(reply: SteeringReplyLike | null | undefined): boolean {
  return reply?.receipt === true;
}
