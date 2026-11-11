package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"workass/internal/acp"
	"workass/internal/agenttext"
	"workass/internal/fleetid"
	"workass/internal/procchild"
)

// The bug ledger: workass_report_bug.
//
// The owner removed every GitHub issue on 2026-11-10 and wants no bug tracking
// there. A Workass bug an agent hits is now reported to Workass itself: the
// report is refused unless it is complete, it is saved under
// state/bug-reports/<id>/ with diagnostics Workass collects on its own (all
// redacted), and a NEW chat in the Workass source checkout gets the report as
// its brief, to reproduce, fix, test and report back. The ledger is the list of
// bugs; nothing lives only in a chat.

const (
	bugReportDirName       = "bug-reports"
	bugReportSourceDirFile = "source-dir"
	bugReportMaxLogPaths   = 8
	bugReportMaxExcerpts   = 8
	bugReportExcerptBytes  = 32 << 10
	bugReportLogTailBytes  = 64 << 10
	bugReportListDefault   = 20
	bugReportListMax       = 200
)

var bugReportSeverities = []string{"blocker", "high", "medium", "low"}
var bugReportStatuses = []string{"open", "fixing", "fixed", "wontfix", "duplicate"}

var bugReportMu sync.Mutex

type bugReportLog struct {
	Source  string `json:"source"`
	Path    string `json:"path,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Excerpt string `json:"excerpt"`
	Error   string `json:"error,omitempty"`
}

type bugReportChat struct {
	TabID    string `json:"tab_id,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	GlobalID string `json:"global_id,omitempty"`
	Title    string `json:"title,omitempty"`
	CWD      string `json:"cwd,omitempty"`
	Error    string `json:"error,omitempty"`
}

