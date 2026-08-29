// Inline markdown → React nodes. Producing nodes (never innerHTML) makes this
// XSS-safe by construction: user/agent text can only ever become text nodes.
import type { ReactNode } from 'react';

export interface InlineMedia {
  src: string;
  alt: string;
}

export interface InlineMediaResolver {
  // Stable array/object identity used by MarkdownBlock's memo boundary. The
  // resolver object itself may be recreated without repainting sealed blocks.
  revision: unknown;
  resolve: (target: string) => InlineMedia | null;
  open: (media: InlineMedia) => void;
}

// Order matters: code spans first (their content is literal), then image/link
// Markdown, then bold and italic. Angle-bracket destinations allow normal local
// paths with spaces: ![Preview](</workspace/calibration ready.png>).
const TOKEN = /(`[^`]+`)|(!?\[[^\]\n]*\]\((?:<[^>\n]+>|[^)\n]+)\))|(\*\*[^*]+\*\*)|(__[^_]+__)|(\*[^*\s][^*]*\*)|(_[^_\s][^_]*_)/;

export function normalizeMarkdownTarget(raw: string): string {
  let target = raw.trim();
  if (target.startsWith('<') && target.endsWith('>')) target = target.slice(1, -1).trim();
  return target;
}

function looksLikeLocalRaster(target: string): boolean {
  const normalized = normalizeMarkdownTarget(target);
  const pathLike = normalized.startsWith('/') || normalized.startsWith('./') || normalized.startsWith('../')
    || normalized.toLowerCase().startsWith('file:') || /^[A-Za-z]:[\\/]/.test(normalized);
  return pathLike && /\.(?:png|jpe?g|webp|gif)(?:[?#].*)?$/i.test(normalized);
}

function looksLikeHostedArtifact(target: string): boolean {
  const normalized = normalizeMarkdownTarget(target);
  return normalized.startsWith('/workass/artifacts/');
}

export function renderInline(text: string, keyBase = 'i', allowLinks = true, media?: InlineMediaResolver): ReactNode[] {
  const out: ReactNode[] = [];
  let rest = text;
  let n = 0;
  while (rest.length) {
    const m = rest.match(TOKEN);
    if (!m || m.index === undefined) { out.push(rest); break; }
    if (m.index > 0) out.push(rest.slice(0, m.index));
    const tok = m[0];
    const key = `${keyBase}-${n++}`;
    if (m[1]) {
      out.push(<code key={key}>{tok.slice(1, -1)}</code>);
    } else if (m[2]) {
      const imageSyntax = tok.startsWith('![');
      const close = tok.indexOf('](');
      const label = tok.slice(imageSyntax ? 2 : 1, close);
      const href = normalizeMarkdownTarget(tok.slice(close + 2, -1));
      const resolved = media?.resolve(href) ?? null;
      if (imageSyntax) {
        if (resolved) {
          out.push(
            <button key={key} type="button" className="assistant-inline-image" title="Ampliar" onClick={() => media?.open(resolved)}>
              <img src={resolved.src} alt={label || resolved.alt} />
            </button>,
          );
        } else if (looksLikeHostedArtifact(href)) {
          const hosted = { src: href, alt: label || 'Artifact image' };
          out.push(
            <button key={key} type="button" className="assistant-inline-image" title="Ampliar" onClick={() => media?.open(hosted)}>
              <img src={hosted.src} alt={hosted.alt} />
            </button>,
          );
        } else {
          out.push(<span key={key} className="assistant-image-pending">{label || 'Imagen'}</span>);
        }
      } else if (resolved) {
        // The durable media exists because this same assistant message also
        // contains an image token for the target. The image is already the
        // lightbox control, so rendering the companion "Open …" link creates
        // two controls for one action and leaves noisy text above the preview.
        // Suppress only this resolved companion; unrelated links still follow
        // the ordinary anchor path below.
      } else if (media && looksLikeLocalRaster(href)) {
        out.push(<span key={key} className="assistant-image-pending">{renderInline(label, key, false)}</span>);
      } else if (allowLinks) out.push(<a key={key} href={href} target="_blank" rel="noreferrer">{renderInline(label, key, allowLinks, media)}</a>);
      else out.push(...renderInline(label, key, allowLinks, media));
    } else if (m[3] || m[4]) {
      out.push(<b key={key}>{renderInline(tok.slice(2, -2), key, allowLinks, media)}</b>);
    } else if (m[5] || m[6]) {
      out.push(<i key={key}>{renderInline(tok.slice(1, -1), key, allowLinks, media)}</i>);
    }
    rest = rest.slice(m.index + tok.length);
  }
  return out;
}

const LANGUAGE_ALIASES: Record<string, string> = {
  js: 'javascript', jsx: 'javascript', mjs: 'javascript', cjs: 'javascript', javascript: 'javascript',
  ts: 'typescript', tsx: 'typescript', typescript: 'typescript',
  py: 'python', python: 'python', rb: 'ruby', ruby: 'ruby',
  sh: 'shell', bash: 'shell', zsh: 'shell', shell: 'shell', console: 'shell',
  ps1: 'powershell', powershell: 'powershell',
  go: 'go', golang: 'go', rs: 'rust', rust: 'rust',
  c: 'c', h: 'c', cpp: 'cpp', 'c++': 'cpp', cc: 'cpp', hpp: 'cpp',
  cs: 'csharp', 'c#': 'csharp', csharp: 'csharp', java: 'java', kt: 'kotlin', kotlin: 'kotlin', swift: 'swift',
  json: 'json', jsonc: 'json', yaml: 'yaml', yml: 'yaml', toml: 'toml', xml: 'markup', html: 'markup', svg: 'markup',
  css: 'css', scss: 'css', less: 'css', sql: 'sql', md: 'markdown', markdown: 'markdown',
  pine: 'pine', pinescript: 'pine', dockerfile: 'dockerfile', docker: 'dockerfile',
  txt: 'text', text: 'text', plaintext: 'text',
};

const LANGUAGE_LABELS: Record<string, string> = {
  javascript: 'javascript', typescript: 'typescript', python: 'python', shell: 'shell', powershell: 'powershell',
  go: 'go', rust: 'rust', c: 'c', cpp: 'c++', csharp: 'c#', java: 'java', kotlin: 'kotlin', swift: 'swift',
  json: 'json', yaml: 'yaml', toml: 'toml', markup: 'html', css: 'css', sql: 'sql', markdown: 'markdown',
  pine: 'pine script', ruby: 'ruby', dockerfile: 'dockerfile', text: 'text',
};

export function normalizeCodeLanguage(raw: string): string {
  const info = String(raw || '').trim().split(/\s+/u)[0] || '';
  const cleaned = info.replace(/^\{?\.?/u, '').replace(/[},].*$/u, '').toLowerCase();
  return LANGUAGE_ALIASES[cleaned] || (cleaned ? 'text' : 'text');
}

export function codeLanguageLabel(raw: string): string {
  const language = normalizeCodeLanguage(raw);
  return LANGUAGE_LABELS[language] || 'text';
}

const COMMON_KEYWORDS = [
  'as', 'async', 'await', 'break', 'case', 'catch', 'class', 'const', 'continue', 'default', 'defer', 'do', 'else',
  'enum', 'export', 'extends', 'finally', 'for', 'from', 'func', 'function', 'go', 'if', 'implements', 'import', 'in',
  'interface', 'let', 'map', 'match', 'method', 'new', 'package', 'private', 'protected', 'public', 'range', 'return',
  'static', 'struct', 'switch', 'throw', 'try', 'type', 'typeof', 'var', 'void', 'while', 'with', 'yield',
];

const LANGUAGE_KEYWORDS: Record<string, string[]> = {
  python: ['and', 'assert', 'def', 'del', 'elif', 'except', 'global', 'is', 'lambda', 'nonlocal', 'not', 'or', 'pass', 'raise'],
  shell: ['case', 'done', 'elif', 'esac', 'fi', 'function', 'select', 'then', 'until'],
  powershell: ['begin', 'class', 'data', 'dynamicparam', 'end', 'filter', 'foreach', 'param', 'process', 'trap'],
  go: ['chan', 'const', 'fallthrough', 'go', 'goto', 'range', 'select'],
  rust: ['crate', 'dyn', 'impl', 'macro_rules', 'mod', 'move', 'mut', 'pub', 'ref', 'self', 'super', 'trait', 'unsafe', 'use', 'where'],
  c: ['auto', 'extern', 'register', 'restrict', 'sizeof', 'typedef', 'union', 'volatile'],
  cpp: ['alignas', 'constexpr', 'namespace', 'noexcept', 'nullptr', 'operator', 'template', 'typename', 'using', 'virtual'],
  csharp: ['abstract', 'base', 'checked', 'delegate', 'event', 'explicit', 'implicit', 'internal', 'is', 'lock', 'namespace', 'override', 'params', 'readonly', 'sealed', 'unchecked'],
  java: ['abstract', 'final', 'instanceof', 'native', 'strictfp', 'synchronized', 'throws', 'transient', 'volatile'],
  kotlin: ['actual', 'companion', 'crossinline', 'data', 'expect', 'fun', 'infix', 'inline', 'object', 'out', 'reified', 'suspend', 'when'],
  swift: ['actor', 'associatedtype', 'convenience', 'deinit', 'extension', 'fileprivate', 'guard', 'init', 'inout', 'mutating', 'nonmutating', 'protocol', 'some', 'subscript'],
  javascript: ['delete', 'instanceof', 'of'],
  typescript: ['abstract', 'declare', 'keyof', 'namespace', 'readonly', 'satisfies', 'unknown'],
  sql: ['all', 'alter', 'and', 'any', 'asc', 'begin', 'by', 'commit', 'create', 'delete', 'desc', 'distinct', 'drop', 'exists', 'group', 'having', 'insert', 'into', 'join', 'left', 'limit', 'not', 'on', 'or', 'order', 'outer', 'right', 'rollback', 'select', 'set', 'table', 'union', 'update', 'values', 'where'],
  pine: ['and', 'by', 'continue', 'else', 'false', 'for', 'if', 'import', 'in', 'method', 'not', 'or', 'switch', 'true', 'type', 'var', 'varip', 'while'],
  ruby: ['alias', 'begin', 'def', 'defined', 'elsif', 'end', 'ensure', 'module', 'next', 'redo', 'rescue', 'retry', 'self', 'super', 'then', 'unless'],
  dockerfile: ['add', 'arg', 'cmd', 'copy', 'entrypoint', 'env', 'expose', 'from', 'healthcheck', 'label', 'run', 'shell', 'stopsignal', 'user', 'volume', 'workdir'],
};

const TYPE_WORDS = new Set([
  'any', 'bool', 'boolean', 'byte', 'char', 'double', 'error', 'float', 'float32', 'float64', 'int', 'int8', 'int16',
  'int32', 'int64', 'long', 'never', 'number', 'object', 'short', 'string', 'uint', 'uint8', 'uint16', 'uint32',
  'uint64', 'unknown', 'void',
]);
const LITERAL_WORDS = new Set(['false', 'nil', 'none', 'null', 'true', 'undefined', 'na']);
const HASH_COMMENT_LANGUAGES = new Set(['python', 'ruby', 'shell', 'powershell', 'yaml', 'toml', 'dockerfile']);
const DASH_COMMENT_LANGUAGES = new Set(['sql']);
const CASE_INSENSITIVE_LANGUAGES = new Set(['sql', 'dockerfile']);
const TYPED_LANGUAGES = new Set(['typescript', 'go', 'rust', 'c', 'cpp', 'csharp', 'java', 'kotlin', 'swift']);

type CodeToken = 'cm' | 'kw' | 'str' | 'num' | 'lit' | 'fn' | 'ty' | 'attr' | 'op';

export function renderCode(raw: string, rawLanguage = '', keyBase = 'c'): ReactNode[] {
  const language = normalizeCodeLanguage(rawLanguage);
  const insensitive = CASE_INSENSITIVE_LANGUAGES.has(language);
  const keywords = new Set([...COMMON_KEYWORDS, ...(LANGUAGE_KEYWORDS[language] || [])].map((word) => insensitive ? word.toLowerCase() : word));
  const out: ReactNode[] = [];
  let tokenIndex = 0;
  let index = 0;

  const push = (value: string, kind?: CodeToken) => {
    if (!value) return;
    if (!kind) out.push(value);
    else out.push(<span className={`tk-${kind}`} key={`${keyBase}-${tokenIndex++}`}>{value}</span>);
  };
  const lineStart = (at: number) => at === 0 || raw[at - 1] === '\n';
  const hashStartsComment = (at: number) => HASH_COMMENT_LANGUAGES.has(language)
    && (lineStart(at) || /\s/u.test(raw[at - 1] || ''));

  while (index < raw.length) {
    const rest = raw.slice(index);

    if (rest.startsWith('<!--')) {
      const end = raw.indexOf('-->', index + 4);
      const next = end < 0 ? raw.length : end + 3;
      push(raw.slice(index, next), 'cm');
      index = next;
      continue;
    }
    if (rest.startsWith('/*')) {
      const end = raw.indexOf('*/', index + 2);
      const next = end < 0 ? raw.length : end + 2;
      push(raw.slice(index, next), 'cm');
      index = next;
      continue;
    }
    if (rest.startsWith('//') || (DASH_COMMENT_LANGUAGES.has(language) && rest.startsWith('--')) || (raw[index] === '#' && hashStartsComment(index))) {
      const end = raw.indexOf('\n', index);
      const next = end < 0 ? raw.length : end;
      push(raw.slice(index, next), 'cm');
      index = next;
      continue;
    }

    const quote = raw[index];
    if (quote === '"' || quote === "'" || quote === '`') {
      let next = index + 1;
      while (next < raw.length) {
        if (raw[next] === '\\') { next += 2; continue; }
        if (raw[next] === quote) { next += 1; break; }
        next += 1;
      }
      push(raw.slice(index, Math.min(next, raw.length)), 'str');
      index = Math.min(next, raw.length);
      continue;
    }

    const number = rest.match(/^(?:0[xob][0-9a-f_]+|\d[\d_]*(?:\.\d[\d_]*)?(?:e[+-]?\d[\d_]*)?)/iu);
    if (number) {
      push(number[0], 'num');
      index += number[0].length;
      continue;
    }

    const identifier = rest.match(/^[$@_\p{L}][$@_\p{L}\p{N}]*/u);
    if (identifier) {
      const value = identifier[0];
      const comparable = insensitive ? value.toLowerCase() : value;
      const tail = raw.slice(index + value.length);
      const nextNonSpace = tail.match(/^\s*(.)/su)?.[1] || '';
      const previous = raw.slice(0, index).match(/\S(?=\s*$)/su)?.[0] || '';
      let kind: CodeToken | undefined;
      if (LITERAL_WORDS.has(comparable.toLowerCase())) kind = 'lit';
      else if (keywords.has(comparable)) kind = 'kw';
      else if (TYPE_WORDS.has(comparable.toLowerCase()) || (TYPED_LANGUAGES.has(language) && /^\p{Lu}/u.test(value))) kind = 'ty';
      else if (nextNonSpace === '(') kind = 'fn';
      else if ((language === 'yaml' || language === 'css' || language === 'toml') && nextNonSpace === ':') kind = 'attr';
      else if (language === 'markup' && (previous === '<' || previous === '/')) kind = 'ty';
      push(value, kind);
      index += value.length;
      continue;
    }

    const operator = rest.match(/^(?:===|!==|=>|::|:=|\?\?|&&|\|\||==|!=|<=|>=|\+\+|--|<<|>>|\.\.\.?|[{}[\]();,.<>:+*/%=&|!?~-])/u);
    if (operator) {
      push(operator[0], 'op');
      index += operator[0].length;
      continue;
    }

    push(raw[index]);
    index += 1;
  }
  return out;
}
