import { memo, useEffect, useRef } from 'react';
import type { ListItem, SignedBlock } from './blocks';
import { renderInline, type InlineMediaResolver } from './inline';
import { CodeBlock } from './CodeBlock';
import { VisualizeBlock } from './VisualizeBlock';

// One block. Memoized on its signature so a sealed block above the streaming
// tail never re-renders while tokens flow into the last block.
function renderList(ordered: boolean, items: ListItem[], keyBase: string, media?: InlineMediaResolver) {
  const Tag = ordered ? 'ol' : 'ul';
  return (
    <Tag className="doclist">
      {items.map((it, idx) => {
        // GFM task item: "[ ] text" / "[x] text" shows a read-only checkbox.
        const task = /^\[([ xX])\]\s+/.exec(it.text);
        return (
        <li key={idx} className={task ? 'task' : undefined}>
          {task ? <input type="checkbox" checked={task[1] !== ' '} disabled readOnly aria-label={task[1] !== ' ' ? 'Hecho' : 'Pendiente'} /> : null}
          {renderInline(task ? it.text.slice(task[0].length) : it.text, `${keyBase}-${idx}`, true, media)}
          {it.sub ? renderList(it.sub.ordered, it.sub.items, `${keyBase}-${idx}`, media) : null}
        </li>
        );
      })}
    </Tag>
  );
}

function BlockView({ sb, media, visualizeTabId, visualizeChatId, artifactOrigin, streaming }: { sb: SignedBlock; media?: InlineMediaResolver; visualizeTabId?: string; visualizeChatId?: string; artifactOrigin?: string; streaming?: boolean }) {
  // The paragraph text this block showed last. While its reply streams, what
  // a render adds at the end goes in its own .mdfresh span so only the new
  // sentence fades in (CSS, opacity only); the rest is never re-animated.
  const shownRaw = useRef<string | null>(null);
  const raw = sb.block.kind === 'p' ? sb.block.raw : null;
  useEffect(() => { shownRaw.current = raw; });
  // Dev-only render probe: inert unless a global collector is present. Used to
  // verify that memo keeps sealed blocks from re-rendering during streaming.
  const probe = (globalThis as { __blockRenders?: string[] }).__blockRenders;
  if (probe) probe.push(sb.sig.slice(0, 28));
  const b = sb.block;
  switch (b.kind) {
    case 'p': {
      const before = shownRaw.current;
      if (streaming && before && b.raw.length > before.length && b.raw.startsWith(before)) {
        return (
          <p className="p">
            {renderInline(before, 'p', true, media)}
            <span className="mdfresh" key={before.length}>{renderInline(b.raw.slice(before.length), `pf${before.length}`, true, media)}</span>
          </p>
        );
      }
      return <p className="p">{renderInline(b.raw, 'p', true, media)}</p>;
    }
    case 'heading':
      return <div className={`mdh mdh${Math.min(b.level, 3)}`}>{renderInline(b.raw, 'h', true, media)}</div>;
    case 'code':
      return <CodeBlock raw={b.raw} language={b.lang} closed={b.closed} />;
    case 'quote':
      return <div className="mdquote">{renderInline(b.raw, 'q', true, media)}</div>;
    case 'hr':
      return <hr className="mdhr" />;
    case 'list':
      return renderList(b.ordered, b.items, 'l', media);
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
    case 'visualize':
      return <VisualizeBlock spec={b.spec} error={b.error} tabId={visualizeTabId} chatId={visualizeChatId} artifactOrigin={artifactOrigin} />;
  }
}

export const MarkdownBlock = memo(BlockView, (a, b) => (
  a.sb.sig === b.sb.sig
  && a.media?.revision === b.media?.revision
  && a.visualizeTabId === b.visualizeTabId
  && a.visualizeChatId === b.visualizeChatId
  && a.artifactOrigin === b.artifactOrigin
  && a.streaming === b.streaming
));
