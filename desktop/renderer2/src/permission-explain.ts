// Plain-language explanation for a permission card (approved mock B,
// 2026-10-24). The old card said only "Quiere ejecutar Bash — corre con el
// modo de permisos actual", which explained nothing when the ask came from a
// Claude Code safety check in a chat that allows everything (user: "no lo
// entendí para nada"). Every card now says what is being approved and why it
// is being asked, in Spanish, with the risky value quoted. Claude Code's own
// reason (English) is translated when it is one of the known families and
// quoted as-is otherwise, under a Spanish lead, so no card is ever left
// without a reason a person can act on.

export interface PermissionContext {
  tool?: string;
  reason?: string;
  blockedPath?: string;
  agentId?: string;
  command?: string;
  path?: string;
  url?: string;
  description?: string;
}

/** Plain text or an inline code span. */
export type WhyPart = string | { code: string };

export interface PermissionExplanation {
  /** Claude Code's safety checks get the warning tone; ordinary asks do not. */
  tone: 'safety' | 'plain';
  title: string;
  why: WhyPart[];
  /** Claude Code's own words, shown only when no family translated them. */
  providerReason?: string;
  /** What the action touches, for "Ver comando" / "Ver archivo". */
  subject?: { kind: 'command' | 'path' | 'url'; text: string };
  /** The flagged value; the collapsed command shows the line holding it. */
  focus?: string;
}

export interface ExplainInput {
  title: string;
  kind: string | null;
  context?: PermissionContext | null;
  modeId?: string | null;
}

/** A chat in "Permitir todo" / full access only sees the asks nothing can bypass. */
export function allowsEverything(modeId: string | null | undefined): boolean {
  return !!modeId && /bypass|full/i.test(modeId);
}

type ToolFamily = 'command' | 'edit' | 'read' | 'web' | 'plan' | 'agent' | 'external' | 'other';

function toolFamily(tool: string, kind: string | null): ToolFamily {
  const name = tool.trim();
  if (/^mcp__/i.test(name)) return 'external';
  switch (name.toLowerCase()) {
    case 'bash': case 'powershell': case 'monitor': return 'command';
    case 'edit': case 'multiedit': case 'write': case 'notebookedit': return 'edit';
    case 'read': case 'glob': case 'grep': case 'ls': return 'read';
    case 'webfetch': case 'websearch': return 'web';
    case 'exitplanmode': return 'plan';
    case 'agent': case 'task': return 'agent';
  }
  switch ((kind || '').toLowerCase()) {
    case 'execute': return 'command';
    case 'edit': case 'delete': case 'move': return 'edit';
    case 'read': case 'search': return 'read';
    case 'fetch': return 'web';
  }
  return 'other';
}

const TITLES: Record<ToolFamily, string> = {
  command: 'Confirmá este comando',
  edit: 'Confirmá esta edición',
  read: 'Confirmá esta lectura',
  web: 'Confirmá este acceso a internet',
  plan: 'Aprobá el plan',
  agent: 'Confirmá este subagente',
  external: 'Confirmá esta herramienta',
  other: 'Confirmá esta acción',
};

// What a chat that asks before every action asks before.
const ASKS_BEFORE: Record<ToolFamily, string> = {
  command: 'ejecutar comandos',
  edit: 'modificar archivos',
  read: 'leer estos archivos',
  web: 'abrir páginas de internet',
  plan: 'pasar del plan a los cambios',
  agent: 'lanzar subagentes',
  external: 'usar herramientas externas',
  other: 'esta acción',
};

function subjectOf(context: PermissionContext | null | undefined): PermissionExplanation['subject'] {
  if (context?.command) return { kind: 'command', text: context.command };
  if (context?.path) return { kind: 'path', text: context.path };
  if (context?.url) return { kind: 'url', text: context.url };
  if (context?.blockedPath) return { kind: 'path', text: context.blockedPath };
  return undefined;
}

// "$V/*.png" → variable "$V", what remains if it is empty "/*.png".
function emptyExpansion(target: string): { variable: string; rest: string } | null {
  const match = /^(\$\{?[A-Za-z_][A-Za-z0-9_]*\}?)(.*)$/.exec(target);
  if (!match) return null;
  return { variable: match[1], rest: match[2] };
}

function stillAsks(allowAll: boolean): string {
  return allowAll ? ', así que pregunta aunque este chat tenga todos los permisos.' : ', así que pide confirmación.';
}

