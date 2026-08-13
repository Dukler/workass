'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { pathToFileURL } = require('node:url');
const test = require('node:test');

const repoRoot = path.resolve(__dirname, '..', '..');
const script = path.join(repoRoot, 'desktop', 'scripts', 'stamp-windows-icon.mjs');
const generator = path.join(repoRoot, 'desktop', 'scripts', 'make-icon.mjs');
const icon = path.join(repoRoot, 'desktop', 'assets', 'icon.ico');
const electron = path.join(repoRoot, '.dev', 'runtime', 'electron', 'win32-x64', 'electron.exe');

test('tracked Windows artwork is generated from the canonical Workass icon', () => {
  const verified = spawnSync(process.execPath, [generator, '--verify'], { encoding: 'utf8' });
  assert.equal(verified.status, 0, verified.stderr);
  assert.match(verified.stdout, /WORKASS_WINDOWS_ICON_ARTWORK_VERIFIED/);
});

test('the tracked Workass ICO has taskbar sizes and a compact full-resolution image', async () => {
  const { parseIco } = await import(pathToFileURL(script).href);
  const images = parseIco(fs.readFileSync(icon));
  assert.deepEqual(images.map((image) => image.width), [16, 24, 32, 48, 64, 128, 256]);
  assert.ok(images.find((image) => image.width === 256).data.length < 18_963);
});

test('the pinned Windows Electron executable is stamped and independently re-verified', {
  skip: !fs.existsSync(electron) && 'the pinned Windows Electron runtime is not staged on this build host',
}, (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-windows-icon-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const executable = path.join(root, 'Workass.exe');
  fs.copyFileSync(electron, executable);
  const before = crypto.createHash('sha256').update(fs.readFileSync(executable)).digest('hex');

  const stamped = spawnSync(process.execPath, [script, '--exe', executable, '--icon', icon], { encoding: 'utf8' });
  assert.equal(stamped.status, 0, stamped.stderr);
  assert.match(stamped.stdout, /WORKASS_WINDOWS_ICON_STAMPED sizes=16,32,48,256/);
  const verified = spawnSync(process.execPath, [script, '--verify', '--exe', executable, '--icon', icon], { encoding: 'utf8' });
  assert.equal(verified.status, 0, verified.stderr);
  assert.match(verified.stdout, /WORKASS_WINDOWS_ICON_VERIFIED sizes=16,32,48,256/);

  const after = crypto.createHash('sha256').update(fs.readFileSync(executable)).digest('hex');
  assert.notEqual(after, before, 'stamping must replace Electron resource bytes');
});
