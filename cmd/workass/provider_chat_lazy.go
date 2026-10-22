package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"workass/internal/chat"
)

// Lazy chat actors (SQLite backend).
//
// Boot opens only the actors restart recovery must settle: a turn that was in
// flight, queued or outbox work, a pending steer or cancel, running background
// work, an unresolved permission, an open obligation, a journal not yet folded
// into its checkpoint, spawned work the manager still tracks, or anything
// RecoverOutbox would change (chat.Quiescent). Every other actor stays closed:
// the runtime keeps its chat.ActorSummary (identity, tab, presentation, lanes,
// counts, activity), which is all session:get, the heartbeat digest, the
// sidebar, permission and background reads need, and its history pages come
// straight from the rows (chat.SQLActors.ReadLedgerPage). The first command,
// or any surface that needs the whole state, opens it, exactly as boot would
// have: opening a quiescent actor commits nothing.
//
// An actor that has been idle for actorIdleTTL, is quiescent, owns no lane
// attachment, has no tracked spawned work and is not the selected chat is
// closed again (retireIdleActors). The JSON backend keeps every actor open.

const (
	eagerActorsEnv  = "WORKASS_EAGER_ACTORS"
	actorIdleTTLEnv = "WORKASS_ACTOR_IDLE_TTL"
	// defaultActorIdleTTL is how long an untouched quiescent actor stays in
	// memory. Reopening a large one costs seconds, so it is generous.
	defaultActorIdleTTL = 10 * time.Minute
)

// eachDiscoverer is a backend that visits heads one at a time.
type eachDiscoverer interface {
	discoverEach(func(chat.ActorHead) error) error
}

// closedLedgerBackend is a backend whose rows answer history reads for an
// actor that is not open.
type closedLedgerBackend interface {
	closedActorLedgers() *chat.SQLActors
}

// lazyActorsRequested: WORKASS_EAGER_ACTORS=1 opens every actor at boot and
// never closes one (the pre-lazy behaviour, for comparison and as a fallback).
func lazyActorsRequested() bool {
	value := strings.TrimSpace(os.Getenv(eagerActorsEnv))
	return value == "" || value == "0" || strings.EqualFold(value, "false")
}

// configuredActorIdleTTL is WORKASS_ACTOR_IDLE_TTL as a Go duration; 0 or
// "off" never closes an idle actor.
func configuredActorIdleTTL() time.Duration {
	value := strings.TrimSpace(os.Getenv(actorIdleTTLEnv))
	if value == "" {
		return defaultActorIdleTTL
	}
	if strings.EqualFold(value, "off") {
		return 0
	}
	ttl, err := time.ParseDuration(value)
	if err != nil || ttl < 0 {
		return defaultActorIdleTTL
	}
	return ttl
}

func (a *providerChatActor) touch() {
	if a != nil {
		a.lastUsed.Store(time.Now().UnixNano())
	}
}

func (a *providerChatActor) idleFor(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, a.lastUsed.Load()))
}

