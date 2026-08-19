package main

import (
	"sync"
	"time"
)

const sessionRefreshWindow = 50 * time.Millisecond

type refreshUrgency uint8

const (
	refreshBackground refreshUrgency = iota
	refreshImmediate
)

type sessionRefreshTarget struct {
	tabID  string
	chatID string
}

// sessionRefreshCoordinator turns exact daemon mutation intents into the
// renderer's existing global session-refresh invalidation. A global refresh is
// sufficient because session:get returns the whole mirror; retaining exact
// targets and their highest generations here proves that coalescing merges
// intents rather than silently dropping them.
type sessionRefreshCoordinator struct {
	mu        sync.Mutex
	broadcast func(string, any)
	window    time.Duration
	pending   map[sessionRefreshTarget]uint64
	timer     *time.Timer
	flushing  bool

	// flushObserver is a test-only receipt seam. Production leaves it nil.
	flushObserver func(map[sessionRefreshTarget]uint64)
}

func newSessionRefreshCoordinator(broadcast func(string, any)) *sessionRefreshCoordinator {
	if broadcast == nil {
		broadcast = func(string, any) {}
	}
	return &sessionRefreshCoordinator{
		broadcast: broadcast,
		window:    sessionRefreshWindow,
		pending:   make(map[sessionRefreshTarget]uint64),
	}
}

func (c *sessionRefreshCoordinator) Request(tabID, chatID string, generation uint64, urgency refreshUrgency) {
	if c == nil {
		return
	}
	target := sessionRefreshTarget{tabID: tabID, chatID: chatID}
	c.mu.Lock()
	if current, exists := c.pending[target]; !exists || generation > current {
		c.pending[target] = generation
	}
	if c.flushing {
		// The in-flight batch has already been detached. This mutation belongs
		// to the next fixed window even when it is user-visible, rather than
		// racing a second broadcast against the current Flush.
		c.ensureTimerLocked()
		c.mu.Unlock()
		return
	}
	if urgency == refreshImmediate {
		c.mu.Unlock()
		c.Flush()
		return
	}
	c.ensureTimerLocked()
	c.mu.Unlock()
}

// RequestFocus publishes one exact, live-only focus intent before the ordinary
// untargeted refresh. The trailing generic refresh is deliberate: browser
// bridges cache the last payload per event channel for late subscribers, so it
// replaces the one-shot focus payload and prevents a reconnect from replaying
// an old agent selection over the user's current local tab.
func (c *sessionRefreshCoordinator) RequestFocus(tabID, chatID string) {
	if c == nil {
		return
	}
	c.broadcast("agent:apply", map[string]any{
		"action": "session-refresh",
		"tabId":  tabID,
		"chatId": chatID,
		"focus":  true,
	})
	c.broadcast("agent:apply", map[string]any{"action": "session-refresh"})
}

func (c *sessionRefreshCoordinator) ensureTimerLocked() {
	if c.timer != nil {
		return
	}
	window := c.window
	if window <= 0 {
		window = sessionRefreshWindow
	}
	var timer *time.Timer
	timer = time.AfterFunc(window, func() {
		c.mu.Lock()
		if c.timer != timer {
			c.mu.Unlock()
			return
		}
		c.timer = nil
		c.mu.Unlock()
		c.Flush()
	})
	c.timer = timer
}

func (c *sessionRefreshCoordinator) Flush() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.flushing || len(c.pending) == 0 {
		c.mu.Unlock()
		return
	}
	batch := c.pending
	c.pending = make(map[sessionRefreshTarget]uint64)
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.flushing = true
	observer := c.flushObserver
	c.mu.Unlock()

	if observer != nil {
		observer(batch)
	}
	c.broadcast("agent:apply", map[string]any{"action": "session-refresh"})

	c.mu.Lock()
	c.flushing = false
	flushOverdue := len(c.pending) > 0 && c.timer == nil
	c.mu.Unlock()
	if flushOverdue {
		c.Flush()
	}
}

func (c *sessionRefreshCoordinator) stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}
