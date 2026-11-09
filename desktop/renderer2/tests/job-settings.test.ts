import assert from 'node:assert/strict';
import { afterEach, test } from 'node:test';
import {
  DEFAULT_JOB_SETTINGS, JOBS_SETTINGS_KEY, fmtHeld, jobSettingsFromReply, loadJobSettings, parseJobSettings,
  parseResources, resourceLine, saveJobSettings, silenceChoiceLabel, silenceChoices, withJobSettings,
} from '../src/job-settings.ts';

const globals = globalThis as unknown as { window?: { api?: Record<string, unknown> } };

afterEach(() => { delete globals.window; });

test('the silence notice delay survives the settings envelope and refuses nonsense', () => {
  assert.deepEqual(parseJobSettings(undefined), DEFAULT_JOB_SETTINGS);
  assert.deepEqual(parseJobSettings({ silenceNoticeAfterMin: -3 }), DEFAULT_JOB_SETTINGS);
  assert.deepEqual(parseJobSettings({ silenceNoticeAfterMin: 'soon' }), DEFAULT_JOB_SETTINGS);
  assert.deepEqual(parseJobSettings({ silenceNoticeAfterMin: 24 * 60 + 1 }), DEFAULT_JOB_SETTINGS, 'past the daemon ceiling');
  assert.deepEqual(parseJobSettings({ silenceNoticeAfterMin: '30' }), { silenceNoticeAfterMin: 30 });
  assert.deepEqual(jobSettingsFromReply({ settings: { jobs: { silenceNoticeAfterMin: 60 } } }), { silenceNoticeAfterMin: 60 });
  const merged = withJobSettings({ chatTitles: { mode: 'agent' } }, { silenceNoticeAfterMin: 10 });
  assert.deepEqual(merged.chatTitles, { mode: 'agent' }, 'saving the delay keeps every other setting');
  assert.deepEqual(merged[JOBS_SETTINGS_KEY], { silenceNoticeAfterMin: 10 });
});

test('the default reads as the three minutes it is, and a value set elsewhere is still offered', () => {
  assert.equal(silenceChoiceLabel(0), '3 min');
  assert.equal(silenceChoiceLabel(10), '10 min');
  assert.equal(silenceChoiceLabel(60), '1 h');
  assert.deepEqual(silenceChoices(0), [0, 10, 30, 60]);
  assert.deepEqual(silenceChoices(15), [0, 10, 15, 30, 60]);
});

test('a daemon that keeps no job setting hides the row instead of showing a default', async () => {
  globals.window = { api: { getSettings: async () => ({ settings: { chatTitles: {} } }) } };
  assert.equal(await loadJobSettings(), null);
  globals.window = { api: { getSettings: async () => ({ settings: { jobs: { silenceNoticeAfterMin: 30 } } }) } };
  assert.deepEqual(await loadJobSettings(), { silenceNoticeAfterMin: 30 });
});

test('saving writes only "jobs" over a fresh read and retries a concurrent write', async () => {
  let stored: Record<string, unknown> = { chatTitles: { mode: 'agent' }, jobs: { silenceNoticeAfterMin: 0 } };
  let revision = 1;
  let conflicts = 1;
  const writes: Array<{ blob: Record<string, unknown>; expected?: string }> = [];
  globals.window = {
    api: {
      getSettings: async () => ({ settings: stored, settingsRevision: `r${revision}` }),
      setSettings: async (blob: Record<string, unknown>, options?: { expectedRevision: string }) => {
        writes.push({ blob, expected: options?.expectedRevision });
        if (conflicts-- > 0) {
          // Another window saved in between.
          stored = { ...stored, modelFavorites: [{ providerId: 'a', modelId: 'b' }] };
          revision++;
          throw new Error('settings changed since they were read');
        }
        stored = blob;
        revision++;
        return blob;
      },
    },
  };
  const saved = await saveJobSettings({ silenceNoticeAfterMin: 30 });
  assert.deepEqual(saved, { silenceNoticeAfterMin: 30 });
  assert.equal(writes.length, 2);
  assert.equal(writes[0].expected, 'r1');
  assert.equal(writes[1].expected, 'r2');
  assert.deepEqual(stored.chatTitles, { mode: 'agent' });
  assert.deepEqual(stored.modelFavorites, [{ providerId: 'a', modelId: 'b' }], 'the concurrent write is not undone');
  assert.deepEqual(stored.jobs, { silenceNoticeAfterMin: 30 });
});

test('a resource line names the holder, how long it has held and the queue', () => {
  const resources = parseResources({
    resources: [
      { name: 'cpu-big', waiting: 0 },
      { name: 'gpu0', waiting: '2', holder: { workId: 'xwHOLDER01', label: 'train cf64', heldSince: '2026-11-09T10:00:00Z', heldForMs: '7440000' } },
      { name: 'gpu1', waiting: 0, holder: { workId: 'xwNOLABEL1' } },
      { name: '', waiting: 3 },
      'junk',
    ],
  });
  assert.ok(resources);
  assert.deepEqual(resources.map((r) => r.name), ['cpu-big', 'gpu0', 'gpu1']);
  assert.equal(resourceLine(resources[0]), 'Libre');
  assert.equal(resourceLine(resources[1]), 'Ocupado por «train cf64» desde hace 2 h 04 min · 2 en cola');
  assert.equal(resourceLine(resources[2]), 'Ocupado por «xwNOLABEL1»', 'no hold time is claimed before the holder stated one');
  assert.equal(parseResources({ nope: true }), null, 'an answer without the list is no answer');
});

test('hold times read the way people say them', () => {
  assert.equal(fmtHeld(20_000), 'menos de un minuto');
  assert.equal(fmtHeld(12 * 60_000), '12 min');
  assert.equal(fmtHeld((2 * 60 + 4) * 60_000), '2 h 04 min');
  assert.equal(fmtHeld((27 * 60 + 5) * 60_000), '1 d 3 h');
});
