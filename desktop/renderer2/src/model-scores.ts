// User-authored model scoring — a renderer preference layer over the live
// catalog. The user (never the vendor) rates each model against THEIR own
// priorities so the future agent-facing catalog can route on those preferences.
//
// Everything here is pure and JSON-round-trippable. The scores are persisted
// through the daemon's app-settings blob (settings:get / settings:set), NOT the
// session mirror and NOT a second localStorage authority. The wire shape and the
// [1..10] bounds mirror the daemon contract in internal/acp/model_scores.go
// exactly (keys intelligence/taste/cost/note, keyed provider id → base model id),
// so a score authored here round-trips byte-for-byte into what the agent-facing
// catalog reads back.

import type { CatalogGroup, ModelOption, ProviderRecord } from './wire/types.ts';
import { userFacingCatalogGroups, userFacingFlatModels, type WorkassRuntimeProfile } from './model-catalog.ts';

// The rated numeric dimensions, in display order, keyed to the daemon's wire
// contract: `intelligence`, `taste`, `cost` (higher cost = MORE expensive).
// `note` is freeform and handled separately (not a numeric dimension). Labels are
// Spanish to match the rest of the Settings surface.
export const SCORE_DIMENSIONS = [
  { key: 'intelligence', label: 'Inteligencia' },
  { key: 'taste', label: 'Gusto' },
  { key: 'cost', label: 'Costo' },
] as const;

export type ScoreDimension = (typeof SCORE_DIMENSIONS)[number]['key'];

// Whole-number 1..10 scale — the daemon clamps authored scores to exactly this
// range (internal/acp/model_scores.go). A blank field means "unscored", never 0.
export const SCORE_MIN = 1;
export const SCORE_MAX = 10;
// Freeform note cap — short by design; a rating, not an essay. Matches the
// daemon's 500-rune bound so the renderer never authors a note it would truncate.
export const NOTE_MAX = 500;

// The settings blob key the scores live under (settings:get.settings.modelScores /
// settings:set). Kept as a named constant so the persistence boundary is explicit.
export const MODEL_SCORES_SETTINGS_KEY = 'modelScores';

const DIMENSION_KEYS: ScoreDimension[] = SCORE_DIMENSIONS.map((d) => d.key);

// One model's user scores. Every dimension is optional — absent = unscored. A
// short freeform note captures whatever the numbers can't.
export type ModelScore = Partial<Record<ScoreDimension, number>> & { note?: string };

// Keyed by provider id, then model id. Nested (never a composite string key) so a
// model id that contains separators can never collide across providers, and the
// structure round-trips through JSON 1:1.
export type ModelScores = Record<string, Record<string, ModelScore>>;

// Synthetic provider id for the flat-catalog fallback (an older daemon that
// exposes no provider groups, only a flat model list). Non-empty and clearly not
// a real provider slug so guards stay meaningful and it cannot shadow a provider.
export const FLAT_PROVIDER_ID = '__flat__';

// ---- validation / clamping ------------------------------------------------

// Clamp a user-entered score to a whole number in [SCORE_MIN, SCORE_MAX].
// Anything non-numeric (empty field, whitespace, NaN, ±Infinity, null) clears the
// dimension → undefined, so a blank input reads as "unscored", never as 0.
export function clampScore(value: unknown): number | undefined {
  if (value === '' || value === null || value === undefined) return undefined;
  if (typeof value === 'string' && value.trim() === '') return undefined;
  const n = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(n)) return undefined;
  return Math.min(SCORE_MAX, Math.max(SCORE_MIN, Math.round(n)));
}

// Normalize a freeform note: trim, cap length, and treat an all-whitespace note
// as absent so it never survives as an empty string.
export function normalizeNote(note: unknown): string | undefined {
  if (typeof note !== 'string') return undefined;
  const trimmed = note.trim().slice(0, NOTE_MAX);
  return trimmed ? trimmed : undefined;
}

// True when a score carries nothing worth persisting (no dimension set, no note).
export function isEmptyScore(score: ModelScore | undefined | null): boolean {
  if (!score) return true;
  for (const key of DIMENSION_KEYS) if (score[key] !== undefined) return false;
  return !score.note;
}

