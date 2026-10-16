// Forkable subsessions — renderer state for the fork:* channels.
//
// Kept out of store.ts on purpose: forks are a self-contained read model.
//  · fork:summary — one cheap line per local chat with fork points; feeds the
//    parent row's neutral subsession chip and its working facts.
//  · fork:list — the full snapshot of ONE chat, fetched when its sidebar group
//    is expanded, when it is the active chat, or while a child of it runs.
//  · the selected child (the main-area subsession view), the sidebar expansion
//    state, and the child transcripts/images that view reads.
// Every per-chat call carries the chat's tagged tabId/chatId so the machine
// router sends it to the daemon that owns the chat.
//
// Compatibility law: an older daemon or bridge has no fork channels. A missing
// method or a failing call silently means "no forks" — no toast, no console
// line, and no retry loop (a chat retries only when it is opened again or its
// daemon raises fork:changed for it).
//
// The parent never carries fork styling in its own surface (user 2026-10-16):
// there is no transcript marker and no fork glyph on parent rows. The fork
// facts live in the child's own view.

import { useSyncExternalStore } from 'react';
import { callThrow, has, on } from '../wire/api';
import type {
  ForkChanged, ForkChatRef, ForkChildView, ForkPointView, ForkRunView, ForkSummaryChat, ForkTranscriptEntry,
} from '../wire/types';
import { collapseForkRetries } from '../fork-format';
import { forkSelectionAfterActive, type ForkSelection } from '../fork-sidebar';
import { store } from './store';

export interface ForkChatState { points: ForkPointView[]; runs: ForkRunView[]; }
export type { ForkSelection };
export type ForkTranscriptState =
  | { status: 'loading' }
  | { status: 'ok'; entries: ForkTranscriptEntry[] }
  | { status: 'error'; error: string };
/** `src` is a small thumbnail; `full` the original, decoded only when opened. */
export interface ForkImageState { src?: string; full?: string; failed?: boolean }

const REFRESH_DEBOUNCE_MS = 120;
const IMAGE_CONCURRENCY = 3;
const IMAGE_CACHE_LIMIT = 48;
// Thumbnails render at 92×83 CSS px; 240 px on the long edge still covers a
// 2× display after the cover crop, at a fraction of a full decode's memory.
const THUMB_EDGE = 240;

/** Downscale once on arrival so a grid of tiles never holds full-size bitmaps. */
async function thumbnailOf(src: string): Promise<string> {
  if (typeof document === 'undefined' || typeof Image === 'undefined') return src;
  try {
    const image = new Image();
    image.src = src;
    await image.decode();
    const scale = Math.min(1, THUMB_EDGE / Math.max(image.naturalWidth, image.naturalHeight, 1));
    if (scale >= 1) return src;
    const canvas = document.createElement('canvas');
    canvas.width = Math.max(1, Math.round(image.naturalWidth * scale));
    canvas.height = Math.max(1, Math.round(image.naturalHeight * scale));
    const context = canvas.getContext('2d');
    if (!context) return src;
    context.imageSmoothingQuality = 'high';
    context.drawImage(image, 0, 0, canvas.width, canvas.height);
    return canvas.toDataURL('image/webp', 0.86);
  } catch {
    return src;
  }
}
const TRANSCRIPT_CACHE_LIMIT = 24;
const UI_KEY = 'workass.sv2.forks';
const UI_LIMIT = 200;

function errorText(err: unknown): string {
  if (err instanceof Error && err.message) return err.message;
  return typeof err === 'string' && err ? err : 'Error desconocido.';
}

function operationId(): string {
  const random = typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
    ? crypto.randomUUID()
    : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
  return `fork-op-${random}`;
}

function childSignature(child: ForkChildView | undefined): string {
  return child ? `${child.childId}|${child.status}|${child.turns}|${child.activity ?? ''}|${child.transcriptRetained}` : '';
}

