'use strict';

const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const vm = require('node:vm');
const test = require('node:test');
const {
  MAX_NODE_SNAPSHOT_KEYS, addDiagnostic, diagnosticsAfter, mapFramePointThroughQuad,
  observationScript, observedTargetGeometryScript, redactPageMessage, resolvePathScript,
  truncateUtf8Bytes,
} = require('./browser-observation');

const DEFAULT_STYLE = {
  display: 'block', visibility: 'visible', opacity: '1', overflow: 'visible',
  overflowX: 'visible', overflowY: 'visible', transform: 'none', perspective: 'none', zoom: '1',
};

function makeText(value, parentElement, ownerDocument) {
  return { nodeType: 3, nodeValue: String(value), parentElement, ownerDocument };
}

function makeElement(ownerDocument, tagName, options = {}) {
  const attributes = { ...(options.attributes || {}) };
  const classes = new Set(String(options.className || '').split(/\s+/u).filter(Boolean));
  const element = {
    nodeType: 1,
    tagName: String(tagName).toUpperCase(),
    ownerDocument,
    parentElement: null,
    children: [],
    childNodes: [],
    className: String(options.className || ''),
    classList: { contains: (name) => classes.has(name) },
    innerText: String(options.innerText || ''),
    textContent: String(options.textContent || options.innerText || ''),
    value: options.value,
    type: options.type || attributes.type || '',
    checked: options.checked,
    selected: options.selected,
    disabled: options.disabled === true,
    readOnly: options.readOnly === true,
    isContentEditable: options.isContentEditable === true,
    isConnected: true,
    scrollHeight: options.scrollHeight || 0,
    clientHeight: options.clientHeight || 0,
    scrollWidth: options.scrollWidth || 0,
    clientWidth: options.clientWidth || 0,
    clientLeft: options.clientLeft || 0,
    clientTop: options.clientTop || 0,
    offsetWidth: options.offsetWidth || options.width || 100,
    offsetHeight: options.offsetHeight || options.height || 20,
    labels: options.labels || null,
    style: { ...DEFAULT_STYLE, ...(options.style || {}) },
    getAttribute(name) { return Object.hasOwn(attributes, name) ? String(attributes[name]) : null; },
    hasAttribute(name) { return Object.hasOwn(attributes, name); },
    matches(selector) {
      return String(selector).split(',').some((entry) => {
        const part = entry.trim();
        if (part === '.monaco-editor' || part === '.CodeMirror' || part === '.cm-editor') return classes.has(part.slice(1));
        if (part === '.view-lines .view-line' || part === '.CodeMirror-code pre' || part === '.cm-content .cm-line') {
          const required = part.slice(part.lastIndexOf(' ') + 1).slice(1);
          if (!classes.has(required)) return false;
          const ancestorClass = part.slice(1, part.indexOf(' '));
          for (let parent = this.parentElement; parent; parent = parent.parentElement) {
            if (String(parent.className || '').split(/\s+/u).includes(ancestorClass)) return true;
          }
          return false;
        }
        if (part === 'a[href]') return this.tagName === 'A' && this.hasAttribute('href');
        if (part === '[tabindex]') return this.hasAttribute('tabindex');
        if (part === '[contenteditable]:not([contenteditable=false])') return this.hasAttribute('contenteditable') && this.getAttribute('contenteditable') !== 'false';
        if (part === '[role=textbox]') return this.getAttribute('role') === 'textbox';
        if (part.startsWith('input:not(')) return this.tagName === 'INPUT' && this.type !== 'password' && this.type !== 'hidden';
        if (part === 'input' || part === 'textarea' || part === 'select' || part === 'button') return this.tagName === part.toUpperCase();
        if (part.startsWith('[role=')) return this.getAttribute('role') === part.slice(6, -1).replaceAll('"', '');
        if (part.startsWith('.')) return classes.has(part.slice(1));
        return part.toLowerCase() === this.tagName.toLowerCase();
      });
    },
    getBoundingClientRect() {
      if (options.rectError) throw Object.assign(new Error('fixture DOM read failed'), { name: 'FixtureReadError' });
      const x = Number(options.x || 0);
      const y = Number(options.y || 0);
      const width = Number(options.width || 100);
      const height = Number(options.height || 20);
      return { x, y, width, height, right: x + width, bottom: y + height };
    },
    getRootNode() { return ownerDocument; },
    contains(candidate) {
      for (let node = candidate; node; node = node.parentElement) if (node === this) return true;
      return false;
    },
    closest(selector) {
      for (let node = this; node; node = node.parentElement) if (node.matches(selector)) return node;
      return null;
    },
    scrollIntoView() {},
    append(child) {
      child.parentElement = this;
      child.ownerDocument = ownerDocument;
      this.childNodes.push(child);
      if (child.nodeType === 1) this.children.push(child);
      return child;
    },
  };
  if (options.id) element.id = String(options.id);
  if (options.name) element.name = String(options.name);
  if (options.title) element.title = String(options.title);
  if (options.shadowRoot) element.shadowRoot = options.shadowRoot;
  if (options.codeMirror) element.CodeMirror = options.codeMirror;
  if (Object.hasOwn(options, 'contentDocument')) element.contentDocument = options.contentDocument;
  if (options.contentWindow) element.contentWindow = options.contentWindow;
  if (options.getBoxQuads) element.getBoxQuads = options.getBoxQuads;
  if (options.rectError) element.rectError = true;
  if (options.text !== undefined) element.append(makeText(options.text, element, ownerDocument));
  return element;
}

