import type { CatalogGroup, ProviderRecord, ProviderUpdate } from './wire/types.ts';

// Provider branding is registration metadata. Identity selects the matching
// row; it never implies a capability or a model family by itself.
export function brandForProvider(
  providerId: string | null | undefined,
  providers: readonly ProviderRecord[] = [],
  groups: readonly CatalogGroup[] = [],
): string {
  if (!providerId) return '';
  return groups.find((group) => group.providerId === providerId)?.assistantBrand
    ?? providers.find((provider) => provider.id === providerId)?.assistantBrand
    ?? '';
}

// The failed step stays on the card only while there is still something to retry.
// A CLI that reached the version by itself leaves nothing for Reintentar to run.
export function updateChainAfterCheck<T extends { failedId: string | null }>(chain: T | undefined, updates: readonly ProviderUpdate[]): T | undefined {
  if (!chain?.failedId) return chain;
  return updates.some((update) => update.providerId === chain.failedId && update.updateAvailable) ? chain : undefined;
}
