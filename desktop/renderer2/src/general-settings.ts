// Ajustes · General, kept in the daemon's app-settings blob (normalized by
// cmd/workass/general_settings.go, which this file mirrors):
//  - newChat: what a chat started here begins with. No agent keeps the open
//    chat as the model; folder "last" is the last project a chat was started
//    in, "active" the open chat's folder, anything else one fixed folder.
//  - whileWorking: what Enter does while a turn runs.
//  - notices: which events are announced, and with a sound or not.
//  - dictation: the language whisper is told (applied by the daemon).

import type { CatalogGroup, ModelOption } from './wire/types';

export type WhileWorking = 'queue' | 'steer';
export interface NewChatDefaults { providerId: string; model: string; effort: string; modeId: string; folder: string }
export interface NoticeSettings { needsYou: boolean; turnEnded: boolean; jobs: boolean; sound: boolean }
export interface GeneralSettings {
  newChat: NewChatDefaults;
  whileWorking: WhileWorking;
  notices: NoticeSettings;
  dictation: { lang: string };
}

export const NEW_CHAT_FOLDER_LAST = 'last';
export const NEW_CHAT_FOLDER_ACTIVE = 'active';

export const DEFAULT_GENERAL_SETTINGS: GeneralSettings = {
  newChat: { providerId: '', model: '', effort: '', modeId: '', folder: NEW_CHAT_FOLDER_LAST },
  whileWorking: 'queue',
  notices: { needsYou: true, turnEnded: true, jobs: true, sound: false },
  dictation: { lang: 'es' },
};

const ID_MAX = 200;
const PATH_MAX = 4096;

function obj(raw: unknown): Record<string, unknown> {
  return raw && typeof raw === 'object' && !Array.isArray(raw) ? raw as Record<string, unknown> : {};
}
function id(raw: unknown): string {
  const value = typeof raw === 'string' ? raw.trim() : '';
  return value.length > ID_MAX ? '' : value;
}

export function parseNewChatFolder(raw: unknown): string {
  const folder = typeof raw === 'string' ? raw.trim() : '';
  if (folder === NEW_CHAT_FOLDER_LAST || folder === NEW_CHAT_FOLDER_ACTIVE) return folder;
  if (folder.length > PATH_MAX) return NEW_CHAT_FOLDER_LAST;
  if (folder.startsWith('/') || folder.startsWith('\\\\') || /^[A-Za-z]:[\\/]/.test(folder)) return folder;
  return NEW_CHAT_FOLDER_LAST;
}

export function parseNewChatDefaults(raw: unknown): NewChatDefaults {
  const value = obj(raw);
  const out: NewChatDefaults = { ...DEFAULT_GENERAL_SETTINGS.newChat, folder: parseNewChatFolder(value.folder) };
  const providerId = id(value.providerId);
  if (!providerId) return out;
  out.providerId = providerId;
  out.modeId = id(value.modeId);
  const model = id(value.model);
  if (model) { out.model = model; out.effort = id(value.effort); }
  return out;
}

export function parseGeneralSettings(raw: unknown): GeneralSettings {
  const value = obj(raw);
  const notices = obj(value.notices);
  const flag = (key: keyof NoticeSettings) => typeof notices[key] === 'boolean' ? notices[key] as boolean : DEFAULT_GENERAL_SETTINGS.notices[key];
  const lang = typeof obj(value.dictation).lang === 'string' ? (obj(value.dictation).lang as string).trim().toLowerCase() : '';
  return {
    newChat: parseNewChatDefaults(value.newChat),
    whileWorking: value.whileWorking === 'steer' ? 'steer' : 'queue',
    notices: { needsYou: flag('needsYou'), turnEnded: flag('turnEnded'), jobs: flag('jobs'), sound: flag('sound') },
    dictation: { lang: lang === 'auto' || /^[a-z]{2,3}$/.test(lang) ? lang : DEFAULT_GENERAL_SETTINGS.dictation.lang },
  };
}

/** The General settings out of a settings:get reply. */
export function generalSettingsFromSettings(reply: unknown): GeneralSettings {
  return parseGeneralSettings(obj(reply).settings);
}

/** The blob with the General keys replaced and every other key kept. */
export function withGeneralSettingsInSettings(current: Record<string, unknown> | null | undefined, value: GeneralSettings): Record<string, unknown> {
  const clean = parseGeneralSettings(value);
  return { ...(current ?? {}), newChat: clean.newChat, whileWorking: clean.whileWorking, notices: clean.notices, dictation: clean.dictation };
}

/** The agents a new chat can start with: the ready ones in this machine's catalog. */
export function newChatAgents(groups: readonly CatalogGroup[]): CatalogGroup[] {
  return groups.filter((group) => (!group.status || group.status === 'ready') && group.models.length > 0);
}

/** The model a default names, or the agent's first (its own default). */
export function newChatModel(defaults: NewChatDefaults, group: CatalogGroup): ModelOption | null {
  return group.models.find((model) => model.modelId === defaults.model) ?? group.models[0] ?? null;
}

/** What Enter does while a turn runs; ⌘/Ctrl+Enter does the other thing. A
 *  chat whose agent cannot be steered keeps the queue on Enter. */
export function busyEnterIntent(whileWorking: WhileWorking, modified: boolean, canSteer = true): 'queue' | 'steer' {
  const enter = whileWorking === 'steer' && canSteer ? 'steer' : 'queue';
  if (!modified) return enter;
  return enter === 'steer' ? 'queue' : 'steer';
}

export const EFFORT_WORD: Record<string, string> = {
  none: 'Ninguno', minimal: 'Mínimo', low: 'Bajo', medium: 'Medio', high: 'Alto', xhigh: 'Muy alto', max: 'Máx', ultra: 'Ultra',
};
