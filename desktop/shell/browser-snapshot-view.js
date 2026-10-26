'use strict';

// Snapshot views built on the observation result of browser-manager.js:
//  - element facts (href, src, alt, title, value) added to json rows without
//    displacing any byte of the legacy fields;
//  - format "compact": one line per element with short refs (e12, f1);
//  - incremental compact snapshots against an earlier compact snapshot of the
//    same document.

const { redactPageValue, truncateUtf8Bytes } = require('./browser-observation');

const MAX_SNAPSHOT_RESPONSE_BYTES = 64 * 1024;
const NODE_FACT_KEYS = ['href', 'src', 'alt', 'title', 'value', 'valueTruncated', 'valueRedacted', 'placeholder'];
const COMPACT_HISTORY = 8;
const COMPACT_TEXT_RESERVE_FOR_OUTLINE = 4096;
const MAX_DIFF_EDITS = 2000;
const MAX_TEXT_CHANGE_RUN_CHARS = 400;

function utf8Bytes(value) {
  return Buffer.byteLength(String(value == null ? '' : value), 'utf8');
}

function jsonBytes(value) {
  return Buffer.byteLength(JSON.stringify(value), 'utf8');
}

// JSON-encoded cost of one string inside a larger JSON document.
function jsonStringBytes(value) {
  return jsonBytes(String(value == null ? '' : value)) - 2;
}

function redactFact(key, value) {
  if (typeof value === 'boolean') return value;
  if (key === 'href' || key === 'src') return redactPageValue(value, 2048);
  if (key === 'value') return redactPageValue(value, 4096);
  return redactPageValue(value, 1024);
}

// Removes the element facts from every row before the legacy byte budget runs,
// so the legacy fields are budgeted exactly as before. Returns the redacted
// facts keyed by element_ref.
function detachNodeFacts(...rowLists) {
  const facts = new Map();
  for (const rows of rowLists) {
    for (const node of Array.isArray(rows) ? rows : []) {
      if (!node || typeof node !== 'object') continue;
      let found = null;
      for (const key of NODE_FACT_KEYS) {
        if (!Object.prototype.hasOwnProperty.call(node, key)) continue;
        const value = node[key];
        delete node[key];
        if (value == null || value === '' || value === false) continue;
        if (typeof value !== 'string' && typeof value !== 'boolean') continue;
        found = found || {};
        found[key] = redactFact(key, value);
      }
      if (found && node.element_ref && !facts.has(node.element_ref)) facts.set(node.element_ref, found);
    }
  }
  return facts;
}

// Adds the facts back, in document order, only while the whole response stays
// within the limit. Rows that do not fit keep their legacy shape and are
// counted in truncation.element_facts_omitted.
function attachNodeFacts(result, facts, limit = MAX_SNAPSHOT_RESPONSE_BYTES) {
  if (!facts || !facts.size) return result;
  const rows = [];
  for (const list of [result.semantic, result.interactive]) {
    for (const node of Array.isArray(list) ? list : []) {
      const fields = node && facts.get(node.element_ref);
      if (fields) rows.push([node, fields]);
    }
  }
  if (!rows.length) return result;
  const unbounded = !Number.isFinite(limit);
  const truncation = result.truncation && typeof result.truncation === 'object' ? result.truncation : null;
  if (truncation) truncation.element_facts_omitted = 0;
  const measure = () => {
    if (truncation) truncation.total_bytes = 0;
    let size = jsonBytes(result);
    if (truncation) {
      truncation.total_bytes = size;
      size = jsonBytes(result);
      truncation.total_bytes = size;
      size = jsonBytes(result);
    }
    return size;
  };
  if (unbounded) {
    for (const [node, fields] of rows) Object.assign(node, fields);
    measure();
    return result;
  }
  // Room for the omitted count and the width of total_bytes.
  let size = measure() + 32;
  let omitted = 0;
  for (const [node, fields] of rows) {
    const cost = jsonBytes(fields) - 1;
    if (size + cost > limit) { omitted += 1; continue; }
    Object.assign(node, fields);
    size += cost;
  }
  if (truncation) truncation.element_facts_omitted = omitted;
  let total = measure();
  for (let index = rows.length - 1; total > limit && index >= 0; index -= 1) {
    const [node, fields] = rows[index];
    let removed = false;
    for (const key of Object.keys(fields)) {
      if (Object.prototype.hasOwnProperty.call(node, key)) { delete node[key]; removed = true; }
    }
    if (removed) {
      if (truncation) truncation.element_facts_omitted += 1;
      total = measure();
    }
  }
  return result;
}

