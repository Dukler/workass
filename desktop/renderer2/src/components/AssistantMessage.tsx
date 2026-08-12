import { memo } from 'react';
import type { MessageImage, Msg, ThinkingEvent, TimelineEvent } from '../store/types';
import { store, useApp, useMsgVersion, useConnStatus, useClock } from '../store/store';
import { parseBlocks } from '../markdown/blocks';
import { MarkdownBlock } from '../markdown/MarkdownBlock';
import { StepRow, StepWords, ToolGroup, PermCard, CompactionRow, RestoredRow } from './messages';
import { IcStampCopy, IcWarnTri, IcRetryArc } from '../icons';
import { whimsyFor } from '../turn-whimsy';
import { buildTranscriptTimelineSegments, stableMarkdownBlockKeys } from '../timeline-layout';
import type { TranscriptTimelineSegment } from '../timeline-layout';
import { fullAssistantText } from '../assistant-output';
import { relTime } from '../rel-time';
import type { WorkassRuntimeProfile } from '../model-catalog';
import { messageImageSrc } from '../image-drafts';
import { normalizeMarkdownTarget, type InlineMediaResolver } from '../markdown/inline';

function assistantMediaResolver(images: MessageImage[] | undefined): InlineMediaResolver {
  const bySource = new Map<string, MessageImage>();
  for (const image of images ?? []) {
    if (image.source) bySource.set(normalizeMarkdownTarget(image.source), image);
  }
  return {
    revision: images ?? null,
    resolve: (target) => {
      const image = bySource.get(normalizeMarkdownTarget(target));
      return image ? { src: messageImageSrc(image), alt: image.name || 'Imagen' } : null;
    },
    open: (media) => store.openImageLightbox(media.src, media.alt),
  };
}

function StructuredAssistantImages({ images }: { images: MessageImage[] | undefined }) {
  const structured = (images ?? []).filter((image) => !image.source);
  if (!structured.length) return null;
  return (
    <div className="assistant-images" aria-label="Imágenes de la respuesta">
      {structured.map((image, index) => {
        const src = messageImageSrc(image);
        const alt = image.name || `Imagen ${index + 1}`;
        return (
          <button key={`${image.mimeType}-${index}`} type="button" className="assistant-inline-image" title="Ampliar" onClick={() => store.openImageLightbox(src, alt)}>
            <img src={src} alt={alt} />
          </button>
        );
      })}
    </div>
  );
}

