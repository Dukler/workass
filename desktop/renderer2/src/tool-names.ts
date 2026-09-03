// Human names + action icons for a SINGLE tool call (approved mock
// desktop/docs/mocks/toolrow-names.html, 2026-07-25). The raw identifier a
// provider sends is an implementation detail — «Bash», «Editing files»,
// «mcp__workass-agent__workass_cancel_subagent», or (Codex) the entire command
// line. The row now states the ACTION, and the exact evidence (command, path,
// query) moves to the mono detail column, which already ellipsizes the middle
// so the filename never drops. Nothing is lost: the raw title survives in the
// row tooltip and in the expanded body.
//
// Only singular rows are renamed. The plural fold summary («1 comando ·
// 1 subagente · 3 llamadas») keeps its count nouns — see KIND_NOUN in
// components/messages.tsx.
//
// This is also the ONE vocabulary the Turnos rail reads (approved mock
// rail-actions, 2026-07-27): the live call, the subagent rows and the running
// background rows all resolve here, so the rail and the chat can never disagree
// about what a call was.
import type { ToolEvent } from './store/types';
import type { SpawnedWorkItem } from './wire/types';

// Which glyph the row leads with. Mapped to real SVGs by ActionIcon (icons.tsx)
// so this module stays free of JSX and testable under node --test.
export type ActionIcon =
  | 'run' | 'read' | 'edit' | 'write' | 'search' | 'web' | 'fetch'
  | 'plan' | 'delete' | 'move' | 'think' | 'agent' | 'chat' | 'mcp' | 'tool'
  // rail-only marks: a lifecycle phase is not a tool call, but it still has to
  // depict itself — no catch-all glyph anywhere (user, 2026-07-25).
  | 'proc' | 'config' | 'shield';

export interface ToolPresentation {
  /** Human action, e.g. «Ejecutar un comando». */
  label: string;
  icon: ActionIcon;
  /** Faint origin tag for MCP calls, e.g. «· workass». */
  tag?: string;
  /**
   * The original title when it was itself the evidence (a raw command line or a
   * bare path, as Codex sends). The row shows it in the detail column when the
   * event carries no command/location of its own.
   */
  evidence?: string;
  /** Raw provider identifier, kept for the tooltip / expanded body. */
  raw: string;
  /**
   * Set only by spawnedWorkActivity, for text we could NOT classify (the
   * daemon's own prose). `label` is that text verbatim and the row must render
   * it with no glyph — a mark we didn't earn would be a wildcard.
   */
  unclassified?: boolean;
}

const ICON_BY_ACTION: Record<string, ActionIcon> = {
  execute: 'run', read: 'read', edit: 'edit', write: 'write', search: 'search',
  websearch: 'web', fetch: 'fetch', plan: 'plan', delete: 'delete', move: 'move',
  think: 'think', agent: 'agent', other: 'tool',
};

// ACP tool kind → action label. The kind is the reliable signal (both adapters
// stamp it); tool names below only refine it.
const KIND_LABEL: Record<string, string> = {
  execute: 'Ejecutar un comando',
  read: 'Leer un archivo',
  edit: 'Editar un archivo',
  delete: 'Borrar un archivo',
  move: 'Mover un archivo',
  search: 'Buscar en archivos',
  fetch: 'Abrir una página web',
  think: 'Pensar',
  agent: 'Delegar en un subagente',
  other: 'Usar una herramienta',
};

