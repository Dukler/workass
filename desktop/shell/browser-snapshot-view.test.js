'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const vm = require('node:vm');
const { makeDOM } = require('./browser-read-dom.test-helper');
const {
  observationScript, pageAbsoluteURL, redactPageValue, redactSecretShapes,
} = require('./browser-observation');
const {
  attachNodeFacts, browserSnapshotForParams, detachNodeFacts, diffSequences, dropSecretFieldAxRows,
} = require('./browser-snapshot-view');
const { fakeBrowser } = require('./browser-fake-page.test-helper');

function observe(setup, url) {
  const dom = makeDOM(url ? { url } : {});
  setup(dom);
  return JSON.parse(JSON.stringify(vm.runInContext(observationScript({ snapshotId: 'snap-facts' }), vm.createContext(dom.context))));
}

test('snapshot rows carry absolute link URLs, image sources, titles and current field values', () => {
  const result = observe(({ h, body }) => {
    body.append(
      h('a', { href: '../deals?page=2', title: 'Weekly deals' }, 'Deals'),
      h('a', { href: '//cdn.example/file.pdf' }, 'Protocol relative'),
      h('a', { href: 'javascript:void(0)' }, 'Script link'),
      h('a', { href: '/item/9' }, h('img', { src: '/img/9.jpg', alt: 'Red shoe', $width: 120, $height: 120 })),
      h('img', { src: 'data:image/gif;base64,R0lGOD', 'data-src': '/lazy/real.jpg', alt: 'Lazy', $width: 50, $height: 50 }),
      h('input', { name: 'q', $value: 'running shoes' }),
      h('input', { name: 'email', placeholder: 'you@example.com' }),
      h('textarea', { 'aria-label': 'Notes', $value: 'leave at door' }),
      h('select', { 'aria-label': 'Sort' }, h('option', {}, 'Best match'), h('option', { $selected: true }, 'Price: low to high')),
    );
  }, 'https://shop.example/catalog/list');
  const byName = (name) => result.semantic.find((node) => node.name === name);
  // The title names the link; its visible text stays in text.
  assert.equal(byName('Weekly deals').href, 'https://shop.example/deals?page=2');
  assert.equal(byName('Weekly deals').text, 'Deals');
  assert.equal(Object.hasOwn(byName('Weekly deals'), 'title'), false, 'a title equal to the name is not repeated');
  assert.equal(byName('Protocol relative').href, 'https://cdn.example/file.pdf');
  assert.equal(byName('Script link').href, 'javascript:');
  const imageLink = result.semantic.find((node) => node.href === 'https://shop.example/item/9');
  assert.equal(imageLink.alt, 'Red shoe', 'an image-only link is named by its picture');
  assert.equal(byName('Red shoe').src, 'https://shop.example/img/9.jpg');
  assert.equal(byName('Lazy').src, 'https://shop.example/lazy/real.jpg', 'a data: placeholder yields to the lazy source');
  const fields = result.semantic.filter((node) => node.role === 'textbox' || node.role === 'combobox');
  assert.deepEqual(fields.map((node) => node.value || null), ['running shoes', null, 'leave at door', 'Price: low to high']);
  assert.equal(fields[1].placeholder, 'you@example.com');
  assert.deepEqual(result.traversal.errors, []);
});

test('snapshot rows never carry password, one-time-code or secret-named field values', () => {
  const result = observe(({ h, body }) => {
    body.append(
      h('input', { type: 'password', name: 'pw', $value: 'hunter2-PASSWORD' }),
      h('input', { name: 'api_token', $value: 'TOKEN-VALUE-1' }),
      h('input', { name: 'pinCode', $value: '4242' }),
      h('input', { autocomplete: 'one-time-code', name: 'code', $value: '987654' }),
      h('input', { autocomplete: 'cc-number', name: 'card', $value: '4111111111111111' }),
      h('input', { id: 'client-secret', $value: 'SECRET-VALUE-2' }),
      h('input', { name: 'passenger', $value: 'Ada Lovelace' }),
    );
  });
  const serialized = JSON.stringify(result);
  for (const secret of ['hunter2-PASSWORD', 'TOKEN-VALUE-1', '4242', '987654', '4111111111111111', 'SECRET-VALUE-2']) {
    assert.doesNotMatch(serialized, new RegExp(secret));
  }
  const textboxes = result.semantic.filter((node) => node.role === 'textbox');
  assert.deepEqual(textboxes.map((node) => node.valueRedacted === true), [true, true, true, true, true, true, false]);
  assert.equal(textboxes[6].value, 'Ada Lovelace', 'a name that merely starts with "pass" is not a secret');
});

