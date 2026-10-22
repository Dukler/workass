package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

const sessionProjectionMessageTail = 60

type actorHistoryProjection uint8

const (
	actorHistoryFull actorHistoryProjection = iota
	actorHistoryTail
	actorHistoryMetadataOnly
)

// StateDigest is the lean actor-derived heartbeat projection. It intentionally
// does not consult session-state.json chat rows: after cutover that file contains
// daemon-global UI preferences only, and using it here would manufacture a
// permanent session:get refresh loop for every real actor-owned chat.
func (r *providerChatRuntime) StateDigest(catalogHashes map[string]string, settingsRevision, procHash string) (map[string]any, error) {
	if r == nil {
		return nil, errors.New("provider chat digest is unavailable")
	}
	ids, err := r.knownChatIDs()
	if err != nil {
		return nil, err
	}
	chats := make([]any, 0, len(ids))
	for _, chatID := range ids {
		actor, summary, err := r.residentOrClosed(chatID)
		if err != nil {
			return nil, err
		}
		var digest chat.DigestSnapshot
		if summary != nil {
			digest = summary.DigestSnapshot()
		} else {
			digest = actor.engine.DigestSnapshot()
		}
		if digest.Deleted {
			continue
		}
		projected := map[string]any{
			"tabId": digest.TabID, "chatId": digest.ChatID,
			"actorRevision":           digest.ActorRevision,
			presentationRevisionField: digest.PresentationRevision,
			"runningJobId":            nullableDigestString(digest.RunningJobID),
			"lastMessageId":           nullableDigestString(digest.LastMessageID),
			"messageCount":            digest.MessageCount, "queueLen": digest.QueueLen,
			"queueHeadId":               nullableDigestString(digest.QueueHeadID),
			agentQueueRevisionField:     int(digest.AgentQueueRevision),
			runtimeControlRevisionField: int(digest.RuntimeControlRevision),
			"providerId":                nullableDigestString(digest.ProviderID),
			"currentModelId":            nullableDigestString(digest.CurrentModelID),
			"currentModeId":             nullableDigestString(digest.CurrentModeID),
			"pendingPermissionIds":      nonNilDigestStrings(digest.PendingPermissionIDs),
		}
		chats = append(chats, projected)
	}
	if catalogHashes == nil {
		catalogHashes = map[string]string{}
	}
	globalRevision := 0
	if r.sessions != nil {
		globalRevision = intValue(r.sessions.GlobalSnapshot()[globalPresentationRevisionField])
	}
	return map[string]any{
		"chats": chats, globalPresentationRevisionField: globalRevision, "catalogHash": catalogHashes,
		"settingsRevision": settingsRevision, "procHash": procHash,
	}, nil
}

// ProjectSession is the pure actor -> frozen Mirror-v1 boundary. The session
// store contributes daemon-global application preferences only. The selected
// chat and any running chat carry a bounded actor tail; idle, unselected chats
// carry metadata only. Recent and explicit full-history reads use the frozen
// chat:archive-load method. Both the wire result and the actor snapshot are
// bounded here: limiting only the serialized rows still paid to deep-copy every
// historical body before a large chat could paint.
func (r *providerChatRuntime) ProjectSession(opts ...archiveReadOptions) (map[string]any, error) {
	return r.projectSession(false, opts...)
}

// projectSession with encodedRows leaves every chat's messages as the row
// encodings its page budget already produced, for a caller that marshals the
// root; the reply bytes are identical.
func (r *providerChatRuntime) projectSession(encodedRows bool, opts ...archiveReadOptions) (map[string]any, error) {
	if r == nil || r.sessions == nil {
		return nil, errors.New("provider chat projection is unavailable")
	}
	known, err := r.knownChatIDs()
	if err != nil {
		return nil, err
	}
	root := r.sessions.GlobalSnapshot()
	activeID := strings.TrimSpace(fieldString(root, "activeId"))
	states := make([]chat.State, 0, len(known))
	windows := make(map[string]actorLedgerWindow, len(known))
	for _, chatID := range known {
		snapshot, err := r.sessionSnapshot(chatID, activeID, sessionProjectionMessageTail, opts)
		if err != nil {
			return nil, err
		}
		state := snapshot.State
		if !state.Deleted {
			states = append(states, state)
			windows[state.ChatID] = actorLedgerWindow{offset: snapshot.LedgerOffset, count: snapshot.LedgerCount, lastActivityAt: snapshot.LastActivityAt}
		}
	}
	states = orderActorChatStates(states, root["chatOrder"])
	activeExists := false
	firstTabID := ""
	for _, state := range states {
		tabID := state.Presentation.TabID
		if firstTabID == "" {
			firstTabID = tabID
		}
		if tabID == activeID {
			activeExists = true
		}
	}
	if !activeExists {
		activeID = firstTabID
		// The fallback selection was not known when idle snapshots were taken.
		// Fetch its bounded tail once, without copying any other idle history.
		for i, state := range states {
			if state.Presentation.TabID != activeID {
				continue
			}
			snapshot, err := r.sessionSnapshot(state.ChatID, activeID, sessionProjectionMessageTail, opts)
			if err != nil {
				return nil, err
			}
			states[i] = snapshot.State
			windows[state.ChatID] = actorLedgerWindow{offset: snapshot.LedgerOffset, count: snapshot.LedgerCount, lastActivityAt: snapshot.LastActivityAt}
			break
		}
	}
	if activeID == "" {
		root["activeId"] = nil
	} else {
		root["activeId"] = activeID
	}

	projectedChats := make([]any, 0, len(states))
	for _, state := range states {
		projected := map[string]any{}
		history := sessionHistoryProjection(state, activeID)
		window := windows[state.ChatID]
		if err := projectActorChatWithHistoryWindow(projected, state, history, sessionProjectionMessageTail, window); err != nil {
			return nil, fmt.Errorf("project actor-native chat %q: %w", state.ChatID, err)
		}
		for _, opt := range opts {
			opt.beforeBudget(anySlice(projected["messages"]))
		}
		page, err := finishArchivePage(anySlice(projected["messages"]), filepath.Dir(r.sessions.path), opts, actorHistoryPageBytes)
		if err != nil {
			return nil, err
		}
		if encodedRows {
			projected["messages"] = page.rawMessages()
		} else {
			projected["messages"] = page.messages
		}
		projected["historyComplete"] = len(page.messages) == intValue(projected["messageCount"])
		projectedChats = append(projectedChats, projected)
	}
	root["chats"] = projectedChats
	// Every chat's rows are finished above; this covers the rest of the root.
	if err := rehydrateExternalSessionImages(root, filepath.Dir(r.sessions.path)); err != nil {
		return nil, err
	}
	return root, nil
}

