import assert from 'node:assert/strict';
import test from 'node:test';
import type { DirListing } from '../src/wire/types.ts';
import {
  canSelectFolder, errorListing, folderCountLabel, folderNameError, isFolderClickThrough, isServerRoots,
  normalizeCreatedDirectory, normalizeEntries, normalizeListing,
  pathLabel, SERVER_ROOTS_LABEL,
} from '../src/workspace-picker.ts';

test('the default request resolves to the server user home and is selectable', () => {
  const home = normalizeListing(
    { path: '/Users/me', parent: '/Users', entries: [{ name: 'Documents', path: '/Users/me/Documents' }, { name: 'Workspace', path: '/Users/me/Workspace' }] },
    null,
  );
  assert.equal(home.path, '/Users/me');
  assert.equal(home.parent, '/Users');
  assert.equal(home.error, undefined);
  assert.deepEqual(home.entries.map((entry) => entry.path), ['/Users/me/Documents', '/Users/me/Workspace']);
  assert.equal(isServerRoots(home), false);
  assert.equal(canSelectFolder(home), true);
  assert.equal(pathLabel(home), '/Users/me');
  assert.equal(folderCountLabel(home), '2 carpetas');
});

test('the explicit roots reply keeps its null path and is never selectable', () => {
  const roots = normalizeListing(
    { path: null, parent: null, entries: [{ name: 'Inicio', path: '/Users/me' }] },
    null,
  );
  assert.equal(roots.path, null);
  assert.equal(roots.parent, null);
  assert.equal(roots.error, undefined);
  assert.deepEqual(roots.entries.map((entry) => entry.path), ['/Users/me']);
  assert.equal(isServerRoots(roots), true);
  assert.equal(canSelectFolder(roots), false, 'the roots are shortcuts, not a folder to confirm');
  assert.equal(pathLabel(roots), SERVER_ROOTS_LABEL);
  assert.equal(folderCountLabel(roots), '1 carpeta');
});

test('a directory reply carries the absolute server path, its parent and its folders', () => {
  const listing = normalizeListing(
    { path: '/srv/repo', parent: '/srv', entries: [{ name: 'internal', path: '/srv/repo/internal' }] },
    '/srv/repo',
  );
  assert.deepEqual(listing, { path: '/srv/repo', parent: '/srv', entries: [{ name: 'internal', path: '/srv/repo/internal' }] });
  assert.equal(isServerRoots(listing), false);
  assert.equal(canSelectFolder(listing), true);
  assert.equal(pathLabel(listing), '/srv/repo');
  assert.equal(folderCountLabel(listing), '1 carpeta');
});

test('malformed entries are dropped, not rendered', () => {
  assert.deepEqual(normalizeEntries('nope'), []);
  assert.deepEqual(
    normalizeEntries([null, 'x', { name: 'sin ruta' }, { path: '  ' }, { name: '  ', path: '/srv/a' }, { name: 'b', path: '/srv/b' }]),
    [{ name: '/srv/a', path: '/srv/a' }, { name: 'b', path: '/srv/b' }],
    'an entry without a name falls back to its path; entries without a path are unusable',
  );
});

test('a folder the server could not read shows its error and cannot be confirmed', () => {
  const denied = normalizeListing(
    { path: '/srv/private', parent: null, entries: [], error: 'open /srv/private: permission denied' },
    '/srv/private',
  );
  assert.equal(denied.error, 'open /srv/private: permission denied');
  assert.equal(denied.entries.length, 0);
  assert.equal(canSelectFolder(denied), false);
  assert.equal(pathLabel(denied), '/srv/private', 'the user still sees where the failure happened');
});

test('an unreadable reply degrades to an honest error anchored at the requested path', () => {
  for (const raw of [undefined, null, 'boom', 42]) {
    const listing = normalizeListing(raw, '/srv/repo');
    assert.equal(listing.path, '/srv/repo');
    assert.equal(listing.parent, null);
    assert.deepEqual(listing.entries, []);
    assert.match(String(listing.error), /servidor/i);
    assert.equal(canSelectFolder(listing), false);
  }
});

