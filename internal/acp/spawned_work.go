package acp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxSpawnedWorkPerChat       = 256
	maxSpawnedWorkReceiptBytes  = 4 * 1024 * 1024
	maxSpawnedWorkTailBytes     = 64 * 1024
	defaultSpawnedWorkTailBytes = 12 * 1024
	spawnedWorkMissingGrace     = 4 * time.Second
	externalWorkMissingGrace    = 10 * time.Second
	maxSpawnedWorkResultExcerpt = 600
	// A pathless record restored without a live ACP-session owner cannot be
	// reconciled after a daemon restart. Mark only that ownerless record
	// orphaned after a grace. A live session's silence is never terminal
	// evidence: its task may legitimately run for hours without progress.
	spawnedWorkPathlessGrace = 90 * time.Second
	// How long a stop waits for a polite exit before it stops asking. It is
	// bounded because a human is holding the button and the invoke is
	// synchronous: a dev server that traps SIGTERM to close its sockets gets its
	// moment, and anything still standing after that never intended to leave.
	spawnedWorkStopGrace = 1500 * time.Millisecond
	spawnedWorkStopPoll  = 50 * time.Millisecond
)

const trackedSubagentSpawnedWorkKind = "subagent"

// The two lifecycles that "running" used to conflate. Work has an end and that
// end is news for its chat; a service has no end, so only its death is news. An
// empty role means work, so every record written before this field existed
// keeps exactly the meaning it had.
const (
	spawnedWorkRoleService = "service"
	// spawnedWorkRoleWork is an explicit declaration, distinct from the empty
	// default only in that it pins: a spawner that says "this is work" is
	// believed over any later inference. A soak lane that runs a listener and
	// then falls silent is exactly the case where the evidence would be wrong
	// and the caller already knew better.
	spawnedWorkRoleWork = "work"
)

// serviceClassifyDwell is how long a process must BOTH hold a listening socket
// and write nothing before Workass concludes it is a service. Either signal
// alone lies: `go test` binds a listener while it is very much working, and a
// slow link step is silent without being a server. Together they describe a
// booted server waiting for traffic. 90s is spawnedWorkPathlessGrace, the
// threshold this file already trusts for "silence has started to mean
// something".
const serviceClassifyDwell = 90 * time.Second

type spawnedWorkLivenessClass string

const (
	spawnedWorkLivenessExternal  spawnedWorkLivenessClass = "external"
	spawnedWorkLivenessSubagent  spawnedWorkLivenessClass = "subagent"
	spawnedWorkLivenessInProcess spawnedWorkLivenessClass = "in-process"
	spawnedWorkLivenessProbed    spawnedWorkLivenessClass = "probed"
)

// SpawnedWorkItem is the provider-neutral, per-chat view of background work
// started naturally by a provider agent. Provider-native session ids and raw
// prompts never leave the daemon.
type SpawnedWorkItem struct {
	ID         string `json:"id"`
	TaskID     string `json:"taskId"`
	ToolCallID string `json:"toolCallId,omitempty"`
	TabID      string `json:"tabId"`
	ChatID     string `json:"chatId"`
	ProviderID string `json:"providerId"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	// Role is the lifecycle, not the shape: "" is work whose completion is news,
	// spawnedWorkRoleService is a process that is expected never to finish. It is
	// declared at registration or inferred by classifySpawnedWorkServices, and is
	// the reason a dev server no longer reports its chat as working.
	Role         string `json:"role,omitempty"`
	Status       string `json:"status"`
	StartedAt    string `json:"startedAt"`
	UpdatedAt    string `json:"updatedAt"`
	FinishedAt   string `json:"finishedAt,omitempty"`
	OutputFile   string `json:"outputFile,omitempty"`
	PID          *int   `json:"pid,omitempty"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	Summary      string `json:"summary,omitempty"`
	LastToolName string `json:"lastToolName,omitempty"`
	// ModelLabel and ResultExcerpt are additive and populated only for tracked
	// subagents. Both are redacted and bounded before they are stored.
	ModelLabel    string `json:"modelLabel,omitempty"`
	ResultExcerpt string `json:"resultExcerpt,omitempty"`
}

type SpawnedWorkReceipt struct {
	ReceiptID   string `json:"receiptId"`
	TaskID      string `json:"taskId"`
	ToolCallID  string `json:"toolCallId,omitempty"`
	TabID       string `json:"tabId"`
	ChatID      string `json:"chatId"`
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	Role        string `json:"role,omitempty"`
	Status      string `json:"status"`
	StartedAt   string `json:"startedAt"`
	FinishedAt  string `json:"finishedAt"`
	ElapsedMs   int64  `json:"elapsedMs"`
	OutputFile  string `json:"outputFile,omitempty"`
	PID         *int   `json:"pid,omitempty"`
	ExitCode    *int   `json:"exitCode,omitempty"`
	Summary     string `json:"summary,omitempty"`
	OutputTail  string `json:"outputTail,omitempty"`
	TailLimited bool   `json:"tailLimited,omitempty"`
}

type spawnedWorkRecord struct {
	Item             SpawnedWorkItem
	SessionID        string
	LastLevelSeen    time.Time
	MissingSince     time.Time
	PathlessSince    time.Time
	SawPID           bool
	ReceiptWritten   bool
	ExternalDoneFile string
	// Service classification evidence, deliberately not persisted: after a
	// daemon restart the dwell simply restarts, which costs one window of a
	// wrong pill and can never resurrect a stale verdict.
	ListeningQuietSince time.Time
	LastOutputSize      int64
	SawOutputSize       bool
}

type spawnedWorkSnapshotItem struct {
	SpawnedWorkItem
	DoneFile string `json:"doneFile,omitempty"`
}

type ExternalWorkRegistrationOptions struct {
	OwnerKey     string
	ParentChatID string
	ParentTabID  string
	TabID        string
	ChatID       string
	Label        string
	Role         string
	PID          *int
	OutputFile   string
	DoneFile     string
}

type ExternalWorkSettleOptions struct {
	OwnerKey     string
	ParentChatID string
	ParentTabID  string
	TabID        string
	ChatID       string
	WorkID       string
	Status       string
	ExitCode     *int
	Summary      string
}

type spawnedWorkCandidate struct {
	SessionID    string
	TabID        string
	ChatID       string
	ToolCallID   string
	ProviderID   string
	ProviderTool string
	Kind         string
	Label        string
	StartedAt    time.Time
}

type spawnToolObservation struct {
	SessionID  string
	TabID      string
	ChatID     string
	ProviderID string
	ToolCallID string
	Title      string
	Command    string
	RawInput   any
	Meta       map[string]any
	Output     string
}

var claudeBackgroundResultRE = regexp.MustCompile(`(?is)running in background with ID:\s*([A-Za-z0-9._-]+).*?output is being written to:\s*([^\r\n]+?\.output)\b`)

func normalizeSpawnedWorkTaskID(raw string) string {
	// Claude's Bash result is prose, and currently terminates the task id with
	// sentence punctuation ("ID: abc123. Output ..."). Keep punctuation that is
	// valid inside an id, but never let the prose terminator become identity.
	return strings.TrimRight(strings.TrimSpace(raw), ".,;:!?")
}

func spawnedWorkKey(tabID, chatID, taskID string) string {
	return strings.TrimSpace(tabID) + "\x00" + strings.TrimSpace(chatID) + "\x00" + normalizeSpawnedWorkTaskID(taskID)
}

func spawnedCandidateKey(sessionID, toolCallID string) string {
	return strings.TrimSpace(sessionID) + "\x00" + strings.TrimSpace(toolCallID)
}

func sameSpawnedWorkToolCall(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	return left == "" || right == "" || left == right
}

func isoNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// normalizeSpawnedWorkRole recognises only declarations it understands. An
// unknown word becomes the empty default rather than an error, because the
// empty default is work — the behaviour every record had before roles existed,
// and the safe direction to fail in. Silencing a chat that really is busy is
// the only outcome here that loses information.
func normalizeSpawnedWorkRole(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "service", "server", "daemon":
		return spawnedWorkRoleService
	case "work", "task", "job":
		return spawnedWorkRoleWork
	default:
		return ""
	}
}

func isServiceSpawnedWork(item SpawnedWorkItem) bool {
	return item.Role == spawnedWorkRoleService
}

func spawnedWorkKind(providerTool, taskType, subagentType string) string {
	joined := strings.ToLower(strings.Join([]string{providerTool, taskType, subagentType}, " "))
	switch {
	case strings.Contains(joined, "workflow"):
		return "workflow"
	case strings.Contains(joined, "agent"), strings.Contains(joined, "task"):
		return "agent"
	case strings.Contains(joined, "bash"), strings.Contains(joined, "shell"), strings.Contains(joined, "command"):
		return "bash"
	default:
		return "background"
	}
}

