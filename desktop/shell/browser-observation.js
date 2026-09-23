'use strict';

const MAX_DIAGNOSTIC_ENTRIES = 200;
const MAX_DIAGNOSTIC_BYTES = 256 * 1024;
const MAX_DIAGNOSTIC_MESSAGE = 2048;
const MAX_REDACTION_SCAN_BYTES = 64 * 1024;
const MAX_OBSERVATION_ELEMENTS = 10000;
const MAX_OBSERVATION_DEPTH = 128;
const MAX_OBSERVATION_FRAMES = 100;
const MAX_OBSERVATION_TEXT_SCAN_BYTES = 256 * 1024;
const MAX_OBSERVATION_TEXT_NODES = 20000;
const MAX_NODE_SNAPSHOT_KEYS = 8;

function utf8Width(character) {
  const codePoint = character.codePointAt(0);
  return codePoint <= 0x7f ? 1 : codePoint <= 0x7ff ? 2 : codePoint <= 0xffff ? 3 : 4;
}

function truncateUtf8Bytes(value, maxBytes) {
  const limit = Math.max(0, Math.floor(Number(maxBytes) || 0));
  let output = '';
  let bytes = 0;
  for (const character of String(value == null ? '' : value)) {
    const width = utf8Width(character);
    if (bytes + width > limit) break;
    output += character;
    bytes += width;
  }
  return output;
}

function mapFramePointThroughQuad(point, viewport, quad) {
  const width = Number(viewport && viewport.width);
  const height = Number(viewport && viewport.height);
  const p1 = quad && quad.p1;
  const p2 = quad && quad.p2;
  const p3 = quad && quad.p3;
  const p4 = quad && quad.p4;
  if (!(width > 0 && height > 0) || !p1 || !p2 || !p3 || !p4) return { available: false, reason: 'frame_geometry_unavailable' };
  const expectedX = Number(p2.x) + Number(p4.x) - Number(p1.x);
  const expectedY = Number(p2.y) + Number(p4.y) - Number(p1.y);
  if (Math.abs(expectedX - Number(p3.x)) > 0.5 || Math.abs(expectedY - Number(p3.y)) > 0.5) {
    return { available: false, reason: 'perspective_frame_transform_unsupported' };
  }
  const u = Number(point && point.x) / width;
  const v = Number(point && point.y) / height;
  return {
    available: true,
    x: Number(p1.x) + u * (Number(p2.x) - Number(p1.x)) + v * (Number(p4.x) - Number(p1.x)),
    y: Number(p1.y) + u * (Number(p2.y) - Number(p1.y)) + v * (Number(p4.y) - Number(p1.y)),
  };
}

