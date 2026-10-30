import type { Chat, Msg, ToolEvent } from './store/types';
import type { SpawnedWorkItem } from './wire/types';
import { toolPresentation, type ToolPresentation } from './tool-names.ts';

// The spawning header of a subagent (rendered inline as one "Subagente · …" task
// row, folded into the normal tool group). Workass stamps subagentHeader/toolKind
// 'agent' on it.
export function isSubagentHeader(t: ToolEvent): boolean {
  return t.subagentHeader === true || t.toolKind === 'agent';
}
// A tool-call made INSIDE a subagent (carries subagentId but is not the header).
// These are dropped from the transcript and shown only in the Turnos rail.
export function isSubagentChild(t: ToolEvent): boolean {
  return !!t.subagentId && t.subagentHeader !== true && t.subagentId !== t.id;
}

export interface SubagentNode {
  id: string;
  label: string;
  provider: string | null;
  // Friendly model+effort combo for the Turnos chip ("Opus4.8-xhigh"). Null when
  // the daemon predates the stamp; the rail then omits the chip.
  model: string | null;
  header?: ToolEvent;
  calls: ToolEvent[];
  work?: SpawnedWorkItem;
  // From the header event: the child is a native fork (no work item needed).
  forked?: boolean;
}

// Steering continuations share one logical turn. All views use the same
// transcript boundary so a child cannot disappear or render twice.
export function currentTurnMessages(chat: Chat | null) {
  const latest = chat ? [...chat.messages].reverse().find((message) => message.role === 'assistant') : undefined;
  if (!latest) return [];
  const root = latest.turnRootId?.trim();
  return root ? chat!.messages.filter((message) => message.role === 'assistant'
    && (message.id === root || message.turnRootId?.trim() === root)) : [latest];
}

export function isAgentWork(item: Pick<SpawnedWorkItem, 'kind'>): boolean {
  return item.kind === 'agent' || item.kind === 'subagent';
}

export function subagentWorkId(item: SpawnedWorkItem): string {
  return item.kind === 'subagent' ? item.id : item.toolCallId || item.id;
}

// Lifetime and metadata come from the durable record; calls remain attributed
// by exact tool identity. This projection grants no control over native work.
function spawnedWorkToolStatus(item: SpawnedWorkItem): string {
  return item.status === 'running' ? 'in_progress'
    : item.status === 'failed' || item.status === 'orphaned' ? 'failed'
    : item.status === 'stopped' || item.status === 'cancelled' ? 'cancelled' : 'completed';
}

// A child's card in the turn that started it reads what its row reads. Ending that
// turn closes every open tool in it, the child's card too, but a Stop or a daemon
// restart does not end the child: it stays running and the card must not say
// "cancelado" or "falló" meanwhile, nor after it succeeds.
export function subagentCardsWithWork(tools: ToolEvent[], items: readonly SpawnedWorkItem[]): ToolEvent[] {
  if (!items.length) return tools;
  const status = new Map(items.filter(isAgentWork).map((item) => [subagentWorkId(item), spawnedWorkToolStatus(item)]));
  return tools.map((tool) => {
    const current = status.get(tool.subagentId || tool.id || '');
    return current && current !== tool.status ? { ...tool, status: current } : tool;
  });
}

// When a child ends, its card in the loaded transcript takes that end for good. A
// row can leave the client's list once it settles (the summary keeps only running
// ones), and the card would fall back to whatever closed the turn. Returns the
// messages it changed. `done` holds what was already applied, so a chat that keeps
// receiving progress does not walk its transcript on every update.
export function settleSubagentCards(messages: readonly Msg[], items: readonly SpawnedWorkItem[], done: Set<string>): Msg[] {
  const settled = items.filter((item) => isAgentWork(item) && item.status !== 'running'
    && !done.has(`${subagentWorkId(item)}:${item.status}`));
  if (!settled.length) return [];
  const status = new Map(settled.map((item) => [subagentWorkId(item), spawnedWorkToolStatus(item)]));
  const changed = new Set<Msg>();
  for (const message of messages) {
    if (message.role !== 'assistant') continue;
    for (const event of message.events) {
      if (event.kind !== 'tool' || !isSubagentHeader(event)) continue;
      const current = status.get(event.subagentId || event.id || '');
      if (current && current !== event.status) {
        event.status = current;
        changed.add(message);
      }
    }
  }
  for (const item of settled) done.add(`${subagentWorkId(item)}:${item.status}`);
  return [...changed];
}