func orderActorChatStates(states []chat.State, rawOrder any) []chat.State {
	if len(states) < 2 {
		return states
	}
	byTab := make(map[string]chat.State, len(states))
	for _, state := range states {
		byTab[state.Presentation.TabID] = state
	}
	known := make([]chat.State, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for _, rawTabID := range normalizedActorChatOrder(rawOrder) {
		tabID, _ := rawTabID.(string)
		state, exists := byTab[tabID]
		if !exists {
			continue
		}
		known = append(known, state)
		seen[tabID] = struct{}{}
	}
	// An actor missing from the saved order is a newly discovered chat. New
	// chats are the only automatic sidebar movement, so put them above the
	// persisted manual order while retaining deterministic knownChatIDs order.
	ordered := make([]chat.State, 0, len(states))
	for _, state := range states {
		if _, exists := seen[state.Presentation.TabID]; exists {
			continue
		}
		ordered = append(ordered, state)
	}
	ordered = append(ordered, known...)
	return ordered
}

func sessionHistoryProjection(state chat.State, activeTabID string) actorHistoryProjection {
	if state.Presentation.TabID == activeTabID || state.Foreground != nil {
		return actorHistoryTail
	}
	return actorHistoryMetadataOnly
}

func (r *providerChatRuntime) ProjectSessionRaw(opts ...archiveReadOptions) ([]byte, error) {
	root, err := r.projectSession(true, opts...)
	if err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

// ProjectArchiveByTab derives the frozen archive payload from the same actor
// ledger as session:get. After eager cutover, found=false means the tab is not
// an actor-owned chat; callers must never fall back to deleted JSONL storage.
func (r *providerChatRuntime) ProjectArchiveByTab(tabID string) ([]any, bool, error) {
	return r.projectArchiveByTab(tabID, actorHistoryFull, sessionProjectionMessageTail)
}

// ProjectRecentArchiveByTab returns only the newest bounded actor rows. It is
// the navigation read: an idle chat is metadata-only in session:get, so opening
// it must not expand every historical attachment merely to paint the current
// viewport. The canonical ledger and the legacy full archive read are unchanged.
func (r *providerChatRuntime) ProjectRecentArchiveByTab(tabID string, limit int, opts ...archiveReadOptions) ([]any, bool, error) {
	if limit < 1 || limit > sessionProjectionMessageTail {
		return nil, false, fmt.Errorf("recent archive limit must be between 1 and %d", sessionProjectionMessageTail)
	}
	return r.projectArchiveByTab(tabID, actorHistoryTail, limit, opts...)
}

// ProjectRecentArchiveByTabRaw is the same tail as its wire reply, encoded once.
func (r *providerChatRuntime) ProjectRecentArchiveByTabRaw(tabID string, limit int, opts ...archiveReadOptions) (wire.EncodedResult, bool, error) {
	return r.recentArchiveRaw(tabID, limit, false, opts...)
}

// ProjectLastExchangeByTab is the read a renderer opens a chat with when it
// has nothing of it on screen (archive-load {tail, lastExchange}): the
// ordinary bounded tail when its reply is at most lastExchangeWholeTailBytes,
// and otherwise, of the newest limit rows, only those from the user's own
// newest message on (the rows the transcript's window always reaches back
// to; a wake-up notice is not the user's message), or all of them when none
// is, under the same page budget. A large tail's first screen therefore
// sends the exchange it shows and not the history above it, and a small one
// arrives whole; the renderer pages in whatever it still lacks by stable
// cursor, as it does after any bounded tail.
func (r *providerChatRuntime) ProjectLastExchangeByTab(tabID string, limit int, opts ...archiveReadOptions) ([]any, bool, error) {
	if limit < 1 || limit > sessionProjectionMessageTail {
		return nil, false, fmt.Errorf("recent archive limit must be between 1 and %d", sessionProjectionMessageTail)
	}
	page, found, err := r.archivePageByTab(tabID, actorHistoryTail, limit, true, opts...)
	return page.messages, found, err
}

// ProjectLastExchangeByTabRaw is the same rows as their wire reply, encoded once.
func (r *providerChatRuntime) ProjectLastExchangeByTabRaw(tabID string, limit int, opts ...archiveReadOptions) (wire.EncodedResult, bool, error) {
	return r.recentArchiveRaw(tabID, limit, true, opts...)
}

func (r *providerChatRuntime) recentArchiveRaw(tabID string, limit int, lastExchange bool, opts ...archiveReadOptions) (wire.EncodedResult, bool, error) {
	if limit < 1 || limit > sessionProjectionMessageTail {
		return nil, false, fmt.Errorf("recent archive limit must be between 1 and %d", sessionProjectionMessageTail)
	}
	if lastExchange {
		// A closed chat's first screen may already be in memory
		// (provider_chat_first_screen.go).
		return r.lastExchangeRaw(tabID, limit, opts...)
	}
	page, found, err := r.archivePageByTab(tabID, actorHistoryTail, limit, false, opts...)
	if err != nil || !found {
		return nil, found, err
	}
	return page.raw(), true, nil
}

// archiveReadOptions are additive archive-load options a newer renderer asks
// for; an older one omits them and keeps the byte-identical response.
type archiveReadOptions struct {
	// deferMessageImages sends a message's own images (a reply's screenshots,
	// a user's attachments) by reference, like tool images, so opening a chat
	// is not a multi-megabyte read before its text can paint (2026-10-13: four
	// screenshots made a ten-message read 4.8 MB).
	deferMessageImages bool
	// leanTools sends large committed tool payloads as a bounded head; the
	// rest is read per row with the toolDetail archive option.
	leanTools bool
	// leanTitles does the same for a tool's title. A title can carry a whole
	// command or file (11 MB titles made one message of a chat 84 MB, 79 MB of
	// it titles) while its row shows one line. A separate option, because a
	// renderer that asked only for leanTools reads titles whole.
	leanTitles bool
}

// archiveReadOptionsFrom reads the additive read options of session:get and
// chat:archive-load; an absent option keeps the byte-identical response.
func archiveReadOptionsFrom(options map[string]any) archiveReadOptions {
	return archiveReadOptions{
		deferMessageImages: options["deferImages"] == true,
		leanTools:          options["leanTools"] == true,
		leanTitles:         options["leanTitles"] == true,
	}
}

// beforeBudget runs before the byte budget measures a page, so a lean page
// carries as many rows as its lean size allows.
func (o archiveReadOptions) beforeBudget(messages []any) {
	if o.leanTools || o.leanTitles {
		leanArchiveToolPayloads(messages, o.leanTools, o.leanTitles)
	}
}

func (o archiveReadOptions) apply(messages []any) {
	if o.deferMessageImages {
		deferArchiveMessageImages(messages)
	}
}

// ProjectArchivePageBeforeByTab returns one bounded canonical ledger page
// immediately before a stable resident message. It is the ordinary upward
// history path; explicit full-history reads remain available for search and
// legacy clients.
func (r *providerChatRuntime) ProjectArchivePageBeforeByTab(tabID, beforeMessageID string, limit int, opts ...archiveReadOptions) ([]any, bool, error) {
	page, found, err := r.archivePageBeforeByTab(tabID, beforeMessageID, limit, opts...)
	return page.messages, found, err
}

// ProjectArchivePageBeforeByTabRaw is the same page as its wire reply, encoded once.
func (r *providerChatRuntime) ProjectArchivePageBeforeByTabRaw(tabID, beforeMessageID string, limit int, opts ...archiveReadOptions) (wire.EncodedResult, bool, error) {
	if limit < 1 || limit > sessionProjectionMessageTail {
		return nil, false, fmt.Errorf("archive page limit must be between 1 and %d", sessionProjectionMessageTail)
	}
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return nil, found, err
	}
	// The pages that complete a closed chat's first slice may already be in
	// memory (provider_chat_first_screen.go).
	if raw, ok := r.cachedSlicePage(chatID, beforeMessageID, limit, opts...); ok {
		return raw, true, nil
	}
	page, found, err := r.archivePageBeforeByChat(chatID, beforeMessageID, limit, opts...)
	if err != nil || !found {
		return nil, found, err
	}
	return page.raw(), true, nil
}

func (r *providerChatRuntime) archivePageBeforeByTab(tabID, beforeMessageID string, limit int, opts ...archiveReadOptions) (archivePage, bool, error) {
	if limit < 1 || limit > sessionProjectionMessageTail {
		return archivePage{}, false, fmt.Errorf("archive page limit must be between 1 and %d", sessionProjectionMessageTail)
	}
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return archivePage{}, found, err
	}
	return r.archivePageBeforeByChat(chatID, beforeMessageID, limit, opts...)
}

// archivePageBeforeByChat is archivePageBeforeByTab of the chat the tab was
// found to own.
func (r *providerChatRuntime) archivePageBeforeByChat(chatID, beforeMessageID string, limit int, opts ...archiveReadOptions) (archivePage, bool, error) {
	page, finished, err := r.ledgerPageBefore(chatID, beforeMessageID, limit, opts)
	if err != nil {
		return archivePage{}, false, err
	}
	if page.Deleted {
		return archivePage{messages: []any{}}, true, nil
	}
	if finished != nil {
		// A closed actor's rows were finished while they were selected.
		return *finished, true, nil
	}
	messages := make([]any, 0, len(page.Events))
	for _, event := range page.Events {
		message, err := projectLedgerMessage(event)
		if err != nil {
			return archivePage{}, false, err
		}
		messages = append(messages, message)
	}
	for _, opt := range opts {
		opt.beforeBudget(messages)
	}
	built, err := finishArchivePage(messages, filepath.Dir(r.sessions.path), opts, actorHistoryPageBytes)
	if err != nil {
		return archivePage{}, false, err
	}
	return built, true, nil
}