function observationScript({ snapshotId, maxNodes = 500, maxText = 32 * 1024, rootOnly = false } = {}) {
  const safeSnapshotId = JSON.stringify(String(snapshotId || ''));
  const nodes = Math.max(1, Math.min(500, Math.floor(Number(maxNodes) || 500)));
  const text = Math.max(1, Math.min(32 * 1024, Math.floor(Number(maxText) || 32 * 1024)));
  const observeChildDocuments = rootOnly !== true;
  return `(() => {
    const snapshotId = ${safeSnapshotId};
    const maxNodes = ${nodes};
    const maxText = ${text};
    const maxWalkElements = ${MAX_OBSERVATION_ELEMENTS};
    const maxWalkDepth = ${MAX_OBSERVATION_DEPTH};
    const maxFrames = ${MAX_OBSERVATION_FRAMES};
    const maxTextScanBytes = ${MAX_OBSERVATION_TEXT_SCAN_BYTES};
    const maxTextScanNodes = ${MAX_OBSERVATION_TEXT_NODES};
    const maxSnapshotKeysPerNode = ${MAX_NODE_SNAPSHOT_KEYS};
    const observeChildDocuments = ${observeChildDocuments};
    const semantic = [];
    const frames = [];
    const refs = [];
    const seenRoots = new Set();
    const editorRoots = [];
    const editorRootSet = new Set();
    const semanticElements = [];
    const editorStateByRoot = new Map();
    let editorRootCount = 0;
    const traversalErrors = [];
    let traversalTruncated = false;
    let depthTruncated = false;
    let frameListTruncated = false;
    const nodeRefs = globalThis[Symbol.for('workass.browser.observed-node-refs')] || new WeakMap();
    if (!globalThis[Symbol.for('workass.browser.observed-node-refs')]) {
      Object.defineProperty(globalThis, Symbol.for('workass.browser.observed-node-refs'), { value: nodeRefs, configurable: false, enumerable: false });
    }
    let nodeCount = 0;
    let visitedElements = 0;
    let scannedFrames = 0;
    const roleFor = (el) => {
      const explicit = el.getAttribute('role');
      if (explicit) return explicit.trim().split(/\\s+/u)[0];
      const tag = el.tagName.toLowerCase();
      if (tag === 'a' && el.hasAttribute('href')) return 'link';
      if (tag === 'button') return 'button';
      if (tag === 'textarea') return 'textbox';
      if (tag === 'select') return el.multiple ? 'listbox' : 'combobox';
      if (tag === 'input') {
        const type = String(el.type || 'text').toLowerCase();
        if (type === 'password' || type === 'text' || type === 'search' || type === 'email' || type === 'url' || type === 'tel') return 'textbox';
        if (type === 'checkbox') return 'checkbox';
        if (type === 'radio') return 'radio';
        if (type === 'button' || type === 'submit' || type === 'reset') return 'button';
      }
      if (/^h[1-6]$/u.test(tag)) return 'heading';
      if (tag === 'main') return 'main';
      if (tag === 'nav') return 'navigation';
      if (tag === 'header') return 'banner';
      if (tag === 'footer') return 'contentinfo';
      if (tag === 'aside') return 'complementary';
      if (tag === 'form') return 'form';
      if (tag === 'img' && el.getAttribute('alt')) return 'img';
      if (tag === 'li') return 'listitem';
      if (tag === 'ul' || tag === 'ol') return 'list';
      if (el.isContentEditable) return 'textbox';
      return '';
    };
    const visible = (el) => {
      const rect = el.getBoundingClientRect();
      const style = el.ownerDocument.defaultView.getComputedStyle(el);
      return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none' && Number(style.opacity || 1) > 0;
    };
    const mapFramePoint = (${mapFramePointThroughQuad.toString()});
    const framePointToParent = (point, frame, childView) => {
      const viewport = { width: childView.innerWidth, height: childView.innerHeight };
      if (typeof frame.getBoxQuads === 'function') {
        try {
          const quad = frame.getBoxQuads({ box: 'content' })[0];
          if (quad) return mapFramePoint(point, viewport, quad);
        } catch { /* use only the conservative axis-aligned fallback below */ }
      }
      let ancestor = frame;
      while (ancestor && ancestor.nodeType === 1) {
        const style = ancestor.ownerDocument.defaultView.getComputedStyle(ancestor);
        const transform = String(style.transform || 'none');
        const identity = transform === 'none' || /^matrix\\(1,\\s*0,\\s*0,\\s*1,\\s*0,\\s*0\\)$/u.test(transform);
        if (!identity || (style.perspective && style.perspective !== 'none') ||
            (style.zoom && style.zoom !== '1' && style.zoom !== 'normal')) {
          return { available: false, reason: 'transformed_frame_geometry_unavailable' };
        }
        ancestor = ancestor.parentElement;
      }
      const rect = frame.getBoundingClientRect();
      const scaleX = frame.offsetWidth > 0 ? rect.width / frame.offsetWidth : 0;
      const scaleY = frame.offsetHeight > 0 ? rect.height / frame.offsetHeight : 0;
      if (!(scaleX > 0 && scaleY > 0 && viewport.width > 0 && viewport.height > 0)) {
        return { available: false, reason: 'frame_geometry_unavailable' };
      }
      return {
        available: true,
        x: rect.x + frame.clientLeft * scaleX + point.x * frame.clientWidth * scaleX / viewport.width,
        y: rect.y + frame.clientTop * scaleY + point.y * frame.clientHeight * scaleY / viewport.height,
      };
    };
    const boundsFor = (el) => {
      const rect = el.getBoundingClientRect();
      const points = [
        { x: rect.x, y: rect.y }, { x: rect.right, y: rect.y },
        { x: rect.right, y: rect.bottom }, { x: rect.x, y: rect.bottom },
      ];
      try {
        let view = el.ownerDocument.defaultView;
        while (view && view !== window) {
          const frame = view.frameElement;
          if (!frame) return { bounds: null, available: false, reason: 'frame_parent_unavailable' };
          for (let index = 0; index < points.length; index += 1) {
            const mapped = framePointToParent(points[index], frame, view);
            if (!mapped.available) return { bounds: null, available: false, reason: mapped.reason };
            points[index] = { x: mapped.x, y: mapped.y };
          }
          view = view.parent;
        }
      } catch { return { bounds: null, available: false, reason: 'frame_geometry_unavailable' }; }
      const xs = points.map((point) => point.x);
      const ys = points.map((point) => point.y);
      const left = Math.min(...xs);
      const top = Math.min(...ys);
      return { bounds: { x: left, y: top, width: Math.max(...xs) - left, height: Math.max(...ys) - top }, available: true, reason: null };
    };
    const nameFor = (el) => {
      const labelledBy = el.getAttribute('aria-labelledby');
      if (labelledBy) {
        const root = el.getRootNode();
        const label = labelledBy.split(/\\s+/u).map((id) => root.getElementById?.(id) || el.ownerDocument.getElementById(id)).filter(Boolean).map((node) => node.innerText || node.textContent || '').join(' ').trim();
        if (label) return label;
      }
      const aria = el.getAttribute('aria-label');
      if (aria) return aria.trim();
      const labels = el.labels ? Array.from(el.labels).map((label) => label.innerText || label.textContent || '').join(' ').trim() : '';
      if (labels) return labels;
      return String(el.getAttribute('alt') || el.getAttribute('title') || el.innerText || el.getAttribute('placeholder') || '').trim().replace(/\\s+/gu, ' ');
    };
    const selectorFor = (el) => {
      if (el.id) return '#' + (globalThis.CSS?.escape ? CSS.escape(el.id) : el.id.replace(/[^a-zA-Z0-9_-]/gu, '\\\\$&'));
      const parts = [];
      for (let node = el; node && node.nodeType === 1 && node !== node.ownerDocument.documentElement; node = node.parentElement) {
        if (node.id) { parts.unshift('#' + (globalThis.CSS?.escape ? CSS.escape(node.id) : node.id)); break; }
        let part = node.tagName.toLowerCase();
        if (node.parentElement) {
          let sameTypeIndex = 0;
          for (const sibling of node.parentElement.children) {
            if (sibling.tagName === node.tagName) sameTypeIndex += 1;
            if (sibling === node) break;
          }
          part += ':nth-of-type(' + Math.max(1, sameTypeIndex) + ')';
        }
        parts.unshift(part);
      }
      return parts.join(' > ');
    };
    const editable = (el) => el.isContentEditable || el.matches('input:not([type=password]):not([type=hidden]),textarea,select,[role=textbox],.monaco-editor,.CodeMirror,.cm-editor');
    const scrollable = (el) => {
      const style = el.ownerDocument.defaultView.getComputedStyle(el);
      const vertical = /(auto|scroll|overlay)/u.test(String(style.overflowY || style.overflow || ''));
      const horizontal = /(auto|scroll|overlay)/u.test(String(style.overflowX || style.overflow || ''));
      return (vertical && el.scrollHeight > el.clientHeight) || (horizontal && el.scrollWidth > el.clientWidth);
    };
    const actionable = (el, role) => !!role || el.matches('a[href],button,input,textarea,select,[tabindex],[contenteditable]:not([contenteditable=false]),.monaco-editor,.CodeMirror,.cm-editor') || scrollable(el);
    const recordTraversalError = (stage, error) => {
      if (traversalErrors.length >= 32) return;
      const candidate = String(error && error.name || 'Error');
      const knownNames = new Set(['AbortError', 'Error', 'InvalidStateError', 'NotAllowedError', 'QuotaExceededError', 'RangeError', 'ReferenceError', 'SecurityError', 'SyntaxError', 'TypeError']);
      const name = knownNames.has(candidate) ? candidate : 'Error';
      traversalErrors.push({ stage, name });
    };
    const visit = (root, path, depth) => {
      if (!root || seenRoots.has(root)) return;
      if (depth > maxWalkDepth) { depthTruncated = true; traversalTruncated = true; return; }
      seenRoots.add(root);
      let children;
      try { children = root.children; } catch (error) { recordTraversalError('children', error); return; }
      for (let index = 0; children && index < children.length; index += 1) {
        if (visitedElements >= maxWalkElements) { traversalTruncated = true; return; }
        let el;
        try { el = children[index]; } catch (error) { recordTraversalError('child', error); continue; }
        if (!el) continue;
        visitedElements += 1;
        const elPath = path.concat([{ kind: 'child', index }]);
        let isVisible = false;
        try {
          if (el.matches('.monaco-editor,.CodeMirror,.cm-editor')) {
            editorRootCount += 1;
            if (!editorRootSet.has(el) && editorRoots.length < 50 && visible(el)) {
              editorRootSet.add(el);
              editorRoots.push(el);
            }
          }
          const role = roleFor(el);
          isVisible = visible(el);
          const isActionable = actionable(el, role);
          if ((role || isActionable) && isVisible) {
            nodeCount += 1;
            if (semantic.length < maxNodes) {
              const rect = boundsFor(el);
              const selector = selectorFor(el);
              const type = String(el.getAttribute('type') || '').toLowerCase();
              const valueLength = type === 'password' ? null : (typeof el.value === 'string' ? el.value.length : null);
              const refIndex = refs.length;
              const nodeKey = snapshotId + ':' + refIndex;
              const identities = nodeRefs.get(el) || new Set();
              identities.add(nodeKey);
              while (identities.size > maxSnapshotKeysPerNode) identities.delete(identities.values().next().value);
              nodeRefs.set(el, identities);
              refs.push({ path: elPath, selector, nodeKey });
              semanticElements.push(el);
              const editorRoot = el.matches('.monaco-editor,.CodeMirror,.cm-editor') ? el : null;
              const editorState = editorRoot ? editorStateByRoot.get(editorRoot) : null;
              const editorKind = editorRoot ? (editorRoot.matches('.monaco-editor') ? 'monaco' : 'code-editor') : null;
              const visibleText = String((editorState && editorState.text) || el.innerText || el.getAttribute('aria-label') || el.getAttribute('placeholder') || '').trim().replace(/\\s+/gu, ' ');
              const readOnly = !!el.readOnly || el.getAttribute('aria-readonly') === 'true' || !!(editorState && editorState.readOnly);
              semantic.push({
                refIndex, role: role || el.tagName.toLowerCase(), name: nameFor(el).slice(0, 240),
                tag: el.tagName.toLowerCase(), selector, ariaLabel: el.getAttribute('aria-label') || null,
                text: visibleText.slice(0, 240), type: type || null,
                checked: typeof el.checked === 'boolean' ? el.checked : null,
                expanded: el.hasAttribute('aria-expanded') ? el.getAttribute('aria-expanded') === 'true' : null,
                selected: el.hasAttribute('aria-selected') ? el.getAttribute('aria-selected') === 'true' : (typeof el.selected === 'boolean' ? el.selected : null),
                disabled: !!el.disabled || el.getAttribute('aria-disabled') === 'true',
                readOnly, editor: editorKind, editable: editable(el), actionable: isActionable,
                focused: el.ownerDocument.activeElement === el || el.contains(el.ownerDocument.activeElement),
                valueLength, bounds: rect.bounds, boundsAvailable: rect.available,
                boundsUnavailableReason: rect.reason, scrollable: scrollable(el),
                headingLevel: role === 'heading' ? Number(el.getAttribute('aria-level') || el.tagName.slice(1)) || null : null,
              });
            }
          }
        } catch (error) { recordTraversalError('element', error); }
        try {
          if (el.shadowRoot) visit(el.shadowRoot, elPath.concat([{ kind: 'shadow' }]), depth + 1);
        } catch (error) { recordTraversalError('shadow', error); }
        if (el.tagName === 'IFRAME' || el.tagName === 'FRAME') {
          if (scannedFrames >= maxFrames) { frameListTruncated = true; traversalTruncated = true; }
          else {
            scannedFrames += 1;
            const frameGeometry = boundsFor(el);
            const frameRecord = {
              selector: '', name: '', accessible: false, targetOwned: false,
              bounds: frameGeometry.bounds, boundsAvailable: frameGeometry.available,
              boundsUnavailableReason: frameGeometry.reason,
            };
            try {
              frameRecord.selector = selectorFor(el);
              frameRecord.name = String(el.name || el.title || '').slice(0, 120);
              const refIndex = refs.length;
              const nodeKey = snapshotId + ':' + refIndex;
              const identities = nodeRefs.get(el) || new Set();
              identities.add(nodeKey);
              while (identities.size > maxSnapshotKeysPerNode) identities.delete(identities.values().next().value);
              nodeRefs.set(el, identities);
              refs.push({ path: elPath, nodeKey, selector: frameRecord.selector });
              frameRecord.refIndex = refIndex;
              const frameDoc = el.contentDocument;
              if (frameDoc && frameDoc.documentElement) {
                frameRecord.accessible = true;
                frameRecord.access = observeChildDocuments ? 'same_origin_dom' : 'owned_frame_observed_separately';
                frameRecord.limitation = observeChildDocuments ? 'owned_cdp_frame_target_not_resolved' : null;
                frames.push(frameRecord);
                if (observeChildDocuments) visit(frameDoc, elPath.concat([{ kind: 'frame' }]), depth + 1);
              } else {
                let crossOrigin = false;
                try { void el.contentWindow.location.href; } catch { crossOrigin = true; }
                frameRecord.access = crossOrigin ? 'unavailable' : 'empty_or_navigating';
                frameRecord.limitation = crossOrigin ? 'cross_origin_requires_owned_cdp_target' : 'frame_document_not_ready';
                frameRecord.targetOwned = false;
                frames.push(frameRecord);
              }
            } catch (error) {
              frameRecord.access = 'unavailable';
              frameRecord.limitation = error && error.name === 'SecurityError'
                ? 'cross_origin_requires_owned_cdp_target'
                : 'frame_document_not_ready';
              recordTraversalError('frame', error);
              frames.push(frameRecord);
            }
          }
        }
        try { visit(el, elPath, depth + 1); } catch (error) { recordTraversalError('descendants', error); }
      }
    };
    try { visit(document, [], 0); } catch (error) { recordTraversalError('document', error); }
    const collectBodyText = () => {
      let output = '';
      let outputBytes = 0;
      let bytesScanned = 0;
      let textNodeCount = 0;
      let exact = true;
      let stopped = false;
      let hasText = false;
      let outputLimited = false;
      try {
        if (!document.body) return { text: '', bytesReturned: 0, bytesScanned: 0, totalBytes: 0, exact: true, truncated: false, textNodeCount: 0 };
        const walker = document.createTreeWalker(document.body, 4);
        let textNode;
        while ((textNode = walker.nextNode())) {
          textNodeCount += 1;
          if (textNodeCount > maxTextScanNodes) { exact = false; stopped = true; break; }
          let parent = textNode.parentElement;
          let parentDepth = 0;
          let shown = true;
          while (parent && parentDepth < maxWalkDepth) {
            const tag = String(parent.tagName || '').toLowerCase();
            if (tag === 'script' || tag === 'style' || tag === 'noscript' || tag === 'template') { shown = false; break; }
            const style = parent.ownerDocument.defaultView.getComputedStyle(parent);
            if (style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity || 1) === 0) { shown = false; break; }
            parent = parent.parentElement;
            parentDepth += 1;
          }
          if (!shown) continue;
          const raw = String(textNode.nodeValue || '');
          if (!raw) continue;
          const append = (character) => {
            const point = character.codePointAt(0);
            const width = point <= 0x7f ? 1 : point <= 0x7ff ? 2 : point <= 0xffff ? 3 : 4;
            if (bytesScanned + width > maxTextScanBytes) { exact = false; stopped = true; return; }
            bytesScanned += width;
            if (!outputLimited && outputBytes + width <= maxText) { output += character; outputBytes += width; }
            else outputLimited = true;
          };
          if (hasText) append(' ');
          if (stopped) break;
          for (const character of raw) { append(character); if (stopped) break; }
          hasText = true;
          if (stopped) break;
        }
      } catch (error) { exact = false; recordTraversalError('text', error); }
      const truncated = !exact || bytesScanned > outputBytes;
      return {
        text: output, bytesReturned: outputBytes, bytesScanned,
        totalBytes: exact ? bytesScanned : null, exact, truncated,
        truncatedBytes: exact ? Math.max(0, bytesScanned - outputBytes) : null,
        textNodeCount,
      };
    };
    const bodyText = collectBodyText();
    const editors = [];
    for (const root of editorRoots) {
      let value = '';
      let valueRead = false;
      let readOnly = root.getAttribute('aria-readonly') === 'true';
      const kind = root.matches('.monaco-editor') ? 'monaco' : 'code-editor';
      try {
        if (kind === 'monaco') {
          const api = globalThis.monaco?.editor;
          const candidates = typeof api?.getEditors === 'function' ? api.getEditors() : [];
          const editor = candidates.find((candidate) => candidate?.getDomNode?.() === root);
          const model = editor?.getModel?.();
          if (model) { value = String(model.getValue()); valueRead = true; }
          readOnly = readOnly || editor?.getRawOptions?.()?.readOnly === true;
        } else if (root.CodeMirror?.getValue) {
          value = String(root.CodeMirror.getValue()); valueRead = true;
          readOnly = readOnly || root.CodeMirror.getOption?.('readOnly') === true;
        }
      } catch { /* the rendered editor lines remain available below */ }
      let lineScanTruncated = false;
      if (!valueRead) {
        try {
          const walker = root.ownerDocument.createTreeWalker(root, 1);
          const lineSelector = '.view-lines .view-line,.CodeMirror-code pre,.cm-content .cm-line';
          const lines = [];
          let candidate;
          let visitedLines = 0;
          while ((candidate = walker.nextNode())) {
            visitedLines += 1;
            if (visitedLines > 2000) { lineScanTruncated = true; break; }
            if (candidate.matches(lineSelector)) lines.push(String(candidate.textContent || ''));
          }
          if (lines.length) value = lines.join('\\n');
        } catch { /* a partial editor record is safer than failing the page snapshot */ }
      }
      const state = {
        selector: selectorFor(root), kind, text: value.slice(0, 12000), valueLength: value.length,
        truncated: value.length > 12000 || lineScanTruncated, focused: root.contains(root.ownerDocument.activeElement),
        readOnly, lineScanTruncated,
      };
      editorStateByRoot.set(root, state);
      editors.push(state);
    }
    for (let index = 0; index < semantic.length; index += 1) {
      const state = editorStateByRoot.get(semanticElements[index]);
      if (!state) continue;
      semantic[index].text = String(state.text || '').trim().replace(/\\s+/gu, ' ').slice(0, 240);
      semantic[index].readOnly = state.readOnly;
      semantic[index].editor = state.kind;
      semantic[index].editable = true;
    }
    return {
      snapshotId, url: location.href, title: document.title, text: bodyText.text,
      textLength: bodyText.totalBytes == null ? bodyText.bytesScanned : bodyText.totalBytes,
      textLengthExact: bodyText.exact, textBytesReturned: bodyText.bytesReturned,
      textBytesScanned: bodyText.bytesScanned, textTruncatedBytes: bodyText.truncatedBytes,
      textTruncated: bodyText.truncated,
      semantic, nodeCount, semanticTruncated: nodeCount > semantic.length || traversalTruncated,
      semanticTruncatedCount: Math.max(0, nodeCount - semantic.length), refs, frames, editors,
      editorRootCount, editorsTruncated: editorRootCount > editorRoots.length,
      interactive: semantic.filter((node) => node.actionable),
      accessibility: { source: 'dom', axTreeAvailable: false, limitation: 'owned_cdp_ax_tree_not_resolved' },
      traversal: {
        visitedElements, maxElements: maxWalkElements, maxDepth: maxWalkDepth,
        truncated: traversalTruncated, depthTruncated, frameListTruncated,
        errorCount: traversalErrors.length, errors: traversalErrors,
        nodeCountExact: !traversalTruncated && traversalErrors.length === 0,
      },
      scroll: { x: scrollX, y: scrollY, width: innerWidth, height: innerHeight, deviceScaleFactor: devicePixelRatio, documentWidth: document.documentElement?.scrollWidth || 0, documentHeight: document.documentElement?.scrollHeight || 0 },
    };
  })()`;
}