function EventView({ ev }: { ev: TimelineEvent }) {
  if (ev.kind === 'thinking') return <StepRow ev={ev} />;
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
  connection,
  profile,
  coalescedSegments,
}: {
  tabId: string;
  msg: Msg;
  owner: Msg;
  terminal: boolean;
  turnSeq?: number;
  connection: ReturnType<typeof useConnStatus>;
  profile: WorkassRuntimeProfile;
  coalescedSegments?: TranscriptTimelineSegment[];
}) {
  const segs = coalescedSegments ?? buildTranscriptTimelineSegments(msg, profile);
  const parsedSegments = segs.map((segment) => 'prose' in segment ? parseBlocks(segment.prose) : null);
  const resultBlocks = msg.result ? parseBlocks(msg.result) : [];
  const media = assistantMediaResolver(msg.images);
  const visualizeChatId = store.chat(tabId)?.chatId ?? '';
  const blockKeys = stableMarkdownBlockKeys(msg, parsedSegments.flatMap((blocks) => blocks?.map((block) => block.sig) ?? []));
  let blockIndex = 0;
  const running = terminal && msg.status === 'running';
  // The settled thinking row renders at the TAIL of the turn, never inline
  // where the event landed. While running, Transcript owns the live pulse as
  // scrollport chrome so tool/prose growth cannot move it (user 2026-08-08).
  //
  // It is pulled out in BOTH states, not just while running: the store keeps a
  // single thinking event per message anchored at `at: msg.content.length` from
  // when it first arrived (store.ts), so letting it fall back inline on
  // completion would snap it from the bottom of the turn up above the answer.
  const thinkEv: ThinkingEvent | null =
    [...msg.events].reverse().find((e): e is ThinkingEvent => e.kind === 'thinking') ?? null;
  // A turn cut off by a daemon disconnect: shown as a quiet "sin conexión" row
  // with a retry, not the generic model-error stamp.
  const interrupted = terminal && msg.status === 'failed' && !!msg.interrupted;
  const showStamp = terminal && (msg.status === 'done' || msg.status === 'failed' || msg.status === 'cancelled') && !interrupted;
  // R4/R5: a completed turn that recorded a checkpoint (changed tracked files)
  // gets per-turn Deshacer/Revisar affordances, revealed on hover.
  const hasCheckpoint = terminal && turnSeq != null && (msg.status === 'done' || msg.status === 'failed');

  return (
    <div className="amsg">
      {running && <ControlsSkippedRow tabId={tabId} />}
      {segs.map((s, segmentIndex) => {
        if ('tools' in s) return <ToolGroup key={s.key} tools={s.tools} revision={s.revision} />;
        if ('event' in s) return s.event.key === thinkEv?.key ? null : <EventView key={s.event.key} ev={s.event} />;
        return parsedSegments[segmentIndex]!.map((sb) => <MarkdownBlock key={blockKeys[blockIndex++]} sb={sb} media={media} visualizeTabId={tabId} visualizeChatId={visualizeChatId} />);
      })}

      {resultBlocks.map((block, index) => <MarkdownBlock key={`result-${index}-${block.sig}`} sb={block} media={media} visualizeTabId={tabId} visualizeChatId={visualizeChatId} />)}
      <StructuredAssistantImages images={msg.images} />

      {!running && thinkEv && <StepRow ev={thinkEv} />}

      {interrupted && (
        <div className="connfail" role="status">
          <span className="cfdot" aria-hidden="true" />
          <span className="cftext">
            {msg.content !== '' || msg.events.length > 0 ? 'Interrumpido · conexión perdida' : 'No se pudo enviar · sin conexión'}
          </span>
          {msg.retryPrompt && (
            <button
              className="cfretry"
              disabled={connection !== 'connected'}
              title={connection === 'connected' ? 'Reintentar el turno' : 'Sin conexión con el daemon'}
              onClick={() => void store.retryTurn(tabId, owner.id)}
            >Reintentar</button>
          )}
        </div>
      )}

      {terminal && msg.permission && <PermCard perm={msg.permission} tabId={tabId} msgId={owner.id} />}

      {showStamp && (
        <div className="stamp">
          <span title="Copiar turno" onClick={() => { void navigator.clipboard?.writeText(fullAssistantText(owner)); }}><IcStampCopy /></span>
          {msg.status === 'cancelled' && <span>detenido</span>}
          {msg.status === 'failed' && <span className="turnfail">error</span>}
          <RelStamp at={msg.at} />
          {hasCheckpoint && (
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
  turnSeq?: number;
  profile: WorkassRuntimeProfile;
  coalescedSegments?: TranscriptTimelineSegment[];
}

function AssistantBody({ tabId, msg, turnSeq, profile, coalescedSegments }: AssistantBodyProps) {
  // Subscribe to this canonical message's topic; token streams bump only this.
  useMsgVersion(msg.id);
  // Only re-renders on the rare connection transition (dedicated topic), so the
  // Reintentar affordance can enable/disable live without APP-bump coupling.
  const connection = useConnStatus();
  return (
    <AssistantSliceBody
      tabId={tabId}
      msg={msg}
      owner={msg}
      terminal={msg.turnTerminal !== false}
      turnSeq={msg.turnTerminal === false ? undefined : turnSeq}
      connection={connection}
      profile={profile}
      coalescedSegments={coalescedSegments}
    />
  );
}

function fmtElapsed(ms: number): string {
  const total = Math.max(0, Math.round(ms / 1000));
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return `${minutes}:${String(seconds).padStart(2, '0')}`;
}

function fmtTokens(n: number): string {
  if (n < 1000) return String(n);
  return `${(n / 1000).toFixed(1).replace('.', ',')}k`;
}

// The live pulse is deliberately rendered by Transcript outside `.doc`.
// Subscribing here preserves the message-isolated streaming path: thought,
// heartbeat and phase updates repaint this one row without coupling Transcript
// or the whole app to every token.
export function LiveTurnPulse({ msg }: { msg: Msg }) {
  useMsgVersion(msg.id);
  const thinkEv = [...msg.events].reverse().find((e): e is ThinkingEvent => e.kind === 'thinking') ?? null;
  if (msg.status !== 'running' || msg.turnTerminal === false) return null;
  return <TurnPulse msg={msg} thinkEv={thinkEv} />;
}

// The turn pulse: the tail row is alive for the WHOLE turn, not only while
// thinking text streams. A max-effort turn can think silently for minutes and
// used to be indistinguishable from a dead chat (2026-07-27) — this row is
// where the heartbeat events land. Honest states (retry, compaction, writing)
// override the whimsy label; step titles beat both. The tool phase deliberately
// does NOT: `usando Bash` pasted a capitalized tool id into a lowercase sentence
// and, since tools run for most of a turn, it swallowed the whimsy word almost
// always (2026-07-29). The tool rows below already name the tool.
function TurnPulse({ msg, thinkEv }: { msg: Msg; thinkEv: ThinkingEvent | null }) {
  const pulse = store.heartbeatFor(msg.jobId);
  const whimsy = `${whimsyFor(msg.jobId)}…`;
  const phase = pulse?.phase ?? '';
  const phaseLabel = phase === 'writing' ? 'escribiendo…'
    : phase === 'compacting' ? 'compactando contexto…'
      : null;
  const vitals: string[] = [];
  if (pulse) {
    vitals.push(fmtElapsed(pulse.elapsedMs));
    if (pulse.outputTokens > 0) vitals.push(`${fmtTokens(pulse.outputTokens)} tokens`);
  }
  return (
    <div className="thinklive">
      <div className="pulseline">
        {phaseLabel ? (
          <div className="steprow steplive" role="status" aria-live="polite">
            <span className="stmark"><span className="ping" aria-hidden="true" /></span>
            <span className="stbody"><StepWords key={phaseLabel} text={phaseLabel} /></span>
          </div>
        ) : thinkEv ? (
          <StepRow ev={thinkEv} live fallbackLabel={whimsy} />
        ) : (
          <div className="steprow steplive" role="status" aria-live="polite">
            <span className="stmark"><span className="ping" aria-hidden="true" /></span>
            <span className="stbody"><StepWords key={whimsy} text={whimsy} /></span>
          </div>
        )}
        {pulse?.retry && (
          <span className="retrychip" role="status">
            <IcRetryArc />
            {`reintentando${pulse.retry.code ? ` (${pulse.retry.code})` : ''}${pulse.retry.attempt ? ` · intento ${pulse.retry.attempt}` : ''}`}
          </span>
        )}
        {vitals.length > 0 && <span className="pulsevitals mono">{vitals.join(' · ')}</span>}
      </div>
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
  return (
    <div className="mismatchrow" role="status">
      <span className="mmicon" aria-hidden="true"><IcWarnTri /></span>
      <span className="mmtext">
        <b>Modelo distinto al configurado</b> — pediste <code className="mono">{skipped.requestedModelId}</code>, está corriendo <code className="mono">{chat.currentModelId ?? 'otro modelo'}</code>.
      </span>
      <button
        className="mmretry"
        title="Volver a aplicar el modelo configurado"
        onClick={() => void store.setModel(chat.id, skipped.requestedModelId)}
      >Reintentar</button>
    </div>
  );
}

// Canonical chronology is represented directly by the message array. Internal
// useMsgVersion drives streaming for the one active continuation segment.
export const AssistantMessage = memo(AssistantBody, (a, b) => (
  a.tabId === b.tabId
  && a.msg === b.msg
  && a.turnSeq === b.turnSeq
  && a.profile === b.profile
  && a.coalescedSegments === b.coalescedSegments
));