// spawnedWorkLivenessClassFor makes the registry's three authorities explicit:
// external work is done-file/PID driven, tracked subagents are owned solely by
// the subagent registry, and provider-spawned work is either engine-owned or
// output/PID probed. A tracked subagent therefore never settles from parent
// engine exit, foreground-turn silence, or an output-file probe.
func spawnedWorkLivenessClassFor(item SpawnedWorkItem) spawnedWorkLivenessClass {
	switch item.Kind {
	case "external":
		return spawnedWorkLivenessExternal
	case trackedSubagentSpawnedWorkKind:
		return spawnedWorkLivenessSubagent
	case "workflow", "agent":
		return spawnedWorkLivenessInProcess
	default:
		if item.OutputFile == "" && item.PID == nil {
			return spawnedWorkLivenessInProcess
		}
		return spawnedWorkLivenessProbed
	}
}

func boolFromMap(raw any, key string) bool {
	m := mapFromAny(raw)
	v, _ := m[key].(bool)
	return v
}

func (m *Manager) acceptsClaudeSpawnedWorkProvider(providerID string) bool {
	providerID = normalizeProviderID(providerID)
	return providerID == "claude" || (providerID == "mock" && !m.isProductionRuntime())
}

// acceptsExternalWorkProvider is deliberately broader than passive spawned-work
// observation. An explicit registration is authenticated by the ACP owner's
// opaque capability, so every real provider can use the same durable receipt
// and wake path. Mock remains a non-production test fixture.
func (m *Manager) acceptsExternalWorkProvider(providerID string) bool {
	providerID = normalizeProviderID(providerID)
	if providerID == "" {
		return false
	}
	return providerID != "mock" || !m.isProductionRuntime()
}

func randomExternalWorkID() (string, error) {
	buf := make([]byte, 9)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "xw" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func (m *Manager) externalWorkDir() string {
	return externalWorkRoot(m.opts.StateDir)
}

func externalWorkRoot(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		return ""
	}
	root := filepath.Join(stateDir, "external-work")
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return filepath.Clean(root)
}

func (m *Manager) providerForSpawnedWorkChat(tabID, chatID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.boundProviderForChatLocked(SessionOptions{TabID: tabID, ChatID: chatID})
}

func (m *Manager) resolveExternalWorkOwner(ownerKey, parentChatID, parentTabID, requestedChatID, requestedTabID string) (string, string, string, error) {
	if !m.ValidateAgentOwner(ownerKey, parentChatID, parentTabID) {
		return "", "", "", errors.New("no running Workass turn owns this external work request")
	}
	chatID, tabID, ok := m.ownerBinding(ownerKey, parentChatID, parentTabID)
	if !ok {
		return "", "", "", errors.New("no running Workass turn owns this external work request")
	}
	requestedChatID = strings.TrimSpace(requestedChatID)
	requestedTabID = strings.TrimSpace(requestedTabID)
	if requestedChatID != "" || requestedTabID != "" {
		if requestedChatID == "" || requestedTabID == "" {
			return "", "", "", errors.New("tab_id and chat_id must be supplied together")
		}
		if !m.ValidateAgentOwner(ownerKey, requestedChatID, requestedTabID) {
			return "", "", "", errors.New("no running Workass turn owns this external work request")
		}
		chatID, tabID = requestedChatID, requestedTabID
	}
	providerID := m.providerForSpawnedWorkChat(tabID, chatID)
	if !m.acceptsExternalWorkProvider(providerID) {
		return "", "", "", errors.New("external work registration is unavailable for this Workass ACP provider")
	}
	return tabID, chatID, providerID, nil
}

