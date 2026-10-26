package chat

import "strings"

// PageFacts are the few presentation and activity facts that decide whether a
// renderer's first screen has to list a chat (session:get {page}): whether it
// is filed away (settled long ago, read, idle, owing nothing), what it is
// called and where it lives (search and the project list), and which chat it
// hangs under. Reading them copies no ledger rows, tool history, background
// records or permission bodies, so a heartbeat can afford them for every chat.
type PageFacts struct {
	ChatID      string
	TabID       string
	Initialized bool
	Deleted     bool
	Title       string
	CWD         string
	Settled     string
	SettledAt   int64
	Unread      bool
	// LastActivityAt is what a metadata-only session read reports as the
	// chat's lastActivityAt (unix ms; 0 when none).
	LastActivityAt int64
	// Live is true while anything is running, waiting on a permission, queued
	// or still owed in the background: such a chat is never filed away.
	Foreground         bool
	PendingPermissions bool
	Queued             bool
	RunningBackground  bool
	ObligationState    string
	ParentTabID        string
	ParentChatID       string
}

func pageFactsOf(state *State, lastActivityAt int64) PageFacts {
	view := state.PresentationState()
	p := view.Presentation
	facts := PageFacts{
		ChatID: view.ChatID, TabID: p.TabID, Initialized: view.Initialized, Deleted: view.Deleted,
		Title: p.Title, Settled: p.Settled, SettledAt: p.SettledAt, Unread: p.Unread,
		LastActivityAt: lastActivityAt,
		Foreground:     view.Foreground != nil,
		Queued:         len(view.StagedQueue)+len(view.Queue) > 0,
	}
	if p.CWD != nil {
		facts.CWD = *p.CWD
	}
	if view.Parent != nil {
		facts.ParentTabID, facts.ParentChatID = view.Parent.TabID, view.Parent.ChatID
	}
	for _, permission := range view.Permissions {
		if !strings.EqualFold(strings.TrimSpace(permission.Event.Status), "resolved") {
			facts.PendingPermissions = true
			break
		}
	}
	for _, item := range view.Background {
		if item.Event.Status == "running" {
			facts.RunningBackground = true
			break
		}
	}
	if view.Obligation != nil {
		facts.ObligationState = view.Obligation.State
	}
	return facts
}

// PageFacts reads a resident actor's facts under its lock.
func (e *Engine) PageFacts() PageFacts {
	e.mu.Lock()
	defer e.mu.Unlock()
	// The same timestamp window as ReadSessionProjectionSnapshot's
	// metadata-only read.
	lastActivityAt := int64(0)
	count := len(e.state.Ledger)
	for index := count - 1; index >= count-min(count, SessionActivityWindow); index-- {
		if at, ok := ledgerStampTime(e.state.Ledger[index].At); ok {
			lastActivityAt = at
			break
		}
	}
	return pageFactsOf(&e.state, lastActivityAt)
}

// PageFacts reads a closed actor's facts from its summary.
func (s ActorSummary) PageFacts() PageFacts {
	state := s.projection
	state.Background = s.activity.Background
	state.Permissions = s.activity.Permissions
	state.Obligation = s.activity.Obligation
	return pageFactsOf(&state, s.LastActivityAt)
}
