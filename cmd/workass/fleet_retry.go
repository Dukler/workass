package main

import (
	"strings"
	"time"

	"workass/internal/fleetlog"
)

// A notice the chat refused is still owed.
//
// The monitor's cursor stays on the refused event and its log entry stays held,
// so owing it costs nothing durable and survives a restart. All that is missing
// is somebody asking again: a remote home's next sync would only come with a
// beat, and a job on this machine has no link and no beat at all. The chat's
// admission is keyed by the event's own identity, so an attempt that did get in
// and only looked refused cannot become a second notice.

const (
	fleetDeliveryRetryBase = 5 * time.Second
	fleetDeliveryRetryMax  = 5 * time.Minute
)

// deliveryRefused schedules the next attempt for one home. One timer per home,
// because one read from that home's log covers every monitor it owes.
func (w *fleetWatcher) deliveryRefused(peerID, ref string) {
	peerID = strings.TrimSpace(peerID)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refused[ref]++
	if w.retriesClosed || w.retryTimers[peerID] != nil {
		return
	}
	delay := w.retryBase << min(w.refused[ref]-1, 16)
	if delay > fleetDeliveryRetryMax || delay <= 0 {
		delay = fleetDeliveryRetryMax
	}
	w.logf("[fleet] the notice for monitor %s stays owed; attempt %d comes in %s", ref, w.refused[ref]+1, delay)
	w.retryTimers[peerID] = time.AfterFunc(delay, func() { w.redeliver(peerID) })
}

func (w *fleetWatcher) deliveryAdmitted(ref string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.refused, ref)
}

func (w *fleetWatcher) isRefused(ref string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refused[ref] > 0
}

func (w *fleetWatcher) redeliver(peerID string) {
	w.mu.Lock()
	delete(w.retryTimers, peerID)
	closed := w.retriesClosed
	synthesized := make(map[string]fleetlog.Event, len(w.synthesized))
	for ref, event := range w.synthesized {
		synthesized[ref] = event
	}
	w.mu.Unlock()
	if closed {
		return
	}
	for ref, event := range synthesized {
		if row, ok := w.monitors.Get(ref); ok && row.TargetHome() == peerID {
			w.applySynthesized(peerID, row, event)
		}
	}
	if peerID == w.self {
		w.drainLocal()
		return
	}
	// A link that is down fails this and owes nothing new: its return syncs anyway.
	w.catchUp(peerID)
}

// applySynthesized projects an end the home could not log because it no longer
// has the child, so no cursor can stand in for it. Held in memory until admitted.
func (w *fleetWatcher) applySynthesized(peerID string, row fleetlog.Registration, event fleetlog.Event) {
	if err := w.project(row, event); err != nil {
		w.mu.Lock()
		w.synthesized[row.Ref] = event
		w.mu.Unlock()
		w.deliveryRefused(peerID, row.Ref)
		return
	}
	w.mu.Lock()
	delete(w.synthesized, row.Ref)
	w.mu.Unlock()
	w.deliveryAdmitted(row.Ref)
	_, _ = w.monitors.Update(row.Ref, func(entry *fleetlog.Registration) { entry.Consumed = true })
}

// stopRetries ends the timers with the daemon. What they would have offered is
// still owed, and the next boot offers it.
func (w *fleetWatcher) stopRetries() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.retriesClosed = true
	for peerID, timer := range w.retryTimers {
		timer.Stop()
		delete(w.retryTimers, peerID)
	}
}
