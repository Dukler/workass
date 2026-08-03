package acp

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Harness-native turn evidence.
//
// Until v3 the daemon answered "is this request finished or merely paused?"
// from its own registry, and could not see a turn the harness started on its
// own at all — the model's prose for such a turn was discarded at
// bridge.go:700-717 because no job owned the session. The Claude Agent SDK
// already publishes both facts. Its Stop hook carries the background work still
// in flight and the wakes scheduled to re-invoke the session, documented in the
// vendored typings as: "Lets hooks distinguish 'session is done' from 'session
// is paused waiting for background work to wake it'."
//
// This file holds what the host reports, keyed by session, and answers the two
// questions the clamp asks of it. It stores facts, never verdicts.

const (
	harnessTurnPhaseStarted = "started"
	harnessTurnPhaseEnded   = "ended"
	harnessTurnPhaseFailed  = "failed"
	// attention is a receipt only. A mid-turn notification is usually resolved
	// mid-turn, and the end-of-turn question is already answered from live
	// state by chatHasPendingPermission — a verdict from a stale notification
	// would be exactly the kind of guess this lane removes.
	harnessTurnPhaseAttention = "attention"
)

type harnessBackgroundTask struct {
	ID     string
	Type   string
	Status string
}

type harnessSessionCron struct {
	Schedule  string
	Recurring bool
}

// harnessTurnEvidence is the latest turn-end report for one session.
//
// TasksKnown/CronsKnown carry the distinction the whole design rests on: an
// absent list is UNKNOWN (an older CLI, or hooks disabled by policy) while an
// empty list is a positive proof of quiet. Collapsing them would let silence
// read as "nothing is running", which is the false-done this lane exists to
// prevent.
type harnessTurnEvidence struct {
	PromptID       string
	Tasks          []harnessBackgroundTask
	TasksKnown     bool
	Crons          []harnessSessionCron
	CronsKnown     bool
	TerminalReason string
	StopReason     string
	OriginKind     string
	// HookEvidence is false when no Stop hook ran for the turn. Absence is not
	// evidence: the clamp must fall back to its pre-v3 behaviour.
	HookEvidence bool
	At           time.Time
	TabID        string
	ChatID       string
	// Parked/ParkNote are derived once, at record time, while the previous
	// report is still in hand — the repeat-suppression rule needs both and the
	// store keeps no history.
	Parked   bool
	ParkNote string
}

// Complete reports whether this evidence can answer the park question at all.
func (e *harnessTurnEvidence) Complete() bool {
	return e != nil && e.HookEvidence && e.TasksKnown && e.CronsKnown
}

// Quiet is the harness stating that nothing is in flight and nothing is
// scheduled to wake this session. Only meaningful when Complete.
func (e *harnessTurnEvidence) Quiet() bool {
	return e.Complete() && len(e.Tasks) == 0 && len(e.Crons) == 0
}

// oneShotCrons counts scheduled wakes that fire once — ScheduleWakeup, the
// self-scheduled park that nothing else in this system can observe.
//
// A recurring cron (/loop, CronCreate) is deliberately NOT counted: it is a
// standing schedule, not evidence that THIS request is unfinished. Counting it
// would make `done` permanently unreachable for a looping chat, which is worse
// than the bug being fixed (gate amendment D6).
func (e *harnessTurnEvidence) oneShotCrons() int {
	if e == nil {
		return 0
	}
	n := 0
	for _, cron := range e.Crons {
		if !cron.Recurring {
			n++
		}
	}
	return n
}

type harnessTurnStore struct {
	mu      sync.Mutex
	latest  map[string]*harnessTurnEvidence
	started map[string]string
}

func newHarnessTurnStore() *harnessTurnStore {
	return &harnessTurnStore{
		latest:  make(map[string]*harnessTurnEvidence),
		started: make(map[string]string),
	}
}

