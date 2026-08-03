import { test } from 'node:test';
import assert from 'node:assert/strict';
import { homeOf, displayDetail, splitTail } from '../src/tool-display.ts';

const CWD = '/Users/dev/Workspace/workass';

test('homeOf infers macOS and Linux homes, null otherwise', () => {
  assert.equal(homeOf(CWD), '/Users/dev');
  assert.equal(homeOf('/home/deb/src'), '/home/deb');
  assert.equal(homeOf('/opt/ci/build'), null);
  assert.equal(homeOf(null), null);
});

test('paths inside the workspace become relative', () => {
  assert.equal(displayDetail(`${CWD}/desktop/renderer2/src/styles/app.css`, CWD), 'desktop/renderer2/src/styles/app.css');
});

test('paths under home (outside the workspace) get ~', () => {
  assert.equal(
    displayDetail('/Users/dev/Library/Application Support/Workass/state/config.json', CWD),
    '~/Library/Application Support/Workass/state/config.json',
  );
});

test('foreign paths stay absolute', () => {
  assert.equal(displayDetail('/tmp/shot.png', CWD), '/tmp/shot.png');
});

test('commands shorten every embedded occurrence', () => {
  assert.equal(
    displayDetail(`wc -l ${CWD}/desktop/app.css /Users/dev/notes.md`, CWD),
    'wc -l desktop/app.css ~/notes.md',
  );
  assert.equal(displayDetail(`git -C ${CWD} log`, CWD), 'git -C . log');
});

test('no cwd → text unchanged', () => {
  assert.equal(displayDetail('/Users/dev/x.txt', null), '/Users/dev/x.txt');
});

test('splitTail: lone path splits keeping the filename whole', () => {
  assert.deepEqual(splitTail('desktop/renderer2/src/app.css'), { head: 'desktop/renderer2/src/', tail: 'app.css' });
  assert.deepEqual(splitTail('~/Library/x/config.json'), { head: '~/Library/x/', tail: 'config.json' });
});

test('splitTail: commands, bare names and dir-ish strings do not split', () => {
  assert.equal(splitTail('wc -l desktop/app.css'), null); // command (has spaces)
  assert.equal(splitTail('app.css'), null);               // no slash
  assert.equal(splitTail('desktop/'), null);              // trailing slash
  assert.equal(splitTail('/etc'), null);                  // root-only head
});
