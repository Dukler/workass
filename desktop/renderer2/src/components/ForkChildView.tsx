// A fork child, read-only, in place of the parent's transcript (approved mock
// sidebar-nested.html, surfaces 1–2; the receipt card is forks.html's). The
// title bar carries a breadcrumb back to the parent; the status and model sit
// on the line under it; a read-only footer replaces the composer.

import { useEffect, useRef, useState, type ReactNode } from 'react';
import type { Chat } from '../store/types';
import type { ForkAttachment, ForkChildView as ForkChild, ForkPointView, ForkRunView } from '../wire/types';
import { store, useClock } from '../store/store';
import { forkStore, useForkImages, useForks } from '../store/forks';
import { IcChevron, IcFork, ModelIcon, hasModelIcon } from '../icons';
import { Topbar } from './Transcript';
import { ToolDetail, fmtDur } from './messages';
import { renderInline } from '../markdown/inline';
import { parseBlocks } from '../markdown/blocks';
import { MarkdownBlock } from '../markdown/MarkdownBlock';
import { observeTranscriptImageOnce } from '../transcript-images';
import { resolveModelSelection } from '../model-selection';
import {
  forkCacheBar, forkCacheShare, forkChildElapsedMs, forkChildLabel, forkInputTotal, forkRowState, forkStatusWord,
  formatForkDuration, formatForkPercent, formatForkTokens, forkTranscriptItems,
} from '../fork-format';

type Ref = { tabId: string; chatId: string };

function StatusDot({ state }: { state: string }) {
  if (state === 'running') return <i className="fk-pulse" aria-hidden="true" />;
  if (state === 'queued') return <i className="fk-ring" aria-hidden="true" />;
  return <i className="fk-dot" aria-hidden="true" />;
}

function Crumbs({ chat, point, label }: { chat: Chat; point?: ForkPointView; label: string }) {
  return (
    <span className="crumbs">
      <button type="button" className="c cpar" title="Volver al chat" onClick={() => forkStore.clearSelection()}>{chat.title}</button>
      {point && (
        <>
          <span className="sep" aria-hidden="true"><IcChevron /></span>
          <span className="c fk" title={`Punto de bifurcación · ${point.name}`}><IcFork />{point.name}</span>
        </>
      )}
      <span className="sep" aria-hidden="true"><IcChevron /></span>
      <b title={label}>{label}</b>
    </span>
  );
}

function modelLabel(chat: Chat, point?: ForkPointView): { name: string; brand: string } {
  if (!point?.modelId) return { name: '', brand: '' };
  const groups = store.catalogGroupsForChat(chat).filter((group) => group.providerId === point.providerId);
  const name = resolveModelSelection(groups, [], point.modelId).model?.name ?? point.modelId;
  return { name, brand: store.providerBrand(point.providerId, chat) };
}

function Meta({ chat, child, point }: { chat: Chat; child: ForkChild; point?: ForkPointView }) {
  const state = forkRowState(child.status);
  const model = modelLabel(chat, point);
  return (
    <div className="ssmeta">
      <StatusDot state={state} />
      <span>{forkStatusWord(child.status)}</span>
      {model.name && (
        <>
          <span className="sep" aria-hidden="true">·</span>
          {hasModelIcon(model.brand) && <span className="mk" aria-hidden="true"><ModelIcon provider={model.brand} /></span>}
          <span>{model.name}</span>
        </>
      )}
    </div>
  );
}

function LiveDuration({ child }: { child: ForkChild }) {
  useClock();
  return <>{formatForkDuration(forkChildElapsedMs(child, Date.now()) ?? 0)}</>;
}