func (r *providerChatRuntime) projectArchiveByTab(tabID string, history actorHistoryProjection, tailLimit int, opts ...archiveReadOptions) ([]any, bool, error) {
	page, found, err := r.archivePageByTab(tabID, history, tailLimit, false, opts...)
	return page.messages, found, err
}

// archivePageByTab is one archive read of the tab's chat: the complete ledger,
// or its bounded tail (with lastExchange, only the tail's last exchange; see
// ProjectLastExchangeByTab).
func (r *providerChatRuntime) archivePageByTab(tabID string, history actorHistoryProjection, tailLimit int, lastExchange bool, opts ...archiveReadOptions) (archivePage, bool, error) {
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return archivePage{}, found, err
	}
	return r.archivePageByChat(chatID, history, tailLimit, lastExchange, opts...)
}

// archivePageByChat is archivePageByTab of the chat the tab was found to own.
func (r *providerChatRuntime) archivePageByChat(chatID string, history actorHistoryProjection, tailLimit int, lastExchange bool, opts ...archiveReadOptions) (archivePage, bool, error) {
	if lastExchange && history == actorHistoryTail {
		// A tail small enough to read and paint at once is the first screen
		// whole (ProjectLastExchangeByTab). A closed chat's rows do not change
		// while it is closed, so what that tail came to is remembered: a large
		// one is never read again just to learn it is large.
		summary, closed := r.closedSummary(chatID)
		if !closed || !r.tailKnownLarge(summary, tailLimit, opts) {
			whole, found, err := r.archivePageByChat(chatID, history, tailLimit, false, opts...)
			if err != nil || !found {
				return whole, found, err
			}
			small := whole.size() <= lastExchangeWholeTailBytes
			if closed {
				r.recordTailSize(summary, tailLimit, opts, small)
			}
			if small {
				return whole, true, nil
			}
		}
	}
	var (
		state  chat.State
		window actorLedgerWindow
		read   bool
	)
	if summary, closed := r.closedSummary(chatID); closed && history != actorHistoryFull {
		// A bounded read of a closed actor comes from its rows. The complete
		// archive opens the actor, whose load is chunked, rather than copying
		// a whole transcript in one read transaction.
		tail := max(1, tailLimit)
		var err error
		if lastExchange {
			tail, err = r.closedLastExchangeTail(summary, tail)
		}
		var (
			snapshot chat.ReadProjectionSnapshot
			finished *archivePage
		)
		if err == nil {
			snapshot, finished, err = r.closedProjectionPage(summary, tail, opts)
		}
		switch {
		case err == nil && finished != nil && history == actorHistoryTail:
			// The rows were finished while they were selected: this is the
			// page finishArchivePage would make of their projection.
			if summary.Deleted {
				return archivePage{messages: []any{}}, true, nil
			}
			if err := uniqueLedgerMessageIDs(snapshot.State.Ledger); err != nil {
				return archivePage{}, false, err
			}
			return *finished, true, nil
		case err == nil:
			state, read = snapshot.State, true
			window = actorLedgerWindow{offset: snapshot.LedgerOffset, count: snapshot.LedgerCount}
		case !errors.Is(err, errClosedActorChanged):
			return archivePage{}, false, err
		}
	}
	if !read {
		matched, err := r.actor(chatID)
		if err != nil {
			return archivePage{}, false, err
		}
		if history == actorHistoryFull {
			state = matched.engine.ReadSnapshot()
			window = actorLedgerWindow{count: len(state.Ledger)}
		} else {
			snapshot := matched.engine.ReadProjectionSnapshot(max(1, tailLimit))
			state = snapshot.State
			window = actorLedgerWindow{offset: snapshot.LedgerOffset, count: snapshot.LedgerCount}
		}
	}
	if state.Deleted {
		// A tombstone owns this historical tab. Never recreate its transcript.
		return archivePage{messages: []any{}}, true, nil
	}
	projected := map[string]any{}
	if err := projectActorChatWithHistoryWindow(projected, state, history, tailLimit, window); err != nil {
		return archivePage{}, false, err
	}
	messages := anySlice(projected["messages"])
	if lastExchange && history == actorHistoryTail {
		messages = messages[lastExchangeStart(messages):]
	}
	for _, opt := range opts {
		opt.beforeBudget(messages)
	}
	if history != actorHistoryFull {
		finished, err := finishArchivePage(messages, filepath.Dir(r.sessions.path), opts, actorHistoryPageBytes)
		if err != nil {
			return archivePage{}, false, err
		}
		return finished, true, nil
	}
	// The complete ledger is never bounded, so it is never sized either: the
	// reply encodes it once.
	annotateSessionImageSizes(messages, filepath.Dir(r.sessions.path))
	deferArchiveToolImages(messages)
	for _, opt := range opts {
		opt.apply(messages)
	}
	if err := rehydrateExternalSessionImages(messages, filepath.Dir(r.sessions.path)); err != nil {
		return archivePage{}, false, err
	}
	return archivePage{messages: messages}, true, nil
}

// deferArchiveMessageImages does for a message's own images what
// deferArchiveToolImages does for tool media: the reference stays, the bytes
// are read per image through the same authenticated archive channel.
func deferArchiveMessageImages(messages []any) {
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		for _, rawImage := range anySlice(message["images"]) {
			image := mapFromAnyMain(rawImage)
			if ref := fieldString(image, sessionImageDataRefField); ref != "" {
				image["deferredImageRef"] = ref
				image["data"] = ""
				delete(image, sessionImageDataRefField)
			}
		}
	}
}

// Tool media stays in the canonical ledger. Archive reads carry only its
// immutable reference so one image-heavy assistant row cannot exceed the
// websocket frame limit before the user can enter the chat. The renderer reads
// each visible image through the same authenticated archive channel.
func deferArchiveToolImages(messages []any) {
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		for _, rawEvent := range anySlice(message["events"]) {
			event := mapFromAnyMain(rawEvent)
			if fieldString(event, "kind") != "tool" {
				continue
			}
			for _, rawImage := range anySlice(event["images"]) {
				image := mapFromAnyMain(rawImage)
				if ref := fieldString(image, sessionImageDataRefField); ref != "" {
					image["deferredImageRef"] = ref
					image["data"] = ""
					delete(image, sessionImageDataRefField)
				}
			}
		}
	}
}

func (r *providerChatRuntime) ProjectToolImageByTab(tabID, ref string) ([]any, bool, error) {
	found, err := r.authorizeSessionImageByTab(tabID, ref)
	if err != nil || !found {
		return nil, found, err
	}
	data, err := readExternalSessionImage(ref, filepath.Dir(r.sessions.path))
	if err != nil {
		return nil, false, err
	}
	return []any{map[string]any{"data": data}}, true, nil
}

// authorizeSessionImageByTab is the one ownership check every chat image read
// passes, over the socket (archive-load imageRef) or HTTP (chat-image): the
// tab's chat must exist, not be deleted, and own the reference. found is false
// with no error when no chat has the tab. A closed chat answers from its rows
// without being opened.
func (r *providerChatRuntime) authorizeSessionImageByTab(tabID, ref string) (bool, error) {
	if _, ok := validSessionImageRef(ref); !ok {
		return false, errors.New("invalid archive image reference")
	}
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return found, err
	}
	// Tool media and, once deferred, a message's own images. The ownership
	// check reads the actor in place: cloning it per image (Snapshot) made each
	// read of a 225 MB chat cost ~15 ms before any image byte was read.
	ownership, err := r.attachmentRefOwnership(chatID, providerSessionImageRefPrefix+ref)
	if err != nil {
		return false, err
	}
	if ownership.Deleted {
		return false, errors.New("chat is deleted")
	}
	if !ownership.Owned {
		return false, errors.New("archive image does not belong to this chat")
	}
	return true, nil
}

// actorByTab opens the one actor attached to tabID (chatByTab finds it
// without opening any other).
func (r *providerChatRuntime) actorByTab(tabID string) (*providerChatActor, bool, error) {
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return nil, found, err
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return nil, false, err
	}
	return actor, true, nil
}

