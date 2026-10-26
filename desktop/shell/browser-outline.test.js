'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { buildOutline, diffOutlines, pageLines, renderLine } = require('./browser-outline');
const { checkUploadPaths, eventsSince, pushEvent } = require('./browser-actions');

// AX nodes in the shape CDP Accessibility.getFullAXTree returns.
let seq = 0;
function ax(role, { name, value, props = {}, children = [], backend, ignored = false } = {}) {
  const node = {
    nodeId: String(++seq), ignored, role: { value: role }, childIds: children.map((child) => child.nodeId),
    ...(name != null ? { name: { value: name } } : {}), ...(value != null ? { value: { value } } : {}),
    properties: Object.entries(props).map(([key, v]) => ({ name: key, value: { value: v } })),
    backendDOMNodeId: backend ?? seq + 1000,
  };
  for (const child of children) child.parentId = node.nodeId;
  return node;
}

test('the outline nests roles and names, puts refs on what an action can target, and keeps field facts', () => {
  const label = ax('StaticText', { name: 'Email' });
  const field = ax('textbox', { name: 'Email', value: 'ada@example.test', props: { required: true, focusable: true }, backend: 11 });
  const secret = ax('textbox', { name: 'Password', value: '•••••••', props: { focusable: true }, backend: 12 });
  const link = ax('link', { name: 'Home', props: { url: 'https://example.test/', focusable: true }, backend: 13 });
  const linkText = ax('StaticText', { name: 'Home' });
  link.childIds = [linkText.nodeId]; linkText.parentId = link.nodeId;
  const canvas = ax('Canvas', { backend: 14 });
  const frame = ax('Iframe', { name: 'Card', props: { url: 'https://pay.example/card' }, backend: 15 });
  const root = ax('RootWebArea', { name: 'Checkout', children: [label, field, secret, link, canvas, frame] });
  const card = ax('textbox', { name: 'Card number', value: '4242', backend: 21 });
  const childRoot = ax('RootWebArea', { name: 'Card', children: [card] });
  const refs = new Map();
  const refFor = (frameKey, backend) => { const key = `${frameKey}|${backend}`; if (!refs.has(key)) refs.set(key, `e${refs.size + 1}`); return refs.get(key); };
  const { lines } = buildOutline([
    { frameKey: 'top:1', url: 'https://shop.example/checkout', nodes: [root, label, field, secret, link, linkText, canvas, frame] },
    { frameKey: 'card:1', url: 'https://pay.example/card', nodes: [childRoot, card], ownedBy: { frameKey: 'top:1', backendNodeId: 15 } },
  ], { refFor, title: 'Checkout', url: 'https://shop.example/checkout' });
  const text = lines.map(renderLine).join('\n');
  assert.equal(text, [
    'page "Checkout" -> https://shop.example/checkout',
    '- textbox "Email" [e1] value="ada@example.test" required',
    '- textbox "Password" [e2] value=[redacted]',
    '- link "Home" [e3] -> https://example.test/',
    '- canvas [e4] (pixels only: text drawn on it cannot be read as text)',
    '- iframe "Card" [e5] -> https://pay.example/card',
    '  - textbox "Card number" [e6] value="4242"',
  ].join('\n'));
  assert.doesNotMatch(text, /•/u, 'a password value never appears, not even as bullets');
});

test('an unreadable frame says so in place instead of disappearing', () => {
  const frame = ax('Iframe', { name: 'Ad', backend: 31 });
  const root = ax('RootWebArea', { children: [frame] });
  const { lines } = buildOutline([
    { frameKey: 'top:1', nodes: [root, frame] },
    { frameKey: 'ad:1', unavailable: 'the frame did not answer in time', ownedBy: { frameKey: 'top:1', backendNodeId: 31 } },
  ], { refFor: () => null, title: 'T', url: 'https://x.test/' });
  assert.match(lines.map(renderLine).join('\n'), /- iframe "Ad"\n {2}- \(frame not readable here: the frame did not answer in time\)/u);
});

