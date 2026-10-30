package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetclient"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
	"workass/internal/wire"
)

// P3, tested where its guarantees actually live: a home that appends, a watcher
// that applies from a cursor, and nothing in between but a link that may or may
// not be up.
//
// The link here is a function call rather than a socket, deliberately: the
// socket is proven end to end in fleet_monitor_e2e_test.go, and these tests are
// about the things a socket cannot show you — a four-day gap, a restart between
// append and delivery, and the fact that NOTHING POLLS.

const (
	testHomeMachine   = "m-home"
	testAwayMachine   = "m-away"
	testAwayChatID    = "away-chat-1"
	testAwayTabID     = "away-tab-1"
	testSubagentLocal = "wa-subagent-1792187130561-8"
)

// fleetTestLink is one peer's half of the link, as the watcher uses it. Every
// invoke is recorded, so "nothing polls" is asserted rather than asserted
// about: a test that expects no traffic checks that there was none.
type fleetTestLink struct {
	t    *testing.T
	home *fleetHome
	peer string

	mu     sync.Mutex
	calls  []string
	status string
	since  time.Time
	down   bool
}

func (l *fleetTestLink) Invoke(ctx context.Context, peerID, channel string, args ...any) (any, error) {
	l.mu.Lock()
	l.calls = append(l.calls, channel)
	down := l.down
	l.mu.Unlock()
	if down {
		return nil, fleetclient.ErrNotSent
	}
	params := firstMapArg(args)
	caller := wire.Caller{Peer: l.peer}
	switch channel {
	case fleetMonitorSyncChannel:
		return l.home.handleSync(caller, params)
	case fleetLogAckChannel:
		return l.home.handleAck(caller, params)
	case fleetDemonitorChannel:
		return l.home.handleDemonitor(caller, params)
	case fleetMonitorChannel:
		return l.home.handleMonitor(caller, params)
	}
	l.t.Fatalf("the watcher called %s, which is not part of the monitor surface", channel)
	return nil, nil
}

func (l *fleetTestLink) States() []fleetclient.State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return []fleetclient.State{{Peer: testHomeMachine, Status: l.status, Since: l.since}}
}

func (l *fleetTestLink) channelCalls(channel string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, call := range l.calls {
		if call == channel {
			count++
		}
	}
	return count
}

func (l *fleetTestLink) totalCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

// fleetTestPair is a home and a watcher on two separate state directories, with
// one clock between them so a four-day gap is a line of code.
type fleetTestPair struct {
	home    *fleetHome
	watcher *fleetWatcher
	link    *fleetTestLink
	target  string
	ref     string

	mu        sync.Mutex
	delivered []fleetMonitorNotice
	pushes    int
	linkUp    bool

	// The clock has its own mutex: the log store reads it while holding its own
	// lock, and sharing one mutex with the delivery slice would couple two
	// unrelated critical sections.
	clockMu sync.Mutex
	now     func() time.Time
	advance func(time.Duration)
}

