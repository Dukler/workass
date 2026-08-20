import type { ToolEvent } from './store/types';
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