test('page value redaction removes key assignments, bearer values and credential shapes', () => {
  const jwt = 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U';
  const cases = [
    ['https://shop.example/cb?access_token=abc123DEF&x=1', /abc123DEF/],
    ['Authorization: Bearer abc.def.ghi', /abc\.def\.ghi/],
    ['key sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV', /sk-ant-api03/],
    ['ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789', /ghp_/],
    ['AKIAABCDEFGHIJKLMNOP is the id', /AKIAABCDEFGHIJKLMNOP/],
    [`session ${jwt}`, /dozjgNryP4J3/],
    ['https://admin:hunter2@intranet.example/', /hunter2/],
    ['-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----', /MIIEpAIBAAKCAQEA/],
    ['{"password":"opensesame"}', /opensesame/],
  ];
  for (const [input, secret] of cases) {
    const output = redactPageValue(input);
    assert.doesNotMatch(output, secret, input);
    assert.match(output, /\[redacted\]/, input);
  }
  assert.equal(redactSecretShapes('https://admin:hunter2@intranet.example/x'), 'https://[redacted]@intranet.example/x');
  assert.equal(redactPageValue('Price: $19.99, free shipping'), 'Price: $19.99, free shipping');
  assert.equal(pageAbsoluteURL('/a b', 'https://x.example/dir/'), 'https://x.example/a%20b');
  assert.equal(pageAbsoluteURL('data:text/html,<b>x</b>', 'https://x.example/'), 'data:');
});

test('element facts are redacted when detached and only fill room the legacy rows leave', () => {
  const semantic = [
    { element_ref: 'el_a', role: 'link', name: 'A', href: 'https://x.example/?token=SECRET1', title: '' },
    { element_ref: 'el_b', role: 'textbox', name: 'B', value: 'Bearer SECRET2', valueRedacted: false },
    { element_ref: 'el_c', role: 'textbox', name: 'C', valueRedacted: true },
  ];
  const interactive = semantic.map((node) => ({ ...node }));
  const facts = detachNodeFacts(semantic, interactive);
  for (const row of [...semantic, ...interactive]) {
    for (const key of ['href', 'value', 'valueRedacted', 'title']) assert.equal(Object.hasOwn(row, key), false);
  }
  assert.doesNotMatch(JSON.stringify(Array.from(facts.values())), /SECRET1|SECRET2/);
  const result = { semantic, interactive, truncation: { total_bytes: 0 } };
  attachNodeFacts(result, facts, Number.POSITIVE_INFINITY);
  assert.equal(result.semantic[2].valueRedacted, true);
  assert.match(result.interactive[0].href, /token=\[redacted\]/);

  const tight = { semantic: semantic.map((node) => { const copy = { ...node }; for (const key of Object.keys(facts.get(node.element_ref) || {})) delete copy[key]; return copy; }), interactive: [], truncation: { total_bytes: 0 } };
  const before = JSON.stringify(tight.semantic);
  const limit = Buffer.byteLength(JSON.stringify(tight)) + 80;
  attachNodeFacts(tight, facts, limit);
  assert.ok(Buffer.byteLength(JSON.stringify(tight)) <= limit);
  assert.equal(tight.truncation.total_bytes, Buffer.byteLength(JSON.stringify(tight)));
  assert.ok(tight.truncation.element_facts_omitted >= 1);
  const stripped = tight.semantic.map((node) => { const copy = { ...node }; for (const key of ['href', 'value', 'valueRedacted', 'title']) delete copy[key]; return copy; });
  assert.equal(JSON.stringify(stripped), before, 'legacy fields are untouched');
});

