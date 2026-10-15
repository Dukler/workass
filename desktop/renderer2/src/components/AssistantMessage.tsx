import { Fragment, memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react';
import type { MessageImage, Msg, ThinkingEvent, TimelineEvent, ToolEvent } from '../store/types';
import { store, useApp, useMsgVersion, useClock } from '../store/store';
import { parseBlocks, parseBlocksGrowing, type SignedBlock, listTexts } from '../markdown/blocks';
import { MarkdownBlock } from '../markdown/MarkdownBlock';
import { ToolGroup, ToolDetail, ToolImageGallery, PermCard, CompactionRow, RestoredRow, toolState, fmtDur, toolLiveLine } from './messages';
import { IcStampCopy, IcWarnTri, IcRetryArc, IcThought, ActionGlyph } from '../icons';
import { toolPresentation } from '../tool-names';
import { isSubagentHeader } from '../subagent-layout';
import { buildCoalescedTurnBlockTimelineSegments, buildTranscriptTimelineSegments, detachTimelineEvent, stableMarkdownBlockKeys } from '../timeline-layout';
import type { TranscriptTimelineSegment } from '../timeline-layout';
import { fullAssistantText } from '../assistant-output';
import { relTime } from '../rel-time';
import { MessageImageButton, cachedImageSrc, loadImageSrc } from './MessageImage';
import { connectedArtifactURL } from '../connected-artifacts';
import { configuredModelUnavailable } from '../model-selection';
import { revealedText, settleMountFades } from '../stream-reveal';
import { markdownImageReferences, normalizeMarkdownTarget, type InlineMediaResolver } from '../markdown/inline';

function renderedImageReferences(blocks: SignedBlock[]): Array<{ label: string; target: string }> {
  return blocks.flatMap(({ block }) => {
    switch (block.kind) {
      case 'p': case 'heading': case 'quote': return markdownImageReferences(block.raw);
      case 'list': return listTexts(block.items).flatMap(markdownImageReferences);
      case 'table': return [...block.header, ...block.rows.flat()].flatMap(markdownImageReferences);
      default: return [];
    }
  });
}

function assistantMediaResolver(tabId: string, images: MessageImage[] | undefined, blocks: SignedBlock[], artifactOrigin = ''): { media: InlineMediaResolver; gallery: MessageImage[] } {
  const bySource = new Map<string, MessageImage>();
  for (const image of images ?? []) {
    if (image.source) bySource.set(normalizeMarkdownTarget(image.source), image);
  }
  const references = renderedImageReferences(blocks);
  const gallery: MessageImage[] = [];
  const legacyImages = (images ?? []).filter((image) => !image.source);
  const imageNameCounts = new Map<string, number>();
  for (const image of legacyImages) imageNameCounts.set(image.name ?? '', (imageNameCounts.get(image.name ?? '') ?? 0) + 1);
  for (const image of legacyImages) {
    // The importer accepts eligible Markdown images in document order. Older
    // records lost source, so a repeated label belongs to its first target.
    const reference = references.find(({ label }) => label === image.name);
    const target = reference && normalizeMarkdownTarget(reference.target);
    if (image.name && imageNameCounts.get(image.name) === 1 && target && !bySource.has(target)) {
      bySource.set(target, image);
    } else {
      gallery.push(image);
    }
  }
  return { gallery, media: {
    revision: images ?? null,
    resolve: (target) => {
      const image = bySource.get(normalizeMarkdownTarget(target));
      if (!image) return null;
      const src = cachedImageSrc(image);
      return src ? { src, alt: image.name || 'Imagen' } : { src: '', alt: image.name || 'Imagen', deferred: { tabId, image } };
    },
    resolveLink: (target) => connectedArtifactURL(artifactOrigin, target, undefined, undefined, store.localMachineId()),
    openLink: (target) => store.openHostedArtifact(tabId, target, artifactOrigin),
    open: (media) => {
      if (media.src) { store.openImageLightbox(media.src, media.alt); return; }
      if (media.deferred) void loadImageSrc(media.deferred.tabId, media.deferred.image).then((src) => { if (src) store.openImageLightbox(src, media.alt); });
    },
  } };
}

function StructuredAssistantImages({ tabId, images }: { tabId: string; images: MessageImage[] | undefined }) {
  const structured = images ?? [];
  if (!structured.length) return null;
  return (
    <div className="assistant-images" aria-label="Imágenes de la respuesta">
      {structured.map((image, index) => {
        const alt = image.name || `Imagen ${index + 1}`;
        return (
          <MessageImageButton key={`${image.mimeType}-${index}`} tabId={tabId} image={image} alt={alt}
            className="assistant-inline-image" onOpen={(src) => store.openImageLightbox(src, alt)} />
        );
      })}
    </div>
  );
}

function EventView({ ev }: { ev: TimelineEvent }) {
  if (ev.kind === 'thinking') return null; // thoughts live in the Working line / Worked row
  if (ev.kind === 'plan') return null; // plan lives in the Tareas rail only (no chat dupe)
  if (ev.kind === 'compaction') return <CompactionRow />;
  if (ev.kind === 'restored') return <RestoredRow ev={ev} />;
  return null; // tool events render as grouped runs (foldToolGroups), never standalone
}

// The stamp's own leaf so the shared clock tick re-renders only this span, not the
// memoized message body. Without a tick, relTime freezes at first render and a
// settled turn reads "hace unos segundos" forever (user report 2026-07-17).
function RelStamp({ at }: { at: string | null }) {
  useClock();
  return <span>{relTime(at)}</span>;
}

function AssistantSliceBody({
  tabId,
  msg,
  owner,
  terminal,
  turnSeq,
  coalescedSegments,
  artifactOrigin,
  midRun = false,
}: {
  tabId: string;
  msg: Msg;
  owner: Msg;
  terminal: boolean;
  turnSeq?: number;
  coalescedSegments?: TranscriptTimelineSegment[];
  artifactOrigin?: string;
  midRun?: boolean;
}) {
  // Text already in this reply when it mounts shows as it is; only text that
  // arrives afterwards fades in. A reply born this instant keeps its fade.
  const root = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const born = msg.at ? Date.parse(msg.at) : Number.NaN;
    if (Number.isFinite(born) && Date.now() - born < 1500) return;
    settleMountFades(root.current);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
  // Thoughts never render inline (the Working line / Worked row owns them).
  // Remove their capture markers from the prose layout too; otherwise a
  // thought arriving mid-token leaves an invisible boundary that renders one
  // sentence as two spaced Markdown paragraphs.
  let segs = coalescedSegments ?? buildTranscriptTimelineSegments(msg);
  for (const event of msg.events) if (event.kind === 'thinking') segs = detachTimelineEvent(segs, event.key);
  const running = terminal && msg.status === 'running';
  const inlineResult = msg.result && !msg.events.some((event) => event.kind === 'result') ? msg.result : '';
  // While the reply streams, its very end shows only up to the last finished
  // sentence, list item, code line or table row (stream-reveal.ts): half-written
  // markdown never renders, so nothing flips type or shrinks mid-stream. Prose
  // that a tool call already followed is final and stays whole (a live turn's
  // trailing tools are deferred out of the layout, so ask the events).
  if (running && !inlineResult) {
    const tail = segs.length - 1;
    const last = segs[tail];
    if (last && 'prose' in last && !msg.events.some((event) => event.kind === 'tool' && event.at >= last.end)) {
      const prose = revealedText(last.prose);
      if (prose !== last.prose) segs = [...segs.slice(0, tail), { ...last, prose }];
    }
  }
  // A running reply reuses its settled blocks (parseBlocksGrowing); a settled one
  // renders rarely enough to parse whole.
  const parsedSegments = segs.map((segment, index) => 'prose' in segment
    ? running ? parseBlocksGrowing(`${msg.id}:${index}`, segment.prose) : parseBlocks(segment.prose)
    : null);
  const resultBlocks = inlineResult ? parseBlocks(running ? revealedText(inlineResult) : inlineResult) : [];
  const { media, gallery } = assistantMediaResolver(tabId, msg.images, parsedSegments.flatMap((blocks) => blocks ?? []).concat(resultBlocks), artifactOrigin);
  const visualizeChatId = store.chat(tabId)?.chatId ?? '';
  const blockKeys = stableMarkdownBlockKeys(msg, parsedSegments.flatMap((blocks) => blocks?.map((block) => block.sig) ?? []));
  let blockIndex = 0;
  // A turn cut off by a daemon disconnect: shown as a quiet "sin conexión" row,
  // not the generic model-error stamp and never as a resend affordance.
  const interrupted = terminal && msg.status === 'failed' && !!msg.interrupted;
  const settled = terminal && (msg.status === 'done' || msg.status === 'failed' || msg.status === 'cancelled') && !interrupted;
  // One copy/time row per run of replies, shown on hover (user 2026-10-15):
  // a run of one-line wake replies printed a row under every line. A reply
  // with another right after it keeps only a state word that is real news.
  const stateWord = msg.status === 'cancelled' ? 'detenido' : msg.status === 'failed' ? 'error' : '';
  const showStamp = settled && (!midRun || !!stateWord);
  // R4/R5: a completed turn that recorded a checkpoint (changed tracked files)
  // gets per-turn Deshacer/Revisar affordances, revealed on hover.
  const hasCheckpoint = terminal && turnSeq != null && (msg.status === 'done' || msg.status === 'failed');

  // Once settled, the steps the turn took (its thoughts and tool calls, in
  // order) fold into one "Worked" row at its head instead of interleaving
  // with the answer (user 2026-10-12). While running, the Working line owns them.
  const workItems = running ? [] : msg.events.filter(isWorkItem);

  return (
    <div className={`amsg${running ? ' streaming' : ''}`} ref={root}>
      {running && <ControlsSkippedRow tabId={tabId} />}
      {workItems.length > 0 && <WorkedRow items={workItems} />}
      {segs.map((s, segmentIndex) => {
        if ('tools' in s) {
          // Tool calls live in the Working line / Worked row, never inline.
          // Subagent task rows and the images a tool returned stay where
          // they landed: they are results, not steps.
          const tasks = s.tools.filter(isSubagentHeader);
          return (
            <Fragment key={s.key}>
              <ToolImageGallery tools={s.tools.filter((t) => !isSubagentHeader(t))} tabId={tabId} />
              {tasks.length > 0 && <ToolGroup tabId={tabId} tools={tasks} revision={s.revision} />}
            </Fragment>
          );
        }
        if ('event' in s) return <EventView key={s.event.key} ev={s.event} />;
        return (
          <div className="chatfind-text" data-chat-find-text key={`prose-${segmentIndex}`}>
            {parsedSegments[segmentIndex]!.map((sb) => <MarkdownBlock key={blockKeys[blockIndex++]} sb={sb} media={media} visualizeTabId={tabId} visualizeChatId={visualizeChatId} artifactOrigin={artifactOrigin} streaming={running} />)}
          </div>
        );
      })}

      {!!resultBlocks.length && (
        <div className="chatfind-text" data-chat-find-text>
          {resultBlocks.map((block, index) => <MarkdownBlock key={`result-${index}`} sb={block} media={media} visualizeTabId={tabId} visualizeChatId={visualizeChatId} artifactOrigin={artifactOrigin} streaming={running} />)}
        </div>
      )}
      <StructuredAssistantImages tabId={tabId} images={gallery} />
      {running && !msg.stopState && <LiveThought msg={msg} />}

      {interrupted && (
        <div className="connfail" role="status">
          <span className="cfdot" aria-hidden="true" />
          <span className="cftext">
            {msg.content !== '' || msg.events.length > 0 ? 'Interrumpido · conexión perdida' : 'No se pudo enviar · sin conexión'}
          </span>
        </div>
      )}

      {/* Questions dock in the composer's place (Composer → QuestionDock); only
          permission prompts stay inline in the transcript. */}
      {terminal && msg.permission && !msg.permission.question && <PermCard perm={msg.permission} tabId={tabId} msgId={owner.id} />}

      {showStamp && (
        <div className={`stamp${midRun ? ' stamp-quiet' : ''}`}>
          {stateWord && <span className={`stamp-state${msg.status === 'failed' ? ' turnfail' : ''}`}>{stateWord}</span>}
          {!midRun && (
            <>
              <button type="button" className="stamp-btn" title="Copiar respuesta" aria-label="Copiar respuesta"
                onClick={() => { void navigator.clipboard?.writeText(fullAssistantText(owner)); }}>
                <IcStampCopy />
              </button>
              <span className="stamp-time"><RelStamp at={msg.at} /></span>
            </>
          )}
          {!midRun && hasCheckpoint && (
            <span className="turnacts">
              <button className="turnact" title="Volver a antes de este turno" onClick={() => void store.openRewind(turnSeq)}>↺ Deshacer</button>
              <button className="turnact" title="Revisar los cambios de este chat" onClick={() => void store.openReview(null)}>Revisar</button>
            </span>
          )}
        </div>
      )}
    </div>
  );
}

interface AssistantBodyProps {
  tabId: string;
  msg: Msg;
  /** Another reply follows this one directly: its copy/time row is the run's. */
  midRun?: boolean;
  turnSeq?: number;
  coalescedSegments?: TranscriptTimelineSegment[];
  artifactOrigin?: string;
}

interface AssistantRowViewProps extends AssistantBodyProps {
  version: number;
}

// Pure versioned view shared by singleton messages and multi-slice turn blocks.
// `Msg` objects are intentionally updated in place, so their topic version is
// the immutable React prop that makes a changed slice cross memoization. The
// cross-slice timeline cache keeps every unaffected slice's segment reference
// stable and lets this comparator skip it completely.
const AssistantRowView = memo(function AssistantRowView({
  tabId,
  msg,
  turnSeq,
  coalescedSegments,
  artifactOrigin,
  midRun,
}: AssistantRowViewProps) {
  return (
    <AssistantSliceBody
      tabId={tabId}
      msg={msg}
      owner={msg}
      terminal={msg.turnTerminal !== false}
      turnSeq={msg.turnTerminal === false ? undefined : turnSeq}
      coalescedSegments={coalescedSegments}
      artifactOrigin={artifactOrigin}
      midRun={midRun}
    />
  );
}, (a, b) => (
  a.tabId === b.tabId
  && a.msg === b.msg
  && a.version === b.version
  && a.turnSeq === b.turnSeq
  && a.coalescedSegments === b.coalescedSegments
  && a.artifactOrigin === b.artifactOrigin
  && a.midRun === b.midRun
));

function AssistantBody({ tabId, msg, turnSeq, coalescedSegments, artifactOrigin, midRun }: AssistantBodyProps) {
  // Subscribe to this canonical message's topic; token streams bump only this.
  const version = useMsgVersion(msg.id);
  return (
    <AssistantRowView
      tabId={tabId}
      msg={msg}
      version={version}
      turnSeq={turnSeq}
      coalescedSegments={coalescedSegments}
      artifactOrigin={artifactOrigin}
      midRun={midRun}
    />
  );
}

function useTurnBlockMessageVersions(messageIdsKey: string): void {
  const messageIds = useMemo(() => messageIdsKey ? messageIdsKey.split('\0') : [], [messageIdsKey]);
  const subscribe = useCallback((callback: () => void) => {
    const unsubscribers = messageIds.map((messageId) => store.subscribe(`msg:${messageId}`, callback));
    return () => { for (const unsubscribe of unsubscribers) unsubscribe(); };
  }, [messageIds]);
  const snapshot = useCallback(() => {
    let version = 0;
    for (const messageId of messageIds) version += store.version(`msg:${messageId}`);
    return version;
  }, [messageIds]);
  useSyncExternalStore(subscribe, snapshot, snapshot);
}

interface AssistantTurnBlockProps {
  tabId: string;
  messages: Msg[];
  turnSeqs: Array<number | undefined>;
  artifactOrigin?: string;
  /** Per message: another reply follows it directly. */
  midRun?: boolean[];
}

// A steered turn can span several canonical assistant rows. Cross-row tool
// folds require those rows to share one layout calculation, but streaming must
// never subscribe the whole Transcript to their token topics. This leaf is the
// sole multi-row subscription owner; its pure row views receive primitive topic
// versions, so only the changed slice reconciles. Fragments preserve the exact
// per-message DOM shape used by ordinary singleton rows.
export function AssistantTurnBlock({ tabId, messages, turnSeqs, artifactOrigin, midRun }: AssistantTurnBlockProps) {
  const messageIdsKey = messages.map((message) => message.id).join('\0');
  useTurnBlockMessageVersions(messageIdsKey);
  const coalesced = buildCoalescedTurnBlockTimelineSegments(messages);
  return (
    <>
      {messages.map((msg, index) => (
        <div className="chatfind-message" data-chat-find-message={msg.id} key={msg.id}>
          <AssistantRowView
            tabId={tabId}
            msg={msg}
            version={store.version(`msg:${msg.id}`)}
            turnSeq={turnSeqs[index]}
            coalescedSegments={coalesced[index]}
            artifactOrigin={artifactOrigin}
            midRun={midRun?.[index]}
          />
        </div>
      ))}
    </>
  );
}

// The live pulse is deliberately rendered by Transcript outside `.doc`.
// Subscribing here preserves the message-isolated streaming path: tool,
// heartbeat and phase updates repaint this one row without coupling Transcript
// or the whole app to every token.
export function LiveTurnPulse({ msg }: { msg: Msg }) {
  useMsgVersion(msg.id);
  if (msg.status !== 'running' || msg.turnTerminal === false) return null;
  return <TurnPulse msg={msg} />;
}

// Tool calls the Working line and Worked row own: every main-thread call of
// the turn, plus subagent task rows (their inner calls live in the Turnos rail).
function isWorkingTool(t: ToolEvent): boolean {
  return !t.subagentId || isSubagentHeader(t);
}

// A step of the turn: one thought, or one call the Working line owns.
type WorkItem = ThinkingEvent | ToolEvent;

function isWorkItem(e: TimelineEvent): e is WorkItem {
  return e.kind === 'thinking' || (e.kind === 'tool' && isWorkingTool(e));
}

function workTools(items: WorkItem[]): ToolEvent[] {
  return items.filter((e): e is ToolEvent => e.kind === 'tool');
}

// A thought as plain text: links keep their label; headings, list marks,
// emphasis and code ticks go.
function thoughtPlain(markdown: string): string {
  return markdown
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/^[ \t]*(?:#{1,6}|[-*+]|\d+\.)[ \t]+/gm, '')
    .replace(/`+|\*\*|~~/g, '')
    .replace(/(^|[^\w*])[*_]([^*_\n]+)[*_](?![\w*])/g, '$1$2')
    .replace(/\s+/g, ' ')
    .trim();
}

// One line for a thought in the list (t3code's rule): a bold-only opening
// line (the Codex summary heading) wins, else its first sentence.
function thoughtPreview(markdown: string): string {
  const heading = /^\s*\*\*([^*\r\n]+)\*\*[ \t]*\r?(?:\n|$)/.exec(markdown)?.[1];
  const text = thoughtPlain(heading ?? markdown);
  if (heading !== undefined) return text;
  const end = /[.?!]["'”’)]?(?=\s)/.exec(text);
  return end ? text.slice(0, end.index + end[0].length) : text;
}

// The thought a running reply shows as its last line: the newest one, as long
// as no text has followed it yet (then it steps aside, as in t3code).
export function liveThoughtFor(msg: Msg): ThinkingEvent | null {
  for (let index = msg.events.length - 1; index >= 0; index--) {
    const event = msg.events[index];
    if (event.kind !== 'thinking') continue;
    return event.text.trim() && msg.content.slice(event.at).trim() === '' ? event : null;
  }
  return null;
}

// The latest thought as the last line of the running reply (approved mocks
// thinking-v9/v10, 2026-10-12, t3code's rule): its first sentence on one line,
// in the text, so the Working line below never moves. Clicking opens the
// whole thought in place (eight lines, then it scrolls) and it keeps following
// the newest thought while open. When the turn settles it folds into the
// Worked row with everything else.
function LiveThought({ msg }: { msg: Msg }) {
  const [open, setOpen] = useState(false);
  const thought = liveThoughtFor(msg);
  if (!thought) return null;
  const toggle = () => {
    if (window.getSelection?.()?.toString()) return;   // selecting text is not a click
    setOpen((value) => !value);
  };
  return (
    <div
      className={`livethought${open ? ' open' : ''}`}
      role="button"
      tabIndex={0}
      aria-expanded={open}
      title={open ? undefined : 'Ver el pensamiento completo'}
      onClick={toggle}
      onKeyDown={(event) => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); setOpen((value) => !value); } }}
    >
      <span className="thoughtic" aria-hidden="true"><IcThought /></span>
      {open
        ? <div className="ltfull">{parseBlocks(thought.text).map((block, index) => <MarkdownBlock key={`${index}-${block.sig}`} sb={block} />)}</div>
        : <span className="ltline">{thoughtPreview(thought.text)}</span>}
      <span className={`workchev${open ? ' open' : ''}`} aria-hidden="true">›</span>
    </div>
  );
}

function toolCount(n: number): string {
  return `${n} ${n === 1 ? 'tool' : 'tools'}`;
}

// "1m 12s" while it counts up; whole seconds, never a decimal ticking.
function fmtWorkElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  if (total < 60) return `${total}s`;
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return seconds ? `${minutes}m ${seconds}s` : `${minutes}m`;
}

function useSecondTicker(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  return now;
}

function WorkingTrail({ t, now, live }: { t: ToolEvent; now: number; live: boolean }) {
  const running = toolState(t.status) === 'running';
  if (running) {
    if (!live) return null;
    return (
      <span className="worktrail live">
        <span className="workspin" aria-hidden="true" />
        {typeof t.startedAt === 'number' ? fmtWorkElapsed(now - t.startedAt) : ''}
      </span>
    );
  }
  const dur = typeof t.startedAt === 'number' && typeof t.endedAt === 'number' ? fmtDur(t.endedAt - t.startedAt) : '';
  return dur ? <span className="worktrail">{dur}</span> : null;
}

// One thought in the list: its first line, opening to the whole reasoning
// (the row then just says "Thought", as in the approved mock thinking-v6).
function ThoughtRow({ ev }: { ev: ThinkingEvent }) {
  const [open, setOpen] = useState(false);
  return (
    <div className={`thoughtrow${open ? ' open' : ''}`}>
      <button type="button" className="thoughthead" aria-expanded={open} onClick={() => setOpen((value) => !value)}>
        <span className="thoughtic" aria-hidden="true"><IcThought /></span>
        <span className="thoughtline">{open ? 'Thought' : thoughtPreview(ev.text)}</span>
        {open && <span className="workchev open" aria-hidden="true">›</span>}
      </button>
      {open && (
        <div className="thoughtfull">
          {parseBlocks(ev.text).map((block, index) => <MarkdownBlock key={`${index}-${block.sig}`} sb={block} />)}
        </div>
      )}
    </div>
  );
}

// The steps behind a Working/Worked line, in the order they happened: each
// thought, then the calls it led to, with their durations (a live call spins
// and counts up).
function WorkList({ items, now, live }: { items: WorkItem[]; now: number; live: boolean }) {
  // The open Working list follows its newest step, like the transcript: it
  // opens at the bottom and stays there as calls and thoughts arrive, unless
  // you scrolled up to read (user 2026-10-12: it did not scroll at all).
  const list = useRef<HTMLDivElement>(null);
  const following = useRef(true);
  useLayoutEffect(() => {
    const el = list.current;
    if (live && el && following.current) el.scrollTop = el.scrollHeight;
  }, [live, items.length]);
  const onScroll = () => {
    const el = list.current;
    if (el) following.current = el.scrollHeight - el.scrollTop - el.clientHeight < 12;
  };
  return (
    <div className="worklist" ref={list} onScroll={live ? onScroll : undefined}>
      {items.map((item) => item.kind === 'thinking'
        ? <ThoughtRow key={item.key} ev={item} />
        : <ToolDetail key={item.key} t={item} standalone trail={<WorkingTrail t={item} now={now} live={live} />} />)}
    </div>
  );
}

// Observed span of the work: first call started → last call ended. Old
// history without timestamps, or a sub-second span, shows no duration.
function workSpanMs(tools: ToolEvent[]): number | null {
  const starts = tools.map((t) => t.startedAt).filter((x): x is number => typeof x === 'number');
  const ends = tools.map((t) => t.endedAt).filter((x): x is number => typeof x === 'number');
  if (!starts.length || !ends.length) return null;
  const span = Math.max(...ends) - Math.min(...starts);
  return span >= 1000 ? span : null;
}

// A settled turn's Working line: the same head without the animation, closed
// by default, opening the same list.
function WorkedRow({ items }: { items: WorkItem[] }) {
  const [open, setOpen] = useState(false);
  const tools = workTools(items);
  const span = workSpanMs(tools);
  const meta: string[] = [];
  if (span != null) meta.push(fmtWorkElapsed(span));
  if (tools.length) meta.push(toolCount(tools.length));
  return (
    <div className={`workfold${open ? ' open' : ''}`}>
      <button type="button" className="workhead" aria-expanded={open} onClick={() => setOpen((value) => !value)}>
        <span className="worklabel">Worked</span>
        {meta.length > 0 && <span className="workmeta">{meta.join(' · ')}</span>}
        <span className={`workchev${open ? ' open' : ''}`} aria-hidden="true">›</span>
      </button>
      {open && <WorkList items={items} now={Date.now()} live={false} />}
    </div>
  );
}

// The Working line (approved mock working-indicator v5 · option 3, 2026-10-12):
// an orbiting dot, "Working" with counting dots, the elapsed time and how many
// tools ran, and to its right, on the same line, the call running now. The
// line is one fixed row that never moves (user 2026-10-12): thoughts live in
// the list it opens, and the transcript always reserves its height
// (--thinklive-h), so neither the turn starting nor ending shifts the text.
// Clicking opens the turn's thoughts and calls in order, which never
// interleave with the transcript; once the turn settles they fold into its
// Worked row. Stop keeps its own "Deteniendo…" row.
function TurnPulse({ msg }: { msg: Msg }) {
  const [open, setOpen] = useState(false);
  const now = useSecondTicker();
  const pulse = store.heartbeatFor(msg.jobId);
  const stopping = !!msg.stopState;
  if (stopping) {
    return (
      <div className="thinklive">
        <div className="pulseline">
          <div className="steprow steplive stopping" role="status" aria-live="polite">
            <span className="stmark"><span className="stopdot" aria-hidden="true" /></span>
            <span className="stbody">Deteniendo…</span>
          </div>
        </div>
      </div>
    );
  }
  const items = msg.events.filter(isWorkItem);
  const tools = workTools(items);

  const elapsedMs = pulse ? pulse.elapsedMs : typeof msg.turnStartedAt === 'number' ? now - msg.turnStartedAt : null;
  const meta: string[] = [];
  if (elapsedMs != null) meta.push(fmtWorkElapsed(elapsedMs));
  if (tools.length) meta.push(toolCount(tools.length));
  const current = [...tools].reverse().find((t) => toolState(t.status) === 'running') ?? null;
  const nowLine = current ? toolLiveLine(current)
    : pulse?.phase === 'compacting' ? 'compacting context'
      : null;
  // The live call carries its action's icon, like every row in the list (user
  // 2026-10-12: all tool calls have icons).
  const nowIcon = current ? toolPresentation(current).icon : null;
  return (
    <div className={`thinklive working${open ? ' open' : ''}`}>
      <button
        type="button"
        className="workhead"
        aria-expanded={open}
        disabled={!items.length}
        onClick={() => setOpen((value) => !value)}
      >
        <span className="workorbit" aria-hidden="true" />
        <span className="worklabel" role="status" aria-live="polite">Working<span className="workdots" aria-hidden="true" /></span>
        {meta.length > 0 && <span className="workmeta">{meta.join(' · ')}</span>}
        {items.length > 0 && <span className={`workchev${open ? ' open' : ''}`} aria-hidden="true">›</span>}
        {pulse?.retry && (
          <span className="retrychip" role="status">
            <IcRetryArc />
            {`reintentando${pulse.retry.code ? ` (${pulse.retry.code})` : ''}${pulse.retry.attempt ? ` · intento ${pulse.retry.attempt}` : ''}`}
          </span>
        )}
        {!open && nowLine && (
          <span className="worknow" title={nowLine}>
            {nowIcon
              ? <span className="evt-kic" aria-hidden="true"><ActionGlyph icon={nowIcon} /></span>
              : <span aria-hidden="true">↳</span>}
            <span className="worknowtext mono">{nowLine}</span>
          </span>
        )}
      </button>
      {open && <WorkList items={items} now={now} live />}
    </div>
  );
}

// Receipt row for a session whose configured model/mode could not be applied:
// the chat is RUNNING on something else, and silence here was the receipts-law
// violation of 2026-07-27 (fable[max] configured, opus applied, only a log
// line knew). Auto-hides the moment the running selection matches the request.
function ControlsSkippedRow({ tabId }: { tabId: string }) {
  useApp();
  const chat = store.chat(tabId);
  const skipped = chat?.controlsSkipped;
  if (!chat || !skipped) return null;
  if (chat.currentModelId === skipped.requestedModelId) return null;
  const group = store.catalogGroupsForChat(chat).find((item) => item.providerId === chat.providerId);
  const unavailable = configuredModelUnavailable(group, skipped.requestedModelId);
  return (
    <div className="mismatchrow" role="status">
      <span className="mmicon" aria-hidden="true"><IcWarnTri /></span>
      <span className="mmtext">
        <b>Modelo distinto al configurado</b> — pediste <code className="mono">{skipped.requestedModelId}</code>, está corriendo <code className="mono">{chat.currentModelId ?? 'otro modelo'}</code>.
      </span>
      {unavailable ? <span className="mmtext">Elegí otro modelo abajo para continuar.</span> : <button
        className="mmretry"
        title="Volver a aplicar el modelo configurado"
        onClick={() => void store.setModel(chat.id, skipped.requestedModelId)}
      >Reintentar</button>}
    </div>
  );
}

// Canonical chronology is represented directly by the message array. Internal
// useMsgVersion drives streaming for the one active continuation segment.
export const AssistantMessage = memo(AssistantBody, (a, b) => (
  a.tabId === b.tabId
  && a.msg === b.msg
  && a.turnSeq === b.turnSeq
  && a.coalescedSegments === b.coalescedSegments
  && a.midRun === b.midRun
));