func (m *Manager) RegisterExternalWork(opts ExternalWorkRegistrationOptions) (map[string]any, error) {
	if err := m.beginWorkAdmission(); err != nil {
		return nil, err
	}
	defer m.endWorkAdmission()
	tabID, chatID, providerID, err := m.resolveExternalWorkOwner(opts.OwnerKey, opts.ParentChatID, opts.ParentTabID, opts.ChatID, opts.TabID)
	if err != nil {
		return nil, err
	}
	label := compactText(redactSensitiveText(opts.Label), 240)
	if label == "" {
		return nil, errors.New("external work label is required")
	}
	if opts.PID != nil && *opts.PID <= 1 {
		return nil, errors.New("external work pid must be greater than 1")
	}

	workID := ""
	for i := 0; i < 8; i++ {
		workID, err = randomExternalWorkID()
		if err != nil {
			return nil, err
		}
		m.spawnedWorkMu.Lock()
		_, exists := m.spawnedWork[spawnedWorkKey(tabID, chatID, workID)]
		m.spawnedWorkMu.Unlock()
		if !exists {
			break
		}
		workID = ""
	}
	if workID == "" {
		return nil, errors.New("external work id collision")
	}

	outputFile := strings.TrimSpace(opts.OutputFile)
	if outputFile == "" {
		dir := m.externalWorkDir()
		if dir == "" {
			return nil, errors.New("external work state directory is unavailable")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		outputFile = filepath.Join(dir, workID+".output")
		f, err := os.OpenFile(outputFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
	} else {
		if safe, ok := validateExternalWorkPath(m.opts.StateDir, outputFile); ok {
			outputFile = safe
		} else {
			return nil, errors.New("external work output_file is not an allowed absolute path")
		}
	}

	doneFile := strings.TrimSpace(opts.DoneFile)
	if doneFile == "" {
		doneFile = outputFile + ".done"
	} else if safe, ok := validateExternalWorkPath(m.opts.StateDir, doneFile); ok {
		doneFile = safe
	} else {
		return nil, errors.New("external work done_file is not an allowed absolute path")
	}
	if safe, ok := validateExternalWorkPath(m.opts.StateDir, doneFile); ok {
		doneFile = safe
	} else {
		return nil, errors.New("external work done_file is not an allowed absolute path")
	}

	now := time.Now().UTC()
	var pid *int
	if opts.PID != nil {
		value := *opts.PID
		pid = &value
	}
	item := SpawnedWorkItem{
		ID: workID, TaskID: workID, TabID: tabID, ChatID: chatID, ProviderID: providerID,
		Kind: "external", Label: label, Role: normalizeSpawnedWorkRole(opts.Role), Status: "running",
		StartedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
		OutputFile: outputFile, PID: pid,
	}
	rec := &spawnedWorkRecord{Item: item, ExternalDoneFile: doneFile, SawPID: opts.PID != nil}
	m.spawnedWorkMu.Lock()
	m.spawnedWork[spawnedWorkKey(tabID, chatID, workID)] = rec
	m.spawnedWorkMu.Unlock()
	m.commitSpawnedWorkChange(tabID, chatID)
	return map[string]any{"ok": true, "workId": workID, "taskId": workID, "outputFile": outputFile, "doneFile": doneFile}, nil
}

func (m *Manager) SettleExternalWork(opts ExternalWorkSettleOptions) (map[string]any, error) {
	tabID, chatID, _, err := m.resolveExternalWorkOwner(opts.OwnerKey, opts.ParentChatID, opts.ParentTabID, opts.ChatID, opts.TabID)
	if err != nil {
		return nil, err
	}
	workID := normalizeSpawnedWorkTaskID(opts.WorkID)
	if workID == "" {
		return nil, errors.New("external work_id is required")
	}
	status := spawnedWorkStatus(opts.Status)
	if status != "exited" && status != "failed" {
		return nil, errors.New("external work status must be exited or failed")
	}
	summary := compactText(redactSensitiveText(opts.Summary), 1000)
	key := spawnedWorkKey(tabID, chatID, workID)
	m.spawnedWorkMu.Lock()
	rec := m.spawnedWork[key]
	if rec == nil || rec.Item.Kind != "external" {
		m.spawnedWorkMu.Unlock()
		return nil, errors.New("external work item not found")
	}
	if rec.Item.Status != "running" {
		m.spawnedWorkMu.Unlock()
		return map[string]any{"ok": true, "already": true}, nil
	}
	changed := m.settleSpawnedWorkLocked(rec, status, opts.ExitCode)
	if changed && summary != "" {
		rec.Item.Summary = summary
		rec.Item.UpdatedAt = rec.Item.FinishedAt
	}
	m.spawnedWorkMu.Unlock()
	if changed {
		m.commitSpawnedWorkChange(tabID, chatID)
	}
	return map[string]any{"ok": true, "already": false, "workId": workID, "status": status}, nil
}

func (m *Manager) observeSpawnToolEvent(obs spawnToolObservation) {
	if !m.acceptsClaudeSpawnedWorkProvider(obs.ProviderID) || obs.ToolCallID == "" || obs.TabID == "" || obs.ChatID == "" {
		return
	}
	claudeMeta := mapFromAny(obs.Meta["claudeCode"])
	providerTool := firstNonEmpty(asString(claudeMeta["toolName"]), obs.Title)
	candidateKey := spawnedCandidateKey(obs.SessionID, obs.ToolCallID)
	if boolFromMap(obs.RawInput, "run_in_background") {
		label := compactText(firstNonEmpty(obs.Command, obs.Title, providerTool, "Background work"), 240)
		m.spawnedWorkMu.Lock()
		m.spawnedCandidates[candidateKey] = spawnedWorkCandidate{
			SessionID: obs.SessionID, TabID: obs.TabID, ChatID: obs.ChatID,
			ToolCallID: obs.ToolCallID, ProviderID: obs.ProviderID,
			ProviderTool: providerTool, Kind: spawnedWorkKind(providerTool, "", ""),
			Label: label, StartedAt: time.Now().UTC(),
		}
		m.spawnedWorkMu.Unlock()
	}
	match := claudeBackgroundResultRE.FindStringSubmatch(obs.Output)
	if len(match) != 3 {
		return
	}
	taskID := normalizeSpawnedWorkTaskID(match[1])
	if taskID == "" {
		return
	}
	outputFile := strings.TrimSpace(match[2])
	if safe, ok := validateClaudeTaskOutputPath(taskID, outputFile); ok {
		outputFile = safe
	} else {
		outputFile = ""
	}
	m.spawnedWorkMu.Lock()
	candidate := m.spawnedCandidates[candidateKey]
	if candidate.TabID == "" {
		candidate = spawnedWorkCandidate{
			SessionID: obs.SessionID, TabID: obs.TabID, ChatID: obs.ChatID,
			ToolCallID: obs.ToolCallID, ProviderID: obs.ProviderID,
			ProviderTool: providerTool, Kind: spawnedWorkKind(providerTool, "", ""),
			Label: compactText(firstNonEmpty(obs.Command, obs.Title, providerTool, taskID), 240), StartedAt: time.Now().UTC(),
		}
	}
	rec, changed := m.upsertSpawnedWorkLocked(candidate, taskID, "", "", "", outputFile)
	delete(m.spawnedCandidates, candidateKey)
	m.spawnedWorkMu.Unlock()
	if changed && rec != nil {
		m.commitSpawnedWorkChange(rec.Item.TabID, rec.Item.ChatID)
	}
}

func (m *Manager) observeClaudeSpawnedWork(tabID, chatID, sessionID string, event map[string]any) {
	if tabID == "" || chatID == "" || event == nil {
		return
	}
	now := time.Now().UTC()
	typeName := strings.TrimSpace(asString(event["type"]))
	if typeName == "snapshot" {
		live := map[string]map[string]any{}
		for _, raw := range anySliceValue(event["tasks"]) {
			task := mapFromAny(raw)
			if id := normalizeSpawnedWorkTaskID(asString(task["taskId"])); id != "" {
				live[id] = task
			}
		}
		changed := false
		m.spawnedWorkMu.Lock()
		for taskID, task := range live {
			candidate := m.candidateForTaskLocked(sessionID, asString(task["toolCallId"]), tabID, chatID)
			rec, itemChanged := m.upsertSpawnedWorkLocked(candidate, taskID, asString(task["description"]), asString(task["taskType"]), "", "")
			if rec != nil {
				rec.LastLevelSeen = now
				rec.MissingSince = time.Time{}
			}
			changed = changed || itemChanged
		}
		for _, rec := range m.spawnedWork {
			if rec.SessionID != sessionID || rec.Item.TabID != tabID || rec.Item.ChatID != chatID || rec.Item.Status != "running" {
				continue
			}
			if _, ok := live[rec.Item.TaskID]; !ok && rec.MissingSince.IsZero() {
				rec.MissingSince = now
			}
		}
		m.spawnedWorkMu.Unlock()
		if changed {
			m.commitSpawnedWorkChange(tabID, chatID)
		}
		return
	}

	taskID := normalizeSpawnedWorkTaskID(asString(event["taskId"]))
	if taskID == "" {
		return
	}
	toolCallID := strings.TrimSpace(asString(event["toolCallId"]))
	patch := mapFromAny(event["patch"])
	m.spawnedWorkMu.Lock()
	candidate := m.candidateForTaskLocked(sessionID, toolCallID, tabID, chatID)
	rec, changed := m.upsertSpawnedWorkLocked(candidate, taskID, firstNonEmpty(asString(event["description"]), asString(patch["description"])), asString(event["taskType"]), asString(event["subagentType"]), asString(event["outputFile"]))
	if rec != nil {
		rec.LastLevelSeen = now
		if typeName == "started" || typeName == "progress" || spawnedWorkStatus(firstNonEmpty(asString(event["status"]), asString(patch["status"]))) == "running" {
			rec.MissingSince = time.Time{}
		}
		if summary := compactText(firstNonEmpty(asString(event["summary"]), asString(patch["error"])), 1000); summary != "" && rec.Item.Summary != summary {
			rec.Item.Summary = summary
			rec.Item.UpdatedAt = isoNow()
			changed = true
		}
		if tool := compactText(asString(event["lastToolName"]), 120); tool != "" && rec.Item.LastToolName != tool {
			rec.Item.LastToolName = tool
			rec.Item.UpdatedAt = isoNow()
			changed = true
		}
		status := firstNonEmpty(asString(event["status"]), asString(patch["status"]))
		if typeName == "notification" || status != "" {
			mapped := spawnedWorkStatus(status)
			if mapped != "running" && rec.Item.Status == "running" {
				changed = m.settleSpawnedWorkLocked(rec, mapped, nil) || changed
			}
		}
	}
	if toolCallID != "" && typeName == "started" {
		delete(m.spawnedCandidates, spawnedCandidateKey(sessionID, toolCallID))
	}
	m.spawnedWorkMu.Unlock()
	if changed && rec != nil {
		m.commitSpawnedWorkChange(tabID, chatID)
	}
}

func anySliceValue(v any) []any {
	items, _ := v.([]any)
	return items
}

func (m *Manager) candidateForTaskLocked(sessionID, toolCallID, tabID, chatID string) spawnedWorkCandidate {
	if toolCallID != "" {
		if candidate, ok := m.spawnedCandidates[spawnedCandidateKey(sessionID, toolCallID)]; ok {
			return candidate
		}
	}
	return spawnedWorkCandidate{
		SessionID: sessionID, TabID: tabID, ChatID: chatID, ToolCallID: toolCallID,
		ProviderID: "claude", StartedAt: time.Now().UTC(),
	}
}

func (m *Manager) upsertSpawnedWorkLocked(candidate spawnedWorkCandidate, taskID, description, taskType, subagentType, outputFile string) (*spawnedWorkRecord, bool) {
	taskID = normalizeSpawnedWorkTaskID(taskID)
	if candidate.TabID == "" || candidate.ChatID == "" || taskID == "" {
		return nil, false
	}
	key := spawnedWorkKey(candidate.TabID, candidate.ChatID, taskID)
	rec := m.spawnedWork[key]
	changed := false
	if rec != nil && !sameSpawnedWorkToolCall(rec.Item.ToolCallID, candidate.ToolCallID) {
		// A prose fallback may enrich only the structured record belonging to
		// the same tool call. Never let a task-id collision rewrite another
		// tool's authoritative lifecycle.
		return rec, false
	}
	if rec == nil {
		started := candidate.StartedAt
		if started.IsZero() {
			started = time.Now().UTC()
		}
		label := compactText(firstNonEmpty(description, candidate.Label, taskID), 240)
		item := SpawnedWorkItem{
			ID: taskID, TaskID: taskID, ToolCallID: candidate.ToolCallID,
			TabID: candidate.TabID, ChatID: candidate.ChatID,
			ProviderID: firstNonEmpty(candidate.ProviderID, "claude"),
			Kind:       spawnedWorkKind(candidate.ProviderTool, firstNonEmpty(taskType, candidate.Kind), subagentType),
			Label:      label, Status: "running",
			StartedAt: started.Format(time.RFC3339Nano), UpdatedAt: isoNow(),
		}
		rec = &spawnedWorkRecord{Item: item, SessionID: candidate.SessionID}
		m.spawnedWork[key] = rec
		changed = true
	}
	if rec.SessionID == "" {
		rec.SessionID = candidate.SessionID
	}
	if rec.Item.ToolCallID == "" && candidate.ToolCallID != "" {
		rec.Item.ToolCallID = candidate.ToolCallID
		changed = true
	}
	if label := compactText(description, 240); label != "" && label != rec.Item.Label {
		rec.Item.Label = label
		changed = true
	}
	kind := spawnedWorkKind(candidate.ProviderTool, firstNonEmpty(taskType, candidate.Kind), subagentType)
	if rec.Item.Kind == "background" && kind != "background" {
		rec.Item.Kind = kind
		changed = true
	}
	if outputFile != "" {
		if safe, ok := validateClaudeTaskOutputPath(taskID, outputFile); ok && safe != rec.Item.OutputFile {
			rec.Item.OutputFile = safe
			changed = true
		}
	}
	if changed {
		rec.Item.UpdatedAt = isoNow()
	}
	return rec, changed
}

func spawnedWorkStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "error", "killed":
		return "failed"
	case "orphaned":
		return "orphaned"
	case "completed", "stopped", "exited", "done":
		return "exited"
	default:
		return "running"
	}
}

func (m *Manager) settleSpawnedWorkLocked(rec *spawnedWorkRecord, status string, exitCode *int) bool {
	if rec == nil || rec.Item.Status != "running" {
		return false
	}
	switch status {
	case "failed", "orphaned":
	default:
		status = "exited"
	}
	rec.Item.Status = status
	rec.Item.FinishedAt = isoNow()
	rec.Item.UpdatedAt = rec.Item.FinishedAt
	rec.Item.ExitCode = exitCode
	return true
}

