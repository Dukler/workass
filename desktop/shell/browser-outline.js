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

const { redactPageMessage } = require('./browser-observation');

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

function stateText(role, props, value) {
  const parts = [];
  if (props.level != null && role === 'heading') parts.push(`level=${props.level}`);
  if (value !== undefined && value !== null && (VALUE_ROLES.has(role) || String(value) !== '')) {
    if (secretValue(value) || props.protected === true) parts.push('value=[redacted]');
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
  if (props.valuetext && VALUE_ROLES.has(role) && String(props.valuetext) !== String(value ?? '')) parts.push(`valuetext=${quote(clip(props.valuetext, 80))}`);
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
      parts.push(...stateText(role, props, VALUE_ROLES.has(role) || props.editable ? value : undefined));
      if (props.url && (role === 'link' || FRAME_ROLES.has(role) || role === 'image' || role === 'img')) {
        parts.push(`-> ${clip(redactPageMessage(String(props.url), URL_LIMIT * 2), URL_LIMIT)}`);
      }
      if (shown === 'canvas') parts.push('(pixels only: text drawn on it cannot be read as text)');
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

// One page of lines, whole lines only, at most maxBytes.
function pageLines(lines, start, maxBytes) {
  const out = [];
  let bytes = 0;
  let index = Math.max(0, start);
  for (; index < lines.length; index += 1) {
    const text = renderLine(lines[index]);
    const size = Buffer.byteLength(text, 'utf8') + 1;
    if (out.length && bytes + size > maxBytes) break;
    out.push(text);
    bytes += size;
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

module.exports = { buildOutline, diffOutlines, pageLines, renderLine, secretValue };