// ledgerPageBefore is Engine.ReadLedgerPageBefore of any known actor. A
// closed one's page comes from its rows and carries only the rows the reply
// keeps under opts (closedHistoryRows) and, unless its window had to be read
// whole, those rows already finished for the wire.
func (r *providerChatRuntime) ledgerPageBefore(chatID, beforeMessageID string, limit int, opts []archiveReadOptions) (chat.LedgerPageSnapshot, *archivePage, error) {
	if summary, closed := r.closedSummary(chatID); closed {
		beforeMessageID = strings.TrimSpace(beforeMessageID)
		if beforeMessageID == "" {
			return chat.LedgerPageSnapshot{}, nil, errors.New("ledger page requires a stable before-message identity")
		}
		rows, finished, err := r.closedHistoryRows(summary, beforeMessageID, limit, opts)
		if err == nil {
			return chat.LedgerPageSnapshot{Events: rows, LedgerCount: summary.LedgerCount, Deleted: summary.Deleted}, finished, nil
		}
		if !errors.Is(err, errClosedActorChanged) {
			return chat.LedgerPageSnapshot{}, nil, err
		}
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return chat.LedgerPageSnapshot{}, nil, err
	}
	page, err := actor.engine.ReadLedgerPageBefore(beforeMessageID, limit)
	return page, nil, err
}

// ledgerMessage is Engine.ReadMessage of any known actor.
func (r *providerChatRuntime) ledgerMessage(chatID, messageID string) (chat.LedgerEvent, bool, error) {
	if summary, closed := r.closedSummary(chatID); closed {
		if summary.Deleted || strings.TrimSpace(messageID) == "" {
			return chat.LedgerEvent{}, false, nil
		}
		event, found, err := r.ledgers.ReadLedgerMessage(chatID, closedView(summary), messageID)
		if err = r.closedReadError(summary, err); !errors.Is(err, errClosedActorChanged) {
			return event, found, err
		}
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return chat.LedgerEvent{}, false, err
	}
	event, found := actor.engine.ReadMessage(messageID)
	return event, found, nil
}

// attachmentRefOwnership is Engine.AttachmentRefOwnership of any known actor.
func (r *providerChatRuntime) attachmentRefOwnership(chatID, ref string) (chat.AttachmentOwnership, error) {
	if summary, closed := r.closedSummary(chatID); closed {
		if summary.Deleted {
			return chat.AttachmentOwnership{Deleted: true}, nil
		}
		refs, err := r.closedAttachmentRefs(summary)
		if err = r.closedReadError(summary, err); err == nil {
			_, owned := refs[ref]
			return chat.AttachmentOwnership{Owned: owned}, nil
		}
		if !errors.Is(err, errClosedActorChanged) {
			return chat.AttachmentOwnership{}, err
		}
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return chat.AttachmentOwnership{}, err
	}
	return actor.engine.AttachmentRefOwnership(ref), nil
}

// ApplyRendererSnapshot translates the frozen session:save payload into typed
// actor commands. Renderer-authored messages, provider runtime fields, lane
// bindings, permissions, usage and outbox state are ignored; after the actor
// commit, the global presentation store receives only daemon-wide UI fields.
func (r *providerChatRuntime) ApplyRendererSnapshot(snapshot any) (bool, error) {
	_, err := r.applyRendererSnapshot(snapshot)
	return err == nil, err
}

func (r *providerChatRuntime) applyRendererSnapshot(snapshot any) (globalPresentationSaveResult, error) {
	if r == nil || r.sessions == nil {
		return globalPresentationSaveResult{}, errors.New("provider chat projection is unavailable")
	}
	detached := mapFromAnyMain(cloneJSON(redactSessionValue(snapshot)))
	if detached == nil {
		return globalPresentationSaveResult{}, errors.New("renderer session snapshot is not an object")
	}
	// Chat rows are deliberately ignored here. Each chat presentation and queue
	// mutation, including deletion, has its own stable-id actor command;
	// session:save persists daemon-global UI state only.
	result, err := r.sessions.SaveActorGlobalSnapshot(detached)
	if err != nil {
		return globalPresentationSaveResult{}, fmt.Errorf("persist daemon-global presentation state: %w", err)
	}
	detached[globalPresentationRevisionField] = int(result.Revision)
	return result, nil
}

func projectActorChat(out map[string]any, state chat.State) error {
	return projectActorChatWithHistory(out, state, actorHistoryFull)
}

func projectActorChatWithHistory(out map[string]any, state chat.State, history actorHistoryProjection) error {
	return projectActorChatWithHistoryLimit(out, state, history, sessionProjectionMessageTail)
}

func projectActorChatWithHistoryLimit(out map[string]any, state chat.State, history actorHistoryProjection, tailLimit int) error {
	return projectActorChatWithHistoryWindow(out, state, history, tailLimit, actorLedgerWindow{count: len(state.Ledger)})
}

type actorLedgerWindow struct {
	lastActivityAt int64
	offset         int
	count          int
}

// projectSubsessionParent names the chat a subsession lives under. A forked
// subsession says so only about itself (subsessionForked); the parent's own
// projection never distinguishes its children (user 2026-10-16).
func projectSubsessionParent(out map[string]any, parent *chat.ParentLink) {
	if parent == nil {
		delete(out, "parentChatId")
		delete(out, "parentTabId")
		delete(out, "subsessionForked")
		return
	}
	out["parentChatId"] = parent.ChatID
	out["parentTabId"] = parent.TabID
	if parent.Forked {
		out["subsessionForked"] = true
	} else {
		delete(out, "subsessionForked")
	}
}

func projectActorChatWithHistoryWindow(out map[string]any, state chat.State, history actorHistoryProjection, tailLimit int, window actorLedgerWindow) error {
	state = state.PresentationState()
	if out == nil {
		return errors.New("chat projection target is nil")
	}
	if window.offset < 0 || window.count < 0 || window.offset+len(state.Ledger) != window.count {
		return errors.New("chat projection ledger window is invalid")
	}
	if !state.Initialized {
		return errors.New("chat actor initialization is incomplete")
	}
	if state.Deleted {
		return errors.New("deleted chat cannot be projected")
	}
	p := state.Presentation
	out["id"] = p.TabID
	out["chatId"] = state.ChatID
	out["actorRevision"] = state.Revision
	projectSubsessionParent(out, state.Parent)
	out["title"] = p.Title
	out["titleLocked"] = p.TitleLocked
	setOptionalProjectionString(out, "group", p.Group)
	setOptionalProjectionString(out, "cwd", p.CWD)
	out["draft"] = p.Draft
	out["unread"] = p.Unread
	out[presentationRevisionField] = p.PresentationRevision
	setOptionalProjectionString(out, "pane", p.Pane)
	setStringOrNil(out, "providerId", string(p.ProviderID))
	setStringOrNil(out, "currentModelId", p.CurrentModelID)
	setStringOrNil(out, "currentModeId", p.CurrentModeID)
	if err := setRawProjection(out, "modelControls", p.ModelControls); err != nil {
		return err
	}
	if err := projectActorUsage(out, state); err != nil {
		return err
	}
	if p.Settled == "" {
		delete(out, "settled")
	} else {
		out["settled"] = p.Settled
	}
	if p.SettledAt == 0 {
		delete(out, "settledAt")
	} else {
		out["settledAt"] = p.SettledAt
	}
	if lastActivityAt := max(actorLastActivityAt(state), window.lastActivityAt); lastActivityAt > 0 {
		out["lastActivityAt"] = lastActivityAt
	} else {
		delete(out, "lastActivityAt")
	}
	if p.WorkspaceRevision == 0 {
		delete(out, workspaceRevisionField)
	} else {
		out[workspaceRevisionField] = p.WorkspaceRevision
	}
	if p.AgentQueueRevision == 0 {
		delete(out, agentQueueRevisionField)
	} else {
		out[agentQueueRevisionField] = p.AgentQueueRevision
	}
	if p.RuntimeControlRevision == 0 {
		delete(out, runtimeControlRevisionField)
	} else {
		out[runtimeControlRevisionField] = p.RuntimeControlRevision
	}
	if len(p.PlanLatest) == 0 {
		out["planLatest"] = []any{}
	} else {
		entries := make([]any, 0, len(p.PlanLatest))
		for _, entry := range p.PlanLatest {
			entries = append(entries, map[string]any{"status": entry.Status, "content": entry.Text})
		}
		out["planLatest"] = entries
	}
	if p.PlanLatestMessageID == "" {
		delete(out, "planLatestMessageId")
	} else {
		out["planLatestMessageId"] = p.PlanLatestMessageID
	}

	messages, messageCount, err := projectActorMessagesWithLedgerWindow(state, history, tailLimit, window)
	if err != nil {
		return err
	}
	out["messages"] = messages
	if history != actorHistoryFull {
		out["messageCount"] = messageCount
		out["historyComplete"] = len(messages) == messageCount
	}
	queue, err := projectActorQueue(state.StagedQueue, state.Queue)
	if err != nil {
		return err
	}
	out["queue"] = queue
	out["pending"] = actorForegroundRunning(state.Foreground)
	delete(out, "serverAuthored")
	delete(out, "liveSession")
	delete(out, "sessionId")
	delete(out, "sessionProviderId")
	delete(out, "sessionError")
	if lane, ok := state.Lanes[state.ActiveLaneID]; ok {
		out["sessionProviderId"] = string(lane.Identity.Realm.ProviderID)
		if !lane.Thread.IsZero() {
			out["sessionId"] = lane.Thread.HeadID
		}
		if lane.Attachment != nil {
			out["liveSession"] = projectLaneAttachment(*lane.Attachment, lane.Delivery)
		}
	}
	if lane, ok := state.Lanes[state.DesiredLaneID]; ok && (lane.Phase == chat.LaneBlocked || lane.Phase == chat.LaneBroken) {
		out["sessionError"] = string(lane.LastError)
	}
	return nil
}

