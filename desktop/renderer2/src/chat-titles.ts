// Chat titles written by an agent (daemon cmd/workass/chat_titles.go). The
// setting lives in the daemon's app-settings blob under "chatTitles"; with
// mode "agent", the first message of a chat is sent once to the chosen agent
// and model for a short title. "prompt" keeps the first-message title.
//
// Which agents can do it, and with which models, comes from the provider list:
// a provider whose adapter offers one-shot prompts lists `oneShotModels`. An
// empty agent or model means the default: the first such agent and its first
// (cheapest) model.

import type { ProviderRecord } from './wire/types';

export type ChatTitleMode = 'prompt' | 'agent';

export interface ChatTitleSettings {
  mode: ChatTitleMode;
  providerId: string;
  model: string;
}

export interface ChatTitleAgent {
  providerId: string;
  name: string;
  models: ReadonlyArray<{ id: string; name: string }>;
}

export const CHAT_TITLES_SETTINGS_KEY = 'chatTitles';

export const DEFAULT_CHAT_TITLES: ChatTitleSettings = { mode: 'prompt', providerId: '', model: '' };

export function parseChatTitles(raw: unknown): ChatTitleSettings {
  const value = raw && typeof raw === 'object' ? (raw as Record<string, unknown>) : {};
  const mode = value.mode === 'agent' || value.mode === 'prompt' ? value.mode : DEFAULT_CHAT_TITLES.mode;
  const providerId = typeof value.providerId === 'string' ? value.providerId.trim() : '';
  const model = typeof value.model === 'string' ? value.model.trim().slice(0, 120) : '';
  return { mode, providerId, model };
}

export function chatTitlesFromSettings(raw: unknown): ChatTitleSettings {
  if (!raw || typeof raw !== 'object') return { ...DEFAULT_CHAT_TITLES };
  const settings = (raw as Record<string, unknown>).settings;
  if (!settings || typeof settings !== 'object') return { ...DEFAULT_CHAT_TITLES };
  return parseChatTitles((settings as Record<string, unknown>)[CHAT_TITLES_SETTINGS_KEY]);
}

export function withChatTitlesInSettings(current: Record<string, unknown> | null | undefined, value: ChatTitleSettings): Record<string, unknown> {
  return { ...(current ?? {}), [CHAT_TITLES_SETTINGS_KEY]: parseChatTitles(value) };
}

/** The enabled agents on this machine that can name chats. */
export function chatTitleAgents(providers: ReadonlyArray<ProviderRecord>): ChatTitleAgent[] {
  const out: ChatTitleAgent[] = [];
  for (const provider of providers) {
    if (!provider.enabled || provider.disabledByUser) continue;
    const models = (provider.oneShotModels ?? []).filter((m) => m && typeof m.id === 'string' && m.id.trim());
    if (models.length) out.push({ providerId: provider.id, name: provider.name || provider.id, models });
  }
  return out;
}

/** The agent and model a chat is named with: the stored choice when it is
 *  still offered, else the default. Null when no agent can name chats. */
export function resolveChatTitleChoice(choice: ChatTitleSettings, agents: ReadonlyArray<ChatTitleAgent>): { agent: ChatTitleAgent; model: { id: string; name: string } } | null {
  const agent = agents.find((a) => a.providerId === choice.providerId) ?? agents[0];
  if (!agent) return null;
  const model = agent.models.find((m) => m.id === choice.model) ?? agent.models[0];
  return { agent, model };
}