test('a reply that omits its exact path fails closed', () => {
  const missing = normalizeListing({ entries: [] }, '/srv/repo');
  assert.equal(missing.path, '/srv/repo');
  assert.match(String(missing.error), /diferente/i);
  assert.equal(canSelectFolder(missing), false);
  assert.equal(normalizeListing({ path: '   ' }, null).path, null, 'a blank path is the roots, not a folder named ""');
});

test('a reply may not redirect a folder click to a different path', () => {
  const listing = normalizeListing({ path: '/srv/wrong', entries: [] }, '/srv/requested');
  assert.equal(listing.path, '/srv/requested');
  assert.match(String(listing.error), /diferente/i);
  assert.equal(canSelectFolder(listing), false);
});

test('the second half of a double click cannot open a replacement row', () => {
  const first = { x: 640, y: 300, at: 1000 };
  assert.equal(isFolderClickThrough(first, { x: 641, y: 298, at: 1210 }), true);
  assert.equal(isFolderClickThrough(first, { x: 641, y: 340, at: 1210 }), false, 'a different row remains responsive');
  assert.equal(isFolderClickThrough(first, { x: 640, y: 300, at: 1600 }), false, 'a later intentional click remains responsive');
  assert.equal(isFolderClickThrough(null, first), false);
});

test('transport failures become a listing, so the dialog can still navigate away', () => {
  assert.equal(errorListing('/srv/repo', new Error('bridge closed')).error, 'bridge closed');
  assert.equal(errorListing('/srv/repo', 'socket hangup').error, 'socket hangup');
  assert.equal(errorListing('/srv/repo', { message: 'reply timed out' }).error, 'reply timed out');
  assert.match(String(errorListing('/srv/repo', new Error('  ')).error), /desconocido/i);
  assert.match(String(errorListing(null, undefined).error), /desconocido/i);
  assert.equal(errorListing('/srv/repo', 'boom').path, '/srv/repo');
});

test('an empty folder is still a folder the user can confirm', () => {
  const empty: DirListing = normalizeListing({ path: '/srv/new', parent: '/srv', entries: [] }, '/srv/new');
  assert.equal(folderCountLabel(empty), 'Sin subcarpetas');
  assert.equal(canSelectFolder(empty), true);
});

test('nothing is selectable before the first listing lands', () => {
  assert.equal(canSelectFolder(null), false);
  assert.equal(isServerRoots(null), false);
  assert.equal(pathLabel(null), SERVER_ROOTS_LABEL);
  assert.equal(folderCountLabel(null), 'Sin subcarpetas');
});

test('new folder names are one direct child, never a path', () => {
  assert.equal(folderNameError('Project Alpha'), null);
  assert.equal(folderNameError('  Project Alpha  '), null);
  assert.match(String(folderNameError('  ')), /nombre/i);
  for (const unsafe of ['.', '..', 'nested/child', `nested\\child`, 'nul\0byte']) {
    assert.match(String(folderNameError(unsafe)), /separadores/i, unsafe);
  }
});

test('a create reply must confirm the exact parent and trimmed name before navigation', () => {
  assert.deepEqual(
    normalizeCreatedDirectory(
      { parent: '/Users/me/Projects', name: 'New App', path: '/Users/me/Projects/New App' },
      '/Users/me/Projects',
      '  New App  ',
    ),
    { parent: '/Users/me/Projects', name: 'New App', path: '/Users/me/Projects/New App' },
  );

  for (const raw of [
    null,
    { parent: '/Users/me/Elsewhere', name: 'New App', path: '/Users/me/Elsewhere/New App' },
    { parent: '/Users/me/Projects', name: 'Other', path: '/Users/me/Projects/Other' },
    { parent: '/Users/me/Projects', name: 'New App' },
  ]) {
    const result = normalizeCreatedDirectory(raw, '/Users/me/Projects', 'New App');
    assert.equal(result.path, null);
    assert.ok(result.error);
  }
});

test('a create failure stays anchored to the requested parent', () => {
  const result = normalizeCreatedDirectory(
    { parent: '/wrong', name: 'New App', path: null, error: 'permission denied' },
    '/Users/me/Projects',
    'New App',
  );
  assert.deepEqual(result, {
    parent: '/Users/me/Projects', name: 'New App', path: null, error: 'permission denied',
  });
});