func newFleetTestPair(t *testing.T, opts ...func(*fleetWatcherOptions)) *fleetTestPair {
	t.Helper()
	clock := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	pair := &fleetTestPair{}
	pair.now = func() time.Time {
		pair.clockMu.Lock()
		defer pair.clockMu.Unlock()
		return clock
	}
	pair.advance = func(step time.Duration) {
		pair.clockMu.Lock()
		clock = clock.Add(step)
		pair.clockMu.Unlock()
	}

	held, err := fleetlog.OpenMonitors(t.TempDir(), fleetlog.MonitorsHeld)
	if err != nil {
		t.Fatalf("held registry: %v", err)
	}
	logs, err := fleetlog.Open(t.TempDir(), fleetlog.Options{Now: pair.now, Logf: t.Logf, Owed: held.MinAcked})
	if err != nil {
		t.Fatalf("entity logs: %v", err)
	}
	pair.home = newFleetHome(testHomeMachine, logs, held, func(peerID, channel string, payload any) int {
		pair.mu.Lock()
		pair.pushes++
		up := pair.linkUp
		pair.mu.Unlock()
		if !up {
			// The link is down: the push reaches nothing. Nothing is retried and
			// nothing polls — the event stays owed and the cursor collects it.
			return 0
		}
		// The hub pushes TO the watcher's home; the watcher receives the event
		// FROM the sending daemon, so the id the watcher sees is the home's.
		_ = peerID
		pair.watcher.Ingest(testHomeMachine, channel, payload)
		return 1
	}, t.Logf)

	watched, err := fleetlog.OpenMonitors(t.TempDir(), fleetlog.MonitorsWatched)
	if err != nil {
		t.Fatalf("watched registry: %v", err)
	}
	pair.link = &fleetTestLink{t: t, home: pair.home, peer: testAwayMachine, status: fleetclient.StatusUp}
	options := fleetWatcherOptions{
		Self: testAwayMachine, Monitors: watched, Mesh: pair.link,
		Budget: newFleetNoticeBudget(), Now: pair.now, Logf: t.Logf,
		ChatExists: func(string, string) bool { return true },
		Deliver: func(notice fleetMonitorNotice) error {
			pair.mu.Lock()
			pair.delivered = append(pair.delivered, notice)
			pair.mu.Unlock()
			return nil
		},
	}
	for _, apply := range opts {
		apply(&options)
	}
	pair.watcher = newFleetWatcher(options)

	watcherID, err := fleetid.New(testAwayMachine, fleetid.Chat, testAwayChatID)
	if err != nil {
		t.Fatal(err)
	}
	pair.target = pair.home.EntityID(fleetid.Subagent, testSubagentLocal)
	row, err := pair.home.register(testAwayMachine, fleetlog.Registration{
		WatcherID: watcherID.String(), TargetID: pair.target, Label: "reviewer",
		WatcherChatID: testAwayChatID, WatcherTabID: testAwayTabID,
		OriginLaneID: "lane-1", OriginOperationID: "parent-op-1",
	})
	if err != nil {
		t.Fatalf("the home would not hold the monitor: %v", err)
	}
	if _, err := pair.watcher.Record(row); err != nil {
		t.Fatalf("the watcher would not record the monitor: %v", err)
	}
	pair.ref = row.Ref
	return pair
}

func (p *fleetTestPair) notices() []fleetMonitorNotice {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]fleetMonitorNotice(nil), p.delivered...)
}

func (p *fleetTestPair) setLink(up bool) {
	p.mu.Lock()
	p.linkUp = up
	p.mu.Unlock()
	p.link.mu.Lock()
	p.link.down = !up
	if up {
		p.link.status = fleetclient.StatusUp
	} else {
		p.link.status = fleetclient.StatusDown
	}
	p.link.mu.Unlock()
}

func (p *fleetTestPair) appendExit(t *testing.T, status, result string) {
	t.Helper()
	receipt := acp.SubagentReceipt{
		ReceiptID: testSubagentLocal, SubagentID: testSubagentLocal, Label: "reviewer",
		Status: status, ProviderID: "mock", Result: result,
		OriginLaneID: "lane-1", OriginOperationID: "parent-op-1",
		OriginMachine: testAwayMachine, OriginChatID: testAwayChatID, OriginTabID: testAwayTabID,
	}
	outcome := fleetlog.OutcomeSettled
	if status == "failed" {
		outcome = fleetlog.OutcomeFailed
	}
	if _, err := p.home.Append(p.target, fleetlog.KindExit, map[string]any{
		"outcome": outcome, "label": "reviewer", "receipt": subagentReceiptPayload(receipt),
	}); err != nil {
		t.Fatalf("append the exit: %v", err)
	}
}