// Accessibility rows that descend from a secret field (password, one-time
// code, secret-named input) are that field's value as text: they are removed
// from both row lists. Returns how many rows were removed.
function dropSecretFieldAxRows({ axNodes, secretFieldAxIds, axRowNodeIds, semantic, interactive }) {
  if (!secretFieldAxIds || !secretFieldAxIds.size || !axRowNodeIds || !axRowNodeIds.size) return 0;
  const parentOf = new Map();
  for (const node of Array.isArray(axNodes) ? axNodes : []) {
    if (node && node.nodeId != null && node.parentId != null) parentOf.set(String(node.nodeId), String(node.parentId));
  }
  const hiddenRefIndexes = new Set();
  for (const [refIndex, nodeId] of axRowNodeIds) {
    let current = parentOf.get(nodeId);
    for (let depth = 0; current && depth < 64; depth += 1) {
      if (secretFieldAxIds.has(current)) { hiddenRefIndexes.add(refIndex); break; }
      current = parentOf.get(current);
    }
  }
  if (!hiddenRefIndexes.size) return 0;
  for (const rows of [semantic, interactive]) {
    for (let index = rows.length - 1; index >= 0; index -= 1) {
      if (hiddenRefIndexes.has(Number(rows[index].refIndex))) rows.splice(index, 1);
    }
  }
  return hiddenRefIndexes.size;
}

// ---------------------------------------------------------------------------
// Sequence diff (Myers, with common prefix/suffix trimmed). Returns operations
// ['=', oldIndex, newIndex], ['-', oldIndex, -1], ['+', -1, newIndex], or null
// when more than maxEdits insertions and deletions would be needed.

function myersMiddle(a, b, maxEdits) {
  const n = a.length;
  const m = b.length;
  const max = Math.min(n + m, maxEdits);
  const offset = max + 1;
  const v = new Int32Array(2 * max + 3);
  const trace = [];
  for (let d = 0; d <= max; d += 1) {
    trace.push(v.slice(offset - d - 1, offset + d + 2));
    for (let k = -d; k <= d; k += 2) {
      let x = (k === -d || (k !== d && v[offset + k - 1] < v[offset + k + 1]))
        ? v[offset + k + 1]
        : v[offset + k - 1] + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) { x += 1; y += 1; }
      v[offset + k] = x;
      if (x >= n && y >= m) {
        const ops = [];
        let cx = n;
        let cy = m;
        for (let step = d; step > 0; step -= 1) {
          const saved = trace[step];
          const at = (kk) => saved[kk + step + 1];
          const kNow = cx - cy;
          const prevK = (kNow === -step || (kNow !== step && at(kNow - 1) < at(kNow + 1))) ? kNow + 1 : kNow - 1;
          const prevX = at(prevK);
          const prevY = prevX - prevK;
          while (cx > prevX && cy > prevY) { cx -= 1; cy -= 1; ops.push(['=', cx, cy]); }
          if (prevK === kNow + 1) { cy -= 1; ops.push(['+', -1, cy]); }
          else { cx -= 1; ops.push(['-', cx, -1]); }
        }
        while (cx > 0 && cy > 0) { cx -= 1; cy -= 1; ops.push(['=', cx, cy]); }
        return ops.reverse();
      }
    }
  }
  return null;
}

function diffSequences(a, b, maxEdits = MAX_DIFF_EDITS) {
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start += 1;
  let endA = a.length;
  let endB = b.length;
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) { endA -= 1; endB -= 1; }
  const ops = [];
  for (let index = 0; index < start; index += 1) ops.push(['=', index, index]);
  const middleA = a.slice(start, endA);
  const middleB = b.slice(start, endB);
  if (middleA.length + middleB.length > 0) {
    let middle;
    if (!middleA.length) middle = middleB.map((_value, index) => ['+', -1, index]);
    else if (!middleB.length) middle = middleA.map((_value, index) => ['-', index, -1]);
    else middle = myersMiddle(middleA, middleB, maxEdits);
    if (!middle) return null;
    let edits = 0;
    for (const [op, oldIndex, newIndex] of middle) {
      if (op !== '=') edits += 1;
      ops.push([op, oldIndex < 0 ? -1 : oldIndex + start, newIndex < 0 ? -1 : newIndex + start]);
    }
    if (edits > maxEdits) return null;
  }
  for (let offset = 0; offset < a.length - endA; offset += 1) ops.push(['=', endA + offset, endB + offset]);
  return ops;
}

