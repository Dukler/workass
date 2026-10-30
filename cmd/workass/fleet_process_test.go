package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetclient"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
	"workass/internal/procchild"
)

// P4's reporting, tested where its guarantees live: an outcome appended at a
// home, a notice projected at a watcher, and the one thing the watcher must
// never do — turn "I do not know" into an outcome.
//
// The local and remote paths are deliberately the same path here, which is the
// claim §2 makes: the local test differs from the remote one only in which
// machine the watcher's id names.

const testProcessLocal = "xwproc1234567"

func processExitPayload(outcome, label string, exitCode int, summary string) map[string]any {
	receipt := acp.SpawnedWorkReceipt{
		ReceiptID: "spawned-" + testProcessLocal, TaskID: testProcessLocal,
		TabID: testAwayTabID, ChatID: testAwayChatID, Kind: "external", Label: label,
		Status: "exited", StartedAt: "2026-10-25T09:00:00Z", FinishedAt: "2026-10-25T09:04:00Z",
		ElapsedMs: 240000, OutputFile: "/tmp/xwproc1234567.output", Summary: summary,
		Outcome: outcome,
	}
	code := exitCode
	receipt.ExitCode = &code
	payload := map[string]any{
		"outcome": outcome, "label": label, "entity": "process",
		"processReceipt": spawnedWorkReceiptPayload(receipt), "exitCode": exitCode,
	}
	return payload
}

// processTarget re-homes the pair's monitor onto a process entity, so the
// existing home/watcher pair can carry a process child without a second
// fixture — which is itself the point: one log, one monitor, one stream.
func processTarget(t *testing.T, pair *fleetTestPair, label string) string {
	t.Helper()
	watcherID, err := fleetid.New(testAwayMachine, fleetid.Chat, testAwayChatID)
	if err != nil {
		t.Fatal(err)
	}
	target := pair.home.EntityID(fleetid.Process, testProcessLocal)
	row, err := pair.home.register(testAwayMachine, fleetlog.Registration{
		WatcherID: watcherID.String(), TargetID: target, Label: label,
		WatcherChatID: testAwayChatID, WatcherTabID: testAwayTabID,
		OriginLaneID: "lane-1", OriginOperationID: "parent-op-1",
	})
	if err != nil {
		t.Fatalf("the home would not hold the process monitor: %v", err)
	}
	if _, err := pair.watcher.Record(row); err != nil {
		t.Fatalf("the watcher would not record the process monitor: %v", err)
	}
	return target
}

func TestAProcessChildOutcomeReachesTheChatThatAskedWithItsOwnWord(t *testing.T) {
	for _, testCase := range []struct {
		outcome string
		status  string
		says    string
		absent  string
	}{
		{outcome: fleetlog.OutcomeSettled, status: "done", says: "finished"},
		{outcome: fleetlog.OutcomeFailed, status: "failed", says: "failed"},
		{outcome: fleetlog.OutcomeTimedOut, status: "timed_out", says: "max_runtime", absent: "finished"},
		{outcome: fleetlog.OutcomeLost, status: "lost", says: "unknown, not finished", absent: "failed"},
	} {
		t.Run(testCase.outcome, func(t *testing.T) {
			pair := newFleetTestPair(t)
			pair.setLink(true)
			target := processTarget(t, pair, "training run")
			if _, err := pair.home.Append(target, fleetlog.KindExit,
				processExitPayload(testCase.outcome, "training run", 1, "the supervisor's own summary")); err != nil {
				t.Fatal(err)
			}
			notices := pair.notices()
			if len(notices) != 1 {
				t.Fatalf("notices = %d, want exactly one", len(notices))
			}
			notice := notices[0]
			if notice.ProcessReceipt == nil || notice.Outcome != testCase.outcome {
				t.Fatalf("the notice must carry the process receipt and the outcome, got %+v", notice)
			}
			if notice.ChatID != testAwayChatID || notice.TabID != testAwayTabID {
				// The chat that ASKED, not the chat on the machine that ran it.
				t.Fatalf("the notice went to %s/%s, want the chat that asked", notice.TabID, notice.ChatID)
			}
			if got := processNoticeStatus(notice.Outcome); got != testCase.status {
				t.Fatalf("notice status = %q, want %q", got, testCase.status)
			}
			body := formatFleetProcessExit(notice)
			if !strings.Contains(body, "<task-notification>") || !strings.Contains(body, "</task-notification>") {
				t.Fatalf("a notice is a task notification, never the user speaking: %q", body)
			}
			if !strings.Contains(body, testCase.says) {
				t.Fatalf("the notice body must say %q, got %q", testCase.says, body)
			}
			if testCase.absent != "" && strings.Contains(body, testCase.absent) {
				// A lost job did not fail, and a timed-out one did not finish.
				// Saying either would be inventing the one fact nobody has.
				t.Fatalf("the notice body must not say %q, got %q", testCase.absent, body)
			}
			if !strings.Contains(body, "<output-file>") {
				// Output is a file read by a bounded tail, never copied into a
				// log, so the notice names where to look.
				t.Fatalf("the notice must name the output file, got %q", body)
			}
			for _, imperative := range []string{"Resume", "You should", "Please", "language"} {
				if strings.Contains(body, imperative) {
					t.Fatalf("a notice states what happened and nothing else; found %q in %q", imperative, body)
				}
			}
		})
	}
}