// §13 P3: "a four-day outage delivers the backlog once, in order, with no poll
// running". This is the claim the whole phase exists for: before it, a child on
// another machine was watched by re-asking every two seconds, so a partition
// was a loss and a reconnect was a guess.
func TestAFourDayOutageDeliversTheBacklogOnceWithNoPollRunning(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(false)

	// Four days of darkness, with the work carrying on at its home.
	if _, err := pair.home.Append(pair.target, fleetlog.KindAttention, map[string]any{
		"attentionKind": "permission", "title": "Permission required: write a file", "label": "reviewer",
	}); err != nil {
		t.Fatalf("append the attention: %v", err)
	}
	pair.advance(2 * 24 * time.Hour)
	if _, err := pair.home.Append(pair.target, fleetlog.KindProgress, map[string]any{"note": "still working"}); err != nil {
		t.Fatalf("append the progress: %v", err)
	}
	pair.advance(2 * 24 * time.Hour)
	pair.appendExit(t, "done", "the deterministic mock result")

	if got := pair.link.totalCalls(); got != 0 {
		t.Fatalf("the watcher made %d call(s) during the outage; nothing may poll", got)
	}
	if len(pair.notices()) != 0 {
		t.Fatalf("a notice was delivered while the link was down: %#v", pair.notices())
	}

	// The link comes up. One round trip, and the backlog arrives in order.
	pair.setLink(true)
	if err := pair.watcher.Resync(context.Background(), testHomeMachine); err != nil {
		t.Fatalf("resync: %v", err)
	}
	notices := pair.notices()
	if len(notices) != 2 {
		t.Fatalf("the backlog produced %d notice(s): %#v", len(notices), notices)
	}
	if notices[0].Kind != fleetlog.KindAttention || notices[1].Kind != fleetlog.KindExit {
		t.Fatalf("the backlog arrived out of order: %q then %q", notices[0].Kind, notices[1].Kind)
	}
	if notices[1].Receipt.Result != "the deterministic mock result" {
		t.Fatalf("the outcome lost its result: %#v", notices[1].Receipt)
	}
	if notices[1].ChatID != testAwayChatID || notices[1].TabID != testAwayTabID {
		t.Fatalf("the outcome did not reach the chat that asked: %#v", notices[1])
	}
	// Progress is opt-in and never a wake, so it is in the log and not in the
	// chat.
	for _, notice := range notices {
		if notice.Kind == fleetlog.KindProgress {
			t.Fatalf("progress became a notice: %#v", notice)
		}
	}
	// ONE round trip for the whole backlog, plus the acknowledgement it earns.
	if syncs := pair.link.channelCalls(fleetMonitorSyncChannel); syncs != 1 {
		t.Fatalf("the backlog cost %d sync call(s)", syncs)
	}
	if acks := pair.link.channelCalls(fleetLogAckChannel); acks != 1 {
		t.Fatalf("the backlog produced %d ack(s)", acks)
	}

	// Exactly once: a second resync delivers nothing, because the cursor moved.
	if err := pair.watcher.Resync(context.Background(), testHomeMachine); err != nil {
		t.Fatalf("second resync: %v", err)
	}
	if after := pair.notices(); len(after) != 2 {
		t.Fatalf("a second resync re-delivered the backlog: %#v", after)
	}
}

