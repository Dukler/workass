package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"workass/internal/chat"
	"workass/internal/fleetlog"
	providercontract "workass/internal/provider"
)

// A monitor event, projected into the chat that asked.
//
// This is the end of the path §1 named as the split: "children on another
// machine report into THAT machine's chat, never to the chat that asked". The
// chat that asked is an entity id now (§3), its daemon holds a monitor on the
// child (§7), and the child's outcome arrives here — on the machine that asked,
// in the chat that asked, with the app closed on both ends.
//
// Every rule the local delivery path follows is followed here, because it IS
// the local delivery path: a settle reuses `deliverSubagentCompletion`
// unchanged, so the admitted row, the notice card and the receipt are
// byte-identical to the ones a subagent on this machine produces. The two
// events that have no local equivalent — `attention` from another machine and
// the `home_down` only this daemon can synthesize — are admitted through the
// same actor fences, with the same presentation shape and the same
// task-notification body, so the model reads them the way it reads every other
// background notice.
//
// The five `aa53975e` anti-regressions, carried over verbatim (§7):
// never a user message; no imperatives and no language instructions; the
// operation id derives from a durable fact so a crash mid-delivery cannot
// double-deliver; an engine-death orphan is not an outcome and never notifies;
// and the three suppression conditions are evaluated before delivery.

// deliverFleetMonitorNotice admits one monitor event to one chat.
func (r *providerChatRuntime) deliverFleetMonitorNotice(notice fleetMonitorNotice) error {
	if r == nil {
		return errors.New("the durable chat actor is unavailable")
	}
	tabID, chatID := strings.TrimSpace(notice.TabID), strings.TrimSpace(notice.ChatID)
	if chatID == "" {
		return errors.New("a monitor notice needs the chat that asked")
	}
	switch notice.Kind {
	case fleetlog.KindExit:
		if notice.ProcessReceipt != nil {
			// A process child has no pre-P4 local delivery to reuse: before
			// this, a registered lane settled in silence and the chat found out
			// by looking. So its notice is built here, through the SAME actor
			// admission every other fleet notice goes through — the stopped-turn
			// fence, the deleted-chat fence, the replayed-operation no-op — and
			// with a task-notification body, never a user message.
			return r.admitFleetNotice(tabID, chatID, notice,
				fleetNoticeOperationID("process-exit", tabID, chatID, notice.ProcessReceipt.ReceiptID),
				processNoticeStatus(notice.Outcome), formatFleetProcessExit(notice))
		}
		// The settle path is the local one, unchanged. Parity is not asserted
		// about this call; it IS the call.
		receipt := notice.Receipt
		receipt.ParentTabID, receipt.ParentChatID = tabID, chatID
		// The host's lane and parent-turn ids name things in the HOST's chat.
		// Here they are replaced by this chat's own, which is what makes the
		// notice land on the right lane and what makes the "parent turn was
		// stopped" suppression evaluable at all. An empty lane falls through to
		// the chat's desired lane, exactly as a local completion with no origin
		// lane does.
		receipt.OriginLaneID = strings.TrimSpace(notice.LaneID)
		receipt.OriginOperationID = strings.TrimSpace(notice.OriginOperationID)
		return r.deliverSubagentCompletion(tabID, chatID, receipt)
	case fleetlog.KindAttention:
		if notice.AttentionKind == "stalled" || notice.AttentionKind == "max_hold" {
			// Non-terminal and one-shot: the job is still running, and the chat
			// is told once because the job itself declared what silence — or a
			// long hold — would mean. A stall that notified on every check
			// would be a chat nobody reads.
			return r.admitFleetNotice(tabID, chatID, notice,
				fleetNoticeOperationID("process-"+notice.AttentionKind, tabID, chatID, notice.TargetID),
				fleetNoticeStatusStalled, formatFleetProcessAttention(notice))
		}
		return r.admitFleetNotice(tabID, chatID, notice,
			fleetNoticeOperationID("attention", tabID, chatID, notice.TargetID+"\x00"+notice.AttentionText),
			"attention", formatFleetAttention(notice))
	case fleetMonitorHomeDown:
		return r.admitFleetNotice(tabID, chatID, notice,
			fleetNoticeOperationID("home-down", tabID, chatID, notice.Home+"\x00"+notice.Since.UTC().Format(time.RFC3339)),
			fleetNoticeStatusHomeDown, formatFleetHomeDown(notice))
	case fleetMonitorHomeUp:
		// home_up is not actionable: the backlog that follows it is. Announcing
		// recovery as its own notice would be a line that says nothing happened.
		return nil
	}
	return nil
}

// fleetNoticeStatusHomeDown is the notice status for a machine this daemon
// cannot see. It is deliberately NOT one of the outcome words: `home_down`
// means unknown, and a watcher that rendered it as `done` or `failed` would be
// converting it into an outcome — the one thing §7 forbids.
const fleetNoticeStatusHomeDown = "home_down"

