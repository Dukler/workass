import type { CatalogGroup, ModelOption } from './wire/types.ts';

export interface ResolvedModelSelection {
  base: string;
  effort: string | null;
  model: ModelOption | null;
}

const CANONICAL_EFFORTS = new Set(['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra']);

/**
 * The base a per-model memory entry is keyed by, matching the daemon's
 * `acp.SplitCanonicalEffortSuffix` exactly (same effort set, same rule).
 *
 * The two ends MUST agree. The daemon rewrites `gpt-5.6-sol[xhigh]` to
 * `gpt-5.6-sol` on every save; a renderer that keeps the suffixed key restores
 * it a moment later and the pair never converges — a session write per second,
 * forever, each one yanking the restored `activeId` back under the user.
 *
 * Only a CANONICAL effort is peeled. Claude ships literal ids like `opus[1m]`,
 * and `1m` is not an effort, so those keys are left alone.
 */
export function canonicalModelControlKey(modelId: string): string {
  const suffix = /^(.*)\[([^[\]]+)\]$/.exec(modelId.trim());
  if (!suffix || !suffix[1]) return modelId;
  return CANONICAL_EFFORTS.has(suffix[2].toLowerCase()) ? suffix[1] : modelId;
}

function findExact(groups: CatalogGroup[], models: ModelOption[], modelId: string): ModelOption | null {
  for (const group of groups) {
    const model = group.models.find((item) => item.modelId === modelId);
    if (model) return model;
  }
  return models.find((item) => item.modelId === modelId) ?? null;
}

// Brackets are ambiguous: Codex uses `${base}[${effort}]`, while Claude uses
// literal adapter model ids such as `opus[1m]`. An exact catalog id always wins.
// Prefer an advertised effort; a canonical suffix on an existing base can also
// be normalized when a persisted capability has disappeared.
export function resolveModelSelection(
  groups: CatalogGroup[],
  models: ModelOption[],
  selectedId: string | null | undefined,
): ResolvedModelSelection {
  if (!selectedId) return { base: '', effort: null, model: null };

  const exact = findExact(groups, models, selectedId);
  if (exact) return { base: selectedId, effort: null, model: exact };

  const suffix = /^(.*)\[([^\[\]]+)\]$/.exec(selectedId);
  if (suffix) {
    const baseModel = findExact(groups, models, suffix[1]);
    const advertisedEffort = baseModel?.efforts?.find((effort) => effort.toLowerCase() === suffix[2].toLowerCase());
    if (baseModel && advertisedEffort) {
      return { base: suffix[1], effort: advertisedEffort, model: baseModel };
    }
    // A persisted selection can outlive a provider capability change. Exact
    // catalog ids already won above, so a canonical suffix on an existing base
    // can be normalized safely: retain the user's model and drop only the now-invalid
    // effort (notably stale haiku[low] from older Claude catalogs).
    if (baseModel && CANONICAL_EFFORTS.has(suffix[2].toLowerCase())) {
      return { base: suffix[1], effort: null, model: baseModel };
    }
  }

  return { base: selectedId, effort: null, model: null };
}

// Resolve a stale selection only when the chat's own per-model memory proves
// which literal provider id it used. This is intentionally narrower than name
// or prefix guessing: exactly one remembered non-effort bracket variant must
// share the degraded selection's root.
export function recoverRememberedLiteralVariant(
  models: ModelOption[],
  selectedId: string | null | undefined,
  rememberedModelIds: readonly string[],
): ModelOption | null {
  if (!selectedId) return null;
  if (findExact([], models, selectedId)) return null;
  const selectedSuffix = /^(.*)\[([^\[\]]+)\]$/.exec(selectedId);
  const root = selectedSuffix && CANONICAL_EFFORTS.has(selectedSuffix[2].toLowerCase())
    ? selectedSuffix[1]
    : selectedId;
  const remembered = new Set(rememberedModelIds);
  const matches = models.filter((model) => {
    if (!remembered.has(model.modelId)) return false;
    const suffix = /^(.*)\[([^\[\]]+)\]$/.exec(model.modelId);
    return !!suffix
      && suffix[1] === root
      && !CANONICAL_EFFORTS.has(suffix[2].toLowerCase());
  });
  return matches.length === 1 ? matches[0] : null;
}

// Claude's adapter can restore its synthetic `default` alias even after the
// user explicitly selected a visible model. The daemon now canonicalizes that
// alias, but older persisted chats still need deterministic normalization. Use only
// this chat's own per-model memory and require exactly one remembered model that
// still exists in the catalog; catalog order and display-name guesses are never
// model identity.
export function recoverRememberedCatalogModel(
  models: ModelOption[],
  selectedId: string | null | undefined,
  rememberedModelIds: readonly string[],
  providerId: string | null | undefined,
): ModelOption | null {
  if (providerId === 'claude' && selectedId?.trim().toLowerCase() === 'default') {
    const remembered = new Set(rememberedModelIds);
    const matches = models.filter((model) => remembered.has(model.modelId));
    return matches.length === 1 ? matches[0] : null;
  }
  return recoverRememberedLiteralVariant(models, selectedId, rememberedModelIds);
}

export function modelContextQualifier(modelId: string | null | undefined): string | null {
  if (!modelId) return null;
  const suffix = /^(.*)\[([^\[\]]+)\]$/.exec(modelId);
  if (!suffix || CANONICAL_EFFORTS.has(suffix[2].toLowerCase())) return null;
  return suffix[2].toUpperCase();
}

export function compactModelProviderLabel(providerId: string | null | undefined, providerName?: string | null): string {
  if (providerId === 'claude') return 'Claude';
  if (providerId === 'codex') return 'Codex';
  const label = (providerName || providerId || '').trim().replace(/\s+ACP$/i, '');
  return label || 'Agente';
}