// §13 P3: "duplicate push + redelivery deduped". Dedup is one comparison — the
// seq — which is why redelivery is free and why a failed ack is harmless.
func TestADuplicatePushAndARedeliveryAreDeduped(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	pair.appendExit(t, "done", "once")
	if got := len(pair.notices()); got != 1 {
		t.Fatalf("the push produced %d notice(s)", got)
	}

	// The same push again, as a reconnect's backlog racing a live push.
	slice, err := pair.home.logs.Since(pair.target, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := slicePayload(slice)
	pair.watcher.Ingest(testHomeMachine, fleetLogEventChannel, map[string]any{
		"targetId": pair.target, "head": slice.Head, "events": payload["events"],
	})
	if err := pair.watcher.Resync(context.Background(), testHomeMachine); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if got := pair.notices(); len(got) != 1 {
		t.Fatalf("a duplicate push and a redelivery produced %d notice(s): %#v", len(got), got)
	}
	// And the watcher's cursor is where the one applied event left it.
	row, ok := pair.watcher.monitors.Get(pair.ref)
	if !ok || row.AppliedSeq != 1 || !row.Consumed {
		t.Fatalf("the cursor after one delivery = %#v", row)
	}
}

// §13 P3: "home_down never becomes an outcome", and "one notice per dark home
// regardless of child count". This is Erlang's noconnection: it means unknown.
func TestHomeDownIsOneNoticePerHomeAndNeverAnOutcome(t *testing.T) {
	pair := newFleetTestPair(t)
	// Two more children on the same dark machine. Twenty would do as well; the
	// point is that the notice is per HOME.
	for _, local := range []string{"wa-subagent-2", "wa-subagent-3"} {
		target := pair.home.EntityID(fleetid.Subagent, local)
		watcherID, _ := fleetid.New(testAwayMachine, fleetid.Chat, testAwayChatID)
		row, err := pair.home.register(testAwayMachine, fleetlog.Registration{
			WatcherID: watcherID.String(), TargetID: target, Label: local,
			WatcherChatID: testAwayChatID, WatcherTabID: testAwayTabID, OriginLaneID: "lane-1",
		})
		if err != nil {
			t.Fatalf("register %s: %v", local, err)
		}
		if _, err := pair.watcher.Record(row); err != nil {
			t.Fatalf("record %s: %v", local, err)
		}
	}
	pair.setLink(false)

	down := fleetclient.State{Peer: testHomeMachine, Status: fleetclient.StatusDown, Since: pair.now()}
	// Below the threshold, silence is just silence: a machine that missed one
	// beat is not a machine that is gone.
	pair.watcher.OnLinkState(down)
	if got := pair.notices(); len(got) != 0 {
		t.Fatalf("a notice fired before the ten-minute threshold: %#v", got)
	}
	pair.advance(11 * time.Minute)
	// Several beats past the threshold produce ONE notice, not one per beat and
	// not one per child.
	for i := 0; i < 4; i++ {
		pair.watcher.OnLinkState(down)
		pair.advance(time.Minute)
	}
	notices := pair.notices()
	if len(notices) != 1 {
		t.Fatalf("a dark home with three children produced %d notice(s): %#v", len(notices), notices)
	}
	notice := notices[0]
	if notice.Kind != fleetMonitorHomeDown {
		t.Fatalf("the notice kind = %q", notice.Kind)
	}
	if notice.Home != testHomeMachine || notice.Since != down.Since {
		t.Fatalf("the notice = %#v", notice)
	}
	if len(notice.LastKnown) != 3 {
		t.Fatalf("the notice named %#v, want all three children", notice.LastKnown)
	}
	// The one rule that matters: this is not an outcome. Not in the kind, not in
	// the receipt, and not in the words the model reads.
	if notice.Receipt.Status != "" {
		t.Fatalf("home_down carried an outcome status %q", notice.Receipt.Status)
	}
	body := formatFleetHomeDown(notice)
	if !strings.Contains(body, "<status>unknown</status>") {
		t.Fatalf("home_down does not say unknown:\n%s", body)
	}
	for _, outcome := range []string{"<status>completed", "<status>failed", "<status>settled", "<result>", "<error>"} {
		if strings.Contains(body, outcome) {
			t.Fatalf("home_down reads as an outcome (%q):\n%s", outcome, body)
		}
	}
	if !strings.Contains(body, "unknown, not finished") || !strings.Contains(body, "<last-known>") {
		t.Fatalf("home_down does not state what is actually known:\n%s", body)
	}

	// Recovery is not announced as its own row: the backlog that follows it is
	// the news.
	pair.setLink(true)
	pair.watcher.OnLinkState(fleetclient.State{Peer: testHomeMachine, Status: fleetclient.StatusUp, Since: pair.now()})
	recovery := pair.notices()
	if len(recovery) != 2 || recovery[1].Kind != fleetMonitorHomeUp {
		t.Fatalf("recovery = %#v", recovery)
	}
	// Dark again, and the latch has been released, so the next outage is its own
	// notice rather than being swallowed by the first.
	pair.setLink(false)
	pair.advance(20 * time.Minute)
	pair.watcher.OnLinkState(fleetclient.State{Peer: testHomeMachine, Status: fleetclient.StatusDown, Since: pair.now().Add(-20 * time.Minute)})
	if got := pair.notices(); len(got) != 3 || got[2].Kind != fleetMonitorHomeDown {
		t.Fatalf("a second outage = %#v", got)
	}
}

// §13 P3: "a dropped watcher leaks no registration". The home cannot know a
// chat on another machine is gone, so the machine that owns it says so, and the
// registration goes at both ends.
func TestADroppedWatcherLeavesNoRegistrationAtEitherEnd(t *testing.T) {
	pair := newFleetTestPair(t, func(options *fleetWatcherOptions) {
		options.ChatExists = func(string, string) bool { return false }
	})
	pair.setLink(true)
	if err := pair.watcher.Resync(context.Background(), testHomeMachine); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if held := pair.home.monitorsHeldFor(testAwayMachine); len(held) != 0 {
		t.Fatalf("the home still owes %#v", held)
	}
	if watched := pair.watcher.monitors.All(); len(watched) != 0 {
		t.Fatalf("the watcher still holds %#v", watched)
	}
	if pair.link.channelCalls(fleetDemonitorChannel) != 1 {
		t.Fatalf("the watcher did not demonitor: %#v", pair.link.calls)
	}
	// And an event appended afterwards reaches nobody, which is the leak this
	// guards against.
	pair.appendExit(t, "done", "nobody is listening")
	if got := pair.notices(); len(got) != 0 {
		t.Fatalf("a dropped monitor still delivered: %#v", got)
	}
}

// A monitor whose registration reply was lost is adopted from the home's own
// enumeration, rather than leaving the home pushing into nothing forever.
func TestAWatcherAdoptsAMonitorWhoseRegistrationReplyWasLost(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	target := pair.home.EntityID(fleetid.Subagent, "wa-subagent-orphan")
	watcherID, _ := fleetid.New(testAwayMachine, fleetid.Chat, testAwayChatID)
	if _, err := pair.home.register(testAwayMachine, fleetlog.Registration{
		WatcherID: watcherID.String(), TargetID: target, Label: "orphan",
		WatcherChatID: testAwayChatID, WatcherTabID: testAwayTabID, OriginLaneID: "lane-1",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The reply never arrived here, so this daemon holds no record of it.
	if _, held := pair.watcher.monitorFor(testHomeMachine, target); held {
		t.Fatal("the watcher already knew about the orphan")
	}
	if _, err := pair.home.Append(target, fleetlog.KindExit, map[string]any{
		"outcome": fleetlog.OutcomeSettled, "label": "orphan",
		"receipt": subagentReceiptPayload(acp.SubagentReceipt{ReceiptID: "orphan-1", Label: "orphan", Status: "done", Result: "done anyway"}),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := pair.watcher.Resync(context.Background(), testHomeMachine); err != nil {
		t.Fatalf("resync: %v", err)
	}
	found := false
	for _, notice := range pair.notices() {
		if notice.Receipt.ReceiptID == "orphan-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an adopted monitor delivered nothing: %#v", pair.notices())
	}
}

// §11's blast radius, on the receiving side: a push for a monitor this daemon
// does not hold is dropped, never adopted. A compromised daemon can lie about
// its own entities to the watchers that asked, and nothing else.
func TestAnUnsolicitedPushNeverBecomesANotice(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.watcher.Ingest(testHomeMachine, fleetLogEventChannel, map[string]any{
		"targetId": "wa:m-home/subagent/never-asked-for", "head": 1,
		"events": []any{map[string]any{
			"entity": "wa:m-home/subagent/never-asked-for", "seq": 1, "kind": fleetlog.KindExit,
			"payload": map[string]any{"receipt": map[string]any{"receiptId": "forged", "status": "done"}},
		}},
	})
	// A push for an entity this daemon DOES monitor, but arriving from the wrong
	// peer, is equally not a notice.
	pair.watcher.Ingest("m-someone-else", fleetLogEventChannel, map[string]any{
		"targetId": pair.target, "head": 1,
		"events": []any{map[string]any{"entity": pair.target, "seq": 1, "kind": fleetlog.KindExit}},
	})
	if got := pair.notices(); len(got) != 0 {
		t.Fatalf("an unsolicited push became %#v", got)
	}
	// And a channel that is not the log channel is ignored outright.
	pair.watcher.Ingest(testHomeMachine, "fleet:link-state", map[string]any{"targetId": pair.target})
	if got := pair.notices(); len(got) != 0 {
		t.Fatalf("an unrelated channel became %#v", got)
	}
}

// The home's own authorization (§11): the peer id comes from the authenticated
// socket, never from the payload, and a monitor may only name a watcher on the
// machine that is actually asking.
func TestOnlyAPairedPeerMayHoldAMonitorAndOnlyForItsOwnWatchers(t *testing.T) {
	pair := newFleetTestPair(t)
	for _, caller := range []wire.Caller{{}, {DeviceID: "a-browser", IP: "127.0.0.1", Local: true}} {
		if _, err := pair.home.handleMonitor(caller, map[string]any{
			"watcher_id": "wa:m-away/chat/" + testAwayChatID, "target_id": pair.target,
		}); err == nil {
			t.Fatalf("a caller with no link registered a monitor: %#v", caller)
		}
		if _, err := pair.home.handleSync(caller, nil); err == nil {
			t.Fatal("a caller with no link read the monitor backlog")
		}
		if _, err := pair.home.handleAck(caller, map[string]any{"target_id": pair.target}); err == nil {
			t.Fatal("a caller with no link acknowledged events")
		}
	}
	// A linked peer naming SOMEONE ELSE's watcher is refused by name: that would
	// be reading a third machine's work through a monitor registered for it.
	if _, err := pair.home.handleMonitor(wire.Caller{Peer: "m-third"}, map[string]any{
		"watcher_id": "wa:m-away/chat/" + testAwayChatID, "target_id": pair.target,
	}); err == nil || !strings.Contains(err.Error(), "m-away") {
		t.Fatalf("a peer registered another machine's watcher: %v", err)
	}
	// An entity this machine does not home is not this machine's to monitor:
	// one writer per entity, at its home.
	if _, err := pair.home.handleMonitor(wire.Caller{Peer: testAwayMachine}, map[string]any{
		"watcher_id": "wa:m-away/chat/" + testAwayChatID, "target_id": "wa:m-elsewhere/subagent/x",
	}); err == nil || !strings.Contains(err.Error(), "not homed on this machine") {
		t.Fatalf("the home accepted a monitor on another home's entity: %v", err)
	}
	// A bare local id is not a monitor target: a monitor is about an entity, and
	// an entity is named by its home.
	if _, err := pair.home.handleMonitor(wire.Caller{Peer: testAwayMachine}, map[string]any{
		"watcher_id": testAwayChatID, "target_id": testSubagentLocal,
	}); err == nil {
		t.Fatal("the home accepted a monitor named with local ids")
	}
	// Another peer may not acknowledge this peer's events, and may not
	// demonitor its monitors.
	if _, err := pair.home.handleDemonitor(wire.Caller{Peer: "m-third"}, map[string]any{"ref": pair.ref}); err == nil {
		t.Fatal("a third machine demonitored another machine's monitor")
	}
	if result, err := pair.home.handleAck(wire.Caller{Peer: "m-third"}, map[string]any{
		"target_id": pair.target, "through_seq": 9,
	}); err != nil || intField(mapFromAnyMain(result), "acked") != 0 {
		t.Fatalf("a third machine's ack = %#v, %v", result, err)
	}
}

// A suppressed exit is still history and is never a notice: the home names
// which of §7's conditions consumed it, and the watcher does not re-decide.
func TestTheHomeSuppressionVerdictTravelsAndIsObeyed(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	if _, err := pair.home.Append(pair.target, fleetlog.KindExit, map[string]any{
		"outcome": fleetlog.OutcomeSettled, "label": "reviewer",
		"noticeSuppressed": true, "suppressReason": "a live waiter will consume this result",
		"receipt": subagentReceiptPayload(acp.SubagentReceipt{ReceiptID: "suppressed-1", Status: "done", Result: "consumed by the waiter"}),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if got := pair.notices(); len(got) != 0 {
		t.Fatalf("a suppressed exit became a notice: %#v", got)
	}
	// It is in the log, where it can be read, and the cursor advanced past it.
	slice, err := pair.home.logs.Since(pair.target, 0, 0)
	if err != nil || len(slice.Events) != 1 {
		t.Fatalf("the suppressed exit is not in the log: %#v (%v)", slice, err)
	}
	row, _ := pair.watcher.monitors.Get(pair.ref)
	if row.AppliedSeq != 1 {
		t.Fatalf("the cursor stalled on a suppressed event: %#v", row)
	}
	// A cancelled child never notifies either — the parent asked for that.
	if _, err := pair.home.Append(pair.target, fleetlog.KindExit, map[string]any{
		"outcome": fleetlog.OutcomeCancelled, "label": "reviewer",
		"receipt": subagentReceiptPayload(acp.SubagentReceipt{ReceiptID: "cancelled-1", Status: "cancelled"}),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if got := pair.notices(); len(got) != 0 {
		t.Fatalf("a cancelled child notified: %#v", got)
	}
}

// §16.3b's window, and no cap (§14h).
func TestTheNoticeBudgetCoalescesAndNeverDropsADifferentEnd(t *testing.T) {
	budget := newFleetNoticeBudget()
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	settle := func(id string) fleetMonitorNotice {
		return fleetMonitorNotice{
			Kind: fleetlog.KindExit, TargetID: "wa:m-home/subagent/" + id, ChatID: testAwayChatID, TabID: testAwayTabID,
			Receipt: acp.SubagentReceipt{ReceiptID: id, Status: "done"},
		}
	}
	// The same notice twice inside the window is one notice: that is a push
	// racing a reconnect's backlog, not two results.
	if ok, _ := budget.admit(settle("one"), now); !ok {
		t.Fatal("the first notice was held")
	}
	if ok, reason := budget.admit(settle("one"), now.Add(30*time.Second)); ok {
		t.Fatal("the same notice was admitted twice inside the window")
	} else if !strings.Contains(reason, "window") {
		t.Fatalf("the hold reason = %q", reason)
	}
	// A DIFFERENT result in the same window still arrives: a result is content,
	// and dropping one to save a line would lose the thing the notice carries.
	if ok, reason := budget.admit(settle("two"), now.Add(31*time.Second)); !ok {
		t.Fatalf("a second distinct result was held: %s", reason)
	}
	// Past the window, the same notice may be delivered again — a redelivery
	// after a lost ack is not a storm.
	if ok, _ := budget.admit(settle("one"), now.Add(2*time.Minute)); !ok {
		t.Fatal("a redelivery past the window was held")
	}

	// No cap (§14h): every distinct end arrives, however many a chat has in an
	// hour. The seventh is the one that was lost live.
	busy := newFleetNoticeBudget()
	for i := 0; i < 40; i++ {
		if ok, reason := busy.admit(settle(fmt.Sprintf("end-%d", i)), now.Add(time.Duration(i)*time.Minute)); !ok {
			t.Fatalf("end %d was held: %s", i, reason)
		}
	}
	// Failures, attentions and home_down go through it the same way.
	failure := settle("failed-one")
	failure.Receipt.Status = "failed"
	if ok, reason := busy.admit(failure, now.Add(41*time.Minute)); !ok {
		t.Fatalf("a failure was held: %s", reason)
	}
	attention := fleetMonitorNotice{Kind: fleetlog.KindAttention, TargetID: "wa:m-home/subagent/x", ChatID: testAwayChatID, TabID: testAwayTabID, AttentionText: "decide"}
	if ok, reason := busy.admit(attention, now.Add(42*time.Minute)); !ok {
		t.Fatalf("an attention was held: %s", reason)
	}
	dark := fleetMonitorNotice{Kind: fleetMonitorHomeDown, Home: testHomeMachine, ChatID: testAwayChatID, TabID: testAwayTabID}
	if ok, reason := busy.admit(dark, now.Add(43*time.Minute)); !ok {
		t.Fatalf("home_down was held: %s", reason)
	}
	// The window is per chat: the same event in another chat is another notice.
	other := settle("end-39")
	other.ChatID = "another-chat"
	if ok, reason := busy.admit(other, now.Add(39*time.Minute)); !ok {
		t.Fatalf("one chat's window held another's notice: %s", reason)
	}
	var absent *fleetNoticeBudget
	if ok, _ := absent.admit(settle("x"), now); !ok {
		t.Fatal("a nil budget held a notice")
	}
}

// The live loss: a seventh successful end in an hour settled its row, moved the
// cursor, was acknowledged, and reached nobody.
func TestEverySuccessfulEndInAnHourBecomesItsOwnNotice(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("end-%d", i)
		if _, err := pair.home.Append(pair.target, fleetlog.KindExit, map[string]any{
			"outcome": fleetlog.OutcomeSettled, "label": "reviewer",
			"receipt": subagentReceiptPayload(acp.SubagentReceipt{ReceiptID: id, Status: "done", Result: "ok"}),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if got := pair.notices(); len(got) != 9 {
		t.Fatalf("%d ends produced %d notices: %#v", 9, len(got), got)
	}
}

// A push that overtakes the stream is not applied out of order and nothing is
// inferred from the gap: the next sync asks from the cursor.
func TestAPushThatOvertakesTheStreamWaitsForTheGap(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(false)
	if _, err := pair.home.Append(pair.target, fleetlog.KindAttention, map[string]any{"title": "first"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	pair.setLink(true)
	// seq 2 arrives while this daemon is still at 0.
	pair.appendExit(t, "done", "second")
	if got := pair.notices(); len(got) != 0 {
		t.Fatalf("an out-of-order push was applied: %#v", got)
	}
	if err := pair.watcher.Resync(context.Background(), testHomeMachine); err != nil {
		t.Fatalf("resync: %v", err)
	}
	notices := pair.notices()
	if len(notices) != 2 || notices[0].Kind != fleetlog.KindAttention || notices[1].Kind != fleetlog.KindExit {
		t.Fatalf("the gap was not filled in order: %#v", notices)
	}
}
