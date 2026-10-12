import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const composer = readFileSync(new URL('../src/components/Composer.tsx', import.meta.url), 'utf8');
const styles = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');

// Approved mock chatbox-v10 (t3code row, 2026-10-12): permission, model and
// effort are quiet menu triggers — icon, label, chevron — on one row.
test('composer selectors are menu triggers with chevrons on one centered row', () => {
  assert.match(composer, /className="selectorcluster"/);
  assert.match(composer, /className="selcv" aria-hidden="true"><IcChevDown \/>/);
  assert.match(styles, /\.selectorcluster\s*\{[^}]*display:\s*flex;[^}]*align-items:\s*center;[^}]*height:\s*26px;/s);
  assert.match(styles, /\.permsel, \.modelsel, \.effortsel\s*\{[^}]*display:\s*inline-flex;[^}]*height:\s*26px;/s);
});

test('the permission control is amber only when the agent may act unasked', () => {
  assert.match(composer, /const riskyMode = modeId === 'bypass' \|\| modeId === 'bypassPermissions';/);
  assert.match(styles, /\.permsel\.risky, \.permsel\.risky \.selic\s*\{[^}]*color:\s*var\(--amber\);/s);
});

test('composer remeasures a wrapped draft when a chat switch changes its rendered width', () => {
  assert.match(composer, /observeComposerTextareaWidth/);
  assert.match(composer, /return observeComposerTextareaWidth\(el, autosize\)/);
});

test('the queue shares the composer reading column', () => {
  assert.match(styles, /\.qlist\s*\{[^}]*width:\s*100%;[^}]*max-width:\s*var\(--read\);[^}]*margin:\s*0 auto 9px;/s);
});

test('draft attachments share the queue and composer reading column', () => {
  assert.match(styles, /\.attrow\s*\{[^}]*width:\s*100%;[^}]*max-width:\s*var\(--read\);[^}]*margin:\s*0 auto 9px;/s);
});
