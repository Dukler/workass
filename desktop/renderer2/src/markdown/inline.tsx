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

// Neutral, language-agnostic code treatment matching the mock: line comments
// faint, a small keyword set ink-bold. No green (design law: accent only).
const KEYWORDS = new Set([
  'func', 'function', 'return', 'if', 'else', 'for', 'range', 'while', 'const', 'let', 'var',
  'import', 'from', 'export', 'default', 'class', 'struct', 'interface', 'type', 'package',
  'def', 'async', 'await', 'new', 'switch', 'case', 'break', 'continue', 'go', 'defer', 'map',
  'public', 'private', 'static', 'void', 'int', 'string', 'bool', 'true', 'false', 'nil', 'null',
]);

export function renderCode(raw: string, keyBase = 'c'): ReactNode[] {
  const out: ReactNode[] = [];
  const lines = raw.split('\n');
  lines.forEach((line, li) => {
    if (li > 0) out.push('\n');
    // whole-line comment
    const trimmed = line.trimStart();
    if (trimmed.startsWith('//') || trimmed.startsWith('#') || trimmed.startsWith('*')) {
      out.push(<span className="cm" key={`${keyBase}-${li}c`}>{line}</span>);
      return;
    }
    // token-split for keyword tinting; also catch trailing // comments
    const commentIdx = line.indexOf('//');
    const codePart = commentIdx >= 0 ? line.slice(0, commentIdx) : line;
    const commentPart = commentIdx >= 0 ? line.slice(commentIdx) : '';
    const pieces = codePart.split(/(\b)/);
    pieces.forEach((piece, pi) => {
      if (KEYWORDS.has(piece)) out.push(<span className="kw" key={`${keyBase}-${li}-${pi}`}>{piece}</span>);
      else out.push(piece);
    });
    if (commentPart) out.push(<span className="cm" key={`${keyBase}-${li}cm`}>{commentPart}</span>);
  });
  return out;
}
