import type { CatalogGroup, ModelOption, ProviderRecord } from './wire/types.ts';

export type WorkassRuntimeProfile = 'prod' | 'dev' | 'test';

export function workassRuntimeProfile(value: unknown): WorkassRuntimeProfile {
  return value === 'dev' || value === 'test' ? value : 'prod';
}

// Development fixtures stay visible and selectable in dev/test. Production
// hides only these narrow identities: preview/beta is a legitimate vendor
// lifecycle, so names such as "preview" or "experimental" must never be hidden
// by a broad prose heuristic.
const INTERNAL_PROVIDER_IDS = new Set(['mock']);
const INTERNAL_MODEL_IDS = new Set([
  'mock-deterministic',
  'coder-model(qwen-oauth)',
]);

export function isProductionFixtureIdentity(
  providerId: string | null | undefined,
  modelId: string | null | undefined,
  name?: string | null,
): boolean {
  const provider = (providerId ?? '').trim().toLowerCase();
  if (INTERNAL_PROVIDER_IDS.has(provider)) return true;
  const id = (modelId ?? '').trim().toLowerCase();
  const label = (name ?? modelId ?? '').trim().toLowerCase();
  if (INTERNAL_MODEL_IDS.has(id)) return true;
  if (id.includes('workass-dev')) return true;
  return label === 'mock deterministic' || label === 'coder-model' || label === 'workass-dev';
}

export function isUserFacingModel(
  providerId: string | null | undefined,
  model: ModelOption,
  profile: WorkassRuntimeProfile = 'prod',
): boolean {
  if (profile !== 'prod') return true;
  return !isProductionFixtureIdentity(providerId, model.modelId, model.name);
}

// Subagent timeline events persist a friendly model label rather than the raw
// model id. This narrow compatibility filter removes old production fixture
// receipts (including deterministic canaries) without hiding real providers.
export function isUserFacingSubagentIdentity(
  providerId: string | null | undefined,
  modelLabel: string | null | undefined,
  profile: WorkassRuntimeProfile = 'prod',
): boolean {
  if (profile !== 'prod') return true;
  const provider = (providerId ?? '').trim().toLowerCase();
  const label = (modelLabel ?? '').trim().toLowerCase();
  if (provider === 'mock') return false;
  if (label.startsWith('mockdeterministic')) return false;
  if (label.startsWith('coder-model')) return false;
  if (label.includes('workass-dev')) return false;
  return true;
}

export function userFacingCatalogGroups(
  groups: readonly CatalogGroup[],
  profile: WorkassRuntimeProfile = 'prod',
): CatalogGroup[] {
  const visible: CatalogGroup[] = [];
  for (const group of groups) {
    const models = (group.models ?? []).filter((model) => isUserFacingModel(group.providerId, model, profile));
    if (models.length === 0) continue;
    visible.push({ ...group, models });
  }
  return visible;
}

// Older hosts expose a flat list with no provider identity. The model-id rules
// still remove the same fixtures; provider-only rules cannot safely be guessed.
export function userFacingFlatModels(
  models: readonly ModelOption[],
  profile: WorkassRuntimeProfile = 'prod',
): ModelOption[] {
  return models.filter((model) => isUserFacingModel('', model, profile));
}

export function userFacingProviders(
  providers: readonly ProviderRecord[],
  profile: WorkassRuntimeProfile = 'prod',
): ProviderRecord[] {
  if (profile !== 'prod') return [...providers];
  return providers.filter((provider) => provider.id.trim().toLowerCase() !== 'mock');
}