// admitFleetNotice is the actor admission every non-settle monitor notice goes
// through. It mirrors deliverSubagentCompletion's fences exactly: a deleted or
// re-homed chat acknowledges and drops rather than being recreated, a stopped
// parent turn suppresses, and a replayed operation id is a no-op.
func (r *providerChatRuntime) admitFleetNotice(tabID, chatID string, notice fleetMonitorNotice, operationID providercontract.OperationID, status, message string) error {
	actor, err := r.actor(chatID)
	if err != nil {
		return err
	}
	laneID := providercontract.LaneID(strings.TrimSpace(notice.LaneID))
	actor.mu.Lock()
	state := actor.engine.ReadSnapshot()
	if state.Deleted || state.Presentation.TabID != strings.TrimSpace(tabID) {
		// A deleted or re-homed actor must never be recreated or addressed
		// through another tab. Acknowledging it is what stops a redelivery loop
		// on a chat that is permanently ineligible.
		actor.mu.Unlock()
		return nil
	}
	if subagentCompletionParentStopped(state, notice.OriginOperationID) {
		actor.mu.Unlock()
		return nil
	}
	if _, exists := state.Operations[operationID]; exists {
		actor.mu.Unlock()
		return nil
	}
	if laneID == "" {
		laneID = state.DesiredLaneID
	}
	lane, ok := state.Lanes[laneID]
	if !ok || lane.Identity.ChatID != strings.TrimSpace(chatID) {
		actor.mu.Unlock()
		return errors.New("the chat that asked has no provider lane for this fleet notice")
	}
	presentation := providercontract.TurnPresentation{
		UserMessageID:      stableAgentMessageIdentity("u", operationID),
		AssistantMessageID: stableAgentMessageIdentity("a", operationID),
		QueueID:            string(operationID), PromptText: message, Title: state.Presentation.Title,
		Origin: "agent", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		NoticeKind: "subagent", NoticeTitle: notice.Label, NoticeStatus: status,
	}
	if err := actor.engine.Apply(chat.Submit{
		OperationID: operationID, LaneID: laneID, Text: message, ModelID: lane.ModelID, ModeID: lane.ModeID,
		Presentation: presentation,
	}); err != nil {
		actor.mu.Unlock()
		return err
	}
	actor.mu.Unlock()
	actor.coordinator.Wake()
	return nil
}

// fleetNoticeOperationID derives the admission key from durable facts only —
// the kind, the chat and the event's own identity — so a redelivery after a
// crash, a reconnect or a failed ack produces the same key and therefore no
// second row.
func fleetNoticeOperationID(kind, tabID, chatID, identity string) providercontract.OperationID {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"fleet-monitor-notice-v1", kind, strings.TrimSpace(tabID), strings.TrimSpace(chatID), identity,
	}, "\x00")))
	return providercontract.OperationID(fmt.Sprintf("fleet-notice:%x", sum[:16]))
}

// formatFleetAttention is the task-notification shape Claude Code uses for its
// own background agents, so the model reads it as a notification and never as
// the user speaking. It states what happened and nothing else: no imperative,
// no instruction about what to do next, no language direction.
func formatFleetAttention(notice fleetMonitorNotice) string {
	var b strings.Builder
	b.WriteString("<task-notification>\n")
	fmt.Fprintf(&b, "<task-id>%s</task-id>\n<status>attention</status>\n", notice.TargetID)
	fmt.Fprintf(&b, "<summary>Agent %q needs a decision on another machine</summary>\n", notice.Label)
	if text := strings.TrimSpace(notice.AttentionText); text != "" {
		fmt.Fprintf(&b, "<result>%s</result>\n", text)
	}
	b.WriteString("</task-notification>")
	return redactedSessionString(b.String())
}

// formatFleetHomeDown says unknown, and says it in words that cannot be read as
// an outcome. There is no <result> and no outcome verb: the children are listed
// as `last known`, because what this daemon knows is when it last heard, not
// what happened afterwards.
func formatFleetHomeDown(notice fleetMonitorNotice) string {
	known := append([]string(nil), notice.LastKnown...)
	sort.Strings(known)
	var b strings.Builder
	b.WriteString("<task-notification>\n")
	fmt.Fprintf(&b, "<task-id>home-down:%s</task-id>\n<status>unknown</status>\n", notice.Home)
	fmt.Fprintf(&b, "<summary>Machine %q has not answered since %s; the state of %d delegated agent(s) there is unknown, not finished</summary>\n",
		notice.Home, notice.Since.UTC().Format(time.RFC3339), len(known))
	if len(known) > 0 {
		fmt.Fprintf(&b, "<last-known>%s</last-known>\n", strings.Join(known, ", "))
	}
	b.WriteString("</task-notification>")
	return redactedSessionString(b.String())
}

// fleetNoticeStatusStalled is a running job that said its silence would
// matter. It is deliberately not an outcome word: the job has not ended, and a
// status a reader could mistake for "finished" would be the same conversion
// §7 forbids for home_down.
const fleetNoticeStatusStalled = "stalled"