// Exact tool identifiers, as each provider spells them. Claude Code sends the
// tool name («Bash», «Read»); Codex sends a phrase («Editing files») or the raw
// command. Keys are lowercased before lookup.
const NAME_LABEL: Record<string, [string, string]> = {
  // action label, action key (drives the icon)
  bash: ['Ejecutar un comando', 'execute'],
  'bash command': ['Ejecutar un comando', 'execute'],
  shell: ['Ejecutar un comando', 'execute'],
  bashoutput: ['Ver la salida de un comando', 'execute'],
  killshell: ['Terminar un proceso', 'execute'],
  killbash: ['Terminar un proceso', 'execute'],
  slashcommand: ['Ejecutar un comando propio', 'execute'],
  read: ['Leer un archivo', 'read'],
  'read file': ['Leer un archivo', 'read'],
  'reading files': ['Leer archivos', 'read'],
  notebookread: ['Leer un notebook', 'read'],
  write: ['Escribir un archivo', 'write'],
  'write file': ['Escribir un archivo', 'write'],
  create: ['Crear un archivo', 'write'],
  edit: ['Editar un archivo', 'edit'],
  multiedit: ['Editar un archivo', 'edit'],
  'edit file': ['Editar un archivo', 'edit'],
  'editing files': ['Editar archivos', 'edit'],
  applypatch: ['Aplicar un parche', 'edit'],
  apply_patch: ['Aplicar un parche', 'edit'],
  patch: ['Aplicar un parche', 'edit'],
  notebookedit: ['Editar un notebook', 'edit'],
  grep: ['Buscar en el código', 'search'],
  glob: ['Buscar archivos', 'search'],
  search: ['Buscar en archivos', 'search'],
  ls: ['Listar una carpeta', 'search'],
  toolsearch: ['Buscar herramientas', 'search'],
  websearch: ['Buscar en la web', 'websearch'],
  'web search': ['Buscar en la web', 'websearch'],
  webfetch: ['Abrir una página web', 'fetch'],
  'web fetch': ['Abrir una página web', 'fetch'],
  fetch: ['Abrir una página web', 'fetch'],
  todowrite: ['Actualizar el plan', 'plan'],
  todoread: ['Revisar el plan', 'plan'],
  taskcreate: ['Anotar una tarea', 'plan'],
  taskupdate: ['Actualizar una tarea', 'plan'],
  tasklist: ['Revisar las tareas', 'plan'],
  exitplanmode: ['Cerrar el plan', 'plan'],
  enterplanmode: ['Abrir el modo plan', 'plan'],
  task: ['Delegar en un subagente', 'agent'],
  agent: ['Delegar en un subagente', 'agent'],
  skill: ['Usar una habilidad', 'think'],
  think: ['Pensar', 'think'],
};

// Workass's OWN MCP servers are ours to name properly; every other server is
// humanized mechanically from its tool id (see humanizeMcp).
const WORKASS_MCP_LABEL: Record<string, [string, string]> = {
  workass_spawn_subagent: ['Lanzar un subagente', 'agent'],
  workass_cancel_subagent: ['Cancelar un subagente', 'agent'],
  workass_retry_subagent: ['Reintentar un subagente', 'agent'],
  workass_message_subagent: ['Escribir a un subagente', 'agent'],
  workass_wait_subagent: ['Esperar a un subagente', 'agent'],
  workass_wait_subagents: ['Esperar a los subagentes', 'agent'],
  workass_list_subagents: ['Ver los subagentes', 'agent'],
  workass_list_subagent_receipts: ['Ver los recibos de subagentes', 'agent'],
  workass_list_spawned_work: ['Ver el trabajo lanzado', 'agent'],
  workass_list_spawned_work_receipts: ['Ver los recibos de trabajo', 'agent'],
  workass_register_external_work: ['Registrar trabajo externo', 'agent'],
  workass_settle_external_work: ['Cerrar trabajo externo', 'agent'],
  workass_list_chats: ['Ver los chats', 'chat'],
  workass_list_update_targets: ['Ver equipos actualizables', 'search'],
  workass_get_update_status: ['Leer el estado de actualización', 'read'],
  workass_apply_update: ['Actualizar Workass', 'write'],
  workass_read_chat: ['Leer un chat', 'read'],
  workass_create_chat: ['Crear un chat', 'chat'],
  workass_rename_chat: ['Renombrar un chat', 'chat'],
  workass_delete_chat: ['Borrar un chat', 'delete'],
  workass_configure_chat: ['Configurar un chat', 'chat'],
  workass_focus_chat: ['Abrir un chat', 'chat'],
  workass_send_chat_message: ['Enviar un mensaje', 'chat'],
  workass_cancel_chat_turn: ['Cancelar un turno', 'chat'],
  workass_agent_catalog: ['Ver el catálogo de modelos', 'plan'],
  workass_host_artifact: ['Publicar un artefacto', 'web'],
  workass_browser_open: ['Abrir el navegador', 'web'],
  workass_browser_navigate: ['Navegar en el navegador', 'web'],
  workass_browser_screenshot: ['Capturar el navegador', 'web'],
  workass_browser_snapshot: ['Leer la página abierta', 'web'],
  workass_browser_click: ['Hacer clic en la página', 'web'],
  workass_browser_type: ['Escribir en la página', 'web'],
  workass_browser_key: ['Pulsar una tecla en la página', 'web'],
  workass_browser_scroll: ['Desplazar la página', 'web'],
  workass_browser_list: ['Ver las pestañas', 'web'],
  workass_browser_history: ['Ver el historial', 'web'],
  workass_browser_batch: ['Operar en el navegador', 'web'],
};