// Coerce arbitrary (possibly persisted/legacy) data into a clean ModelScore, or
// undefined when nothing valid remains — so callers can drop empty entries.
export function sanitizeScore(raw: unknown): ModelScore | undefined {
  if (!raw || typeof raw !== 'object') return undefined;
  const input = raw as Record<string, unknown>;
  const out: ModelScore = {};
  for (const key of DIMENSION_KEYS) {
    const clamped = clampScore(input[key]);
    if (clamped !== undefined) out[key] = clamped;
  }
  const note = normalizeNote(input.note);
  if (note !== undefined) out.note = note;
  return isEmptyScore(out) ? undefined : out;
}

// ---- serialization (persistence boundary) ---------------------------------

// Validate/normalize a persisted scores blob (from the session mirror). Entries
// for providers/models absent from the current catalog are preserved (the
// catalog can lag saved prefs), but each entry is sanitized and empties are
// dropped so the store never holds junk.
export function parseModelScores(raw: unknown): ModelScores {
  const out: ModelScores = {};
  if (!raw || typeof raw !== 'object') return out;
  for (const [providerId, models] of Object.entries(raw as Record<string, unknown>)) {
    if (!providerId || !models || typeof models !== 'object') continue;
    for (const [modelId, score] of Object.entries(models as Record<string, unknown>)) {
      if (!modelId) continue;
      const clean = sanitizeScore(score);
      if (clean) (out[providerId] ??= {})[modelId] = clean;
    }
  }
  return out;
}

// The serialized form is just the sanitized map (idempotent normalization).
// Named so the persistence boundary is explicit and symmetrical with parse.
export function serializeModelScores(scores: ModelScores): ModelScores {
  return parseModelScores(scores);
}

// ---- settings blob (daemon persistence boundary) --------------------------
// Scores live inside the daemon's app-settings blob, reached through the frozen
// settings:get / settings:set channels. settings:get replies wrap the blob as
// `{ settings, models, modes, taskKinds }`; settings:set REPLACES the stored blob
// (it does not deep-merge), so a write MUST round-trip every existing key and
// touch only modelScores — otherwise it would wipe models/permissionModes/chatMode
// and any unknown keys another surface owns.

// Pull the flat settings object out of a settings:get reply. Returns {} when the
// reply is missing/malformed so a read degrades to "no scores" rather than throw.
export function settingsFromReply(raw: unknown): Record<string, unknown> {
  if (raw && typeof raw === 'object') {
    const inner = (raw as Record<string, unknown>).settings;
    if (inner && typeof inner === 'object') return inner as Record<string, unknown>;
  }
  return {};
}

// Read + sanitize the model scores carried inside a settings:get reply.
export function modelScoresFromSettings(raw: unknown): ModelScores {
  return parseModelScores(settingsFromReply(raw)[MODEL_SCORES_SETTINGS_KEY]);
}

// Merge scores into the current settings blob for settings:set, preserving every
// other key untouched (known and unknown). The scores are serialized (sanitized)
// on the way out so the write is always canonical.
export function withModelScoresInSettings(
  current: Record<string, unknown> | null | undefined,
  scores: ModelScores,
): Record<string, unknown> {
  return { ...(current ?? {}), [MODEL_SCORES_SETTINGS_KEY]: serializeModelScores(scores) };
}

// ---- immutable updates ----------------------------------------------------

function updateModelScore(
  scores: ModelScores,
  providerId: string,
  modelId: string,
  mutate: (prev: ModelScore) => ModelScore,
): ModelScores {
  if (!providerId || !modelId) return scores;
  const prev = scores[providerId]?.[modelId] ?? {};
  const nextScore = mutate(prev);
  const providerScores = { ...(scores[providerId] ?? {}) };
  if (isEmptyScore(nextScore)) delete providerScores[modelId];
  else providerScores[modelId] = nextScore;
  const next = { ...scores };
  if (Object.keys(providerScores).length === 0) delete next[providerId];
  else next[providerId] = providerScores;
  return next;
}