function makeDocument(bodySetup) {
  const view = {
    innerWidth: 1440, innerHeight: 900, devicePixelRatio: 1,
    scrollX: 0, scrollY: 0, frameElement: null, parent: null,
    getComputedStyle(element) { return element.style || DEFAULT_STYLE; },
  };
  view.parent = view;
  const document = {
    nodeType: 9, title: 'fixture title', defaultView: view, activeElement: null,
    getElementById() { return null; },
    createTreeWalker(root, whatToShow) {
      const descendants = [];
      const walk = (node) => {
        for (const child of node.childNodes || []) {
          if ((whatToShow === 4 && child.nodeType === 3) || (whatToShow === 1 && child.nodeType === 1)) descendants.push(child);
          if (child.nodeType === 1) walk(child);
        }
      };
      walk(root);
      let index = 0;
      return { nextNode() { return descendants[index++] || null; } };
    },
  };
  view.document = document;
  const html = makeElement(document, 'html');
  const body = makeElement(document, 'body');
  html.getBoundingClientRect = () => ({ x: 0, y: 0, width: view.innerWidth, height: view.innerHeight, right: view.innerWidth, bottom: view.innerHeight });
  body.getBoundingClientRect = () => ({ x: 0, y: 0, width: view.innerWidth, height: view.innerHeight, right: view.innerWidth, bottom: view.innerHeight });
  html.append(body);
  document.children = [html];
  document.documentElement = html;
  document.body = body;
  document.activeElement = body;
  bodySetup(body, document);
  document.elementFromPoint = (x, y) => {
    const deepest = (root) => {
      const children = Array.from(root?.children || []);
      for (let index = children.length - 1; index >= 0; index -= 1) {
        const child = children[index];
        let rect;
        try { rect = child.getBoundingClientRect(); } catch { continue; }
        if (x < rect.x || y < rect.y || x >= rect.right || y >= rect.bottom) continue;
        const shadow = child.shadowRoot ? deepest(child.shadowRoot) : null;
        return shadow || deepest(child) || child;
      }
      return null;
    };
    return deepest(document) || body;
  };
  return { document, view, html, body };
}

function executeSnapshot({ setup = () => {}, snapshotId = 'snapshot-1', maxText = 32 * 1024, globals = {} } = {}) {
  const state = makeDocument(setup);
  state.document.documentElement.scrollWidth = 1440;
  state.document.documentElement.scrollHeight = 900;
  const context = vm.createContext({
    document: state.document, window: state.view,
    location: { href: 'https://fixture.invalid/' },
    innerWidth: state.view.innerWidth, innerHeight: state.view.innerHeight,
    devicePixelRatio: 1, scrollX: 0, scrollY: 0,
    ...globals,
  });
  const script = observationScript({ snapshotId, maxText });
  const result = vm.runInContext(script, context, { timeout: 5000 });
  return { ...state, context, result, script };
}