test('sequence diff finds a minimal edit script', () => {
  const lcs = (a, b) => {
    const table = Array.from({ length: a.length + 1 }, () => new Array(b.length + 1).fill(0));
    for (let i = a.length - 1; i >= 0; i -= 1) for (let j = b.length - 1; j >= 0; j -= 1) {
      table[i][j] = a[i] === b[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1]);
    }
    return table[0][0];
  };
  let seed = 7;
  const random = () => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed / 2147483648; };
  for (let round = 0; round < 200; round += 1) {
    const a = Array.from({ length: Math.floor(random() * 30) }, () => 'abcde'[Math.floor(random() * 5)]);
    const b = Array.from({ length: Math.floor(random() * 30) }, () => 'abcde'[Math.floor(random() * 5)]);
    const ops = diffSequences(a, b);
    const equal = ops.filter(([op]) => op === '=');
    assert.equal(equal.length, lcs(a, b), JSON.stringify([a, b]));
    for (const [, i, j] of equal) assert.equal(a[i], b[j]);
    assert.equal(ops.filter(([op]) => op !== '+').length, a.length);
    assert.equal(ops.filter(([op]) => op !== '-').length, b.length);
  }
  assert.equal(diffSequences(['a', 'b'], ['c', 'd'], 1), null, 'too many edits fall back');
});

// ---------------------------------------------------------------------------
// Through the real BrowserManager observation methods over a fake page.

function openPage(chatId, build, url = 'https://shop.example/search?q=shoes') {
  const fake = fakeBrowser({ url, chatId });
  fake.loadRoot(url, build);
  const snapshot = (params = {}) => browserSnapshotForParams(fake.manager, fake.entry, params);
  return { ...fake, snapshot, dom: fake.frames.get('root').dom };
}

function listing(count, { prefix = 'Item', extra = () => [] } = {}) {
  return ({ h, body }) => {
    body.append(h('h1', {}, 'Results for shoes'));
    body.append(h('input', { name: 'q', 'aria-label': 'Search', $value: 'shoes' }));
    body.append(h('input', { type: 'password', name: 'password', 'aria-label': 'Password', $value: 'hunter2-SECRET' }));
    const list = h('ul');
    for (let index = 1; index <= count; index += 1) {
      list.append(h('li', {}, h('a', { href: `/itm/${index}?hash=item${index}&var=${'x'.repeat(40)}` }, `${prefix} ${index} running shoe size ${index % 12}`), ` $${index}.99 free shipping`));
    }
    body.append(list, ...extra({ h }));
  };
}

test('json snapshots keep every legacy byte and add element facts only in leftover room', async () => {
  const withFacts = openPage('json-superset', listing(8));
  const snapshot = await withFacts.snapshot();
  const link = snapshot.semantic.find((node) => node.role === 'link' && node.name.startsWith('Item 3 '));
  assert.equal(link.href, 'https://shop.example/itm/3?hash=item3&var=' + 'x'.repeat(40));
  const linkInteractive = snapshot.interactive.find((node) => node.element_ref === link.element_ref);
  assert.equal(linkInteractive.href, link.href);
  const password = snapshot.semantic.find((node) => node.name === 'Password');
  assert.equal(password.valueRedacted, true);
  assert.equal(Object.hasOwn(password, 'value'), false);
  assert.doesNotMatch(JSON.stringify(snapshot), /hunter2-SECRET/);
  assert.equal(snapshot.truncation.element_facts_omitted, 0);
  assert.equal(snapshot.truncation.total_bytes, Buffer.byteLength(JSON.stringify(snapshot)));

  // A heavy page: the legacy fields are budgeted exactly as before.
  const heavy = openPage('json-heavy', listing(400));
  const heavySnapshot = await heavy.snapshot();
  assert.ok(Buffer.byteLength(JSON.stringify(heavySnapshot)) <= 64 * 1024);
  assert.equal(heavySnapshot.truncation.total_bytes, Buffer.byteLength(JSON.stringify(heavySnapshot)));
  const scrub = (value) => JSON.parse(JSON.stringify(value, (key, item) => {
    if (['href', 'src', 'alt', 'title', 'value', 'valueTruncated', 'valueRedacted', 'placeholder', 'element_facts_omitted', 'total_bytes', 'element_ref', 'frame_ref', 'snapshot_id'].includes(key)) return undefined;
    return item;
  }));
  const legacy = openPage('json-legacy', listing(400));
  // The same page with the facts removed from the page script's rows.
  const original = legacy.webContents.executeJavaScript;
  legacy.webContents.executeJavaScript = async (script, gesture) => {
    const value = await original(script, gesture);
    if (value && Array.isArray(value.semantic)) {
      for (const list of [value.semantic, value.interactive]) {
        for (const node of list) for (const key of ['href', 'src', 'alt', 'title', 'value', 'valueTruncated', 'valueRedacted', 'placeholder']) delete node[key];
      }
    }
    return value;
  };
  const legacySnapshot = await legacy.snapshot();
  const comparable = (value) => { const copy = scrub(value); delete copy.truncation; return copy; };
  assert.deepEqual(comparable(heavySnapshot), comparable(legacySnapshot));
  assert.ok(heavySnapshot.truncation.element_facts_omitted >= 0);
});