func TestACancelledProcessChildNeverNotifiesTheChatThatCancelledIt(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	target := processTarget(t, pair, "training run")
	payload := processExitPayload(fleetlog.OutcomeCancelled, "training run", 130, "Stopped on request after 9s")
	payload["noticeSuppressed"] = true
	payload["suppressReason"] = "the owner cancelled this job"
	if _, err := pair.home.Append(target, fleetlog.KindExit, payload); err != nil {
		t.Fatal(err)
	}
	if notices := pair.notices(); len(notices) != 0 {
		t.Fatalf("a cancelled job must not notify the party that asked for the cancel, got %+v", notices)
	}
	// The event is still history: suppression is about the notice, never about
	// the log.
	slice, err := pair.home.logs.Since(target, 0, 0)
	if err != nil || len(slice.Events) != 1 || slice.Events[0].Kind != fleetlog.KindExit {
		t.Fatalf("the cancel must still be logged, got %+v / %v", slice.Events, err)
	}
}

func TestADeclaredStallNotifiesOnceAndNeverReadsLikeAnEnding(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	target := processTarget(t, pair, "training run")
	if _, err := pair.home.Append(target, fleetlog.KindAttention, map[string]any{
		"entity": "process", "attentionKind": "stalled", "label": "training run",
		"title": "no output for 41m0s, past the declared 40m0s (still running)",
	}); err != nil {
		t.Fatal(err)
	}
	notices := pair.notices()
	if len(notices) != 1 || notices[0].AttentionKind != "stalled" {
		t.Fatalf("notices = %+v, want one stalled attention", notices)
	}
	body := formatFleetProcessAttention(notices[0])
	if !strings.Contains(body, "still running") {
		// A notice that read like an ending would have somebody clean up after
		// work that is still going.
		t.Fatalf("a stall notice must say the job is still running, got %q", body)
	}
	if strings.Contains(body, "finished") || strings.Contains(body, "failed") {
		t.Fatalf("a stall is not an outcome, got %q", body)
	}
	if processNoticeStatus("") == fleetNoticeStatusStalled {
		t.Fatal("stalled must not be reachable as an outcome status")
	}
}

func TestAMaxHoldAttentionNamesTheHolderAndNeverKillsIt(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	target := processTarget(t, pair, "training run")
	if _, err := pair.home.Append(target, fleetlog.KindAttention, map[string]any{
		"entity": "process", "attentionKind": "max_hold", "label": "training run",
		"resource": "gpu0", "title": "still holding gpu0 after 9h0m0s, past the 8h0m0s it declared",
	}); err != nil {
		t.Fatal(err)
	}
	notices := pair.notices()
	if len(notices) != 1 || notices[0].AttentionKind != "max_hold" {
		t.Fatalf("notices = %+v, want one max_hold attention", notices)
	}
	body := formatFleetProcessAttention(notices[0])
	if !strings.Contains(body, "has not been stopped") {
		// §16.7: a host never kills a holder to be fair. The notice has to say
		// so, or a reader will assume the job was taken away from them.
		t.Fatalf("a max_hold notice must say the job was not stopped, got %q", body)
	}
}