type bugReportEvent struct {
	At     string `json:"at"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
	By     string `json:"by,omitempty"`
}

type bugReport struct {
	Schema       string           `json:"schema"`
	ID           string           `json:"id"`
	OperationID  string           `json:"operation_id"`
	CreatedAt    string           `json:"created_at"`
	UpdatedAt    string           `json:"updated_at"`
	Status       string           `json:"status"`
	Title        string           `json:"title"`
	WhatHappened string           `json:"what_happened"`
	Expected     string           `json:"expected"`
	ReproSteps   []string         `json:"repro_steps"`
	Severity     string           `json:"severity"`
	Workaround   string           `json:"workaround"`
	Version      string           `json:"workass_version"`
	Machine      string           `json:"machine"`
	Reported     map[string]any   `json:"reported,omitempty"`
	Logs         []bugReportLog   `json:"logs"`
	WorkIDs      []string         `json:"work_ids,omitempty"`
	Reporter     bugReportChat    `json:"reporter"`
	Thread       bugReportChat    `json:"thread"`
	History      []bugReportEvent `json:"history"`
	Files        map[string]any   `json:"files"`
}

func (h *agentControlHandler) bugReportRoot() string {
	dir := ""
	if h != nil && h.manager != nil {
		dir = h.manager.StateDir()
	}
	if strings.TrimSpace(dir) == "" && h != nil {
		dir = h.stateDir
	}
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, bugReportDirName)
}

func (h *agentControlHandler) bugReportCall(ctx context.Context, method, ownerKey, tabID, chatID string, params map[string]any) (any, error) {
	if h == nil || h.manager == nil {
		return nil, errors.New("the bug ledger requires the chat manager")
	}
	if err := h.authorizeActorOwner(ownerKey, tabID, chatID, "Workass chat control caller is not an owned ACP session"); err != nil {
		return nil, err
	}
	root := h.bugReportRoot()
	if root == "" {
		return nil, errors.New("this daemon has no state directory to keep the bug ledger in")
	}
	switch method {
	case "bug.report":
		return h.reportBug(ctx, root, ownerKey, tabID, chatID, params)
	case "bug.list":
		return listBugReports(root, fieldString(params, "bug_id"), fieldString(params, "status"), intField(params, "limit"))
	case "bug.update":
		return h.updateBugReport(root, tabID, chatID, params)
	}
	return nil, errors.New("unknown bug ledger method")
}

// missingBugReportFields names everything a report lacks, all at once, so one
// retry can complete it.
func missingBugReportFields(params map[string]any) []string {
	missing := []string{}
	for _, field := range []string{"title", "what_happened", "expected", "workaround"} {
		if strings.TrimSpace(fieldString(params, field)) == "" {
			missing = append(missing, field)
		}
	}
	steps := 0
	for _, step := range stringSliceField(params["repro_steps"]) {
		if strings.TrimSpace(step) != "" {
			steps++
		}
	}
	if steps == 0 {
		missing = append(missing, "repro_steps")
	}
	severity := strings.TrimSpace(fieldString(params, "severity"))
	if !stringInList(severity, bugReportSeverities) {
		missing = append(missing, "severity (blocker | high | medium | low)")
	}
	logs := 0
	for _, value := range append(stringSliceField(params["log_paths"]), stringSliceField(params["log_excerpts"])...) {
		if strings.TrimSpace(value) != "" {
			logs++
		}
	}
	if logs == 0 {
		missing = append(missing, "log_paths or log_excerpts (at least one log showing the bug)")
	}
	return missing
}

func stringInList(value string, list []string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// bugReportID is derived from the reporting chat and the operation id, so a
// retried call finds the report it already wrote.
func bugReportID(tabID, chatID, operationID string, now time.Time) string {
	sum := sha256.Sum256([]byte(tabID + "\x00" + chatID + "\x00" + operationID))
	return "bug-" + now.UTC().Format("20060102") + "-" + hex.EncodeToString(sum[:4])
}

func findBugReportByOperation(root, tabID, chatID, operationID string) (bugReport, bool) {
	sum := sha256.Sum256([]byte(tabID + "\x00" + chatID + "\x00" + operationID))
	suffix := "-" + hex.EncodeToString(sum[:4])
	entries, err := os.ReadDir(root)
	if err != nil {
		return bugReport{}, false
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		report, err := readBugReport(root, entry.Name())
		if err == nil && report.OperationID == operationID && report.Reporter.TabID == tabID && report.Reporter.ChatID == chatID {
			return report, true
		}
	}
	return bugReport{}, false
}

func readBugReport(root, id string) (bugReport, error) {
	if !validBugReportID(id) {
		return bugReport{}, errors.New("not a bug report id")
	}
	data, err := os.ReadFile(filepath.Join(root, id, "report.json"))
	if err != nil {
		return bugReport{}, fmt.Errorf("no bug report %s in the ledger", id)
	}
	var report bugReport
	if err := json.Unmarshal(data, &report); err != nil {
		return bugReport{}, fmt.Errorf("bug report %s is unreadable", id)
	}
	return report, nil
}

func validBugReportID(id string) bool {
	if !strings.HasPrefix(id, "bug-") || len(id) > 40 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// workassCredentialRE is Workass's own credential shapes: a chat's tool token,
// a job-start credential and a one-shot run credential. A log or a context
// file named in a report can carry one bare, which the generic redaction only
// catches after a "credential:" key.
var workassCredentialRE = regexp.MustCompile(`\b(?:wct|wjs|wrt)_[0-9a-f]{16,}`)

func writeBugReportJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = workassCredentialRE.ReplaceAll(data, []byte("[redacted]"))
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func (h *agentControlHandler) reportBug(ctx context.Context, root, ownerKey, tabID, chatID string, params map[string]any) (any, error) {
	operationID := strings.TrimSpace(fieldString(params, "operation_id"))
	if operationID == "" {
		return nil, errors.New("workass_report_bug requires a caller-stable operation_id")
	}
	bugReportMu.Lock()
	existing, replay := findBugReportByOperation(root, tabID, chatID, operationID)
	bugReportMu.Unlock()
	if replay {
		// The report is already in the ledger. A thread that could not be
		// opened the first time is tried again; one that was opened is not
		// opened twice.
		if existing.Thread.ChatID == "" {
			existing = h.openBugFixThread(ctx, root, ownerKey, tabID, chatID, existing, params)
		}
		return bugReportAnswer(root, existing, true), nil
	}
	if missing := missingBugReportFields(params); len(missing) > 0 {
		return nil, fmt.Errorf("this bug report is incomplete, and nothing was saved. Missing: %s. A report needs a title, what happened, what you expected, the steps that reproduce it, its severity, the workaround in use (or \"none\"), and at least one log showing it: log_paths (absolute paths of log files) and/or log_excerpts (the relevant lines). Workass adds its own diagnostics, version and machine", strings.Join(missing, "; "))
	}
	logs, err := collectBugReportLogs(stringSliceField(params["log_paths"]), stringSliceField(params["log_excerpts"]))
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	id := bugReportID(tabID, chatID, operationID, now)
	dir := filepath.Join(root, id)
	machine := h.bugReportMachine()
	reported := map[string]any{}
	if value := strings.TrimSpace(fieldString(params, "workass_version")); value != "" {
		reported["workass_version"] = acp.RedactSensitiveText(value)
	}
	if value := strings.TrimSpace(fieldString(params, "machine")); value != "" {
		reported["machine"] = acp.RedactSensitiveText(value)
	}
	steps := []string{}
	for _, step := range stringSliceField(params["repro_steps"]) {
		if step = strings.TrimSpace(step); step != "" {
			steps = append(steps, acp.RedactSensitiveText(step))
		}
	}
	workIDs := []string{}
	for _, workID := range stringSliceField(params["work_ids"]) {
		if workID = strings.TrimSpace(workID); workID != "" {
			workIDs = append(workIDs, workID)
		}
	}
	report := bugReport{
		Schema: "workass-bug-report/1", ID: id, OperationID: operationID,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), Status: "open",
		Title:        acp.RedactSensitiveText(strings.TrimSpace(fieldString(params, "title"))),
		WhatHappened: acp.RedactSensitiveText(strings.TrimSpace(fieldString(params, "what_happened"))),
		Expected:     acp.RedactSensitiveText(strings.TrimSpace(fieldString(params, "expected"))),
		ReproSteps:   steps, Severity: strings.TrimSpace(fieldString(params, "severity")),
		Workaround: acp.RedactSensitiveText(strings.TrimSpace(fieldString(params, "workaround"))),
		Version:    daemonVersion, Machine: machine, Reported: reported,
		Logs: logs, WorkIDs: workIDs,
		Reporter: h.bugReportReporter(tabID, chatID),
		History:  []bugReportEvent{{At: now.Format(time.RFC3339), Status: "open", Note: "reported", By: chatID}},
		Files: map[string]any{
			"report":      filepath.Join(dir, "report.json"),
			"brief":       filepath.Join(dir, "brief.md"),
			"diagnostics": filepath.Join(dir, "diagnostics.json"),
		},
	}
	if len(reported) == 0 {
		report.Reported = nil
	}

	bugReportMu.Lock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		bugReportMu.Unlock()
		return nil, err
	}
	diagnostics := h.collectBugReportDiagnostics(ctx, ownerKey, tabID, chatID, workIDs)
	if err := writeBugReportJSON(filepath.Join(dir, "diagnostics.json"), diagnostics); err != nil {
		bugReportMu.Unlock()
		return nil, err
	}
	if err := writeBugReportJSON(filepath.Join(dir, "report.json"), report); err != nil {
		bugReportMu.Unlock()
		return nil, err
	}
	bugReportMu.Unlock()

	report = h.openBugFixThread(ctx, root, ownerKey, tabID, chatID, report, params)
	return bugReportAnswer(root, report, false), nil
}

func bugReportAnswer(root string, report bugReport, replayed bool) map[string]any {
	answer := map[string]any{
		"ok": true, "replayed": replayed, "bug_id": report.ID, "status": report.Status,
		"report_file":      filepath.Join(root, report.ID, "report.json"),
		"diagnostics_file": filepath.Join(root, report.ID, "diagnostics.json"),
	}
	if report.Thread.ChatID != "" {
		thread := map[string]any{"tab_id": report.Thread.TabID, "chat_id": report.Thread.ChatID, "cwd": report.Thread.CWD}
		if report.Thread.GlobalID != "" {
			thread["global_id"] = report.Thread.GlobalID
		}
		answer["thread"] = thread
	} else {
		answer["thread_error"] = report.Thread.Error
		answer["warning"] = "The report is saved in the ledger, but no fixing chat was opened: " + report.Thread.Error + ". Call workass_report_bug again with the same operation_id (and repo_dir if the Workass source checkout was not found) to open it."
	}
	return answer
}

func collectBugReportLogs(paths, excerpts []string) ([]bugReportLog, error) {
	if len(paths) > bugReportMaxLogPaths {
		return nil, fmt.Errorf("at most %d log_paths; name the files that show the bug", bugReportMaxLogPaths)
	}
	if len(excerpts) > bugReportMaxExcerpts {
		return nil, fmt.Errorf("at most %d log_excerpts", bugReportMaxExcerpts)
	}
	logs := []bugReportLog{}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("log path %q is not absolute; nothing was saved", path)
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("log path %q is not a readable file on this machine; nothing was saved. Pass the lines themselves in log_excerpts instead", path)
		}
		tail, err := readLogTail(path, bugReportLogTailBytes)
		if err != nil {
			return nil, fmt.Errorf("log path %q could not be read; nothing was saved", path)
		}
		logs = append(logs, bugReportLog{Source: "path", Path: path, Bytes: info.Size(), Excerpt: acp.RedactSensitiveText(tail)})
	}
	for _, excerpt := range excerpts {
		excerpt = strings.TrimSpace(excerpt)
		if excerpt == "" {
			continue
		}
		if len(excerpt) > bugReportExcerptBytes {
			excerpt = excerpt[len(excerpt)-bugReportExcerptBytes:]
		}
		logs = append(logs, bugReportLog{Source: "excerpt", Excerpt: acp.RedactSensitiveText(strings.ToValidUTF8(excerpt, "�"))})
	}
	return logs, nil
}

func (h *agentControlHandler) bugReportMachine() string {
	host, _ := os.Hostname()
	parts := []string{}
	if self := strings.TrimSpace(h.selfMachineID); self != "" {
		parts = append(parts, self)
	}
	if host != "" {
		parts = append(parts, host)
	}
	parts = append(parts, runtime.GOOS+"/"+runtime.GOARCH)
	return strings.Join(parts, " · ")
}

func (h *agentControlHandler) bugReportReporter(tabID, chatID string) bugReportChat {
	reporter := bugReportChat{TabID: tabID, ChatID: chatID}
	if self := strings.TrimSpace(h.selfMachineID); self != "" && chatID != "" {
		if id, err := fleetid.New(self, fleetid.Chat, chatID); err == nil {
			reporter.GlobalID = id.String()
		}
	}
	if h.chats != nil && h.chats.providerChats != nil {
		if chat, err := h.chats.providerChats.ReadChat(tabID, chatID, 1, false); err == nil {
			reporter.Title = acp.SanitizeDisplayText(fieldString(chat, "title"), 120)
			reporter.CWD = fieldString(chat, "cwd")
		}
	}
	return reporter
}

// collectBugReportDiagnostics is what Workass knows about itself, redacted:
// the daemon, its providers and processes, the end of its own logs, the update
// status, the reporting chat's turn diagnostics and background work, and the
// supervised jobs named or owned by that chat. A part that cannot be read says
// so instead of failing the report.
func (h *agentControlHandler) collectBugReportDiagnostics(ctx context.Context, ownerKey, tabID, chatID string, workIDs []string) any {
	out := diagnosticsReport{
		manager: h.manager, logRoot: os.Getenv(logRootEnvVar), startedAt: daemonStartedAt, now: time.Now,
	}.buildWith(diagnosticsLogFiles, bugReportLogTailBytes)
	out["schema"] = "workass-bug-diagnostics/1"
	if daemon, ok := out["daemon"].(map[string]any); ok {
		daemon["machineId"] = h.selfMachineID
	}
	updateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if status, err := h.answerUpdateLocally(updateCtx, "update.status", map[string]any{}); err != nil {
		out["updateStatus"] = map[string]any{"error": err.Error()}
	} else {
		out["updateStatus"] = status
	}
	cancel()
	if h.chats != nil && h.chats.providerChats != nil {
		if diag, err := h.chats.providerChats.TurnDiagnostics(map[string]any{"tab_id": tabID, "chat_id": chatID, "limit": 5}); err != nil {
			out["chatDiagnostics"] = map[string]any{"error": err.Error()}
		} else {
			out["chatDiagnostics"] = diag
		}
	}
	out["spawnedWork"] = h.manager.ListSpawnedWork(tabID, chatID)
	jobs := []any{}
	named := map[string]bool{}
	for _, workID := range workIDs {
		named[workID] = true
		view, ok := h.manager.ProcessChildByID(workID)
		if !ok {
			jobs = append(jobs, map[string]any{"work_id": workID, "error": "no such supervised job on this machine"})
			continue
		}
		jobs = append(jobs, bugReportJob(view))
	}
	for _, view := range h.manager.ListProcessChildren() {
		if named[view.WorkID] || view.ChatID != chatID || view.TabID != tabID {
			continue
		}
		jobs = append(jobs, bugReportJob(view))
	}
	out["processChildren"] = jobs
	if receipts, err := h.manager.ListSpawnedWorkReceipts(ownerKey, chatID, tabID, "", "", 20); err == nil {
		out["spawnedWorkReceipts"] = receipts
	}
	return redactDiagnostics(out)
}

func bugReportJob(view acp.ProcessChildView) map[string]any {
	data, _ := json.Marshal(view)
	row := map[string]any{}
	_ = json.Unmarshal(data, &row)
	if view.OutputFile != "" {
		if tail, err := readLogTail(view.OutputFile, 16<<10); err == nil {
			row["output_tail"] = tail
		}
		if tail, err := readLogTail(procchild.SupervisorLogPath(view.OutputFile), 16<<10); err == nil {
			row["supervisor_log_tail"] = tail
		}
	}
	return row
}

// bugReportSourceDir finds the Workass source checkout the fixing chat works
// in: the one the caller names, the one remembered from an earlier report,
// $WORKASS_SOURCE_DIR, this daemon's own directory when it runs from source, or the
// reporting chat's own directory when that is the checkout.
func (h *agentControlHandler) bugReportSourceDir(root, requested, reporterCWD string) (string, error) {
	if requested = strings.TrimSpace(requested); requested != "" {
		if !isWorkassSourceRepo(requested) {
			return "", fmt.Errorf("repo_dir %s is not a Workass source checkout (no cmd/workass/main.go)", requested)
		}
		_ = os.WriteFile(filepath.Join(root, bugReportSourceDirFile), []byte(filepath.Clean(requested)+"\n"), 0o600)
		return filepath.Clean(requested), nil
	}
	candidates := []string{}
	if data, err := os.ReadFile(filepath.Join(root, bugReportSourceDirFile)); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(data)))
	}
	candidates = append(candidates, os.Getenv("WORKASS_SOURCE_DIR"))
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	candidates = append(candidates, reporterCWD)
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" && isWorkassSourceRepo(candidate) {
			return filepath.Clean(candidate), nil
		}
	}
	return "", errors.New("no Workass source checkout was found on this machine; pass repo_dir (it is remembered for later reports)")
}

func isWorkassSourceRepo(dir string) bool {
	return filepath.IsAbs(dir) && isWorkassSourceCheckout(dir)
}

// openBugFixThread opens the NEW chat that fixes the bug, in the Workass
// source checkout, and sends it the report as its brief. Whatever happens is
// written back to the report, so the ledger always says whether a chat owns it.
func (h *agentControlHandler) openBugFixThread(ctx context.Context, root, ownerKey, tabID, chatID string, report bugReport, params map[string]any) bugReport {
	fail := func(err error) bugReport {
		report.Thread = bugReportChat{Error: acp.RedactSensitiveText(err.Error())}
		report.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		bugReportMu.Lock()
		_ = writeBugReportJSON(filepath.Join(root, report.ID, "report.json"), report)
		bugReportMu.Unlock()
		return report
	}
	if h.chats == nil || h.chats.providerChats == nil {
		return fail(errors.New("this daemon has no chat actor to open a fixing chat with"))
	}
	repo, err := h.bugReportSourceDir(root, fieldString(params, "repo_dir"), report.Reporter.CWD)
	if err != nil {
		return fail(err)
	}
	brief := bugReportBrief(root, report)
	if err := os.WriteFile(filepath.Join(root, report.ID, "brief.md"), []byte(brief), 0o600); err != nil {
		return fail(err)
	}
	create := map[string]any{
		"operation_id": "bugfix-create-" + report.ID,
		"title":        acp.SanitizeDisplayText("Bug "+report.ID+": "+report.Title, 80),
		"cwd":          repo,
		"focus":        false,
	}
	explicit := false
	for _, key := range []string{"provider_id", "model_id", "effort", "mode_id"} {
		if value := strings.TrimSpace(fieldString(params, key)); value != "" {
			create[key] = value
			explicit = true
		}
	}
	if !explicit {
		// Workass's own default, as a chat the owner starts by hand gets it.
		// Before, this create named nothing, so the fixing chat took the first
		// ready provider's first model rather than the one in Ajustes · General.
		for key, value := range h.newChatDefaultControls(ctx, ownerKey, tabID, chatID) {
			create[key] = value
		}
	}
	// The fixing chat starts with full access unless the reporter named a mode:
	// it reproduces, edits, builds and ships, and a permission card on each step
	// stalls a chat nobody is watching (owner, 2026-11-11). The permission
	// ceiling still holds: a reporter below full access cannot hand out more than
	// it has, and a card would hold this call for a person, so such a chat opens
	// at the default above and the owner raises it.
	if fieldString(params, "mode_id") == "" {
		providerID := fieldString(create, "provider_id")
		if fullMode := h.bugFixFullAccessMode(ctx, providerID); fullMode != "" &&
			!permissionChangeRaises(h.permissionCallerFor(ctx, ownerKey, tabID, chatID), permissionChange{providerID: providerID, modeID: fullMode}) {
			create["mode_id"] = fullMode
		}
	}
	created, err := h.chats.createOnThisMachine(h.withPermissionGate(ctx, ownerKey, tabID, chatID), create)
	if err != nil {
		return fail(fmt.Errorf("the fixing chat could not be created: %w", err))
	}
	threadTab, threadChat := fieldString(created, "tabId"), fieldString(created, "chatId")
	send := map[string]any{
		"tab_id": threadTab, "chat_id": threadChat,
		"operation_id": "bugfix-brief-" + report.ID, "message": brief, "delivery": "auto",
	}
	if err := h.admitSend(ctx, ownerKey, tabID, chatID, send); err != nil {
		return fail(fmt.Errorf("the fixing chat %s was created but its brief was not sent: %w", threadChat, err))
	}
	if _, err := h.chats.send(send); err != nil {
		return fail(fmt.Errorf("the fixing chat %s was created but its brief was not sent: %w", threadChat, err))
	}
	report.Thread = bugReportChat{TabID: threadTab, ChatID: threadChat, CWD: repo, Title: fieldString(created, "title")}
	if self := strings.TrimSpace(h.selfMachineID); self != "" {
		if id, err := fleetid.New(self, fleetid.Chat, threadChat); err == nil {
			report.Thread.GlobalID = id.String()
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	report.Status, report.UpdatedAt = "fixing", now
	report.History = append(report.History, bugReportEvent{At: now, Status: "fixing", Note: "fixing chat opened", By: threadChat})
	bugReportMu.Lock()
	_ = writeBugReportJSON(filepath.Join(root, report.ID, "report.json"), report)
	bugReportMu.Unlock()
	return report
}

// bugReportBrief is the fixing chat's first message: the report itself, where
// its files are, and who to tell.
func bugReportBrief(root string, report bugReport) string {
	steps := make([]string, 0, len(report.ReproSteps))
	for i, step := range report.ReproSteps {
		steps = append(steps, fmt.Sprintf("%d. %s", i+1, step))
	}
	logs := make([]string, 0, len(report.Logs))
	for _, log := range report.Logs {
		excerpt := log.Excerpt
		if len(excerpt) > 4000 {
			excerpt = "…" + excerpt[len(excerpt)-4000:]
		}
		label := "excerpt"
		if log.Path != "" {
			label = log.Path
		}
		logs = append(logs, "--- "+label+"\n"+excerpt)
	}
	reporter := firstNonEmptyString(report.Reporter.GlobalID, report.Reporter.ChatID)
	replacer := strings.NewReplacer(
		"{{id}}", report.ID,
		"{{title}}", report.Title,
		"{{severity}}", report.Severity,
		"{{version}}", report.Version,
		"{{machine}}", report.Machine,
		"{{what_happened}}", report.WhatHappened,
		"{{expected}}", report.Expected,
		"{{steps}}", strings.Join(steps, "\n"),
		"{{workaround}}", report.Workaround,
		"{{logs}}", strings.Join(logs, "\n\n"),
		"{{report_file}}", filepath.Join(root, report.ID, "report.json"),
		"{{diagnostics_file}}", filepath.Join(root, report.ID, "diagnostics.json"),
		"{{reporter_chat}}", reporter,
		"{{reporter_tab}}", report.Reporter.TabID,
		"{{reporter_chat_id}}", report.Reporter.ChatID,
	)
	return workassCredentialRE.ReplaceAllString(replacer.Replace(agenttext.Get("bugreport.brief")), "[redacted]")
}

func listBugReports(root, id, status string, limit int) (any, error) {
	if id = strings.TrimSpace(id); id != "" {
		report, err := readBugReport(root, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"bug": report}, nil
	}
	status = strings.TrimSpace(status)
	if status != "" && !stringInList(status, bugReportStatuses) {
		return nil, errors.New("status must be one of open, fixing, fixed, wontfix, duplicate")
	}
	if limit <= 0 {
		limit = bugReportListDefault
	}
	if limit > bugReportListMax {
		limit = bugReportListMax
	}
	entries, _ := os.ReadDir(root)
	reports := []bugReport{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		report, err := readBugReport(root, entry.Name())
		if err != nil || (status != "" && report.Status != status) {
			continue
		}
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].CreatedAt != reports[j].CreatedAt {
			return reports[i].CreatedAt > reports[j].CreatedAt
		}
		return reports[i].ID > reports[j].ID
	})
	total := len(reports)
	if len(reports) > limit {
		reports = reports[:limit]
	}
	rows := make([]map[string]any, 0, len(reports))
	for _, report := range reports {
		row := map[string]any{
			"bug_id": report.ID, "title": report.Title, "status": report.Status, "severity": report.Severity,
			"created_at": report.CreatedAt, "updated_at": report.UpdatedAt, "workass_version": report.Version,
			"report_file": filepath.Join(root, report.ID, "report.json"),
		}
		if report.Thread.ChatID != "" {
			row["thread_chat_id"] = report.Thread.ChatID
			row["thread_tab_id"] = report.Thread.TabID
		}
		if len(report.History) > 0 {
			row["last_note"] = report.History[len(report.History)-1].Note
		}
		rows = append(rows, row)
	}
	return map[string]any{"bugs": rows, "total": total, "ledger": root}, nil
}

func (h *agentControlHandler) updateBugReport(root, tabID, chatID string, params map[string]any) (any, error) {
	id := strings.TrimSpace(fieldString(params, "bug_id"))
	status := strings.TrimSpace(fieldString(params, "status"))
	note := acp.RedactSensitiveText(strings.TrimSpace(fieldString(params, "note")))
	operationID := strings.TrimSpace(fieldString(params, "operation_id"))
	if !stringInList(status, bugReportStatuses) {
		return nil, errors.New("status must be one of open, fixing, fixed, wontfix, duplicate")
	}
	if note == "" {
		return nil, errors.New("a status change needs a note: what was found or changed (for fixed: the commit and the version that ships it)")
	}
	bugReportMu.Lock()
	defer bugReportMu.Unlock()
	report, err := readBugReport(root, id)
	if err != nil {
		return nil, err
	}
	marker := "op:" + operationID
	for _, event := range report.History {
		if strings.HasSuffix(event.By, marker) {
			return map[string]any{"ok": true, "replayed": true, "bug_id": report.ID, "status": report.Status}, nil
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	report.Status, report.UpdatedAt = status, now
	report.History = append(report.History, bugReportEvent{At: now, Status: status, Note: note, By: chatID + " " + marker})
	if err := writeBugReportJSON(filepath.Join(root, report.ID, "report.json"), report); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "replayed": false, "bug_id": report.ID, "status": report.Status}, nil
}

// bugFixFullAccessMode is the provider's own full-access mode on this machine,
// or empty when the provider is unnamed, not ready, or offers none.
func (h *agentControlHandler) bugFixFullAccessMode(ctx context.Context, providerID string) string {
	providerID = strings.TrimSpace(providerID)
	if h == nil || h.manager == nil || providerID == "" {
		return ""
	}
	groups, _ := h.manager.Catalog(ctx)["groups"].([]acp.CatalogGroup)
	for _, group := range groups {
		if group.ProviderID != providerID || (group.Status != "" && group.Status != "ready") {
			continue
		}
		for _, offered := range group.Modes {
			if acp.PermissionLevel(providerID, offered.ID) == fleetLimitFull {
				return offered.ID
			}
		}
	}
	return ""
}

// newChatDefaultControls is Ajustes · General's new-chat agent, model, effort
// and permission mode, resolved against this machine's catalog the way the
// composer resolves them for a chat the owner starts (applyNewChatDefaults in
// desktop/renderer2/src/store/store.ts): no agent named, or one not ready
// here, is nothing; a model gone from the catalog falls back to the agent's
// own default; an effort applies only with its own model; a permission mode
// only if the agent offers it. A mode above the asking chat's own level is
// left out, so the default never turns a create into a request for approval.
func (h *agentControlHandler) newChatDefaultControls(ctx context.Context, ownerKey, tabID, chatID string) map[string]any {
	if h == nil || h.settings == nil || h.manager == nil {
		return nil
	}
	defaults := mapFromAnyMain(h.settings()["newChat"])
	providerID := strings.TrimSpace(fieldString(defaults, "providerId"))
	if providerID == "" {
		return nil
	}
	groups, _ := h.manager.Catalog(ctx)["groups"].([]acp.CatalogGroup)
	for _, group := range groups {
		if group.ProviderID != providerID || (group.Status != "" && group.Status != "ready") || len(group.Models) == 0 {
			continue
		}
		out := map[string]any{"provider_id": providerID}
		if model := strings.TrimSpace(fieldString(defaults, "model")); model != "" {
			for _, candidate := range group.Models {
				if candidate.ModelID != model {
					continue
				}
				out["model_id"] = model
				if effort := strings.TrimSpace(fieldString(defaults, "effort")); effort != "" {
					for _, offered := range candidate.Efforts {
						if offered == effort {
							out["effort"] = effort
						}
					}
				}
			}
		}
		if mode := strings.TrimSpace(fieldString(defaults, "modeId")); mode != "" {
			for _, offered := range group.Modes {
				if offered.ID != mode {
					continue
				}
				caller := h.permissionCallerFor(ctx, ownerKey, tabID, chatID)
				if !permissionChangeRaises(caller, permissionChange{providerID: providerID, modeID: mode}) {
					out["mode_id"] = mode
				}
			}
		}
		return out
	}
	return nil
}
