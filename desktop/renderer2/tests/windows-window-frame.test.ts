import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const sourceRoot = path.resolve(here, '..', 'src');

test('Windows relies on native caption controls instead of renderer IPC buttons', () => {
  const app = fs.readFileSync(path.join(sourceRoot, 'components', 'App.tsx'), 'utf8');
  const css = fs.readFileSync(path.join(sourceRoot, 'styles', 'app.css'), 'utf8');

  assert.doesNotMatch(app, /WindowControls/);
  assert.doesNotMatch(css, /\.window-controls\b|\.window-control\b/);
  assert.match(css, /:root\.electron-windows \.tl \{ display: none; \}/);
});