func TestAProcessChildNoticeIsNeverDeliveredTwiceForOneEvent(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	target := processTarget(t, pair, "training run")
	payload := processExitPayload(fleetlog.OutcomeSettled, "training run", 0, "Finished with exit 0 after 4m0s")
	if _, err := pair.home.Append(target, fleetlog.KindExit, payload); err != nil {
		t.Fatal(err)
	}
	// A reconnect re-states the cursor and the home answers from it. The event
	// is already applied, so dedup by seq makes the second pass free.
	if err := pair.watcher.Resync(context.Background(), testHomeMachine); err != nil {
		t.Fatal(err)
	}
	if err := pair.watcher.Resync(context.Background(), testHomeMachine); err != nil {
		t.Fatal(err)
	}
	if notices := pair.notices(); len(notices) != 1 {
		t.Fatalf("notices = %d, want exactly one for one event", len(notices))
	}
	first := pair.notices()[0]
	operation := fleetNoticeOperationID("process-exit", first.TabID, first.ChatID, first.ProcessReceipt.ReceiptID)
	again := fleetNoticeOperationID("process-exit", first.TabID, first.ChatID, first.ProcessReceipt.ReceiptID)
	if operation != again || operation == "" {
		// The admission key derives from durable facts only, so a redelivery
		// after a crash produces the same key and therefore no second row.
		t.Fatalf("the notice operation id must be derived and stable, got %q and %q", operation, again)
	}
}

func TestAProcessChildOnADarkMachineIsUnknownAndNeverAnOutcome(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	processTarget(t, pair, "training run")
	pair.setLink(false)
	down := fleetclient.State{Peer: testHomeMachine, Status: fleetclient.StatusDown, Since: pair.now()}
	pair.advance(11 * time.Minute)
	pair.watcher.OnLinkState(down)
	notices := pair.notices()
	if len(notices) == 0 {
		t.Fatal("a machine silent past the threshold must produce one notice")
	}
	for _, notice := range notices {
		if notice.Kind != fleetMonitorHomeDown {
			t.Fatalf("a dark machine produces home_down and nothing else, got %q", notice.Kind)
		}
		if notice.Outcome != "" || notice.ProcessReceipt != nil {
			// home_down means UNKNOWN, not dead. A watcher that attached an
			// outcome to it would be converting it into one.
			t.Fatalf("home_down must carry no outcome and no receipt, got %+v", notice)
		}
		if status := fleetNoticeStatusHomeDown; status == "done" || status == "failed" {
			t.Fatalf("home_down must not be rendered as an outcome word, got %q", status)
		}
	}
}

func TestAMonitorOfferIsAcceptedOnlyFromTheMachineThatHomesTheJob(t *testing.T) {
	pair := newFleetTestPair(t)
	target := pair.home.EntityID(fleetid.Process, testProcessLocal)
	watcherID, err := fleetid.New(testAwayMachine, fleetid.Chat, testAwayChatID)
	if err != nil {
		t.Fatal(err)
	}
	offer := map[string]any{
		"targetId": target, "watcherId": watcherID.String(), "label": "training run",
		"watcherChatId": testAwayChatID, "watcherTabId": testAwayTabID,
	}
	// A third machine must not be able to make this daemon watch a job it does
	// not home: a peer that could would be reading another machine's work
	// through a monitor it registered for it.
	if _, err := pair.watcher.RegisterOffer("m-somebody-else", offer); err == nil {
		t.Fatal("an offer from a machine that does not home the target must be refused")
	}
	// Nor may a peer name a watcher on a third machine.
	foreign, err := fleetid.New("m-third", fleetid.Chat, "someone-elses-chat")
	if err != nil {
		t.Fatal(err)
	}
	foreignOffer := map[string]any{"targetId": target, "watcherId": foreign.String()}
	if _, err := pair.watcher.RegisterOffer(testHomeMachine, foreignOffer); err == nil {
		t.Fatal("an offer naming a watcher on another machine must be refused")
	}
	accepted, err := pair.watcher.RegisterOffer(testHomeMachine, offer)
	if err != nil {
		t.Fatalf("the machine that homes the job must be able to offer the monitor: %v", err)
	}
	if ref, _ := mapFromAnyMain(accepted)["ref"].(string); ref == "" {
		t.Fatalf("an accepted offer must return its monitor ref, got %+v", accepted)
	}
	// And a chat that is not here is a leak the home cannot detect, so this
	// side refuses it.
	gone := newFleetTestPair(t, func(options *fleetWatcherOptions) {
		options.ChatExists = func(string, string) bool { return false }
	})
	if _, err := gone.watcher.RegisterOffer(testHomeMachine, map[string]any{
		"targetId":  gone.home.EntityID(fleetid.Process, testProcessLocal),
		"watcherId": watcherID.String(), "watcherChatId": "a-chat-that-is-gone",
	}); err == nil {
		t.Fatal("an offer for a chat that no longer exists must be refused")
	}
}

