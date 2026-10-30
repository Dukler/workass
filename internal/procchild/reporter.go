package procchild

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Reporting: the supervisor telling its own daemon, and being answered.
//
// This is the primary path (docs/specs/fleet-runtime.md §13 P4). Everything a
// supervisor witnesses is reported to the daemon on the machine it is running
// on, over the authenticated Agent API — the same tool surface an agent calls,
// with a credential minted for this one job — and the daemon answers with the
// highest seq it applied. The daemon then publishes it: the entity log, the
// monitors at both ends, and the owning chat wherever in the fleet it is.
//
// The outbox is the write-ahead copy, not the channel. The order is always
// insert, then send, then mark delivered on the ack, which gives the three
// properties the design needs and the spool never had:
//
//   - A crash between the insert and the send replays once. The event is in
//     the queue and nothing has acknowledged it.
//   - A daemon that is down (a restart, a self-update) costs nothing: the
//     queue grows and drains in seq order when it answers again.
//   - A supervisor that CANNOT report is never silent. It says so on stderr,
//     it says so in its receipt, it keeps trying, and the gap is reported to
//     the daemon when it reconnects, so the row shows that it happened rather
//     than looking healthy throughout.

// HeartbeatInterval is how often a running supervisor reports that it is still
// there. It is internal/fleetclient's link heartbeat deliberately: one cadence
// for "is the other end alive", so the two cannot drift apart.
const HeartbeatInterval = time.Minute

// SilenceThreshold is how long the daemon waits past the heartbeat before it
// calls a running job's supervisor silent. Three beats, so one lost report is
// not an incident.
const SilenceThreshold = 3 * HeartbeatInterval

// reportRetryInterval is how often an unreachable daemon is retried. Much
// faster than the heartbeat, because an unreachable daemon is usually a
// restart measured in seconds and the job is waiting to be known about.
const reportRetryInterval = 5 * time.Second

// reportGrace is how long the daemon may be unreachable before the supervisor
// says so out loud. Below it, a restart is routine and a warning would be
// noise; above it, the operator's rule applies and silence is the one
// unacceptable outcome.
const reportGrace = 30 * time.Second

// reportComplaintInterval is how often the complaint is repeated while the
// daemon stays unreachable. One line, not one per retry.
const reportComplaintInterval = 5 * time.Minute

// settleReportDeadline bounds how long a finished supervisor keeps trying to
// deliver its outcome before it exits saying it could not. It does not
// matter that it is generous: the process has nothing else to do, and the
// alternative is an outcome that exists only in the outbox.
const settleReportDeadline = 2 * time.Minute

// Report is one batch on its way to the daemon. It carries the registration
// facts every time, because the daemon's row and this batch are one call: a
// daemon that restarted and lost nothing but its in-memory row rebuilds it
// from the same message that delivers the events.
type Report struct {
	Spec          Spec
	ChildID       string
	OutputFile    string
	DoneFile      string
	SupervisorPID int
	ProcessGroup  int
	Group         GroupKind
	Events        []Event
	// Heartbeat is the cadence this supervisor reports at, so the daemon can
	// decide what counts as silence without a second constant of its own.
	Heartbeat time.Duration
	// ReportGap is how long this supervisor was unable to reach the daemon
	// immediately before this batch. Zero on a healthy report.
	ReportGap time.Duration
}

// Acknowledgement is the daemon's answer: the row it holds, and the highest
// seq it applied. AckThrough is the only thing that makes an event delivered.
type Acknowledgement struct {
	ChildID    string
	Status     string
	AckThrough uint64
}

// Reporter is the daemon client. It is injected so the supervisor is testable
// without a daemon, and so the one place that speaks to the daemon stays the
// one place that speaks to the daemon.
type Reporter interface {
	Report(request Report) (Acknowledgement, error)
}

// ReportingHealth is what a run can honestly say about having been reported.
type ReportingHealth struct {
	// Delivered is true when every event this run produced was acknowledged.
	Delivered bool
	// Pending is how many events are still in the outbox, undelivered.
	Pending int
	// Gap is the longest stretch this run could not reach its daemon.
	Gap time.Duration
	// LastError is the last reason a report failed, redaction already applied
	// by the reporter.
	LastError string
	// Unreported is a run with no daemon to report to at all, which only
	// happens when the operator asked for one explicitly.
	Unreported bool
}