// Sidebar disclosure state, persisted like the sidebar's project scope.
interface ForkUiState { chats: Record<string, boolean>; }
function loadUi(): ForkUiState {
  try {
    const raw = typeof localStorage === 'undefined' ? null : localStorage.getItem(UI_KEY);
    const parsed = raw ? JSON.parse(raw) as Partial<ForkUiState> : null;
    return {
      chats: parsed?.chats && typeof parsed.chats === 'object' ? { ...parsed.chats } : {},
    };
  } catch {
    return { chats: {} };
  }
}

class ForkStore {
  private listeners = new Set<() => void>();
  private imageListeners = new Set<() => void>();
  private versionValue = 0;
  private imageVersionValue = 0;
  private byTab = new Map<string, ForkChatState>();
  private summaryEntries: ForkSummaryChat[] = [];
  private timers = new Map<string, ReturnType<typeof setTimeout>>();
  private summaryTimer: ReturnType<typeof setTimeout> | null = null;
  private inflight = new Map<string, Promise<void>>();
  private again = new Set<string>();
  private summaryInflight: Promise<void> | null = null;
  private summaryAgain = false;
  private installed = false;
  private transcripts = new Map<string, ForkTranscriptState>();
  private transcriptFor = new Map<string, string>();
  private images = new Map<string, ForkImageState>();
  private imageQueue: Array<() => Promise<void>> = [];
  private imagesActive = 0;
  private ui: ForkUiState = loadUi();
  private shownAll = new Set<string>();
  private lastActiveId: string | null = null;
  selected: ForkSelection | null = null;

  // ---- subscription plumbing --------------------------------------------
  subscribe = (cb: () => void) => { this.listeners.add(cb); return () => { this.listeners.delete(cb); }; };
  subscribeImages = (cb: () => void) => { this.imageListeners.add(cb); return () => { this.imageListeners.delete(cb); }; };
  version = () => this.versionValue;
  imageVersion = () => this.imageVersionValue;
  private bump() {
    this.versionValue += 1;
    for (const cb of Array.from(this.listeners)) cb();
  }
  private bumpImages() {
    this.imageVersionValue += 1;
    for (const cb of Array.from(this.imageListeners)) cb();
  }

  /**
   * Once per window: subscribe to fork:changed, follow chat switches (they end
   * a child selection) and reconnects, and read the summary.
   */
  start() {
    if (this.installed) return;
    this.installed = true;
    on('onForkChanged', (payload: ForkChanged) => this.onChanged(payload));
    this.lastActiveId = store.state.activeId ?? null;
    store.subscribe('app', () => this.followActiveChat());
    let connection = store.state.connection;
    store.subscribe('conn', () => {
      const next = store.state.connection;
      if (next === 'connected' && connection !== 'connected') this.scheduleSummary();
      connection = next;
    });
    void this.refreshSummary();
  }

  private followActiveChat() {
    const next = store.state.activeId ?? null;
    const previous = this.lastActiveId;
    this.lastActiveId = next;
    if (previous === next) return;
    const kept = forkSelectionAfterActive(this.selected, previous, next);
    if (kept !== this.selected) { this.selected = kept; this.bump(); }
  }

  private onChanged(payload: ForkChanged | null | undefined) {
    if (!payload || !payload.tabId || !payload.chatId) return;
    this.scheduleSummary();
    // Full snapshots are kept only for chats this window has looked at.
    if (!this.byTab.has(payload.tabId) && store.state.activeId !== payload.tabId) return;
    this.scheduleRefresh({ tabId: payload.tabId, chatId: payload.chatId });
  }