function resolvePathScript(pathValue, nodeKey = '') {
  const encoded = JSON.stringify(Array.isArray(pathValue) ? pathValue : []);
  const key = JSON.stringify(String(nodeKey || ''));
  return `(() => { let root = document; let node = null; for (const step of ${encoded}) { if (step.kind === 'child') { node = root?.children?.[step.index] || null; root = node; } else if (step.kind === 'shadow') { root = node?.shadowRoot || null; node = null; } else if (step.kind === 'frame') { root = node?.contentDocument || null; node = null; } else return { node: null, identityValid: false }; if (!root) return { node: null, identityValid: false }; } const store = globalThis[Symbol.for('workass.browser.observed-node-refs')]; return { node, identityValid: !!node && store?.get(node)?.has(${key}) }; })()`;
}

function targetExpression(locator) {
  if (locator && locator.temporaryTarget === true) {
    return `globalThis[Symbol.for('workass.browser.action-target')] || null`;
  }
  if (locator && Array.isArray(locator.path)) {
    const resolved = resolvePathScript(locator.path, locator.nodeKey);
    return `(() => { const resolved = ${resolved}; return resolved.identityValid ? resolved.node : null; })()`;
  }
  const selector = JSON.stringify(String(locator && locator.selector || ''));
  return `(() => { const matches = document.querySelectorAll(${selector}); return matches.length === 1 ? matches[0] : null; })()`;
}