func TestASelfHomedOwnerIsDeliveredThroughTheSameProjection(t *testing.T) {
	pair := newFleetTestPair(t)
	pair.setLink(true)
	// The watcher's machine IS the home. Nothing about the path changes except
	// which machine the watcher's id names, and that is the whole claim of §2.
	selfHomed := newFleetTestPair(t, func(options *fleetWatcherOptions) {
		options.Self = testHomeMachine
	})
	watcherID, err := fleetid.New(testHomeMachine, fleetid.Chat, "local-chat-1")
	if err != nil {
		t.Fatal(err)
	}
	target := selfHomed.home.EntityID(fleetid.Process, testProcessLocal)
	row, err := selfHomed.home.register(testHomeMachine, fleetlog.Registration{
		WatcherID: watcherID.String(), TargetID: target, Label: "local job",
		WatcherChatID: "local-chat-1", WatcherTabID: "local-tab-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selfHomed.watcher.Record(row); err != nil {
		t.Fatal(err)
	}
	selfHomed.home.SetLocalDelivery(selfHomed.watcher.IngestLocalEvent)
	before := selfHomed.link.totalCalls()
	if _, err := selfHomed.home.Append(target, fleetlog.KindExit,
		processExitPayload(fleetlog.OutcomeSettled, "local job", 0, "Finished with exit 0 after 1s")); err != nil {
		t.Fatal(err)
	}
	notices := selfHomed.notices()
	if len(notices) != 1 || notices[0].ChatID != "local-chat-1" {
		t.Fatalf("a self-homed owner must be delivered the same notice, got %+v", notices)
	}
	if selfHomed.link.totalCalls() != before {
		// A self-homed monitor has no link to acknowledge over: invoking a peer
		// named "me" would be a call to nobody, every time a local job settled.
		t.Fatalf("a local delivery must make no link calls, got %d", selfHomed.link.totalCalls()-before)
	}
	// The home retires the event itself, which is what lets a terminal event
	// leave the log.
	selfHomed.home.AckLocal(target, 1)
	if acked, ok := selfHomed.home.monitors.Get(row.Ref); !ok || acked.AckedSeq != 1 {
		t.Fatalf("a local delivery must still be acknowledged, got %+v", acked)
	}
}

func TestADeclaredOwnerNamesTheChatThatIsOwedTheOutcome(t *testing.T) {
	// No owner: the calling chat, on this machine.
	owner, err := resolveProcessOwner("", testHomeMachine, "chat-1", "tab-1", "lane-1", "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if owner.Machine != testHomeMachine || owner.ChatID != "chat-1" || owner.TabID != "tab-1" {
		t.Fatalf("an absent owner must mean the calling chat, got %+v", owner)
	}
	// A chat on another machine: the whole point of P4, a job on the box with
	// the GPU reporting into the chat that asked.
	remote, err := resolveProcessOwner("wa:m-7f3ad2/chat/app-chat-1792187130561", testHomeMachine, "chat-1", "tab-1", "lane-1", "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if remote.Machine != "m-7f3ad2" || remote.ChatID != "app-chat-1792187130561" {
		t.Fatalf("a declared owner must name its own machine and chat, got %+v", remote)
	}
	if remote.TabID != "" {
		// A chat on another machine has no tab id this machine could know.
		t.Fatalf("a remote owner must carry no local tab id, got %q", remote.TabID)
	}
	if remote.LaneID != "lane-1" {
		t.Fatalf("the origin stamp stays this chat's own, got %q", remote.LaneID)
	}
	// A legacy machine-tagged id still resolves, per the identifier law.
	legacy, err := resolveProcessOwner("M~m-7f3ad2~app-chat-1792187130561", testHomeMachine, "chat-1", "tab-1", "", "")
	if err != nil || legacy.Machine != "m-7f3ad2" || legacy.ChatID != "app-chat-1792187130561" {
		t.Fatalf("a legacy machine-tagged owner must still resolve, got %+v / %v", legacy, err)
	}
	for _, bad := range []string{"just-a-chat-id-with-no-home", "wa:m-7f3ad2/subagent/wa-subagent-1"} {
		if _, err := resolveProcessOwner(bad, testHomeMachine, "chat-1", "tab-1", "", ""); err == nil {
			t.Fatalf("owner %q must be refused rather than guessed at", bad)
		}
	}
}

func TestTheProcessEntityIdContainsTodaysWorkId(t *testing.T) {
	pair := newFleetTestPair(t)
	target := pair.home.EntityID(fleetid.Process, testProcessLocal)
	id, err := fleetid.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	if id.Type != fleetid.Process || id.Local != testProcessLocal || id.Home != testHomeMachine {
		// The identifier law: a global id CONTAINS today's id, it never
		// replaces it.
		t.Fatalf("the global id must contain the work id unchanged, got %+v", id)
	}
	if !procchild.ValidID(id.Local) {
		t.Fatalf("the local part must still be a work id the registry accepts, got %q", id.Local)
	}
}