func actorForegroundRunning(foreground *chat.ForegroundTurn) bool {
	if foreground == nil {
		return false
	}
	switch foreground.Status {
	case chat.ForegroundDispatching, chat.ForegroundRunning:
		return true
	default:
		return false
	}
}

// actorLastActivityAt projects the lifecycle clock independently of transcript
// residency. Ledger order is canonical, so the newest parseable row is the
// latest committed activity; a foreground turn may be newer still.
func actorLastActivityAt(state chat.State) int64 {
	latest := int64(0)
	for index := len(state.Ledger) - 1; index >= 0; index-- {
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(state.Ledger[index].At))
		if err != nil {
			continue
		}
		latest = at.UnixMilli()
		break
	}
	if foreground := state.Foreground; foreground != nil {
		if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(foreground.StartedAt)); err == nil && at.UnixMilli() > latest {
			latest = at.UnixMilli()
		}
	}
	return latest
}

// projectActorMessages materializes only the history requested by the
// projection policy. Full archive reads carry every row; bounded actor reads
// supply a contiguous suffix plus its canonical absolute offset/count;
// metadata-only session rows retain just live foreground rows that have not
// reached the ledger yet.
func projectActorMessages(state chat.State, history actorHistoryProjection) ([]any, int, error) {
	return projectActorMessagesWithTailLimit(state, history, sessionProjectionMessageTail)
}

func projectActorMessagesWithTailLimit(state chat.State, history actorHistoryProjection, tailLimit int) ([]any, int, error) {
	return projectActorMessagesWithLedgerWindow(state, history, tailLimit, actorLedgerWindow{count: len(state.Ledger)})
}

// uniqueLedgerMessageIDs fails, as projectActorMessagesWithLedgerWindow does,
// when two rows share a stable message id.
func uniqueLedgerMessageIDs(ledger []chat.LedgerEvent) error {
	seen := make(map[string]struct{}, len(ledger))
	for _, event := range ledger {
		if _, duplicate := seen[event.MessageID]; duplicate {
			return fmt.Errorf("actor ledger contains duplicate message id %q", event.MessageID)
		}
		seen[event.MessageID] = struct{}{}
	}
	return nil
}

func projectActorMessagesWithLedgerWindow(state chat.State, history actorHistoryProjection, tailLimit int, window actorLedgerWindow) ([]any, int, error) {
	state = state.PresentationState()
	if history == actorHistoryTail && (tailLimit < 1 || tailLimit > sessionProjectionMessageTail) {
		return nil, 0, fmt.Errorf("actor history tail limit must be between 1 and %d", sessionProjectionMessageTail)
	}
	if window.offset < 0 || window.count < 0 || window.offset+len(state.Ledger) != window.count {
		return nil, 0, errors.New("actor history ledger window is invalid")
	}
	seenMessage := make(map[string]struct{}, len(state.Ledger)+4)
	for _, event := range state.Ledger {
		if _, duplicate := seenMessage[event.MessageID]; duplicate {
			return nil, 0, fmt.Errorf("actor ledger contains duplicate message id %q", event.MessageID)
		}
		seenMessage[event.MessageID] = struct{}{}
	}

	extra := make([]map[string]any, 0, 4)
	if foreground := state.Foreground; foreground != nil {
		if !foreground.UserConsumed {
			userID := strings.TrimSpace(foreground.Input.Presentation.UserMessageID)
			if userID == "" {
				userID = fmt.Sprintf("message:%s:user", foreground.OperationID)
			}
			if _, exists := seenMessage[userID]; !exists {
				user := map[string]any{
					"id": userID, "role": "user", "content": foreground.Input.Text,
					"status": "pending", "at": nilIfEmpty(foreground.StartedAt), "events": []any{},
				}
				presentation := foreground.Input.Presentation
				if notice := providercontract.NoticeView(presentation.NoticeKind, presentation.NoticeTitle, presentation.NoticeStatus, foreground.Input.Text); notice != nil {
					user["notice"] = notice
				}
				if queueID := strings.TrimSpace(foreground.Input.Presentation.QueueID); queueID != "" {
					user[agentQueueMessageField] = queueID
				}
				images, err := projectionAttachments(foreground.Input.Attachments)
				if err != nil {
					return nil, 0, err
				}
				if len(images) > 0 {
					user["images"] = images
				}
				extra = append(extra, user)
				seenMessage[userID] = struct{}{}
			}
		}

		assistantID := strings.TrimSpace(foreground.CurrentAssistantMessageID)
		if _, exists := seenMessage[assistantID]; !exists {
			events, err := projectTimeline(foreground.Timeline)
			if err != nil {
				return nil, 0, err
			}
			assistant := map[string]any{
				"id": assistantID, "role": "assistant", "content": foreground.AssistantContent,
				"status": "running", "at": nil, "events": events,
			}
			if startedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(foreground.StartedAt)); err == nil {
				assistant["turnStartedAt"] = startedAt.UnixMilli()
			}
			if foreground.AssistantResult != "" {
				assistant["result"] = foreground.AssistantResult
			}
			images, err := projectionAttachments(foreground.AssistantAttachments)
			if err != nil {
				return nil, 0, err
			}
			if len(images) > 0 {
				assistant["images"] = images
			}
			if permission := projectPermission(foreground.Permission); permission != nil {
				assistant["permission"] = permission
			}
			if foreground.Turn.NativeID != "" {
				assistant["jobId"] = foreground.Turn.NativeID
			}
			if assistantID != foreground.RootAssistantMessageID {
				assistant["turnRootId"] = foreground.RootAssistantMessageID
				assistant["turnTerminal"] = true
			}
			extra = append(extra, assistant)
			seenMessage[assistantID] = struct{}{}
		}

		for pending := state.PendingSteer; pending != nil; pending = pending.Next {
			steerRows, err := projectPendingSteer(pending, foreground)
			if err != nil {
				return nil, 0, err
			}
			for _, row := range steerRows {
				id := fieldString(row, "id")
				if _, duplicate := seenMessage[id]; duplicate {
					return nil, 0, fmt.Errorf("pending steer duplicates message id %q", id)
				}
				seenMessage[id] = struct{}{}
				extra = append(extra, row)
			}
		}
	}

	total := window.count + len(extra)
	first := 0
	switch history {
	case actorHistoryMetadataOnly:
		first = window.count
	case actorHistoryTail:
		if total > tailLimit {
			first = total - tailLimit
		}
	case actorHistoryFull:
	default:
		return nil, 0, fmt.Errorf("unknown actor history projection %d", history)
	}
	messages := make([]any, 0, total-first)
	ledgerStart := max(first, window.offset)
	for absoluteIndex := ledgerStart; absoluteIndex < window.count; absoluteIndex++ {
		message, err := projectLedgerMessage(state.Ledger[absoluteIndex-window.offset])
		if err != nil {
			return nil, 0, err
		}
		messages = append(messages, message)
	}
	extraStart := max(0, first-window.count)
	for index := extraStart; index < len(extra); index++ {
		messages = append(messages, extra[index])
	}
	return messages, total, nil
}