func (m *Manager) registerSubagentSpawnedWork(tabID, chatID string, run SubagentRun) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" || strings.TrimSpace(run.ID) == "" {
		return
	}
	startedAt := strings.TrimSpace(run.StartedAt)
	if startedAt == "" {
		startedAt = isoNow()
	}
	updatedAt := firstNonEmpty(strings.TrimSpace(run.LastActivityAt), startedAt)
	item := SpawnedWorkItem{
		ID: run.ID, TaskID: run.ID, TabID: tabID, ChatID: chatID,
		ProviderID: strings.TrimSpace(run.ProviderID), Kind: trackedSubagentSpawnedWorkKind,
		Label: compactText(redactSensitiveText(run.Label), 240), Status: "running",
		StartedAt: startedAt, UpdatedAt: updatedAt,
		Summary:      compactText(redactSensitiveText(run.LatestActivity), 1000),
		LastToolName: compactText(redactSensitiveText(run.Phase), 120),
	}
	m.spawnedWorkMu.Lock()
	m.spawnedWork[spawnedWorkKey(tabID, chatID, run.ID)] = &spawnedWorkRecord{Item: item}
	m.spawnedWorkMu.Unlock()
	m.commitSpawnedWorkChange(tabID, chatID)
}

func (m *Manager) updateSubagentSpawnedWork(tabID, chatID, id, phase, activity string) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	id = normalizeSpawnedWorkTaskID(id)
	if tabID == "" || chatID == "" || id == "" {
		return
	}
	summary := compactText(redactSensitiveText(activity), 1000)
	lastToolName := compactText(redactSensitiveText(phase), 120)
	changed := false
	m.spawnedWorkMu.Lock()
	rec := m.spawnedWork[spawnedWorkKey(tabID, chatID, id)]
	if rec != nil && rec.Item.Kind == trackedSubagentSpawnedWorkKind && rec.Item.Status == "running" {
		if rec.Item.Summary != summary {
			rec.Item.Summary = summary
			changed = true
		}
		if rec.Item.LastToolName != lastToolName {
			rec.Item.LastToolName = lastToolName
			changed = true
		}
		if changed {
			rec.Item.UpdatedAt = isoNow()
		}
	}
	m.spawnedWorkMu.Unlock()
	if changed {
		m.commitSpawnedWorkChange(tabID, chatID)
	}
}

func (m *Manager) settleSubagentSpawnedWork(tabID, chatID string, run SubagentRun) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	id := normalizeSpawnedWorkTaskID(run.ID)
	if tabID == "" || chatID == "" || id == "" {
		return
	}
	status := "exited"
	if run.Status == "failed" {
		status = "failed"
	}
	summary := compactText(redactSensitiveText(run.LatestActivity), 1000)
	lastToolName := compactText(redactSensitiveText(run.Phase), 120)
	modelLabel := compactText(redactSensitiveText(run.ModelLabel), 120)
	excerpt := compactText(redactSensitiveText(firstNonEmpty(run.Result, run.Error)), maxSpawnedWorkResultExcerpt)
	changed := false
	m.spawnedWorkMu.Lock()
	rec := m.spawnedWork[spawnedWorkKey(tabID, chatID, id)]
	if rec != nil && rec.Item.Kind == trackedSubagentSpawnedWorkKind {
		changed = m.settleSpawnedWorkLocked(rec, status, nil)
		if changed {
			rec.Item.Summary = summary
			rec.Item.LastToolName = lastToolName
			rec.Item.ModelLabel = modelLabel
			rec.Item.ResultExcerpt = excerpt
			rec.Item.UpdatedAt = rec.Item.FinishedAt
		}
	}
	m.spawnedWorkMu.Unlock()
	if changed {
		m.commitSpawnedWorkChange(tabID, chatID)
	}
}

func (m *Manager) hasRunningSpawnedWorkForChat(tabID, chatID string) bool {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return false
	}
	m.spawnedWorkMu.Lock()
	defer m.spawnedWorkMu.Unlock()
	for _, rec := range m.spawnedWork {
		if rec == nil || rec.Item.Status != "running" || rec.Item.TabID != tabID || rec.Item.ChatID != chatID {
			continue
		}
		// A service is alive but owes this chat nothing, so it must not pin an
		// engine awake. Left in, every chat that ever started a dev server would
		// hold its bridge resident forever — the expensive half of the bug this
		// classification exists to fix.
		if isServiceSpawnedWork(rec.Item) {
			continue
		}
		liveness := spawnedWorkLivenessClassFor(rec.Item)
		if liveness != spawnedWorkLivenessExternal && liveness != spawnedWorkLivenessSubagent {
			return true
		}
	}
	return false
}

func (m *Manager) orphanInProcessSpawnedWorkForChat(tabID, chatID, reason string) bool {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return false
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "engine-exit"
	}
	summary := "Orphaned: the ACP engine exited while this ran in-process (reason: " + reason + ")"
	changed := false
	m.spawnedWorkMu.Lock()
	for _, rec := range m.spawnedWork {
		if rec == nil || rec.Item.TabID != tabID || rec.Item.ChatID != chatID || rec.Item.Status != "running" {
			continue
		}
		if spawnedWorkLivenessClassFor(rec.Item) != spawnedWorkLivenessInProcess {
			continue
		}
		if m.settleSpawnedWorkLocked(rec, "orphaned", nil) {
			rec.Item.Summary = summary
			rec.Item.UpdatedAt = rec.Item.FinishedAt
			changed = true
		}
	}
	m.spawnedWorkMu.Unlock()
	if changed {
		m.commitSpawnedWorkChange(tabID, chatID)
	}
	return changed
}

func (m *Manager) spawnedWorkLoop() {
	ticker := time.NewTicker(m.opts.SpawnedWorkReconcileInterval)
	defer ticker.Stop()
	for range ticker.C {
		m.reconcileSpawnedWork()
	}
}

func (m *Manager) reconcileSpawnedWork() {
	now := time.Now().UTC()
	type probeItem struct {
		key, path, tabID, chatID, doneFile string
		liveness                           spawnedWorkLivenessClass
	}
	items := []probeItem{}
	m.spawnedWorkMu.Lock()
	for key, rec := range m.spawnedWork {
		if rec.Item.Status == "running" {
			liveness := spawnedWorkLivenessClassFor(rec.Item)
			if liveness == spawnedWorkLivenessSubagent {
				// The in-memory subagent registry is the sole live authority.
				// finishSubagent/cancel settles this record; reconciliation must
				// never infer its death from a quiet owning chat.
				continue
			}
			items = append(items, probeItem{
				key: key, path: rec.Item.OutputFile, tabID: rec.Item.TabID, chatID: rec.Item.ChatID,
				doneFile: rec.ExternalDoneFile, liveness: liveness,
			})
		}
	}
	m.spawnedWorkMu.Unlock()
	// A pathless record loaded from disk has no live session owner. Resolve
	// foreground turn liveness outside the registry lock so a just-restored
	// turn can reclaim it before the orphan grace starts. Live-session records
	// are never settled from silence.
	chatTurnRunning := map[string]bool{}
	for _, item := range items {
		if item.liveness == spawnedWorkLivenessExternal {
			continue
		}
		if item.path != "" || item.tabID == "" || item.chatID == "" {
			continue
		}
		key := item.tabID + "\x00" + item.chatID
		if _, seen := chatTurnRunning[key]; seen {
			continue
		}
		_, running := m.RunningJobForChat(item.tabID, item.chatID)
		chatTurnRunning[key] = running
	}
	// External lanes are probed too, for classification only. Their liveness is
	// still decided solely by done-file/PID above — that branch never reads
	// pidsByPath, so an absent path here can never settle one. Leaving them out
	// made the registered `expo start` lane, the case service classification
	// exists for, permanently unclassifiable: no probed path, no lane PIDs, no
	// candidate.
	paths := make([]string, 0, len(items))
	seenPaths := map[string]struct{}{}
	for _, item := range items {
		if item.path == "" {
			continue
		}
		if _, seen := seenPaths[item.path]; seen {
			continue
		}
		seenPaths[item.path] = struct{}{}
		paths = append(paths, item.path)
	}
	pidsByPath, probeSupported := map[string][]int{}, false
	if len(paths) > 0 {
		pidsByPath, probeSupported = m.opts.SpawnedWorkPIDProbe(paths)
	}
	for _, probe := range items {
		if probe.liveness == spawnedWorkLivenessExternal {
			changed := false
			var tabID, chatID string
			done, exitCode := readExternalDoneFile(m.opts.StateDir, probe.doneFile)
			m.spawnedWorkMu.Lock()
			rec := m.spawnedWork[probe.key]
			if rec != nil && rec.Item.Status == "running" && rec.Item.Kind == "external" {
				tabID, chatID = rec.Item.TabID, rec.Item.ChatID
				switch {
				case done:
					status := "exited"
					summary := "Done marker written (exit unknown)"
					if exitCode != nil {
						summary = fmt.Sprintf("Done marker written (exit %d)", *exitCode)
						if *exitCode != 0 {
							status = "failed"
						}
					}
					changed = m.settleSpawnedWorkLocked(rec, status, exitCode)
					if changed {
						rec.Item.Summary = summary
						rec.Item.UpdatedAt = rec.Item.FinishedAt
					}
				case rec.Item.PID != nil && !externalPIDAlive(*rec.Item.PID):
					if rec.MissingSince.IsZero() {
						rec.MissingSince = now
					}
					if now.Sub(rec.MissingSince) >= externalWorkMissingGrace {
						changed = m.settleSpawnedWorkLocked(rec, "exited", nil)
						if changed {
							rec.Item.Summary = "Process exited without a done marker"
							rec.Item.UpdatedAt = rec.Item.FinishedAt
						}
					}
				default:
					rec.MissingSince = time.Time{}
				}
			}
			m.spawnedWorkMu.Unlock()
			if changed {
				m.commitSpawnedWorkChange(tabID, chatID)
			}
			continue
		}
		pids := pidsByPath[probe.path]
		supported := probe.path != "" && probeSupported
		changed := false
		var tabID, chatID string
		m.spawnedWorkMu.Lock()
		rec := m.spawnedWork[probe.key]
		if rec != nil && rec.Item.Status == "running" {
			tabID, chatID = rec.Item.TabID, rec.Item.ChatID
			if len(pids) > 0 {
				pid := pids[len(pids)-1]
				if rec.Item.PID == nil || *rec.Item.PID != pid {
					rec.Item.PID = &pid
					rec.Item.UpdatedAt = isoNow()
					changed = true
				}
				rec.SawPID = true
				rec.MissingSince = time.Time{}
			} else if probe.path == "" && rec.Item.PID == nil {
				// A level-snapshot absence keeps its authoritative short grace.
				if !rec.MissingSince.IsZero() && now.Sub(rec.MissingSince) >= spawnedWorkMissingGrace {
					changed = m.settleSpawnedWorkLocked(rec, "exited", nil)
				} else if turnKey := rec.Item.TabID + "\x00" + rec.Item.ChatID; rec.SessionID != "" || chatTurnRunning[turnKey] {
					rec.PathlessSince = time.Time{}
				} else {
					if rec.PathlessSince.IsZero() {
						rec.PathlessSince = now
					}
					if now.Sub(rec.PathlessSince) >= spawnedWorkPathlessGrace {
						changed = m.settleSpawnedWorkLocked(rec, "orphaned", nil)
						if changed && rec.Item.Summary == "" {
							rec.Item.Summary = "Orphaned by reconciliation: no live ACP session owns this task, and no output file or PID can verify it."
						}
					}
				}
			} else {
				if supported && rec.MissingSince.IsZero() {
					rec.MissingSince = now
				}
				if !rec.MissingSince.IsZero() && now.Sub(rec.MissingSince) >= spawnedWorkMissingGrace {
					changed = m.settleSpawnedWorkLocked(rec, "exited", nil)
				}
			}
		}
		m.spawnedWorkMu.Unlock()
		if changed {
			m.commitSpawnedWorkChange(tabID, chatID)
		}
	}
	m.classifySpawnedWorkServices(now, pidsByPath)
	// After classification, so a row promoted to "service" on this tick is
	// already excluded from the evidence a park would otherwise rest on.
	m.sweepStalledObligations(now)
}