// journal is the supervisor's recorder: one outbox, one reporter, and the
// honesty about whether the second one is working.
type journal struct {
	childID     string
	outbox      *Outbox
	reporter    Reporter
	diagnostics io.Writer
	now         func() time.Time
	// facts answers what the registration facts are RIGHT NOW. They change
	// during a run — a child gains a pid and a process group when it starts —
	// and a replayed batch must carry the current ones, not the ones that were
	// true when the first event was queued.
	facts func() Report

	// retry and deadline are the two durations a test needs to move. They are
	// the constants above unless a Spec overrode them, so production has one
	// answer and no knob.
	retry    time.Duration
	deadline time.Duration

	mu               sync.Mutex
	ackThrough       uint64
	unreachableSince time.Time
	complainedAt     time.Time
	longestGap       time.Duration
	lastError        string
	unreported       bool
	lastBeat         time.Time
}

func newJournal(childID string, outbox *Outbox, reporter Reporter, diagnostics io.Writer, now func() time.Time, facts func() Report, retry, deadline time.Duration) *journal {
	if now == nil {
		now = time.Now
	}
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	if retry <= 0 {
		retry = reportRetryInterval
	}
	if deadline <= 0 {
		deadline = settleReportDeadline
	}
	return &journal{
		childID: childID, outbox: outbox, reporter: reporter,
		diagnostics: diagnostics, now: now, facts: facts,
		retry: retry, deadline: deadline, unreported: reporter == nil,
		// The clock starts at construction, not at the first tick: otherwise
		// the first tick is always "due" and every run reports a heartbeat
		// seconds after the event it already reported.
		lastBeat: now(),
	}
}

// record queues one event and tries to deliver everything queued.
//
// The queue write is the part that may not fail: an event the supervisor
// witnessed and could not record is the one thing no later retry recovers. A
// delivery that fails is reported and retried.
func (j *journal) record(kind string, payload map[string]any) (Event, error) {
	event, err := j.outbox.Append(j.childID, kind, payload)
	if err != nil {
		return Event{}, err
	}
	j.flush()
	return event, nil
}

// flush delivers every queued event in seq order and applies the ack.
//
// It is safe to call at any time and from any state: the queue is the whole of
// what it sends, so a flush with nothing pending is one read, and a flush after
// an outage sends the backlog in order.
func (j *journal) flush() {
	if j == nil || j.reporter == nil {
		return
	}
	pending, err := j.outbox.Pending(j.childID, 0)
	if err != nil {
		j.noteFailure(fmt.Errorf("the outbox could not be read: %w", err))
		return
	}
	j.mu.Lock()
	gap := time.Duration(0)
	if !j.unreachableSince.IsZero() {
		gap = j.now().Sub(j.unreachableSince)
	}
	j.mu.Unlock()
	request := j.facts()
	request.ChildID = j.childID
	request.Events = pending
	request.Heartbeat = HeartbeatInterval
	request.ReportGap = gap
	ack, err := j.reporter.Report(request)
	if err != nil {
		j.noteFailure(err)
		return
	}
	if len(pending) > 0 && ack.AckThrough == 0 {
		// A daemon that applied events must say how far it got. One that does
		// not is either older than this contract or wrong, and either way
		// reading its silence as an ack would retire the only copy of
		// something nobody has been told. Checked here rather than in the HTTP
		// client so it holds whatever transport delivered the report.
		j.noteFailure(errors.New("the daemon accepted the report without acknowledging any event; it is older than the supervised-job reporting contract and must be updated"))
		return
	}
	j.noteSuccess(ack)
	if ack.AckThrough > 0 {
		if err := j.outbox.MarkDelivered(j.childID, ack.AckThrough); err != nil {
			// The daemon has it; this machine merely failed to write down that
			// it has it. The cost is one redelivery, which the daemon dedups by
			// seq, so it is said once and not treated as a failure.
			fmt.Fprintf(j.diagnostics, "workass run: the daemon acknowledged through %d but the outbox could not record it (%v); it will be re-sent and ignored there\n", ack.AckThrough, err)
		}
	}
}

// beat reports liveness on the declared cadence, and is also the retry: an
// unreachable daemon is tried again much faster than the heartbeat, because a
// restart is usually over in seconds.
func (j *journal) beat() {
	if j == nil || j.reporter == nil {
		return
	}
	now := j.now()
	j.mu.Lock()
	unreachable := !j.unreachableSince.IsZero()
	due := HeartbeatInterval
	if unreachable {
		due = j.retry
	}
	if now.Sub(j.lastBeat) < due {
		j.mu.Unlock()
		return
	}
	j.lastBeat = now
	j.mu.Unlock()
	if unreachable {
		// Nothing new to say; the backlog is the message.
		j.flush()
		return
	}
	if _, err := j.record(KindHeartbeat, nil); err != nil {
		fmt.Fprintf(j.diagnostics, "workass run: could not record a heartbeat (%v)\n", err)
	}
}

