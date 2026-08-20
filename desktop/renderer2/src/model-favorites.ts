import type { CatalogGroup, ModelOption } from './wire/types.ts';

export const MODEL_FAVORITES_SETTINGS_KEY = 'modelFavorites';
export const MAX_MODEL_FAVORITES = 100;

export interface ModelFavorite {
  providerId: string;
  modelId: string;
}

export interface FavoriteCatalogModel extends ModelFavorite {
  providerName: string;
  model: ModelOption;
}

export function parseModelFavorites(raw: unknown): ModelFavorite[] {
  if (!Array.isArray(raw)) return [];
  const out: ModelFavorite[] = [];
  const seen = new Set<string>();
  for (const value of raw) {
    if (!value || typeof value !== 'object') continue;
    const providerId = typeof (value as Record<string, unknown>).providerId === 'string'
      ? ((value as Record<string, unknown>).providerId as string).trim()
      : '';
    const modelId = typeof (value as Record<string, unknown>).modelId === 'string'
      ? ((value as Record<string, unknown>).modelId as string).trim()
      : '';
    if (!providerId || !modelId) continue;
    const key = favoriteKey(providerId, modelId);
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ providerId, modelId });
    if (out.length >= MAX_MODEL_FAVORITES) break;
  }
  return out;
}

export function serializeModelFavorites(favorites: readonly ModelFavorite[]): ModelFavorite[] {
  return parseModelFavorites(favorites);
}

export function modelFavoritesFromSettings(raw: unknown): ModelFavorite[] {
  if (!raw || typeof raw !== 'object') return [];
  const settings = (raw as Record<string, unknown>).settings;
  if (!settings || typeof settings !== 'object') return [];
  return parseModelFavorites((settings as Record<string, unknown>)[MODEL_FAVORITES_SETTINGS_KEY]);
}

export function withModelFavoritesInSettings(
  current: Record<string, unknown> | null | undefined,
  favorites: readonly ModelFavorite[],
): Record<string, unknown> {
  return { ...(current ?? {}), [MODEL_FAVORITES_SETTINGS_KEY]: serializeModelFavorites(favorites) };
}

export function favoriteKey(providerId: string, modelId: string): string {
  return JSON.stringify([providerId, modelId]);
}

export function isModelFavorite(
  favorites: readonly ModelFavorite[], providerId: string, modelId: string,
): boolean {
  return favorites.some((favorite) => favorite.providerId === providerId && favorite.modelId === modelId);
}

export function toggleModelFavorite(
  favorites: readonly ModelFavorite[], providerId: string, modelId: string,
): ModelFavorite[] {
  const clean = parseModelFavorites(favorites);
  const index = clean.findIndex((favorite) => favorite.providerId === providerId && favorite.modelId === modelId);
  if (index >= 0) return clean.filter((_, current) => current !== index);
  return parseModelFavorites([...clean, { providerId, modelId }]);
}

// Resolve in the user's saved order. Stale favorites remain persisted so a
// temporarily missing provider can recover later, but only live, user-facing
// catalog entries are rendered.
export function favoriteCatalogModels(
  groups: readonly CatalogGroup[], favorites: readonly ModelFavorite[],
): FavoriteCatalogModel[] {
  const byKey = new Map<string, FavoriteCatalogModel>();
  for (const group of groups) {
    for (const model of group.models) {
      byKey.set(favoriteKey(group.providerId, model.modelId), {
        providerId: group.providerId,
        providerName: group.providerName || group.providerId,
        modelId: model.modelId,
        model,
      });
    }
  }
  return parseModelFavorites(favorites)
    .map((favorite) => byKey.get(favoriteKey(favorite.providerId, favorite.modelId)))
    .filter((favorite): favorite is FavoriteCatalogModel => !!favorite);
}