// ---------------------------------------------------------------------------
// Compact lines.

const DROPPED_ROLES = new Set([
  'statictext', 'inlinetextbox', 'linebreak', 'generic', 'none', 'presentation', 'list', 'listitem',
  'listmarker', 'paragraph', 'group', 'section', 'div', 'span', 'layouttable', 'layouttablerow',
  'layouttablecell', 'strong', 'emphasis', 'time', 'superscript', 'subscript', 'code', 'term',
  'definition', 'descriptionlist', 'descriptionlistterm', 'descriptionlistdetail', 'blockquote',
  'caption', 'rootwebarea', 'webarea', 'table', 'row', 'rowgroup', 'cell', 'gridcell',
  'columnheader', 'rowheader', 'label', 'legend', 'abbr', 'mark', 'insertion', 'deletion',
  'contentdeletion', 'contentinsertion', 'ruby', 'figcaption', 'pre', 'iframe', 'iframepresentational',
]);
const LANDMARK_ROLES = new Set(['main', 'navigation', 'banner', 'contentinfo', 'complementary', 'region', 'form', 'search', 'article']);
const CHECKABLE_ROLES = /^(?:checkbox|radio|switch|menuitemcheckbox|menuitemradio)$/;

function oneLine(value, maxBytes = 1024) {
  return truncateUtf8Bytes(String(value == null ? '' : value).replace(/\s+/g, ' ').trim(), maxBytes);
}

