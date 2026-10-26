'use strict';

// workass_browser_read: the rendered page (or one frame, element or selector
// match) as markdown, paged by byte offset. Each frame is read in its own
// document; cross-site frames go through the owned frame sessions with the same
// bounded budget as snapshots, and are stitched back where their iframe sits.

const crypto = require('node:crypto');
const {
  PAGE_FACT_HELPERS, redactPageMessage, redactPageValue, targetExpression,
} = require('./browser-observation');

const READ_DEFAULT_BYTES = 32 * 1024;
const READ_MIN_BYTES = 2048;
const READ_MAX_BYTES = 64 * 1024;
const READ_FRAME_LIMIT = 40;
const READ_CHILD_FRAMES_BUDGET_MS = 8000;
const READ_MAX_ELEMENTS = 200000;
const READ_MAX_CHARS = 8 * 1024 * 1024;
const READ_CACHE_TTL_MS = 10 * 60 * 1000;
const READ_FRAME_SETUP_WAIT_MS = 1500;
const STRUCTURED_INLINE_BYTES = 8 * 1024;
const FRAME_MARKER = '\u0000WKFRAME:';
const FRAME_MARKER_END = '\u0000';
const FRAME_MARKER_RE = /\u0000WKFRAME:([^\u0000]{1,200})\u0000/g;