// record keeps the latest report per session and returns it.
//
// A steered turn produces several Stop hooks and a fresh prompt_id per
// direction, and a transparently retried OAuth failure produces a `failed` that
// a later success supersedes. Latest-wins is therefore the rule, and evidence is
// consumed only when a job ends (gate amendment D5).
func (s *harnessTurnStore) record(sessionID string, evidence *harnessTurnEvidence) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || evidence == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest[sessionID] = evidence
}

func (s *harnessTurnStore) get(sessionID string) *harnessTurnEvidence {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest[sessionID]
}

func (s *harnessTurnStore) forget(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.latest, sessionID)
	delete(s.started, sessionID)
}

// markStarted records the prompt id of a harness-born turn and reports whether
// this is a new one. It is the adoption trigger.
func (s *harnessTurnStore) markStarted(sessionID, promptID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.started[sessionID]; ok && prior == promptID {
		return false
	}
	s.started[sessionID] = promptID
	return true
}

func (s *harnessTurnStore) clearStarted(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.started, sessionID)
}

func harnessEvidenceFromUpdate(update map[string]any) *harnessTurnEvidence {
	evidence := &harnessTurnEvidence{
		PromptID:       asString(update["promptId"]),
		TerminalReason: asString(update["terminalReason"]),
		StopReason:     asString(update["stopReason"]),
		OriginKind:     asString(update["originKind"]),
		HookEvidence:   asBool(update["harnessEvidence"]),
		At:             time.Now(),
	}
	if raw, ok := update["backgroundTasks"].([]any); ok {
		evidence.TasksKnown = true
		for _, item := range raw {
			task := mapFromAny(item)
			evidence.Tasks = append(evidence.Tasks, harnessBackgroundTask{
				ID:     asString(task["id"]),
				Type:   asString(task["type"]),
				Status: asString(task["status"]),
			})
		}
	}
	if raw, ok := update["sessionCrons"].([]any); ok {
		evidence.CronsKnown = true
		for _, item := range raw {
			cron := mapFromAny(item)
			evidence.Crons = append(evidence.Crons, harnessSessionCron{
				Schedule:  asString(cron["schedule"]),
				Recurring: asBool(cron["recurring"]),
			})
		}
	}
	return evidence
}

// parkEvidence answers whether the harness says something will wake this chat.
//
// Two filters apply before a task counts (gate amendment D9). A task whose
// registry mirror was classified a service is a dev server, not owed work — the
// shape that already cost this product a chat pinned "Trabajando" for hours.
// And a task identical to one reported at the previous turn end, same type and
// same status, is not FRESH evidence: it parks once, and a second identical
// report does not re-arm it, or a never-exiting task parks the chat forever.
func (m *Manager) harnessParkEvidence(tabID, chatID string, evidence, prior *harnessTurnEvidence) (parked bool, note string) {
	if !evidence.Complete() {
		return false, ""
	}
	priorTasks := map[string]string{}
	if prior != nil {
		for _, task := range prior.Tasks {
			priorTasks[task.ID] = task.Type + "\x00" + task.Status
		}
	}
	fresh := 0
	kinds := map[string]bool{}
	for _, task := range evidence.Tasks {
		if m.harnessTaskIsService(tabID, chatID, task) {
			continue
		}
		if signature, ok := priorTasks[task.ID]; ok && signature == task.Type+"\x00"+task.Status {
			continue
		}
		fresh++
		if task.Type != "" {
			kinds[task.Type] = true
		}
	}
	oneShot := evidence.oneShotCrons()
	if fresh == 0 && oneShot == 0 {
		return false, ""
	}
	parts := make([]string, 0, 2)
	if fresh > 0 {
		labels := make([]string, 0, len(kinds))
		for kind := range kinds {
			labels = append(labels, kind)
		}
		sortStrings(labels)
		if len(labels) > 0 {
			parts = append(parts, "background work in flight ("+strings.Join(labels, ", ")+")")
		} else {
			parts = append(parts, "background work in flight")
		}
	}
	if oneShot > 0 {
		schedule := ""
		for _, cron := range evidence.Crons {
			if !cron.Recurring && cron.Schedule != "" {
				schedule = cron.Schedule
				break
			}
		}
		if schedule != "" {
			parts = append(parts, "a scheduled wake ("+schedule+")")
		} else {
			parts = append(parts, "a scheduled wake")
		}
	}
	return true, "waiting on " + strings.Join(parts, " and ")
}