// classifySpawnedWorkServices decides which running records have stopped being
// work and become services. It runs after the settle pass so it only ever
// judges rows this tick still calls running.
//
// The verdict needs two agreeing signals, because each alone has a loud
// counterexample: a listening TCP socket (a build lane never binds a port, a
// dev server always does) AND an output file that stopped growing (a server
// goes quiet once booted; a test run holding a listener does not). Promotion is
// one-way. A server that logs a request would otherwise flap back to "working"
// on every page load, and having once bound a port and gone quiet is not
// something a build lane does by accident.
//
// The socket question is asked of every process holding the lane's output file,
// not only the one PID on the record. `npx expo start` is the reason: the
// wrapper that owns the record binds nothing, and the node child that inherited
// its stdout is the one actually listening.
func (m *Manager) classifySpawnedWorkServices(now time.Time, pidsByPath map[string][]int) {
	if m.opts.SpawnedWorkListenProbe == nil {
		return
	}
	type candidate struct {
		key, path string
		pids      []int
	}
	candidates := []candidate{}
	m.spawnedWorkMu.Lock()
	for key, rec := range m.spawnedWork {
		// Any declared role, work or service, ends the question. Inference only
		// ever fills a silence.
		if rec == nil || rec.Item.Status != "running" || rec.Item.Role != "" {
			continue
		}
		// A tracked subagent is owned by the subagent registry and is work by
		// construction, so it is never a classification candidate.
		if spawnedWorkLivenessClassFor(rec.Item) == spawnedWorkLivenessSubagent {
			continue
		}
		// Quiescence is half the evidence, so a record with no output file to
		// measure stays work unless its spawner declared otherwise.
		if rec.Item.OutputFile == "" {
			rec.ListeningQuietSince = time.Time{}
			continue
		}
		lane := append([]int(nil), pidsByPath[rec.Item.OutputFile]...)
		if rec.Item.PID != nil {
			lane = append(lane, *rec.Item.PID)
		}
		if len(lane) == 0 {
			rec.ListeningQuietSince = time.Time{}
			continue
		}
		candidates = append(candidates, candidate{key: key, path: rec.Item.OutputFile, pids: lane})
	}
	m.spawnedWorkMu.Unlock()
	if len(candidates) == 0 {
		return
	}

	pids := []int{}
	seenPID := map[int]struct{}{}
	for _, item := range candidates {
		for _, pid := range item.pids {
			if _, dup := seenPID[pid]; dup {
				continue
			}
			seenPID[pid] = struct{}{}
			pids = append(pids, pid)
		}
	}
	listening, ok := m.opts.SpawnedWorkListenProbe(pids)
	if !ok {
		// An unsupported or failed probe is not evidence of anything. Leave every
		// dwell clock exactly where it was and try again next tick.
		return
	}
	sizes := make(map[string]int64, len(candidates))
	for _, item := range candidates {
		if _, done := sizes[item.path]; done {
			continue
		}
		if info, err := os.Stat(item.path); err == nil {
			sizes[item.path] = info.Size()
		} else {
			sizes[item.path] = -1
		}
	}

	promoted := map[[2]string]struct{}{}
	m.spawnedWorkMu.Lock()
	for _, item := range candidates {
		rec := m.spawnedWork[item.key]
		if rec == nil || rec.Item.Status != "running" || rec.Item.Role != "" {
			continue
		}
		size := sizes[item.path]
		// An unreadable size and a first observation both count as "grew": the
		// clock may only start from a size this pass can compare against one it
		// actually saw, so quiet is never inferred from a single sample.
		quiet := size >= 0 && rec.SawOutputSize && size == rec.LastOutputSize
		rec.LastOutputSize, rec.SawOutputSize = size, size >= 0
		bound := false
		for _, pid := range item.pids {
			if listening[pid] {
				bound = true
				break
			}
		}
		if !bound || !quiet {
			rec.ListeningQuietSince = time.Time{}
			continue
		}
		if rec.ListeningQuietSince.IsZero() {
			rec.ListeningQuietSince = now
			continue
		}
		if now.Sub(rec.ListeningQuietSince) < serviceClassifyDwell {
			continue
		}
		rec.Item.Role = spawnedWorkRoleService
		rec.Item.UpdatedAt = isoNow()
		promoted[[2]string{rec.Item.TabID, rec.Item.ChatID}] = struct{}{}
	}
	m.spawnedWorkMu.Unlock()
	for pair := range promoted {
		m.commitSpawnedWorkChange(pair[0], pair[1])
	}
}

func (m *Manager) commitSpawnedWorkChange(tabID, chatID string) {
	if tabID == "" || chatID == "" {
		return
	}
	m.persistSpawnedWorkSnapshot(tabID, chatID)
	m.persistNewSpawnedWorkReceipts(tabID, chatID)
	m.touchSpawnedWorkBridgeActivity(tabID, chatID, time.Now())
	payload := map[string]any{
		"tabId": tabID, "chatId": chatID, "items": m.ListSpawnedWork(tabID, chatID),
	}
	// Additive, and on this channel deliberately rather than a new one: the
	// obligation is derived from exactly the background state this event
	// already reports, and both change on the same reconcile tick.
	if obligation := m.ObligationFor(tabID, chatID); obligation != nil {
		payload["obligation"] = obligation
	}
	m.emit("spawned-work:changed", payload)
}

func (m *Manager) touchSpawnedWorkBridgeActivity(tabID, chatID string, now time.Time) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" || now.IsZero() {
		return
	}
	for _, bridge := range m.allBridges() {
		bridge.mu.Lock()
		if strings.TrimSpace(bridge.tabID) == tabID && strings.TrimSpace(bridge.chatID) == chatID {
			bridge.lastActivity = now
		}
		bridge.mu.Unlock()
	}
}

func (m *Manager) ListSpawnedWork(tabID, chatID string) []SpawnedWorkItem {
	return publicSpawnedWorkItems(m.listSpawnedWorkRaw(tabID, chatID))
}