test('compact snapshots list every link with its URL on a page the json format cannot hold', async () => {
  const page = openPage('compact-listing', listing(400));
  const json = await page.snapshot();
  const compact = await page.snapshot({ format: 'compact' });
  assert.equal(compact.format, 'compact');
  assert.ok(Buffer.byteLength(JSON.stringify(compact)) <= 64 * 1024);
  const lines = compact.elements.split('\n');
  const linkLines = lines.filter((line) => / link "Item \d+/.test(line));
  const jsonLinks = json.semantic.filter((node) => node.role === 'link' && node.href);
  assert.ok(linkLines.length > jsonLinks.length * 2, `compact ${linkLines.length} links vs json ${jsonLinks.length}`);
  assert.match(lines.find((line) => line.includes('"Item 7 running')), /^e\d+ link "Item 7 running shoe size 7" href=https:\/\/shop\.example\/itm\/7\?hash=item7&var=x+$/);
  assert.match(compact.elements, /textbox "Search" value="shoes"/);
  assert.match(compact.elements, /textbox "Password" value=\[redacted\]/);
  assert.doesNotMatch(JSON.stringify(compact), /hunter2-SECRET/);
  assert.doesNotMatch(compact.elements, /listitem/, 'list items repeat their link text');
  const refs = lines.map((line) => line.split(' ')[0]);
  assert.equal(new Set(refs).size, refs.length, 'one line per element');
  assert.ok(compact.text.length > 0, 'page text is kept beside the elements');

  // A short ref works with the compact snapshot_id for actions.
  const target = await page.manager.observationTarget(page.entry, { elementRef: refs[2], snapshotId: compact.snapshot_id });
  assert.equal(target.status, undefined);
  assert.ok(Array.isArray(target.path));
  const stale = await page.manager.observationTarget(page.entry, { elementRef: refs[2], snapshotId: json.snapshot_id });
  assert.equal(stale.status, 'stale');
});

test('incremental compact snapshots report only changes and keep earlier refs valid', async () => {
  const page = openPage('compact-incremental', listing(5));
  const first = await page.snapshot({ format: 'compact' });
  const searchRef = first.elements.split('\n').find((line) => line.includes('textbox "Search"')).split(' ')[0];
  const item2Ref = first.elements.split('\n').find((line) => line.includes('"Item 2 ')).split(' ')[0];
  // The page changes in place: the search value, one new result, one removed.
  const { dom } = page;
  const search = dom.body.children.find((node) => node.tagName === 'INPUT' && node.getAttribute('name') === 'q');
  search.value = 'trail shoes';
  const list = dom.body.children.find((node) => node.tagName === 'UL');
  list.children[3].style = { display: 'none' };
  list.append(dom.h('li', {}, dom.h('a', { href: '/itm/77' }, 'Item 77 trail shoe'), ' $77.99'));
  const second = await page.snapshot({ sinceSnapshotId: first.snapshot_id });
  assert.equal(second.incremental.applied, true);
  assert.equal(second.base_snapshot_id, first.snapshot_id);
  const changes = second.changes.split('\n');
  assert.ok(changes.some((line) => line.startsWith(`~ ${searchRef} textbox "Search" value="trail shoes"`)), second.changes);
  assert.ok(changes.some((line) => /^\+ e\d+ link "Item 77 trail shoe" href=https:\/\/shop\.example\/itm\/77$/.test(line)), second.changes);
  assert.ok(changes.some((line) => /^- e\d+ link "Item 4 /.test(line)), second.changes);
  assert.equal(second.incremental.changed, 1);
  assert.equal(second.incremental.added, 1);
  assert.equal(second.incremental.removed, 1);
  assert.ok(second.incremental.unchanged >= 5);
  assert.equal(second.elements, undefined, 'unchanged lines are not repeated');
  // Unchanged and changed elements keep their refs with the new snapshot_id.
  for (const ref of [searchRef, item2Ref]) {
    const target = await page.manager.observationTarget(page.entry, { elementRef: ref, snapshotId: second.snapshot_id });
    assert.equal(target.status, undefined, ref);
  }
  assert.ok(Buffer.byteLength(JSON.stringify(second)) < Buffer.byteLength(JSON.stringify(first)));

  // Nothing changed: an empty diff.
  const third = await page.snapshot({ format: 'compact', sinceSnapshotId: second.snapshot_id });
  assert.equal(third.changes, '');
  assert.equal(third.text_unchanged, true);

  // A navigation replaces the document: a full compact snapshot says why.
  page.navigate('https://shop.example/other', listing(2, { prefix: 'Other' }));
  const fourth = await page.snapshot({ format: 'compact', sinceSnapshotId: third.snapshot_id });
  assert.deepEqual(fourth.incremental, { applied: false, since_snapshot_id: third.snapshot_id, reason: 'document_changed' });
  assert.match(fourth.elements, /link "Other 1 /);
  const unknown = await page.snapshot({ sinceSnapshotId: 'not-a-snapshot' });
  assert.equal(unknown.incremental.reason, 'base_snapshot_unknown');
  await assert.rejects(page.snapshot({ format: 'json', sinceSnapshotId: fourth.snapshot_id }), /requires format compact/);
  await assert.rejects(page.snapshot({ format: 'yaml' }), /format must be outline, json or compact/);
});

test('accessibility text inside a secret field never becomes a snapshot row', async () => {
  // Unit: rows whose accessibility node descends from a secret field go.
  const semantic = [{ refIndex: 0, role: 'textbox' }, { refIndex: 1, role: 'StaticText', name: 'SECRET' }, { refIndex: 2, role: 'StaticText', name: 'Visible' }];
  const interactive = [{ refIndex: 0 }];
  const removed = dropSecretFieldAxRows({
    axNodes: [{ nodeId: '1' }, { nodeId: '2', parentId: '1' }, { nodeId: '3', parentId: '2' }, { nodeId: '4', parentId: '9' }],
    secretFieldAxIds: new Set(['1']), axRowNodeIds: new Map([[1, '3'], [2, '4']]), semantic, interactive,
  });
  assert.equal(removed, 1);
  assert.deepEqual(semantic.map((row) => row.refIndex), [0, 2]);

  // Through the snapshot: Chromium exposes an input's value as a StaticText
  // child of its textbox; for a secret-named field that text is dropped.
  const fake = fakeBrowser({ url: 'https://shop.example/account' });
  const root = fake.loadRoot('https://shop.example/account', ({ h, body }) => {
    body.append(
      h('input', { name: 'api_key', 'aria-label': 'API key', $value: 'sk-live-AX-SECRET' }),
      h('input', { name: 'nickname', 'aria-label': 'Nickname', $value: 'Ada' }),
    );
  });
  const [apiKey, nickname] = root.dom.body.children;
  const apiKeyText = { nodeType: 3, nodeValue: 'sk-live-AX-SECRET' };
  const nicknameText = { nodeType: 3, nodeValue: 'Ada' };
  root.axNodes = [
    { nodeId: '1', role: { value: 'RootWebArea' }, name: { value: '' }, backendDOMNodeId: fake.backendIdFor(root.dom.body) },
    { nodeId: '2', parentId: '1', role: { value: 'textbox' }, name: { value: 'API key' }, backendDOMNodeId: fake.backendIdFor(apiKey) },
    { nodeId: '3', parentId: '2', role: { value: 'generic' }, ignored: true, backendDOMNodeId: fake.backendIdFor({ nodeType: 1, tagName: 'DIV' }) },
    { nodeId: '4', parentId: '3', role: { value: 'StaticText' }, name: { value: 'sk-live-AX-SECRET' }, backendDOMNodeId: fake.backendIdFor(apiKeyText) },
    { nodeId: '5', parentId: '1', role: { value: 'textbox' }, name: { value: 'Nickname' }, backendDOMNodeId: fake.backendIdFor(nickname) },
    { nodeId: '6', parentId: '5', role: { value: 'StaticText' }, name: { value: 'Ada' }, backendDOMNodeId: fake.backendIdFor(nicknameText) },
  ];
  const snapshot = await browserSnapshotForParams(fake.manager, fake.entry, {});
  assert.equal(snapshot.accessibility.axTreeAvailable, true);
  assert.doesNotMatch(JSON.stringify(snapshot), /sk-live-AX-SECRET/);
  assert.ok(snapshot.semantic.some((row) => row.role === 'StaticText' && row.name === 'Ada'), 'other fields keep their text rows');
  const field = snapshot.semantic.find((row) => row.name === 'API key');
  assert.equal(field.valueRedacted, true);
});
