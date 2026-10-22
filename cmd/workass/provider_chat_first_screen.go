package main

import (
	"context"
	"sort"
	"time"

	"workass/internal/chat"
	"workass/internal/wire"
)

// First screens of closed chats.
//
// A renderer opens a chat it has nothing of with its last exchange
// (ProjectLastExchangeByTab). For a closed chat that read decodes the rows
// from disk and finishes them for the wire: a few milliseconds when the newest
// reply is large, which an installation holding every chat in memory did not
// pay. A closed chat cannot change while it is closed, so its first screen is
// kept, as the reply bytes, once read; and after boot the runtime reads the
// first screens of closed chats in the background, most recently active
// first, so that the first click on any of them answers from memory.
//
// The cache is bounded (firstScreenCacheBytes; an entry larger than
// firstScreenEntryBytes is never kept), an entry is dropped when its chat
// opens, and it is only ever used for the closed chat at the revision it was
// read at, with the same tail and options: it is the bytes the read would
// return.

const (
	// firstScreenWarmLimit and firstScreenWarmOptions are the read a renderer
	// opens a chat with (CHAT_INITIAL_HISTORY and HISTORY_READ_OPTIONS in
	// desktop/renderer2). Another read is answered, and kept, on demand.
	firstScreenWarmLimit = 10
	// firstScreenWarmDelay lets boot and the first session reads finish first.
	firstScreenWarmDelay  = 2 * time.Second
	firstScreenEntryBytes = 4 << 20
	firstScreenCacheBytes = 32 << 20
)

var firstScreenWarmOptions = archiveReadOptions{deferMessageImages: true, leanTools: true, leanTitles: true}

type firstScreenEntry struct {
	revision uint64
	limit    int
	opts     archiveReadOptions
	raw      []byte
}

type firstScreenCache struct {
	entries map[string]firstScreenEntry
	bytes   int
}

// lastExchangeRaw is ProjectLastExchangeByTabRaw: a closed chat's first
// screen comes from the cache when it holds this read, and is kept when not.
func (r *providerChatRuntime) lastExchangeRaw(tabID string, limit int, opts ...archiveReadOptions) (wire.EncodedResult, bool, error) {
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return nil, found, err
	}
	return r.lastExchangeRawByChat(chatID, limit, opts...)
}

func (r *providerChatRuntime) lastExchangeRawByChat(chatID string, limit int, opts ...archiveReadOptions) (wire.EncodedResult, bool, error) {
	summary, closed := r.closedSummary(chatID)
	cacheable := closed && len(opts) <= 1
	var opt archiveReadOptions
	if len(opts) == 1 {
		opt = opts[0]
	}
	if cacheable {
		if raw, ok := r.cachedFirstScreen(summary, limit, opt); ok {
			return raw, true, nil
		}
	}
	page, found, err := r.archivePageByChat(chatID, actorHistoryTail, limit, true, opts...)
	if err != nil || !found {
		return nil, found, err
	}
	raw := page.raw()
	if cacheable {
		r.storeFirstScreen(summary, limit, opt, raw)
	}
	return raw, true, nil
}

// cachedFirstScreen returns a copy of the kept reply (the hub frames a reply
// inside its buffer) when it is this read of the chat as it is now.
func (r *providerChatRuntime) cachedFirstScreen(summary chat.ActorSummary, limit int, opts archiveReadOptions) (wire.EncodedResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.firstScreens.entries[summary.ChatID]
	current, closed := r.summaries[summary.ChatID]
	if !ok || !closed || current.Revision != summary.Revision || entry.revision != summary.Revision || entry.limit != limit || entry.opts != opts {
		return nil, false
	}
	out := make([]byte, len(entry.raw), len(entry.raw)+rawReplySpare)
	copy(out, entry.raw)
	return out, true
}

// storeFirstScreen keeps raw as the chat's first screen, if the chat is still
// closed at the revision it was read at and the entry fits the budget.
func (r *providerChatRuntime) storeFirstScreen(summary chat.ActorSummary, limit int, opts archiveReadOptions, raw []byte) {
	if len(raw) > firstScreenEntryBytes {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if current, closed := r.summaries[summary.ChatID]; !closed || current.Revision != summary.Revision {
		return
	}
	previous := len(r.firstScreens.entries[summary.ChatID].raw)
	if r.firstScreens.bytes-previous+len(raw) > firstScreenCacheBytes {
		return
	}
	if r.firstScreens.entries == nil {
		r.firstScreens.entries = make(map[string]firstScreenEntry)
	}
	r.firstScreens.entries[summary.ChatID] = firstScreenEntry{
		revision: summary.Revision, limit: limit, opts: opts, raw: append([]byte(nil), raw...),
	}
	r.firstScreens.bytes += len(raw) - previous
}

// dropFirstScreenLocked forgets the chat's first screen; the caller holds r.mu.
func (r *providerChatRuntime) dropFirstScreenLocked(chatID string) {
	if entry, ok := r.firstScreens.entries[chatID]; ok {
		r.firstScreens.bytes -= len(entry.raw)
		delete(r.firstScreens.entries, chatID)
	}
}

// runFirstScreenWarm reads the first screens of closed chats once, after
// boot, until the runtime closes.
func (r *providerChatRuntime) runFirstScreenWarm(ctx context.Context, delay time.Duration) {
	defer r.wg.Done()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	r.warmFirstScreens(ctx)
}

// warmFirstScreens reads, most recently active first, the first screen of
// every closed chat whose last exchange is small enough to keep, pausing
// between chats so that interactive reads share the connection. It returns
// how many it kept.
func (r *providerChatRuntime) warmFirstScreens(ctx context.Context) int {
	if r == nil || r.ledgers == nil {
		return 0
	}
	r.mu.Lock()
	summaries := make([]chat.ActorSummary, 0, len(r.summaries))
	for _, summary := range r.summaries {
		if !summary.Deleted && summary.LedgerCount > 0 {
			summaries = append(summaries, summary)
		}
	}
	r.mu.Unlock()
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].LastActivityAt != summaries[j].LastActivityAt {
			return summaries[i].LastActivityAt > summaries[j].LastActivityAt
		}
		return summaries[i].ChatID < summaries[j].ChatID
	})
	kept := 0
	for _, summary := range summaries {
		if ctx.Err() != nil {
			break
		}
		if r.warmFirstScreen(summary) {
			kept++
		}
		time.Sleep(time.Millisecond)
	}
	return kept
}

func (r *providerChatRuntime) warmFirstScreen(summary chat.ActorSummary) bool {
	if _, ok := r.cachedFirstScreen(summary, firstScreenWarmLimit, firstScreenWarmOptions); ok {
		return true
	}
	// Size the exchange from the rows' headers first: one far too large to
	// keep is never read in the background.
	tail, err := r.closedLastExchangeTail(summary, firstScreenWarmLimit)
	if err != nil {
		return false
	}
	stored, err := r.ledgers.LedgerBytes(summary.ChatID, closedView(summary), summary.LedgerCount-tail)
	if err != nil || stored > 2*firstScreenEntryBytes {
		return false
	}
	if _, found, err := r.lastExchangeRawByChat(summary.ChatID, firstScreenWarmLimit, firstScreenWarmOptions); err != nil || !found {
		return false
	}
	_, ok := r.cachedFirstScreen(summary, firstScreenWarmLimit, firstScreenWarmOptions)
	return ok
}
