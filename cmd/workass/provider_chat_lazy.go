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
		stamps, err := ledgers.ReadLedgerStamps(chatID, head.LedgerLength, chat.SessionActivityWindow)
		if err != nil {
			return fmt.Errorf("read chat %q tail: %w", chatID, err)
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
	if head.JournalBytes != 0 {
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

// closedLedgerPage reads a page of a closed actor's rows (see
// chat.SQLActors.ReadLedgerPage), checked against its summary's revision.
func (r *providerChatRuntime) closedLedgerPage(summary chat.ActorSummary, beforeSequence uint64, beforeMessageID string, limit int) (chat.LedgerPageSnapshot, error) {
	page, revision, err := r.ledgers.ReadLedgerPage(summary.ChatID, beforeSequence, beforeMessageID, limit)
	if err != nil {
		if _, stillClosed := r.closedSummary(summary.ChatID); !stillClosed {
			return chat.LedgerPageSnapshot{}, errClosedActorChanged
		}
		return chat.LedgerPageSnapshot{}, err
	}
	if revision != summary.Revision || page.LedgerCount != summary.LedgerCount {
		return chat.LedgerPageSnapshot{}, errClosedActorChanged
	}
	return page, nil
}

// closedHistoryRows returns the rows a bounded history read of a closed
// actor keeps: of the window ReadLedgerPage(before, limit) would return, the
// newest rows whose projections fit the reply byte budget under the reader's
// options, oldest first. It is the selection boundedArchiveHistory makes over
// the projected window (the newest row always, then each older row while it
// fits), computed while reading rows newest first, so only the kept rows and
// the first that does not fit are decoded. A window holding a pending or
// running row (which binds the rows after it together) is read whole.
func (r *providerChatRuntime) closedHistoryRows(summary chat.ActorSummary, beforeMessageID string, limit int, opts []archiveReadOptions) ([]chat.LedgerEvent, error) {
	window, revision, err := r.ledgers.LedgerWindowBefore(summary.ChatID, 0, beforeMessageID, limit)
	if err == nil && (revision != summary.Revision || window.Count != summary.LedgerCount) {
		err = errClosedActorChanged
	}
	var live bool
	if err == nil {
		live, err = r.closedWindowHasLiveRow(summary, window, revision)
	}
	if err == nil && live {
		page, err := r.closedLedgerPage(summary, 0, beforeMessageID, limit)
		if err != nil {
			return nil, err
		}
		return page.Events, nil
	}
	stateDir := filepath.Dir(r.sessions.path)
	deferring := deferringMessageImages(opts)
	imageSizes := make(map[string]int64)
	used := int64(2) // JSON array brackets, as actorHistorySuffixWithin counts them.
	var kept []chat.LedgerEvent
	if err == nil {
		err = r.ledgers.WalkLedgerBackward(summary.ChatID, window, revision, func(event chat.LedgerEvent) (bool, error) {
			message, err := projectLedgerMessage(event)
			if err != nil {
				return false, err
			}
			messages := []any{message}
			for _, opt := range opts {
				opt.beforeBudget(messages)
			}
			size, err := archiveReplyBytes(messages[0], stateDir, imageSizes, deferring)
			if err != nil {
				return false, err
			}
			if len(kept) > 0 && used+size+1 > actorHistoryPageBytes {
				return false, nil
			}
			used += size + 1
			kept = append(kept, event)
			return true, nil
		})
	}
	if err != nil {
		if _, stillClosed := r.closedSummary(summary.ChatID); !stillClosed || errors.Is(err, errClosedActorChanged) {
			return nil, errClosedActorChanged
		}
		return nil, err
	}
	for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
		kept[left], kept[right] = kept[right], kept[left]
	}
	return kept, nil
}

// closedLiveScan is what is known about a closed actor's pending or running
// rows: every row after scannedFrom has been scanned, and live lists those
// found. It belongs to one actor revision.
type closedLiveScan struct {
	revision    uint64
	scannedFrom int
	live        []int
}

// closedWindowHasLiveRow reports whether a row in the window has its own
// pending or running status. Rows are scanned once per closed actor, as
// history reads first reach them; a summary taken from a resident actor
// already knows.
func (r *providerChatRuntime) closedWindowHasLiveRow(summary chat.ActorSummary, window chat.LedgerWindow, revision uint64) (bool, error) {
	if known, ok := summary.LiveRows(); ok {
		return liveRowInWindow(known, window), nil
	}
	r.mu.Lock()
	scan := r.liveScans[summary.ChatID]
	if scan == nil || scan.revision != revision {
		scan = &closedLiveScan{revision: revision, scannedFrom: window.Count}
		r.liveScans[summary.ChatID] = scan
	}
	from := scan.scannedFrom
	r.mu.Unlock()
	if window.Start < from {
		found, err := r.ledgers.ScanLedgerLiveRows(summary.ChatID, window.Start, from, window.Count, revision)
		if err != nil {
			return false, err
		}
		r.mu.Lock()
		if r.liveScans[summary.ChatID] == scan && scan.scannedFrom == from {
			scan.live = append(scan.live, found...)
			scan.scannedFrom = window.Start
		}
		r.mu.Unlock()
		if liveRowInWindow(found, window) {
			return true, nil
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return liveRowInWindow(scan.live, window), nil
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
	if tail <= 0 || summary.LedgerCount == 0 {
		snapshot := summary.ProjectionSnapshot(nil)
		snapshot.LastActivityAt = 0
		return snapshot, nil
	}
	rows, err := r.closedHistoryRows(summary, "", tail, opts)
	if err != nil {
		return chat.ReadProjectionSnapshot{}, err
	}
	return summary.ProjectionSnapshot(rows), nil
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
	r.mu.Unlock()
	_ = actor.coordinator.Close(context.Background())
	return true
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
