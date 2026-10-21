package chat

import (
	"reflect"
	"sort"
	"strings"
	"time"

	"workass/internal/provider"
)

// ActorSummary is what the daemon's read surfaces need from a chat actor that
// is not resident: its identity and tab attachment, the bounded read
// projection (presentation, lanes, usage, queue; no ledger rows), the ledger's
// length and newest identity, the session timestamp, and the background and
// permission activity. It carries no ledger bodies, tool history, receipts,
// coverage or outbox, so a daemon can hold one for every chat it knows
// without decoding a single transcript.
//
// A summary is only ever built for a quiescent actor (Quiescent): nothing in
// it can change until the actor is opened again, and opening it makes the
// engine the authority again. It has no write path.
type ActorSummary struct {
	ChatID      string
	TabID       string
	Initialized bool
	Deleted     bool
	Revision    uint64
	// LedgerCount is the committed ledger length.
	LedgerCount int
	// LastMessageID is the newest ledger row's stable message id.
	LastMessageID string
	// LastActivityAt is the newest parseable ledger timestamp among the last
	// SessionActivityWindow rows (unix ms; 0 when none): exactly what
	// ReadSessionProjectionSnapshot reports for a metadata-only read.
	LastActivityAt int64

	projection  State
	activity    ActivitySnapshot
	connections map[string]struct{}
	// liveRows are the sequences of rows whose own status is pending or
	// running, when liveRowsKnown (a summary of a resident actor).
	liveRows      []int
	liveRowsKnown bool
}

// LiveRows returns the sequences of ledger rows whose own status is pending or
// running, when the summary knows them (it was taken from a resident actor).
func (s ActorSummary) LiveRows() ([]int, bool) {
	return s.liveRows, s.liveRowsKnown
}

// SessionActivityWindow is how many trailing ledger rows the session
// timestamp inspects (the session projection's message tail).
const SessionActivityWindow = 60

// LedgerStamp is the part of one ledger row a summary reads: its identity and
// its timestamp.
type LedgerStamp struct {
	MessageID string
	At        string
}

// SummarizeHead builds the summary of an actor from its checkpoint head (the
// State without ledger rows) and the stamps of its trailing ledger rows,
// oldest first, at most SessionActivityWindow of them, ending with the newest
// row.
func SummarizeHead(head State, ledgerCount int, tail []LedgerStamp) ActorSummary {
	summary := summarizeState(&head, ledgerCount)
	if len(tail) > 0 {
		summary.LastMessageID = tail[len(tail)-1].MessageID
	}
	start := max(0, len(tail)-min(ledgerCount, SessionActivityWindow))
	for index := len(tail) - 1; index >= start; index-- {
		if at, ok := ledgerStampTime(tail[index].At); ok {
			summary.LastActivityAt = at
			break
		}
	}
	return summary
}

// summarizeResident builds the summary of a resident actor's state.
func summarizeResident(state *State) ActorSummary {
	count := len(state.Ledger)
	summary := summarizeState(state, count)
	if count > 0 {
		summary.LastMessageID = state.Ledger[count-1].MessageID
	}
	for index := count - 1; index >= count-min(count, SessionActivityWindow); index-- {
		if at, ok := ledgerStampTime(state.Ledger[index].At); ok {
			summary.LastActivityAt = at
			break
		}
	}
	summary.liveRowsKnown = true
	for index := range state.Ledger {
		if status := state.Ledger[index].Status; status == "pending" || status == "running" {
			summary.liveRows = append(summary.liveRows, index+1)
		}
	}
	return summary
}

func summarizeState(state *State, ledgerCount int) ActorSummary {
	summary := ActorSummary{
		ChatID: state.ChatID, TabID: state.Presentation.TabID, Initialized: state.Initialized,
		Deleted: state.Deleted, Revision: state.Revision, LedgerCount: ledgerCount,
		projection: state.cloneReadProjection(len(state.Ledger)),
		activity:   activitySnapshotOf(state),
	}
	summary.projection.Ledger = []LedgerEvent{}
	for _, lane := range state.Lanes {
		if lane.Attachment != nil {
			summary.addConnection(lane.Attachment.ConnectionID)
		}
	}
	for _, entry := range state.Outbox {
		if entry.Kind == EffectDetachLane {
			summary.addConnection(entry.ConnectionID)
		}
	}
	return summary
}