function iconFor(action: string): ActionIcon {
  return ICON_BY_ACTION[action] ?? (action as ActionIcon) ?? 'tool';
}

// «mcp__workass-agent__workass_cancel_subagent» → server + tool id.
function splitMcp(raw: string): { server: string; tool: string } | null {
  if (!/^mcp__/i.test(raw)) return null;
  const rest = raw.slice(5);
  const cut = rest.indexOf('__');
  if (cut <= 0) return { server: '', tool: rest };
  return { server: rest.slice(0, cut), tool: rest.slice(cut + 2) };
}

// Server tag shown faintly after the label: «workass-agent» and
// «workass-browser» are the same product to the user, so both read «workass».
function serverTag(server: string): string {
  if (!server) return '';
  if (/^workass(-|$)/i.test(server)) return 'workass';
  return server;
}

// A tool id we have no translation for — third-party MCP, or a tool that ships
// after this map was written — still reads better spaced out: «create_issue» →
// «Create issue», «SomeNewTool» → «Some new tool». Acronyms are left alone.
export function humanizeName(id: string): string {
  const spaced = id
    .replace(/[_-]+/g, ' ')
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1 $2')
    .replace(/\s+/g, ' ')
    .trim();
  if (!spaced) return '';
  return spaced
    .split(' ')
    .map((word, i) => (/^[A-Z]{2,}$/.test(word) ? word : i === 0 ? word.charAt(0).toUpperCase() + word.slice(1) : word.toLowerCase()))
    .join(' ');
}

// A short plain phrase is a NAME («Bash», «Editing files», «Call hidden tool»);
// anything carrying a slash, a dot, a flag, quotes or shell punctuation is the
// call itself, which is how Codex titles its rows («/bin/zsh -lc "ssh …"`,
// `internal/acp/bridge.go`). Only the latter is demoted to the detail column —
// a name pushed there would leave the row with no identity at all.
function isBareName(title: string): boolean {
  return title.length <= 28 && /^[A-Za-z][A-Za-z0-9 _-]*$/.test(title) && !/\s-{1,2}\w/.test(title);
}
function looksLikeEvidence(title: string): boolean {
  return !!title && !isBareName(title);
}

// Same classification the fold summary uses, kept here for titles that carry no
// ACP kind (reloaded history predating toolKind, or a raw command line).
function sniffAction(title: string): string {
  if (/^\s*\$|(^|\b)(ran|run|exec|command|terminal|shell|bash|npm|git)/.test(title)) return 'execute';
  if (/(^|\b)(read|reading|view|cat|open|le[ií])/.test(title)) return 'read';
  if (/(^|\b)(edit|wrote|write|creat|modif|patch|append|replac|escrib)/.test(title)) return 'edit';
  if (/(^|\b)(search|grep|find|glob|busc)/.test(title)) return 'search';
  if (/(^|\b)(fetch|download|http|web|url|curl|descarg)/.test(title)) return 'fetch';
  if (/(^|\b)(delet|remov|rm|borr)/.test(title)) return 'delete';
  if (/(^|\b)(move|mv|rename|mov)/.test(title)) return 'move';
  return '';
}