function quoted(value) {
  return '"' + oneLine(value, 480).replace(/"/g, '\\"') + '"';
}

const INTERACTIVE_ROLES = new Set([
  'link', 'button', 'textbox', 'searchbox', 'combobox', 'listbox', 'checkbox', 'radio', 'switch', 'slider',
  'spinbutton', 'tab', 'menuitem', 'menuitemcheckbox', 'menuitemradio', 'option', 'treeitem', 'scrollbar',
]);

// The DOM rows mark every element with a role as actionable, so compact
// output decides what can be acted on from the role and the element itself.
// Returns 'interactive', 'outline' or '' (dropped).
function compactKind(node) {
  const role = String(node.role || node.tag || '').toLowerCase();
  const tag = String(node.tag || '').toLowerCase();
  if (node.editor || INTERACTIVE_ROLES.has(role) || node.editable === true || node.scrollable === true) return 'interactive';
  // A focusable or editable element without a role keeps its tag as role.
  if (node.actionable === true && tag && role === tag && role !== 'img' && !LANDMARK_ROLES.has(role)) return 'interactive';
  if (DROPPED_ROLES.has(role)) return '';
  if (LANDMARK_ROLES.has(role)) return node.ariaLabel ? 'outline' : '';
  return oneLine(node.name || node.alt || '') ? 'outline' : '';
}

function compactNodeBody(node) {
  const role = String(node.role || node.tag || 'element');
  let name = oneLine(node.name || '', 480);
  if (!name && node.alt) name = oneLine(node.alt, 480);
  if (!name && node.actionable && node.text) name = oneLine(node.text, 480);
  const parts = [role];
  if (name) parts.push(quoted(name));
  if (Number.isInteger(node.headingLevel)) parts.push('level=' + node.headingLevel);
  if (node.href) parts.push('href=' + node.href);
  if (node.src) parts.push('src=' + node.src);
  if (node.alt && oneLine(node.alt) !== name) parts.push('alt=' + quoted(node.alt));
  if (node.title && oneLine(node.title) !== name) parts.push('title=' + quoted(node.title));
  if (node.valueRedacted) parts.push('value=[redacted]');
  else if (node.value) parts.push('value=' + quoted(node.value) + (node.valueTruncated ? '(truncated)' : ''));
  else if (node.placeholder && oneLine(node.placeholder) !== name) parts.push('placeholder=' + quoted(node.placeholder));
  if (node.checked === true) parts.push('checked');
  else if (node.checked === false && CHECKABLE_ROLES.test(role)) parts.push('unchecked');
  if (node.expanded === true) parts.push('expanded');
  else if (node.expanded === false) parts.push('collapsed');
  if (node.selected === true) parts.push('selected');
  if (node.disabled === true) parts.push('disabled');
  if (node.readOnly === true && node.editable === true) parts.push('readonly');
  if (node.focused === true && compactKind(node) === 'interactive') parts.push('focused');
  if (node.editor) parts.push('editor=' + node.editor);
  if (node.scrollable === true) parts.push('scrollable');
  return parts.join(' ');
}

const compactStates = new WeakMap();

function compactState(entry) {
  let state = compactStates.get(entry);
  if (!state) {
    state = { records: new Map() };
    compactStates.set(entry, state);
  }
  return state;
}

function rememberCompact(entry, snapshotId, record) {
  const state = compactState(entry);
  state.records.set(snapshotId, record);
  while (state.records.size > COMPACT_HISTORY) state.records.delete(state.records.keys().next().value);
}

function textWords(text) {
  return String(text || '').split(/\s+/).filter(Boolean);
}

function fitJSONString(value, allowance) {
  let text = String(value || '');
  if (allowance <= 0) return '';
  let cost = jsonStringBytes(text);
  while (cost > allowance && text) {
    text = truncateUtf8Bytes(text, Math.max(0, utf8Bytes(text) - (cost - allowance) - 1));
    cost = jsonStringBytes(text);
  }
  return text;
}

// Collects every candidate line, in document order, with its stable identity.
function compactCandidates(manager, entry, raw) {
  const observation = entry.observations && entry.observations.get(raw.snapshot_id);
  const frameRefs = observation && observation.frameRefs instanceof Map ? observation.frameRefs : new Map();
  const rootFrame = typeof manager.frameForSession === 'function' ? manager.frameForSession(entry, null) : null;
  const rootFrameId = rootFrame ? rootFrame.id : null;
  const lines = [];
  for (const node of Array.isArray(raw.semantic) ? raw.semantic : []) {
    const kind = node && node.element_ref ? compactKind(node) : '';
    if (!kind) continue;
    const ref = observation && observation.refs ? observation.refs.get(node.element_ref) : null;
    let frameId = (node.frame_ref && frameRefs.get(node.frame_ref)) || (ref && ref.frameId) || null;
    if (frameId === rootFrameId) frameId = null;
    const identity = String(frameId || '') + '|' + (ref && Array.isArray(ref.path)
      ? JSON.stringify(ref.path)
      : 'b' + String(ref && ref.backendDOMNodeId != null ? ref.backendDOMNodeId : node.element_ref));
    lines.push({ node, fullRef: node.element_ref, frameId, identity, interactive: kind === 'interactive' });
  }
  // Repeated rows are folded: a picture inside the link or button just before
  // it becomes that line's src, and consecutive duplicate outline lines (for
  // example a DOM row and an accessibility row for one heading) collapse.
  const deduplicated = [];
  for (const line of lines) {
    const previous = deduplicated[deduplicated.length - 1];
    const role = String(line.node.role || '').toLowerCase();
    const name = oneLine(line.node.name || line.node.alt || '');
    if (previous && previous.interactive && !line.interactive && (role === 'img' || role === 'image') &&
        previous.frameId === line.frameId && !previous.node.src && line.node.src &&
        name && (oneLine(previous.node.alt || '') === name || oneLine(previous.node.name || '') === name)) {
      previous.node = { ...previous.node, src: line.node.src };
      continue;
    }
    line.body = compactNodeBody(line.node);
    if (previous && !line.interactive && !previous.interactive && previous.body === line.body && previous.frameId === line.frameId) continue;
    deduplicated.push(line);
  }
  for (const line of deduplicated) {
    line.body = compactNodeBody(line.node);
    line.key = String(line.frameId || '') + '\u0001' + line.body;
    delete line.node;
  }
  return { lines: deduplicated, observation, frameRefs, rootFrameId };
}

function frameLabelFor(record, frameId) {
  if (!frameId) return '';
  if (!record.frameLabels.has(frameId)) {
    record.nextFrame += 1;
    record.frameLabels.set(frameId, 'f' + record.nextFrame);
  }
  return record.frameLabels.get(frameId);
}

function displayLine(line, record) {
  const label = frameLabelFor(record, line.frameId);
  return line.ref + ' ' + line.body + (label ? ' in=' + label : '');
}

function compactFrames(raw, frameRefs, record, shortByFullRef) {
  const rows = [];
  for (const frame of Array.isArray(raw.frames) ? raw.frames : []) {
    const frameId = frame.frame_ref ? frameRefs.get(frame.frame_ref) : null;
    // A frame boundary without an owned frame keeps its label by position.
    const label = frameLabelFor(record, frameId || ('dom:' + String(frame.selector || frame.name || rows.length)));
    const parts = [label, 'frame'];
    if (frame.name) parts.push(quoted(frame.name));
    if (frame.url) parts.push('url=' + frame.url);
    parts.push('access=' + (frame.access || 'unavailable'));
    if (frame.parent_element_ref && shortByFullRef.has(frame.parent_element_ref)) parts.push('inside=' + shortByFullRef.get(frame.parent_element_ref));
    if (frame.accessible !== true && frame.limitation) parts.push('limitation=' + oneLine(frame.limitation, 160));
    rows.push({ label, frameId, text: parts.join(' ') });
  }
  return rows;
}

function aliasRefs(observation, lines, frameLabels) {
  if (!observation || !observation.refs) return;
  for (const line of lines) {
    const record = observation.refs.get(line.fullRef);
    if (record && line.ref) observation.refs.set(line.ref, record);
  }
  if (!(observation.frameRefs instanceof Map)) observation.frameRefs = new Map();
  for (const [frameId, label] of frameLabels) {
    if (!String(frameId).startsWith('dom:')) observation.frameRefs.set(label, frameId);
  }
}

function compactBase(raw, format) {
  return {
    format,
    snapshot_id: raw.snapshot_id,
    url: raw.url,
    title: raw.title,
    document_generation: raw.document_generation,
    viewport_generation: raw.viewport_generation,
    loading: raw.loading === true,
    error: raw.error || null,
    effective_viewport: raw.effective_viewport,
    scroll: raw.scroll,
  };
}

function fullCompact(manager, entry, raw, candidates, incremental = null) {
  const record = { documentGeneration: raw.document_generation, lines: [], text: '', frameLabels: new Map(), nextRef: 0, nextFrame: 0 };
  let nextRef = 0;
  for (const line of candidates.lines) { nextRef += 1; line.ref = 'e' + nextRef; }
  record.nextRef = nextRef;
  const shortByFullRef = new Map(candidates.lines.map((line) => [line.fullRef, line.ref]));
  const frames = compactFrames(raw, candidates.frameRefs, record, shortByFullRef);
  const result = compactBase(raw, 'compact');
  if (incremental) result.incremental = incremental;
  if (frames.length) result.frames = frames.map((frame) => frame.text).join('\n');
  result.elements = '';
  result.element_count = 0;
  result.elements_omitted = 0;
  result.text = '';
  result.text_truncated = raw.text_truncated === true;
  result.semantic_node_count = raw.semantic_node_count;
  const fixed = jsonBytes(result) + 64;
  let budget = MAX_SNAPSHOT_RESPONSE_BYTES - fixed;
  const displayed = candidates.lines.map((line) => ({ line, text: displayLine(line, record) }));
  const cost = (item) => jsonStringBytes(item.text) + 2;
  const keep = new Set();
  for (const item of displayed) {
    if (!item.line.interactive) continue;
    const itemCost = cost(item);
    if (itemCost > budget) continue;
    keep.add(item);
    budget -= itemCost;
  }
  const others = displayed.filter((item) => !item.line.interactive);
  const othersCost = others.reduce((sum, item) => sum + cost(item), 0);
  const outlineReserve = Math.min(othersCost, COMPACT_TEXT_RESERVE_FOR_OUTLINE, Math.max(0, budget));
  const text = fitJSONString(raw.text || '', Math.max(0, budget - outlineReserve));
  budget -= jsonStringBytes(text);
  if (utf8Bytes(text) < utf8Bytes(raw.text || '')) result.text_truncated = true;
  for (const item of others) {
    const itemCost = cost(item);
    if (itemCost > budget) continue;
    keep.add(item);
    budget -= itemCost;
  }
  const shown = displayed.filter((item) => keep.has(item));
  result.elements = shown.map((item) => item.text).join('\n');
  result.element_count = shown.length;
  result.elements_omitted = displayed.length - shown.length;
  result.text = text;
  while (jsonBytes(result) > MAX_SNAPSHOT_RESPONSE_BYTES && shown.length) {
    shown.pop();
    result.elements = shown.map((item) => item.text).join('\n');
    result.element_count = shown.length;
    result.elements_omitted = displayed.length - shown.length;
  }
  record.lines = shown.map((item) => item.line);
  record.text = text;
  record.framesText = result.frames || '';
  aliasRefs(candidates.observation, record.lines, record.frameLabels);
  rememberCompact(entry, raw.snapshot_id, record);
  return result;
}

function textChangeRuns(oldText, newText) {
  const before = textWords(oldText);
  const after = textWords(newText);
  const ops = diffSequences(before, after, MAX_DIFF_EDITS);
  if (!ops) return null;
  const runs = [];
  let current = null;
  for (const [op, oldIndex, newIndex] of ops) {
    if (op === '=') { current = null; continue; }
    const word = op === '+' ? after[newIndex] : before[oldIndex];
    if (!current || current.op !== op) {
      current = { op, words: [] };
      runs.push(current);
    }
    current.words.push(word);
  }
  return runs.map((run) => {
    let text = run.words.join(' ');
    if (text.length > MAX_TEXT_CHANGE_RUN_CHARS) text = text.slice(0, MAX_TEXT_CHANGE_RUN_CHARS) + ' (truncated)';
    return run.op + ' ' + quoted(text);
  });
}

function incrementalCompact(manager, entry, raw, candidates, base, since) {
  const keys = candidates.lines.map((line) => line.key);
  const ops = diffSequences(base.lines.map((line) => line.key), keys, MAX_DIFF_EDITS);
  if (!ops) {
    return fullCompact(manager, entry, raw, candidates, { applied: false, since_snapshot_id: since, reason: 'too_many_changes' });
  }
  const record = {
    documentGeneration: raw.document_generation, lines: [], text: '',
    frameLabels: new Map(base.frameLabels), nextRef: base.nextRef, nextFrame: base.nextFrame,
  };
  const removed = [];
  const added = [];
  const status = new Array(candidates.lines.length).fill('');
  for (const [op, oldIndex, newIndex] of ops) {
    if (op === '=') {
      candidates.lines[newIndex].ref = base.lines[oldIndex].ref;
      status[newIndex] = '=';
    } else if (op === '-') removed.push(base.lines[oldIndex]);
    else added.push(newIndex);
  }
  // A removed and an added line for the same element is a change of that
  // element: it keeps its ref.
  const removedByIdentity = new Map();
  for (const line of removed) {
    if (!removedByIdentity.has(line.identity)) removedByIdentity.set(line.identity, []);
    removedByIdentity.get(line.identity).push(line);
  }
  const reallyRemoved = new Set(removed);
  for (const newIndex of added) {
    const line = candidates.lines[newIndex];
    const previous = removedByIdentity.get(line.identity);
    if (previous && previous.length) {
      const old = previous.shift();
      reallyRemoved.delete(old);
      line.ref = old.ref;
      status[newIndex] = '~';
    } else {
      record.nextRef += 1;
      line.ref = 'e' + record.nextRef;
      status[newIndex] = '+';
    }
  }
  const shortByFullRef = new Map(candidates.lines.map((line) => [line.fullRef, line.ref]));
  const framesText = compactFrames(raw, candidates.frameRefs, record, shortByFullRef).map((frame) => frame.text).join('\n');
  record.framesText = framesText;
  const result = compactBase(raw, 'compact');
  result.base_snapshot_id = since;
  if (framesText !== base.framesText) result.frames = framesText;
  result.incremental = { applied: true, since_snapshot_id: since, unchanged: 0, added: 0, changed: 0, removed: 0, omitted: 0 };
  result.changes = '';
  const textRuns = (raw.text || '') === base.text ? [] : textChangeRuns(base.text, raw.text || '');
  result.text_unchanged = (raw.text || '') === base.text;
  if (!textRuns) result.text = '';
  else if (textRuns.length) result.text_changes = '';
  const fixed = jsonBytes(result) + 96;
  let budget = MAX_SNAPSHOT_RESPONSE_BYTES - fixed;
  const items = [];
  for (let index = 0; index < candidates.lines.length; index += 1) {
    if (status[index] === '+' || status[index] === '~') {
      const line = candidates.lines[index];
      items.push({ index, line, op: status[index], text: status[index] + ' ' + displayLine(line, record) });
    }
  }
  const removedItems = removed.filter((line) => reallyRemoved.has(line))
    .map((line) => ({ line, op: '-', text: '- ' + displayLine(line, record) }));
  const cost = (item) => jsonStringBytes(item.text) + 2;
  const keep = new Set();
  const take = (list) => {
    for (const item of list) {
      const itemCost = cost(item);
      if (itemCost > budget) continue;
      keep.add(item);
      budget -= itemCost;
    }
  };
  take(items.filter((item) => item.line.interactive));
  if (textRuns === null) {
    result.text = fitJSONString(raw.text || '', Math.max(0, budget - 2048));
    budget -= jsonStringBytes(result.text);
    result.text_truncated = utf8Bytes(result.text) < utf8Bytes(raw.text || '');
  } else if (textRuns.length) {
    const shownRuns = [];
    for (const run of textRuns) {
      const runCost = jsonStringBytes(run) + 2;
      if (runCost > budget - 2048) break;
      shownRuns.push(run);
      budget -= runCost;
    }
    result.text_changes = shownRuns.join('\n');
    if (shownRuns.length < textRuns.length) result.text_changes_omitted = textRuns.length - shownRuns.length;
  }
  take(items.filter((item) => !item.line.interactive));
  take(removedItems);
  const order = [...items, ...removedItems].filter((item) => keep.has(item));
  result.changes = order.map((item) => item.text).join('\n');
  const shownSet = new Set(order.map((item) => item.line));
  result.incremental.unchanged = status.filter((value) => value === '=').length;
  result.incremental.added = order.filter((item) => item.op === '+').length;
  result.incremental.changed = order.filter((item) => item.op === '~').length;
  result.incremental.removed = order.filter((item) => item.op === '-').length;
  result.incremental.omitted = items.length + removedItems.length - order.length;
  // What the agent has now seen, in the new document order: unchanged lines,
  // shown additions and changes, and the old form of a change that did not
  // fit (so it is reported again). Omitted additions are reported again too.
  const baseByRef = new Map(base.lines.map((line) => [line.ref, line]));
  record.lines = [];
  for (let index = 0; index < candidates.lines.length; index += 1) {
    const line = candidates.lines[index];
    if (status[index] === '=' || shownSet.has(line)) record.lines.push(line);
    else if (status[index] === '~') {
      const old = baseByRef.get(line.ref);
      if (old) record.lines.push({ ...line, key: old.key, body: old.body });
    }
  }
  record.text = textRuns === null ? result.text : (raw.text || '');
  aliasRefs(candidates.observation, record.lines, record.frameLabels);
  rememberCompact(entry, raw.snapshot_id, record);
  return result;
}

function renderCompactSnapshot(manager, entry, raw, { since = null } = {}) {
  const candidates = compactCandidates(manager, entry, raw);
  if (since == null) return fullCompact(manager, entry, raw, candidates);
  const base = compactState(entry).records.get(since);
  if (!base) {
    return fullCompact(manager, entry, raw, candidates, { applied: false, since_snapshot_id: since, reason: 'base_snapshot_unknown' });
  }
  if (base.documentGeneration !== raw.document_generation) {
    return fullCompact(manager, entry, raw, candidates, { applied: false, since_snapshot_id: since, reason: 'document_changed' });
  }
  return incrementalCompact(manager, entry, raw, candidates, base, since);
}

async function browserSnapshotForParams(manager, entry, params = {}) {
  const hasSince = params.sinceSnapshotId != null;
  if (hasSince && (typeof params.sinceSnapshotId !== 'string' || !params.sinceSnapshotId.trim())) {
    throw new Error('since_snapshot_id must be a non-empty string');
  }
  const format = params.format == null ? (hasSince ? 'compact' : 'json') : String(params.format);
  if (format !== 'json' && format !== 'compact') throw new Error('format must be json or compact');
  if (format === 'json') {
    if (hasSince) throw new Error('since_snapshot_id requires format compact');
    return manager.browserSnapshotSettled(entry);
  }
  const raw = await manager.browserSnapshotSettled(entry, { unbounded: true });
  return renderCompactSnapshot(manager, entry, raw, { since: hasSince ? params.sinceSnapshotId.trim() : null });
}

module.exports = {
  MAX_SNAPSHOT_RESPONSE_BYTES,
  NODE_FACT_KEYS,
  attachNodeFacts,
  browserSnapshotForParams,
  compactNodeBody,
  detachNodeFacts,
  diffSequences,
  dropSecretFieldAxRows,
  renderCompactSnapshot,
};