test('generated observation executes and preserves legacy interactive and editor fields', () => {
  let monacoRoot;
  const codeMirrorRootRef = {};
  const snapshot = executeSnapshot({
    setup(body, document) {
      body.append(makeElement(document, 'button', {
        attributes: { 'aria-label': 'Save', 'aria-readonly': 'true' }, innerText: 'Save now',
      }));
      body.append(makeElement(document, 'div', { attributes: { tabindex: '0' }, innerText: 'Focusable custom control' }));
      body.append(makeElement(document, 'section', {
        innerText: 'Nested scroll region', scrollHeight: 400, clientHeight: 100,
        style: { overflowY: 'auto' },
      }));
      monacoRoot = body.append(makeElement(document, 'div', { className: 'monaco-editor', innerText: 'rendered Monaco text' }));
      codeMirrorRootRef.root = body.append(makeElement(document, 'div', {
        className: 'CodeMirror', innerText: 'rendered CodeMirror text',
        codeMirror: { getValue: () => 'CodeMirror model value', getOption: () => true },
      }));
      const cmEditor = body.append(makeElement(document, 'div', { className: 'cm-editor' }));
      const cmContent = cmEditor.append(makeElement(document, 'div', { className: 'cm-content' }));
      cmContent.append(makeElement(document, 'div', { className: 'cm-line', innerText: 'CodeMirror 6 line text', text: 'CodeMirror 6 line text' }));
    },
    globals: {
      monaco: { editor: { getEditors: () => [{
        getDomNode: () => monacoRoot,
        getModel: () => ({ getValue: () => 'Monaco model value' }),
        getRawOptions: () => ({ readOnly: true }),
      }] } },
    },
  });

  const interactive = snapshot.result.interactive;
  const button = interactive.find((node) => node.ariaLabel === 'Save');
  assert.ok(button);
  assert.equal(button.readOnly, true);
  assert.equal(button.text, 'Save now');
  assert.ok(interactive.some((node) => node.tag === 'div' && node.actionable && node.text === 'Focusable custom control'));
  assert.ok(interactive.some((node) => node.tag === 'section' && node.scrollable));

  const monaco = interactive.find((node) => node.editor === 'monaco');
  assert.equal(monaco.text, 'Monaco model value');
  assert.equal(monaco.readOnly, true);
  const codeMirror = interactive.find((node) => node.editor === 'code-editor' && node.text === 'CodeMirror model value');
  assert.equal(codeMirror.readOnly, true);
  const cm6 = snapshot.result.editors.find((editor) => editor.text.includes('CodeMirror 6 line text'));
  assert.ok(cm6, 'rendered CodeMirror line extraction remains available when no CodeMirror.getValue exists');
  assert.ok(snapshot.result.interactive.every((node) => Number.isInteger(node.refIndex)));
  assert.equal(snapshot.result.accessibility.axTreeAvailable, false);
  assert.equal(snapshot.result.accessibility.limitation, 'owned_cdp_ax_tree_not_resolved');
});

test('snapshot text limits count UTF-8 bytes and never split a code point', () => {
  const snapshot = executeSnapshot({
    maxText: 5,
    setup(body, document) { body.append(makeElement(document, 'div', { text: 'é🛰x' })); },
  }).result;
  assert.equal(snapshot.text, 'é');
  assert.equal(snapshot.textBytesReturned, 2);
  assert.equal(snapshot.textLength, 7);
  assert.equal(snapshot.textTruncated, true);
  assert.equal(snapshot.textTruncatedBytes, 5);
  assert.equal(Buffer.byteLength(snapshot.text), 2);
  assert.equal(truncateUtf8Bytes('é🛰x', 5), 'é');

  const defaultLimit = executeSnapshot({
    setup(body, document) { body.append(makeElement(document, 'div', { text: 'a'.repeat(40000) })); },
  }).result;
  assert.equal(Buffer.byteLength(defaultLimit.text), 32 * 1024);
  assert.equal(defaultLimit.textLength, 40000);
  assert.equal(defaultLimit.textTruncatedBytes, 40000 - (32 * 1024));
});