test('outline pages split on whole lines and say where to continue', () => {
  const words = ['alpha', 'bravo', 'charlie', 'delta', 'echo', 'foxtrot', 'golf', 'hotel', 'india', 'juliet'];
  const lines = Array.from({ length: 50 }, (_v, i) => ({ depth: 0, key: `k${i}`, ref: null, text: `- text: ${words[i % 10]} ${words[(i * 3) % 10]} ${'x'.repeat(40)}` }));
  const first = pageLines(lines, 0, 600);
  assert.ok(first.next > 0 && first.next < 50);
  assert.ok(Buffer.byteLength(first.text) <= 600);
  const second = pageLines(lines, first.next, 600);
  assert.equal(second.text.split('\n')[0], renderLine(lines[first.next]));
});

// Expands {a..b} / {a..b..s} ranges back into lines; ranges in one line
// advance together.
function expandAll(text) {
  return text.split('\n').flatMap((line) => {
    const ranges = [...line.matchAll(/\{(\d+)\.\.(\d+)(?:\.\.(\d+))?\}/gu)];
    if (!ranges.length) return [line];
    const count = (Number(ranges[0][2]) - Number(ranges[0][1])) / Number(ranges[0][3] || 1) + 1;
    return Array.from({ length: count }, (_v, k) => {
      let i = 0;
      return line.replace(/\{(\d+)\.\.(\d+)(?:\.\.(\d+))?\}/gu, (whole, from, to, step = '1') => {
        i += 1;
        const n = Number(from) + k * Number(step);
        const width = from.length === to.length && from.startsWith('0') ? from.length : 0;
        return width ? String(n).padStart(width, '0') : String(n);
      });
    });
  });
}

test('a run of siblings that differ only in counting numbers folds into one line and expands back exactly', () => {
  const items = Array.from({ length: 2000 }, (_v, i) => ({ depth: 1, key: `i${i}`, ref: `e${i + 2}`,
    text: `- link "Item ${i + 1}" [e${i + 2}] -> http://127.0.0.1:28975/heavy/item/${i + 1}` }));
  const lines = [{ depth: 0, key: 'list', ref: null, text: '- list' }, ...items, { depth: 0, key: 'end', ref: null, text: '- text: End of list: FX-END' }];
  const page = pageLines(lines, 0, 20000);
  assert.equal(page.next, null, 'the whole list fits one page');
  assert.deepEqual(page.text.split('\n'), ['- list', '  - link "Item {1..2000}" [e{2..2001}] -> http://127.0.0.1:28975/heavy/item/{1..2000}', '- text: End of list: FX-END']);
  assert.deepEqual(expandAll(page.text), lines.map(renderLine), 'nothing lost');
  // zero padding keeps its width; steps other than one say so
  const padded = Array.from({ length: 12 }, (_v, i) => ({ depth: 0, key: `p${i}`, ref: null, text: `- option "${String(i + 1).padStart(2, '0')}" [e${10 + i * 2}]` }));
  assert.equal(pageLines(padded, 0, 5000).text, '- option "{01..12}" [e{10..32..2}]');
  assert.deepEqual(expandAll(pageLines(padded, 0, 5000).text), padded.map(renderLine));
});

test('lines that do not count in step, too few lines, and lines with children stay as they are', () => {
  const three = [1, 2, 3].map((n) => ({ depth: 0, key: `t${n}`, ref: null, text: `- button "Day ${n}" [e${n}]` }));
  assert.equal(pageLines(three, 0, 5000).text, three.map(renderLine).join('\n'));
  const uneven = [1, 2, 4, 5, 6].map((n) => ({ depth: 0, key: `u${n}`, ref: null, text: `- button "Day ${n}" [e${n}]` }));
  assert.equal(pageLines(uneven, 0, 5000).text.split('\n')[0], '- button "Day 1" [e1]');
  const parents = [1, 2, 3, 4, 5].flatMap((n) => [{ depth: 0, key: `g${n}`, ref: null, text: `- group "Row ${n}"` }, { depth: 1, key: `c${n}`, ref: null, text: '- text: cell' }]);
  assert.equal(pageLines(parents, 0, 5000).text, parents.map(renderLine).join('\n'));
  const braces = [1, 2, 3, 4, 5].map((n) => ({ depth: 0, key: `b${n}`, ref: null, text: `- text: {x} ${n}` }));
  assert.equal(pageLines(braces, 0, 5000).text, braces.map(renderLine).join('\n'));
});