export function nodeForSpawnedWork(item: SpawnedWorkItem, node?: SubagentNode): SubagentNode {
  const id = subagentWorkId(item);
  const status = spawnedWorkToolStatus(item);
  const start = Date.parse(item.startedAt);
  const end = item.finishedAt ? Date.parse(item.finishedAt) : NaN;
  const header: ToolEvent = node?.header ?? {
    key: id, id, at: Number.isFinite(start) ? start : 0, kind: 'tool', toolKind: 'agent', title: item.label,
    status, command: null, location: null, input: null, output: null, terminalId: null,
    subagentId: id, subagentHeader: true,
  };
  return { id, label: item.label || node?.label || 'Subagente',
    provider: item.assistantBrand || node?.provider || null,
    model: item.modelLabel || node?.model || null, calls: node?.calls ?? [], work: item,
    forked: item.forked === true || node?.forked === true || undefined,
    header: { ...header, status,
      output: item.resultExcerpt || (item.status !== 'running' ? item.summary : '') || header.output,
      startedAt: Number.isFinite(start) ? start : header.startedAt,
      endedAt: Number.isFinite(end) ? end : undefined } };
}

// Foreground settlement cannot decide an independently tracked child's life.
// Running children from earlier turns stay visible even without a current header.
export function reconcileSubagentWork(nodes: SubagentNode[], items: readonly SpawnedWorkItem[]): { nodes: SubagentNode[]; liveIds: Set<string> } {
  const byId = new Map<string, SpawnedWorkItem>();
  const liveIds = new Set<string>();
  for (const item of items) {
    if (!isAgentWork(item)) continue;
    const id = subagentWorkId(item);
    if (item.status === 'running') liveIds.add(id);
    const previous = byId.get(id);
    if (!previous || (item.status === 'running' && previous.status !== 'running')
      || ((item.status === 'running') === (previous.status === 'running') && Date.parse(item.startedAt) >= Date.parse(previous.startedAt))) {
      byId.set(id, item);
    }
  }
  const represented = new Set(nodes.map((node) => node.id));
  const reconciled = nodes.map((node) => {
    const item = byId.get(node.id);
    return item ? nodeForSpawnedWork(item, node) : node;
  });
  for (const [id, item] of byId) {
    if (item.status === 'running' && !represented.has(id)) reconciled.push(nodeForSpawnedWork(item));
  }
  return { liveIds, nodes: reconciled };
}

// Work an agent started itself (a Claude subagent's own shell or Monitor) is
// shown under that agent's row instead of as an unattributed top-level row.
// Only when the parent agent is in the same list — every listed agent renders
// somewhere, so a nested child is never hidden; an orphan stays top-level.
export function nestSpawnedWork(items: readonly SpawnedWorkItem[]): { byParent: Map<string, SpawnedWorkItem[]>; nested: Set<string> } {
  const agentTasks = new Set(items.filter(isAgentWork).map((item) => item.taskId).filter(Boolean));
  const byParent = new Map<string, SpawnedWorkItem[]>();
  const nested = new Set<string>();
  for (const item of items) {
    const parent = item.parentTaskId;
    if (!parent || isAgentWork(item) || parent === item.taskId || !agentTasks.has(parent)) continue;
    byParent.set(parent, [...(byParent.get(parent) ?? []), item]);
    nested.add(item.id);
  }
  return { byParent, nested };
}