function observedTargetGeometryScript(locator, { mapFrames = true } = {}) {
  const target = targetExpression(locator);
  return [
    '(() => {',
    'const el = ' + target + ';',
    'if (!el || !el.isConnected) return { status: \"stale\" };',
    'const composedParent = (node) => node?.parentElement || node?.getRootNode?.()?.host || null;',
    'const composedContains = (parent, child) => { for (let node = child; node; node = composedParent(node)) if (node === parent) return true; return false; };',
    'let disabledAncestor = el; while (disabledAncestor && disabledAncestor.nodeType === 1) { if (disabledAncestor.disabled || disabledAncestor.getAttribute(\"aria-disabled\") === \"true\" || disabledAncestor.hasAttribute(\"inert\")) return { status: \"disabled\" }; disabledAncestor = composedParent(disabledAncestor); }',
    'const view = el.ownerDocument.defaultView;',
    'const style = view.getComputedStyle(el);',
    'if (style.display === \"none\" || style.visibility === \"hidden\" || Number(style.opacity || 1) === 0) return { status: \"missing\" };',
    'el.scrollIntoView({ block: \"center\", inline: \"center\" });',
    'const rect = el.getBoundingClientRect();',
    'const points = [{ x: rect.x, y: rect.y }, { x: rect.right, y: rect.y }, { x: rect.right, y: rect.bottom }, { x: rect.x, y: rect.bottom }];',
    'const deepHit = (doc, x, y) => { let hit = doc?.elementFromPoint?.(x, y) || null; const visited = new Set(); while (hit && !visited.has(hit)) { visited.add(hit); const nested = hit.shadowRoot?.elementFromPoint?.(x, y); if (!nested || nested === hit) break; hit = nested; } return hit; };',
    'let localPoint = { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2 };',
    'const localHit = deepHit(el.ownerDocument, localPoint.x, localPoint.y);',
    'if (!(localHit === el || composedContains(el, localHit))) return { status: \"blocked\", reason: \"covered\" };',
    ...(mapFrames ? [
    'const mapPoint = (' + mapFramePointThroughQuad.toString() + ');',
    'const mapFrame = (point, frame, childView) => {',
    '  const viewport = { width: childView.innerWidth, height: childView.innerHeight };',
    '  if (typeof frame.getBoxQuads === \"function\") { try { const quad = frame.getBoxQuads({ box: \"content\" })[0]; if (quad) return mapPoint(point, viewport, quad); } catch {} }',
    '  for (let ancestor = frame; ancestor && ancestor.nodeType === 1; ancestor = ancestor.parentElement) {',
    '    const css = ancestor.ownerDocument.defaultView.getComputedStyle(ancestor);',
    '    const transform = String(css.transform || \"none\");',
    '    const identity = transform === \"none\" || /^matrix\\(1,\\s*0,\\s*0,\\s*1,\\s*0,\\s*0\\)$/.test(transform);',
    '    if (!identity || (css.perspective && css.perspective !== \"none\") || (css.zoom && css.zoom !== \"1\" && css.zoom !== \"normal\")) return { available: false, reason: \"transformed_frame_geometry_unavailable\" };',
    '  }',
    '  const frameRect = frame.getBoundingClientRect();',
    '  const sx = frame.offsetWidth > 0 ? frameRect.width / frame.offsetWidth : 0;',
    '  const sy = frame.offsetHeight > 0 ? frameRect.height / frame.offsetHeight : 0;',
    '  if (!(sx > 0 && sy > 0 && viewport.width > 0 && viewport.height > 0)) return { available: false, reason: \"frame_geometry_unavailable\" };',
    '  return { available: true, x: frameRect.x + frame.clientLeft * sx + point.x * frame.clientWidth * sx / viewport.width, y: frameRect.y + frame.clientTop * sy + point.y * frame.clientHeight * sy / viewport.height };',
    '};',
    'try {',
    '  const topWindow = (() => { try { return view.top || window; } catch { return window; } })();',
    '  let owner = view;',
    '  while (owner && owner !== topWindow) {',
    '    const frame = owner.frameElement;',
    '    if (!frame) return { status: \"unavailable\", reason: \"frame_parent_unavailable\" };',
    '    for (let i = 0; i < points.length; i += 1) { const mapped = mapFrame(points[i], frame, owner); if (!mapped.available) return { status: \"unavailable\", reason: mapped.reason }; points[i] = { x: mapped.x, y: mapped.y }; }',
    '    const mappedCenter = mapFrame(localPoint, frame, owner); if (!mappedCenter.available) return { status: \"unavailable\", reason: mappedCenter.reason };',
    '    const parentHit = deepHit(frame.ownerDocument, mappedCenter.x, mappedCenter.y); if (parentHit !== frame) return { status: \"blocked\", reason: \"frame_boundary_covered\" };',
    '    localPoint = { x: mappedCenter.x, y: mappedCenter.y };',
    '    owner = owner.parent;',
    '  }',
    '} catch { return { status: \"unavailable\", reason: \"frame_geometry_unavailable\" }; }',
    ] : []),
    'const xs = points.map((point) => point.x); const ys = points.map((point) => point.y);',
    'const left = Math.min(...xs); const top = Math.min(...ys);',
    'return { status: \"ready\", tag: String(el.tagName || \"\").toLowerCase(), name: String(el.getAttribute(\"aria-label\") || el.innerText || el.textContent || \"\").trim().slice(0, 160), x: localPoint.x, y: localPoint.y, bounds: { x: left, y: top, width: Math.max(...xs) - left, height: Math.max(...ys) - top } };',
    '})()',
  ].join('\n');
}