// harnessTaskIsService consults the registry the SDK's own task lifecycle
// already mirrors into (observeClaudeSpawnedWork, spawned_work.go:532), where
// the service classifier lives.
func (m *Manager) harnessTaskIsService(tabID, chatID string, task harnessBackgroundTask) bool {
	if strings.TrimSpace(task.ID) == "" {
		return false
	}
	m.spawnedWorkMu.Lock()
	rec := m.spawnedWork[spawnedWorkKey(tabID, chatID, task.ID)]
	m.spawnedWorkMu.Unlock()
	return rec != nil && isServiceSpawnedWork(rec.Item)
}

func asBool(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// observeClaudeTurn is the daemon's entry point for the host's turn-lifecycle
// updates. It never blocks the bridge reader: everything it does is bounded and
// synchronous, and the adopted turn is ended by a later update, not by waiting.
func (m *Manager) observeClaudeTurn(bridge *Bridge, tabID, chatID, sessionID string, update map[string]any) {
	switch asString(update["phase"]) {
	case harnessTurnPhaseStarted:
		promptID := asString(update["promptId"])
		if asBool(update["humanAuthored"]) {
			// The user's own turn already has a job; nothing to adopt.
			m.harnessTurns.clearStarted(sessionID)
			return
		}
		if !m.harnessTurns.markStarted(sessionID, promptID) {
			return
		}
		m.adoptHarnessTurn(bridge, tabID, chatID, sessionID, promptID)
	case harnessTurnPhaseEnded:
		prior := m.harnessTurns.get(sessionID)
		evidence := harnessEvidenceFromUpdate(update)
		evidence.TabID, evidence.ChatID = tabID, chatID
		evidence.Parked, evidence.ParkNote = m.harnessParkEvidence(tabID, chatID, evidence, prior)
		m.harnessTurns.record(sessionID, evidence)
		m.harnessTurns.clearStarted(sessionID)
		m.endAdoptedHarnessTurn(bridge, sessionID)
	case harnessTurnPhaseFailed, harnessTurnPhaseAttention:
		// Receipts only. A `failed` can be superseded by the host's transparent
		// OAuth retry, and a mid-turn notification is normally resolved
		// mid-turn — the end-of-turn question is answered from live permission
		// state, not from a stale event (gate amendments D5, D7 and the scope
		// cut on attention).
	}
}

// adoptHarnessTurn gives a harness-born turn the job it needs to be visible.
//
// It deliberately does NOT go through the normal start path: that path sends
// session/prompt, and a harness turn must receive no prompt — the host has no
// activePrompt for it, so a synthesized prompt would push a spurious user
// message into the live SDK stream and then be settled by the harness turn's
// own terminal result. The job here holds no prompt RPC, takes no promptMu, and
// is ended by the matching phase:"ended" update (gate amendment D1).
func (m *Manager) adoptHarnessTurn(bridge *Bridge, tabID, chatID, sessionID, promptID string) {
	if bridge == nil || strings.TrimSpace(chatID) == "" || strings.TrimSpace(sessionID) == "" {
		return
	}
	// A race with a human turn loses to the human turn. That is the safe
	// direction: the cost is one un-adopted turn, i.e. exactly today's
	// behaviour, and never two jobs on one chat.
	if _, running := m.RunningJobForChat(tabID, chatID); running {
		return
	}
	if existing := bridge.jobForSession(sessionID); existing != nil && existing.Status == "running" {
		return
	}
	now := time.Now()
	job := &Job{
		ID:          "harness-turn-" + strconv.FormatInt(now.UnixNano(), 10),
		Kind:        "app-chat",
		Title:       "Continuing on its own",
		Status:      "running",
		StartedAt:   now.Format(time.RFC3339Nano),
		ChatID:      chatID,
		TabID:       tabID,
		SessionID:   sessionID,
		ProviderID:  bridge.ProviderID(),
		harnessTurn: true,
	}
	job.touchActivity()
	m.mu.Lock()
	if m.resetting {
		m.mu.Unlock()
		return
	}
	m.jobs[job.ID] = job
	m.mu.Unlock()
	bridge.setJobForSession(sessionID, job)
	// A turn nobody asked for resumes the chat's existing obligation; it never
	// opens a new one, because the user asked for nothing here.
	m.resumeObligation(tabID, chatID)
	m.emit("job:event", map[string]any{"type": "start", "job": job.Public()})
}

// endAdoptedHarnessTurn closes the job adopted for this session, if any.
//
// This is the only thing that ends such a job, which is why the host emits
// phase:"ended" from every terminal-result path including the one where no
// prompt is active. An adopted job that never ends would park every later human
// prompt behind ErrChatBusy on a phantom `working` record — the stuck-queue
// incident rebuilt inside the fix for it.
func (m *Manager) endAdoptedHarnessTurn(bridge *Bridge, sessionID string) {
	if bridge == nil {
		return
	}
	job := bridge.jobForSession(sessionID)
	if job == nil || !job.harnessTurn || job.Status != "running" {
		return
	}
	job.Status = "done"
	m.mu.Lock()
	m.rememberFinishedJobLocked(job.ID)
	delete(m.jobs, job.ID)
	m.mu.Unlock()
	bridge.flushJobBuffers(job)
	m.settleObligationForJob(job)
	m.emit("job:event", map[string]any{"type": "end", "job": job.Public()})
	bridge.clearJobForSession(sessionID, job)
	m.notifyJobEnd(job.TabID, job.ChatID)
}

// abandonAdoptedHarnessTurn is the backstop for an engine that dies mid-turn.
// Without it the adopted job outlives the bridge that owned it.
func (m *Manager) abandonAdoptedHarnessTurn(bridge *Bridge, sessionID string) {
	if bridge == nil {
		return
	}
	job := bridge.jobForSession(sessionID)
	if job == nil || !job.harnessTurn || job.Status != "running" {
		return
	}
	job.Status = "failed"
	job.StopReason = "engine-crash"
	m.mu.Lock()
	m.rememberFinishedJobLocked(job.ID)
	delete(m.jobs, job.ID)
	m.mu.Unlock()
	m.settleObligationForJob(job)
	m.emit("job:event", map[string]any{"type": "end", "job": job.Public()})
	bridge.clearJobForSession(sessionID, job)
	m.notifyJobEnd(job.TabID, job.ChatID)
	m.harnessTurns.clearStarted(sessionID)
}

// getByChat finds the latest turn-end report for a chat. The store is keyed by
// session because that is what the wire carries; the map is one entry per live
// chat, so the scan is cheaper than maintaining a second index.
func (s *harnessTurnStore) getByChat(tabID, chatID string) *harnessTurnEvidence {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var newest *harnessTurnEvidence
	for _, evidence := range s.latest {
		if evidence == nil || evidence.TabID != tabID || evidence.ChatID != chatID {
			continue
		}
		if newest == nil || evidence.At.After(newest.At) {
			newest = evidence
		}
	}
	return newest
}

// abandonAdoptedHarnessTurns ends every adopted job on a bridge that is going
// away. Such a job has no prompt to re-drive, so crash recovery cannot help it.
func (m *Manager) abandonAdoptedHarnessTurns(bridge *Bridge) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	sessions := make([]string, 0, len(bridge.jobsBySession))
	for sessionID, job := range bridge.jobsBySession {
		if job != nil && job.harnessTurn && job.Status == "running" {
			sessions = append(sessions, sessionID)
		}
	}
	bridge.mu.Unlock()
	for _, sessionID := range sessions {
		m.abandonAdoptedHarnessTurn(bridge, sessionID)
	}
}