// discoverActors records every accepted actor and, when actors may stay
// closed, keeps a summary instead of an open actor for each quiescent one.
func (r *providerChatRuntime) discoverActors(backend chatActorBackend) error {
	var ledgers *chat.SQLActors
	if lazy, ok := backend.(closedLedgerBackend); ok && lazyActorsRequested() {
		ledgers = lazy.closedActorLedgers()
	}
	r.ledgers = ledgers
	closed, reasons := 0, map[string]int{}
	var newestStamps map[string]chat.NewestLedgerStamp
	if ledgers != nil {
		var err error
		if newestStamps, err = ledgers.ReadNewestStamps(); err != nil {
			return fmt.Errorf("read chat ledger stamps: %w", err)
		}
	}
	visit := func(head chat.ActorHead) error {
		// The store may contain an empty envelope from a chat:create attempt
		// that never committed InitializeChat. It has no accepted chat
		// identity, so discovery must not turn it into a runtime chat. Any
		// nonempty pre-initialization state still fails closed through actor
		// construction.
		if unacceptedEmptyActorHead(head.State, head.LedgerLength) {
			return nil
		}
		chatID := head.State.ChatID
		r.known[chatID] = struct{}{}
		if ledgers == nil {
			return nil
		}
		reason := r.headMustOpen(head)
		if reason != "" {
			reasons[reason]++
			return nil
		}
		var stamps []chat.LedgerStamp
		if newest, ok := newestStamps[chatID]; ok && newest.Sequence == head.LedgerLength && newest.HasLedgerTimestamp() {
			stamps = []chat.LedgerStamp{newest.LedgerStamp}
		} else if head.LedgerLength > 0 {
			// The newest row has no timestamp: read back for one.
			var err error
			if stamps, err = ledgers.ReadLedgerStamps(chatID, head.LedgerLength, chat.SessionActivityWindow); err != nil {
				return fmt.Errorf("read chat %q tail: %w", chatID, err)
			}
		}
		r.summaries[chatID] = chat.SummarizeHead(head.State, head.LedgerLength, stamps)
		closed++
		return nil
	}
	var err error
	if each, ok := backend.(eachDiscoverer); ok {
		err = each.discoverEach(visit)
	} else {
		var heads []chat.ActorHead
		heads, err = backend.discover()
		for index := 0; err == nil && index < len(heads); index++ {
			err = visit(heads[index])
			heads[index] = chat.ActorHead{}
		}
	}
	if err == nil && ledgers != nil {
		log.Printf("[workass] chat actors: %d known, %d left closed until used, %d opened for restart recovery %v",
			len(r.known), closed, len(r.known)-closed, reasons)
		// Decoding every head left garbage several times the size of what
		// discovery keeps; hand it back before the daemon starts serving.
		debug.FreeOSMemory()
	}
	return err
}

// headMustOpen names why boot must open this actor, or "" when it may stay
// closed: the head is quiescent, it is exactly the committed actor (no
// journal, rows in the current checkpoint format) and the manager tracks no
// spawned work for it.
func (r *providerChatRuntime) headMustOpen(head chat.ActorHead) string {
	if ok, reason := chat.Quiescent(head.State); !ok {
		return reason
	}
	if head.JournalRecords != 0 {
		return "journal"
	}
	if head.LedgerLength > 0 && head.LedgerGen == 0 {
		return "rows before incremental checkpoints"
	}
	if tabID := strings.TrimSpace(head.State.Presentation.TabID); tabID != "" && r.manager != nil && len(r.manager.ListSpawnedWork(tabID, head.State.ChatID)) > 0 {
		return "spawned work"
	}
	return ""
}