  // ---- reads ---------------------------------------------------------------
  /** The full snapshot, or undefined when this window never fetched it. */
  chat(tabId: string | null | undefined): ForkChatState | undefined {
    return tabId ? this.byTab.get(tabId) : undefined;
  }
  summary(): readonly ForkSummaryChat[] { return this.summaryEntries; }
  run(tabId: string, runId: string): ForkRunView | undefined {
    return this.chat(tabId)?.runs.find((run) => run.runId === runId);
  }
  point(tabId: string, forkPointId: string): ForkPointView | undefined {
    return this.chat(tabId)?.points.find((point) => point.forkPointId === forkPointId);
  }
  /** The newest attempt of a child chain, i.e. what its row currently shows. */
  slotChild(tabId: string, runId: string, rootId: string): ForkChildView | undefined {
    const run = this.run(tabId, runId);
    return run ? collapseForkRetries(run.children).find((slot) => slot.rootId === rootId)?.child : undefined;
  }

  // ---- fetching ------------------------------------------------------------
  /** On chat open / group expand: fetch once if this window has no snapshot yet. */
  ensure(ref: Partial<ForkChatRef> | null | undefined) {
    if (!ref?.tabId || !ref.chatId) return;
    this.start();
    if (this.byTab.has(ref.tabId)) { this.scheduleRefresh(ref as ForkChatRef); return; }
    void this.refresh(ref as ForkChatRef);
  }

  scheduleRefresh(ref: ForkChatRef) {
    const existing = this.timers.get(ref.tabId);
    if (existing) clearTimeout(existing);
    this.timers.set(ref.tabId, setTimeout(() => {
      this.timers.delete(ref.tabId);
      void this.refresh(ref);
    }, REFRESH_DEBOUNCE_MS));
  }

  refresh(ref: ForkChatRef): Promise<void> {
    const running = this.inflight.get(ref.tabId);
    if (running) { this.again.add(ref.tabId); return running; }
    const task = this.fetchList(ref).finally(() => {
      this.inflight.delete(ref.tabId);
      if (this.again.delete(ref.tabId)) void this.refresh(ref);
    });
    this.inflight.set(ref.tabId, task);
    return task;
  }

  private async fetchList(ref: ForkChatRef) {
    let next: ForkChatState = { points: [], runs: [] };
    if (has('forkList')) {
      try {
        const reply = await callThrow('forkList', ref.tabId, ref.chatId);
        if (reply && reply.ok !== false) {
          next = {
            points: Array.isArray(reply.forkPoints) ? reply.forkPoints : [],
            runs: Array.isArray(reply.runs) ? reply.runs : [],
          };
        }
      } catch {
        // Older daemon ("unknown channel") or a machine that is down: no forks.
      }
    }
    this.apply(ref.tabId, next);
  }

  private apply(tabId: string, next: ForkChatState) {
    const prev = this.byTab.get(tabId);
    this.byTab.set(tabId, next);
    this.syncSelectedTranscript(prev);
    this.bump();
  }

  scheduleSummary() {
    if (this.summaryTimer) clearTimeout(this.summaryTimer);
    this.summaryTimer = setTimeout(() => { this.summaryTimer = null; void this.refreshSummary(); }, REFRESH_DEBOUNCE_MS);
  }

  refreshSummary(): Promise<void> {
    if (this.summaryInflight) { this.summaryAgain = true; return this.summaryInflight; }
    this.summaryInflight = this.fetchSummary().finally(() => {
      this.summaryInflight = null;
      if (this.summaryAgain) { this.summaryAgain = false; void this.refreshSummary(); }
    });
    return this.summaryInflight;
  }

  private async fetchSummary() {
    let entries: ForkSummaryChat[] = [];
    if (has('forkSummary')) {
      try {
        const reply = await callThrow('forkSummary');
        if (reply && reply.ok !== false && Array.isArray(reply.chats)) entries = reply.chats;
      } catch {
        // Older daemon: no summary, so collapsed rows simply carry no badge.
      }
    }
    this.summaryEntries = entries;
    // A running child needs its start time for the row's "Trabajando 18s",
    // which only the full snapshot carries. Running chats are few.
    for (const entry of entries) {
      if ((entry.counts?.running ?? 0) > 0 && entry.tabId && entry.chatId) this.ensure({ tabId: entry.tabId, chatId: entry.chatId });
    }
    this.bump();
  }