// Set (or clear) a single numeric dimension. An empty/invalid value clears it;
// clearing the last field prunes the model — and, if now empty, the provider.
export function withScoreValue(
  scores: ModelScores,
  providerId: string,
  modelId: string,
  dimension: ScoreDimension,
  value: unknown,
): ModelScores {
  return updateModelScore(scores, providerId, modelId, (prev) => {
    const next = { ...prev };
    const clamped = clampScore(value);
    if (clamped === undefined) delete next[dimension];
    else next[dimension] = clamped;
    return next;
  });
}

// Set (or clear) the freeform note draft for a model. Do not trim here: this
// value feeds a controlled text input, and trimming a just-typed trailing space
// rewrites the DOM value before the next character can arrive ("never use"
// becomes "neveruse"). The field normalizes on blur, and serialization always
// normalizes again at the daemon persistence boundary.
export function withNoteValue(
  scores: ModelScores,
  providerId: string,
  modelId: string,
  note: unknown,
): ModelScores {
  return updateModelScore(scores, providerId, modelId, (prev) => {
    const next = { ...prev };
    const draft = typeof note === 'string' ? note.slice(0, NOTE_MAX) : '';
    if (draft === '') delete next.note;
    else next.note = draft;
    return next;
  });
}

// ---- reset ----------------------------------------------------------------

// Reset one model to unscored (single-row reset). Prunes the provider if empty.
// Returns the same reference when nothing changes so callers can skip a re-render.
export function clearModelScore(scores: ModelScores, providerId: string, modelId: string): ModelScores {
  if (!scores[providerId]?.[modelId]) return scores;
  const providerScores = { ...scores[providerId] };
  delete providerScores[modelId];
  const next = { ...scores };
  if (Object.keys(providerScores).length === 0) delete next[providerId];
  else next[providerId] = providerScores;
  return next;
}

// Reset everything to unscored (panel-level reset action).
export function clearAllModelScores(): ModelScores {
  return {};
}

// ---- reads ----------------------------------------------------------------

export function getModelScore(scores: ModelScores, providerId: string, modelId: string): ModelScore | undefined {
  return scores[providerId]?.[modelId];
}

// How many models carry at least one score — drives the "N puntuados" tag and the
// enabled/disabled state of the reset-all action.
export function countScoredModels(scores: ModelScores): number {
  let n = 0;
  for (const models of Object.values(scores)) n += Object.keys(models).length;
  return n;
}

// ---- grouping -------------------------------------------------------------

export interface ScoreableModel {
  modelId: string;
  name: string;
}
export interface ScoreableGroup {
  providerId: string;
  providerName: string;
  models: ScoreableModel[];
}

function dedupeModels(models: readonly ModelOption[] | undefined): ScoreableModel[] {
  const seen = new Set<string>();
  const out: ScoreableModel[] = [];
  for (const model of models ?? []) {
    if (!model?.modelId || seen.has(model.modelId)) continue;
    seen.add(model.modelId);
    out.push({ modelId: model.modelId, name: model.name || model.modelId });
  }
  return out;
}

// Build the ordered provider→models list the scoring panel renders, from the live
// catalog/state. Prefers the P4 provider-grouped catalog; falls back to the flat
// model list under a single synthetic group when an older daemon exposes no
// groups. Models are deduplicated by id within a group and empty groups dropped.
export function groupModelsForScoring(
  groups: readonly CatalogGroup[],
  models: readonly ModelOption[],
  providers: readonly ProviderRecord[] = [],
  profile: WorkassRuntimeProfile = 'prod',
): ScoreableGroup[] {
  const nameById = new Map(providers.map((p) => [p.id, p.name] as const));
  const out: ScoreableGroup[] = [];
  if (groups.length) {
    for (const group of userFacingCatalogGroups(groups, profile)) {
      const groupModels = dedupeModels(group.models);
      if (groupModels.length === 0) continue;
      out.push({
        providerId: group.providerId,
        providerName: group.providerName || nameById.get(group.providerId) || group.providerId,
        models: groupModels,
      });
    }
    return out;
  }
  const flat = dedupeModels(userFacingFlatModels(models, profile));
  if (flat.length === 0) return out;
  out.push({ providerId: FLAT_PROVIDER_ID, providerName: 'Modelos', models: flat });
  return out;
}