func (s *ActorSummary) addConnection(id string) {
	if id = strings.TrimSpace(id); id == "" {
		return
	}
	if s.connections == nil {
		s.connections = make(map[string]struct{})
	}
	s.connections[id] = struct{}{}
}

func ledgerStampTime(raw string) (int64, bool) {
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return at.UnixMilli(), true
}

// NamesConnection reports whether a lane attachment or a detach receipt of the
// actor names this provider connection id. It is the candidate filter for
// surfaces that locate an actor by session id; the actor itself still decides.
func (s ActorSummary) NamesConnection(id string) bool {
	_, ok := s.connections[strings.TrimSpace(id)]
	return ok
}

// ProjectionSnapshot is the summary's ReadProjectionSnapshot: rows are the
// ledger rows [LedgerCount-len(rows), LedgerCount) read from the store, or
// none for a metadata-only read, which then carries LastActivityAt.
func (s ActorSummary) ProjectionSnapshot(rows []LedgerEvent) ReadProjectionSnapshot {
	state := s.projection.cloneReadProjection(0)
	state.Ledger = cloneLedgerEvents(rows)
	snapshot := ReadProjectionSnapshot{State: state, LedgerOffset: s.LedgerCount - len(rows), LedgerCount: s.LedgerCount}
	if len(rows) == 0 {
		snapshot.LastActivityAt = s.LastActivityAt
	}
	return snapshot
}

// DigestSnapshot is the summary's Engine.DigestSnapshot.
func (s ActorSummary) DigestSnapshot() DigestSnapshot {
	view := s.projection.PresentationState()
	return digestSnapshotOf(&view, s.LedgerCount, s.LastMessageID)
}

// ActivitySnapshot is the summary's Engine.ReadActivitySnapshot.
func (s ActorSummary) ActivitySnapshot() ActivitySnapshot {
	return s.activity.clone()
}

// IdentitySnapshot is the summary's Engine.IdentitySnapshot.
func (s ActorSummary) IdentitySnapshot() IdentitySnapshot {
	return IdentitySnapshot{ChatID: s.ChatID, TabID: s.TabID, Deleted: s.Deleted}
}

func (a ActivitySnapshot) clone() ActivitySnapshot {
	out := ActivitySnapshot{ChatID: a.ChatID, TabID: a.TabID, Deleted: a.Deleted, Obligation: a.Obligation.Clone()}
	if a.Background != nil {
		out.Background = make(map[string]BackgroundState, len(a.Background))
		for id, value := range a.Background {
			out.Background[id] = cloneBackgroundState(value)
		}
	}
	if a.Permissions != nil {
		out.Permissions = make(map[string]PermissionState, len(a.Permissions))
		for id, value := range a.Permissions {
			value.Event = *clonePermission(&value.Event)
			out.Permissions[id] = value
		}
	}
	if a.Lanes != nil {
		out.Lanes = make(map[provider.LaneID]ActivityLaneSnapshot, len(a.Lanes))
		for id, lane := range a.Lanes {
			out.Lanes[id] = lane
		}
	}
	return out
}

func cloneBackgroundState(value BackgroundState) BackgroundState {
	if value.Event.PID != nil {
		pid := *value.Event.PID
		value.Event.PID = &pid
	}
	if value.Event.ExitCode != nil {
		code := *value.Event.ExitCode
		value.Event.ExitCode = &code
	}
	return value
}

func activitySnapshotOf(state *State) ActivitySnapshot {
	snapshot := ActivitySnapshot{
		ChatID: state.ChatID, TabID: state.Presentation.TabID, Deleted: state.Deleted,
		Obligation: state.Obligation.Clone(),
	}
	if len(state.Background) > 0 {
		snapshot.Background = make(map[string]BackgroundState, len(state.Background))
		for id, value := range state.Background {
			snapshot.Background[id] = cloneBackgroundState(value)
		}
	}
	if len(state.Permissions) > 0 {
		snapshot.Permissions = make(map[string]PermissionState, len(state.Permissions))
		for id, value := range state.Permissions {
			value.Event = *clonePermission(&value.Event)
			snapshot.Permissions[id] = value
		}
	}
	if len(state.Lanes) > 0 {
		snapshot.Lanes = make(map[provider.LaneID]ActivityLaneSnapshot, len(state.Lanes))
		for id, lane := range state.Lanes {
			snapshot.Lanes[id] = ActivityLaneSnapshot{ProviderID: lane.Identity.Realm.ProviderID, SessionID: lane.Thread.HeadID}
		}
	}
	return snapshot
}