function isSensitivePageKey(key) {
  const normalized = String(key || '')
    .replace(/([a-z0-9])([A-Z])/gu, '$1_$2')
    .toLowerCase()
    .replace(/[^a-z0-9]+/gu, '_');
  if (['api_key', 'token', 'secret', 'password', 'credential', 'bearer'].some((fragment) => normalized.includes(fragment))) return true;
  const parts = normalized.split('_').filter(Boolean);
  if (parts.includes('authorization')) return true;
  if (parts.some((part, index) => ['api', 'private', 'auth', 'access'].includes(part) && parts[index + 1] === 'key')) return true;
  return /(?:^|_)(?:apikey|privatekey|authkey|accesskey)(?:_|$)/u.test(normalized);
}

function isPageWhitespace(character) {
  return character === ' ' || character === '\t' || character === '\n'
    || character === '\r' || character === '\f' || character === '\v';
}

function isPageKeyStart(character) {
  if (!character) return false;
  const code = character.charCodeAt(0);
  return (code >= 65 && code <= 90) || (code >= 97 && code <= 122) || character === '_' || character === '$';
}

function isPageKeyPart(character) {
  if (isPageKeyStart(character) || character === '-' || character === '.') return true;
  const code = character ? character.charCodeAt(0) : 0;
  return code >= 48 && code <= 57;
}

