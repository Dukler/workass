// Ajustes · Permisos (daemon cmd/workass/agent_permissions.go). The owner's rules
// live in the daemon's app-settings blob under "agentPermissions" and are read
// by the daemon when they apply:
//  - subagentRaise: an agent may raise the permissions of the chats and
//    subagents it starts or changes, never above its own. Off, only the owner
//    raises them (a card in the chat).
//  - browserNavigate / browserUpload: allow, ask (a card) or block.
//  - browserDownload: allow or block; a download starts after the action that
//    caused it, so nothing can stop to ask.
// The four permission levels come from the provider catalog: each group names
// its native mode at each level (`permissionLevels`, daemon-authored).

import type { CatalogGroup } from './wire/types';

export type BrowserRule = 'allow' | 'ask' | 'block';
export type DownloadRule = 'allow' | 'block';

export interface AgentPermissionSettings {
  subagentRaise: boolean;
  browserNavigate: BrowserRule;
  browserDownload: DownloadRule;
  browserUpload: BrowserRule;
}

export const AGENT_PERMISSIONS_SETTINGS_KEY = 'agentPermissions';

// What Workass did before the settings existed.
export const DEFAULT_AGENT_PERMISSIONS: AgentPermissionSettings = {
  subagentRaise: true, browserNavigate: 'allow', browserDownload: 'allow', browserUpload: 'allow',
};

function rule<T extends string>(value: unknown, allowed: readonly T[], fallback: T): T {
  const text = typeof value === 'string' ? value.trim().toLowerCase() : '';
  return (allowed as readonly string[]).includes(text) ? text as T : fallback;
}

export function parseAgentPermissions(raw: unknown): AgentPermissionSettings {
  const value = raw && typeof raw === 'object' ? (raw as Record<string, unknown>) : {};
  const d = DEFAULT_AGENT_PERMISSIONS;
  return {
    subagentRaise: typeof value.subagentRaise === 'boolean' ? value.subagentRaise : d.subagentRaise,
    browserNavigate: rule(value.browserNavigate, ['allow', 'ask', 'block'] as const, d.browserNavigate),
    browserDownload: rule(value.browserDownload, ['allow', 'block'] as const, d.browserDownload),
    browserUpload: rule(value.browserUpload, ['allow', 'ask', 'block'] as const, d.browserUpload),
  };
}

export function agentPermissionsFromSettings(raw: unknown): AgentPermissionSettings {
  if (!raw || typeof raw !== 'object') return { ...DEFAULT_AGENT_PERMISSIONS };
  const settings = (raw as Record<string, unknown>).settings;
  if (!settings || typeof settings !== 'object') return { ...DEFAULT_AGENT_PERMISSIONS };
  return parseAgentPermissions((settings as Record<string, unknown>)[AGENT_PERMISSIONS_SETTINGS_KEY]);
}

export function withAgentPermissionsInSettings(current: Record<string, unknown> | null | undefined, value: AgentPermissionSettings): Record<string, unknown> {
  return { ...(current ?? {}), [AGENT_PERMISSIONS_SETTINGS_KEY]: parseAgentPermissions(value) };
}

// ---- levels ---------------------------------------------------------------

export type PermissionLevel = 'ask' | 'edit' | 'plan' | 'full';

export const PERMISSION_LEVELS: ReadonlyArray<{ id: PermissionLevel; name: string; detail: string }> = [
  { id: 'ask', name: 'Preguntar', detail: 'Lee libre; cada edición o comando te pide permiso.' },
  { id: 'edit', name: 'Editar sin preguntar', detail: 'Edita archivos sin pedirte permiso.' },
  { id: 'plan', name: 'Planear', detail: 'Solo lee y propone un plan; no cambia nada.' },
  { id: 'full', name: 'Acceso total', detail: 'Hace todo sin preguntar.' },
];

export interface LevelMapping { level: PermissionLevel; agents: Array<{ providerId: string; agent: string; mode: string }>; }

/** Each level with the native mode every agent in the catalog uses for it,
 *  by the mode's own name. A level no agent has is left out. */
export function permissionLevelMappings(groups: ReadonlyArray<CatalogGroup>): LevelMapping[] {
  const out: LevelMapping[] = [];
  for (const { id } of PERMISSION_LEVELS) {
    const agents: LevelMapping['agents'] = [];
    for (const group of groups) {
      const modeId = group.permissionLevels?.[id];
      if (!modeId) continue;
      const mode = group.modes.find((m) => m.id === modeId);
      agents.push({ providerId: group.providerId, agent: group.providerName || group.providerId, mode: mode?.name || modeId });
    }
    if (agents.length) out.push({ level: id, agents });
  }
  return out;
}

// ---- the browser profile's sessions (shell) ---------------------------------

export interface BrowserProfileApi {
  profileSites(): Promise<{ sites: string[] } | null>;
  clearSessions(): Promise<{ sites: string[] } | null>;
}

/** The shell's profile calls, when this renderer runs in a shell that has them. */
export function browserProfileApi(api: unknown = typeof window !== 'undefined' ? window.workassBrowser : undefined): BrowserProfileApi | null {
  const candidate = api as Partial<BrowserProfileApi> | undefined;
  return candidate && typeof candidate.profileSites === 'function' && typeof candidate.clearSessions === 'function'
    ? candidate as BrowserProfileApi
    : null;
}
