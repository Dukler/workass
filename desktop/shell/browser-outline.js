'use strict';

// The page outline: what an agent reads to know what is on a page and what it
// can act on, as nested lines with short refs (e12) that click, type and the
// other actions take directly. Built from Chromium's own accessibility tree
// (CDP Accessibility.getFullAXTree) of every frame the tab owns, cross-site
// frames included, each placed under its <iframe>. Pure: the manager gathers
// the trees; this file renders, pages and diffs them.
//
//   page "Checkout" -> https://shop.example/checkout
//   - heading "Payment" level=2 [e3]
//   - textbox "Email" [e4] value="ada@example.com" required
//   - iframe "Card" [e5] -> https://pay.example/card
//     - textbox "Card number" [e6] value=""
//   - button "Pay" [e7] disabled

const { pageSecretField, redactPageMessage } = require('./browser-observation');

const MAX_LINES = 20000;
const NAME_LIMIT = 200;
const VALUE_LIMIT = 200;
const TEXT_LIMIT = 300;
const URL_LIMIT = 300;

// Roles that get a ref: what an action can target.
const REF_ROLES = new Set([
  'button', 'link', 'textbox', 'searchbox', 'combobox', 'listbox', 'option', 'checkbox', 'radio', 'switch',
  'slider', 'spinbutton', 'tab', 'menuitem', 'menuitemcheckbox', 'menuitemradio', 'treeitem', 'heading',
  'Iframe', 'IframePresentational', 'canvas', 'Canvas', 'image', 'img', 'textarea', 'ToggleButton', 'PopUpButton',
  'DisclosureTriangle', 'gridcell', 'columnheader', 'scrollbar', 'dialog', 'alertdialog', 'video', 'audio',
]);
const VALUE_ROLES = new Set(['textbox', 'searchbox', 'combobox', 'spinbutton', 'slider', 'textarea', 'ComboBoxMenuButton', 'PopUpButton', 'listbox', 'scrollbar', 'progressbar', 'meter']);
// Containers that add nothing when they have no name: their children move up.
const TRANSPARENT_ROLES = new Set(['generic', 'none', 'presentation', 'GenericContainer', 'Div', 'Section', 'LayoutTable', 'MenuListPopup', 'rowgroup',
  'LayoutTableRow', 'LayoutTableCell', 'Pre', 'Unknown', 'Ruby', 'Abbr', 'Mark', 'Time', 'Emphasis', 'Strong', 'Subscript',
  'Superscript', 'Code', 'Insertion', 'Deletion', 'Label', 'LabelText', 'Legend', 'Caption', 'Paragraph', 'paragraph',
  'group', 'Span', 'Inline', 'LineBreak', 'ListMarker', 'Search', 'FooterAsNonLandmark', 'HeaderAsNonLandmark', 'SectionWithoutName']);
const SKIP_ROLES = new Set(['InlineTextBox', 'LineBreak', 'ListMarker']);
// Fields whose value line says it all: their inner editor is not repeated.
const LEAF_FIELD_ROLES = new Set(['textbox', 'searchbox', 'spinbutton', 'slider', 'textarea', 'progressbar', 'meter', 'scrollbar']);
const TEXT_ROLES = new Set(['StaticText', 'text']);
const FRAME_ROLES = new Set(['Iframe', 'IframePresentational']);
const ROOT_ROLES = new Set(['RootWebArea', 'WebArea']);
const ROLE_ALIASES = new Map([['Iframe', 'iframe'], ['IframePresentational', 'iframe'], ['img', 'image'], ['ToggleButton', 'button'],
  ['PopUpButton', 'combobox'], ['ComboBoxMenuButton', 'combobox'], ['DisclosureTriangle', 'button'], ['LayoutTable', 'table'],
  ['Date', 'date'], ['DateTime', 'datetime'], ['InputTime', 'time'], ['ColorWell', 'color'], ['Canvas', 'canvas'], ['Video', 'video'], ['Audio', 'audio']]);

function clip(text, limit) {
  const value = String(text == null ? '' : text).replace(/\s+/gu, ' ').trim();
  return value.length > limit ? value.slice(0, limit - 1) + '…' : value;
}