func (j *journal) noteFailure(err error) {
	now := j.now()
	j.mu.Lock()
	if j.unreachableSince.IsZero() {
		j.unreachableSince = now
	}
	gap := now.Sub(j.unreachableSince)
	if gap > j.longestGap {
		j.longestGap = gap
	}
	j.lastError = strings.TrimSpace(err.Error())
	complain := gap >= reportGrace && (j.complainedAt.IsZero() || now.Sub(j.complainedAt) >= reportComplaintInterval)
	if complain {
		j.complainedAt = now
	}
	message := j.lastError
	j.mu.Unlock()
	if complain {
		// The operator's rule, literally: never silent. A job whose daemon
		// cannot be reached says so, keeps running, and keeps trying.
		fmt.Fprintf(j.diagnostics,
			"workass run: NOT REPORTING — this job has been unable to reach its Workass daemon for %s (%s). The work continues and every event is queued in the outbox; the chat that asked will be told when the daemon answers.\n",
			gap.Round(time.Second), message)
	}
}

func (j *journal) noteSuccess(ack Acknowledgement) {
	now := j.now()
	j.mu.Lock()
	recovered := time.Duration(0)
	if !j.unreachableSince.IsZero() {
		recovered = now.Sub(j.unreachableSince)
		if recovered > j.longestGap {
			j.longestGap = recovered
		}
		j.unreachableSince = time.Time{}
		j.complainedAt = time.Time{}
	}
	j.lastError = ""
	if ack.AckThrough > j.ackThrough {
		j.ackThrough = ack.AckThrough
	}
	complained := recovered >= reportGrace
	j.mu.Unlock()
	if complained {
		fmt.Fprintf(j.diagnostics, "workass run: reporting again after %s; the queued events have been delivered\n", recovered.Round(time.Second))
	}
}

// settle delivers the terminal event and does not give up quickly.
//
// A finished supervisor has nothing else to do, so it keeps trying until the
// deadline. What it must not do is exit quietly having failed: the return value
// is what the receipt reports and what the final stderr line says.
func (j *journal) settle() ReportingHealth {
	health := ReportingHealth{}
	if j == nil {
		return health
	}
	if j.reporter == nil {
		health.Unreported = true
		if j.outbox != nil {
			health.Pending, _ = j.outbox.PendingCount(j.childID)
		}
		return health
	}
	deadline := j.now().Add(j.deadline)
	for {
		j.flush()
		pending, err := j.outbox.PendingCount(j.childID)
		if err == nil && pending == 0 {
			break
		}
		if !j.now().Before(deadline) {
			break
		}
		time.Sleep(j.retry)
	}
	pending, _ := j.outbox.PendingCount(j.childID)
	j.mu.Lock()
	health = ReportingHealth{
		Delivered: pending == 0, Pending: pending,
		Gap: j.longestGap, LastError: j.lastError,
	}
	j.mu.Unlock()
	if !health.Delivered {
		fmt.Fprintf(j.diagnostics,
			"workass run: THIS OUTCOME WAS NOT REPORTED — %d event(s) are still queued in the outbox after %s of trying (%s). The daemon will read them for this job when it next reconciles it; until then its chat shows the job as not reporting.\n",
			health.Pending, j.deadline, health.LastError)
	}
	return health
}

// health is the current reporting state, for a caller that wants it without
// settling.
func (j *journal) health() ReportingHealth {
	if j == nil {
		return ReportingHealth{}
	}
	pending := 0
	if j.outbox != nil {
		pending, _ = j.outbox.PendingCount(j.childID)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return ReportingHealth{
		Delivered: pending == 0 && j.reporter != nil, Pending: pending,
		Gap: j.longestGap, LastError: j.lastError, Unreported: j.reporter == nil,
	}
}

// ErrUnreportedRun is the refusal a run with no credential gets.
//
// It exists because of what the operator ruled out: a run that supervises,
// spools and settles while no chat is ever told was indistinguishable, from
// the outside, from a run that reported — and that is the shape the first live
// test failed in. A human who genuinely wants an unreported run says so.
var ErrUnreportedRun = errors.New(
	"this run has no Workass credential, so nothing can be reported to a chat. " +
		"A supervised job that reports to nobody is not supervision, so it is refused rather than run quietly: " +
		"pass --unreported if you want the supervision, the outbox and the receipt with no chat told")