// processNoticeStatus maps a process child's outcome onto the status the notice
// line shows.
//
// `settled` becomes "done" and `failed` becomes "failed", which are the two
// words the line has always carried. `timed_out` and `lost` are their own
// words because neither is either of those: a job stopped at its ceiling did
// not fail on its own terms, and a lost job's outcome is genuinely unknown —
// rendering it as finished or as failed would be inventing the one fact nobody
// has.
func processNoticeStatus(outcome string) string {
	switch strings.TrimSpace(outcome) {
	case fleetlog.OutcomeSettled:
		return "done"
	case fleetlog.OutcomeTimedOut:
		return fleetlog.OutcomeTimedOut
	case fleetlog.OutcomeLost:
		return fleetlog.OutcomeLost
	case fleetlog.OutcomeCancelled:
		return fleetlog.OutcomeCancelled
	default:
		return "failed"
	}
}

// formatFleetProcessExit is the task-notification shape Claude Code uses for
// its own background work, so the model reads it as a notification and never
// as the user speaking.
//
// It states what happened and where to look, and nothing else: no imperative,
// no instruction about what to do next, no language direction. The output file
// is named because that is the actionable part — output is a file, read by a
// bounded tail, never copied into a log (delegation-fabric.md §3).
func formatFleetProcessExit(notice fleetMonitorNotice) string {
	receipt := notice.ProcessReceipt
	if receipt == nil {
		return ""
	}
	label := firstNonEmptyString(notice.Label, receipt.Label, receipt.TaskID)
	var b strings.Builder
	b.WriteString("<task-notification>\n")
	fmt.Fprintf(&b, "<task-id>%s</task-id>\n<status>%s</status>\n", receipt.TaskID, processNoticeStatus(notice.Outcome))
	fmt.Fprintf(&b, "<summary>%s</summary>\n", processExitSummary(notice, label))
	if receipt.ExitCode != nil {
		fmt.Fprintf(&b, "<exit-code>%d</exit-code>\n", *receipt.ExitCode)
	}
	if receipt.ElapsedMs > 0 {
		fmt.Fprintf(&b, "<elapsed>%s</elapsed>\n", (time.Duration(receipt.ElapsedMs) * time.Millisecond).Round(time.Second))
	}
	if path := strings.TrimSpace(receipt.OutputFile); path != "" {
		fmt.Fprintf(&b, "<output-file>%s</output-file>\n", path)
	}
	if summary := strings.TrimSpace(receipt.Summary); summary != "" {
		fmt.Fprintf(&b, "<result>%s</result>\n", summary)
	}
	b.WriteString("</task-notification>")
	return redactedSessionString(b.String())
}

// processExitSummary says what happened in one sentence, and says UNKNOWN when
// that is what it is. A lost job gets the only honest sentence available: its
// supervisor is gone and wrote no outcome, so what the work did is not known —
// which is a different sentence from "it failed", with a different fix.
func processExitSummary(notice fleetMonitorNotice, label string) string {
	switch strings.TrimSpace(notice.Outcome) {
	case fleetlog.OutcomeSettled:
		return fmt.Sprintf("Job %q finished", label)
	case fleetlog.OutcomeTimedOut:
		return fmt.Sprintf("Job %q was stopped at the max_runtime it declared; whether its work was complete is unknown", label)
	case fleetlog.OutcomeLost:
		return fmt.Sprintf("Job %q is lost: its supervisor is gone and wrote no outcome, so what it did is unknown, not finished", label)
	case fleetlog.OutcomeCancelled:
		return fmt.Sprintf("Job %q was stopped on request", label)
	default:
		return fmt.Sprintf("Job %q failed", label)
	}
}

// formatFleetProcessAttention is the one-shot non-terminal notice: a declared
// stall, or a resource held past the hold its holder declared. The job is
// STILL RUNNING, and the wording says so, because a notice that read like an
// ending would have somebody clean up after work that is still going.
func formatFleetProcessAttention(notice fleetMonitorNotice) string {
	var b strings.Builder
	b.WriteString("<task-notification>\n")
	fmt.Fprintf(&b, "<task-id>%s</task-id>\n<status>%s</status>\n", notice.TargetID, fleetNoticeStatusStalled)
	switch notice.AttentionKind {
	case "max_hold":
		fmt.Fprintf(&b, "<summary>Job %q is still holding the resource it declared a shorter hold for; it has not been stopped</summary>\n", notice.Label)
	default:
		fmt.Fprintf(&b, "<summary>Job %q has gone quiet past the stale_after it declared; it is still running</summary>\n", notice.Label)
	}
	if text := strings.TrimSpace(notice.AttentionText); text != "" {
		fmt.Fprintf(&b, "<evidence>%s</evidence>\n", text)
	}
	b.WriteString("</task-notification>")
	return redactedSessionString(b.String())
}
