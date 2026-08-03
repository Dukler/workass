import { useRef, useState } from 'react';
import type { Chat } from '../store/types';
import { store } from '../store/store';
import { messageImageSrc } from '../image-drafts';
import { steerStatusLabel } from '../steering';

// Minimalist message queue (redesign 2026-07-12): each queued follow-up is its
// own box sized to its text. Double-click to edit in place (saves as you type,
// no button), drag a box to reorder (no handle — the box itself is the grip),
// and the ✕ (hover-only) removes it. They send one per turn end, in this order.
export function QueueList({ chat }: { chat: Chat }) {
  const queue = chat.queue ?? [];
  const waitingSteers = chat.messages.filter((message) => message.role === 'user' && message.steerBoundary === 'waiting');
  const [editing, setEditing] = useState<string | null>(null);
  const dragId = useRef<string | null>(null);
  const [over, setOver] = useState<{ id: string; after: boolean } | null>(null);
  if (!queue.length && !waitingSteers.length) return null;

  const drop = (targetId: string) => {
    const id = dragId.current;
    dragId.current = null;
    const spot = over;
    setOver(null);
    if (!id || id === targetId) return;
    const idx = queue.findIndex((q) => q.id === targetId) + (spot?.id === targetId && spot.after ? 1 : 0);
    store.reorderQueued(chat.id, id, idx);
  };

  return (
    <div className="qlist">
      {!!waitingSteers.length && <div className="qlabel">Steering · {waitingSteers.length}</div>}
      {waitingSteers.map((message) => (
        <div key={message.id} className="qrow steerwaiting">
          <span className="qcontent">
            {!!message.images?.length && (
              <span className="qimages" aria-label={`${message.images.length} adjunto${message.images.length === 1 ? '' : 's'}`}>
                {message.images.map((image, index) => (
                  <img key={`${image.name ?? 'image'}-${index}`} src={messageImageSrc(image)} alt={image.name ?? 'Adjunto'} tabIndex={0} aria-keyshortcuts="Meta+C Control+C" />
                ))}
              </span>
            )}
            <span className="qtext">{message.content}</span>
            {!!message.steerState && <span className="qattachstate">{steerStatusLabel(message.steerState, true)}</span>}
          </span>
        </div>
      ))}
      {!!queue.length && <div className="qlabel">En cola · {queue.length}</div>}
      {queue.map((q) => {
        const isEditing = editing === q.id;
        const overCls = over?.id === q.id ? (over.after ? 'over-after' : 'over-before') : '';
        return (
          <div
            key={q.id}
            className={`qrow ${isEditing ? 'editing' : ''} ${overCls}`}
            draggable={!isEditing}
            onDragStart={(e) => { dragId.current = q.id; e.dataTransfer.effectAllowed = 'move'; }}
            onDragOver={(e) => {
              if (!dragId.current || dragId.current === q.id) return;
              e.preventDefault();
              const r = e.currentTarget.getBoundingClientRect();
              const after = e.clientY > r.top + r.height / 2;
              if (over?.id !== q.id || over.after !== after) setOver({ id: q.id, after });
            }}
            onDrop={(e) => { e.preventDefault(); drop(q.id); }}
            onDragEnd={() => { dragId.current = null; setOver(null); }}
            onDoubleClick={() => { if (!isEditing) setEditing(q.id); }}
          >
            {isEditing ? (
              <textarea
                className="qedit"
                autoFocus
                defaultValue={q.text}
                rows={1}
                ref={(el) => { if (el) { el.style.height = 'auto'; el.style.height = `${el.scrollHeight}px`; } }}
                onChange={(e) => {
                  store.editQueued(chat.id, q.id, e.target.value);
                  e.currentTarget.style.height = 'auto';
                  e.currentTarget.style.height = `${e.currentTarget.scrollHeight}px`;
                }}
                onBlur={() => setEditing(null)}
                onKeyDown={(e) => {
                  if ((e.key === 'Enter' && !e.shiftKey) || e.key === 'Escape') { e.preventDefault(); setEditing(null); }
                }}
              />
            ) : (
              <>
                <span className="qcontent">
                  {!!q.images?.length && !q.draftImages?.length && (
                    <span className="qimages" aria-label={`${q.images.length} adjunto${q.images.length === 1 ? '' : 's'}`}>
                      {q.images.map((image, index) => (
                        <img key={`${image.name ?? 'image'}-${index}`} src={messageImageSrc(image)} alt={image.name ?? 'Adjunto'} tabIndex={0} aria-keyshortcuts="Meta+C Control+C" />
                      ))}
                    </span>
                  )}
                  {!!q.draftImages?.length && (
                    <span className="qimages" aria-label={`${q.draftImages.length} adjunto${q.draftImages.length === 1 ? '' : 's'}`}>
                      {q.draftImages.map((image) => (
                        <img key={image.id} src={image.url} alt={image.name} tabIndex={0} aria-keyshortcuts="Meta+C Control+C" />
                      ))}
                    </span>
                  )}
                  <span className="qtext">{q.text}</span>
                  {q.source === 'agent' && <span className="qattachstate">Enviado por agente</span>}
                  {q.attachmentState === 'preparing' && <span className="qattachstate">Preparando adjuntos…</span>}
                  {q.attachmentState === 'failed' && !!q.draftImages?.length && (
                    <button className="qattachretry" title={q.attachmentError} onClick={() => store.retryQueuedAttachments(chat.id, q.id)}>
                      Reintentar adjuntos
                    </button>
                  )}
                  {q.attachmentState === 'failed' && !q.draftImages?.length && (
                    <span className="qattachstate" title={q.attachmentError}>Volvé a adjuntar: {q.attachmentNames?.join(', ') || 'preparación interrumpida'}</span>
                  )}
                </span>
                <button
                  className="qx"
                  title="Quitar"
                  onMouseDown={(e) => e.stopPropagation()}
                  onClick={() => store.removeQueued(chat.id, q.id)}
                >
                  <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.7"><path d="M4 4l8 8M12 4l-8 8" /></svg>
                </button>
              </>
            )}
          </div>
        );
      })}
    </div>
  );
}
