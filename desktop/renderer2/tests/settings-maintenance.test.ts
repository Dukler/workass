import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  agentProcessesLine, diagnosticsFileName, durationLabel, formatBytes, formatMemoryKb, hibernateLine, recycleLine, revealLabel,
  storagePartsLine, storageTitle,
} from '../src/components/settings/maintenance.ts';
import type { ProcessSummary } from '../src/wire/types.ts';

// What Ajustes · Mantenimiento says. Every number comes from the daemon; these
// pin how it is written.

test('sizes on disk are decimal like Finder, memory binary like Activity Monitor, with a decimal comma', () => {
  assert.equal(formatBytes(0), '0 B');
  assert.equal(formatBytes(512), '512 B');
  assert.equal(formatBytes(14_200_000_000), '14,2 GB');
  assert.equal(formatBytes(980_000_000), '980 MB');
  assert.equal(formatBytes(2_000_000), '2 MB');
  assert.equal(formatMemoryKb(4 * 1024 * 1024), '4 GB');
  assert.equal(formatMemoryKb(2.3 * 1024 * 1024), '2,3 GB');
  assert.equal(formatMemoryKb(300 * 1024), '300 MB');
});

test('the data row splits the total into chats, images, jobs and the rest, leaving out empty parts', () => {
  assert.equal(storagePartsLine({ totalBytes: 21_600_000_000, parts: { chats: 14_200_000_000, images: 6_100_000_000, jobs: 1_300_000_000, other: 0 } }),
    'Chats 14,2 GB · imágenes 6,1 GB · trabajos 1,3 GB');
  assert.equal(storagePartsLine({ parts: { chats: 0, images: 5_000_000, other: 40_000_000 } }), 'Imágenes 5 MB · otros 40 MB');
  assert.equal(storagePartsLine({ parts: {} }), 'Todavía no hay datos guardados.');
});

test('the machine is named only inside the app, and the folder button names the file manager', () => {
  assert.equal(storageTitle(true, 'darwin'), 'Espacio en este Mac');
  assert.equal(storageTitle(true, 'win32'), 'Espacio en esta computadora');
  assert.equal(storageTitle(false, undefined), 'Espacio en disco');
  assert.equal(revealLabel('darwin'), 'Mostrar en Finder');
  assert.equal(revealLabel('win32'), 'Mostrar en el Explorador');
  assert.equal(revealLabel('linux'), 'Abrir carpeta');
});

const proc = (over: Partial<ProcessSummary>): ProcessSummary => ({
  id: 'p', kind: 'acp', label: 'agent', pid: 1, cwd: '/', command: 'agent', chatId: null, managed: true, engine: true,
  status: 'running', code: null, startedAt: '', finishedAt: null, lastLine: '', ...over,
});

test('the agents line counts only running agent processes and adds their memory', () => {
  assert.equal(agentProcessesLine([]), 'Ningún agente en marcha');
  assert.equal(agentProcessesLine([proc({ rssKb: 300 * 1024 })]), '1 proceso de agente · 300 MB');
  assert.equal(agentProcessesLine([
    proc({ id: 'a', rssKb: 1024 * 1024 }), proc({ id: 'b', rssKb: 1.3 * 1024 * 1024 }),
    proc({ id: 'c', status: 'exited', rssKb: 9 * 1024 * 1024 }), proc({ id: 'd', engine: false, rssKb: 9 * 1024 * 1024 }),
  ]), '2 procesos de agentes · 2,3 GB');
  assert.equal(agentProcessesLine([proc({ rssKb: 0 })]), '1 proceso de agente');
});

test('the idle and memory rules are the daemon\'s own values, or nothing', () => {
  assert.equal(hibernateLine({ engine: { hibernateTtlMs: 20 * 60 * 1000 } }), 'Un chat sin actividad cierra su proceso a los 20 min y lo retoma cuando le escribís.');
  assert.equal(hibernateLine({}), '');
  assert.equal(hibernateLine(undefined), '');
  assert.deepEqual(recycleLine({ engine: { maxRssKb: 4 * 1024 * 1024, rssSampleIntervalMs: 30_000 } }),
    { value: '4 GB', detail: 'Si la pasa, se cierra apenas queda sin actividad; se mide cada 30 s.' });
  assert.equal(recycleLine({ engine: {} }), null);
  assert.equal(durationLabel(90 * 60 * 1000), '90 min');
  assert.equal(durationLabel(2 * 60 * 60 * 1000), '2 h');
  assert.equal(durationLabel(250), '0 s');
});

test('the diagnostic file is named by the local date and time', () => {
  assert.equal(diagnosticsFileName(new Date(2026, 10, 9, 15, 7)), 'workass-diagnostico-2026-11-09-1507.json');
});