func (m *Manager) listSpawnedWorkRaw(tabID, chatID string) []SpawnedWorkItem {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return []SpawnedWorkItem{}
	}
	m.spawnedWorkMu.Lock()
	out := make([]SpawnedWorkItem, 0)
	for _, rec := range m.spawnedWork {
		if rec.Item.TabID == tabID && rec.Item.ChatID == chatID {
			out = append(out, rec.Item)
		}
	}
	m.spawnedWorkMu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Status == "running") != (out[j].Status == "running") {
			return out[i].Status == "running"
		}
		return out[i].StartedAt > out[j].StartedAt
	})
	return capSpawnedWorkItemsPreservingRunning(out)
}

func publicSpawnedWorkItems(items []SpawnedWorkItem) []SpawnedWorkItem {
	out := make([]SpawnedWorkItem, len(items))
	for i, item := range items {
		out[i] = publicSpawnedWorkItem(item)
	}
	return out
}

func publicSpawnedWorkItem(item SpawnedWorkItem) SpawnedWorkItem {
	item.Label = redactSensitiveText(item.Label)
	item.OutputFile = redactSensitiveText(item.OutputFile)
	item.Summary = redactSensitiveText(item.Summary)
	item.ResultExcerpt = redactSensitiveText(item.ResultExcerpt)
	return item
}

func (m *Manager) ReadSpawnedWork(tabID, chatID, id string, tailBytes int) map[string]any {
	items := m.listSpawnedWorkRaw(tabID, chatID)
	for _, item := range items {
		if item.ID != id && item.TaskID != id {
			continue
		}
		if tailBytes <= 0 || tailBytes > maxSpawnedWorkTailBytes {
			tailBytes = defaultSpawnedWorkTailBytes
		}
		tail, limited := readSpawnedWorkTailForItem(m.opts.StateDir, item, tailBytes)
		return map[string]any{"ok": true, "item": publicSpawnedWorkItem(item), "tail": tail, "tailLimited": limited}
	}
	return map[string]any{"ok": false, "error": "spawned work item not found"}
}

// StopSpawnedWork ends one running row because a human asked it to, and is the
// whole contract behind the stop square: after it answers ok, the row is not
// running and neither is what it was running.
//
// Three shapes of row, three answers:
//   - A tracked subagent owns no process of its own — the in-memory registry is
//     its life — so it is cancelled through the same path a coordinator uses,
//     and that path settles the record.
//   - A row with processes gets SIGTERM, a bounded grace, then SIGKILL for
//     whatever ignored it. The pid set is the record's own PID plus every
//     process holding its output file open: the same probe liveness already
//     trusts, and for `npx expo start` the difference between killing the
//     wrapper and killing the node that actually holds the port.
//   - A row with no live process left is the case this button most needs to
//     answer. A lane whose process died without writing its done-file stays
//     "running" forever — the exact ghost the user is looking at — so with
//     nothing to signal, the stop IS the settle.
//
// Settled as "exited", never "failed": a stop is an outcome the user chose, and
// this card carries no failure wording (user, 2026-07-25).
func (m *Manager) StopSpawnedWork(tabID, chatID, id string) map[string]any {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	id = normalizeSpawnedWorkTaskID(id)
	if tabID == "" || chatID == "" || id == "" {
		return map[string]any{"ok": false, "error": "spawned-work:stop requires {tabId, chatId, id}"}
	}
	key, item, found := "", SpawnedWorkItem{}, false
	m.spawnedWorkMu.Lock()
	for candidate, rec := range m.spawnedWork {
		if rec.Item.TabID != tabID || rec.Item.ChatID != chatID {
			continue
		}
		if rec.Item.ID != id && normalizeSpawnedWorkTaskID(rec.Item.TaskID) != id {
			continue
		}
		key, item, found = candidate, rec.Item, true
		break
	}
	m.spawnedWorkMu.Unlock()
	if !found {
		return map[string]any{"ok": false, "error": "spawned work item not found"}
	}
	if item.Status != "running" {
		// Idempotent on purpose: a second tap, a stale row on a slow client, and
		// a lane that settled itself between render and click all arrive here,
		// and none of them is something the user did wrong.
		return map[string]any{"ok": true, "id": item.ID, "status": item.Status, "alreadyFinished": true}
	}
	if spawnedWorkLivenessClassFor(item) == spawnedWorkLivenessSubagent {
		// Owner-authenticated cancellation is for one agent addressing another's
		// child. This is the human at the controller, whose authority the wire's
		// controller lease already established before the invoke got here.
		if !m.cancelSubagentByID(item.ID, "") {
			return map[string]any{"ok": false, "error": "spawned work item is no longer cancellable"}
		}
		return map[string]any{"ok": true, "id": item.ID, "status": "cancelling", "cancelled": true}
	}
	signalled, forced := m.terminateSpawnedWorkPIDs(m.spawnedWorkStopPIDs(item))
	summary := "Stopped on request: no live process remained"
	if len(signalled) > 0 {
		summary = fmt.Sprintf("Stopped on request (SIGTERM ×%d)", len(signalled))
		if len(forced) > 0 {
			summary = fmt.Sprintf("Stopped on request (SIGTERM ×%d, SIGKILL ×%d)", len(signalled), len(forced))
		}
	}
	changed := false
	m.spawnedWorkMu.Lock()
	if rec := m.spawnedWork[key]; rec != nil {
		changed = m.settleSpawnedWorkLocked(rec, "exited", nil)
		if changed {
			rec.Item.Summary = summary
			rec.Item.UpdatedAt = rec.Item.FinishedAt
		}
		item = rec.Item
	}
	m.spawnedWorkMu.Unlock()
	if changed {
		m.commitSpawnedWorkChange(tabID, chatID)
	}
	return map[string]any{
		"ok": true, "id": item.ID, "status": item.Status, "stopped": true,
		"signalled": signalled, "forced": forced, "summary": summary,
	}
}

// spawnedWorkStopPIDs is the blast radius of one stop, and deliberately no
// wider: the processes holding this row's own output file, plus the pid the
// record carries. Never a process group — the daemon spawns lanes from its own
// session, so negating a pid could reach the daemon itself. Self and parent are
// excluded for the same reason, since a probe that catches the daemon mid-read
// of that file would otherwise hand this function its own pid.
func (m *Manager) spawnedWorkStopPIDs(item SpawnedWorkItem) []int {
	seen := map[int]struct{}{}
	out := []int{}
	add := func(pid int) {
		if pid <= 1 || pid == os.Getpid() || pid == os.Getppid() {
			return
		}
		if _, dup := seen[pid]; dup {
			return
		}
		seen[pid] = struct{}{}
		out = append(out, pid)
	}
	if item.OutputFile != "" && m.opts.SpawnedWorkPIDProbe != nil {
		if byPath, supported := m.opts.SpawnedWorkPIDProbe([]string{item.OutputFile}); supported {
			for _, pid := range byPath[item.OutputFile] {
				add(pid)
			}
		}
	}
	if item.PID != nil {
		add(*item.PID)
	}
	return out
}

func (m *Manager) terminateSpawnedWorkPIDs(pids []int) (signalled []int, forced []int) {
	signal := m.opts.SpawnedWorkSignal
	if signal == nil || len(pids) == 0 {
		return []int{}, []int{}
	}
	signalled, forced = []int{}, []int{}
	for _, pid := range pids {
		if signal(pid, false) {
			signalled = append(signalled, pid)
		}
	}
	if len(signalled) == 0 {
		return signalled, forced
	}
	survivors := append([]int(nil), signalled...)
	deadline := time.Now().Add(spawnedWorkStopGrace)
	for {
		alive := survivors[:0]
		for _, pid := range survivors {
			if externalPIDAlive(pid) {
				alive = append(alive, pid)
			}
		}
		survivors = alive
		if len(survivors) == 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(spawnedWorkStopPoll)
	}
	for _, pid := range survivors {
		if signal(pid, true) {
			forced = append(forced, pid)
		}
	}
	return signalled, forced
}

func validateClaudeTaskOutputPath(taskID, raw string) (string, bool) {
	taskID = normalizeSpawnedWorkTaskID(taskID)
	if taskID == "" || strings.ContainsAny(taskID, `/\\`) {
		return "", false
	}
	path := filepath.Clean(strings.TrimSpace(raw))
	if !filepath.IsAbs(path) || filepath.Base(path) != taskID+".output" || filepath.Base(filepath.Dir(path)) != "tasks" {
		return "", false
	}
	allowedRoots := []string{os.TempDir()}
	if filepath.Separator == '/' {
		allowedRoots = append(allowedRoots, "/private/tmp", "/tmp")
	}
	allowed := false
	for _, root := range allowedRoots {
		rel, err := filepath.Rel(filepath.Clean(root), path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", false
	}
	hasClaudeDir := false
	for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if strings.HasPrefix(strings.ToLower(filepath.Base(dir)), "claude-") {
			hasClaudeDir = true
			break
		}
	}
	if !hasClaudeDir {
		return "", false
	}
	return path, true
}