test('DOM walk is bounded, reports depth limits and surfaces per-element errors', () => {
  const large = executeSnapshot({
    setup(body, document) {
      for (let index = 0; index < 10050; index += 1) body.append(makeElement(document, 'div'));
    },
  }).result;
  assert.equal(large.traversal.visitedElements, large.traversal.maxElements);
  assert.equal(large.traversal.truncated, true);
  assert.equal(large.semanticTruncated, true);
  assert.equal(large.traversal.nodeCountExact, false);

  const deep = executeSnapshot({
    setup(body, document) {
      let parent = body;
      for (let index = 0; index < 160; index += 1) parent = parent.append(makeElement(document, 'div'));
    },
  }).result;
  assert.equal(deep.traversal.depthTruncated, true);
  assert.equal(deep.traversal.errorCount, 0);

  const errored = executeSnapshot({
    setup(body, document) { body.append(makeElement(document, 'button', { rectError: true })); },
  }).result;
  assert.equal(errored.traversal.errorCount, 1);
  assert.equal(errored.traversal.errors[0].stage, 'element');
  assert.equal(errored.traversal.errors[0].name, 'Error', 'unrecognized error names are sanitized');
});

test('per-element identity retains only host-retained snapshot keys and rejects stale identity', () => {
  const state = makeDocument((body, document) => {
    body.append(makeElement(document, 'button', { attributes: { 'aria-label': 'target' } }));
  });
  const context = vm.createContext({
    document: state.document, window: state.view,
    location: { href: 'https://fixture.invalid/' },
    innerWidth: 1440, innerHeight: 900, devicePixelRatio: 1, scrollX: 0, scrollY: 0,
  });
  let oldest;
  let latest;
  for (let index = 1; index <= MAX_NODE_SNAPSHOT_KEYS + 2; index += 1) {
    const result = vm.runInContext(observationScript({ snapshotId: 'snap-' + index }), context);
    if (index === 1) oldest = result.refs[0].nodeKey;
    if (index === MAX_NODE_SNAPSHOT_KEYS + 2) latest = result.refs[0].nodeKey;
  }
  const store = vm.runInContext('globalThis[Symbol.for(\"workass.browser.observed-node-refs\")]', context);
  const node = state.body.children[0];
  const keys = store.get(node);
  assert.equal(keys.size, MAX_NODE_SNAPSHOT_KEYS);
  assert.equal(keys.has(latest), true);
  assert.equal(keys.has(oldest), false);
  const current = vm.runInContext(resolvePathScript([{ kind: 'child', index: 0 }, { kind: 'child', index: 0 }, { kind: 'child', index: 0 }], latest), context);
  assert.equal(current.node, node);
  assert.equal(current.identityValid, true);
  const stale = vm.runInContext(resolvePathScript([{ kind: 'child', index: 0 }, { kind: 'child', index: 0 }, { kind: 'child', index: 0 }], oldest), context);
  assert.equal(stale.identityValid, false);
});

test('frame geometry maps content coordinates through affine transforms and rejects perspective', () => {
  const mapped = mapFramePointThroughQuad(
    { x: 200, y: 100 }, { width: 400, height: 200 },
    { p1: { x: 120, y: 80 }, p2: { x: 520, y: 80 }, p3: { x: 480, y: 280 }, p4: { x: 80, y: 280 } },
  );
  assert.deepEqual(mapped, { available: true, x: 300, y: 180 });
  const unsupported = mapFramePointThroughQuad(
    { x: 200, y: 100 }, { width: 400, height: 200 },
    { p1: { x: 0, y: 0 }, p2: { x: 400, y: 0 }, p3: { x: 300, y: 200 }, p4: { x: 0, y: 200 } },
  );
  assert.equal(unsupported.available, false);
  assert.equal(unsupported.reason, 'perspective_frame_transform_unsupported');
  const geometryScript = observedTargetGeometryScript({ selector: '#target' });
  assert.match(geometryScript, /getBoxQuads/);
  assert.match(geometryScript, /clientLeft/);
  assert.match(geometryScript, /transformed_frame_geometry_unavailable/);
});