// closedSummary is the summary of a known actor that is not open.
func (r *providerChatRuntime) closedSummary(chatID string) (chat.ActorSummary, bool) {
	if r == nil {
		return chat.ActorSummary{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	summary, ok := r.summaries[strings.TrimSpace(chatID)]
	return summary, ok
}

// residentOrClosed returns the open actor, or the summary of a closed one,
// without opening it and without counting as a use (a heartbeat read must not
// keep every actor in memory). A known actor that is neither is opened.
func (r *providerChatRuntime) residentOrClosed(chatID string) (*providerChatActor, *chat.ActorSummary, error) {
	chatID = strings.TrimSpace(chatID)
	r.mu.Lock()
	if r.bootErr == nil && !r.closed {
		if actor := r.actors[chatID]; actor != nil {
			r.mu.Unlock()
			return actor, nil, nil
		}
		if summary, ok := r.summaries[chatID]; ok {
			r.mu.Unlock()
			return nil, &summary, nil
		}
	}
	r.mu.Unlock()
	actor, err := r.actor(chatID)
	return actor, nil, err
}

// chatByTab finds the one actor attached to tabID among open and closed
// actors, without opening any.
func (r *providerChatRuntime) chatByTab(tabID string) (string, bool, error) {
	tabID = strings.TrimSpace(tabID)
	if r == nil || tabID == "" {
		return "", false, nil
	}
	ids, err := r.knownChatIDs()
	if err != nil {
		return "", false, err
	}
	matched := ""
	for _, chatID := range ids {
		actor, summary, err := r.residentOrClosed(chatID)
		if err != nil {
			return "", false, err
		}
		identity := chat.IdentitySnapshot{}
		if summary != nil {
			identity = summary.IdentitySnapshot()
		} else {
			identity = actor.engine.IdentitySnapshot()
		}
		if strings.TrimSpace(identity.TabID) != tabID {
			continue
		}
		if matched != "" {
			return "", false, fmt.Errorf("multiple actor chats claim tab id %q", tabID)
		}
		matched = chatID
	}
	return matched, matched != "", nil
}

// errClosedActorChanged: the actor was opened, and changed, while a closed
// read was in progress; the caller reads the now open actor instead.
var errClosedActorChanged = errors.New("closed chat actor changed during the read")

// closedView is the ledger a closed actor's rows must still be.
func closedView(summary chat.ActorSummary) chat.LedgerView {
	return chat.LedgerView{Count: summary.LedgerCount, Revision: summary.Revision}
}

// closedReadError turns a failed closed read into errClosedActorChanged when
// the actor changed underneath it (it was opened and committed, or is open
// now), so the caller reads the open actor instead.
func (r *providerChatRuntime) closedReadError(summary chat.ActorSummary, err error) error {
	if err == nil {
		return nil
	}
	current, stillClosed := r.closedSummary(summary.ChatID)
	if errors.Is(err, chat.ErrLedgerChanged) || !stillClosed || current.Revision != summary.Revision {
		return errClosedActorChanged
	}
	return err
}

// closedLedgerPage reads a page of a closed actor's rows (see
// chat.SQLActors.ReadLedgerPage).
func (r *providerChatRuntime) closedLedgerPage(summary chat.ActorSummary, beforeSequence uint64, beforeMessageID string, limit int) (chat.LedgerPageSnapshot, error) {
	page, err := r.ledgers.ReadLedgerPage(summary.ChatID, closedView(summary), beforeSequence, beforeMessageID, limit)
	return page, r.closedReadError(summary, err)
}

// closedHistoryRows returns the rows a bounded history read of a closed
// actor keeps: of the window ReadLedgerPage(before, limit) would return, the
// newest rows whose finished encodings fit the reply byte budget under the
// reader's options, oldest first. It is the selection finishArchivePage makes
// over the projected window (the newest row always, then each older row while
// it fits, sized by finishArchiveRow), computed while reading rows newest first, so only the kept rows and
// the first that does not fit are decoded. A row with its own pending or
// running status binds the rows after it together, so the rows the walk did
// not decode are checked for one (once per closed actor, closedLiveScan), and
// a window holding one is read whole.
//
// Unless the window was read whole, page is those rows finished for the wire:
// exactly what finishArchivePage makes of them, since the walk sized each row
// by finishing it. A reply can send it as it is instead of projecting,
// finishing and encoding the same rows a second time.
func (r *providerChatRuntime) closedHistoryRows(summary chat.ActorSummary, beforeMessageID string, limit int, opts []archiveReadOptions) ([]chat.LedgerEvent, *archivePage, error) {
	view := closedView(summary)
	window, err := r.ledgers.LedgerWindowBefore(summary.ChatID, view, 0, beforeMessageID, limit)
	if err != nil {
		return nil, nil, r.closedReadError(summary, err)
	}
	stateDir := filepath.Dir(r.sessions.path)
	resolved := make(map[string]string)
	used := int64(2) // JSON array brackets, as finishArchivePage counts them.
	var kept []chat.LedgerEvent
	keptMessages, keptEncoded := []any{}, [][]byte{}
	decodedFrom, live := window.End+1, false
	err = r.ledgers.WalkLedgerBackward(summary.ChatID, view, window, func(event chat.LedgerEvent) (bool, error) {
		decodedFrom--
		if event.Status == "pending" || event.Status == "running" {
			live = true
			return false, nil
		}
		message, err := projectLedgerMessage(event)
		if err != nil {
			return false, err
		}
		messages := []any{message}
		for _, opt := range opts {
			opt.beforeBudget(messages)
		}
		data, err := finishArchiveRow(messages, stateDir, opts, resolved)
		if err != nil {
			return false, err
		}
		size := int64(len(data))
		if len(kept) > 0 && used+size+1 > actorHistoryPageBytes {
			return false, nil
		}
		used += size + 1
		kept = append(kept, event)
		keptMessages = append(keptMessages, messages[0])
		keptEncoded = append(keptEncoded, data)
		return true, nil
	})
	if err != nil {
		return nil, nil, r.closedReadError(summary, err)
	}
	if !live && decodedFrom-1 > window.Start {
		// Rows (Start, decodedFrom) were not decoded.
		if live, err = r.closedRowsHaveLiveRow(summary, window, decodedFrom); err != nil {
			return nil, nil, r.closedReadError(summary, err)
		}
	}
	if live {
		page, err := r.closedLedgerPage(summary, 0, beforeMessageID, limit)
		if err != nil {
			return nil, nil, err
		}
		return page.Events, nil, nil
	}
	for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
		kept[left], kept[right] = kept[right], kept[left]
		keptMessages[left], keptMessages[right] = keptMessages[right], keptMessages[left]
		keptEncoded[left], keptEncoded[right] = keptEncoded[right], keptEncoded[left]
	}
	return kept, &archivePage{messages: keptMessages, encoded: keptEncoded}, nil
}

// closedLiveScan is what history reads have learned about a closed actor's
// rows with their own pending or running status: every row after
// scannedFrom has been checked, and live lists those found. It belongs to
// one actor revision.
type closedLiveScan struct {
	revision    uint64
	scannedFrom int
	live        []int
}

// closedRowsHaveLiveRow reports whether a row in (window.Start, decodedFrom)
// has its own pending or running status; rows from decodedFrom to the window
// end were decoded and checked by the caller. Each row of a closed actor is
// scanned at most once (scrolling up continues the checked region), and a
// summary taken from a resident actor already knows.
func (r *providerChatRuntime) closedRowsHaveLiveRow(summary chat.ActorSummary, window chat.LedgerWindow, decodedFrom int) (bool, error) {
	unchecked := chat.LedgerWindow{Start: window.Start, End: decodedFrom - 1, Count: window.Count}
	if known, ok := summary.LiveRows(); ok {
		return liveRowInWindow(known, unchecked), nil
	}
	r.mu.Lock()
	scan := r.liveScans[summary.ChatID]
	if scan == nil || scan.revision != summary.Revision {
		scan = &closedLiveScan{revision: summary.Revision, scannedFrom: window.Count}
		r.liveScans[summary.ChatID] = scan
	}
	if window.End >= scan.scannedFrom {
		// The decoded rows continue the checked region down to decodedFrom.
		scan.scannedFrom = min(scan.scannedFrom, decodedFrom-1)
	}
	from := scan.scannedFrom
	known := liveRowInWindow(scan.live, unchecked)
	r.mu.Unlock()
	if known {
		return true, nil
	}
	if unchecked.Start >= from {
		return false, nil
	}
	found, err := r.ledgers.ScanLedgerLiveRows(summary.ChatID, closedView(summary), unchecked.Start, min(from, unchecked.End))
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	if r.liveScans[summary.ChatID] == scan && scan.scannedFrom == from && min(from, unchecked.End) == from {
		scan.live = append(scan.live, found...)
		scan.scannedFrom = unchecked.Start
	}
	r.mu.Unlock()
	return liveRowInWindow(found, unchecked), nil
}

func liveRowInWindow(live []int, window chat.LedgerWindow) bool {
	for _, seq := range live {
		if seq > window.Start && seq <= window.End {
			return true
		}
	}
	return false
}

// closedProjectionSnapshot is ReadProjectionSnapshot(tail) of a closed actor,
// carrying only the tail rows a bounded read under opts keeps.
func (r *providerChatRuntime) closedProjectionSnapshot(summary chat.ActorSummary, tail int, opts []archiveReadOptions) (chat.ReadProjectionSnapshot, error) {
	snapshot, _, err := r.closedProjectionPage(summary, tail, opts)
	return snapshot, err
}

// closedProjectionPage is closedProjectionSnapshot and, when closedHistoryRows
// finished them, its rows as the tail reply sends them.
func (r *providerChatRuntime) closedProjectionPage(summary chat.ActorSummary, tail int, opts []archiveReadOptions) (chat.ReadProjectionSnapshot, *archivePage, error) {
	if tail <= 0 || summary.LedgerCount == 0 {
		snapshot := summary.ProjectionSnapshot(nil)
		snapshot.LastActivityAt = 0
		return snapshot, nil, nil
	}
	rows, page, err := r.closedHistoryRows(summary, "", tail, opts)
	if err != nil {
		return chat.ReadProjectionSnapshot{}, nil, err
	}
	return summary.ProjectionSnapshot(rows), page, nil
}

// closedLastExchangeTail is how many of a closed actor's newest rows
// ProjectLastExchangeByTab keeps of a tail of limit: back to the user's own
// newest message among them, found by the rows' role column so that no other
// row is read, or all limit rows when none is the user's.
func (r *providerChatRuntime) closedLastExchangeTail(summary chat.ActorSummary, limit int) (int, error) {
	from := summary.LedgerCount - limit
	sequence, err := r.ledgers.NewestLedgerRowOfRole(summary.ChatID, closedView(summary), from, "user", ownUserMessage)
	if err = r.closedReadError(summary, err); err != nil {
		return 0, err
	}
	if sequence == 0 {
		return limit, nil
	}
	return summary.LedgerCount - sequence + 1, nil
}

// closedSessionSnapshot is ReadSessionProjectionSnapshot of a closed actor:
// metadata only unless it is the selected chat (a closed actor has no
// foreground turn). tail must be chat.SessionActivityWindow, the window the
// summary's timestamp was taken over.
func (r *providerChatRuntime) closedSessionSnapshot(summary chat.ActorSummary, activeTabID string, tail int, opts []archiveReadOptions) (chat.ReadProjectionSnapshot, error) {
	if summary.TabID != activeTabID {
		return summary.ProjectionSnapshot(nil), nil
	}
	return r.closedProjectionSnapshot(summary, tail, opts)
}

// sessionSnapshot is ReadSessionProjectionSnapshot of any known actor; opts
// are the session read's options, which bound a closed actor's rows.
func (r *providerChatRuntime) sessionSnapshot(chatID, activeTabID string, tail int, opts []archiveReadOptions) (chat.ReadProjectionSnapshot, error) {
	actor, summary, err := r.residentOrClosed(chatID)
	if err != nil {
		return chat.ReadProjectionSnapshot{}, err
	}
	if summary != nil && tail == chat.SessionActivityWindow {
		snapshot, err := r.closedSessionSnapshot(*summary, activeTabID, tail, opts)
		if !errors.Is(err, errClosedActorChanged) {
			return snapshot, err
		}
	}
	if summary != nil {
		if actor, err = r.actor(chatID); err != nil {
			return chat.ReadProjectionSnapshot{}, err
		}
	}
	return actor.engine.ReadSessionProjectionSnapshot(activeTabID, tail), nil
}

// actorPrewarm opens the closed actors a renderer views, in the background.
// Reads of a closed chat never need it open, but its first command does: on
// a 300 MB chat that open took a second, which a send right after viewing
// the chat paid on top of its own work. One open runs at a time; a chat
// viewed meanwhile replaces any other waiting one (the newest view is the
// one a send follows), so clicking through many chats queues at most one
// open behind the one in progress.
type actorPrewarm struct {
	running bool
	next    string
	wg      sync.WaitGroup
}

// PrewarmChatByTab opens the tab's chat in the background if it is closed:
// a renderer is showing it (see actorPrewarm). It returns at once and never
// holds the runtime lock while an actor loads.
func (r *providerChatRuntime) PrewarmChatByTab(tabID string) {
	if r == nil || r.ledgers == nil {
		return
	}
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return
	}
	r.PrewarmChat(chatID)
}