// digestSnapshotOf is DigestSnapshot over a presentation view. ledgerCount and
// lastMessageID describe the view's committed ledger, which a summary's view
// does not carry; a live foreground (never present in a summary) still walks
// view.Ledger.
func digestSnapshotOf(state *State, ledgerCount int, lastMessageID string) DigestSnapshot {
	digest := DigestSnapshot{
		Initialized:            state.Initialized,
		Deleted:                state.Deleted,
		ChatID:                 state.ChatID,
		TabID:                  state.Presentation.TabID,
		ActorRevision:          state.Revision,
		PresentationRevision:   state.Presentation.PresentationRevision,
		AgentQueueRevision:     state.Presentation.AgentQueueRevision,
		RuntimeControlRevision: state.Presentation.RuntimeControlRevision,
		ProviderID:             string(state.Presentation.ProviderID),
		CurrentModelID:         state.Presentation.CurrentModelID,
		CurrentModeID:          state.Presentation.CurrentModeID,
		QueueLen:               len(state.StagedQueue) + len(state.Queue),
	}
	if len(state.StagedQueue) > 0 {
		digest.QueueHeadID = state.StagedQueue[0].ID
	} else if len(state.Queue) > 0 {
		digest.QueueHeadID = strings.TrimSpace(state.Queue[0].Presentation.QueueID)
		if digest.QueueHeadID == "" {
			digest.QueueHeadID = string(state.Queue[0].OperationID)
		}
	}

	if foreground := state.Foreground; foreground == nil {
		// The actor ledger owns one stable visible message per event. An idle
		// digest therefore needs only its slice header and final identity; no
		// body scan or history-sized id map is necessary every five seconds.
		digest.MessageCount = ledgerCount
		if digest.MessageCount > 0 {
			digest.LastMessageID = lastMessageID
		}
	} else {
		// A live foreground can contribute up to four not-yet-ledger rows. Keep
		// the existing stable-id merge for that short-lived state so renderer
		// counts remain exact across consumption and steering boundaries.
		seen := make(map[string]struct{}, len(state.Ledger)+4)
		addMessage := func(id string) {
			if _, exists := seen[id]; exists {
				return
			}
			seen[id] = struct{}{}
			digest.MessageCount++
			digest.LastMessageID = id
		}
		for _, event := range state.Ledger {
			addMessage(event.MessageID)
		}
		digest.RunningJobID = foreground.Turn.NativeID
		if !foreground.UserConsumed {
			userID := strings.TrimSpace(foreground.Input.Presentation.UserMessageID)
			if userID == "" {
				userID = "message:" + string(foreground.OperationID) + ":user"
			}
			addMessage(userID)
		}
		addMessage(strings.TrimSpace(foreground.CurrentAssistantMessageID))
		for pending := state.PendingSteer; pending != nil; pending = pending.Next {
			userID := strings.TrimSpace(pending.Presentation.UserMessageID)
			if userID == "" {
				userID = "message:" + string(pending.OperationID) + ":user"
			}
			addMessage(userID)
			continuationID := strings.TrimSpace(pending.Presentation.AssistantMessageID)
			if continuationID == "" {
				continuationID = foreground.RootAssistantMessageID + "~after~" + string(pending.OperationID)
			}
			addMessage(continuationID)
		}
	}
	for id, permission := range state.Permissions {
		if !strings.EqualFold(strings.TrimSpace(permission.Event.Status), "resolved") {
			digest.PendingPermissionIDs = append(digest.PendingPermissionIDs, id)
		}
	}
	sort.Strings(digest.PendingPermissionIDs)
	return digest
}

