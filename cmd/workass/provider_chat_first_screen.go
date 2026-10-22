package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"workass/internal/chat"
	"workass/internal/wire"
)

// First screens and first slices of closed chats.
//
// A renderer opens a chat it has nothing of with ProjectLastExchangeByTab and
// then reads, page by page before the oldest row it has, the rest of its
// first slice (ten rows). For a closed chat each of those reads decodes rows
// from disk and finishes them for the wire, where an installation holding
// every chat in memory only encoded them: on the 298 MB chat the slice took
// 0.3 s against 0.13 s. A closed chat cannot change while it is closed, so
// those reads are kept, as their reply bytes:
//
//   - the first screen of every closed chat, once read (firstScreenCacheBytes,
//     no entry over firstScreenEntryBytes);
//   - the pages completing the first slice of the most recently active
//     closed chats (slicePageCacheBytes; a more recently active chat evicts a
//     less recent one's);
//   - for every closed chat, whether its tail is small enough to be its first
//     screen whole, so a large one is never read again just to learn that.
//
// After boot the runtime reads all of this in the background, most recently
// active first, pausing between chats so that interactive reads share the
// connection; a chat closed again after being idle is read again the same
// way. Everything kept is dropped when its chat opens, and is only ever used
// for the closed chat at the revision it was read at, with the same read: it
// is the bytes that read returns.

const (
	// firstScreenWarmLimit and firstScreenWarmOptions are the read a renderer
	// opens a chat with (CHAT_INITIAL_HISTORY and HISTORY_READ_OPTIONS in
	// desktop/renderer2). Another read is answered, and kept, on demand.
	firstScreenWarmLimit = 10
	// firstScreenWarmDelay lets boot and the first session reads finish first.
	firstScreenWarmDelay  = 2 * time.Second
	firstScreenEntryBytes = 4 << 20
	firstScreenCacheBytes = 32 << 20
	// slicePageCacheBytes holds the rest of the first slice of the most
	// recently active chats: the five largest of the user's need 21 MB.
	slicePageCacheBytes = 32 << 20
	// warmTailStoredBytes: a chat whose newest rows are stored larger than
	// this is never read in the background (it is read when it is opened).
	warmTailStoredBytes = 32 << 20
	// lastExchangeWholeTailBytes: a tail up to this size is a chat's first
	// screen whole. Reading and painting it costs about what its last exchange
	// alone does, and it spares the second read.
	lastExchangeWholeTailBytes = 2 << 20
)

var firstScreenWarmOptions = archiveReadOptions{deferMessageImages: true, leanTools: true, leanTitles: true}

type firstScreenEntry struct {
	revision uint64
	limit    int
	opts     archiveReadOptions
	raw      []byte
}

// tailSize is what a closed chat's tail came to under one read.
type tailSize struct {
	revision uint64
	limit    int
	opts     archiveReadOptions
	small    bool
}

type slicePageKey struct {
	before string
	limit  int
}

// sliceEntry is the pages a renderer reads after a chat's first screen.
type sliceEntry struct {
	revision uint64
	opts     archiveReadOptions
	activity int64
	pages    map[slicePageKey][]byte
	bytes    int
}

