// Formatting for Ajustes · Herramientas (HerramientasPage.tsx). Pure, so the
// renderer tests import it directly.

import type { AgentMcpServer } from '../../wire/types';
import type { Workspace } from '../../store/types';

/** "≈ 260", "≈ 1,9 k": tokens as an estimate, never as an exact count. */
export function fmtTokens(tokens: number): string {
  const exact = Number.isFinite(tokens) ? Math.max(0, Math.round(tokens)) : 0;
  const n = exact >= 100 ? Math.round(exact / 10) * 10 : exact;
  if (n >= 1000) return `≈ ${(exact / 1000).toFixed(1).replace('.', ',')} k`;
  return `≈ ${n}`;
}

function baseName(path: string): string {
  const parts = path.split(/[\\/]+/).filter(Boolean);
  return parts[parts.length - 1] ?? path;
}

function samePath(a: string, b: string): boolean {
  const clean = (p: string) => p.replace(/[\\/]+$/, '');
  return clean(a) === clean(b);
}

/** A project folder by the name the sidebar gives it. */
export function projectName(path: string, workspaces: ReadonlyArray<Workspace>): string {
  const found = workspaces.find((w) => samePath(w.path, path));
  return found?.name || baseName(path);
}

/** Where a server is configured, in one line: its reach and its file. */
export function mcpServerDetail(server: AgentMcpServer, workspaces: ReadonlyArray<Workspace>): string {
  const project = server.project ? projectName(server.project, workspaces) : '';
  switch (server.scope) {
    case 'project': return `Proyecto ${project} · ${baseName(server.file)}`;
    case 'local': return `Solo vos, en ${project} · ${server.file}`;
    default: return `Usuario · ${server.file}`;
  }
}

/** Only what the config files say; nothing for a server they say nothing about. */
export function mcpStatusLabel(status: AgentMcpServer['status']): { text: string; tone: string } | null {
  if (status === 'off') return { text: 'Apagado', tone: '' };
  if (status === 'pending') return { text: 'Sin aprobar', tone: 'warn' };
  return null;
}

/** "Claude Code y Codex no tienen servidores MCP configurados." */
export function mcpEmptyLine(names: ReadonlyArray<string>): string {
  const list = names.filter(Boolean);
  if (list.length === 0) return 'Ningún agente tiene servidores MCP configurados.';
  const joined = list.length === 1 ? list[0] : `${list.slice(0, -1).join(', ')} y ${list[list.length - 1]}`;
  return `${joined} no ${list.length === 1 ? 'tiene' : 'tienen'} servidores MCP configurados.`;
}
