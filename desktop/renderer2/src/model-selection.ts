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
  }

  return { base: selectedId, effort: null, model: null };
}

export function modelContextQualifier(modelId: string | null | undefined): string | null {
  if (!modelId) return null;
  const suffix = /^(.*)\[([^\[\]]+)\]$/.exec(modelId);
  if (!suffix || CANONICAL_EFFORTS.has(suffix[2].toLowerCase())) return null;
  return suffix[2].toUpperCase();
}
