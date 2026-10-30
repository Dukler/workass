package main

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetclient"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
)

// The watcher half of P3: one ordered stream per watcher, and the one event
// nobody else can synthesize.
//
// docs/specs/fleet-runtime.md §7. A watcher receives, on one ordered stream per
// monitor: exit{outcome}, attention, waiting, resumed, and opt-in progress. And
// then the event this file exists for:
//
//	home_down{home, since, last_known}
//
// It is synthesized by the WATCHER'S OWN daemon from the link heartbeat, never
// sent by the home — a machine that is dark cannot tell you it is dark. This is
// Erlang's {'DOWN', ref, _, _, noconnection}: it means UNKNOWN, NOT DEAD. A
// watcher must never convert it into an outcome, which is the single most
// important rule in this file and the one with its own regression test. Before
// this, a child on a machine that went dark stayed `running` forever, which was
// a claim the daemon had no evidence for.
//
// What arrives here is a replica, not an assertion: every event is the same
// record the home can be re-asked for at any time, identified by a seq in that
// entity's total order. So dedup is one comparison, redelivery is free, and a
// four-day partition is a postponement rather than a loss.

// fleetHomeDownThreshold is §16.3a's decided number: a machine silent for ten
// minutes produces one home_down notice per host, listing its affected
// children. The heartbeat (≈1 min) is derived from it, not a separate knob.
const fleetHomeDownThreshold = 10 * time.Minute

// fleetMonitorKind is the vocabulary a projection switches on. It is the log's
// vocabulary plus the two states only the watcher can know.
const (
	fleetMonitorHomeDown = "home_down"
	fleetMonitorHomeUp   = "home_up"
)

// fleetMonitorInvoker is the sliver of the mesh this file needs. Taking an
// interface keeps the watcher testable without a socket and keeps it from
// reaching into link management it has no business in.
type fleetMonitorInvoker interface {
	Invoke(ctx context.Context, peerID, channel string, args ...any) (any, error)
	States() []fleetclient.State
}

// fleetMonitorNotice is one actionable event, ready for projection into a chat.
type fleetMonitorNotice struct {
	Kind     string
	TargetID string
	ChatID   string
	TabID    string
	LaneID   string
	// OriginOperationID binds the notice to the parent turn that asked, so the
	// "parent was stopped" suppression can be applied at the chat.
	OriginOperationID string
	Label             string
	// Receipt is set for an exit, and is the exact record the local path uses.
	Receipt acp.SubagentReceipt
	// ProcessReceipt and Outcome are set for a PROCESS child's exit (P4). The
	// two receipt types are kept apart rather than merged because each is the
	// exact record its own projection already reads, and a merged one would be
	// a third shape that neither surface renders today.
	ProcessReceipt *acp.SpawnedWorkReceipt
	Outcome        string
	// Attention is set for an attention event.
	AttentionKind string
	AttentionText string
	// Home, Since and LastKnown describe a home_down/home_up. LastKnown is what
	// the watcher last saw, never an outcome.
	Home      string
	Since     time.Time
	LastKnown []string
}

// fleetWatcher holds this daemon's monitors on other machines' entities.
type fleetWatcher struct {
	hostedState  func(peerID string, row fleetlog.Registration, fields map[string]any)
	settleHosted func(row fleetlog.Registration, event fleetlog.Event)
	hostedRows   func(peerID string) []acp.SpawnedWorkItem
	applyMu      sync.Mutex
	resyncMu     sync.Mutex
	catchingUp   map[string]bool
	lastResync   map[string]time.Time
	self         string
	monitors     *fleetlog.Monitors
	mesh         fleetMonitorInvoker
	deliver      func(fleetMonitorNotice) error
	budget       *fleetNoticeBudget
	// chatExists answers whether a watcher entity is still here. The home cannot
	// know this, which is why dropping a monitor whose watcher is gone is the
	// watcher's own job.
	chatExists func(tabID, chatID string) bool
	// chatTab resolves this machine's own tab for a chat an offer named
	// without one.
	chatTab   func(chatID string) string
	threshold time.Duration
	now       func() time.Time
	logf      func(string, ...any)

	mu sync.Mutex
	// darkSince latches the home_down notice per home, so a machine that has
	// been dark for a day produces one notice and not one per heartbeat.
	darkSince map[string]time.Time
	announced map[string]bool
}

