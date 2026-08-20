import assert from 'node:assert/strict';
import test from 'node:test';
import { splitPath, basename, labelChangedFiles, envView } from '../src/env-files.ts';
import type { ChatEnvPayload, ChatEnvRepo } from '../src/wire/types.ts';

const f = (path: string, adds = 1, dels = 0) => ({ path, adds, dels });
const parents = (files: ReturnType<typeof f>[]) => labelChangedFiles(files).map((r) => r.parent);
const names = (files: ReturnType<typeof f>[]) => labelChangedFiles(files).map((r) => r.name);

const repo = (name: string, files: ReturnType<typeof f>[], extra: Partial<ChatEnvRepo> = {}): ChatEnvRepo => ({
  name, branch: 'main', files, adds: 0, dels: 0, filesTruncated: false, ...extra,
});
const payload = (repos: ChatEnvRepo[], extra: Partial<ChatEnvPayload> = {}): ChatEnvPayload => ({
  chatId: 'c', tabId: 't', cwd: '/w', repos, unchanged: [], reposTruncated: false, filesTruncated: false,
  repoLimit: 12, fileLimit: 200, approximation: 'x', ...extra,
});

test('label leads with the basename and shows no parent when the name is unique', () => {
  const files = [f('desktop/renderer2/src/store/store.ts'), f('cmd/workass/main.go'), f('a/b/c/d/unique.ts')];
  assert.deepEqual(names(files), ['store.ts', 'main.go', 'unique.ts']);
  // A unique basename gets NO parent context however deep it sits — the full
  // path stays available in the row tooltip, never as the primary label.
  assert.deepEqual(parents(files), ['', '', '']);
});

test('same-name files get the shortest distinguishing parent tail', () => {
  // Distinct top-level parents → one segment each, no ellipsis (it IS the parent).
  assert.deepEqual(parents([f('store/types.ts'), f('wire/types.ts')]), ['store', 'wire']);
  // Share the immediate parent, differ one level up → tail grows to stay unique.
  assert.deepEqual(parents([f('aaa/x/foo.ts'), f('bbb/x/foo.ts')]), ['aaa/x', 'bbb/x']);
  // Differ at the leaf directory, share the prefix → minimal tail + "…/" marker.
  assert.deepEqual(parents([f('src/a/b/foo.ts'), f('src/a/c/foo.ts')]), ['…/b', '…/c']);
});

test('disambiguation handles root files and suffix-of-a-longer-path collisions', () => {
  // Root-level file has no parent to show; the nested sibling shows enough.
  assert.deepEqual(parents([f('foo.ts'), f('a/foo.ts')]), ['', 'a']);
  // "x/foo" is a suffix of "a/x/foo": the short one shows its full parent, the
  // long one is forced one segment deeper, so they still read differently.
  assert.deepEqual(parents([f('x/foo.ts'), f('a/x/foo.ts')]), ['x', 'a/x']);
  // Three-way including a root file.
  assert.deepEqual(parents([f('foo.ts'), f('a/x/foo.ts'), f('b/x/foo.ts')]), ['', 'a/x', 'b/x']);
});

test('separators and odd paths are normalized cross-platform', () => {
  assert.deepEqual(splitPath('a//b\\c'), ['a', 'b', 'c']);
  assert.deepEqual(splitPath('./x/y/'), ['x', 'y']);
  assert.deepEqual(splitPath('  '), []);
  assert.deepEqual(splitPath(''), []);
  assert.equal(basename('a\\b\\c.ts'), 'c.ts');
  assert.equal(basename('c.ts'), 'c.ts');
  assert.equal(basename('a/b/'), 'b');
  // A Windows-style path and a POSIX one collide on the same basename and still
  // disambiguate correctly across the mixed separators.
  assert.deepEqual(parents([f('a\\b\\types.ts'), f('a/c/types.ts')]), ['…/b', '…/c']);
  // Leading "./" is dropped before disambiguation.
  assert.deepEqual(parents([f('./a/foo.ts'), f('b/foo.ts')]), ['a', 'b']);
});

test('a blank path never yields a blank row', () => {
  const rows = labelChangedFiles([f('')]);
  assert.equal(rows[0].name, '—');
  assert.equal(rows[0].parent, '');
});

test('envView drops empty repos, sums the shown diffstat, and disambiguates within a repo', () => {
  const view = envView(payload([
    repo('workass', [f('a/x.ts', 3, 1), f('b/x.ts', 2, 0), f('main.go', 4, 4)]),
    repo('idle', []),
  ], { unchanged: ['idle'] }));
  assert.equal(view.groups.length, 1, 'the repo with no changed files is not a group');
  assert.equal(view.groups[0].name, 'workass');
  assert.equal(view.fileCount, 3);
  assert.equal(view.adds, 9);
  assert.equal(view.dels, 5);
  assert.equal(view.hasChanges, true);
  assert.deepEqual(view.groups[0].rows.map((r) => r.name), ['x.ts', 'x.ts', 'main.go']);
  // Same-name files inside one repo are disambiguated by their repo-relative tail.
  assert.deepEqual(view.groups[0].rows.map((r) => r.parent), ['a', 'b', '']);
});

test('envView surfaces truncation and treats a missing payload as no changes', () => {
  const empty = envView(undefined);
  assert.equal(empty.hasChanges, false);
  assert.deepEqual(empty.groups, []);
  assert.equal(empty.fileCount, 0);
  assert.deepEqual(empty.unchanged, []);

  const truncated = envView(payload([repo('r', [f('a.ts')], { filesTruncated: true })], { reposTruncated: true, filesTruncated: true }));
  assert.equal(truncated.reposTruncated, true);
  assert.equal(truncated.filesTruncated, true);
  assert.equal(truncated.groups[0].filesTruncated, true);
});