type firstScreenCache struct {
	entries    map[string]firstScreenEntry
	bytes      int
	tails      map[string]tailSize
	slices     map[string]sliceEntry
	sliceBytes int
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
	opt := singleReadOption(opts)
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

func singleReadOption(opts []archiveReadOptions) archiveReadOptions {
	if len(opts) == 1 {
		return opts[0]
	}
	return archiveReadOptions{}
}

// closedCurrentLocked reports whether the chat is closed at summary's
// revision; the caller holds r.mu.
func (r *providerChatRuntime) closedCurrentLocked(summary chat.ActorSummary) bool {
	current, closed := r.summaries[summary.ChatID]
	return closed && current.Revision == summary.Revision
}

// copyReply is a kept reply as a reply the hub may frame in place.
func copyReply(raw []byte) wire.EncodedResult {
	out := make([]byte, len(raw), len(raw)+rawReplySpare)
	copy(out, raw)
	return out
}

// cachedFirstScreen returns a copy of the kept reply (the hub frames a reply
// inside its buffer) when it is this read of the chat as it is now.
func (r *providerChatRuntime) cachedFirstScreen(summary chat.ActorSummary, limit int, opts archiveReadOptions) (wire.EncodedResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.firstScreens.entries[summary.ChatID]
	if !ok || !r.closedCurrentLocked(summary) || entry.revision != summary.Revision || entry.limit != limit || entry.opts != opts {
		return nil, false
	}
	return copyReply(entry.raw), true
}

// storeFirstScreen keeps raw as the chat's first screen, if the chat is still
// closed at the revision it was read at and the entry fits the budget.
func (r *providerChatRuntime) storeFirstScreen(summary chat.ActorSummary, limit int, opts archiveReadOptions, raw []byte) {
	if len(raw) > firstScreenEntryBytes {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.closedCurrentLocked(summary) {
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

// tailKnownLarge reports whether this read of the closed chat's tail is known
// to be larger than a whole first screen.
func (r *providerChatRuntime) tailKnownLarge(summary chat.ActorSummary, limit int, opts []archiveReadOptions) bool {
	if len(opts) > 1 {
		return false
	}
	opt := singleReadOption(opts)
	r.mu.Lock()
	defer r.mu.Unlock()
	known, ok := r.firstScreens.tails[summary.ChatID]
	return ok && r.closedCurrentLocked(summary) && known.revision == summary.Revision && known.limit == limit && known.opts == opt && !known.small
}

// recordTailSize remembers what the closed chat's tail came to.
func (r *providerChatRuntime) recordTailSize(summary chat.ActorSummary, limit int, opts []archiveReadOptions, small bool) {
	if len(opts) > 1 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.closedCurrentLocked(summary) {
		return
	}
	if r.firstScreens.tails == nil {
		r.firstScreens.tails = make(map[string]tailSize)
	}
	r.firstScreens.tails[summary.ChatID] = tailSize{revision: summary.Revision, limit: limit, opts: singleReadOption(opts), small: small}
}

// cachedSlicePage returns a copy of a kept page of a closed chat's first
// slice when it is this read of the chat as it is now.
func (r *providerChatRuntime) cachedSlicePage(chatID, beforeMessageID string, limit int, opts ...archiveReadOptions) (wire.EncodedResult, bool) {
	if len(opts) > 1 {
		return nil, false
	}
	opt := singleReadOption(opts)
	chatID = strings.TrimSpace(chatID)
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.firstScreens.slices[chatID]
	current, closed := r.summaries[chatID]
	if !ok || !closed || current.Revision != entry.revision || entry.opts != opt {
		return nil, false
	}
	raw, ok := entry.pages[slicePageKey{before: strings.TrimSpace(beforeMessageID), limit: limit}]
	if !ok {
		return nil, false
	}
	return copyReply(raw), true
}

// storeSlice keeps the pages of a closed chat's first slice if the chat is
// still closed at the revision they were read at, evicting less recently
// active chats' slices to make room; a chat less recent than all of them is
// not kept.
func (r *providerChatRuntime) storeSlice(summary chat.ActorSummary, opts archiveReadOptions, pages map[slicePageKey][]byte) bool {
	size := 0
	for _, raw := range pages {
		size += len(raw)
	}
	if size == 0 || size > slicePageCacheBytes {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.closedCurrentLocked(summary) {
		return false
	}
	cache := &r.firstScreens
	cache.dropSliceLocked(summary.ChatID)
	for cache.sliceBytes+size > slicePageCacheBytes {
		oldest, found := "", false
		var oldestActivity int64
		for chatID, entry := range cache.slices {
			if entry.activity < summary.LastActivityAt && (!found || entry.activity < oldestActivity) {
				oldest, oldestActivity, found = chatID, entry.activity, true
			}
		}
		if !found {
			return false
		}
		cache.dropSliceLocked(oldest)
	}
	if cache.slices == nil {
		cache.slices = make(map[string]sliceEntry)
	}
	kept := make(map[slicePageKey][]byte, len(pages))
	for key, raw := range pages {
		kept[key] = append([]byte(nil), raw...)
	}
	cache.slices[summary.ChatID] = sliceEntry{revision: summary.Revision, opts: opts, activity: summary.LastActivityAt, pages: kept, bytes: size}
	cache.sliceBytes += size
	return true
}

func (c *firstScreenCache) dropSliceLocked(chatID string) {
	if entry, ok := c.slices[chatID]; ok {
		c.sliceBytes -= entry.bytes
		delete(c.slices, chatID)
	}
}

// dropFirstScreenLocked forgets everything kept for the chat; the caller holds
// r.mu.
func (r *providerChatRuntime) dropFirstScreenLocked(chatID string) {
	if entry, ok := r.firstScreens.entries[chatID]; ok {
		r.firstScreens.bytes -= len(entry.raw)
		delete(r.firstScreens.entries, chatID)
	}
	delete(r.firstScreens.tails, chatID)
	r.firstScreens.dropSliceLocked(chatID)
}

// runFirstScreenWarm reads the first slices of closed chats once, after
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

// warmFirstScreens reads, most recently active first, the first screen and
// the rest of the first slice of every closed chat, pausing between chats so
// that interactive reads share the connection. It returns how many first
// screens it kept.
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
		if r.warmChat(summary) {
			kept++
		}
		time.Sleep(time.Millisecond)
	}
	return kept
}

// warmChat reads a closed chat's first screen and, when that is not the whole
// first slice, the pages a renderer reads after it. It reports whether the
// first screen is kept.
func (r *providerChatRuntime) warmChat(summary chat.ActorSummary) bool {
	if summary.Deleted || summary.LedgerCount == 0 {
		return false
	}
	// Size the newest rows from their headers first: rows far too large to
	// keep are never read in the background.
	stored, err := r.ledgers.LedgerBytes(summary.ChatID, closedView(summary), summary.LedgerCount-firstScreenWarmLimit)
	if err != nil || stored > warmTailStoredBytes {
		return false
	}
	first, found, err := r.lastExchangeRawByChat(summary.ChatID, firstScreenWarmLimit, firstScreenWarmOptions)
	if err != nil || !found {
		return false
	}
	_, kept := r.cachedFirstScreen(summary, firstScreenWarmLimit, firstScreenWarmOptions)
	r.warmSlice(summary, first)
	return kept
}

// warmSlice reads the pages a renderer reads after a first screen, exactly as
// it pages (desktop/renderer2 store ensureRecentHistory): before the oldest
// row it has, for the rows its slice still lacks, until it has them all or a
// page brings nothing new.
func (r *providerChatRuntime) warmSlice(summary chat.ActorSummary, first []byte) {
	ids, ok := replyMessageIDs(first)
	want := min(firstScreenWarmLimit, summary.LedgerCount)
	if !ok || len(ids) >= want {
		return
	}
	pages := make(map[slicePageKey][]byte)
	for len(ids) < want && len(ids) > 0 {
		boundary, limit := ids[0], want-len(ids)
		page, found, err := r.archivePageBeforeByChat(summary.ChatID, boundary, limit, firstScreenWarmOptions)
		if err != nil || !found {
			return
		}
		raw := page.raw()
		pageIDs, ok := replyMessageIDs(raw)
		if !ok {
			return
		}
		prefix := pageIDs
		for index, id := range pageIDs {
			if id == boundary {
				prefix = pageIDs[:index]
				break
			}
		}
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			seen[id] = struct{}{}
		}
		overlap := false
		for _, id := range prefix {
			if _, dup := seen[id]; dup {
				overlap = true
			}
		}
		if len(prefix) == 0 || overlap {
			break
		}
		pages[slicePageKey{before: boundary, limit: limit}] = raw
		take := prefix[max(0, len(prefix)-(want-len(ids))):]
		ids = append(append([]string(nil), take...), ids...)
	}
	r.storeSlice(summary, firstScreenWarmOptions, pages)
}

// replyMessageIDs are the stable ids of a history reply's rows, in order.
func replyMessageIDs(raw []byte) ([]string, bool) {
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, false
	}
	ids := make([]string, len(rows))
	for index, row := range rows {
		ids[index] = row.ID
	}
	return ids, true
}