// Quiescent reports whether an actor may be left closed: nothing it holds
// needs the daemon until someone opens it again. Boot opens every other
// actor (restart recovery must settle it), and the daemon never retires one
// from memory unless this holds. The reason names the first obstacle.
//
// It reads no ledger rows, so it judges a checkpoint head as well as a
// resident state:
//   - no foreground turn, pending steer or cancel, and no queued input;
//   - every outbox entry is terminal (completed, failed, ambiguous): pending
//     work would run as soon as the coordinator woke;
//   - no running background work and no unresolved permission;
//   - no open obligation (working or parked), which boot and the periodic
//     reconciliation settle;
//   - restart recovery (RecoverOutbox) would change nothing, so opening the
//     actor later is exactly what opening it now would have been.
func Quiescent(state State) (bool, string) {
	switch {
	case !state.Initialized:
		return false, "uninitialized"
	case state.Foreground != nil:
		return false, "foreground turn"
	case state.PendingSteer != nil:
		return false, "pending steer"
	case state.PendingCancel != nil:
		return false, "pending cancel"
	case len(state.Queue) > 0 || len(state.StagedQueue) > 0:
		return false, "queued input"
	}
	for index := range state.Outbox {
		switch state.Outbox[index].Status {
		case OutboxCompleted, OutboxFailed, OutboxAmbiguous:
		default:
			return false, "outbox work"
		}
	}
	for _, item := range state.Background {
		if status := strings.TrimSpace(item.Event.Status); status == "" || strings.EqualFold(status, "running") {
			return false, "running background work"
		}
	}
	for _, permission := range state.Permissions {
		if !strings.EqualFold(strings.TrimSpace(permission.Event.Status), "resolved") {
			return false, "pending permission"
		}
	}
	if obligation := state.Obligation; obligation != nil && (obligation.State == "working" || obligation.State == "parked") {
		return false, "open obligation"
	}
	if !recoveryIsNoop(state) {
		return false, "restart recovery"
	}
	return true, ""
}

// recoveryIsNoop reports whether RecoverOutbox would leave the state exactly
// as it is (apart from the revision it always advances). It runs the reducer
// on a probe holding copies of the only collections recovery edits when no
// foreground turn exists. A state with a foreground turn is never a no-op.
func recoveryIsNoop(state State) bool {
	if state.Foreground != nil || state.PendingSteer != nil {
		return false
	}
	probe := State{
		ChatID: state.ChatID, Deleted: state.Deleted, Queue: state.Queue,
		Outbox:      append([]OutboxEntry(nil), state.Outbox...),
		Lanes:       make(map[provider.LaneID]LaneState, len(state.Lanes)),
		Permissions: make(map[string]PermissionState, len(state.Permissions)),
	}
	for id, lane := range state.Lanes {
		probe.Lanes[id] = lane
	}
	for id, permission := range state.Permissions {
		probe.Permissions[id] = permission
	}
	effects, err := reduceRecoverOutbox(&probe)
	if err != nil || len(effects) != 0 || probe.Foreground != nil || probe.PendingSteer != nil {
		return false
	}
	if len(probe.Outbox) != len(state.Outbox) || len(probe.Lanes) != len(state.Lanes) || len(probe.Permissions) != len(state.Permissions) {
		return false
	}
	for index := range state.Outbox {
		if !reflect.DeepEqual(probe.Outbox[index], state.Outbox[index]) {
			return false
		}
	}
	for id, lane := range state.Lanes {
		if !reflect.DeepEqual(probe.Lanes[id], lane) {
			return false
		}
	}
	for id, permission := range state.Permissions {
		if !reflect.DeepEqual(probe.Permissions[id], permission) {
			return false
		}
	}
	return true
}

// CreationReceipt is the part of an actor that opening or re-creating it
// checks: whether it is initialized or a tombstone, and its immutable
// creation receipt.
type CreationReceipt struct {
	Initialized bool
	Deleted     bool
	OperationID provider.OperationID
	Digest      string
}

// CreationReceipt reads the receipt without copying the actor.
func (e *Engine) CreationReceipt() CreationReceipt {
	e.mu.Lock()
	defer e.mu.Unlock()
	return CreationReceipt{
		Initialized: e.state.Initialized, Deleted: e.state.Deleted,
		OperationID: e.state.CreationOperationID, Digest: e.state.CreationDigest,
	}
}
