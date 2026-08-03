import { memo } from 'react';
import type { SignedBlock } from './blocks';
import { renderInline, renderCode, type InlineMediaResolver } from './inline';

// One block. Memoized on its signature so a sealed block above the streaming
// tail never re-renders while tokens flow into the last block.
function BlockView({ sb, media }: { sb: SignedBlock; media?: InlineMediaResolver }) {
  // Dev-only render probe: inert unless a global collector is present. Used to
  // verify that memo keeps sealed blocks from re-rendering during streaming.
  const probe = (globalThis as { __blockRenders?: string[] }).__blockRenders;
  if (probe) probe.push(sb.sig.slice(0, 28));
  const b = sb.block;
  switch (b.kind) {
    case 'p':
      return <p className="p">{renderInline(b.raw, 'p', true, media)}</p>;
    case 'heading':
      return <div className={`mdh mdh${Math.min(b.level, 3)}`}>{renderInline(b.raw, 'h', true, media)}</div>;
    case 'code':
      return <div className="codebox">{renderCode(b.raw)}</div>;
    case 'quote':
      return <div className="mdquote">{renderInline(b.raw, 'q', true, media)}</div>;
    case 'hr':
      return <hr className="mdhr" />;
    case 'list': {
      const Tag = b.ordered ? 'ol' : 'ul';
      return (
        <Tag className="doclist">
          {b.items.map((it, idx) => <li key={idx}>{renderInline(it, `l-${idx}`, true, media)}</li>)}
        </Tag>
      );
    }
    case 'table':
      return (
        <div className="mdtable-wrap">
          <table className="mdtable">
            <thead>
              <tr>{b.header.map((h, idx) => <th key={idx} style={{ textAlign: b.align[idx] === 'c' ? 'center' : b.align[idx] === 'r' ? 'right' : 'left' }}>{renderInline(h, `th-${idx}`, true, media)}</th>)}</tr>
            </thead>
            <tbody>
              {b.rows.map((row, ri) => (
                <tr key={ri}>{row.map((cell, ci) => <td key={ci} style={{ textAlign: b.align[ci] === 'c' ? 'center' : b.align[ci] === 'r' ? 'right' : 'left' }}>{renderInline(cell, `td-${ri}-${ci}`, true, media)}</td>)}</tr>
              ))}
            </tbody>
          </table>
        </div>
      );
  }
}

export const MarkdownBlock = memo(BlockView, (a, b) => a.sb.sig === b.sb.sig && a.media?.revision === b.media?.revision);
