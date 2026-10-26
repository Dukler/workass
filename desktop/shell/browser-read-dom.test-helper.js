'use strict';

// A small DOM for running page scripts in node:vm. It models what the read and
// observation scripts use: element trees, attributes, computed display, forms,
// tables, labels and a compound-selector subset of querySelectorAll.

const BLOCK_TAGS = new Set([
  'html', 'body', 'div', 'p', 'section', 'article', 'main', 'nav', 'header', 'footer', 'aside', 'form',
  'ul', 'ol', 'li', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'table', 'thead', 'tbody', 'tfoot', 'tr', 'td', 'th',
  'pre', 'blockquote', 'dl', 'dt', 'dd', 'hr', 'figure', 'figcaption', 'details', 'summary', 'caption', 'fieldset',
]);
const DISPLAY = { li: 'list-item', table: 'table', tr: 'table-row', td: 'table-cell', th: 'table-cell', thead: 'table-header-group', tbody: 'table-row-group', caption: 'table-caption', head: 'none', script: 'none', style: 'none', template: 'none' };

function text(value) {
  return { nodeType: 3, nodeValue: String(value), parentElement: null };
}

function parseCompound(source) {
  const compound = { tag: '', classes: [], ids: [], attrs: [], nots: [] };
  let rest = source.trim();
  const tag = /^[a-zA-Z][a-zA-Z0-9-]*/.exec(rest);
  if (tag) { compound.tag = tag[0].toLowerCase(); rest = rest.slice(tag[0].length); }
  while (rest) {
    let match;
    if ((match = /^\.([\w-]+)/.exec(rest))) compound.classes.push(match[1]);
    else if ((match = /^#([\w-]+)/.exec(rest))) compound.ids.push(match[1]);
    else if ((match = /^\[([\w:-]+)(?:([~^*$]?=)(?:"([^"]*)"|([^\]\s"]*))(?:\s+i)?)?\]/.exec(rest))) compound.attrs.push({ name: match[1], op: match[2] || '', value: match[3] != null ? match[3] : (match[4] || ''), insensitive: /\s+i\]$/.test(match[0]) });
    else if ((match = /^:not\(([^)]*\))\)/.exec(rest)) || (match = /^:not\(([^)]*)\)/.exec(rest))) compound.nots.push(parseCompound(match[1]));
    else throw Object.assign(new Error('unsupported selector: ' + source), { name: 'SyntaxError' });
    rest = rest.slice(match[0].length);
  }
  return compound;
}

function matchesCompound(element, compound) {
  if (compound.tag && element.tagName.toLowerCase() !== compound.tag) return false;
  for (const name of compound.classes) if (!String(element.getAttribute('class') || '').split(/\s+/).includes(name)) return false;
  for (const id of compound.ids) if (element.getAttribute('id') !== id) return false;
  for (const { name, op, value, insensitive } of compound.attrs) {
    const actual = element.getAttribute(name);
    if (actual == null) return false;
    const left = insensitive ? actual.toLowerCase() : actual;
    const right = insensitive ? value.toLowerCase() : value;
    if (op === '=' && left !== right) return false;
    if (op === '^=' && !left.startsWith(right)) return false;
    if (op === '~=' && !left.split(/\s+/).includes(right)) return false;
    if (op === '*=' && !left.includes(right)) return false;
  }
  for (const not of compound.nots) if (matchesCompound(element, not)) return false;
  return true;
}

function matchesSelector(element, selector) {
  return String(selector).split(',').some((part) => matchesCompound(element, parseCompound(part)));
}