// Runs inside the page. Uses only its arguments, PAGE_FACT_HELPERS and page
// globals; returns markdown plus frame placements and structured data.
function pageReadDocument(config, scopedTarget) {
  const STOP = { stop: true };
  const doc = document;
  const view = doc.defaultView || window;
  const base = doc.baseURI || (typeof location === 'object' && location ? location.href : '');
  const maxElements = Number(config.maxElements) || 200000;
  const maxChars = Number(config.maxChars) || 8 * 1024 * 1024;
  const nonce = String(config.nonce || '');
  const frameTokens = globalThis[Symbol.for('workass.browser.read-frames')] || null;
  const frameMarker = String(config.frameMarker || '');
  const frameMarkerEnd = String(config.frameMarkerEnd || '');
  let elements = 0;
  let totalChars = 0;
  let stopped = '';
  const placed = [];
  const SKIP = new Set(['script', 'style', 'noscript', 'template', 'head', 'meta', 'link', 'title', 'base',
    'svg', 'math', 'canvas', 'object', 'embed', 'map', 'param', 'source', 'track', 'datalist', 'area']);
  const tagOf = (node) => String(node && node.tagName || '').toLowerCase();
  const attr = (node, name) => {
    try { const value = node.getAttribute(name); return value == null ? '' : String(value); } catch { return ''; }
  };
  const collapse = (value) => String(value == null ? '' : value).replace(/\s+/g, ' ').trim();
  const escapeLabel = (value) => collapse(value).replace(/\\/g, '\\\\').replace(/\[/g, '\\[').replace(/\]/g, '\\]');
  const escapeURL = (value) => String(value).replace(/\s/g, '%20').replace(/\(/g, '%28').replace(/\)/g, '%29');
  const styleOf = (node) => { try { return view.getComputedStyle(node); } catch { return null; } };

  const newWriter = (flat) => ({ parts: [], pendingBreak: 0, pendingSpace: false, lineStart: true, indents: [], marker: '', flat, empty: true });
  let w = newWriter(false);
  const push = (text) => {
    w.parts.push(text);
    w.empty = false;
    totalChars += text.length;
    if (totalChars > maxChars) { stopped = 'output_limit'; throw STOP; }
  };
  const emit = (text) => {
    if (!text) return;
    if (w.flat) {
      if (!w.empty && w.pendingBreak) push(' · ');
      else if (!w.empty && w.pendingSpace && !softSpaceSkipped(text)) push(' ');
      w.pendingBreak = 0; w.pendingSpace = false;
      push(text);
      return;
    }
    if (w.pendingBreak) {
      if (!w.empty) push(w.pendingBreak >= 2 ? '\n\n' : '\n');
      w.pendingBreak = 0; w.lineStart = true; w.pendingSpace = false;
    }
    if (w.lineStart) {
      if (w.marker) { push(w.indents.slice(0, -1).join('') + w.marker); w.marker = ''; }
      else if (w.indents.length) push(w.indents.join(''));
      w.lineStart = false;
    } else if (w.pendingSpace && !softSpaceSkipped(text)) push(' ');
    w.pendingSpace = false;
    push(text);
  };
  const emitBlock = (text) => {
    // Multi-line preformatted text, indented with the current context.
    if (w.flat) { emit(collapse(text)); return; }
    const lines = String(text).split('\n');
    emit(lines[0]);
    const indent = w.indents.join('');
    for (let index = 1; index < lines.length; index += 1) push('\n' + indent + lines[index]);
  };
  const space = () => { w.pendingSpace = true; };
  // Images, fields, buttons, frames and links are separate tokens even when
  // the page renders them with no whitespace in between.
  const emitToken = (text) => { if (!text) return; space(); emit(text); if (!w.pendingSpace) w.pendingSpace = 'soft'; };
  const softSpaceSkipped = (text) => w.pendingSpace === 'soft' && /^[.,;:!?)\]}%]/.test(text);
  const brk = (count) => {
    if (w.flat) { w.pendingBreak = Math.max(w.pendingBreak, 1); return; }
    w.pendingBreak = Math.max(w.pendingBreak, count);
  };
  const boundary = (ctx, count) => { if (ctx.inlineChildren) space(); else brk(count); };
  const flatten = (node, ctx, extra) => {
    const saved = w;
    const writer = newWriter(true);
    w = writer;
    try { walkChildren(node, { ...ctx, ...extra, inlineChildren: false }); }
    finally { w = saved; }
    return collapse(writer.parts.join(''));
  };

  const isBlockDisplay = (display) => !!display && !/^inline/.test(display) && display !== 'contents' && display !== 'ruby' && display !== 'none';
  const visibleElement = (node) => {
    const style = styleOf(node);
    if (style && (style.display === 'none' || Number(style.opacity) === 0)) return false;
    if (typeof node.checkVisibility === 'function' && (!style || style.display !== 'contents')) {
      try { if (!node.checkVisibility()) return false; } catch { /* treated as visible */ }
    }
    return true;
  };
  // A label's own words, without the text of the controls it wraps (a
  // wrapped select would otherwise lend the label all its option texts).
  const labelText = (label) => {
    const parts = [];
    const visitLabel = (current) => {
      for (const child of Array.from(current.childNodes || [])) {
        if (child.nodeType === 3) parts.push(String(child.nodeValue || ''));
        else if (child.nodeType === 1 && !['select', 'textarea', 'input', 'button', 'datalist', 'script', 'style'].includes(tagOf(child))) visitLabel(child);
      }
    };
    visitLabel(label);
    return parts.join(' ');
  };
  const accessibleName = (node) => {
    const labelled = attr(node, 'aria-labelledby');
    if (labelled) {
      const text = labelled.split(/\s+/).map((id) => {
        try { const found = (node.getRootNode && node.getRootNode().getElementById ? node.getRootNode().getElementById(id) : null) || doc.getElementById(id); return found ? (found.innerText || found.textContent || '') : ''; } catch { return ''; }
      }).join(' ');
      if (collapse(text)) return collapse(text);
    }
    const aria = collapse(attr(node, 'aria-label'));
    if (aria) return aria;
    try {
      if (node.labels && node.labels.length) {
        const text = collapse(Array.from(node.labels).map((label) => labelText(label)).join(' '));
        if (text) return text;
      }
    } catch { /* no label list */ }
    return collapse(attr(node, 'placeholder') || attr(node, 'title') || attr(node, 'name'));
  };

  const walkChildren = (parent, ctx) => {
    let children = null;
    try {
      if (parent.shadowRoot && parent.shadowRoot.childNodes) children = parent.shadowRoot.childNodes;
      else if (tagOf(parent) === 'slot' && typeof parent.assignedNodes === 'function') {
        const assigned = parent.assignedNodes({ flatten: true });
        children = assigned && assigned.length ? assigned : parent.childNodes;
      } else children = parent.childNodes;
    } catch { children = null; }
    if (!children) return;
    for (let index = 0; index < children.length; index += 1) walk(children[index], ctx);
  };

  const handleText = (node, ctx) => {
    if (!ctx.textVisible) return;
    const raw = String(node.nodeValue || '');
    if (!raw) return;
    if (/^\s/.test(raw)) space();
    let text = collapse(raw);
    // Inside a link label, brackets would end the label early.
    if (ctx.inLink) text = text.replace(/\\/g, '\\\\').replace(/\[/g, '\\[').replace(/\]/g, '\\]');
    if (text) emit(text);
    if (text && /\s$/.test(raw)) space();
  };

  const handleLink = (node, ctx) => {
    const raw = node.getAttribute('href') != null ? node.getAttribute('href') : node.getAttribute('xlink:href');
    const href = raw != null ? pageAbsoluteURL(raw, base) : '';
    if (!href || /^(?:javascript|vbscript|data):$/.test(href) || ctx.inLink) { walkChildren(node, ctx); return; }
    let text = flatten(node, ctx, { inLink: true });
    if (!text) text = escapeLabel(attr(node, 'aria-label') || attr(node, 'title'));
    if (text.length > 600) {
      // A link around a whole block: keep the content, then the target.
      walkChildren(node, ctx);
      space();
      emitToken('[(link)](' + escapeURL(href) + ')');
      return;
    }
    // A link title that says more than the (often truncated) text rides along
    // as the markdown link title.
    const title = collapse(attr(node, 'title'));
    const plain = text.replace(/\\([\\[\]])/g, '$1');
    const titled = title && title !== plain && !plain.includes(title) ? ' "' + title.replace(/"/g, '\\"') + '"' : '';
    emitToken('[' + text + '](' + escapeURL(href) + titled + ')');
  };

  const handleImage = (node) => {
    const alt = collapse(attr(node, 'alt'));
    let rect = null;
    try { rect = node.getBoundingClientRect(); } catch { rect = null; }
    const width = rect ? Number(rect.width) || 0 : 0;
    const height = rect ? Number(rect.height) || 0 : 0;
    if (!alt && (width < 100 || height < 100)) return;
    if (alt && rect && width < 2 && height < 2) return;
    const src = pageImageSource(node, base);
    if (!src || src === 'data:') { if (alt) emitToken('[Image: ' + escapeLabel(alt) + ']'); return; }
    emitToken('![' + escapeLabel(alt) + '](' + escapeURL(src) + ')');
  };

  const handleMedia = (node, kind) => {
    const src = pageImageSource(node, base);
    const label = collapse(attr(node, 'title') || attr(node, 'aria-label'));
    if (!src || src === 'data:') { if (label) emitToken('[' + kind + ': ' + escapeLabel(label) + ']'); return; }
    emitToken('[' + kind + (label ? ': ' + escapeLabel(label) : '') + '](' + escapeURL(src) + ')');
  };

  const handleFrame = (node) => {
    const token = frameTokens ? frameTokens.get(node) : null;
    const label = collapse(attr(node, 'title') || attr(node, 'name') || attr(node, 'aria-label'));
    if (token && nonce && String(token).startsWith(nonce + ':')) {
      placed.push(String(token));
      if (w.flat) { emitToken('[Frame' + (label ? ': ' + escapeLabel(label) : '') + ']'); return; }
      brk(2);
      emit(frameMarker + String(token) + frameMarkerEnd);
      brk(2);
      return;
    }
    const src = pageAbsoluteURL(attr(node, 'src'), base);
    if (!label && (!src || src === 'about:blank')) return;
    emitToken('[Frame' + (label ? ': ' + escapeLabel(label) : '') + (src && src !== 'about:blank' ? ' ' + src : '') + ']');
  };

  const cellText = (cell, ctx) => flatten(cell, ctx, {}).replace(/\|/g, '\\|');
  const handleTable = (node, ctx) => {
    if (w.flat) { walkChildren(node, ctx); return; }
    let rows = [];
    try { rows = Array.from(node.rows || []); } catch { rows = []; }
    rows = rows.filter((row) => visibleElement(row));
    const role = attr(node, 'role').toLowerCase();
    let nested = false;
    try { nested = typeof node.querySelector === 'function' && !!node.querySelector('table'); } catch { nested = false; }
    const widths = rows.map((row) => { try { return Array.from(row.cells || []).length; } catch { return 0; } });
    const maxCells = widths.length ? Math.max(...widths) : 0;
    if (role === 'presentation' || role === 'none' || nested || !rows.length || maxCells < 2) {
      brk(1); walkChildren(node, ctx); brk(1);
      return;
    }
    const matrix = [];
    let headerRow = false;
    for (let index = 0; index < rows.length; index += 1) {
      const row = rows[index];
      const cells = [];
      let allHeaders = true;
      for (const cell of Array.from(row.cells || [])) {
        if (!visibleElement(cell)) continue;
        if (tagOf(cell) !== 'th') allHeaders = false;
        cells.push(cellText(cell, ctx));
        const span = Math.min(30, Math.max(1, Number(cell.colSpan) || Number(attr(cell, 'colspan')) || 1));
        for (let extra = 1; extra < span; extra += 1) cells.push('');
      }
      if (index === 0) headerRow = allHeaders || tagOf(row.parentElement) === 'thead';
      matrix.push(cells);
    }
    const columns = Math.min(30, Math.max(...matrix.map((cells) => cells.length), 1));
    const line = (cells) => '| ' + Array.from({ length: columns }, (_value, index) => cells[index] || '').join(' | ') + ' |';
    const lines = [];
    let caption = '';
    try { caption = node.caption ? collapse(node.caption.innerText || node.caption.textContent || '') : ''; } catch { caption = ''; }
    if (caption) lines.push('Table: ' + caption);
    const header = headerRow ? matrix.shift() : Array.from({ length: columns }, () => '');
    lines.push(line(header));
    lines.push('|' + Array.from({ length: columns }, () => ' --- |').join(''));
    for (const cells of matrix) lines.push(line(cells));
    brk(2);
    emitBlock(lines.join('\n'));
    brk(2);
  };

  const fieldValue = (node) => {
    const facts = pageFieldFacts(node);
    if (!facts) return null;
    if (facts.valueRedacted) return { text: '[redacted]' };
    if (facts.value) return { text: '"' + facts.value.replace(/\s+/g, ' ') + '"' + (facts.valueTruncated ? ' (truncated)' : '') };
    return { text: '', placeholder: facts.placeholder || '' };
  };
  const handleField = (node, tag, role, ctx) => {
    const type = String(attr(node, 'type') || node.type || '').toLowerCase();
    if (tag === 'input') {
      if (type === 'hidden') return true;
      const name = accessibleName(node);
      let hasLabel = false;
      try { hasLabel = !!(node.labels && node.labels.length); } catch { hasLabel = false; }
      if (type === 'checkbox' || type === 'radio') {
        const mark = type === 'radio' ? (node.checked ? '(x)' : '( )') : (node.checked ? '[x]' : '[ ]');
        emitToken(mark + (!hasLabel && name ? ' ' + escapeLabel(name) : ''));
        return true;
      }
      if (type === 'submit' || type === 'button' || type === 'reset' || type === 'image') {
        const label = collapse(node.value || attr(node, 'value') || attr(node, 'alt') || name || (type === 'submit' ? 'Submit' : ''));
        emitToken('[Button: ' + escapeLabel(label) + ']');
        return true;
      }
      if (type === 'file') { emitToken('[File input' + (name ? ' "' + name + '"' : '') + ']'); return true; }
      const value = fieldValue(node);
      const kind = type === 'password' ? 'Password field' : 'Input';
      let text = '[' + kind + (name ? ' "' + name + '"' : '') + (type && type !== 'text' && type !== 'password' ? ' (' + type + ')' : '');
      if (value && value.text) text += ' = ' + value.text;
      else text += ', empty' + (value && value.placeholder && value.placeholder !== name ? ', placeholder "' + value.placeholder + '"' : '');
      emitToken(text + ']');
      return true;
    }
    if (tag === 'textarea') {
      const name = accessibleName(node);
      const value = fieldValue(node);
      emitToken('[Text area' + (name ? ' "' + name + '"' : '') + (value && value.text ? ' = ' + value.text : ', empty') + ']');
      return true;
    }
    if (tag === 'select') {
      const name = accessibleName(node);
      const value = fieldValue(node);
      let options = [];
      try { options = Array.from(node.options || []).map((option) => collapse(option.label || option.textContent || '')).filter(Boolean); } catch { options = []; }
      const shown = options.slice(0, 50).map((option) => option.replace(/\|/g, '/'));
      emitToken('[Select' + (name ? ' "' + name + '"' : '') + (value && value.text ? ' = ' + value.text : '') +
        (shown.length ? '; options: ' + shown.join(' | ') + (options.length > shown.length ? ' (+' + (options.length - shown.length) + ' more)' : '') : '') + ']');
      return true;
    }
    if (tag === 'button' || role === 'button') {
      // aria-label names the button over its content (often only an icon).
      const label = escapeLabel(attr(node, 'aria-label')) || flatten(node, ctx, {}) || accessibleName(node);
      if (label) emitToken('[Button: ' + label.replace(/\]/g, '\\]') + ']');
      return true;
    }
    if (role === 'checkbox' || role === 'switch' || role === 'radio') {
      const checked = attr(node, 'aria-checked') === 'true';
      const mark = role === 'radio' ? (checked ? '(x)' : '( )') : (checked ? '[x]' : '[ ]');
      const label = flatten(node, ctx, {}) || accessibleName(node);
      emitToken(mark + (label ? ' ' + label : ''));
      return true;
    }
    return false;
  };

  const walk = (node, ctx) => {
    if (!node) return;
    if (node.nodeType === 3) { handleText(node, ctx); return; }
    if (node.nodeType === 11 || node.nodeType === 9) { walkChildren(node, ctx); return; }
    if (node.nodeType !== 1) return;
    if (ctx.skip && node === ctx.skip) return;
    elements += 1;
    if (elements > maxElements) { stopped = 'element_limit'; throw STOP; }
    const tag = tagOf(node);
    if (SKIP.has(tag)) return;
    if (tag === 'input' && String(attr(node, 'type')).toLowerCase() === 'hidden') return;
    const style = styleOf(node);
    const display = style ? String(style.display || '') : 'inline';
    if (display === 'none') return;
    if (style && style.opacity !== undefined && style.opacity !== '' && Number(style.opacity) === 0) return;
    if (display !== 'contents' && typeof node.checkVisibility === 'function') {
      try { if (!node.checkVisibility()) return; } catch { /* treated as visible */ }
    }
    const textVisible = !style || (style.visibility !== 'hidden' && style.visibility !== 'collapse');
    const role = attr(node, 'role').trim().split(/\s+/)[0].toLowerCase();
    const block = isBlockDisplay(display);
    const rowFlex = (display === 'flex' || display === 'inline-flex') && !/column/.test(String(style && style.flexDirection || ''));
    const childCtx = { ...ctx, textVisible, inlineChildren: rowFlex };
    const open = () => { if (block) boundary(ctx, 1); };
    const close = () => { if (block) boundary(ctx, 1); };

    if (/^h[1-6]$/.test(tag) || role === 'heading') {
      const level = Math.max(1, Math.min(6, Number(attr(node, 'aria-level')) || Number(tag.slice(1)) || 2));
      if (w.flat) { walkChildren(node, childCtx); return; }
      const text = flatten(node, childCtx, {});
      if (!text) return;
      brk(2); emit('#'.repeat(level) + ' ' + text); brk(2);
      return;
    }
    if (tag === 'br') { if (w.flat) space(); else brk(1); return; }
    if ((tag === 's' || tag === 'del' || tag === 'strike') && textVisible) {
      const text = flatten(node, childCtx, {});
      if (text) emitToken('~~' + text + '~~');
      return;
    }
    if (tag === 'hr') { if (!w.flat) { brk(2); emit('---'); brk(2); } return; }
    if (tag === 'a') { open(); handleLink(node, childCtx); close(); return; }
    if (tag === 'img') { handleImage(node); return; }
    if (tag === 'video' || tag === 'audio') { open(); handleMedia(node, tag === 'video' ? 'Video' : 'Audio'); close(); return; }
    if (tag === 'iframe' || tag === 'frame') { handleFrame(node); return; }
    if (tag === 'table') { handleTable(node, childCtx); return; }
    if (tag === 'pre') {
      const text = String(node.innerText != null ? node.innerText : node.textContent || '').replace(/\n+$/, '');
      if (!text.trim() || !textVisible) return;
      if (w.flat) { emit(collapse(text)); return; }
      const fence = text.includes('```') ? '~~~' : '```';
      brk(2); emitBlock(fence + '\n' + text + '\n' + fence); brk(2);
      return;
    }
    if ((tag === 'code' || tag === 'kbd' || tag === 'samp') && !ctx.pre) {
      const text = collapse(node.textContent || '');
      if (text && textVisible) emit(text.includes('`') ? '`` ' + text + ' ``' : '`' + text + '`');
      return;
    }
    if (['input', 'textarea', 'select', 'button'].includes(tag) || role === 'button' || role === 'checkbox' || role === 'switch' || role === 'radio') {
      if (!textVisible) { walkChildren(node, childCtx); return; }
      open();
      if (!handleField(node, tag, role, childCtx)) walkChildren(node, childCtx);
      close();
      return;
    }
    if (tag === 'ul' || tag === 'ol' || tag === 'menu') {
      if (w.flat) { walkChildren(node, { ...childCtx, list: { ordered: false, next: 1 } }); return; }
      brk(ctx.list ? 1 : 2);
      const start = Number(attr(node, 'start')) || 1;
      walkChildren(node, { ...childCtx, inlineChildren: false, list: { ordered: tag === 'ol', next: start } });
      brk(ctx.list ? 1 : 2);
      return;
    }
    if (tag === 'li') {
      if (w.flat) { brk(1); walkChildren(node, childCtx); brk(1); return; }
      const list = ctx.list || { ordered: false, next: 1 };
      const explicit = Number(attr(node, 'value'));
      if (Number.isInteger(explicit) && explicit > 0) list.next = explicit;
      const marker = list.ordered ? list.next + '. ' : '- ';
      list.next += 1;
      brk(1);
      w.marker = marker;
      w.indents.push(' '.repeat(marker.length));
      try { walkChildren(node, { ...childCtx, inlineChildren: false }); }
      finally { w.indents.pop(); w.marker = ''; }
      brk(1);
      return;
    }
    if (tag === 'dt') { brk(1); walkChildren(node, childCtx); brk(1); return; }
    if (tag === 'dd') {
      if (w.flat) { walkChildren(node, childCtx); return; }
      brk(1); w.indents.push('  ');
      try { walkChildren(node, childCtx); } finally { w.indents.pop(); }
      brk(1);
      return;
    }
    if (tag === 'blockquote') {
      if (w.flat) { walkChildren(node, childCtx); return; }
      brk(2); w.indents.push('> ');
      try { walkChildren(node, childCtx); } finally { w.indents.pop(); }
      brk(2);
      return;
    }
    if (tag === 'p') { boundary(ctx, 2); walkChildren(node, childCtx); boundary(ctx, 2); return; }
    if (tag === 'form' && !w.flat) {
      let action = '';
      try { action = pageAbsoluteURL(attr(node, 'action') || base, base); } catch { action = ''; }
      const method = (attr(node, 'method') || 'get').toUpperCase();
      brk(1); emit('[Form: ' + method + (action ? ' ' + action : '') + ']'); brk(1);
      walkChildren(node, childCtx);
      brk(1);
      return;
    }
    open();
    walkChildren(node, childCtx);
    close();
  };

  const render = (roots, skip) => {
    const saved = w;
    const writer = newWriter(false);
    w = writer;
    try {
      for (const root of roots) {
        brk(2);
        walk(root, { textVisible: true, inlineChildren: false, skip: skip || null, list: null, inLink: false });
      }
    } catch (error) {
      if (error !== STOP) throw error;
    } finally {
      w = saved;
    }
    return writer.parts.join('').replace(/[ \t]+\n/g, '\n').replace(/\n{3,}/g, '\n\n').trim();
  };

  const microValue = (node) => {
    const tag = tagOf(node);
    if (node.hasAttribute && node.hasAttribute('content')) return attr(node, 'content');
    if (['a', 'link', 'area'].includes(tag)) return pageAbsoluteURL(attr(node, 'href'), base);
    if (['img', 'audio', 'video', 'source', 'iframe', 'embed', 'track'].includes(tag)) return pageAbsoluteURL(attr(node, 'src'), base);
    if (tag === 'time' && attr(node, 'datetime')) return attr(node, 'datetime');
    if ((tag === 'data' || tag === 'meter') && attr(node, 'value')) return attr(node, 'value');
    return collapse(node.textContent || '').slice(0, 500);
  };
  const microItem = (scope, depth) => {
    const item = {};
    const type = collapse(attr(scope, 'itemtype'));
    if (type) item['@type'] = type.split(' ').map((value) => value.replace(/^https?:\/\/schema\.org\//, '')).join(' ');
    const queue = Array.from(scope.children || []);
    let count = 0;
    while (queue.length && count < 200) {
      const node = queue.shift();
      const props = collapse(attr(node, 'itemprop'));
      if (props) {
        count += 1;
        const value = node.hasAttribute && node.hasAttribute('itemscope') ? (depth < 4 ? microItem(node, depth + 1) : '[nested item]') : microValue(node);
        for (const prop of props.split(' ')) {
          if (!(prop in item)) item[prop] = value;
          else if (Array.isArray(item[prop])) item[prop].push(value);
          else item[prop] = [item[prop], value];
        }
      }
      if (!(node.hasAttribute && node.hasAttribute('itemscope'))) queue.push(...Array.from(node.children || []));
    }
    return item;
  };
  const collectStructured = () => {
    const structured = { jsonLd: [], meta: [], microdata: [] };
    try {
      for (const script of Array.from(doc.querySelectorAll('script[type="application/ld+json" i]')).slice(0, 50)) {
        const raw = String(script.textContent || '').trim();
        if (!raw) continue;
        try { structured.jsonLd.push({ json: JSON.stringify(JSON.parse(raw)) }); }
        catch { structured.jsonLd.push({ json: collapse(raw), unparsed: true }); }
      }
    } catch { /* structured data is optional */ }
    try {
      const description = doc.querySelector('meta[name="description" i]');
      if (description && attr(description, 'content')) structured.meta.push(['description', collapse(attr(description, 'content'))]);
      const canonical = doc.querySelector('link[rel~="canonical" i]');
      if (canonical && attr(canonical, 'href')) structured.meta.push(['canonical', pageAbsoluteURL(attr(canonical, 'href'), base)]);
      for (const meta of Array.from(doc.querySelectorAll('meta[property^="og:" i], meta[property^="product:" i], meta[name^="product:" i], meta[itemprop][content]')).slice(0, 80)) {
        const key = attr(meta, 'property') || attr(meta, 'name') || attr(meta, 'itemprop');
        const content = collapse(attr(meta, 'content'));
        if (key && content && !(meta.closest && meta.closest('[itemscope]'))) structured.meta.push([key, content]);
      }
    } catch { /* structured data is optional */ }
    try {
      for (const scope of Array.from(doc.querySelectorAll('[itemscope]:not([itemprop])')).slice(0, 50)) {
        structured.microdata.push(JSON.stringify(microItem(scope, 0)));
      }
    } catch { /* structured data is optional */ }
    return structured;
  };

  const lang = collapse(attr(doc.documentElement || {}, 'lang'));
  if (config.scope === 'selector') {
    let matches = [];
    try { matches = Array.from(doc.querySelectorAll(config.selector)); }
    catch { return { ok: false, status: 'invalid_selector', reason: 'the selector is not valid CSS' }; }
    if (!matches.length) return { ok: false, status: 'missing', reason: 'the selector matched no element' };
    const outer = matches.filter((node) => !matches.some((other) => other !== node && other.contains && other.contains(node)));
    const main = render(outer.slice(0, 1000));
    return { ok: true, url: location.href, title: doc.title, lang, main, outside: '', placed, matches: outer.length, stats: { elements, stopped } };
  }
  if (config.scope === 'target') {
    if (!scopedTarget || !scopedTarget.isConnected) return { ok: false, status: 'stale', reason: 'the element is gone; refresh the snapshot' };
    const main = render([scopedTarget]);
    return { ok: true, url: location.href, title: doc.title, lang, main, outside: '', placed, stats: { elements, stopped } };
  }
  const root = doc.body || doc.documentElement;
  let mainRoot = null;
  if (config.mainFirst && root) {
    try {
      const candidates = Array.from(doc.querySelectorAll('main, [role="main"]'));
      mainRoot = candidates.find((node) => node !== root && visibleElement(node)) || null;
    } catch { mainRoot = null; }
  }
  let main = '';
  let outside = '';
  if (mainRoot) {
    main = render([mainRoot]);
    if (!stopped) outside = render([root], mainRoot);
  } else if (root) main = render([root]);
  // Frames this document hides are not appended anywhere.
  const hidden = [];
  if (frameTokens) {
    try {
      for (const frame of Array.from(doc.querySelectorAll('iframe, frame'))) {
        const token = frameTokens.get(frame);
        if (token && String(token).startsWith(nonce + ':') && !placed.includes(String(token))) hidden.push(String(token));
      }
    } catch { /* unplaced frames are appended instead */ }
  }
  return {
    ok: true, url: location.href, title: doc.title, lang, main, outside, mainFound: !!mainRoot, placed, hidden,
    structured: config.structured ? collectStructured() : null, stats: { elements, stopped },
  };
}

function readScript(config, locator = null) {
  const target = locator ? targetExpression(locator) : 'null';
  return `(() => {
    ${PAGE_FACT_HELPERS}
    const pageReadDocument = (${pageReadDocument.toString()});
    return pageReadDocument(${JSON.stringify(config)}, ${target});
  })()`;
}

// Credentials inside page text are redacted line by line, so an unterminated
// quoted secret can only hide the rest of its own line.
function redactMarkdown(markdown) {
  return String(markdown || '').split('\n').map((line) => redactPageValue(line)).join('\n');
}

function structuredSection(structured) {
  if (!structured) return '';
  const lines = [];
  for (const item of structured.jsonLd || []) {
    let type = '';
    try {
      const parsed = JSON.parse(item.json);
      const first = Array.isArray(parsed) ? parsed[0] : parsed;
      type = first && (first['@type'] || (Array.isArray(first['@graph']) ? 'graph' : '')) || '';
      if (Array.isArray(type)) type = type.join(', ');
    } catch { type = ''; }
    lines.push('[Structured data: JSON-LD' + (type ? ' ' + String(type) : '') + (item.unparsed ? ', not valid JSON' : '') + ']');
    lines.push('```json', item.json, '```');
  }
  for (const json of structured.microdata || []) {
    lines.push('[Structured data: microdata]', '```json', json, '```');
  }
  const meta = structured.meta || [];
  if (meta.length) {
    lines.push('[Page metadata]');
    for (const [key, value] of meta) lines.push('- ' + key + ': ' + value);
  }
  return lines.join('\n');
}

function frameHeader(frame) {
  return redactPageMessage(frame && frame.url ? frame.url : '', 512) || 'unknown address';
}

const readCaches = new WeakMap();

function validateReadParams(params) {
  const offset = params.offset == null ? 0 : Number(params.offset);
  if (!Number.isSafeInteger(offset) || offset < 0) throw new Error('offset must be an integer of 0 or more');
  const maxBytes = params.maxBytes == null ? READ_DEFAULT_BYTES : Number(params.maxBytes);
  if (!Number.isInteger(maxBytes) || maxBytes < READ_MIN_BYTES || maxBytes > READ_MAX_BYTES) {
    throw new Error(`max_bytes must be an integer from ${READ_MIN_BYTES} to ${READ_MAX_BYTES}`);
  }
  for (const key of ['selector', 'elementRef', 'snapshotId', 'frameRef']) {
    if (params[key] != null && (typeof params[key] !== 'string' || !params[key].trim())) {
      throw new Error(`${key === 'elementRef' ? 'element_ref' : key === 'snapshotId' ? 'snapshot_id' : key === 'frameRef' ? 'frame_ref' : key} must be a non-empty string`);
    }
  }
  if (params.includeFrames != null && typeof params.includeFrames !== 'boolean') throw new Error('include_frames must be a boolean');
  const hasRef = params.elementRef != null;
  const hasFrame = params.frameRef != null;
  if ((hasRef || hasFrame) && params.snapshotId == null) throw new Error('element_ref and frame_ref need the snapshot_id they came from');
  if (params.snapshotId != null && !hasRef && !hasFrame) throw new Error('snapshot_id is only used with element_ref or frame_ref');
  if (hasRef && hasFrame) throw new Error('pass element_ref or frame_ref, not both');
  if (hasRef && params.selector != null) throw new Error('pass element_ref or selector, not both');
  return { offset, maxBytes, includeFrames: params.includeFrames !== false };
}

function sliceUtf8(buffer, offset, maxBytes) {
  let start = Math.min(offset, buffer.length);
  while (start < buffer.length && (buffer[start] & 0xc0) === 0x80) start += 1;
  let end = Math.min(buffer.length, start + maxBytes);
  if (end < buffer.length) {
    while (end > start && (buffer[end] & 0xc0) === 0x80) end -= 1;
    const newline = buffer.lastIndexOf(0x0a, end - 1);
    if (newline >= start + Math.floor(maxBytes / 2)) end = newline + 1;
  }
  return { start, end, content: buffer.subarray(start, end).toString('utf8') };
}

async function evaluateReadFrame(manager, entry, frame, script, deadline) {
  if (frame && frame.sessionId) {
    const remainingMs = deadline - Date.now();
    if (remainingMs <= 0) throw new Error('browser frame read budget exhausted');
    return manager.evaluateFrame(entry, frame.id, script, { timeoutMs: remainingMs });
  }
  if (frame) return manager.evaluateFrame(entry, frame.id, script);
  return manager.evaluateMainWorld(entry, script, false);
}

// Tags each child frame's owner element in its parent document so the read
// script can put that frame's content where the iframe is.
async function markFrameOwner(manager, entry, parent, child, token, deadline) {
  const sessionId = parent.sessionId || undefined;
  const remaining = () => {
    if (!sessionId) return null;
    const value = deadline - Date.now();
    if (value <= 0) throw new Error('browser frame read budget exhausted');
    return value;
  };
  const owner = await manager.ownedSessionCommand(entry, 'DOM.getFrameOwner', { frameId: child.id }, sessionId, remaining());
  const backendNodeId = Number(owner && owner.backendNodeId);
  if (!backendNodeId) return false;
  const resolved = await manager.ownedSessionCommand(entry, 'DOM.resolveNode', {
    backendNodeId, ...(parent.contextId != null ? { executionContextId: parent.contextId } : {}),
  }, sessionId, remaining());
  const objectId = resolved && resolved.object && resolved.object.objectId;
  if (!objectId) return false;
  try {
    const marked = await manager.ownedSessionCommand(entry, 'Runtime.callFunctionOn', {
      objectId,
      functionDeclaration: "function(token){ const key=Symbol.for('workass.browser.read-frames'); let map=globalThis[key]; if(!map){ map=new WeakMap(); Object.defineProperty(globalThis,key,{value:map,configurable:true}); } map.set(this,token); return true; }",
      arguments: [{ value: token }],
      returnByValue: true,
    }, sessionId, remaining());
    return !!(marked && marked.result && marked.result.value === true);
  } finally {
    try { await manager.ownedSessionCommand(entry, 'Runtime.releaseObject', { objectId }, sessionId); } catch { /* released with its context */ }
  }
}

async function extractDocument(manager, entry, scope) {
  const deadline = Date.now() + (manager.readChildFramesBudgetMs ?? READ_CHILD_FRAMES_BUDGET_MS);
  const nonce = crypto.randomBytes(9).toString('base64url');
  const liveFrames = Array.from(entry.frames.values()).filter((frame) => !frame.detached);
  const rootFrame = typeof manager.frameForSession === 'function' ? manager.frameForSession(entry, null) : null;
  const scopeFrame = scope.frameId ? entry.frames.get(scope.frameId) : rootFrame;
  if (scope.frameId && (!scopeFrame || scopeFrame.detached)) {
    return { status: 'stale', reason: 'that frame is gone; refresh the snapshot' };
  }
  const subtree = scopeFrame && typeof manager.frameSubtreeIds === 'function' ? manager.frameSubtreeIds(entry, scopeFrame.id) : new Set();
  const childrenOf = (frameId) => liveFrames.filter((frame) => frame.parentFrameId === frameId && subtree.has(frame.id));
  const results = new Map();
  const tokenFrames = new Map();
  const unavailable = [];
  let framesRead = 0;
  let framesSkipped = 0;
  let tokenIndex = 0;
  const readFrame = async (frame, isScope) => {
    const children = scope.includeFrames && frame ? childrenOf(frame.id) : [];
    const tokens = new Map();
    for (const child of children) {
      tokenIndex += 1;
      const token = nonce + ':' + tokenIndex;
      tokenFrames.set(token, child.id);
      tokens.set(child.id, token);
      try { await markFrameOwner(manager, entry, frame, child, token, deadline); }
      catch { /* an unmarked frame is appended after its parent instead */ }
    }
    const config = {
      nonce, frameMarker: FRAME_MARKER, frameMarkerEnd: FRAME_MARKER_END,
      maxElements: READ_MAX_ELEMENTS, maxChars: READ_MAX_CHARS,
      scope: isScope ? scope.kind : 'document', selector: isScope ? scope.selector : null,
      mainFirst: isScope && scope.kind === 'document' && frame === rootFrame,
      structured: isScope && scope.kind === 'document',
    };
    const locator = isScope && scope.kind === 'target' ? scope.locator : null;
    const raw = await evaluateReadFrame(manager, entry, frame, readScript(config, locator), deadline);
    framesRead += 1;
    results.set(frame ? frame.id : '', { frame, raw, children, tokens });
    if (!raw || raw.ok !== true) return;
    for (const child of children) {
      if (framesRead + framesSkipped >= READ_FRAME_LIMIT) { framesSkipped += 1; continue; }
      const token = tokens.get(child.id);
      if (raw.hidden && raw.hidden.includes(token)) continue;
      // A scoped read only includes frames that sit inside the scope.
      if (config.scope !== 'document' && !(raw.placed || []).includes(token)) continue;
      if (child.unavailableReason && child.contextId == null && child.sessionId == null) {
        unavailable.push({ url: frameHeader(child), reason: redactPageMessage(child.unavailableReason, 160) });
        results.set(child.id, { frame: child, raw: null, error: child.unavailableReason });
        continue;
      }
      try { await readFrame(child, false); }
      catch (error) {
        const reason = /budget exhausted|did not answer/.test(String(error && error.message)) ? 'frame_read_timed_out' : 'frame_not_readable';
        unavailable.push({ url: frameHeader(child), reason });
        results.set(child.id, { frame: child, raw: null, error: reason });
      }
    }
  };
  let temporary = null;
  try {
    if (scope.kind === 'target' && scope.target && scope.target.backendDOMNodeId != null) {
      temporary = await manager.exposeBackendTarget(entry, scope.target);
    }
    await readFrame(scopeFrame || null, true);
  } finally {
    try { await temporary?.clear(); } catch { /* navigation already removed the frame context */ }
  }
  const top = results.get(scopeFrame ? scopeFrame.id : '');
  if (!top || !top.raw || top.raw.ok !== true) {
    return { status: (top && top.raw && top.raw.status) || 'unavailable', reason: (top && top.raw && top.raw.reason) || 'the page could not be read' };
  }
  const placedEverywhere = new Set();
  for (const value of results.values()) for (const token of (value.raw && value.raw.placed) || []) placedEverywhere.add(token);
  const renderFrame = (frameId, depth) => {
    const record = results.get(frameId);
    if (!record) return '';
    if (!record.raw || record.raw.ok !== true) return '';
    const stitch = (text) => String(text || '').replace(FRAME_MARKER_RE, (_match, token) => {
      const childId = tokenFrames.get(token);
      const child = childId ? results.get(childId) : null;
      const frame = child ? child.frame : (childId ? entry.frames.get(childId) : null);
      const url = frameHeader(frame);
      if (!child) return '[Frame: ' + url + ' (not read: frame limit or budget)]';
      if (!child.raw || child.raw.ok !== true) return '[Frame: ' + url + ' (not readable: ' + (child.error || 'unavailable') + ')]';
      const body = depth < 8 ? renderFrame(childId, depth + 1) : '';
      if (!body.trim()) return '';
      return '[Frame: ' + url + ']\n' + body + '\n[End of frame]';
    });
    let text = stitch(record.raw.main);
    if (record.raw.outside) text += '\n\n---\n[Page content outside the main region: header, navigation, sidebars, footer]\n\n' + stitch(record.raw.outside);
    if (scope.kind === 'document' || depth > 0) {
      for (const child of record.children || []) {
        const token = record.tokens.get(child.id);
        if (placedEverywhere.has(token) || (record.raw.hidden || []).includes(token)) continue;
        const childRecord = results.get(child.id);
        if (!childRecord || !childRecord.raw || childRecord.raw.ok !== true) continue;
        const body = renderFrame(child.id, depth + 1);
        if (body.trim()) text += '\n\n[Frame: ' + frameHeader(child) + ' (position on the page unknown)]\n' + body + '\n[End of frame]';
      }
    }
    return text.replace(/\n{3,}/g, '\n\n').trim();
  };
  // Redaction works line by line, so the parts are redacted before they are
  // joined and every offset below is exact.
  const body = redactMarkdown(renderFrame(scopeFrame ? scopeFrame.id : '', 0));
  const structured = redactMarkdown(structuredSection(top.raw.structured));
  let markdown = body;
  if (structured) {
    const structuredBytes = Buffer.byteLength(structured, 'utf8');
    const separator = '\n\n---\n\n';
    if (structuredBytes <= STRUCTURED_INLINE_BYTES) markdown = structured + separator + body;
    else {
      const pointer = (offset) => '[Structured data (' + Math.ceil(structuredBytes / 1024) + ' KiB) follows the page content, from offset ' + offset + ']\n\n';
      const tailStart = (offset) => Buffer.byteLength(pointer(offset) + body + separator, 'utf8');
      let at = tailStart(0);
      while (tailStart(at) !== at) at = tailStart(at);
      markdown = pointer(at) + body + separator + structured;
    }
  }
  return {
    status: 'ok',
    url: redactPageMessage(top.raw.url || entry.url || '', 2048),
    title: redactPageMessage(top.raw.title || '', 1024),
    lang: redactPageMessage(top.raw.lang || '', 32),
    buffer: Buffer.from(markdown, 'utf8'),
    mainFound: top.raw.mainFound === true,
    matches: top.raw.matches,
    frames: { read: Math.max(0, framesRead - 1), unavailable, skipped: framesSkipped },
    stopped: Array.from(results.values()).map((value) => value.raw && value.raw.stats && value.raw.stats.stopped).find(Boolean) || '',
  };
}

// Frames that are still attaching right after a navigation get a short,
// bounded moment to finish; nothing is cancelled.
async function waitForFrameSetup(manager, entry) {
  const pending = Array.from(entry.pendingTargetTasks || []).filter((task) => task && !task.settled && !task.cancelled && task.promise);
  if (!pending.length) return;
  let timer;
  await Promise.race([
    Promise.allSettled(pending.map((task) => task.promise)),
    new Promise((resolve) => { timer = setTimeout(resolve, manager.readFrameSetupWaitMs ?? READ_FRAME_SETUP_WAIT_MS); }),
  ]).finally(() => clearTimeout(timer));
  manager.assertCurrentEntry(entry);
}

async function browserRead(manager, entry, params = {}) {
  const { offset, maxBytes, includeFrames } = validateReadParams(params);
  manager.assertCurrentEntry(entry);
  await manager.ensureCDP(entry);
  if (offset === 0) await waitForFrameSetup(manager, entry);
  await manager.refreshFrameTree(entry);
  const scope = { kind: 'document', includeFrames, selector: null, frameId: null, locator: null, target: null };
  const scopeDescription = { kind: 'page' };
  if (params.frameRef != null) {
    const observation = entry.observations.get(params.snapshotId);
    const frameId = observation && observation.frameRefs instanceof Map ? observation.frameRefs.get(params.frameRef) : null;
    if (!observation || observation.documentGeneration !== entry.documentGeneration || !frameId) {
      return { status: 'stale', reason: 'frame_ref is not in that snapshot of the current document; refresh the snapshot' };
    }
    scope.frameId = frameId;
    scopeDescription.kind = 'frame';
    scopeDescription.frame_ref = params.frameRef;
  }
  if (params.elementRef != null) {
    const target = await manager.observationTarget(entry, { elementRef: params.elementRef, snapshotId: params.snapshotId });
    if (target.status) return { status: target.status, reason: target.reason };
    scope.kind = 'target';
    scope.target = target;
    scope.frameId = target.frameId || null;
    scope.locator = target.backendDOMNodeId != null ? { temporaryTarget: true } : target;
    scopeDescription.kind = 'element';
    scopeDescription.element_ref = params.elementRef;
  } else if (params.selector != null) {
    scope.kind = 'selector';
    scope.selector = params.selector;
    scopeDescription.kind = scope.frameId ? 'frame selector' : 'selector';
    scopeDescription.selector = redactPageMessage(params.selector, 512);
  }
  const key = JSON.stringify([scope.kind, scope.frameId, scope.selector, params.elementRef || null, params.snapshotId || null, includeFrames]);
  const cached = readCaches.get(entry);
  let document = null;
  let reused = false;
  if (offset > 0 && cached && cached.key === key && cached.documentGeneration === entry.documentGeneration &&
      cached.interactionGeneration === entry.interactionGeneration && Date.now() - cached.at < READ_CACHE_TTL_MS) {
    document = cached.document;
    reused = true;
  }
  if (!document) {
    document = await extractDocument(manager, entry, scope);
    if (document.status !== 'ok') return { status: document.status, reason: document.reason };
    readCaches.set(entry, {
      key, document, at: Date.now(),
      documentGeneration: entry.documentGeneration, interactionGeneration: entry.interactionGeneration,
    });
  }
  const total = document.buffer.length;
  const slice = sliceUtf8(document.buffer, offset, maxBytes);
  const next = slice.end < total ? slice.end : null;
  const result = {
    url: document.url,
    title: document.title,
    scope: scopeDescription,
    content: slice.content,
    offset: slice.start,
    bytes_returned: slice.end - slice.start,
    total_bytes: total,
    next_offset: next,
    complete: next == null,
    document_generation: entry.documentGeneration,
    loading: entry.loading === true,
  };
  if (next != null) result.next = `Call workass_browser_read again with offset ${next} and the same scope for the next part; ${total - next} bytes remain.`;
  if (document.lang) result.lang = document.lang;
  if (document.matches != null) result.selector_matches = document.matches;
  if (scope.kind === 'document' && !scope.frameId) result.main_region_first = document.mainFound;
  if (document.frames.read || document.frames.unavailable.length || document.frames.skipped) result.frames = document.frames;
  if (document.stopped) result.page_limit = document.stopped === 'element_limit'
    ? `the page has more than ${READ_MAX_ELEMENTS} elements; the rest was not read`
    : `the page text exceeds ${READ_MAX_CHARS} characters; the rest was not read`;
  if (offset > 0 && !reused) result.note = 'The earlier read of this page was replaced (the page changed or the read expired), so this part comes from a new read.';
  if (offset > total) result.note = 'offset is past the end of the page content.';
  return result;
}

module.exports = {
  READ_DEFAULT_BYTES,
  READ_MAX_BYTES,
  READ_MIN_BYTES,
  browserRead,
  pageReadDocument,
  readScript,
  redactMarkdown,
  sliceUtf8,
  structuredSection,
  validateReadParams,
};