/** Where the child's input came from: cache read (accent), cache write, fresh input. */
function Receipt({ child }: { child: ForkChild }) {
  const usage = child.usage;
  const bar = forkCacheBar(usage);
  const share = formatForkPercent(forkCacheShare(usage, child.cacheHitShare));
  const state = forkRowState(child.status);
  const running = state === 'running';
  // The provider reports a child's usage when its turn ends; until then the
  // receipt says so instead of showing a row of zeros.
  const pending = (running || state === 'queued') && forkInputTotal(usage) === 0;
  const tokens = (value: number) => (pending ? '—' : formatForkTokens(value));
  const cells: Array<{ label: string; value: ReactNode; swatch?: 'r' | 'w' | 'f' }> = [
    { label: 'Caché leída', value: tokens(usage.cacheRead), swatch: 'r' },
    { label: 'Caché escrita', value: tokens(usage.cacheCreation), swatch: 'w' },
    { label: 'Entrada', value: tokens(usage.input), swatch: 'f' },
    { label: 'Salida', value: tokens(usage.output) },
    { label: 'Turnos', value: pending ? '—' : String(Math.max(0, child.turns || 0)) },
    { label: 'Tiempo', value: running ? <LiveDuration child={child} /> : formatForkDuration(child.wallMs) },
  ];
  return (
    <div className="rcpt">
      <div className="rchd">
        <span className="big">{pending ? 'Uso pendiente' : `${share} caché`}</span>
        <span className="of">{pending ? 'se registra al terminar' : `${formatForkTokens(usage.cacheRead)} de ${formatForkTokens(forkInputTotal(usage))} tok de entrada`}</span>
      </div>
      <div className="cbar" role="img" aria-label={pending ? 'Uso pendiente' : `${share} de la entrada salió de la caché`}>
        {!bar.empty && <i className="r" style={{ width: `${bar.read}%` }} />}
        {!bar.empty && <i className="w" style={{ width: `${bar.write}%` }} />}
        {!bar.empty && bar.showFresh && <i className="f" style={{ width: `${bar.fresh}%` }} />}
      </div>
      <div className="rcgrid">
        {cells.map((cell) => (
          <div className="rc" key={cell.label}>
            {cell.swatch && <i className={`sw ${cell.swatch}`} aria-hidden="true" />}
            <span className="rl">{cell.label}</span>
            <span className="rv">{cell.value}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function Prefix({ point }: { point: ForkPointView }) {
  // A quiet note, not a link: the marker it used to scroll to is gone — the
  // parent transcript carries no fork styling (user 2026-10-16), and the
  // breadcrumb above already returns to the chat.
  return (
    <div className="pfx" role="note">
      <span className="pg" aria-hidden="true"><IcFork /></span>
      <span className="pt">Prefijo heredado</span>
      <span className="pm">· {formatForkTokens(point.prefixSize)} tok · hasta «<span className="fn">{point.name}</span>»</span>
      <span className="rule" aria-hidden="true" />
    </div>
  );
}

function Thumb({ chatRef, runId, childId, index, attachment }: { chatRef: Ref; runId: string; childId: string; index: number; attachment: ForkAttachment }) {
  useForkImages();
  const holder = useRef<HTMLElement>(null);
  const [visible, setVisible] = useState(false);
  useEffect(() => {
    if (!holder.current) return;
    return observeTranscriptImageOnce(holder.current, () => setVisible(true));
  }, []);
  useEffect(() => {
    if (visible) forkStore.loadImage(chatRef, runId, childId, index);
  }, [visible, chatRef, runId, childId, index]);
  const image = forkStore.image(runId, childId, index);
  const caption = `r${String(index + 1).padStart(2, '0')}`;
  return (
    <figure ref={holder} title={attachment.name}>
      {image?.src
        ? <img src={image.src} alt={attachment.name} className="zoomable" onClick={() => store.openImageLightbox(image.full ?? image.src!, attachment.name)} />
        : <span className={`kthumb-ph${image?.failed ? ' failed' : ''}`} role="img" aria-label={image?.failed ? 'Imagen no disponible' : 'Cargando imagen'} />}
      <figcaption>{caption}</figcaption>
    </figure>
  );
}

function UserMessage({ chatRef, run, child, text }: { chatRef: Ref; run: ForkRunView; child: ForkChild; text: string }) {
  return (
    <div className="userpill">
      {child.attachments.length > 0 && (
        <div className="kthumbs">
          {child.attachments.map((attachment, index) => (
            <Thumb key={`${child.childId}:${index}`} chatRef={chatRef} runId={run.runId} childId={child.childId} index={index} attachment={attachment} />
          ))}
        </div>
      )}
      {!!text.trim() && <div className="userpill-text">{renderInline(text)}</div>}
    </div>
  );
}

function Prose({ text }: { text: string }) {
  return <div className="amsg"><div className="chatfind-text">{parseBlocks(text).map((block, index) => <MarkdownBlock key={`${index}-${block.sig}`} sb={block} />)}</div></div>;
}

function Body({ chatRef, run, child }: { chatRef: Ref; run: ForkRunView; child: ForkChild }) {
  const state = forkRowState(child.status);
  if (state === 'queued') return <div className="fknote">En cola: empieza cuando se libere un lugar.</div>;
  const transcript = forkStore.transcriptOf(run.runId, child.childId);
  if (!child.transcriptRetained && !transcript) return <div className="fknote">El transcript de esta subsesión no se conservó.</div>;
  if (!transcript || transcript.status === 'loading') return <div className="fknote">Cargando el transcript…</div>;
  if (transcript.status === 'error') return <div className="fknote">Transcript no disponible · {transcript.error}</div>;
  const items = forkTranscriptItems(transcript.entries, state === 'running');
  return (
    <>
      {items.map((item) => {
        if (item.kind === 'user') return <UserMessage key={item.key} chatRef={chatRef} run={run} child={child} text={item.text} />;
        if (item.kind === 'tool') {
          const t = item.tool;
          const trail = t.status === 'in_progress'
            ? <span className="evt-run" aria-hidden="true" />
            // Stamps are when the recorder saw each event, so a sub-0.1s gap
            // says nothing about the tool; show only a duration that means one.
            : typeof t.startedAt === 'number' && typeof t.endedAt === 'number' && t.endedAt - t.startedAt >= 100
              ? <span className="evt-dur-head">{fmtDur(t.endedAt - t.startedAt)}</span> : null;
          return <div key={item.key} className="toolsolo" data-status={t.status === 'failed' ? 'failed' : 'done'}><ToolDetail t={t} standalone trail={trail} /></div>;
        }
        if (item.kind === 'error') return <div key={item.key} className="fknote">{item.text}</div>;
        return <Prose key={item.key} text={item.text} />;
      })}
      {child.error && state !== 'completed' && <div className="fknote">{child.error}</div>}
    </>
  );
}

function Footer({ chatRef, run, child }: { chatRef: Ref; run: ForkRunView; child: ForkChild }) {
  const [busy, setBusy] = useState<'stop' | 'retry' | 'keep' | null>(null);
  const state = forkRowState(child.status);
  const live = state === 'running' || state === 'queued';
  const kept = run.retention === 'keep';
  const days = Math.max(0, Math.round(run.retentionDays || 0));
  const act = async (which: 'stop' | 'retry' | 'keep', work: () => Promise<void>) => {
    if (busy) return;
    setBusy(which);
    try { await work(); } finally { setBusy(null); }
  };
  return (
    <div className="rofoot">
      <span className="t">
        {kept
          ? <><b>Subsesión conservada</b> · no se borra sola</>
          : <><b>Subsesión descartable</b> · se borra en {days} {days === 1 ? 'día' : 'días'}</>}
      </span>
      <span className="sp" />
      {live
        ? <button type="button" className="btn" disabled={busy !== null} onClick={() => void act('stop', () => forkStore.cancelChild(chatRef, run.runId, child.childId))}>{busy === 'stop' ? 'Deteniendo…' : 'Detener'}</button>
        : <button type="button" className="btn" disabled={busy !== null} onClick={() => void act('retry', () => forkStore.retryChild(chatRef, run.runId, child.childId))}>{busy === 'retry' ? 'Reintentando…' : 'Reintentar'}</button>}
      {kept
        ? <span className="rokept">Conservado</span>
        : <button type="button" className="btn" disabled={busy !== null} onClick={() => void act('keep', () => forkStore.retain(chatRef, run.runId))}>{busy === 'keep' ? 'Conservando…' : 'Conservar'}</button>}
    </div>
  );
}

/** The main-area subsession view for the selected child of the active chat. */
export function ForkChildView({ chat }: { chat: Chat }) {
  useForks();
  const selection = forkStore.selectedFor(chat.id);
  const chatRef = useRef<Ref>({ tabId: chat.id, chatId: chat.chatId ?? selection?.chatId ?? '' }).current;
  useEffect(() => { forkStore.ensure(chatRef); }, [chatRef]);
  const run = selection ? forkStore.run(chat.id, selection.runId) : undefined;
  const child = selection && run ? forkStore.slotChild(chat.id, selection.runId, selection.rootId) : undefined;
  const point = run ? forkStore.point(chat.id, run.forkPointId) : undefined;
  const label = child ? forkChildLabel(child) : 'Subsesión';
  return (
    <>
      <Topbar chat={chat} title={<Crumbs chat={chat} point={point} label={label} />} />
      <div className="transcriptviewport fkview">
        <div className="scroll">
          <div className="doc">
            {!run || !child ? (
              <div className="fknote">{forkStore.chat(chat.id) ? 'Esta subsesión ya no existe.' : 'Cargando la subsesión…'}</div>
            ) : (
              <>
                <Meta chat={chat} child={child} point={point} />
                <Receipt child={child} />
                {point && <Prefix point={point} />}
                <Body chatRef={chatRef} run={run} child={child} />
              </>
            )}
          </div>
        </div>
      </div>
      {run && child && <div className="compwrap"><Footer chatRef={chatRef} run={run} child={child} /></div>}
    </>
  );
}