  // ---- sidebar disclosure --------------------------------------------------
  chatOpenExplicit(tabId: string): boolean | undefined {
    return Object.prototype.hasOwnProperty.call(this.ui.chats, tabId) ? this.ui.chats[tabId] : undefined;
  }
  setChatOpen(ref: ForkChatRef, open: boolean) {
    delete this.ui.chats[ref.tabId];
    this.ui.chats[ref.tabId] = open;
    const keys = Object.keys(this.ui.chats);
    for (const key of keys.slice(0, Math.max(0, keys.length - UI_LIMIT))) delete this.ui.chats[key];
    this.saveUi();
    if (open) this.ensure(ref);
    this.bump();
  }
  /** "Mostrar N más" is per parent chat (its subsessions are one flat list). */
  allShown(tabId: string): boolean { return this.shownAll.has(tabId); }
  showAll(tabId: string) { this.shownAll.add(tabId); this.bump(); }
  private saveUi() {
    try { localStorage.setItem(UI_KEY, JSON.stringify(this.ui)); } catch { /* private mode */ }
  }

  // ---- child selection (the main-area subsession view) ---------------------
  /** Pick a child: its parent becomes the active chat and the child view replaces the transcript. */
  select(next: ForkSelection) {
    this.start();
    this.selected = { ...next };
    if (store.state.activeId !== next.tabId) store.switchChat(next.tabId);
    this.ensure({ tabId: next.tabId, chatId: next.chatId });
    this.loadTranscript(true);
    this.bump();
  }
  clearSelection() {
    if (!this.selected) return;
    this.selected = null;
    this.bump();
  }
  isSelected(tabId: string, runId: string, rootId: string): boolean {
    const s = this.selected;
    return !!s && s.tabId === tabId && s.runId === runId && s.rootId === rootId;
  }
  selectedFor(tabId: string | null | undefined): ForkSelection | null {
    return this.selected && this.selected.tabId === tabId ? this.selected : null;
  }

  transcriptOf(runId: string, childId: string): ForkTranscriptState | undefined {
    return this.transcripts.get(`${runId}:${childId}`);
  }

  /** Re-read the selected child's transcript when its row moved on (status/turns/activity). */
  private syncSelectedTranscript(prev: ForkChatState | undefined) {
    const s = this.selected;
    if (!s) return;
    const before = prev?.runs.find((run) => run.runId === s.runId);
    const prevChild = before ? collapseForkRetries(before.children).find((slot) => slot.rootId === s.rootId)?.child : undefined;
    const child = this.slotChild(s.tabId, s.runId, s.rootId);
    if (childSignature(prevChild) !== childSignature(child)) this.loadTranscript(false);
  }

  private loadTranscript(force: boolean) {
    const s = this.selected;
    if (!s) return;
    const child = this.slotChild(s.tabId, s.runId, s.rootId);
    if (!child) return;
    const key = `${s.runId}:${child.childId}`;
    const signature = childSignature(child);
    if (!force && this.transcriptFor.get(key) === signature && this.transcripts.get(key)?.status === 'ok') return;
    if (child.status === 'queued') return;
    this.transcriptFor.set(key, signature);
    if (!this.transcripts.has(key)) this.transcripts.set(key, { status: 'loading' });
    void (async () => {
      let state: ForkTranscriptState;
      try {
        const reply = await callThrow('forkTranscript', { tabId: s.tabId, chatId: s.chatId, runId: s.runId, childId: child.childId });
        state = !reply
          ? { status: 'error', error: 'Este Workass no expone el transcript de los hijos.' }
          : reply.ok && Array.isArray(reply.entries)
            ? { status: 'ok', entries: reply.entries }
            : { status: 'error', error: reply.error || 'El transcript ya no está disponible.' };
      } catch (err) {
        state = { status: 'error', error: errorText(err) };
      }
      this.transcripts.delete(key);
      this.transcripts.set(key, state);
      while (this.transcripts.size > TRANSCRIPT_CACHE_LIMIT) {
        const oldest = this.transcripts.keys().next().value;
        if (oldest === undefined) break;
        this.transcripts.delete(oldest);
        this.transcriptFor.delete(oldest);
      }
      this.bump();
    })();
  }