func projectActorTerminalJob(state chat.State, operationID providercontract.OperationID, turn providercontract.TurnRef) (map[string]any, error) {
	var user *chat.LedgerEvent
	var assistant *chat.LedgerEvent
	for index := range state.Ledger {
		event := &state.Ledger[index]
		if event.OperationID != operationID {
			continue
		}
		if event.Role == "user" && user == nil {
			user = event
		}
		if event.Role == "assistant" && event.Terminal != nil {
			assistant = event
		}
	}
	if assistant == nil || assistant.Terminal == nil {
		return nil, errors.New("terminal operation is missing its actor ledger receipt")
	}
	terminal := assistant.Terminal
	lane, ok := state.Lanes[assistant.LaneID]
	if !ok {
		return nil, errors.New("terminal operation lost its exact provider lane")
	}
	jobID := firstNonEmptyString(turn.NativeID, assistant.NativeTurnID, providercontract.DeriveJobID(state.ChatID, operationID))
	if jobID == "" {
		return nil, errors.New("terminal operation is missing its native turn id")
	}
	status := strings.ToLower(strings.TrimSpace(terminal.Status))
	jobStatus := "failed"
	code := any(1)
	if status == "completed" || status == "done" {
		jobStatus = "done"
		code = 0
	} else if status == "cancelled" || strings.EqualFold(terminal.StopReason, "cancelled") {
		code = 130
	}
	if terminal.Code != nil {
		code = *terminal.Code
	}
	startedAt := ""
	promptText := ""
	userMessageID := ""
	if user != nil {
		startedAt, promptText, userMessageID = user.At, user.Text, user.MessageID
	}
	finishedAt := firstNonEmptyString(terminal.FinishedAt, assistant.At)
	if finishedAt == "" {
		finishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	result := firstNonEmptyString(terminal.Result, assistant.Result, assistant.Text)
	consumedSteers := make([]string, 0, len(terminal.ConsumedSteerIDs))
	for _, id := range terminal.ConsumedSteerIDs {
		consumedSteers = append(consumedSteers, string(id))
	}
	job := map[string]any{
		"id": jobID, "kind": "app-chat", "key": nil, "title": state.Presentation.Title,
		"status": jobStatus, "startedAt": startedAt, "finishedAt": finishedAt, "code": code,
		"permissionMode": "", "chatId": state.ChatID, "tabId": nullableString(state.Presentation.TabID),
		"sessionId": nullableString(lane.Thread.HeadID), "providerId": string(lane.Identity.Realm.ProviderID),
		"userMessageId": nullableString(userMessageID), "assistantMessageId": nullableString(assistant.MessageID),
		"promptText": promptText, "result": nullableString(result), "error": nullableString(terminal.Error),
		"stopReason": nullableString(terminal.StopReason), "crashInterrupted": terminal.CrashInterrupted,
		"interrupted": terminal.Interrupted, "consumedSteerIds": consumedSteers,
	}
	if user != nil {
		if notice := projectNotice(*user); notice != nil {
			job["notice"] = notice
		}
	}
	if strings.TrimSpace(terminal.DispositionState) != "" {
		disposition := map[string]any{"state": terminal.DispositionState}
		if strings.TrimSpace(terminal.DispositionSource) != "" {
			disposition["source"] = terminal.DispositionSource
		}
		job["disposition"] = disposition
	}
	return map[string]any{"type": "end", "job": job}, nil
}

func projectLaneAttachment(snapshot providercontract.LaneAttachmentSnapshot, delivery providercontract.DeliveryCapabilities) map[string]any {
	models := make([]any, 0, len(snapshot.Models))
	for _, model := range snapshot.Models {
		models = append(models, map[string]any{
			"modelId": model.ID, "name": model.Name, "efforts": append([]string(nil), model.Efforts...),
			"serviceTiers": append([]string(nil), model.ServiceTiers...),
		})
	}
	modes := make([]any, 0, len(snapshot.Modes))
	for _, mode := range snapshot.Modes {
		modes = append(modes, map[string]any{"id": mode.ID, "name": mode.Name})
	}
	projected := map[string]any{
		"sessionId": snapshot.ConnectionID, "cwd": snapshot.CWD, "agent": snapshot.Agent,
		"providerId": string(snapshot.ProviderID), "providerName": snapshot.ProviderName,
		"models": models, "currentModelId": nullableDigestString(snapshot.CurrentModelID),
		"modes": modes, "currentModeId": nullableDigestString(snapshot.CurrentModeID),
		"imageSupport":       snapshot.ImageSupport,
		"planUsageSupported": snapshot.PlanUsageSupported, "planUsageResetSupported": snapshot.PlanUsageResetSupported,
		"commandCatalogSupported": snapshot.CommandCatalogSupported,
		"deliveryCapabilities":    projectDeliveryCapabilities(delivery),
	}
	if snapshot.CommandCatalog != nil {
		projected["commandCatalog"] = projectRuntimeCommandCatalog(snapshot.CommandCatalog)
	}
	return projected
}

func projectDeliveryCapabilities(capabilities providercontract.DeliveryCapabilities) map[string]any {
	projected := map[string]any{
		"stableInputIdentity":     capabilities.StableInputIdentity,
		"liveSteer":               capabilities.LiveSteer,
		"steerConsumptionReceipt": capabilities.SteerConsumptionReceipt,
		"consumptionReceipt":      capabilities.ConsumptionReceipt,
		// Frozen renderer compatibility; native turn readback is retired.
		"turnReadback": false,
	}
	if capabilities.StopAndSend {
		projected["stopAndSend"] = true
	}
	return projected
}

func projectRuntimeCommandCatalog(catalog *providercontract.RuntimeCommandCatalog) any {
	if catalog == nil {
		return nil
	}
	commands := make([]any, 0, len(catalog.Commands))
	for _, command := range catalog.Commands {
		commands = append(commands, map[string]any{
			"name": command.Name, "description": command.Description,
			"argumentHint": command.ArgumentHint, "aliases": append([]string(nil), command.Aliases...),
		})
	}
	agents := make([]any, 0, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		agents = append(agents, map[string]any{"name": agent.Name, "description": agent.Description, "model": agent.Model})
	}
	return map[string]any{
		"commands": commands, "agents": agents, "outputStyle": catalog.OutputStyle,
		"availableOutputStyles": append([]string(nil), catalog.AvailableOutputStyles...),
		"commandsTruncated":     catalog.CommandsTruncated, "agentsTruncated": catalog.AgentsTruncated,
		"stylesTruncated": catalog.StylesTruncated, "asOf": catalog.AsOf,
	}
}

func projectLedgerMessage(event chat.LedgerEvent) (map[string]any, error) {
	message := map[string]any{}
	message["id"] = event.MessageID
	message["role"] = event.Role
	message["content"] = event.Text
	// Only fork initialization excludes rows: they are the history this chat
	// inherited from the chat it was forked from (shown folded in the child).
	if event.ContextExcluded || event.Inherited {
		message["inherited"] = true
	}
	if notice := projectNotice(event); notice != nil {
		message["notice"] = notice
	}
	if event.Result == "" {
		delete(message, "result")
	} else {
		message["result"] = event.Result
	}
	status := event.Status
	if status == "" {
		status = "done"
	}
	message["status"] = status
	if event.At == "" {
		message["at"] = nil
	} else {
		message["at"] = event.At
	}
	if len(event.Timeline) > 0 {
		events, err := projectTimeline(event.Timeline)
		if err != nil {
			return nil, err
		}
		message["events"] = events
	} else if _, exists := message["events"]; !exists {
		message["events"] = []any{}
	}
	images, err := projectionAttachments(event.Attachments)
	if err != nil {
		return nil, err
	}
	if len(images) > 0 {
		message["images"] = images
	}
	if event.SteerState != "" {
		message["steerState"] = event.SteerState
	} else {
		delete(message, "steerState")
	}
	if event.SteerBoundary != "" {
		message["steerBoundary"] = event.SteerBoundary
	}
	if event.SteerContinuationID != "" {
		message["steerContinuationId"] = event.SteerContinuationID
	}
	if event.SteerContinuationFor != "" {
		message["steerContinuationFor"] = event.SteerContinuationFor
	}
	if event.TurnRootID != "" {
		message["turnRootId"] = event.TurnRootID
	} else {
		delete(message, "turnRootId")
	}
	if event.TurnTerminal != nil {
		message["turnTerminal"] = *event.TurnTerminal
	} else {
		delete(message, "turnTerminal")
	}
	if permission := projectPermission(event.Permission); permission != nil {
		message["permission"] = permission
	} else {
		delete(message, "permission")
	}
	if event.NativeTurnID != "" {
		message["jobId"] = event.NativeTurnID
	}
	if event.TurnStartedAt > 0 {
		message["turnStartedAt"] = event.TurnStartedAt
	}
	if event.QueueID != "" {
		message[agentQueueMessageField] = event.QueueID
	} else {
		delete(message, agentQueueMessageField)
	}
	if event.Interrupted {
		message["interrupted"] = true
	} else {
		delete(message, "interrupted")
	}
	// Old actor snapshots may contain the retired retryPrompt field. Never
	// project it: recovery buttons could resend an already accepted provider
	// prompt. A distinct new prompt resumes the saved native session instead.
	delete(message, "retryPrompt")
	return message, nil
}

func projectPendingSteer(pending *chat.PendingSteer, foreground *chat.ForegroundTurn) ([]map[string]any, error) {
	if pending == nil || foreground == nil {
		return nil, nil
	}
	userID := strings.TrimSpace(pending.Presentation.UserMessageID)
	if userID == "" {
		userID = fmt.Sprintf("message:%s:user", pending.OperationID)
	}
	continuationID := strings.TrimSpace(pending.Presentation.AssistantMessageID)
	if continuationID == "" {
		continuationID = foreground.RootAssistantMessageID + "~after~" + string(pending.OperationID)
	}
	steerState := "sending"
	status := "pending"
	switch pending.Status {
	case chat.SteerAccepted:
		steerState, status = "accepted", "done"
	case chat.SteerUncertain:
		steerState, status = "uncertain", "done"
	}
	images, err := projectionAttachments(pending.Attachments)
	if err != nil {
		return nil, err
	}
	user := map[string]any{
		"id": userID, "role": "user", "content": pending.Text, "status": status,
		"at": nilIfEmpty(pending.Presentation.StartedAt), "events": []any{},
		"steerState": steerState, "turnRootId": foreground.RootAssistantMessageID,
	}
	if queueID := strings.TrimSpace(pending.Presentation.QueueID); queueID != "" {
		user[agentQueueMessageField] = queueID
	}
	if len(images) > 0 {
		user["images"] = images
	}
	assistant := map[string]any{
		"id": continuationID, "role": "assistant", "content": "", "status": "pending", "at": nil,
		"events": []any{}, "jobId": foreground.Turn.NativeID,
		"turnRootId": foreground.RootAssistantMessageID, "turnTerminal": true,
	}
	if pending.AwaitConsumption || pending.Status == chat.SteerDispatching {
		user["steerBoundary"] = "waiting"
		user["steerContinuationId"] = continuationID
		assistant["steerBoundary"] = "waiting"
		assistant["steerContinuationFor"] = userID
	}
	return []map[string]any{user, assistant}, nil
}

func projectTimeline(entries []chat.TimelineEntry) ([]any, error) {
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		item := map[string]any{"key": entry.Key, "at": entry.At}
		switch entry.Kind {
		case providercontract.EventAssistantChunk:
			if entry.Result == nil {
				return nil, errors.New("assistant result timeline entry lost its typed payload")
			}
			item["kind"], item["start"], item["end"] = "result", entry.Result.Start, entry.Result.End
		case providercontract.EventThinkingUpdate:
			if entry.Thinking == nil {
				return nil, errors.New("thinking timeline entry lost its typed payload")
			}
			item["kind"], item["text"] = "thinking", entry.Thinking.Text
		case providercontract.EventPlanUpdate:
			if entry.Plan == nil {
				return nil, errors.New("plan timeline entry lost its typed payload")
			}
			projected := make([]any, 0, len(entry.Plan.Entries))
			for _, planEntry := range entry.Plan.Entries {
				projected = append(projected, map[string]any{"status": planEntry.Status, "content": planEntry.Text})
			}
			item["kind"], item["entries"] = "plan", projected
		case providercontract.EventToolUpdate:
			if entry.Tool == nil {
				return nil, errors.New("tool timeline entry lost its typed payload")
			}
			tool := entry.Tool
			item["kind"] = "tool"
			item["id"] = nullableString(tool.ToolCallID)
			item["toolKind"] = nullableString(tool.ToolKind)
			item["title"], item["status"] = tool.Title, tool.Status
			item["command"] = nullableString(tool.Command)
			item["terminalId"] = nullableString(tool.TerminalID)
			item["input"] = nullableString(tool.Input)
			item["output"] = nullableString(tool.Output)
			item["location"] = nullableString(tool.Location)
			images, err := projectionAttachments(tool.Attachments)
			if err != nil {
				return nil, err
			}
			if len(images) > 0 {
				item["images"] = images
			}
			if tool.StartedAtUnixMS > 0 {
				item["startedAt"] = tool.StartedAtUnixMS
			}
			if tool.EndedAtUnixMS > 0 {
				item["endedAt"] = tool.EndedAtUnixMS
			}
			if tool.SubagentID != "" {
				item["subagentId"] = tool.SubagentID
			}
			if tool.SubagentLabel != "" {
				item["subagentLabel"] = tool.SubagentLabel
			}
			if tool.SubagentProvider != "" {
				item["subagentProvider"] = tool.SubagentProvider
			}
			if tool.SubagentModel != "" {
				item["subagentModel"] = tool.SubagentModel
			}
			if tool.SubagentHeader {
				item["subagentHeader"] = true
			}
			if tool.SubagentForked {
				item["subagentForked"] = true
			}
		case providercontract.EventCompactionStarted, providercontract.EventCompactionCheckpoint:
			if entry.Compaction == nil {
				return nil, errors.New("compaction timeline entry lost its typed payload")
			}
			item["kind"] = "compaction"
		case providercontract.EventCheckpointRestored:
			if entry.Restored == nil || entry.Restored.TurnSequence <= 0 {
				return nil, errors.New("checkpoint restore timeline entry lost its typed payload")
			}
			item["kind"], item["turnSeq"] = "restored", entry.Restored.TurnSequence
		default:
			return nil, fmt.Errorf("unsupported actor timeline event %q", entry.Kind)
		}
		out = append(out, item)
	}
	return out, nil
}