function quote(text) {
  return '"' + String(text).replace(/\\/gu, '\\\\').replace(/"/gu, '\\"') + '"';
}

function axValue(field) {
  return field && Object.prototype.hasOwnProperty.call(field, 'value') ? field.value : undefined;
}

function propertiesOf(node) {
  const out = {};
  for (const property of Array.isArray(node.properties) ? node.properties : []) {
    if (property && typeof property.name === 'string') out[property.name] = axValue(property.value);
  }
  return out;
}

// A secret field's value: Chromium reports a password field's value as
// bullets; it is never returned.
function secretValue(value) {
  const text = String(value == null ? '' : value);
  return text.length > 0 && /^[•●*]+$/u.test(text);
}

// The same rule the json snapshot and read apply (browser-observation.js,
// pageSecretField): type, autocomplete, name and id of the field's element,
// given as CDP's flat [name, value, name, value, ...] attribute list.
function outlineSecretField(attributes) {
  const map = new Map();
  const list = Array.isArray(attributes) ? attributes : [];
  for (let index = 0; index + 1 < list.length; index += 2) map.set(String(list[index]).toLowerCase(), String(list[index + 1]));
  return pageSecretField({ getAttribute: (name) => (map.has(name) ? map.get(name) : null), type: map.get('type') || '', id: map.get('id') || '' });
}

function stateText(role, props, value, secret = false) {
  const parts = [];
  if (props.level != null && role === 'heading') parts.push(`level=${props.level}`);
  if (value !== undefined && value !== null && (VALUE_ROLES.has(role) || String(value) !== '')) {
    if (secret || secretValue(value) || props.protected === true) parts.push('value=[redacted]');
    else parts.push(`value=${quote(clip(redactPageMessage(String(value), VALUE_LIMIT * 2), VALUE_LIMIT))}`);
  }
  if (props.checked !== undefined && props.checked !== 'false' && props.checked !== false) parts.push(props.checked === 'mixed' ? 'checked=mixed' : 'checked');
  else if (props.checked === 'false' || props.checked === false) parts.push('unchecked');
  if (props.pressed !== undefined && props.pressed !== 'false' && props.pressed !== false) parts.push('pressed');
  if (props.selected === true) parts.push('selected');
  if (props.expanded === true) parts.push('expanded');
  else if (props.expanded === false) parts.push('collapsed');
  if (props.disabled === true) parts.push('disabled');
  if (props.readonly === true) parts.push('readonly');
  if (props.required === true) parts.push('required');
  if (props.invalid && props.invalid !== 'false') parts.push('invalid');
  if (props.focused === true) parts.push('focused');
  if (props.modal === true) parts.push('modal');
  if (props.hasPopup && props.hasPopup !== 'false' && role !== 'combobox') parts.push(`haspopup=${props.hasPopup}`);
  if (!secret && props.valuetext && VALUE_ROLES.has(role) && String(props.valuetext) !== String(value ?? '')) parts.push(`valuetext=${quote(clip(props.valuetext, 80))}`);
  return parts;
}

// frames: [{ frameKey, url, title, nodes, ownedBy: { frameKey, backendNodeId } | null,
//            unavailable: reason | null }] with the top frame first.
// refFor(frameKey, backendNodeId, role, name) -> 'e12'.
// Returns { lines: [{ depth, text, key, ref }], truncated }.
function buildOutline(frames, { refFor = () => null, title = '', url = '' } = {}) {
  const lines = [];
  let truncated = false;
  const push = (line) => {
    if (lines.length >= MAX_LINES) { truncated = true; return false; }
    lines.push(line);
    return true;
  };
  const top = frames[0];
  const childFrames = new Map();
  for (const frame of frames.slice(1)) {
    if (frame.ownedBy) childFrames.set(`${frame.ownedBy.frameKey}|${frame.ownedBy.backendNodeId}`, frame);
  }
  const placed = new Set();
  push({ depth: 0, key: 'page', ref: null, text: `page ${quote(clip(redactPageMessage(title || top?.title || ''), NAME_LIMIT))} -> ${clip(redactPageMessage(url || top?.url || ''), URL_LIMIT)}` });

  const renderFrame = (frame, depth, seen) => {
    if (!frame || seen.has(frame.frameKey)) return;
    seen.add(frame.frameKey);
    placed.add(frame.frameKey);
    if (frame.unavailable) {
      push({ depth, key: `${frame.frameKey}|unavailable`, ref: null, text: `- (frame not readable here: ${clip(frame.unavailable, 120)})` });
      return;
    }
    const nodes = Array.isArray(frame.nodes) ? frame.nodes : [];
    const byId = new Map(nodes.map((node) => [String(node.nodeId), node]));
    const root = nodes.find((node) => !node.parentId) || nodes[0];
    if (!root) return;
    const visit = (node, depthNow, parentKey, guard) => {
      if (!node || guard.has(node.nodeId) || truncated) return;
      guard.add(node.nodeId);
      const role = String(axValue(node.role) || '');
      const children = (Array.isArray(node.childIds) ? node.childIds : []).map((id) => byId.get(String(id))).filter(Boolean);
      if (node.ignored === true || SKIP_ROLES.has(role)) {
        if (!SKIP_ROLES.has(role)) renderChildren(children, depthNow, parentKey, guard);
        return;
      }
      if (ROOT_ROLES.has(role)) { renderChildren(children, depthNow, parentKey, guard); return; }
      const name = clip(redactPageMessage(String(axValue(node.name) || ''), NAME_LIMIT * 2), NAME_LIMIT);
      const props = propertiesOf(node);
      const backend = Number(node.backendDOMNodeId);
      const key = `${frame.frameKey}|${Number.isFinite(backend) && backend > 0 ? backend : 'ax' + node.nodeId}`;
      if (TEXT_ROLES.has(role)) {
        if (name && !secretValue(name)) push({ depth: depthNow, key, ref: null, text: `- text: ${clip(name, TEXT_LIMIT)}` });
        return;
      }
      const focusable = props.focusable === true && !TRANSPARENT_ROLES.has(role);
      if (TRANSPARENT_ROLES.has(role) && !name && !props.focusable && !props.editable) {
        renderChildren(children, depthNow, parentKey, guard);
        return;
      }
      const shown = ROLE_ALIASES.get(role) || (TRANSPARENT_ROLES.has(role) ? 'group' : role || 'generic');
      const value = axValue(node.value);
      const wantsRef = Number.isFinite(backend) && backend > 0 && (REF_ROLES.has(role) || focusable || props.editable);
      const ref = wantsRef ? refFor(frame.frameKey, backend, shown, name) : null;
      const parts = [`- ${shown}`];
      if (name) parts.push(quote(name));
      if (ref) parts.push(`[${ref}]`);
      const secret = !!(frame.secretBackendIds && frame.secretBackendIds.has(backend));
      parts.push(...stateText(role, props, VALUE_ROLES.has(role) || props.editable ? value : undefined, secret));
      if (props.url && (role === 'link' || FRAME_ROLES.has(role) || role === 'image' || role === 'img')) {
        parts.push(`-> ${clip(redactPageMessage(String(props.url), URL_LIMIT * 2), URL_LIMIT)}`);
      }
      if (shown === 'canvas') parts.push('(pixels only: text drawn on it cannot be read as text)');
      // A plain list item around one thing is that thing: the list line
      // above already says it is a list.
      if (role === 'listitem' && !name && !ref && parts.length === 1) {
        const inner = children.filter((child) => !SKIP_ROLES.has(String(axValue(child.role) || '')));
        if (inner.length === 1) { renderChildren(inner, depthNow, parentKey, guard); return; }
      }
      // A table row of plain cells is one line.
      if (role === 'row' && children.length && children.every((child) => ['cell', 'gridcell', 'columnheader', 'rowheader'].includes(String(axValue(child.role) || '')))) {
        const cells = children.map((child) => clip(redactPageMessage(String(axValue(child.name) || ''), 120), 120));
        const childRefs = children.filter((child) => String(axValue(child.role)) === 'gridcell' && Number(child.backendDOMNodeId) > 0)
          .map((child) => refFor(frame.frameKey, Number(child.backendDOMNodeId), 'gridcell', String(axValue(child.name) || ''))).filter(Boolean);
        push({ depth: depthNow, key, ref: null, text: `- row: ${cells.join(' | ')}${childRefs.length ? ` [${childRefs.join(' ')}]` : ''}` });
        return;
      }
      if (!push({ depth: depthNow, key, ref, text: parts.join(' ') })) return;
      if (FRAME_ROLES.has(role)) {
        const child = childFrames.get(key);
        if (child) renderFrame(child, depthNow + 1, seen);
        else push({ depth: depthNow + 1, key: `${key}|nf`, ref: null, text: '- (frame content not available)' });
        return;
      }
      if (LEAF_FIELD_ROLES.has(role)) return;
      // Nothing inside a secret field is shown either: an editable combobox's
      // inner text is its value, a select's chosen option is its value.
      if (secret) return;
      // A control named by its own text does not repeat that text below it.
      const nameCovers = (child) => TEXT_ROLES.has(String(axValue(child.role) || '')) && name && name.includes(clip(String(axValue(child.name) || ''), NAME_LIMIT));
      renderChildren(children.filter((child) => !nameCovers(child)), depthNow + 1, key, guard, name);
    };
    // A label's text right before the control it names is said once, by the
    // control.
    const labelOf = (node) => {
      for (let current = node, hops = 0; current && hops < 4; hops += 1) {
        const role = String(axValue(current.role) || '');
        if (TEXT_ROLES.has(role)) return clip(String(axValue(current.name) || ''), NAME_LIMIT).replace(/[:*\s]+$/u, '');
        if (!(current.ignored === true || TRANSPARENT_ROLES.has(role))) return '';
        const kids = (Array.isArray(current.childIds) ? current.childIds : []).map((id) => byId.get(String(id)))
          .filter((kid) => kid && !SKIP_ROLES.has(String(axValue(kid.role) || '')));
        current = kids.length === 1 ? kids[0] : null;
      }
      return '';
    };
    const namedBy = (node, label) => {
      for (let current = node, hops = 0; current && hops < 4; hops += 1) {
        const role = String(axValue(current.role) || '');
        if (TEXT_ROLES.has(role)) return false;
        const name = clip(String(axValue(current.name) || ''), NAME_LIMIT);
        if (name && (name === label || name.startsWith(label))) return !TRANSPARENT_ROLES.has(role) || current.ignored !== true;
        const kids = (Array.isArray(current.childIds) ? current.childIds : []).map((id) => byId.get(String(id))).filter(Boolean);
        current = kids.length === 1 ? kids[0] : null;
      }
      return false;
    };
    const renderChildren = (children, depthNow, parentKey, guard, parentName = '') => {
      for (let index = 0; index < children.length; index += 1) {
        const label = labelOf(children[index]);
        if (label && index + 1 < children.length && namedBy(children[index + 1], label)) continue;
        // A group's legend or a table's caption is its name already.
        if (label && parentName && (label === parentName || parentName.startsWith(label))) continue;
        const childRole = String(axValue(children[index].role) || '');
        if ((childRole === 'caption' || childRole === 'legend') && parentName &&
            clip(String(axValue(children[index].name) || ''), NAME_LIMIT) === parentName) continue;
        visit(children[index], depthNow, parentKey, guard);
      }
    };
    visit(root, depth, frame.frameKey, new Set());
  };
  if (top) renderFrame(top, 0, new Set());
  // Frames whose <iframe> was not found in their parent's tree still show.
  for (const frame of frames.slice(1)) {
    if (!placed.has(frame.frameKey) && !truncated) {
      push({ depth: 0, key: `${frame.frameKey}|orphan`, ref: null, text: `- iframe -> ${clip(redactPageMessage(frame.url || ''), URL_LIMIT)} (placement unknown)` });
      renderFrame(frame, 1, new Set());
    }
  }
  return { lines, truncated };
}

function renderLine(line) {
  return '  '.repeat(Math.max(0, line.depth)) + line.text;
}

// Folding: a run of at least FOLD_MIN sibling leaf lines that differ only in
// counting numbers (each number constant or stepping the same amount line
// by line) is shown as one line with {first..last} ranges, or
// {first..last..step}. Expanding the ranges gives back every line exactly,
// refs included, so nothing is lost: `- link "Item {1..2000}" [e{2..2001}]`
// is 2000 links, e2 to e2001.
const FOLD_MIN = 4;
const FOLD_NUMBER_DIGITS = 12;

function numberTokens(text) {
  if (text.includes('{') || text.includes('}')) return null;
  const parts = text.split(/(\d+)/u);
  for (let i = 1; i < parts.length; i += 2) if (parts[i].length > FOLD_NUMBER_DIGITS) return null;
  return parts;
}

function isLeaf(lines, index) {
  return index + 1 >= lines.length || lines[index + 1].depth <= lines[index].depth;
}

// The run of foldable lines starting at index: { end (exclusive), text }, or
// null when fewer than FOLD_MIN lines fold.
function foldRun(lines, index) {
  const first = lines[index];
  if (!isLeaf(lines, index)) return null;
  const base = numberTokens(first.text);
  if (!base || base.length < 3) return null;
  const steps = new Array(base.length).fill(null);
  const widthFixed = new Array(base.length).fill(true);
  const values = (parts) => parts.map((part, i) => (i % 2 ? Number(part) : part));
  let previous = values(base);
  let end = index + 1;
  for (; end < lines.length; end += 1) {
    const line = lines[end];
    if (line.depth !== first.depth || !isLeaf(lines, end)) break;
    const parts = numberTokens(line.text);
    if (!parts || parts.length !== base.length) break;
    const current = values(parts);
    let fits = true;
    for (let i = 0; i < base.length && fits; i += 1) {
      if (i % 2 === 0) { fits = parts[i] === base[i]; continue; }
      const step = current[i] - previous[i];
      if (steps[i] == null && end === index + 1) steps[i] = step;
      fits = step === steps[i] && step >= 0 && (step === 0 ? parts[i] === base[i]
        : parts[i].length === base[i].length || (String(current[i]) === parts[i] && String(Number(base[i])) === base[i]));
      if (parts[i].length !== base[i].length) widthFixed[i] = false;
    }
    if (!fits) break;
    previous = current;
  }
  if (end - index < FOLD_MIN || !steps.some((step) => step > 0)) return null;
  const last = numberTokens(lines[end - 1].text);
  const text = base.map((part, i) => {
    if (i % 2 === 0 || !steps[i]) return part;
    // Zero-padded numbers keep their width; others are plain integers.
    const from = widthFixed[i] ? part : String(Number(part));
    const to = widthFixed[i] ? last[i] : String(Number(last[i]));
    return `{${from}..${to}${steps[i] === 1 ? '' : `..${steps[i]}`}}`;
  }).join('');
  return { end, text };
}

// One page of lines, whole lines (and whole folded runs) only, at most
// maxBytes. `next` is the line index to continue from.
function pageLines(lines, start, maxBytes) {
  const out = [];
  let bytes = 0;
  let index = Math.max(0, start);
  while (index < lines.length) {
    const run = foldRun(lines, index);
    const text = '  '.repeat(Math.max(0, lines[index].depth)) + (run ? run.text : lines[index].text);
    const size = Buffer.byteLength(text, 'utf8') + 1;
    if (out.length && bytes + size > maxBytes) break;
    out.push(text);
    bytes += size;
    index = run ? run.end : index + 1;
  }
  return { text: out.join('\n'), next: index < lines.length ? index : null, bytes };
}

// What changed between two outlines of a page: lines added (+), removed (-)
// and changed (~, with what it was), in page order, bounded.
function diffOutlines(before, after, { maxBytes = 6000 } = {}) {
  const oldByKey = new Map();
  for (const line of before) if (!oldByKey.has(line.key)) oldByKey.set(line.key, line);
  const newKeys = new Set(after.map((line) => line.key));
  const changes = [];
  let added = 0; let removed = 0; let changed = 0;
  for (const line of after) {
    const old = oldByKey.get(line.key);
    if (!old) { added += 1; changes.push({ kind: '+', line }); }
    else if (old.text !== line.text) { changed += 1; changes.push({ kind: '~', line, was: old }); }
  }
  for (const line of before) {
    if (!newKeys.has(line.key)) { removed += 1; changes.push({ kind: '-', line }); }
  }
  const out = [];
  let bytes = 0;
  let omitted = 0;
  for (const change of changes) {
    const body = renderLine(change.line).trimStart();
    const text = change.kind === '~'
      ? `~ ${body}  (was: ${clip(change.was.text.replace(/^- /u, ''), 160)})`
      : `${change.kind} ${body}`;
    const size = Buffer.byteLength(text, 'utf8') + 1;
    if (bytes + size > maxBytes) { omitted += 1; continue; }
    out.push(text);
    bytes += size;
  }
  return { added, removed, changed, text: out.join('\n'), omitted, total: before.length };
}

module.exports = { buildOutline, diffOutlines, outlineSecretField, pageLines, renderLine, secretValue };