// PrewarmChat is PrewarmChatByTab for a chat id.
func (r *providerChatRuntime) PrewarmChat(chatID string) {
	if r == nil || r.ledgers == nil {
		return
	}
	chatID = strings.TrimSpace(chatID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.bootErr != nil {
		return
	}
	if _, closed := r.summaries[chatID]; !closed {
		return
	}
	if r.prewarm.running {
		r.prewarm.next = chatID
		return
	}
	r.prewarm.running = true
	r.prewarm.wg.Add(1)
	go r.runPrewarm(chatID)
}

func (r *providerChatRuntime) runPrewarm(chatID string) {
	defer r.prewarm.wg.Done()
	for chatID != "" {
		r.mu.Lock()
		_, closed := r.summaries[chatID]
		stop := r.closed
		r.mu.Unlock()
		if closed && !stop {
			// openActor holds only this chat's opening slot while it loads;
			// a command or read arriving meanwhile waits for, or reads
			// around, this same load.
			if _, err := r.actor(chatID); err != nil {
				r.mu.Lock()
				closing := r.closed
				r.mu.Unlock()
				if !closing {
					log.Printf("[workass] open viewed chat %s in the background: %v", chatID, err)
				}
			}
		}
		r.mu.Lock()
		chatID, r.prewarm.next = r.prewarm.next, ""
		if chatID == "" || r.closed {
			chatID = ""
			r.prewarm.running = false
		}
		r.mu.Unlock()
	}
}

// resumeOpenedActor is ResumeActors for one actor opened after startup: the
// Entorno reference is restored once per daemon run (as boot restores every
// open actor's) and the coordinator wakes. A quiescent actor has nothing for
// background or obligation reconciliation to settle.
func (r *providerChatRuntime) resumeOpenedActor(actor *providerChatActor) {
	state := actor.engine.IdentitySnapshot()
	if !state.Deleted && r.claimEnvironmentRestore(state.ChatID) {
		if err := r.restoreActorEnvironment(actor); err != nil {
			log.Printf("[workass] restore chat %s environment reference: %v", state.ChatID, err)
		}
	}
	actor.coordinator.Wake()
}

// claimEnvironmentRestore reports whether this daemon run has yet to restore
// the chat's Entorno reference, and records that it now has.
func (r *providerChatRuntime) claimEnvironmentRestore(chatID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, done := r.envRestored[chatID]; done {
		return false
	}
	r.envRestored[chatID] = struct{}{}
	return true
}

// runIdleActorRetirement closes idle actors until the runtime closes.
func (r *providerChatRuntime) runIdleActorRetirement(ctx context.Context, ttl time.Duration) {
	defer r.wg.Done()
	interval := min(max(ttl/4, time.Second), time.Minute)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.retireIdleActors(ctx, ttl)
		}
	}
}