test('actual frame extraction accounts for content borders and transforms, and names inaccessible targets', () => {
  let childDoc;
  const snapshot = executeSnapshot({
    setup(body, document) {
      const sameOrigin = body.append(makeElement(document, 'iframe', {
        name: 'same-origin', width: 500, height: 300,
        contentDocument: null,
        getBoxQuads: () => [{
          p1: { x: 110, y: 130 }, p2: { x: 510, y: 130 },
          p3: { x: 490, y: 330 }, p4: { x: 90, y: 330 },
        }],
      }));
      childDoc = makeDocument((childBody, nestedDocument) => {
        childBody.append(makeElement(nestedDocument, 'button', {
          attributes: { 'aria-label': 'Inside frame' }, x: 20, y: 10, width: 100, height: 30, innerText: 'Frame target',
        }));
      });
      childDoc.view.innerWidth = 400;
      childDoc.view.innerHeight = 200;
      childDoc.document.defaultView = childDoc.view;
      childDoc.view.frameElement = sameOrigin;
      childDoc.view.parent = document.defaultView;
      sameOrigin.contentDocument = childDoc.document;
      sameOrigin.contentWindow = childDoc.view;

      body.append(makeElement(document, 'iframe', {
        name: 'cross-origin', contentDocument: null,
        contentWindow: { get location() { throw Object.assign(new Error('cross origin'), { name: 'SecurityError' }); } },
      }));
    },
  });
  const inside = snapshot.result.semantic.find((node) => node.name === 'Inside frame');
  assert.ok(inside);
  assert.deepEqual(
    { x: inside.bounds.x, y: inside.bounds.y, width: inside.bounds.width, height: inside.bounds.height },
    { x: 126, y: 140, width: 103, height: 30 },
  );
  assert.equal(inside.boundsAvailable, true);
  const ref = snapshot.result.refs[inside.refIndex];
  const targetGeometry = vm.runInContext(
    observedTargetGeometryScript({ path: ref.path, nodeKey: ref.nodeKey }),
    snapshot.context,
  );
  assert.equal(targetGeometry.status, 'ready');
  assert.equal(targetGeometry.x, 177.5);
  assert.equal(targetGeometry.y, 155);
  const sameOrigin = snapshot.result.frames.find((frame) => frame.name === 'same-origin');
  assert.equal(sameOrigin.accessible, true);
  assert.equal(sameOrigin.targetOwned, false);
  assert.equal(sameOrigin.limitation, 'owned_cdp_frame_target_not_resolved');
  const crossOrigin = snapshot.result.frames.find((frame) => frame.name === 'cross-origin');
  assert.equal(crossOrigin.access, 'unavailable');
  assert.equal(crossOrigin.limitation, 'cross_origin_requires_owned_cdp_target');
});

test('page-message redaction covers JSON keys, variants, nested strings, and bearer values', () => {
  const fixtures = [
    '{\"token\":\"fixture-only-secret\"}',
    '{\"api_key\": \"fixture-only-secret\", \"next\": {\"clientSecret\": \"fixture-only-secret\"}}',
    '{\"api key\":\"fixture-only-secret\",\"privateKey\":\"fixture-only-secret\"}',
    'token=fixture-only-secret api-key: fixture-only-secret apiKey=\"fixture-only-secret\"',
    'accesstoken=FIXTURE_ONLY_VALUE dbpassword=FIXTURE_ONLY_VALUE credentials=FIXTURE_ONLY_VALUE clientsecret=FIXTURE_ONLY_VALUE',
    '{"accesstoken":"FIXTURE_ONLY_VALUE","dbpassword":"FIXTURE_ONLY_VALUE","credentials":"FIXTURE_ONLY_VALUE","clientsecret":"FIXTURE_ONLY_VALUE"}',
    '{"authorization":"FIXTURE_ONLY_VALUE","authKey":"FIXTURE_ONLY_VALUE","accessKey":"FIXTURE_ONLY_VALUE"}',
    'Authorization: Bearer fixture-only-secret',
    'payload=\"{\\\"access_token\\\":\\\"fixture-only-secret\\\"}\"',
    '{\"message\":\"token=fixture-only-secret and Bearer fixture-only-secret\"}',
    'nested={\"refreshToken\":\"fixture-only-secret\",\"credential_value\":\"fixture-only-secret\"}',
  ];
  for (const input of fixtures) {
    const output = redactPageMessage(input);
    assert.doesNotMatch(output, /fixture-only-secret|FIXTURE_ONLY_VALUE/u, 'fixture value was redacted');
    assert.match(output, /\[redacted\]/u);
    assert.equal(redactPageMessage(output), output, 'redaction remains stable');
  }
  assert.equal(redactPageMessage('é🛰x', 5), 'é');
  assert.ok(Buffer.byteLength(redactPageMessage('é🛰x', 5)) <= 5);
});

