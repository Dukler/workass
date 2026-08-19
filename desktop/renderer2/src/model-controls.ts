import type { ModeOption, ModelOption } from './wire/types.ts';
import { canonicalModelControlKey } from './model-selection.ts';

export interface RememberedModelControls {
  effort?: string;
  modeId?: string;
}

export interface RestoredControlSelection {
  modelId: string | null;
  modeId: string | null;
}

export type ImageDraftCapability = 'unknown' | 'supported' | 'unsupported';

export interface RestoredProviderBinding {
  selectedProviderId: string | null;
  sessionProviderId: string | null;
  useLiveControls: boolean;
}

// Per chat, nested by provider then base model id. Provider is part of the key
// because two agents may advertise the same model id with different controls.
export type ModelControlMemory = Record<string, Record<string, RememberedModelControls>>;

// A new chat may already be initializing its inherited provider to refresh plan
// usage when the user picks another provider. The explicit picker change must
// win over that older async response, even before a session id exists.
export function nextModelControlRevision(current: number | undefined): number {
  return (current ?? 0) + 1;
}

export function modelControlsChangedDuringInit(startedRevision: number, currentRevision: number | undefined): boolean {
  return (currentRevision ?? 0) !== startedRevision;
}

export function providerSwitchRequiresHandover(
  previousProviderId: string | null | undefined,
  nextProviderId: string | null | undefined,
): boolean {
  return !!nextProviderId && nextProviderId !== previousProviderId;
}

// Provider selection and live-session ownership intentionally diverge while a
// user stages the next agent during another provider's turn. Capabilities from
// that old engine are not negative evidence about the selected provider: until
// the selected provider owns a session, image support is unresolved and the
// composer must keep the user's pasted file.
export function imageDraftCapability(
  sessionId: string | null | undefined,
  sessionProviderId: string | null | undefined,
  selectedProviderId: string | null | undefined,
  imageSupport: boolean | undefined,
): ImageDraftCapability {
  if (!sessionId || !sessionProviderId || !selectedProviderId || sessionProviderId !== selectedProviderId) {
    return 'unknown';
  }
  // Capability fields are additive. An older daemon or a metadata-only
  // projection can attach the exact session/provider without carrying this
  // field, and absence is not evidence that the provider rejects images.
  if (imageSupport === undefined) return 'unknown';
  return imageSupport ? 'supported' : 'unsupported';
}

// The persisted picker is the user's staged intent. A runtime liveSession is
// separate evidence about the engine that currently owns the chat. Live model,
// mode, commands and capabilities are authoritative only when both providers
// match; otherwise using them would erase the staged provider on reload.
export function restoredProviderBinding(
  persistedProviderId: string | null | undefined,
  liveProviderId: string | null | undefined,
): RestoredProviderBinding {
  const sessionProviderId = liveProviderId ?? null;
  const selectedProviderId = persistedProviderId ?? sessionProviderId;
  return {
    selectedProviderId,
    sessionProviderId,
    useLiveControls: !!sessionProviderId && selectedProviderId === sessionProviderId,
  };
}

// A live ACP session is the authority for what the adapter is actually using.
// Persisted fields are the fallback only when the daemon has no live binding
// (for example after a daemon restart). This prevents a renderer reload from
// repainting and resending a stale provider's controls over the live session.
export function restoredControlSelection(
  persistedModelId: string | null | undefined,
  persistedModeId: string | null | undefined,
  live?: RestoredControlSelection | null,
): RestoredControlSelection {
  if (live) return { modelId: live.modelId, modeId: live.modeId };
  return {
    modelId: persistedModelId ?? null,
    modeId: persistedModeId ?? null,
  };
}

const MODE_FAMILIES = {
  unrestricted: ['agent-full-access', 'bypassPermissions', 'bypass', 'dontAsk'],
  readonly: ['read-only', 'plan'],
  agent: ['agent', 'default', 'ask', 'auto', 'acceptEdits', 'guardian'],
} as const;

type ModeFamily = keyof typeof MODE_FAMILIES;

function modeFamily(modeId: string | null | undefined): ModeFamily | null {
  const id = (modeId ?? '').trim().toLowerCase();
  if (!id) return null;
  for (const [family, aliases] of Object.entries(MODE_FAMILIES) as Array<[ModeFamily, readonly string[]]>) {
    if (aliases.some((alias) => alias.toLowerCase() === id)) return family;
  }
  return null;
}

function availableMode(modes: ModeOption[], id: string | null | undefined): string | null {
  const wanted = (id ?? '').trim();
  if (!wanted) return null;
  return modes.find((mode) => mode.id === wanted)?.id ?? null;
}

