// Chat titles written by an agent (daemon cmd/workass/chat_titles.go). The
// setting lives in the daemon's app-settings blob under "chatTitles"; with
// mode "agent", the first message of a chat is sent once to the chosen agent
// and model for a short title. "prompt" keeps the first-message title.

export type ChatTitleMode = 'prompt' | 'agent';

export interface ChatTitleSettings {
  mode: ChatTitleMode;
  providerId: string;
  model: string;
}

export const CHAT_TITLES_SETTINGS_KEY = 'chatTitles';

export const DEFAULT_CHAT_TITLES: ChatTitleSettings = { mode: 'prompt', providerId: 'claude', model: 'haiku' };

/** The agents that can write titles today, and the models offered for it.
 *  Claude Code takes model aliases, so the newest model of each family is
 *  used without the list going stale. */
export const CHAT_TITLE_AGENTS: ReadonlyArray<{ providerId: string; name: string; models: ReadonlyArray<{ id: string; name: string; note: string }> }> = [
  {
    providerId: 'claude',
    name: 'Claude Code',
    models: [
      { id: 'haiku', name: 'Haiku', note: 'rápido y barato' },
      { id: 'sonnet', name: 'Sonnet', note: 'más fino' },
      { id: 'opus', name: 'Opus', note: 'el más capaz, el más caro' },
    ],
  },
];

export function parseChatTitles(raw: unknown): ChatTitleSettings {
  const value = raw && typeof raw === 'object' ? (raw as Record<string, unknown>) : {};
  const mode = value.mode === 'agent' || value.mode === 'prompt' ? value.mode : DEFAULT_CHAT_TITLES.mode;
  const providerId = typeof value.providerId === 'string' && value.providerId.trim() ? value.providerId.trim() : DEFAULT_CHAT_TITLES.providerId;
  const model = typeof value.model === 'string' && value.model.trim() ? value.model.trim().slice(0, 120) : DEFAULT_CHAT_TITLES.model;
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