function parsePageKey(source, start) {
  let opening = start;
  let escapedQuote = false;
  if (source[start] === '\\' && (source[start + 1] === '"' || source[start + 1] === "'")) {
    opening += 1;
    escapedQuote = true;
  }
  const first = source[opening];
  if (first === '"' || first === "'") {
    let end = opening + 1;
    while (end < source.length && end - start <= 128) {
      if (source[end] === '\\') {
        if (escapedQuote && source[end + 1] === first) {
          return { key: source.slice(opening + 1, end), end: end + 2 };
        }
        end += 2;
        continue;
      }
      if (!escapedQuote && source[end] === first) {
        return { key: source.slice(opening + 1, end), end: end + 1 };
      }
      end += 1;
    }
    return { key: null, end: start + 1 };
  }
  if (!isPageKeyStart(first)) return null;
  let end = start + 1;
  while (end < source.length && end - start <= 128 && isPageKeyPart(source[end])) end += 1;
  if (end - start > 128) return { key: null, end };
  return { key: source.slice(start, end), end };
}

function isValueBoundary(character) {
  return !character || isPageWhitespace(character) || character === ',' || character === ';'
    || character === '&' || character === '}' || character === ']' || character === ')' || character === '#';
}

function consumeEscapedQuotedValue(source, start) {
  const quote = source[start + 1];
  let index = start + 2;
  while (index < source.length) {
    if (source[index] === '\\' && source[index + 1] === quote) {
      const after = index + 2;
      if (after >= source.length || isValueBoundary(source[after])) {
        return { end: after, replacement: '\\' + quote + '[redacted]\\' + quote, present: true };
      }
      index += 2;
      continue;
    }
    if (source[index] === '\\') { index += 2; continue; }
    index += 1;
  }
  return { end: source.length, replacement: '\\' + quote + '[redacted]', present: true };
}