function makeDOM({ url = 'https://shop.example/items/1', title = 'Fixture page' } = {}) {
  const view = { innerWidth: 1440, innerHeight: 900, devicePixelRatio: 1, scrollX: 0, scrollY: 0 };
  const document = { nodeType: 9, title, baseURI: url, defaultView: view, activeElement: null };
  const all = () => {
    const found = [];
    const visit = (node) => {
      for (const child of node.childNodes || []) {
        if (child.nodeType === 1) { found.push(child); visit(child); }
      }
    };
    visit(document);
    return found;
  };
  document.querySelectorAll = (selector) => all().filter((element) => matchesSelector(element, selector));
  document.createTreeWalker = (root, whatToShow) => {
    const found = [];
    const visit = (node) => {
      for (const child of node.childNodes || []) {
        if ((whatToShow === 4 && child.nodeType === 3) || (whatToShow === 1 && child.nodeType === 1)) found.push(child);
        if (child.nodeType === 1) visit(child);
      }
    };
    visit(root);
    let index = 0;
    return { nextNode() { return found[index++] || null; } };
  };
  document.querySelector = (selector) => document.querySelectorAll(selector)[0] || null;
  document.getElementById = (id) => all().find((element) => element.getAttribute('id') === id) || null;
  view.document = document;
  view.getComputedStyle = (element) => element.computedStyle();

  function h(tagName, attributes = {}, ...children) {
    const tag = String(tagName).toLowerCase();
    const attrs = {};
    const props = {};
    for (const [key, value] of Object.entries(attributes || {})) {
      if (key.startsWith('$')) props[key.slice(1)] = value;
      else attrs[key] = String(value);
    }
    const element = {
      nodeType: 1, tagName: tag.toUpperCase(), ownerDocument: document, parentElement: null,
      childNodes: [], children: [], isConnected: true, style: props.style || {},
      scrollHeight: 0, clientHeight: 0, scrollWidth: 0, clientWidth: 0, offsetWidth: 200, offsetHeight: 20,
      getAttribute(name) { return Object.prototype.hasOwnProperty.call(attrs, name) ? attrs[name] : null; },
      hasAttribute(name) { return Object.prototype.hasOwnProperty.call(attrs, name); },
      setAttribute(name, value) { attrs[name] = String(value); },
      matches(selector) { return matchesSelector(this, selector); },
      closest(selector) { for (let node = this; node && node.nodeType === 1; node = node.parentElement) if (node.matches(selector)) return node; return null; },
      contains(other) { for (let node = other; node; node = node.parentElement) if (node === this) return true; return false; },
      querySelectorAll(selector) {
        const found = [];
        const visit = (node) => { for (const child of node.children) { if (child.matches(selector)) found.push(child); visit(child); } };
        visit(this);
        return found;
      },
      querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
      getRootNode() { return document; },
      getBoundingClientRect() {
        // An element inside a display:none ancestor has no box.
        for (let node = this; node && node.nodeType === 1; node = node.parentElement) {
          if (node.style && node.style.display === 'none') return { x: 0, y: 0, width: 0, height: 0, right: 0, bottom: 0 };
        }
        const width = props.width != null ? props.width : 200;
        const height = props.height != null ? props.height : 20;
        return { x: 0, y: 0, width, height, right: width, bottom: height };
      },
      computedStyle() {
        return {
          display: DISPLAY[tag] || (BLOCK_TAGS.has(tag) ? 'block' : 'inline'),
          visibility: 'visible', opacity: '1', flexDirection: 'row',
          overflow: 'visible', overflowX: 'visible', overflowY: 'visible',
          ...element.style,
        };
      },
      append(...nodes) {
        for (const node of nodes) {
          const child = typeof node === 'string' ? text(node) : node;
          child.parentElement = this;
          if (child.nodeType === 1) { child.ownerDocument = document; this.children.push(child); }
          this.childNodes.push(child);
        }
        return this;
      },
    };
    Object.defineProperty(element, 'textContent', { get() { return element.childNodes.map((child) => child.nodeType === 3 ? child.nodeValue : child.textContent).join(''); } });
    Object.defineProperty(element, 'innerText', { get() { return element.textContent; } });
    Object.defineProperty(element, 'id', { get() { return attrs.id || ''; } });
    if (tag === 'input' || tag === 'textarea') {
      element.type = (attrs.type || (tag === 'textarea' ? 'textarea' : 'text')).toLowerCase();
      element.value = props.value != null ? String(props.value) : (attrs.value || '');
      element.checked = props.checked === true;
    }
    if (tag === 'select') {
      Object.defineProperty(element, 'options', { get() { return element.children.filter((child) => child.tagName === 'OPTION'); } });
      Object.defineProperty(element, 'selectedOptions', { get() { return element.options.filter((option) => option.selected); } });
    }
    if (tag === 'option') {
      element.selected = props.selected === true;
      Object.defineProperty(element, 'label', { get() { return element.textContent; } });
    }
    if (tag === 'a') Object.defineProperty(element, 'href', { get() { return new URL(attrs.href, document.baseURI).href; } });
    if (tag === 'img') element.currentSrc = props.currentSrc || '';
    if (tag === 'table') {
      Object.defineProperty(element, 'rows', { get() {
        const rows = [];
        for (const child of element.children) {
          if (child.tagName === 'TR') rows.push(child);
          else if (['THEAD', 'TBODY', 'TFOOT'].includes(child.tagName)) rows.push(...child.children.filter((row) => row.tagName === 'TR'));
        }
        return rows;
      } });
      Object.defineProperty(element, 'caption', { get() { return element.children.find((child) => child.tagName === 'CAPTION') || null; } });
    }
    if (tag === 'tr') Object.defineProperty(element, 'cells', { get() { return element.children.filter((child) => child.tagName === 'TD' || child.tagName === 'TH'); } });
    if (tag === 'td' || tag === 'th') element.colSpan = Number(attrs.colspan || 1);
    if (props.labels) element.labels = props.labels;
    element.append(...children);
    return element;
  }

  const html = h('html', { lang: 'en' });
  const head = h('head');
  const body = h('body');
  html.append(head, body);
  html.parentElement = null;
  html.scrollWidth = 1440;
  html.scrollHeight = 900;
  document.activeElement = body;
  document.childNodes = [html];
  document.children = [html];
  document.documentElement = html;
  document.head = head;
  document.body = body;
  const context = {
    document, window: view, location: { href: url }, URL, JSON, Symbol, WeakMap, Map, Set,
    innerWidth: 1440, innerHeight: 900, devicePixelRatio: 1, scrollX: 0, scrollY: 0,
  };
  context.globalThis = context;
  return { document, view, html, head, body, h, text, context };
}

module.exports = { makeDOM, matchesSelector, text };