func validateExternalWorkPath(stateDir, raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	path := filepath.Clean(raw)
	if path != raw || !filepath.IsAbs(path) {
		return "", false
	}
	base := filepath.Base(path)
	if base == "" || base == "." || base == string(filepath.Separator) || strings.ContainsAny(base, `/\\`) {
		return "", false
	}
	allowedRoots := []string{os.TempDir()}
	if filepath.Separator == '/' {
		allowedRoots = append(allowedRoots, "/tmp", "/private/tmp")
	}
	if root := externalWorkRoot(stateDir); root != "" {
		allowedRoots = append(allowedRoots, root)
	}
	allowed := false
	for _, root := range allowedRoots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", false
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", false
		}
	} else if !os.IsNotExist(err) {
		return "", false
	}
	return path, true
}

func readSpawnedWorkTail(taskID, path string, limit int) (string, bool) {
	path, ok := validateClaudeTaskOutputPath(taskID, path)
	if !ok || limit <= 0 {
		return "", false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	limited := info.Size() > int64(limit)
	if limited {
		_, _ = file.Seek(-int64(limit), 2)
	}
	data, _ := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if len(data) > limit {
		data = data[len(data)-limit:]
		limited = true
	}
	text := strings.ReplaceAll(string(data), "\x00", "")
	return redactSensitiveText(text), limited
}

func readExternalWorkTail(stateDir, path string, limit int) (string, bool) {
	path, ok := validateExternalWorkPath(stateDir, path)
	if !ok || limit <= 0 {
		return "", false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	limited := info.Size() > int64(limit)
	if limited {
		_, _ = file.Seek(-int64(limit), 2)
	}
	data, _ := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if len(data) > limit {
		data = data[len(data)-limit:]
		limited = true
	}
	text := strings.ReplaceAll(string(data), "\x00", "")
	return redactSensitiveText(text), limited
}

func readSpawnedWorkTailForItem(stateDir string, item SpawnedWorkItem, limit int) (string, bool) {
	if item.Kind == "external" {
		return readExternalWorkTail(stateDir, item.OutputFile, limit)
	}
	return readSpawnedWorkTail(item.TaskID, item.OutputFile, limit)
}

func readExternalDoneFile(stateDir, path string) (bool, *int) {
	path, ok := validateExternalWorkPath(stateDir, path)
	if !ok {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, nil
	}
	if len(data) > 64 {
		data = data[:64]
	}
	line := string(data)
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	code, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return true, nil
	}
	return true, &code
}

func (m *Manager) spawnedWorkSnapshotPath(tabID string) string {
	if strings.TrimSpace(m.opts.StateDir) == "" || strings.TrimSpace(tabID) == "" {
		return ""
	}
	return filepath.Join(m.opts.StateDir, "spawned-work", safeArchiveName(tabID)+".json")
}

func (m *Manager) persistSpawnedWorkSnapshot(tabID, chatID string) {
	path := m.spawnedWorkSnapshotPath(tabID)
	if path == "" {
		return
	}
	items := m.listSpawnedWorkSnapshotItems(tabID, chatID)
	data, err := json.Marshal(items)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

func (m *Manager) listSpawnedWorkSnapshotItems(tabID, chatID string) []spawnedWorkSnapshotItem {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return []spawnedWorkSnapshotItem{}
	}
	m.spawnedWorkMu.Lock()
	out := make([]spawnedWorkSnapshotItem, 0)
	for _, rec := range m.spawnedWork {
		if rec.Item.TabID == tabID && rec.Item.ChatID == chatID {
			out = append(out, spawnedWorkSnapshotItem{SpawnedWorkItem: rec.Item, DoneFile: rec.ExternalDoneFile})
		}
	}
	m.spawnedWorkMu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if durableSpawnedWorkPriority(out[i].SpawnedWorkItem) != durableSpawnedWorkPriority(out[j].SpawnedWorkItem) {
			return durableSpawnedWorkPriority(out[i].SpawnedWorkItem)
		}
		return out[i].StartedAt > out[j].StartedAt
	})
	return capSpawnedWorkSnapshotItemsPreservingRunning(out)
}

func durableSpawnedWorkPriority(item SpawnedWorkItem) bool {
	return item.Status == "running"
}

func capSpawnedWorkItemsPreservingRunning(items []SpawnedWorkItem) []SpawnedWorkItem {
	if len(items) <= maxSpawnedWorkPerChat {
		return items
	}
	running := 0
	for _, item := range items {
		if item.Status == "running" {
			running++
		}
	}
	if running >= maxSpawnedWorkPerChat {
		return items[:running]
	}
	return items[:maxSpawnedWorkPerChat]
}

func capSpawnedWorkSnapshotItemsPreservingRunning(items []spawnedWorkSnapshotItem) []spawnedWorkSnapshotItem {
	if len(items) <= maxSpawnedWorkPerChat {
		return items
	}
	durable := 0
	for _, item := range items {
		if durableSpawnedWorkPriority(item.SpawnedWorkItem) {
			durable++
		}
	}
	if durable >= maxSpawnedWorkPerChat {
		return items[:durable]
	}
	return items[:maxSpawnedWorkPerChat]
}

func (m *Manager) loadSpawnedWorkSnapshots() {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return
	}
	paths, _ := filepath.Glob(filepath.Join(m.opts.StateDir, "spawned-work", "*.json"))
	healedPairs := map[[2]string]struct{}{}
	canonicalByKey := map[string]bool{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || len(data) > maxSpawnedWorkReceiptBytes {
			continue
		}
		var items []spawnedWorkSnapshotItem
		if json.Unmarshal(data, &items) != nil {
			continue
		}
		for _, stored := range items {
			item := stored.SpawnedWorkItem
			if item.TabID == "" || item.ChatID == "" || item.TaskID == "" {
				continue
			}
			rawTaskID := strings.TrimSpace(item.TaskID)
			taskID := normalizeSpawnedWorkTaskID(rawTaskID)
			if taskID == "" {
				continue
			}
			canonical := rawTaskID == taskID
			if !canonical || item.ID != taskID {
				healedPairs[[2]string{item.TabID, item.ChatID}] = struct{}{}
			}
			item.ID = taskID
			item.TaskID = taskID
			if item.OutputFile != "" {
				if item.Kind == "external" {
					if safe, ok := validateExternalWorkPath(m.opts.StateDir, item.OutputFile); ok {
						item.OutputFile = safe
					} else {
						item.OutputFile = ""
					}
				} else if safe, ok := validateClaudeTaskOutputPath(taskID, item.OutputFile); ok {
					item.OutputFile = safe
				} else {
					item.OutputFile = ""
				}
			}
			doneFile := strings.TrimSpace(stored.DoneFile)
			if item.Kind == "external" && doneFile == "" && item.OutputFile != "" {
				doneFile = item.OutputFile + ".done"
			}
			if doneFile != "" {
				if safe, ok := validateExternalWorkPath(m.opts.StateDir, doneFile); ok {
					doneFile = safe
				} else {
					doneFile = ""
					healedPairs[[2]string{item.TabID, item.ChatID}] = struct{}{}
				}
			}
			key := spawnedWorkKey(item.TabID, item.ChatID, taskID)
			incoming := &spawnedWorkRecord{Item: item, SawPID: item.PID != nil, ExternalDoneFile: doneFile}
			if existing := m.spawnedWork[key]; existing != nil {
				preferIncoming := canonical && !canonicalByKey[key]
				if !sameSpawnedWorkToolCall(existing.Item.ToolCallID, incoming.Item.ToolCallID) {
					if preferIncoming {
						m.spawnedWork[key] = incoming
						canonicalByKey[key] = true
					}
					healedPairs[[2]string{item.TabID, item.ChatID}] = struct{}{}
					continue
				}
				mergeLoadedSpawnedWorkRecord(existing, incoming, preferIncoming)
				canonicalByKey[key] = canonicalByKey[key] || canonical
				healedPairs[[2]string{item.TabID, item.ChatID}] = struct{}{}
				continue
			}
			m.spawnedWork[key] = incoming
			canonicalByKey[key] = canonical
		}
	}
	orphanedPairs := map[[2]string]struct{}{}
	for _, rec := range m.spawnedWork {
		if rec == nil || rec.Item.Status != "running" || spawnedWorkLivenessClassFor(rec.Item) != spawnedWorkLivenessSubagent {
			continue
		}
		if m.settleSpawnedWorkLocked(rec, "orphaned", nil) {
			rec.Item.Summary = "Orphaned after daemon restart: the tracked subagent process cannot survive daemon replacement."
			rec.Item.LastToolName = "orphaned"
			rec.Item.UpdatedAt = rec.Item.FinishedAt
			orphanedPairs[[2]string{rec.Item.TabID, rec.Item.ChatID}] = struct{}{}
		}
	}
	for pair := range healedPairs {
		if _, orphaned := orphanedPairs[pair]; orphaned {
			continue
		}
		m.persistSpawnedWorkSnapshot(pair[0], pair[1])
	}
	for pair := range orphanedPairs {
		// No subagent registry survives daemon replacement, so this is the only
		// point where silence becomes terminal for tracked subagent work. The
		// orphan wake is intentional: there is no surviving adoption path that
		// could double-notify this recovery event.
		m.commitSpawnedWorkChange(pair[0], pair[1])
	}
}