test('a plain list item around one link is the link', () => {
  const links = [1, 2].map((n) => ax('link', { name: `Item ${n}`, props: { url: `https://example.test/${n}`, focusable: true }, backend: 100 + n }));
  const items = links.map((link) => ax('listitem', { children: [link] }));
  const linkA = ax('link', { name: 'A', props: { focusable: true }, backend: 201 });
  const more = ax('StaticText', { name: 'and more' });
  const both = ax('listitem', { children: [linkA, more] });
  const list = ax('list', { children: [...items, both] });
  const root = ax('RootWebArea', { name: 'List', children: [list] });
  const { lines } = buildOutline([{ frameKey: 'top:1', url: 'https://example.test/', nodes: [root, list, ...items, ...links, both, linkA, more] }],
    { refFor: (_f, backend) => `e${backend}` });
  const text = lines.map(renderLine).join('\n');
  assert.match(text, /^- list\n {2}- link "Item 1" \[e101\] -> https:\/\/example\.test\/1$/mu, 'no listitem line around a lone link');
  assert.match(text, /- listitem\n {4}- link "A" \[e201\]\n {4}- text: and more/u, 'an item with more than one thing keeps its line');
});

test('the diff of two outlines lists what was added, removed and changed', () => {
  const before = [
    { depth: 0, key: 'page', text: 'page "A" -> x' },
    { depth: 0, key: 'f|1', text: '- button "Show details" [e1] collapsed' },
    { depth: 0, key: 'f|2', text: '- text: (no dialog yet)' },
  ];
  const after = [
    { depth: 0, key: 'page', text: 'page "A" -> x' },
    { depth: 0, key: 'f|1', text: '- button "Show details" [e1] expanded' },
    { depth: 0, key: 'f|3', text: '- text: Revealed: secret plan' },
  ];
  const diff = diffOutlines(before, after);
  assert.deepEqual([diff.added, diff.removed, diff.changed], [1, 1, 1]);
  assert.equal(diff.text, [
    '~ - button "Show details" [e1] expanded  (was: button "Show details" [e1] collapsed)',
    '+ - text: Revealed: secret plan',
    '- - text: (no dialog yet)',
  ].join('\n'));
  assert.deepEqual(diffOutlines(before, before), { added: 0, removed: 0, changed: 0, text: '', omitted: 0, total: 3 });
});