test('page-message redaction fails closed on incomplete quotes, nested assignments, and URL parameters', () => {
  const sentinel = 'FIXTURE_ONLY_VALUE';
  const cutOff = redactPageMessage('{"token":"' + sentinel.repeat(5000), 1024);
  assert.doesNotMatch(cutOff, /FIXTURE_ONLY_VALUE/u);
  assert.match(cutOff, /\{"token":"\[redacted\]/u);

  const nested = redactPageMessage('payload token: { nested: "' + sentinel + '" } and credential=["' + sentinel + '", {"inner":"' + sentinel + '"}] next=ok');
  assert.doesNotMatch(nested, /FIXTURE_ONLY_VALUE/u);
  assert.equal(nested, 'payload token: [redacted] and credential=[redacted] next=ok');

  const url = redactPageMessage('https://example.test/?token=' + sentinel + '&next=ok');
  assert.equal(url, 'https://example.test/?token=[redacted]&next=ok');
  assert.doesNotMatch(url, /FIXTURE_ONLY_VALUE/u);

  const escaped = redactPageMessage('payload="{\\"access_token\\":\\"' + sentinel + '\\"}"');
  assert.doesNotMatch(escaped, /FIXTURE_ONLY_VALUE/u);
});

test('large incomplete quoted assignments complete within a bounded interval', { timeout: 10000 }, () => {
  const modulePath = require.resolve('./browser-observation');
  const childScript = [
    'const { redactPageMessage } = require(' + JSON.stringify(modulePath) + ');',
    'const inputs = [\'{"token":"\' + \'FIXTURE_ONLY_VALUE\'.repeat(5000), \'token=FIXTURE_ONLY_VALUE;\'.repeat(4000)];',
    'const started = Date.now();',
    'const outputs = inputs.map((input) => redactPageMessage(input, 1024));',
    'process.stdout.write(JSON.stringify({ elapsedMs: Date.now() - started, leaked: outputs.some((output) => output.includes(\'FIXTURE_ONLY_VALUE\')), outputBytes: outputs.map((output) => Buffer.byteLength(output)) }));',
  ].join('\n');
  const child = spawnSync(process.execPath, ['-e', childScript], {
    encoding: 'utf8', timeout: 5000, maxBuffer: 1024,
  });
  assert.equal(child.error, undefined, 'large redaction finishes before the five-second safety bound');
  assert.equal(child.status, 0);
  const result = JSON.parse(child.stdout);
  assert.equal(result.leaked, false);
  assert.ok(result.outputBytes.every((bytes) => bytes <= 1024));
  assert.ok(result.elapsedMs < 5000);
});

test('diagnostic cursors report initial ring loss and clamp future requests', () => {
  const entry = { documentGeneration: 3, diagnosticSequence: 0, diagnosticBytes: 0, diagnostics: [] };
  addDiagnostic(entry, 'console.warn', 'safe warning', 1);
  assert.equal(diagnosticsAfter(entry, 0).truncated, false);
  for (let index = 0; index < 205; index += 1) addDiagnostic(entry, 'console.warn', 'warning-' + index, index + 2);
  assert.equal(entry.diagnostics.length, 200);
  assert.equal(entry.diagnostics[0].record.sequence, 7);
  assert.equal(diagnosticsAfter(entry, 0, 100).truncated, true);
  assert.equal(diagnosticsAfter(entry, 1, 100).truncated, true);
  assert.equal(diagnosticsAfter(entry, 205, 20).entries.length, 1);
  const future = diagnosticsAfter(entry, 9999, 20);
  assert.equal(future.entries.length, 0);
  assert.equal(future.cursor, 206);
  assert.equal(future.nextSequence, 207);
});
