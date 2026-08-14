import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const sourceRoot = path.resolve(here, '..', 'src');

test('Windows relies on native caption controls instead of renderer IPC buttons', () => {
  const app = fs.readFileSync(path.join(sourceRoot, 'components', 'App.tsx'), 'utf8');
  const main = fs.readFileSync(path.join(sourceRoot, 'main.tsx'), 'utf8');
  const css = fs.readFileSync(path.join(sourceRoot, 'styles', 'app.css'), 'utf8');

  assert.equal(fs.existsSync(path.join(sourceRoot, 'components', 'WindowControls.tsx')), false);
  assert.doesNotMatch(app, /WindowControls/);
  assert.doesNotMatch(css, /\.window-controls\b|\.window-control\b/);
  assert.match(main, /window\.workassWindow\?\.platform === 'win32'/);
  assert.match(css, /:root\.electron-windows \.tl \{ display: none; \}/);
  assert.match(css, /:root\.electron-windows #root \{ --side-toggle-x: 9px; \}/);
  assert.match(css, /:root\.electron:not\(\.electron-windows\) \.tbar \{ -webkit-app-region: drag; \}/);
  assert.match(css, /:root\.electron \.titlebar \{ -webkit-app-region: no-drag; \}/);
});

test('the sidebar toggle is a topmost no-drag titlebar island', () => {
  const app = fs.readFileSync(path.join(sourceRoot, 'components', 'App.tsx'), 'utf8');
  const css = fs.readFileSync(path.join(sourceRoot, 'styles', 'app.css'), 'utf8');

  assert.match(app, /<div className="side-toggle-slot">[\s\S]*className="tico side-toggle"/);
  assert.ok(app.indexOf('<SidebarToggle expanded=') > app.indexOf('<ImageClipboardController />'));
  assert.doesNotMatch(app, /event\.detail/);
  assert.doesNotMatch(app, /onClick=/);
  assert.match(app, /onPointerUp=[\s\S]*event\.button === 0[\s\S]*store\.toggleSide\(\);/);
  assert.match(app, /event\.key !== 'Enter' && event\.key !== ' '/);
  assert.match(css, /\.side-toggle-slot \{[\s\S]*position: fixed;[\s\S]*height: 40px;[\s\S]*-webkit-app-region: no-drag;/);
});