function consumeSensitiveValue(source, start) {
  if (start >= source.length || isValueBoundary(source[start])) return { end: start, replacement: '[redacted]', present: true };
  const first = source[start];
  if (first === '\\' && (source[start + 1] === '"' || source[start + 1] === "'")) return consumeEscapedQuotedValue(source, start);
  if (first === '"' || first === "'") {
    let index = start + 1;
    while (index < source.length) {
      if (source[index] === '\\') { index += 2; continue; }
      if (source[index] === first) return { end: index + 1, replacement: first + '[redacted]' + first, present: true };
      index += 1;
    }
    return { end: source.length, replacement: first + '[redacted]', present: true };
  }
  if (first === '{' || first === '[') {
    const stack = [first === '{' ? '}' : ']'];
    let quote = '';
    let index = start + 1;
    while (index < source.length) {
      const character = source[index];
      if (quote) {
        if (character === '\\') { index += 2; continue; }
        if (character === quote) quote = '';
      } else if (character === '"' || character === "'") quote = character;
      else if (character === '{') stack.push('}');
      else if (character === '[') stack.push(']');
      else if (character === '}' || character === ']') {
        if (stack[stack.length - 1] === character) {
          stack.pop();
          if (!stack.length) return { end: index + 1, replacement: '[redacted]', present: true };
        }
      }
      index += 1;
    }
    return { end: source.length, replacement: '[redacted]', present: true };
  }
  let end = start;
  while (end < source.length && !isValueBoundary(source[end])) end += 1;
  if (end === start) return { end: start, replacement: '[redacted]', present: true };
  return { end, replacement: '[redacted]', present: true };
}