// retireIdleActors closes every open actor that has not been used for ttl,
// is quiescent, owns no lane attachment or tracked spawned work, and is not
// the selected chat. It returns how many it closed.
func (r *providerChatRuntime) retireIdleActors(ctx context.Context, ttl time.Duration) int {
	if r == nil || r.ledgers == nil || ttl <= 0 {
		return 0
	}
	r.mu.Lock()
	if r.closed || r.actorsPaused || r.bootErr != nil {
		r.mu.Unlock()
		return 0
	}
	now := time.Now()
	candidates := make(map[string]*providerChatActor)
	for chatID, actor := range r.actors {
		if actor.idleFor(now) >= ttl {
			candidates[chatID] = actor
		}
	}
	r.mu.Unlock()
	if len(candidates) == 0 {
		return 0
	}
	activeTabID := ""
	if r.sessions != nil {
		activeTabID = strings.TrimSpace(fieldString(r.sessions.GlobalSnapshot(), "activeId"))
	}
	retired := 0
	for chatID, actor := range candidates {
		if ctx.Err() != nil {
			break
		}
		if r.retireActor(chatID, actor, activeTabID, ttl) {
			retired++
		}
	}
	if retired > 0 {
		// The ledgers just released are most of the heap; hand them back now
		// rather than when the scavenger gets to them.
		debug.FreeOSMemory()
		open, closed := r.lazyActorCounts()
		log.Printf("[workass] closed %d idle chat actors (%d open, %d closed)", retired, open, closed)
	}
	return retired
}