type fleetWatcherOptions struct {
	Self       string
	Monitors   *fleetlog.Monitors
	Mesh       fleetMonitorInvoker
	Deliver    func(fleetMonitorNotice) error
	Budget     *fleetNoticeBudget
	ChatExists func(tabID, chatID string) bool
	// ChatTab answers which tab a chat on THIS machine lives in.
	//
	// It exists because a job's owner may be declared as a global chat id —
	// `workass run --owner wa:m-away/chat/app-chat-1` — and the machine that
	// runs the job has no way to know that chat's tab. The notice admission
	// fence compares the tab exactly and silently drops a mismatch, so an
	// offer with no tab produced a monitor that recorded every event and
	// delivered none of them. This machine knows its own chats; it fills the
	// gap rather than inheriting it.
	ChatTab func(chatID string) string
	// HostedState and SettleHosted keep the row in the asking chat true to what
	// the machine running the work says (fleet_hosted_work.go).
	HostedState  func(peerID string, row fleetlog.Registration, fields map[string]any)
	SettleHosted func(row fleetlog.Registration, event fleetlog.Event)
	// HostedRows is what this daemon shows as running on one other machine.
	HostedRows func(peerID string) []acp.SpawnedWorkItem
	Threshold  time.Duration
	Now        func() time.Time
	Logf       func(string, ...any)
}

func newFleetWatcher(opts fleetWatcherOptions) *fleetWatcher {
	watcher := &fleetWatcher{
		self: strings.TrimSpace(opts.Self), monitors: opts.Monitors, mesh: opts.Mesh,
		deliver: opts.Deliver, budget: opts.Budget, chatExists: opts.ChatExists, chatTab: opts.ChatTab,
		threshold: opts.Threshold, now: opts.Now, logf: opts.Logf,
		hostedState: opts.HostedState, settleHosted: opts.SettleHosted, hostedRows: opts.HostedRows,
		darkSince: map[string]time.Time{}, announced: map[string]bool{},
	}
	if watcher.threshold <= 0 {
		watcher.threshold = fleetHomeDownThreshold
	}
	if watcher.now == nil {
		watcher.now = time.Now
	}
	if watcher.logf == nil {
		watcher.logf = func(string, ...any) {}
	}
	return watcher
}

func (w *fleetWatcher) ready() bool {
	return w != nil && w.monitors != nil && strings.TrimSpace(w.self) != ""
}

// Record is the watcher's durable half of `monitor` (§7: durable at BOTH ends).
// The watcher's home records it so the monitor survives this daemon's restart;
// the target's home records it so it knows who is owed. Neither can be inferred
// from the other.
func (w *fleetWatcher) Record(row fleetlog.Registration) (fleetlog.Registration, error) {
	if !w.ready() {
		return fleetlog.Registration{}, errors.New("this daemon holds no fleet monitors")
	}
	return w.monitors.Add(row)
}

// WatchRemoteSubagent records the monitor for a subagent this daemon's chat
// asked another machine to run, from the `monitor` block that machine returned
// with the spawn. Then it applies whatever already happened: the child may well
// have finished while the reply was in flight, and the log does not care which
// order those two events reached this machine in.
func (w *fleetWatcher) WatchRemoteSubagent(ctx context.Context, peer string, result any, tabID, chatID, laneID, operationID string) {
	if !w.ready() {
		return
	}
	monitor := mapFromAnyMain(mapFromAnyMain(result)["monitor"])
	targetID := strings.TrimSpace(fieldString(monitor, "targetId"))
	if targetID == "" {
		return
	}
	watcher, err := fleetid.New(w.self, fleetid.Chat, strings.TrimSpace(chatID))
	if err != nil {
		return
	}
	row, err := w.Record(fleetlog.Registration{
		WatcherID: watcher.String(), TargetID: targetID,
		Label:         fieldString(monitor, "label"),
		WatcherChatID: strings.TrimSpace(chatID), WatcherTabID: strings.TrimSpace(tabID),
		OriginLaneID: strings.TrimSpace(laneID), OriginOperationID: strings.TrimSpace(operationID),
	})
	if err != nil {
		w.logf("[fleet] could not record a monitor on %s: %v", targetID, err)
		return
	}
	// The child may have ended before this monitor existed here, and a push that
	// beat this line was dropped for having no monitor to land on. A child that
	// fails in its first moments does exactly that, so the log is asked once more.
	w.catchUp(peer)
	if slice := mapFromAnyMain(monitor["slice"]); len(slice) > 0 {
		w.applySlice(ctx, peer, row.Ref, slice)
	}
}