// The machine a row's work runs on, when that is not this one. A row from a daemon
// that stored the whole link name still reads as the machine's own name.
export function workHostName(item?: Pick<SpawnedWorkItem, 'hostMachine' | 'hostMachineName'> | null): string {
  if (!item?.hostMachine) return '';
  const name = String(item.hostMachineName ?? '').replace(/^workass daemon\s*/, '').replace(/\s*\(m-[0-9a-f]+\)$/, '').trim();
  return name || item.hostMachine;
}

export function canStopSpawnedWorkItem(item: Pick<SpawnedWorkItem, 'kind' | 'pid' | 'outputFile'>): boolean {
  // Native agents are observed only, even if a provider includes process metadata.
  return item.kind !== 'agent' && !(item.kind === 'workflow' && !item.pid && !item.outputFile);
}

const ACTIVE_TOOL_STATUS = new Set(['in_progress', 'pending', 'running']);

// Compact, user-facing activity for a running subagent summary: the call it is
// on right now, named by the SAME action vocabulary the transcript rows use
// (tool-names.ts). It used to be a second, hand-kept classifier with its own
// gerunds, so the rail could say «Ejecutando un comando» where the chat said
// «Ejecutar un comando» — one map now, and the row gets the action's glyph too
// (approved mock rail-actions, 2026-07-27).
//
// Never surface the raw command here: the expandable body is the place for exact
// tool evidence, while the collapsed row stays scannable — so the caller renders
// `label` only, not `evidence`.
export function subagentActivity(node: SubagentNode): ToolPresentation {
  let active: ToolEvent | undefined;
  for (let i = node.calls.length - 1; i >= 0; i--) {
    if (ACTIVE_TOOL_STATUS.has(String(node.calls[i].status).toLowerCase())) {
      active = node.calls[i];
      break;
    }
  }
  // Between calls the subagent is working and nothing more specific is true —
  // same word the daemon's own «working» phase gets.
  if (!active) return { label: 'Trabajando', icon: 'run', raw: '' };
  return toolPresentation(active);
}

// Reconstruct one node per explicitly-attributed child. Synthetic Workass
// headers use id===subagentId; native Claude Task headers are discovered when
// leaf calls reference their tool-call id. Main-thread tools stay separate.
export function extractSubagents(tools: ToolEvent[]): { nodes: SubagentNode[]; mainTools: ToolEvent[]; hasSubagents: boolean } {
  const parentIds = new Set<string>();
  for (const tool of tools) if (tool.subagentId) parentIds.add(tool.subagentId);
  const order: string[] = [];
  const byId = new Map<string, SubagentNode>();
  const ensure = (id: string): SubagentNode => {
    let node = byId.get(id);
    if (!node) {
      node = { id, label: id, provider: null, model: null, calls: [] };
      byId.set(id, node);
      order.push(id);
    }
    return node;
  };
  const mainTools: ToolEvent[] = [];
  for (const tool of tools) {
    if (tool.subagentId && (tool.subagentHeader || tool.id === tool.subagentId)) {
      const node = ensure(tool.subagentId);
      node.header = tool;
      if (tool.subagentForked) node.forked = true;
      if (tool.title) node.label = tool.title;
      if (!node.provider && tool.subagentProvider) node.provider = tool.subagentProvider;
      if (!node.model && tool.subagentModel) node.model = tool.subagentModel;
    } else if (tool.subagentId) {
      const node = ensure(tool.subagentId);
      node.calls.push(tool);
      if (!node.provider && tool.subagentProvider) node.provider = tool.subagentProvider;
      if (!node.model && tool.subagentModel) node.model = tool.subagentModel;
      if (node.label === node.id && tool.subagentLabel) node.label = tool.subagentLabel;
    } else if (tool.id && parentIds.has(tool.id)) {
      const node = ensure(tool.id);
      node.header = tool;
      if (tool.title) node.label = tool.title;
      if (!node.provider && tool.subagentProvider) node.provider = tool.subagentProvider;
      if (!node.model && tool.subagentModel) node.model = tool.subagentModel;
    } else {
      mainTools.push(tool);
    }
  }
  return { nodes: order.map((id) => byId.get(id)!), mainTools, hasSubagents: order.length > 0 };
}