// retireActor closes one actor if it is still the open one, still idle, and
// may be left closed. Lock order: actor.mu, then r.mu, then the engine's own
// lock (inside Retire); lookups take r.mu alone, so none can return the actor
// between its retirement and its removal from the map.
func (r *providerChatRuntime) retireActor(chatID string, actor *providerChatActor, activeTabID string, ttl time.Duration) bool {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	identity := actor.engine.IdentitySnapshot()
	tabID := strings.TrimSpace(identity.TabID)
	if tabID != "" && tabID == activeTabID {
		return false
	}
	if !actor.coordinator.Idle() {
		return false
	}
	if tabID != "" && r.manager != nil && len(r.manager.ListSpawnedWork(tabID, chatID)) > 0 {
		return false
	}
	r.mu.Lock()
	if r.closed || r.actors[chatID] != actor || actor.idleFor(time.Now()) < ttl {
		r.mu.Unlock()
		return false
	}
	summary, ok, err := actor.engine.Retire(nil)
	if err != nil || !ok {
		r.mu.Unlock()
		if err != nil {
			log.Printf("[workass] close idle chat %s: %v", chatID, err)
		}
		return false
	}
	delete(r.actors, chatID)
	r.summaries[chatID] = summary
	delete(r.liveScans, chatID)
	delete(r.closedRefs, chatID)
	r.mu.Unlock()
	_ = actor.coordinator.Close(context.Background())
	return true
}