test('uploads take only plain files the agent names, never credentials, links or too much', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wk-upload-'));
  const ok = path.join(dir, 'report.pdf');
  fs.writeFileSync(ok, 'pdf');
  assert.deepEqual(checkUploadPaths([ok]).map((item) => item.size), [3]);
  for (const name of ['id_rsa', 'server.pem', 'api-token.txt', '.env', '.env.local', 'login.keychain-db', 'secrets.json']) {
    const file = path.join(dir, name);
    fs.writeFileSync(file, 'x');
    assert.throws(() => checkUploadPaths([file]), /credential/u, name);
  }
  fs.mkdirSync(path.join(dir, '.ssh'));
  fs.writeFileSync(path.join(dir, '.ssh', 'notes.txt'), 'x');
  assert.throws(() => checkUploadPaths([path.join(dir, '.ssh', 'notes.txt')]), /credentials folder/u);
  fs.symlinkSync(ok, path.join(dir, 'link.pdf'));
  assert.throws(() => checkUploadPaths([path.join(dir, 'link.pdf')]), /symbolic link/u);
  fs.linkSync(ok, path.join(dir, 'hard.pdf'));
  assert.throws(() => checkUploadPaths([path.join(dir, 'hard.pdf')]), /hard links/u);
  const big = path.join(dir, 'big.bin');
  fs.writeFileSync(big, Buffer.alloc(10 * 1024 * 1024 + 1));
  assert.throws(() => checkUploadPaths([big]), /more than 10 MB/u);
  assert.throws(() => checkUploadPaths(['relative.txt']), /absolute/u);
  assert.throws(() => checkUploadPaths([]), /1 to 10/u);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('an upload path that goes through a linked folder, or into a known credentials folder, is refused', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wk-upload-'));
  fs.mkdirSync(path.join(dir, '.kube'));
  fs.writeFileSync(path.join(dir, '.kube', 'config'), 'kube');
  fs.symlinkSync(path.join(dir, '.kube'), path.join(dir, 'innocent'));
  assert.throws(() => checkUploadPaths([path.join(dir, 'innocent', 'config')]), /link/u, 'a linked folder');
  fs.mkdirSync(path.join(dir, 'docs'));
  fs.writeFileSync(path.join(dir, 'docs', 'plain.txt'), 'plain');
  fs.symlinkSync(path.join(dir, 'docs'), path.join(dir, 'docs-link'));
  assert.throws(() => checkUploadPaths([path.join(dir, 'docs-link', 'plain.txt')]), /link/u, 'any linked folder, not only credential ones');
  for (const parts of [['.config', 'gh', 'hosts.yml'], ['.claude', '.credentials.json'], ['.docker', 'config.json'], ['.config', 'op', 'config']]) {
    fs.mkdirSync(path.join(dir, ...parts.slice(0, -1)), { recursive: true });
    fs.writeFileSync(path.join(dir, ...parts), 'x');
    assert.throws(() => checkUploadPaths([path.join(dir, ...parts)]), /credential/u, parts.join('/'));
  }
  // The system's own aliases (/tmp, /var on macOS) are not links the agent chose.
  assert.equal(checkUploadPaths([path.join(dir, 'docs', 'plain.txt')]).length, 1);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('a secret field never shows its value in the outline or its diffs, by type, autocomplete, name or id', () => {
  const { outlineSecretField } = require('./browser-outline');
  const fields = [
    { backend: 41, name: 'One-time code', value: '482913', attributes: ['type', 'text', 'autocomplete', 'one-time-code'] },
    { backend: 42, name: 'Card number', value: '4111 1111 1111 1111', attributes: ['type', 'text', 'autocomplete', 'cc-number'] },
    { backend: 43, name: 'Security code', value: '737', attributes: ['type', 'text', 'name', 'cvv'] },
    { backend: 44, name: 'Password', value: 'hunter2-shown', attributes: ['type', 'text', 'id', 'password'] },
    { backend: 45, name: 'City', value: 'Lima', attributes: ['type', 'text', 'name', 'city'] },
  ];
  for (const field of fields) assert.equal(outlineSecretField(field.attributes), field.backend !== 45, field.name);
  const nodes = fields.map((field) => ax('textbox', { name: field.name, value: field.value, props: { focusable: true, editable: 'plaintext' }, backend: field.backend }));
  const root = ax('RootWebArea', { name: 'Pay', children: nodes });
  const frame = { frameKey: 'top:1', url: 'https://pay.example/', nodes: [root, ...nodes], secretBackendIds: new Set([41, 42, 43, 44]) };
  const { lines } = buildOutline([frame], { refFor: (_f, backend) => `e${backend}` });
  const text = lines.map(renderLine).join('\n');
  for (const secret of ['482913', '4111', '737', 'hunter2']) assert.doesNotMatch(text, new RegExp(secret), secret);
  assert.match(text, /textbox "City" \[e45\] value="Lima"/);
  assert.equal((text.match(/value=\[redacted\]/g) || []).length, 4);
  const changed = buildOutline([{ ...frame, nodes: [root, ...nodes.map((node) => (node.backendDOMNodeId === 45 ? node : { ...node, value: { value: 'NEW-SECRET-9' } }))] }], { refFor: (_f, backend) => `e${backend}` });
  assert.doesNotMatch(diffOutlines(lines, changed.lines).text, /NEW-SECRET-9/, 'a diff shows no secret either');
});

test('events are numbered so an action reports only what happened during it', () => {
  const entry = {};
  pushEvent(entry, { type: 'navigated', url: 'a' });
  const mark = entry.eventSeq;
  pushEvent(entry, { type: 'dialog_opened', dialog: 'alert' });
  assert.deepEqual(eventsSince(entry, mark), [{ type: 'dialog_opened', dialog: 'alert' }]);
});