func mergeLoadedSpawnedWorkRecord(dst, src *spawnedWorkRecord, preferSrc bool) {
	if dst == nil || src == nil {
		return
	}
	if preferSrc {
		prior := dst.Item
		dst.Item = src.Item
		mergeMissingSpawnedWorkFields(&dst.Item, prior)
	} else {
		mergeMissingSpawnedWorkFields(&dst.Item, src.Item)
	}
	dst.SawPID = dst.SawPID || src.SawPID
	dst.ReceiptWritten = dst.ReceiptWritten || src.ReceiptWritten
	if dst.ExternalDoneFile == "" {
		dst.ExternalDoneFile = src.ExternalDoneFile
	}
}

func mergeMissingSpawnedWorkFields(dst *SpawnedWorkItem, src SpawnedWorkItem) {
	if dst == nil {
		return
	}
	if dst.ToolCallID == "" {
		dst.ToolCallID = src.ToolCallID
	}
	if dst.ProviderID == "" {
		dst.ProviderID = src.ProviderID
	}
	if dst.Kind == "" || (dst.Kind == "background" && src.Kind != "" && src.Kind != "background") {
		dst.Kind = src.Kind
	}
	if dst.Role == "" {
		dst.Role = src.Role
	}
	if dst.Label == "" {
		dst.Label = src.Label
	}
	if dst.Status == "" {
		dst.Status = src.Status
	}
	if dst.StartedAt == "" {
		dst.StartedAt = src.StartedAt
	}
	if dst.UpdatedAt == "" {
		dst.UpdatedAt = src.UpdatedAt
	}
	if dst.OutputFile == "" {
		dst.OutputFile = src.OutputFile
	}
	if dst.PID == nil {
		dst.PID = src.PID
	}
	if dst.ExitCode == nil {
		dst.ExitCode = src.ExitCode
	}
	if dst.Summary == "" {
		dst.Summary = src.Summary
	}
	if dst.LastToolName == "" {
		dst.LastToolName = src.LastToolName
	}
	if dst.ModelLabel == "" {
		dst.ModelLabel = src.ModelLabel
	}
	if dst.ResultExcerpt == "" {
		dst.ResultExcerpt = src.ResultExcerpt
	}
	if dst.Status != "running" && dst.FinishedAt == "" {
		dst.FinishedAt = src.FinishedAt
	}
}

func (m *Manager) spawnedWorkReceiptPath(tabID string) string {
	if strings.TrimSpace(m.opts.StateDir) == "" || strings.TrimSpace(tabID) == "" {
		return ""
	}
	return filepath.Join(m.opts.StateDir, "spawned-work-receipts", safeArchiveName(tabID)+".jsonl")
}

func (m *Manager) spawnedWorkReceiptFromItem(item SpawnedWorkItem) SpawnedWorkReceipt {
	started, _ := time.Parse(time.RFC3339Nano, item.StartedAt)
	finished, _ := time.Parse(time.RFC3339Nano, item.FinishedAt)
	tail, limited := readSpawnedWorkTailForItem(m.opts.StateDir, item, defaultSpawnedWorkTailBytes)
	publicItem := publicSpawnedWorkItem(item)
	elapsed := int64(0)
	if !started.IsZero() && !finished.IsZero() {
		elapsed = finished.Sub(started).Milliseconds()
	}
	return SpawnedWorkReceipt{
		ReceiptID: "spawned-" + item.TaskID, TaskID: item.TaskID, ToolCallID: item.ToolCallID,
		TabID: item.TabID, ChatID: item.ChatID,
		Kind: item.Kind, Label: publicItem.Label, Role: item.Role, Status: item.Status,
		StartedAt: item.StartedAt, FinishedAt: item.FinishedAt, ElapsedMs: elapsed,
		OutputFile: publicItem.OutputFile, PID: item.PID, ExitCode: item.ExitCode,
		Summary: publicItem.Summary, OutputTail: tail, TailLimited: limited,
	}
}

func (m *Manager) persistNewSpawnedWorkReceipts(tabID, chatID string) {
	path := m.spawnedWorkReceiptPath(tabID)
	if path == "" {
		return
	}
	type pendingReceipt struct {
		key  string
		item SpawnedWorkItem
	}
	m.spawnedWorkMu.Lock()
	terminal := make([]pendingReceipt, 0)
	for key, rec := range m.spawnedWork {
		if rec.Item.TabID == tabID && rec.Item.ChatID == chatID && rec.Item.Status != "running" && !rec.ReceiptWritten {
			terminal = append(terminal, pendingReceipt{key: key, item: rec.Item})
		}
	}
	m.spawnedWorkMu.Unlock()
	for _, pending := range terminal {
		receipt := m.spawnedWorkReceiptFromItem(pending.item)
		data, err := json.Marshal(receipt)
		if err != nil {
			continue
		}
		m.receiptMu.Lock()
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		existing, _ := os.ReadFile(path)
		lines := boundedSpawnedReceiptLines(existing)
		duplicate := false
		for _, line := range lines {
			var prior SpawnedWorkReceipt
			if json.Unmarshal(line, &prior) == nil && prior.ReceiptID == receipt.ReceiptID {
				duplicate = true
				break
			}
		}
		if !duplicate {
			lines = append(lines, data)
		}
		if len(lines) > maxSpawnedWorkPerChat {
			lines = lines[len(lines)-maxSpawnedWorkPerChat:]
		}
		payload := append(bytes.Join(lines, []byte("\n")), '\n')
		for len(payload) > maxSpawnedWorkReceiptBytes && len(lines) > 1 {
			lines = lines[1:]
			payload = append(bytes.Join(lines, []byte("\n")), '\n')
		}
		tmp := path + ".tmp"
		persisted := duplicate
		if os.WriteFile(tmp, payload, 0o600) == nil {
			persisted = os.Rename(tmp, path) == nil
		}
		m.receiptMu.Unlock()
		m.spawnedWorkMu.Lock()
		if rec := m.spawnedWork[pending.key]; persisted && rec != nil && rec.Item.Status != "running" {
			rec.ReceiptWritten = true
		}
		m.spawnedWorkMu.Unlock()
	}
}

func boundedSpawnedReceiptLines(data []byte) [][]byte {
	if len(data) > maxSpawnedWorkReceiptBytes {
		data = data[len(data)-maxSpawnedWorkReceiptBytes:]
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			data = data[idx+1:]
		}
	}
	lines := make([][]byte, 0, maxSpawnedWorkPerChat)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 16*1024), 128*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) > 0 && json.Valid(line) {
			lines = append(lines, append([]byte(nil), line...))
		}
	}
	return lines
}

func (m *Manager) ListSpawnedWorkReceipts(ownerKey, parentChatID, parentTabID, requestedChatID, requestedTabID string, limit int) ([]SpawnedWorkReceipt, error) {
	chatID, tabID, ok := m.ownerBinding(ownerKey, parentChatID, parentTabID)
	if !ok || chatID != strings.TrimSpace(requestedChatID) || tabID != strings.TrimSpace(requestedTabID) {
		return nil, fmt.Errorf("tab_id + chat_id must exactly match the owning Workass chat")
	}
	path := m.spawnedWorkReceiptPath(tabID)
	if path == "" {
		return []SpawnedWorkReceipt{}, nil
	}
	m.receiptMu.Lock()
	data, err := os.ReadFile(path)
	m.receiptMu.Unlock()
	if err != nil {
		return []SpawnedWorkReceipt{}, nil
	}
	if limit <= 0 || limit > maxSpawnedWorkPerChat {
		limit = 32
	}
	lines := boundedSpawnedReceiptLines(data)
	out := make([]SpawnedWorkReceipt, 0, len(lines))
	for _, line := range lines {
		var receipt SpawnedWorkReceipt
		if json.Unmarshal(line, &receipt) == nil && receipt.ReceiptID != "" && receipt.TabID == tabID && receipt.ChatID == chatID {
			receipt.Label = redactSensitiveText(receipt.Label)
			receipt.OutputFile = redactSensitiveText(receipt.OutputFile)
			receipt.Summary = redactSensitiveText(receipt.Summary)
			receipt.OutputTail = redactSensitiveText(receipt.OutputTail)
			out = append(out, receipt)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (m *Manager) ListSpawnedWorkForOwner(ownerKey, parentChatID, parentTabID, requestedChatID, requestedTabID string, tailBytes int) ([]map[string]any, error) {
	chatID, tabID, ok := m.ownerBinding(ownerKey, parentChatID, parentTabID)
	if !ok || chatID != strings.TrimSpace(requestedChatID) || tabID != strings.TrimSpace(requestedTabID) {
		return nil, fmt.Errorf("tab_id + chat_id must exactly match the owning Workass chat")
	}
	items := m.listSpawnedWorkRaw(tabID, chatID)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		encoded, _ := json.Marshal(publicSpawnedWorkItem(item))
		var row map[string]any
		_ = json.Unmarshal(encoded, &row)
		if tailBytes != 0 {
			tail, limited := readSpawnedWorkTailForItem(m.opts.StateDir, item, tailBytes)
			row["tail"] = tail
			row["tailLimited"] = limited
		}
		out = append(out, row)
	}
	return out, nil
}

// Kept small and deterministic for logs/tests.
func parseSpawnedWorkPID(raw string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	return n, err == nil && n > 0
}
