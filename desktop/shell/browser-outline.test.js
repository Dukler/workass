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
  const lines = Array.from({ length: 50 }, (_v, i) => ({ depth: 0, key: `k${i}`, ref: null, text: `- text: line ${i} ${'x'.repeat(40)}` }));
  const first = pageLines(lines, 0, 600);
  assert.ok(first.next > 0 && first.next < 50);
  assert.ok(Buffer.byteLength(first.text) <= 600);
  const second = pageLines(lines, first.next, 600);
  assert.match(second.text, new RegExp(`^- text: line ${first.next} `, 'u'));
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

test('events are numbered so an action reports only what happened during it', () => {
  const entry = {};
  pushEvent(entry, { type: 'navigated', url: 'a' });
  const mark = entry.eventSeq;
  pushEvent(entry, { type: 'dialog_opened', dialog: 'alert' });
  assert.deepEqual(eventsSince(entry, mark), [{ type: 'dialog_opened', dialog: 'alert' }]);
});
