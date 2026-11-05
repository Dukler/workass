package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"workass/internal/acp"
	"workass/internal/changes"
	"workass/internal/chat"
	"workass/internal/wire"
)

// The activity pane's «Cambios» tab (internal/changes). Four additive
// channels and one event:
//
//	changes:watch   [{id?, chatId?}] → {id, state}  start, or renew, a watch of one chat's repos
//	changes:unwatch [{id}]           → {ok}
//	changes:focus   []               → {ok}         the window regained focus
//	changes:diff    [{root, path}]   → Diff         one file, on hover/expand
//	changes:state   (event)          → State
//
// A watch lives as long as the socket that asked for it (or its lease, renewed
// by repeating changes:watch with the id), so a client that disconnects or
// hides stops git without saying so.

// changesEditRows bounds how far back one chat's stored tool events are read:
// the newest rows only, and none older than the attribution window.
const changesEditRows = 400

func registerChangesHandlers(hub *wire.Hub, manager *acp.Manager, runtime *providerChatRuntime) (*changes.Service, int) {
	source := newChangesSource(runtime)
	service := changes.New(changes.Options{
		Runner:  acp.RunGitReadOnly,
		Source:  source.activity,
		Publish: func(state changes.State) { hub.Broadcast("changes:state", state) },
		Redact:  acp.RedactSensitiveText,
	})
	if manager != nil {
		manager.SetToolEditObserver(func(edit acp.ToolEdit) {
			// Every agent edit reaches here from the provider event path. With
			// nobody watching this is one atomic load.
			if !service.Watched() {
				return
			}
			status := strings.ToLower(strings.TrimSpace(edit.Status))
			if status != "completed" && status != "success" {
				return
			}
			service.NoteChatEdit(edit.ChatID, changes.EditPaths(edit.Location, edit.Input, edit.CWD))
		})
	}
	hub.RegisterWithCaller("changes:watch", func(caller wire.Caller, args []any) (any, error) {
		if caller.IsPeer() {
			return nil, errors.New("changes:watch is for a client that shows the tab")
		}
		arg := firstMapArg(args)
		id, state := service.WatchChat(fieldString(arg, "id"), fieldString(arg, "chatId"), caller.Done)
		reply := map[string]any{"id": id}
		if state != nil {
			reply["state"] = state
		}
		return reply, nil
	})
	hub.Register("changes:unwatch", func(args []any) (any, error) {
		service.Unwatch(fieldString(firstMapArg(args), "id"))
		return map[string]any{"ok": true}, nil
	})
	hub.Register("changes:focus", func(args []any) (any, error) {
		service.Focus()
		return map[string]any{"ok": true}, nil
	})
	// A diff runs git, so it runs beside every other lane: a slow repo never
	// holds up a send or a transcript page from the same client.
	hub.RegisterConcurrentRead("changes:diff", func(args []any) (any, error) {
		arg := firstMapArg(args)
		ctx, cancel := context.WithTimeout(context.Background(), 2*changes.DefaultCallTimeout)
		defer cancel()
		return service.Diff(ctx, fieldString(arg, "root"), fieldString(arg, "path"))
	})
	return service, 4
}

// changesSource reads the chats' stored tool events for attribution. A chat's
// edits are read again only when its revision moved.
type changesSource struct {
	runtime *providerChatRuntime
	mu      sync.Mutex
	cache   map[string]changesCacheEntry
}

type changesCacheEntry struct {
	revision uint64
	since    int64
	edits    []changes.Edit
}

func newChangesSource(runtime *providerChatRuntime) *changesSource {
	return &changesSource{runtime: runtime, cache: map[string]changesCacheEntry{}}
}

