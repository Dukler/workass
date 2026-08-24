import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const composer = readFileSync(new URL('../src/components/Composer.tsx', import.meta.url), 'utf8');
const styles = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');

test('composer selectors use no chevrons and share one centered control row', () => {
  assert.doesNotMatch(composer, /<span className="eff">▾<\/span>/);
  assert.match(composer, /className="selectorcluster"/);
  assert.match(styles, /\.selectorcluster\s*\{[^}]*display:\s*flex;[^}]*align-items:\s*center;[^}]*height:\s*20px;/s);
  assert.match(styles, /\.selectorcluster \.ctxring-wrap\s*\{[^}]*display:\s*flex;[^}]*align-items:\s*center;[^}]*height:\s*20px;[^}]*line-height:\s*0;/s);
  assert.match(styles, /\.modelsel,\s*\.effortsel,\s*\.ctxring\s*\{[^}]*height:\s*20px;[^}]*align-items:\s*center;/s);
});

test('effort gauge is a full-size peer of the context ring', () => {
  assert.match(styles, /\.effortsel \.egauge\s*\{[^}]*width:\s*17px;[^}]*height:\s*17px;/s);
  assert.match(styles, /\.effortsel \.egauge svg\s*\{[^}]*width:\s*17px;[^}]*height:\s*17px;/s);
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
