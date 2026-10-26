'use strict';

// Code editors (Monaco, CodeMirror) keep their text in objects of the page's
// own JavaScript world. Workass's scripts run in a separate world (the fake
// page models that: page globals are invisible there), so every editor
// operation goes through one page-world call on the element the agent world
// found. These tests give the page real editor globals and check that typing
// verification, read-only detection, select-all, delete and the snapshot's
// editor text all still use them.

const assert = require('node:assert/strict');
const test = require('node:test');
const { fakeBrowser } = require('./browser-fake-page.test-helper');

function monacoPage({ readOnly = false, value = 'const answer = 41;' } = {}) {
  const fake = fakeBrowser({ url: 'https://code.example/' });
  const root = fake.loadRoot('https://code.example/', ({ h, body }) => {
    body.append(h('div', { class: 'monaco-editor', id: 'ed' },
      h('div', { class: 'view-lines' }, h('div', { class: 'view-line' }, 'const answer')),
      h('textarea', { class: 'inputarea', 'aria-label': 'Editor input' })));
  });
  const element = root.dom.document.querySelector('.monaco-editor');
  const input = root.dom.document.querySelector('textarea');
  root.dom.document.activeElement = input;
  const state = { value, selection: null, focused: false, edits: 0 };
  const editor = {
    getDomNode: () => element,
    getModel: () => ({ getValue: () => state.value, getValueLength: () => state.value.length, getFullModelRange: () => ({ all: true }) }),
    getRawOptions: () => ({ readOnly }),
    focus: () => { state.focused = true; },
    setSelection: (range) => { state.selection = range; },
    getSelection: () => ({ isEmpty: () => !state.selection }),
    executeEdits: () => { state.edits += 1; state.value = ''; },
  };
  // The page's own global, in its own world only.
  root.context.monaco = { editor: { getEditors: () => [editor] } };
  return { fake, root, state };
}

test('the agent world cannot see the page\'s editor objects; the page-world call can', async () => {
  const { fake } = monacoPage();
  await fake.manager.refreshFrameTree(fake.entry);
  assert.equal(await fake.manager.evaluateAgentWorld(fake.entry, 'typeof globalThis.monaco'), 'undefined', 'separate worlds');
  const inspected = await fake.manager.editorPageCall(fake.entry, null, 'document.querySelector(".monaco-editor")', 'inspect');
  assert.deepEqual(inspected, { editor: 'monaco', readOnly: false, value: 'const answer = 41;', length: 18 });
});

test('key handling reads, selects and deletes a Monaco editor\'s text through its model', async () => {
  const { fake, state } = monacoPage();
  await fake.manager.refreshFrameTree(fake.entry);
  assert.deepEqual(await fake.manager.focusedTextLength(fake.entry, null), { found: true, length: 18 });
  assert.deepEqual(await fake.manager.selectAllBrowserTarget(fake.entry, null), { found: true, selectionVerified: true, strategy: 'monaco-model' });
  assert.ok(state.selection && state.focused);
  assert.deepEqual(await fake.manager.deleteSelectedBrowserText(fake.entry, null, 'Backspace'), { found: true, deletionVerified: true, strategy: 'monaco-model' });
  assert.equal(state.edits, 1);
  assert.equal(state.value, '');
});

test('a read-only Monaco editor is detected and the snapshot reads its model', async () => {
  const { fake } = monacoPage({ readOnly: true, value: 'SELECT * FROM orders;' });
  await fake.manager.refreshFrameTree(fake.entry);
  const editors = [{ selector: '#ed', kind: 'monaco', text: 'const answer', readOnly: false }];
  const rows = [{ editor: 'monaco', selector: '#ed', text: 'const answer', readOnly: false }];
  await fake.manager.readEditorModels(fake.entry, null, editors, [rows]);
  assert.equal(editors[0].text, 'SELECT * FROM orders;');
  assert.equal(editors[0].readOnly, true);
  assert.equal(rows[0].text, 'SELECT * FROM orders;');
  assert.equal(rows[0].readOnly, true);
});

test('a CodeMirror instance on its element (an expando of the page world) is used the same way', async () => {
  const fake = fakeBrowser({ url: 'https://code.example/' });
  const root = fake.loadRoot('https://code.example/', ({ h, body }) => {
    body.append(h('div', { class: 'CodeMirror', id: 'cm' }, h('textarea', { 'aria-label': 'Code' })));
  });
  const element = root.dom.document.querySelector('.CodeMirror');
  root.dom.document.activeElement = root.dom.document.querySelector('textarea');
  let value = 'print(1)';
  let selected = false;
  element.CodeMirror = {
    getValue: () => value, getOption: () => false, focus() {},
    execCommand: (name) => { if (name === 'selectAll') selected = true; },
    somethingSelected: () => selected, replaceSelection: (text) => { value = text; selected = false; },
  };
  await fake.manager.refreshFrameTree(fake.entry);
  assert.deepEqual(await fake.manager.focusedTextLength(fake.entry, null), { found: true, length: 8 });
  assert.deepEqual(await fake.manager.selectAllBrowserTarget(fake.entry, null), { found: true, selectionVerified: true, strategy: 'codemirror-command' });
  assert.deepEqual(await fake.manager.deleteSelectedBrowserText(fake.entry, null, 'Backspace'), { found: true, deletionVerified: true, strategy: 'codemirror-selection' });
  assert.equal(value, '');
});