func projectPermission(permission *providercontract.PermissionEvent) map[string]any {
	if permission == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(permission.Status)) {
	case "resolved", "failed":
		return nil
	}
	options := make([]any, 0, max(len(permission.OptionDetails), len(permission.Options)))
	if len(permission.OptionDetails) > 0 {
		for _, option := range permission.OptionDetails {
			options = append(options, map[string]any{"optionId": option.ID, "name": option.Name, "kind": option.Kind})
		}
	} else {
		for _, optionID := range permission.Options {
			options = append(options, map[string]any{"optionId": optionID, "name": optionID, "kind": ""})
		}
	}
	out := map[string]any{
		"id": permission.RequestID, "title": permission.Title,
		"kind": nullableString(permission.Kind), "options": options,
	}
	if permission.Question != nil {
		questionOptions := make([]any, 0, len(permission.Question.Options))
		for _, option := range permission.Question.Options {
			item := map[string]any{"label": option.Label, "description": option.Description}
			if option.ID != "" {
				item["id"] = option.ID
			}
			questionOptions = append(questionOptions, item)
		}
		question := map[string]any{
			"question": permission.Question.Question, "header": permission.Question.Header,
			"options": questionOptions, "multiSelect": permission.Question.MultiSelect,
		}
		if permission.Question.WorkassTool {
			question["workassTool"] = true
			question["questionId"] = permission.Question.ID
			question["operationId"] = permission.Question.OperationID
			question["allowFreeText"] = permission.Question.AllowFreeText
		}
		out["question"] = question
	}
	return out
}