export function toolPresentation(t: ToolEvent): ToolPresentation {
  const raw = String(t.title ?? '').trim();
  const kind = String(t.toolKind ?? '').toLowerCase();

  const mcp = splitMcp(raw);
  if (mcp) {
    const known = WORKASS_MCP_LABEL[mcp.tool.toLowerCase()];
    const tag = serverTag(mcp.server);
    if (known) return { label: known[0], icon: iconFor(known[1]), tag: tag || undefined, raw };
    return { label: humanizeName(mcp.tool) || KIND_LABEL.other, icon: 'mcp', tag: tag || undefined, raw };
  }

  const named = NAME_LABEL[raw.toLowerCase()];
  if (named) return { label: named[0], icon: iconFor(named[1]), raw };

  // No name match: the title is either a raw command/path (Codex) or an unknown
  // tool. Trust the ACP kind first, sniff the text second. 'other' is not an
  // action — it falls through so an unnamed tool keeps its own id.
  const action = KIND_LABEL[kind] && kind !== 'other' ? kind : sniffAction(raw.toLowerCase());
  if (action && KIND_LABEL[action]) {
    const evidence = looksLikeEvidence(raw) ? raw : undefined;
    return { label: KIND_LABEL[action], icon: iconFor(action), evidence, raw };
  }

  // Unknown tool, bare-name title: humanize it rather than flattening to «Usar
  // una herramienta», which would drop the only identifying text on the row.
  if (raw && !looksLikeEvidence(raw)) return { label: humanizeName(raw) || raw, icon: 'tool', raw };
  return { label: KIND_LABEL.other, icon: 'tool', evidence: raw || undefined, raw };
}

// ── Running background work (the rail's inline .bgr rows) ────────────────────
// A spawned subagent reports a PHASE, not a tool id, whenever it is between
// calls: the daemon's own closed vocabulary (internal/acp/subagents.go —
// "starting" through "orphaned"), which used to reach the row in English.
const PHASE_LABEL: Record<string, [string, ActionIcon]> = {
  starting: ['Arrancando', 'proc'],
  initializing: ['Abriendo la sesión', 'proc'],
  configuring: ['Ajustando controles', 'config'],
  working: ['Trabajando', 'run'],
  thinking: ['Pensando', 'think'],
  planning: ['Actualizando el plan', 'plan'],
  waiting_permission: ['Esperando permiso', 'shield'],
  orphaned: ['Sin proceso', 'agent'],
};

// What a running spawned row is doing right now. Two vocabularies reach the same
// pair of fields and only one of them is ours to translate:
//   • phase "tool" → `summary` is the tool title → the action vocabulary above;
//   • a known daemon phase → its own word and mark;
//   • a bare tool id (native Task progress sends "Read"/"Bash" here) → action;
//   • anything else is FREE TEXT the daemon wrote for a human (a tracked lane's
//     description) — it is passed through verbatim with NO glyph. Inventing a
//     mark for a sentence we did not classify is exactly what we don't do.
export function spawnedWorkActivity(item: Pick<SpawnedWorkItem, 'summary' | 'lastToolName'>): ToolPresentation | null {
  const summary = String(item.summary ?? '').trim();
  const phase = String(item.lastToolName ?? '').trim();

  if (phase.toLowerCase() === 'tool' && summary) return toolPresentation({ title: summary } as ToolEvent);

  const known = PHASE_LABEL[phase.toLowerCase()];
  if (known) {
    // «Esperando permiso» keeps the exact action it is blocked on as evidence.
    const evidence = /^permission required:/i.test(summary) ? summary.slice(summary.indexOf(':') + 1).trim() : undefined;
    return { label: known[0], icon: known[1], evidence: evidence || undefined, raw: phase };
  }

  // A tool id sent as the phase (native Task progress) only counts when the
  // action map actually recognizes it — otherwise it is prose, not a tool.
  if (phase && isBareName(phase)) {
    const act = toolPresentation({ title: phase } as ToolEvent);
    if (act.label !== phase) return act;
  }

  const text = summary || phase;
  return text ? { label: text, icon: 'tool', raw: text, unclassified: true } : null;
}

// The work's kind, as a word instead of the wire enum («subagent», «bash»).
// An unknown kind passes through untouched rather than being flattened.
const KIND_WORD: Record<string, string> = {
  bash: 'comando', shell: 'comando', agent: 'agente', subagent: 'subagente',
  workflow: 'flujo', background: 'segundo plano', task: 'tarea',
};
export function spawnedWorkKindWord(kind: string): string {
  return KIND_WORD[String(kind ?? '').toLowerCase()] ?? String(kind ?? '');
}