// closedRefSet is the attachment references a closed actor's ledger owns, at
// one revision.
type closedRefSet struct {
	revision uint64
	refs     map[string]struct{}
}

// closedAttachmentRefs is the set every image read of a closed actor checks:
// read from its rows once (chat.SQLActors.LedgerAttachmentRefs), then kept
// until the actor opens, so scrolling a screenshot-heavy chat does not scan
// it per image.
func (r *providerChatRuntime) closedAttachmentRefs(summary chat.ActorSummary) (map[string]struct{}, error) {
	r.mu.Lock()
	cached := r.closedRefs[summary.ChatID]
	r.mu.Unlock()
	if cached != nil && cached.revision == summary.Revision {
		return cached.refs, nil
	}
	refs, err := r.ledgers.LedgerAttachmentRefs(summary.ChatID, closedView(summary))
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if current, stillClosed := r.summaries[summary.ChatID]; stillClosed && current.Revision == summary.Revision {
		r.closedRefs[summary.ChatID] = &closedRefSet{revision: summary.Revision, refs: refs}
	}
	r.mu.Unlock()
	return refs, nil
}

// closedChatIDs lists the known actors that are not open, sorted.
func (r *providerChatRuntime) closedChatIDs() []string {
	r.mu.Lock()
	ids := make([]string, 0, len(r.summaries))
	for chatID := range r.summaries {
		ids = append(ids, chatID)
	}
	r.mu.Unlock()
	sort.Strings(ids)
	return ids
}

// lazyActorCounts reports how many known actors are open and closed.
func (r *providerChatRuntime) lazyActorCounts() (open, closed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.actors), len(r.summaries)
}