export function explainPermission(input: ExplainInput): PermissionExplanation {
  const context = input.context ?? null;
  const tool = (context?.tool || input.title || '').trim();
  const family = toolFamily(tool, input.kind);
  const reason = (context?.reason || '').trim();
  const allowAll = allowsEverything(input.modeId);
  const subject = subjectOf(context);
  const base = { subject };

  // rm on a variable that may be empty: the case that started this.
  const emptyVar = /possibly-empty variable path(?: inside command substitution)?:\s*(\S+)/i.exec(reason);
  if (emptyVar) {
    const target = emptyVar[1];
    const split = emptyExpansion(target);
    const why: WhyPart[] = ['El comando borra ', { code: target }, '.'];
    if (split && split.rest.startsWith('/')) {
      why.push(' Si ', { code: split.variable }, ' quedara vacía, borraría ', { code: split.rest }, ' desde la raíz del disco.');
    } else if (split) {
      why.push(' Si ', { code: split.variable }, ' quedara vacía, borraría otra cosa que la que se quiere.');
    }
    why.push(' Claude Code no puede comprobar que tenga un valor' + stillAsks(allowAll));
    return { ...base, tone: 'safety', title: 'Confirmá este borrado', why, focus: target };
  }

  // rm aimed at the root, the home folder or another system path.
  const critical = /Dangerous rm(?:dir)? operation detected:\s*'([^']+)'/i.exec(reason);
  if (critical || /protected system path/i.test(reason)) {
    const why: WhyPart[] = critical
      ? ['El comando borra ', { code: critical[1] }, ', una carpeta clave del sistema o tu carpeta personal.']
      : ['El comando borra algo dentro de una carpeta clave del sistema.'];
    why.push(' Claude Code siempre pide confirmación para esto.');
    return { ...base, tone: 'safety', title: 'Confirmá este borrado', why, focus: critical?.[1] };
  }

  // rm inside something too tangled to analyze.
  if (/dangerous rm|runs rm and could not be checked|too many command substitutions/i.test(reason)) {
    return {
      ...base, tone: 'safety', title: 'Confirmá este borrado',
      why: ['El comando borra archivos, pero es demasiado complejo para que Claude Code verifique qué borra' + stillAsks(allowAll) + ' Revisalo antes de permitirlo.'],
    };
  }

  // Git, shell or Claude Code configuration.
  if (/sensitive file/i.test(reason)) {
    const path = context?.path || context?.blockedPath;
    return {
      ...base, tone: 'safety', title: TITLES[family === 'other' ? 'edit' : family],
      why: path
        ? ['Va a modificar ', { code: path }, ', un archivo sensible (configuración de git, de la terminal o de Claude Code)' + stillAsks(allowAll)]
        : ['Va a modificar un archivo sensible (configuración de git, de la terminal o de Claude Code)' + stillAsks(allowAll)],
    };
  }

  // Outside the folder the chat works in.
  const outside = context?.blockedPath || (/outside the working director/i.test(reason) ? context?.path : '');
  if (outside) {
    return {
      ...base, tone: allowAll ? 'safety' : 'plain', title: TITLES[family],
      why: ['Va a usar ', { code: outside }, ', que está fuera de la carpeta de trabajo de este chat' + stillAsks(allowAll)],
    };
  }

  if (family === 'plan') {
    return { ...base, tone: 'plain', title: TITLES.plan, why: ['El agente terminó de planear y quiere empezar a hacer cambios.'] };
  }

  // A reason no family recognises: lead in Spanish, quote Claude Code's words.
  if (reason) {
    return {
      ...base, tone: allowAll ? 'safety' : 'plain', title: TITLES[family],
      why: [allowAll
        ? 'Claude Code frenó esta acción por seguridad y pregunta aunque este chat tenga todos los permisos.'
        : 'Claude Code pide confirmación antes de seguir.'],
      providerReason: reason,
    };
  }

  // No reason at all: the chat's own mode asked.
  if (allowAll) {
    return {
      ...base, tone: 'safety', title: TITLES[family],
      why: ['Claude Code pidió confirmación aunque este chat tenga todos los permisos, y no dijo por qué. Revisá la acción antes de permitirla.'],
    };
  }
  const external = family === 'external' ? /^mcp__([^_]+(?:_[^_]+)*?)__/i.exec(tool)?.[1] : undefined;
  return {
    ...base, tone: 'plain', title: TITLES[family],
    why: [external
      ? `Este chat pide confirmación antes de usar herramientas externas (${external}).`
      : `Este chat pide confirmación antes de ${ASKS_BEFORE[family]}.`],
  };
}

/** One readable line of a command for the collapsed card: the line holding
 *  the flagged value when there is one, else the first. */
export function commandPreview(text: string, focus?: string, max = 96): { line: string; more: number } {
  const lines = text.split('\n').map((line) => line.trim()).filter(Boolean);
  const chosen = (focus && lines.find((line) => line.includes(focus))) || lines[0] || '';
  let line = chosen;
  if (line.length > max) {
    // Keep the flagged value in view: cut around it rather than off the end.
    const at = focus ? line.indexOf(focus) : -1;
    if (at > max / 2) {
      const start = Math.max(0, Math.min(at - Math.floor(max / 3), line.length - (max - 1)));
      line = '…' + line.slice(start + 1, start + max - 1).trimEnd();
      if (start + max - 1 < chosen.length) line = line.slice(0, max - 1) + '…';
    } else {
      line = line.slice(0, max - 1).trimEnd() + '…';
    }
  }
  return { line, more: Math.max(0, lines.length - 1) };
}