function redactBearerValues(source) {
  const output = [];
  let copiedThrough = 0;
  let index = 0;
  while (index < source.length) {
    const before = source[index - 1] || '';
    if ((index === 0 || !/[A-Za-z0-9_]/u.test(before))
        && source.slice(index, index + 6).toLowerCase() === 'bearer'
        && isPageWhitespace(source[index + 6])) {
      let tokenStart = index + 6;
      while (isPageWhitespace(source[tokenStart])) tokenStart += 1;
      let tokenEnd = tokenStart;
      while (tokenEnd < source.length && /[A-Za-z0-9._~+\\/=\\-]/u.test(source[tokenEnd])) tokenEnd += 1;
      if (tokenEnd > tokenStart) {
        output.push(source.slice(copiedThrough, tokenStart), '[redacted]');
        copiedThrough = tokenEnd;
        index = tokenEnd;
        continue;
      }
    }
    index += 1;
  }
  output.push(source.slice(copiedThrough));
  return output.join('');
}

function redactTextAssignments(source) {
  const text = redactBearerValues(String(source));
  const output = [];
  let copiedThrough = 0;
  let index = 0;
  while (index < text.length) {
    const parsed = parsePageKey(text, index);
    if (!parsed) { index += 1; continue; }
    if (!parsed.key) { index = Math.max(index + 1, parsed.end); continue; }
    let separator = parsed.end;
    while (separator < text.length && isPageWhitespace(text[separator])) separator += 1;
    if (text[separator] !== ':' && text[separator] !== '=') { index += 1; continue; }
    if (!isSensitivePageKey(parsed.key)) { index = parsed.end; continue; }
    let valueStart = separator + 1;
    while (valueStart < text.length && isPageWhitespace(text[valueStart])) valueStart += 1;
    const value = consumeSensitiveValue(text, valueStart);
    if (!value.present) { index = Math.max(parsed.end, valueStart); continue; }
    output.push(text.slice(copiedThrough, valueStart), value.replacement);
    copiedThrough = value.end;
    index = Math.max(value.end, parsed.end);
  }
  output.push(text.slice(copiedThrough));
  return output.join('');
}

function redactPageTextInner(source) {
  return redactTextAssignments(String(source));
}

function redactPageMessage(value, maxBytes = MAX_DIAGNOSTIC_MESSAGE) {
  const limitValue = Number(maxBytes);
  const limit = Number.isFinite(limitValue) ? Math.max(0, Math.floor(limitValue)) : MAX_DIAGNOSTIC_MESSAGE;
  const input = truncateUtf8Bytes(String(value == null ? '' : value), MAX_REDACTION_SCAN_BYTES);
  return truncateUtf8Bytes(redactPageTextInner(input), limit);
}

function addDiagnostic(entry, kind, text, timestamp = Date.now()) {
  const message = redactPageMessage(text);
  const record = { sequence: ++entry.diagnosticSequence, generation: entry.documentGeneration, kind: String(kind || 'warning'), message, timestamp };
  const bytes = Buffer.byteLength(JSON.stringify(record));
  if (bytes > MAX_DIAGNOSTIC_BYTES) return null;
  entry.diagnostics.push({ record, bytes });
  entry.diagnosticBytes += bytes;
  while (entry.diagnostics.length > MAX_DIAGNOSTIC_ENTRIES || entry.diagnosticBytes > MAX_DIAGNOSTIC_BYTES) {
    const removed = entry.diagnostics.shift();
    entry.diagnosticBytes -= removed.bytes;
  }
  return record;
}

function diagnosticsAfter(entry, afterSequence = 0, limit = 20) {
  const latest = Math.max(0, Number(entry.diagnosticSequence) || 0);
  const requested = Math.max(0, Math.floor(Number(afterSequence) || 0));
  const after = Math.min(requested, latest);
  const boundedLimit = Math.max(1, Math.min(100, Math.floor(Number(limit) || 20)));
  const first = entry.diagnostics[0]?.record.sequence || (entry.diagnosticSequence + 1);
  const truncated = after < first - 1;
  const selected = entry.diagnostics.map((item) => item.record).filter((record) => record.sequence > after).slice(0, boundedLimit);
  return { entries: selected, cursor: selected.at(-1)?.sequence || after, nextSequence: latest + 1, truncated };
}

module.exports = {
  MAX_DIAGNOSTIC_BYTES,
  MAX_DIAGNOSTIC_ENTRIES,
  MAX_DIAGNOSTIC_MESSAGE,
  MAX_NODE_SNAPSHOT_KEYS,
  MAX_OBSERVATION_DEPTH,
  MAX_OBSERVATION_ELEMENTS,
  MAX_OBSERVATION_FRAMES,
  MAX_OBSERVATION_TEXT_SCAN_BYTES,
  MAX_OBSERVATION_TEXT_NODES,
  addDiagnostic,
  diagnosticsAfter,
  mapFramePointThroughQuad,
  observationScript,
  observedTargetGeometryScript,
  redactPageMessage,
  resolvePathScript,
  targetExpression,
  truncateUtf8Bytes,
};