// activity lists the chats active since `since` — their folder, title,
// provider — with the files their agents' edit tool calls named in that time.
// It reads stored state only; a chat that is not in memory is read from its
// rows without being opened.
func (s *changesSource) activity(since time.Time) []changes.ChatActivity {
	r := s.runtime
	if r == nil {
		return nil
	}
	ids, err := r.knownChatIDs()
	if err != nil {
		return nil
	}
	sinceMS := since.UnixMilli()
	out := make([]changes.ChatActivity, 0, 16)
	seen := make(map[string]struct{}, len(ids))
	for _, chatID := range ids {
		actor, summary := r.residentOrClosedNoOpen(chatID)
		var facts chat.PageFacts
		switch {
		case actor != nil:
			facts = actor.engine.PageFacts()
		case summary != nil:
			facts = summary.PageFacts()
		default:
			continue
		}
		if facts.Deleted || !facts.Initialized || facts.LastActivityAt < sinceMS {
			continue
		}
		seen[chatID] = struct{}{}
		out = append(out, changes.ChatActivity{
			ChatID: facts.ChatID, TabID: facts.TabID, Title: facts.Title, ProviderID: facts.ProviderID,
			CWD: facts.CWD, LastActivityAt: facts.LastActivityAt,
			Edits: s.edits(actor, summary, chatID, facts.CWD, sinceMS),
		})
	}
	s.mu.Lock()
	for chatID := range s.cache {
		if _, ok := seen[chatID]; !ok {
			delete(s.cache, chatID)
		}
	}
	s.mu.Unlock()
	return out
}

func (s *changesSource) edits(actor *providerChatActor, summary *chat.ActorSummary, chatID, cwd string, sinceMS int64) []changes.Edit {
	var revision uint64
	if actor != nil {
		actor.engine.View(func(state chat.State) { revision = state.Revision })
	} else {
		revision = summary.Revision
	}
	s.mu.Lock()
	cached, ok := s.cache[chatID]
	s.mu.Unlock()
	if ok && cached.revision == revision && cached.since <= sinceMS {
		return pruneEdits(cached.edits, sinceMS)
	}
	var edits []changes.Edit
	visit := func(event *chat.LedgerEvent) bool {
		rowAt := ledgerRowMillis(event.At)
		if rowAt > 0 && rowAt < sinceMS {
			return false
		}
		for _, entry := range event.Timeline {
			tool := entry.Tool
			if tool == nil || !changes.IsEditTool(tool.ToolKind, tool.Status) {
				continue
			}
			at := tool.EndedAtUnixMS
			if at <= 0 {
				at = tool.StartedAtUnixMS
			}
			if at <= 0 {
				at = rowAt
			}
			if at > 0 && at < sinceMS {
				continue
			}
			for _, path := range changes.EditPaths(tool.Location, tool.Input, cwd) {
				edits = append(edits, changes.Edit{Path: path, At: at})
			}
		}
		return true
	}
	if actor != nil {
		actor.engine.View(func(state chat.State) {
			revision = state.Revision
			for index, rows := len(state.Ledger)-1, 0; index >= 0 && rows < changesEditRows; index, rows = index-1, rows+1 {
				if !visit(&state.Ledger[index]) {
					break
				}
			}
		})
	} else if s.runtime.ledgers != nil {
		view := closedView(*summary)
		window, err := s.runtime.ledgers.LedgerWindowBefore(chatID, view, 0, "", changesEditRows)
		if err == nil {
			err = s.runtime.ledgers.WalkLedgerBackward(chatID, view, window, func(event chat.LedgerEvent) (bool, error) {
				return visit(&event), nil
			})
		}
		if err != nil {
			// The actor changed under the read, or its rows predate the
			// current format: its edits are read on a later pass.
			return nil
		}
	}
	s.mu.Lock()
	s.cache[chatID] = changesCacheEntry{revision: revision, since: sinceMS, edits: edits}
	s.mu.Unlock()
	return edits
}

func pruneEdits(edits []changes.Edit, sinceMS int64) []changes.Edit {
	out := edits[:0:0]
	for _, edit := range edits {
		if edit.At == 0 || edit.At >= sinceMS {
			out = append(out, edit)
		}
	}
	return out
}

func ledgerRowMillis(raw string) int64 {
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return at.UnixMilli()
}

// residentOrClosedNoOpen is residentOrClosed for a reader that must never pay
// for opening an actor: a chat that is neither in memory nor summarized is
// skipped.
func (r *providerChatRuntime) residentOrClosedNoOpen(chatID string) (*providerChatActor, *chat.ActorSummary) {
	chatID = strings.TrimSpace(chatID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bootErr != nil || r.closed {
		return nil, nil
	}
	if actor := r.actors[chatID]; actor != nil {
		return actor, nil
	}
	if summary, ok := r.summaries[chatID]; ok {
		return nil, &summary
	}
	return nil, nil
}
