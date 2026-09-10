package chat

import "workass/internal/provider"

// An immutable routing hint for the last committed actor state. Stop searches
// multiple actors; acquiring each actor's persistence mutex makes unrelated
// history writes part of its critical path. The owning actor still validates
// and commits cancellation under its normal serialization boundary.
type cancelRoutingSnapshot struct {
	foregroundOperation provider.OperationID
	foregroundNativeID  string
	queue               []provider.OperationID
	jobs                map[string]struct{}
}

// Caller owns e.mu (or is constructing the engine). Publish only after storage
// succeeds. Streaming content and effect claims do not change owners, so they
// reuse the same routing snapshot without allocating or hashing job IDs.
func (e *Engine) installCommittedState(next State) {
	e.state = next
	var operation provider.OperationID
	var nativeID string
	if next.Foreground != nil {
		operation = next.Foreground.OperationID
		nativeID = next.Foreground.Turn.NativeID
	}
	previous := e.cancelRouting.Load()
	if previous != nil && previous.foregroundOperation == operation && previous.foregroundNativeID == nativeID && len(previous.queue) == len(next.Queue) {
		unchanged := true
		for i, queued := range next.Queue {
			if previous.queue[i] != queued.OperationID {
				unchanged = false
				break
			}
		}
		if unchanged {
			return
		}
	}
	routing := &cancelRoutingSnapshot{
		foregroundOperation: operation, foregroundNativeID: nativeID,
		queue: make([]provider.OperationID, len(next.Queue)), jobs: make(map[string]struct{}, len(next.Queue)+2),
	}
	if operation != "" {
		routing.jobs[provider.DeriveJobID(next.ChatID, operation)] = struct{}{}
	}
	if nativeID != "" {
		routing.jobs[nativeID] = struct{}{}
	}
	for i, queued := range next.Queue {
		routing.queue[i] = queued.OperationID
		routing.jobs[provider.DeriveJobID(next.ChatID, queued.OperationID)] = struct{}{}
	}
	e.cancelRouting.Store(routing)
}