// Preserve an exact provider-native id whenever possible. Across providers,
// translate only within the same permission intent (full access, read-only, or
// normal agent). If that is impossible, use the freshly initialized provider
// default rather than sending a stale id and aborting the turn.
export function compatibleModeId(
  requested: string | null | undefined,
  modes: ModeOption[],
  providerDefault?: string | null,
): string | null {
  const exact = availableMode(modes, requested);
  if (exact) return exact;
  const family = modeFamily(requested);
  if (family) {
    for (const alias of MODE_FAMILIES[family]) {
      const match = modes.find((mode) => mode.id.toLowerCase() === alias.toLowerCase());
      if (match) return match.id;
    }
  }
  return availableMode(modes, providerDefault);
}

export function defaultEffortId(efforts: string[]): string | null {
  if (efforts.length === 0) return null;
  if (efforts.includes('high')) return 'high';
  const light = efforts.filter((effort) => effort !== 'max' && effort !== 'ultra');
  return light[light.length - 1] ?? efforts[0] ?? null;
}

export function compatibleEffortId(
  remembered: string | null | undefined,
  model: ModelOption | null | undefined,
  fallback?: string | null,
): string | null {
  const efforts = model?.efforts ?? [];
  if (efforts.length === 0) return null;
  if (remembered && efforts.includes(remembered)) return remembered;
  if (fallback && efforts.includes(fallback)) return fallback;
  return defaultEffortId(efforts);
}

export function composeModelSelection(modelId: string, effort: string | null): string {
  return effort ? `${modelId}[${effort}]` : modelId;
}

export function rememberedModelControls(
  memory: ModelControlMemory | undefined,
  providerId: string | null | undefined,
  modelId: string | null | undefined,
): RememberedModelControls | undefined {
  if (!providerId || !modelId) return undefined;
  // Read by the same key it is written under, or a remembered effort goes
  // missing the moment its model leaves the catalog.
  return memory?.[providerId]?.[canonicalModelControlKey(modelId)];
}

export function rememberModelControls(
  memory: ModelControlMemory | undefined,
  providerId: string | null | undefined,
  modelId: string | null | undefined,
  controls: RememberedModelControls,
): ModelControlMemory | undefined {
  if (!providerId || !modelId) return memory;
  // Canonical here, not at the call sites. When a model leaves the catalog,
  // resolveModelSelection can no longer split it and hands back the whole
  // `gpt-5.6-sol[xhigh]` as the base — so a reconcile pass re-added the suffixed
  // key the daemon had just stripped, forever. Keying centrally means no future
  // caller can reintroduce that.
  modelId = canonicalModelControlKey(modelId);
  const next = memory ?? {};
  const provider = next[providerId] ?? (next[providerId] = {});
  const previous = provider[modelId] ?? {};
  provider[modelId] = {
    ...(previous.effort ? { effort: previous.effort } : {}),
    ...(previous.modeId ? { modeId: previous.modeId } : {}),
    ...(controls.effort ? { effort: controls.effort } : {}),
    ...(controls.modeId ? { modeId: controls.modeId } : {}),
  };
  return next;
}

export function normalizeModelControlMemory(raw: unknown): ModelControlMemory | undefined {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return undefined;
  const out: ModelControlMemory = {};
  for (const [providerId, providerRaw] of Object.entries(raw as Record<string, unknown>)) {
    if (!providerId || !providerRaw || typeof providerRaw !== 'object' || Array.isArray(providerRaw)) continue;
    const provider: Record<string, RememberedModelControls> = {};
    for (const [modelId, controlsRaw] of Object.entries(providerRaw as Record<string, unknown>)) {
      if (!modelId || !controlsRaw || typeof controlsRaw !== 'object' || Array.isArray(controlsRaw)) continue;
      const controls = controlsRaw as Record<string, unknown>;
      const effort = typeof controls.effort === 'string' && controls.effort ? controls.effort : undefined;
      const modeId = typeof controls.modeId === 'string' && controls.modeId ? controls.modeId : undefined;
      if (!effort && !modeId) continue;
      // Keyed the way the daemon keys it. A suffixed key read back unchanged is
      // written back unchanged, and the daemon strips it again on every save.
      // An entry already stored under the base wins, matching the daemon's own
      // migration, so collapsing never overwrites the newer shape.
      const key = canonicalModelControlKey(modelId);
      if (key !== modelId && provider[key]) continue;
      provider[key] = { ...(effort ? { effort } : {}), ...(modeId ? { modeId } : {}) };
    }
    if (Object.keys(provider).length) out[providerId] = provider;
  }
  return Object.keys(out).length ? out : undefined;
}