// Ingest is the mesh's OnEvent, narrowed to the one channel this file owns. An
// event that arrives for a monitor this daemon does not hold is DROPPED, not
// adopted: a monitor carries a capability naming the watcher, so an unsolicited
// push is exactly what must not become a notice (§11).
func (w *fleetWatcher) Ingest(peerID, channel string, payload any) {
	if channel == fleetStateChannel {
		w.ingestState(peerID, mapFromAnyMain(payload))
		return
	}
	if !w.ready() || channel != fleetLogEventChannel {
		return
	}
	fields := mapFromAnyMain(payload)
	targetID := strings.TrimSpace(fieldString(fields, "targetId"))
	if targetID == "" {
		return
	}
	row, ok := w.monitorFor(peerID, targetID)
	if !ok {
		return
	}
	w.applySlice(context.Background(), peerID, row.Ref, fields)
}

func (w *fleetWatcher) monitorFor(peerID, targetID string) (fleetlog.Registration, bool) {
	for _, row := range w.monitors.ByTarget(strings.TrimSpace(targetID)) {
		if row.TargetHome() == strings.TrimSpace(peerID) {
			return row, true
		}
	}
	return fleetlog.Registration{}, false
}

// applySlice applies one batch in order, deduplicating by seq, then
// acknowledges exactly what it applied.
//
// The order is the whole guarantee: apply, then advance the cursor, then ack.
// A crash between apply and ack re-receives and deduplicates (the cursor moved,
// so the projection's own operation id makes the second admission a no-op); a
// crash between receive and apply re-receives and applies. What cannot happen
// is an ack for something that was never applied, which is the only way an
// event is lost.
func (w *fleetWatcher) applySlice(ctx context.Context, peerID, ref string, slice map[string]any) {
	// One at a time. A push and a sync, or two syncs, can carry the same seq, and
	// the dedup below is only true if nothing else is between its read of the
	// cursor and its write. Found live: one exit delivered twice.
	w.applyMu.Lock()
	defer w.applyMu.Unlock()
	row, ok := w.monitors.Get(ref)
	if !ok {
		return
	}
	events := anySlice(slice["events"])
	truncatedTo := uint64(intField(slice, "truncatedTo"))
	if boolFieldValue(slice, "truncated") {
		// Below the watermark: the events between the cursor and first_seq are
		// gone. Resync from the head and SAY SO, rather than failing or
		// pretending the gap was empty (§5).
		w.logf("[fleet] %s compacted the log for %s past this daemon's cursor %d (to_seq %d); resyncing from the head",
			peerID, row.TargetID, row.AppliedSeq, truncatedTo)
		if truncatedTo > row.AppliedSeq {
			row, _ = w.monitors.Update(ref, func(entry *fleetlog.Registration) { entry.AppliedSeq = truncatedTo })
		}
	}
	applied := row.AppliedSeq
	ordered := make([]fleetlog.Event, 0, len(events))
	for _, raw := range events {
		if event, ok := decodeFleetEvent(raw); ok {
			ordered = append(ordered, event)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
	for _, event := range ordered {
		if event.Seq <= applied {
			// Already applied. Dedup by seq is this comparison and nothing more.
			continue
		}
		if event.Seq > applied+1 {
			// A gap means this push overtook something. Nothing is inferred from
			// a gap: the sync below asks the home from the cursor, in order.
			w.logf("[fleet] %s pushed seq %d for %s with this daemon at %d; asking for the gap",
				peerID, event.Seq, row.TargetID, applied)
			break
		}
		w.project(row, event)
		applied = event.Seq
		if _, ok := w.monitors.Update(ref, func(entry *fleetlog.Registration) {
			entry.AppliedSeq = applied
			if fleetlog.Terminal(event.Kind) {
				entry.Consumed = true
			}
		}); !ok {
			return
		}
	}
	if applied > row.AppliedSeq {
		w.ack(ctx, peerID, row.TargetID, applied)
	}
}

// ack tells the home what has been consumed, which is what finally lets a
// terminal event leave its log. A failed ack is harmless: the event stays owed
// and is redelivered, and the projection deduplicates it.
func (w *fleetWatcher) ack(ctx context.Context, peerID, targetID string, through uint64) {
	if strings.TrimSpace(peerID) == w.self {
		// A self-homed monitor has no link to acknowledge over: the home is in
		// this process, and it retires the event through fleetHome.AckLocal
		// after this returns. Invoking a peer named "me" would be a call to
		// nobody, every time a local job settled.
		return
	}
	if w.mesh == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := w.mesh.Invoke(callCtx, peerID, fleetLogAckChannel, map[string]any{
		"target_id": targetID, "through_seq": through,
	}); err != nil {
		w.logf("[fleet] could not acknowledge %s through seq %d: %v", targetID, through, err)
	}
}

// Resync is the one round trip on link up: every cursor this daemon holds for
// that home, answered with the backlog from each. Nothing polls — this runs
// because the link came up, not because a timer fired.
//
// It is also where the registry is reconciled in both directions: a monitor the
// home holds for a watcher that no longer exists here is demonitored, so a
// dropped watcher leaks no registration (§7).
func (w *fleetWatcher) Resync(ctx context.Context, peerID string) error {
	if !w.ready() || w.mesh == nil {
		return nil
	}
	w.resyncMu.Lock()
	defer w.resyncMu.Unlock()
	peerID = strings.TrimSpace(peerID)
	rows := w.monitors.ByTargetHome(peerID)
	cursors := make([]any, 0, len(rows))
	for _, row := range rows {
		cursors = append(cursors, map[string]any{"target_id": row.TargetID, "after_seq": row.AppliedSeq})
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	answer, err := w.mesh.Invoke(callCtx, peerID, fleetMonitorSyncChannel, map[string]any{
		"watcher_machine": w.self, "cursors": cursors,
	})
	if err != nil {
		return err
	}
	fields := mapFromAnyMain(answer)
	known := map[string]fleetlog.Registration{}
	for _, row := range rows {
		known[row.TargetID] = row
	}
	for _, raw := range anySlice(fields["monitors"]) {
		entry := mapFromAnyMain(raw)
		targetID := strings.TrimSpace(fieldString(entry, "targetId"))
		watcherID := strings.TrimSpace(fieldString(entry, "watcherId"))
		if targetID == "" || watcherID == "" {
			continue
		}
		row, held := known[targetID]
		if !held {
			// A monitor the home holds and this daemon does not: the registration
			// reply was lost. Adopt it rather than leaving the home pushing into
			// nothing — the home is the one that knows what it owes.
			// The home may not know this chat's tab, and a notice with no tab is
			// dropped by the admission fence without a word. This machine knows it.
			tabID := strings.TrimSpace(fieldString(entry, "watcherTabId"))
			if tabID == "" && w.chatTab != nil {
				tabID = strings.TrimSpace(w.chatTab(fieldString(entry, "watcherChatId")))
			}
			adopted, err := w.Record(fleetlog.Registration{
				WatcherID: watcherID, TargetID: targetID, Label: fieldString(entry, "label"),
				WatcherChatID: fieldString(entry, "watcherChatId"), WatcherTabID: tabID,
				OriginLaneID: fieldString(entry, "originLaneId"), OriginOperationID: fieldString(entry, "originOperationId"),
			})
			if err != nil {
				continue
			}
			row = adopted
		}
		if w.chatExists != nil && row.WatcherChatID != "" && !w.chatExists(row.WatcherTabID, row.WatcherChatID) {
			// The watcher is gone. The home cannot know that, so this side says
			// it, and the registration is dropped at both ends.
			w.drop(callCtx, peerID, row)
			continue
		}
		known[row.TargetID] = row
	}
	for _, raw := range anySlice(fields["slices"]) {
		slice := mapFromAnyMain(raw)
		targetID := strings.TrimSpace(fieldString(slice, "targetId"))
		row, held := known[targetID]
		if !held {
			continue
		}
		w.applySlice(ctx, peerID, row.Ref, slice)
	}
	// Whatever a dropped link cost the rows, now that the monitors are settled.
	for _, raw := range anySlice(fields["states"]) {
		w.ingestState(peerID, mapFromAnyMain(raw))
	}
	w.adoptHostedRows(ctx, peerID)
	return nil
}

func (w *fleetWatcher) drop(ctx context.Context, peerID string, row fleetlog.Registration) {
	if w.mesh != nil {
		if _, err := w.mesh.Invoke(ctx, peerID, fleetDemonitorChannel, map[string]any{"ref": row.Ref}); err != nil {
			w.logf("[fleet] could not demonitor %s on %s: %v", row.TargetID, peerID, err)
			return
		}
	}
	if _, _, err := w.monitors.Remove(row.Ref); err != nil {
		w.logf("[fleet] could not forget the monitor on %s: %v", row.TargetID, err)
	}
}

// OnLinkState is the mesh's heartbeat, read as §7's two synthesized events.
//
// It is called on every beat rather than only on a change, because the
// threshold is a duration and a duration needs to be re-read. The latch is what
// keeps one notice per home: a dark machine with twenty children produces ONE
// home_down listing them, and a day of darkness produces no second one.
func (w *fleetWatcher) OnLinkState(state fleetclient.State) {
	if !w.ready() {
		return
	}
	home := strings.TrimSpace(state.Peer)
	if home == "" {
		return
	}
	if state.Status == fleetclient.StatusUp {
		w.mu.Lock()
		wasDark := w.announced[home]
		delete(w.darkSince, home)
		delete(w.announced, home)
		w.mu.Unlock()
		w.resyncIfOwed(home)
		if wasDark {
			// home_up, then the backlog from the cursor. One stream, one
			// vocabulary (§7).
			w.emit(fleetMonitorNotice{Kind: fleetMonitorHomeUp, Home: home})
		}
		return
	}
	since := state.Since
	if since.IsZero() {
		since = w.now()
	}
	w.mu.Lock()
	if previous, ok := w.darkSince[home]; ok && previous.Before(since) {
		since = previous
	}
	w.darkSince[home] = since
	announced := w.announced[home]
	w.mu.Unlock()
	if announced || w.now().Sub(since) < w.threshold {
		return
	}
	rows := w.monitors.ByTargetHome(home)
	if len(rows) == 0 {
		return
	}
	w.mu.Lock()
	w.announced[home] = true
	w.mu.Unlock()
	// One notice per home, not per child, and it names the children so the
	// chat can say `last known` rather than invent a state (§7, §16.3a).
	byChat := map[string][]fleetlog.Registration{}
	for _, row := range rows {
		if row.Consumed || row.WatcherChatID == "" {
			continue
		}
		byChat[row.WatcherTabID+"\x00"+row.WatcherChatID] = append(byChat[row.WatcherTabID+"\x00"+row.WatcherChatID], row)
	}
	for _, group := range byChat {
		lastKnown := make([]string, 0, len(group))
		for _, row := range group {
			lastKnown = append(lastKnown, firstNonEmptyString(row.Label, row.TargetID))
		}
		sort.Strings(lastKnown)
		w.emit(fleetMonitorNotice{
			Kind: fleetMonitorHomeDown, Home: home, Since: since,
			ChatID: group[0].WatcherChatID, TabID: group[0].WatcherTabID,
			LaneID: group[0].OriginLaneID, OriginOperationID: group[0].OriginOperationID,
			TargetID: group[0].TargetID, LastKnown: lastKnown,
		})
	}
}

// project turns one log event into at most one chat notice.
//
// The actionable filter (§7): settled, lost, timed_out, attention, a declared
// stall, and home_down. `spawned`, `progress`, `waiting` and `resumed` never
// notify — they are visible by reading, which is exactly when freshness
// matters. A cancelled child never notifies either: the parent asked for that.
func (w *fleetWatcher) project(row fleetlog.Registration, event fleetlog.Event) {
	// A monitor already recorded without its tab, by a build that adopted one so.
	if strings.TrimSpace(row.WatcherTabID) == "" && w.chatTab != nil {
		row.WatcherTabID = strings.TrimSpace(w.chatTab(row.WatcherChatID))
	}
	w.settleHostedRow(row, event)
	if fieldString(event.Payload, "entity") == "process" {
		w.projectProcess(row, event)
		return
	}
	switch event.Kind {
	case fleetlog.KindExit:
		if boolFieldValue(event.Payload, "noticeSuppressed") {
			// The home already applied the suppression conditions, and it named
			// which one. Re-deciding them here with less information would be a
			// weaker copy of a rule that already ran.
			w.logf("[fleet] %s settled without a notice: %s", row.TargetID, fieldString(event.Payload, "suppressReason"))
			return
		}
		receipt := subagentReceiptFromPayload(mapFromAnyMain(event.Payload["receipt"]))
		if receipt.Status != "done" && receipt.Status != "failed" {
			return
		}
		w.emit(fleetMonitorNotice{
			Kind: fleetlog.KindExit, TargetID: row.TargetID,
			ChatID: row.WatcherChatID, TabID: row.WatcherTabID,
			LaneID: row.OriginLaneID, OriginOperationID: row.OriginOperationID,
			Label: firstNonEmptyString(receipt.Label, row.Label), Receipt: receipt,
		})
	case fleetlog.KindAttention:
		w.emit(fleetMonitorNotice{
			Kind: fleetlog.KindAttention, TargetID: row.TargetID,
			ChatID: row.WatcherChatID, TabID: row.WatcherTabID,
			LaneID: row.OriginLaneID, OriginOperationID: row.OriginOperationID,
			Label:         firstNonEmptyString(fieldString(event.Payload, "label"), row.Label),
			AttentionKind: fieldString(event.Payload, "attentionKind"),
			AttentionText: fieldString(event.Payload, "title"),
		})
	}
}

func (w *fleetWatcher) emit(notice fleetMonitorNotice) {
	if w.deliver == nil {
		return
	}
	if w.budget != nil {
		if deferred, reason := w.budget.admit(notice, w.now()); !deferred {
			w.logf("[fleet] held a %s notice for %s: %s", notice.Kind, notice.ChatID, reason)
			return
		}
	}
	if err := w.deliver(notice); err != nil {
		w.logf("[fleet] could not deliver a %s notice to %s: %v", notice.Kind, notice.ChatID, err)
	}
}

func decodeFleetEvent(raw any) (fleetlog.Event, bool) {
	fields := mapFromAnyMain(raw)
	if len(fields) == 0 {
		return fleetlog.Event{}, false
	}
	seq := uint64(intField(fields, "seq"))
	kind := strings.TrimSpace(fieldString(fields, "kind"))
	if seq == 0 || kind == "" {
		return fleetlog.Event{}, false
	}
	event := fleetlog.Event{
		Entity: strings.TrimSpace(fieldString(fields, "entity")), Seq: seq, Kind: kind,
		Payload: mapFromAnyMain(fields["payload"]),
	}
	if at, err := time.Parse(time.RFC3339Nano, fieldString(fields, "at")); err == nil {
		event.At = at
	}
	return event, true
}

// projectProcess turns one process child's log event into at most one notice.
//
// The actionable filter is §7's, with this subsystem's two extra outcomes:
// settled, failed, timed_out and lost notify; a declared stall and a decision
// notify as attention; `created`, `queued`, `started` and a cancelled exit
// never do. `queued` and `started` are visible by reading, which is exactly
// when a queue position matters — somebody is already looking. A cancelled
// exit was already marked suppressed by its home, because the party that would
// be told is the party that asked.
func (w *fleetWatcher) projectProcess(row fleetlog.Registration, event fleetlog.Event) {
	switch event.Kind {
	case fleetlog.KindExit:
		if boolFieldValue(event.Payload, "noticeSuppressed") {
			w.logf("[fleet] process child %s settled without a notice: %s",
				row.TargetID, fieldString(event.Payload, "suppressReason"))
			return
		}
		outcome := strings.TrimSpace(fieldString(event.Payload, "outcome"))
		receipt := spawnedWorkReceiptFromPayload(mapFromAnyMain(event.Payload["processReceipt"]))
		w.emit(fleetMonitorNotice{
			Kind: fleetlog.KindExit, TargetID: row.TargetID,
			ChatID: row.WatcherChatID, TabID: row.WatcherTabID,
			LaneID: row.OriginLaneID, OriginOperationID: row.OriginOperationID,
			Label:          firstNonEmptyString(receipt.Label, fieldString(event.Payload, "label"), row.Label),
			ProcessReceipt: &receipt, Outcome: outcome,
		})
	case fleetlog.KindAttention:
		w.emit(fleetMonitorNotice{
			Kind: fleetlog.KindAttention, TargetID: row.TargetID,
			ChatID: row.WatcherChatID, TabID: row.WatcherTabID,
			LaneID: row.OriginLaneID, OriginOperationID: row.OriginOperationID,
			Label:         firstNonEmptyString(fieldString(event.Payload, "label"), row.Label),
			AttentionKind: fieldString(event.Payload, "attentionKind"),
			AttentionText: fieldString(event.Payload, "title"),
			Outcome:       "",
		})
	}
}