  // ---- images (lazy, cached in memory, a few at a time) --------------------
  image(runId: string, childId: string, index: number): ForkImageState | undefined {
    return this.images.get(`${runId}:${childId}:${index}`);
  }

  loadImage(ref: ForkChatRef, runId: string, childId: string, index: number) {
    const key = `${runId}:${childId}:${index}`;
    if (this.images.has(key)) return;
    this.images.set(key, {});
    this.imageQueue.push(async () => {
      let next: ForkImageState;
      try {
        const reply = await callThrow('forkImage', { ...ref, runId, childId, index });
        if (reply?.ok && typeof reply.data === 'string' && reply.data) {
          const full = reply.data.startsWith('data:') ? reply.data : `data:${reply.mimeType || 'image/png'};base64,${reply.data}`;
          next = { src: await thumbnailOf(full), full };
        } else {
          next = { failed: true };
        }
      } catch {
        next = { failed: true };
      }
      this.images.set(key, next);
      while (this.images.size > IMAGE_CACHE_LIMIT) {
        const oldest = this.images.keys().next().value;
        if (oldest === undefined) break;
        this.images.delete(oldest);
      }
      this.bumpImages();
    });
    this.pumpImages();
  }

  private pumpImages() {
    while (this.imagesActive < IMAGE_CONCURRENCY && this.imageQueue.length) {
      const job = this.imageQueue.shift()!;
      this.imagesActive += 1;
      void job().finally(() => { this.imagesActive -= 1; this.pumpImages(); });
    }
  }

  // ---- actions -------------------------------------------------------------
  async cancelChild(ref: ForkChatRef, runId: string, childId: string): Promise<void> {
    try {
      const reply = await callThrow('forkCancelChild', { ...ref, runId, childId, operationId: operationId() });
      if (reply && !reply.ok) store.addToast('No se pudo detener', reply.error || 'El daemon rechazó la parada.');
    } catch (err) {
      store.addToast('No se pudo detener', errorText(err));
    }
    this.scheduleRefresh(ref);
  }

  async retryChild(ref: ForkChatRef, runId: string, childId: string): Promise<void> {
    try {
      const reply = await callThrow('forkRetryChild', { ...ref, runId, childId, operationId: operationId() });
      if (reply && !reply.ok) store.addToast('No se pudo reintentar', reply.error || 'El daemon rechazó el reintento.');
    } catch (err) {
      store.addToast('No se pudo reintentar', errorText(err));
    }
    this.scheduleRefresh(ref);
  }

  async retain(ref: ForkChatRef, runId: string): Promise<void> {
    try {
      const reply = await callThrow('forkRetain', { ...ref, runId, keep: true });
      if (reply && !reply.ok) store.addToast('No se pudo conservar', reply.error || 'El daemon rechazó el cambio.');
      else if (reply?.run) {
        const state = this.byTab.get(ref.tabId);
        if (state) this.apply(ref.tabId, { ...state, runs: state.runs.map((run) => (run.runId === runId ? reply.run! : run)) });
      }
    } catch (err) {
      store.addToast('No se pudo conservar', errorText(err));
    }
    this.scheduleRefresh(ref);
  }
}

export const forkStore = new ForkStore();

/** Fine-grained: every fork snapshot, summary, disclosure or selection change. */
export function useForks(): number {
  return useSyncExternalStore(forkStore.subscribe, forkStore.version, forkStore.version);
}

/** Thumbnails only, so twelve images arriving never repaint the sidebar. */
export function useForkImages(): number {
  return useSyncExternalStore(forkStore.subscribeImages, forkStore.imageVersion, forkStore.imageVersion);
}
