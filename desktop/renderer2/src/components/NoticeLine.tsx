import { useState } from 'react';
import type { MsgNotice } from '../store/types';
import { IcActAgent, IcChevron } from '../icons';
import { noticeWord } from '../notice-word.ts';

// A Workass wake-up notice (a tracked subagent finished) as ONE quiet line,
// never a bubble as if the user had written it (mock
// desktop/docs/mocks/subsessions/completion-notice.html, approved 2026-10-16).
// The model still receives the whole notice; the result stays folded here.
// "falló" is gray like every outcome word — never red. A fleet notice about an
// unreachable machine says "sin contacto", never an outcome (see notice-word).
export function NoticeLine({ notice }: { notice: MsgNotice }) {
  const [open, setOpen] = useState(false);
  const word = noticeWord(notice.status);
  const title = notice.title?.trim() || 'Subagente';
  const body = notice.body?.trim() ?? '';
  return (
    <>
      <button
        type="button" className={`note${open ? ' open' : ''}`} aria-expanded={body ? open : undefined}
        title={body ? (open ? 'Ocultar el resultado' : 'Mostrar el resultado') : undefined}
        onClick={() => { if (body) setOpen(!open); }}
      >
        <span className="ic" aria-hidden="true"><IcActAgent /></span>
        <span className="nm">{title}</span>
        <span className="st">{word}</span>
        {body && <span className="cv" aria-hidden="true"><IcChevron /></span>}
      </button>
      {open && body && <div className="note-res">{body}</div>}
    </>
  );
}