func projectActorQueue(staged []chat.StagedQueueEntry, queue []chat.QueueEntry) ([]any, error) {
	out := make([]any, 0, len(staged)+len(queue))
	for _, entry := range staged {
		item := map[string]any{"id": entry.ID, "text": entry.Text}
		if entry.Source != "" {
			item["source"] = entry.Source
		}
		if entry.Delivery != "" {
			item["delivery"] = entry.Delivery
		}
		if entry.QueuedAt != "" {
			item["queuedAt"] = entry.QueuedAt
		}
		images, err := projectionAttachments(entry.Attachments)
		if err != nil {
			return nil, err
		}
		if len(images) > 0 {
			item["images"] = images
		}
		if len(entry.AttachmentNames) > 0 {
			item["attachmentNames"] = append([]string(nil), entry.AttachmentNames...)
		}
		if entry.AttachmentState != "" {
			item["attachmentState"] = entry.AttachmentState
		}
		if entry.AttachmentError != "" {
			item["attachmentError"] = entry.AttachmentError
		}
		out = append(out, item)
	}
	for _, entry := range queue {
		id := strings.TrimSpace(entry.Presentation.QueueID)
		if id == "" {
			id = string(entry.OperationID)
		}
		item := map[string]any{
			"id": id, "text": entry.Text, "delivery": "queue",
		}
		if notice := providercontract.NoticeView(entry.Presentation.NoticeKind, entry.Presentation.NoticeTitle, entry.Presentation.NoticeStatus, entry.Text); notice != nil {
			item["notice"] = notice
		}
		if entry.Presentation.Origin != "" {
			source := entry.Presentation.Origin
			if source == "human" {
				source = "host"
			}
			item["source"] = source
		}
		images, err := projectionAttachments(entry.Attachments)
		if err != nil {
			return nil, err
		}
		if len(images) > 0 {
			item["images"] = images
		}
		out = append(out, item)
	}
	return out, nil
}

func nilIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func projectionAttachments(attachments []providercontract.Attachment) ([]any, error) {
	out := make([]any, 0, len(attachments))
	for index, attachment := range attachments {
		ref := strings.TrimPrefix(strings.TrimSpace(attachment.Ref), providerSessionImageRefPrefix)
		if ref == "" || ref == strings.TrimSpace(attachment.Ref) {
			return nil, fmt.Errorf("actor attachment %d is missing a durable session image reference", index)
		}
		item := map[string]any{
			"mimeType": attachment.MIMEType, "name": attachment.Name,
			sessionImageDataRefField: ref,
		}
		if attachment.Source != "" {
			item["source"] = attachment.Source
		}
		setWireImageSize(item, attachment.Width, attachment.Height)
		out = append(out, item)
	}
	return out, nil
}

func setOptionalProjectionString(target map[string]any, key string, value *string) {
	if value == nil {
		target[key] = nil
		return
	}
	target[key] = *value
}

func setStringOrNil(target map[string]any, key, value string) {
	if strings.TrimSpace(value) == "" {
		target[key] = nil
		return
	}
	target[key] = value
}

func setRawProjection(target map[string]any, key string, raw json.RawMessage) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		delete(target, key)
		return nil
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("actor presentation field %q is invalid: %w", key, err)
	}
	target[key] = value
	return nil
}

func projectActorUsage(target map[string]any, state chat.State) error {
	byProvider := make(map[string]any)
	if raw := state.Presentation.ContextUsageByProvider; len(raw) > 0 && strings.TrimSpace(string(raw)) != "null" {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&byProvider); err != nil {
			return fmt.Errorf("actor presentation field %q is invalid: %w", "contextUsageByProvider", err)
		}
		if byProvider == nil {
			byProvider = make(map[string]any)
		}
	}
	type observedUsage struct {
		laneID string
		value  providercontract.UsageEvent
	}
	latest := make(map[string]observedUsage)
	for laneID, usage := range state.Usage {
		lane, ok := state.Lanes[laneID]
		if !ok {
			return fmt.Errorf("actor usage belongs to unknown lane %q", laneID)
		}
		providerID := strings.TrimSpace(string(lane.Identity.Realm.ProviderID))
		used := usage.Used
		if used <= 0 {
			used = usage.InputTokens + usage.OutputTokens
		}
		if providerID == "" || used < 0 || usage.Size <= 0 {
			continue
		}
		prior, exists := latest[providerID]
		if exists && (prior.value.ObservedAtUnixMS > usage.ObservedAtUnixMS ||
			(prior.value.ObservedAtUnixMS == usage.ObservedAtUnixMS && prior.laneID >= string(laneID))) {
			continue
		}
		latest[providerID] = observedUsage{laneID: string(laneID), value: usage}
	}
	for providerID, observed := range latest {
		usage := observed.value
		used := usage.Used
		if used <= 0 {
			used = usage.InputTokens + usage.OutputTokens
		}
		projected := map[string]any{"used": used, "size": usage.Size}
		if usage.ObservedAtUnixMS > 0 {
			projected["updatedAt"] = time.UnixMilli(usage.ObservedAtUnixMS).UTC().Format(time.RFC3339Nano)
		}
		byProvider[providerID] = projected
	}
	if len(byProvider) == 0 {
		delete(target, "contextUsageByProvider")
	} else {
		target["contextUsageByProvider"] = byProvider
	}
	selected := strings.TrimSpace(string(state.Presentation.ProviderID))
	if selected != "" {
		if value, ok := byProvider[selected]; ok {
			target["usage"] = cloneJSON(value)
			return nil
		}
	}
	delete(target, "usage")
	return nil
}

// ownUserMessage reports whether the row projects as the user's own message:
// a user row that is not a wake-up notice. It is the row the transcript's
// window always reaches back to (transcriptWindowStart in the renderer).
func ownUserMessage(event chat.LedgerEvent) bool {
	return event.Role == "user" && projectNotice(event) == nil
}

// lastExchangeStart is the index of the newest projected message that is the
// user's own (ownUserMessage), or 0 when none is.
func lastExchangeStart(messages []any) int {
	for index := len(messages) - 1; index >= 0; index-- {
		message := mapFromAnyMain(messages[index])
		if fieldString(message, "role") != "user" {
			continue
		}
		if _, notice := message["notice"]; !notice {
			return index
		}
	}
	return 0
}

// projectNotice is the quiet transcript line for a Workass wake-up notice:
// what finished, how, and its result folded (mock
// desktop/docs/mocks/subsessions/completion-notice.html, approved 2026-10-16).
// Rows from earlier builds carry no notice fields but have the deterministic
// subagent-completion operation id; their legacy text names the agent and
// outcome, so history draws them the same quiet way.
func projectNotice(event chat.LedgerEvent) map[string]any {
	if event.Role != "user" {
		return nil
	}
	kind, title, status := event.NoticeKind, event.NoticeTitle, event.NoticeStatus
	if kind == "" {
		if !strings.HasPrefix(string(event.OperationID), "subagent-completion:") ||
			!strings.HasPrefix(event.Text, "Internal agent completion notice.") {
			return nil
		}
		kind = "subagent"
		title = legacyNoticeField(event.Text, "Agent: ")
		status = legacyNoticeField(event.Text, "Outcome: ")
	}
	return providercontract.NoticeView(kind, title, status, event.Text)
}

func legacyNoticeField(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}
