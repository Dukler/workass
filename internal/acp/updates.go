package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// Vendor CLIs can take a few seconds to initialize on a busy machine. This
	// remains bounded, but must not discard an otherwise detected provider (and
	// therefore hide its update) merely because the first version probe lost a
	// short scheduler race.
	cliVersionTimeout              = 5 * time.Second
	mockUpdateEnv                  = "WORKASS_MOCK_UPDATE"
	providerUpdateTailBytes        = 4 * 1024
	providerUpdateProgressThrottle = time.Second
)

var (
	postUpdateCLIVersionTimeout  = 10 * time.Second
	postUpdateCLIVersionBackoffs = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
)

var semverTokenRE = regexp.MustCompile(`(?i)\bv?([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?(?:\+[0-9A-Za-z][0-9A-Za-z.-]*)?)\b`)

type CLIVersion struct {
	Version string `json:"version,omitempty"`
	Raw     string `json:"raw,omitempty"`
}

type ProviderUpdatesPayload struct {
	CheckedAt string           `json:"checkedAt"`
	Updates   []ProviderUpdate `json:"updates"`
}

type ProviderUpdate struct {
	ProviderID      string `json:"providerId"`
	CLI             string `json:"cli"`
	Installed       string `json:"installed"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"updateAvailable"`
	Hint            string `json:"hint"`
	LastError       string `json:"lastError,omitempty"`
	ExitCode        *int   `json:"exitCode,omitempty"`
	Tail            string `json:"tail,omitempty"`
	RecheckError    string `json:"recheckError,omitempty"`
}

type ProviderUpdateProgress struct {
	ProviderID string `json:"providerId"`
	Status     string `json:"status"`
	StartedAt  string `json:"startedAt"`
	Tail       string `json:"tail"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	Error      string `json:"error,omitempty"`
}

type ProviderUpdateCommand struct {
	Command string
	Args    []string
}

type providerUpdateSpec struct {
	ProviderID string
	CLI        string
	Source     string
	Hint       string
}

type providerUpdateCandidate struct {
	spec      providerUpdateSpec
	installed string
}

type providerUpdateFailure struct {
	LastError string
	ExitCode  int
	Tail      string
}

type providerUpdateRun struct {
	mu         sync.Mutex
	providerID string
	status     string
	startedAt  time.Time
	exitCode   *int
	errText    string
	tail       outputTail
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	lastEmit   time.Time
}

type outputTail struct {
	limit int
	text  string
}

type lenientSemver struct {
	major int
	minor int
	patch int
	pre   []string
}

func defaultProviderUpdateSources() map[string]string {
	return map[string]string{
		"claude": "https://registry.npmjs.org/@anthropic-ai/claude-code/latest",
		"codex":  "https://registry.npmjs.org/@openai/codex/latest",
		"qwen":   "https://registry.npmjs.org/@qwen-code/qwen-code/latest",
	}
}

func defaultProviderUpdateCommands() map[string]ProviderUpdateCommand {
	return map[string]ProviderUpdateCommand{
		"claude": {Command: "claude", Args: []string{"update"}},
		"codex":  {Command: "codex", Args: []string{"update"}},
		"qwen":   {Command: "qwen", Args: []string{"update"}},
	}
}

func copyProviderUpdateCommands(in map[string]ProviderUpdateCommand) map[string]ProviderUpdateCommand {
	out := make(map[string]ProviderUpdateCommand, len(in))
	for rawID, cmd := range in {
		id := normalizeProviderID(rawID)
		command := strings.TrimSpace(cmd.Command)
		if id == "" || command == "" {
			continue
		}
		out[id] = ProviderUpdateCommand{
			Command: command,
			Args:    append([]string(nil), cmd.Args...),
		}
	}
	return out
}

func newOutputTail(limit int) outputTail {
	if limit <= 0 {
		limit = providerUpdateTailBytes
	}
	return outputTail{limit: limit}
}

func (t *outputTail) append(raw string) {
	if raw == "" {
		return
	}
	next := t.text + raw
	if len(next) > t.limit {
		buf := []byte(next)
		buf = buf[len(buf)-t.limit:]
		for len(buf) > 0 && !utf8.Valid(buf) {
			buf = buf[1:]
		}
		next = string(buf)
	}
	t.text = next
}

func (t *outputTail) string() string {
	return t.text
}

func (r *providerUpdateRun) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status == "running"
}

func (r *providerUpdateRun) setStarted(cmd *exec.Cmd, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmd = cmd
	r.cancel = cancel
}

func (r *providerUpdateRun) appendOutput(stream, text string) {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return
	}
	text = redactSensitiveText(text)
	line := text
	if stream != "" {
		line = "[" + stream + "] " + text
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tail.append(line + "\n")
}

func (r *providerUpdateRun) finish(status string, code int, errText string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
	r.exitCode = intPtr(code)
	r.errText = redactSensitiveText(errText)
	r.cancel = nil
}

func (r *providerUpdateRun) kill() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status != "running" {
		return false
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	return true
}

func (r *providerUpdateRun) tailString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tail.string()
}

func (r *providerUpdateRun) snapshot() ProviderUpdateProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *providerUpdateRun) snapshotLocked() ProviderUpdateProgress {
	progress := ProviderUpdateProgress{
		ProviderID: r.providerID,
		Status:     r.status,
		StartedAt:  timeString(r.startedAt),
		Tail:       redactSensitiveText(r.tail.string()),
		Error:      redactSensitiveText(r.errText),
	}
	if r.exitCode != nil {
		progress.ExitCode = intPtr(*r.exitCode)
	}
	return progress
}

func (r *providerUpdateRun) markEmitted(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastEmit = at
}

func limitUTF8Bytes(raw string, limit int) string {
	if limit <= 0 || len(raw) <= limit {
		return raw
	}
	buf := []byte(raw)
	buf = buf[len(buf)-limit:]
	for len(buf) > 0 && !utf8.Valid(buf) {
		buf = buf[1:]
	}
	return string(buf)
}

func intPtr(v int) *int {
	return &v
}

func providerUpdateCommandForProvider(id string, commands map[string]ProviderUpdateCommand) (ProviderUpdateCommand, bool) {
	cmd, ok := commands[normalizeProviderID(id)]
	if !ok || strings.TrimSpace(cmd.Command) == "" {
		return ProviderUpdateCommand{}, false
	}
	cmd.Command = strings.TrimSpace(cmd.Command)
	cmd.Args = append([]string(nil), cmd.Args...)
	return cmd, true
}

func cliUpdateSpecForProvider(id string, sources map[string]string) (providerUpdateSpec, bool) {
	id = normalizeProviderID(id)
	source := strings.TrimSpace(sources[id])
	if source == "" {
		return providerUpdateSpec{}, false
	}
	switch id {
	case "claude":
		return providerUpdateSpec{ProviderID: "claude", CLI: "claude", Source: source, Hint: "claude update"}, true
	case "codex":
		return providerUpdateSpec{ProviderID: "codex", CLI: "codex", Source: source, Hint: "codex update"}, true
	case "qwen":
		return providerUpdateSpec{ProviderID: "qwen", CLI: "qwen", Source: source, Hint: "qwen update"}, true
	default:
		return providerUpdateSpec{}, false
	}
}

func cliVersionCommandForProvider(id string) (string, bool) {
	switch normalizeProviderID(id) {
	case "claude":
		return "claude", true
	case "codex":
		return "codex", true
	case "qwen":
		return "qwen", true
	default:
		return "", false
	}
}

func (m *Manager) providerUpdateLoop() {
	ticker := time.NewTicker(m.opts.ProviderUpdateInterval)
	defer ticker.Stop()
	m.runProviderUpdateTicks(ticker.C)
}

func (m *Manager) runProviderUpdateTicks(ticks <-chan time.Time) {
	for range ticks {
		m.scheduleProviderUpdateCheck()
	}
}

func (m *Manager) scheduleProviderUpdateCheck() {
	m.updateCheckMu.Lock()
	m.mu.Lock()
	resetting := m.resetting
	m.mu.Unlock()
	if resetting || m.updateCheckRunning {
		m.updateCheckMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.updateCheckRunning = true
	m.updateCheckCancel = cancel
	m.updateCheckWG.Add(1)
	m.updateCheckMu.Unlock()

	go func() {
		defer func() {
			m.updateCheckMu.Lock()
			m.updateCheckRunning = false
			m.updateCheckCancel = nil
			m.updateCheckMu.Unlock()
			m.updateCheckWG.Done()
		}()
		m.runScheduledProviderUpdateCheck(ctx)
	}()
}

func (m *Manager) runScheduledProviderUpdateCheck(ctx context.Context) {
	for attempt := 0; ; attempt++ {
		_, hasCandidates, hasFailures := m.checkProviderUpdates(ctx)
		if ctx.Err() != nil || !hasCandidates || !hasFailures || attempt >= len(m.opts.ProviderUpdateRetryBackoffs) {
			return
		}
		delay := m.opts.ProviderUpdateRetryBackoffs[attempt]
		if m.opts.Logf != nil {
			m.opts.Logf("provider update check retry scheduled", map[string]any{
				"attempt": attempt + 1,
				"delayMs": delay.Milliseconds(),
			})
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) stopScheduledProviderUpdateCheck() {
	m.updateCheckMu.Lock()
	cancel := m.updateCheckCancel
	if cancel != nil {
		cancel()
	}
	m.updateCheckMu.Unlock()
	m.updateCheckWG.Wait()
}

func (m *Manager) startInstalledCLIVersionCheck(parent context.Context, providerID string) <-chan *CLIVersion {
	if _, ok := cliVersionCommandForProvider(providerID); !ok {
		return nil
	}
	ch := make(chan *CLIVersion, 1)
	go func() {
		ch <- m.detectInstalledCLIVersion(parent, providerID)
	}()
	return ch
}

func collectInstalledCLIVersion(ch <-chan *CLIVersion) *CLIVersion {
	if ch == nil {
		return nil
	}
	select {
	case version := <-ch:
		return version
	default:
		return nil
	}
}

func (m *Manager) StartProviderUpdate(parent context.Context, providerID string) (map[string]any, error) {
	id := normalizeProviderID(providerID)
	if id == "" {
		return nil, providerUpdateError("providers:update-unknown-provider", "providerId is required", nil)
	}
	command, ok := providerUpdateCommandForProvider(id, m.opts.ProviderUpdateCommands)
	if !ok {
		return nil, providerUpdateError("providers:update-unknown-provider", "unknown providerId", map[string]any{"providerId": id})
	}
	if !m.providerExists(id) {
		return nil, providerUpdateError("providers:update-unknown-provider", "unknown providerId", map[string]any{"providerId": id})
	}
	if cli, supported := cliVersionCommandForProvider(id); supported && sameExecutableName(command.Command, cli) {
		resolved, err := m.providerCLIExecutable(id)
		if err != nil {
			return nil, providerUpdateError("providers:update-cli-not-found", err.Error(), map[string]any{"providerId": id})
		}
		command.Command = resolved
	}
	if m.providerUpdateRunning(id) {
		return nil, providerUpdateError("providers:update-in-progress", "ya hay una actualización en curso", map[string]any{
			"providerId": id,
		})
	}

	update, ok := m.currentProviderUpdate(parent, id)
	if !ok || !update.UpdateAvailable {
		fields := map[string]any{"providerId": id}
		if ok {
			fields["installed"] = update.Installed
			fields["latest"] = update.Latest
		}
		return nil, providerUpdateError("providers:update-no-pending", "no pending update", fields)
	}

	now := time.Now().UTC()
	run := &providerUpdateRun{
		providerID: id,
		status:     "running",
		startedAt:  now,
		tail:       newOutputTail(providerUpdateTailBytes),
	}
	m.updateMu.Lock()
	if existing := m.providerUpdateRuns[id]; existing != nil && existing.active() {
		m.updateMu.Unlock()
		return nil, providerUpdateError("providers:update-in-progress", "ya hay una actualización en curso", map[string]any{
			"providerId": id,
		})
	}
	m.providerUpdateRuns[id] = run
	m.updateMu.Unlock()

	run.markEmitted(time.Now())
	m.emitProviderUpdateProgress(run.snapshot())
	m.startProviderUpdateRun(id, run, command)
	return map[string]any{"ok": true, "providerId": id}, nil
}

func (m *Manager) startProviderUpdateRun(providerID string, run *providerUpdateRun, command ProviderUpdateCommand) {
	timeout := m.opts.ProviderUpdateRunTimeout
	if timeout <= 0 {
		timeout = defaultProviderUpdateRunTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, command.Command, command.Args...)
	cmd.Dir = m.opts.RootDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		run.appendOutput("system", "stdout pipe: "+err.Error())
		m.finishProviderUpdateRun(providerID, run, 1, "failed", "stdout pipe: "+err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		run.appendOutput("system", "stderr pipe: "+err.Error())
		m.finishProviderUpdateRun(providerID, run, 1, "failed", "stderr pipe: "+err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		cancel()
		run.appendOutput("system", err.Error())
		m.finishProviderUpdateRun(providerID, run, 1, "failed", err.Error())
		return
	}
	run.setStarted(cmd, cancel)
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		m.readProviderUpdateOutput(stdout, "stdout", run)
	}()
	go func() {
		defer readers.Done()
		m.readProviderUpdateOutput(stderr, "stderr", run)
	}()
	go func() {
		err := cmd.Wait()
		readers.Wait()
		cancel()
		exitCode := 0
		status := "done"
		lastError := ""
		if err != nil {
			status = "failed"
			exitCode = -1
			if cmd.ProcessState != nil {
				exitCode = cmd.ProcessState.ExitCode()
			}
			lastError = err.Error()
			if ctx.Err() == context.DeadlineExceeded {
				lastError = "actualizacion excedio el limite de " + timeout.String()
			}
		}
		m.finishProviderUpdateRun(providerID, run, exitCode, status, lastError)
	}()
}

func (m *Manager) readProviderUpdateOutput(r io.Reader, stream string, run *providerUpdateRun) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for scanner.Scan() {
		run.appendOutput(stream, scanner.Text())
		m.emitProviderUpdateProgressThrottled(run)
	}
	if err := scanner.Err(); err != nil {
		run.appendOutput("system", "output read error: "+err.Error())
		m.emitProviderUpdateProgressThrottled(run)
	}
}

func (m *Manager) finishProviderUpdateRun(providerID string, run *providerUpdateRun, exitCode int, status, lastError string) {
	if status == "" {
		status = "done"
	}
	if status != "done" && lastError == "" {
		lastError = "actualizacion fallida"
	}
	run.finish(status, exitCode, lastError)
	tail := run.tailString()

	m.updateMu.Lock()
	if m.providerUpdateRuns[providerID] == run {
		delete(m.providerUpdateRuns, providerID)
	}
	if exitCode == 0 && status == "done" {
		delete(m.providerUpdateFailures, providerID)
	} else {
		m.providerUpdateFailures[providerID] = providerUpdateFailure{
			LastError: redactSensitiveText(firstNonEmpty(lastError, "actualizacion fallida")),
			ExitCode:  exitCode,
			Tail:      redactSensitiveText(tail),
		}
	}
	m.updateMu.Unlock()

	if exitCode == 0 && status == "done" {
		if version, recheckError := m.detectInstalledCLIVersionAfterProviderUpdate(providerID); version != nil {
			m.setProviderCLIVersion(providerID, version)
		} else {
			m.setProviderUpdateRecheckError(providerID, recheckError)
		}
	} else if version := m.detectInstalledCLIVersion(context.Background(), providerID); version != nil {
		m.setProviderCLIVersion(providerID, version)
	}
	list := m.ProvidersList()
	m.emit("providers:list", list)
	m.CheckProviderUpdates(context.Background())
	m.emitProviderUpdateProgress(run.snapshot())
}

func (m *Manager) providerExists(providerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.providers[normalizeProviderID(providerID)] != nil
}

func (m *Manager) setProviderCLIVersion(providerID string, version *CLIVersion) {
	if version == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime := m.providers[normalizeProviderID(providerID)]; runtime != nil {
		runtime.CLIVersion = copyCLIVersion(version)
	}
	m.clearProviderUpdateRecheckError(providerID)
}

func (m *Manager) providerUpdateRunning(providerID string) bool {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	run := m.providerUpdateRuns[normalizeProviderID(providerID)]
	if run == nil {
		return false
	}
	if run.active() {
		return true
	}
	delete(m.providerUpdateRuns, normalizeProviderID(providerID))
	return false
}

func (m *Manager) emitProviderUpdateProgressThrottled(run *providerUpdateRun) {
	now := time.Now()
	run.mu.Lock()
	if now.Sub(run.lastEmit) < providerUpdateProgressThrottle {
		run.mu.Unlock()
		return
	}
	run.lastEmit = now
	progress := run.snapshotLocked()
	run.mu.Unlock()
	m.emitProviderUpdateProgress(progress)
}

func (m *Manager) emitProviderUpdateProgress(progress ProviderUpdateProgress) {
	progress.ProviderID = normalizeProviderID(progress.ProviderID)
	progress.Tail = redactSensitiveText(limitUTF8Bytes(progress.Tail, providerUpdateTailBytes))
	progress.Error = redactSensitiveText(progress.Error)
	m.emit("providers:update-progress", progress)
}

func (m *Manager) killAllProviderUpdates() int {
	m.updateMu.Lock()
	runs := make([]*providerUpdateRun, 0, len(m.providerUpdateRuns))
	for _, run := range m.providerUpdateRuns {
		runs = append(runs, run)
	}
	m.updateMu.Unlock()
	stopped := 0
	for _, run := range runs {
		if run.kill() {
			stopped++
		}
	}
	return stopped
}

func (m *Manager) currentProviderUpdate(ctx context.Context, providerID string) (ProviderUpdate, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if version := m.detectInstalledCLIVersion(ctx, providerID); version != nil {
		m.setProviderCLIVersion(providerID, version)
	}
	candidate, ok := m.providerUpdateCandidate(providerID)
	if !ok {
		return ProviderUpdate{}, false
	}
	latest, err := m.latestCLIVersion(ctx, candidate.spec)
	if err != nil {
		m.logProviderUpdateSkip(candidate.spec.ProviderID, err)
		return ProviderUpdate{}, false
	}
	cmp, ok := compareLenientSemver(candidate.installed, latest)
	if !ok {
		m.logProviderUpdateSkip(candidate.spec.ProviderID, fmt.Errorf("version not comparable: installed=%q latest=%q", candidate.installed, latest))
		return ProviderUpdate{}, false
	}
	update := ProviderUpdate{
		ProviderID:      candidate.spec.ProviderID,
		CLI:             candidate.spec.CLI,
		Installed:       candidate.installed,
		Latest:          latest,
		UpdateAvailable: cmp > 0,
		Hint:            candidate.spec.Hint,
	}
	m.decorateProviderUpdateFailure(&update)
	m.rememberProviderUpdate(update)
	return update, true
}

func (m *Manager) detectInstalledCLIVersion(parent context.Context, providerID string) *CLIVersion {
	version, err := m.detectInstalledCLIVersionWithTimeout(parent, providerID, cliVersionTimeout)
	if err != nil {
		cli, _ := cliVersionCommandForProvider(providerID)
		if m.opts.Logf != nil {
			m.opts.Logf("provider cli version unavailable", map[string]any{
				"provider": normalizeProviderID(providerID),
				"cli":      cli,
				"error":    redactSensitiveText(err.Error()),
			})
		}
		return nil
	}
	return version
}

func (m *Manager) detectInstalledCLIVersionWithTimeout(parent context.Context, providerID string, timeout time.Duration) (*CLIVersion, error) {
	_, ok := cliVersionCommandForProvider(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown provider cli: %s", normalizeProviderID(providerID))
	}
	resolved, err := m.providerCLIExecutable(providerID)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		timeout = cliVersionTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "--version")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := err.Error()
		if text := strings.TrimSpace(stderr.String()); text != "" {
			detail += ": " + text
		}
		return nil, fmt.Errorf("%s", redactSensitiveText(detail))
	}
	raw := redactSensitiveText(strings.TrimSpace(stdout.String()))
	if raw == "" {
		return nil, fmt.Errorf("%s --version returned empty output", normalizeProviderID(providerID))
	}
	return &CLIVersion{Version: parseSemverToken(raw), Raw: raw}, nil
}

func (m *Manager) providerCLIExecutable(providerID string) (string, error) {
	id := normalizeProviderID(providerID)
	provider, ok := m.providerSnapshot(id)
	if !ok {
		return "", fmt.Errorf("unknown provider cli: %s", id)
	}
	return resolveProviderExecutable(provider)
}

func sameExecutableName(command, name string) bool {
	command = strings.TrimSpace(command)
	name = strings.TrimSpace(name)
	if command == "" || name == "" {
		return false
	}
	base := strings.ToLower(filepath.Base(command))
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, ".cmd")
	want := strings.ToLower(filepath.Base(name))
	want = strings.TrimSuffix(want, ".exe")
	want = strings.TrimSuffix(want, ".cmd")
	return base == want
}

func (m *Manager) detectInstalledCLIVersionAfterProviderUpdate(providerID string) (*CLIVersion, string) {
	var lastErr error
	for attempt := 0; attempt <= len(postUpdateCLIVersionBackoffs); attempt++ {
		version, err := m.detectInstalledCLIVersionWithTimeout(context.Background(), providerID, postUpdateCLIVersionTimeout)
		if err == nil && version != nil && version.Version != "" {
			return version, ""
		}
		if err == nil {
			err = fmt.Errorf("%s --version output did not contain a comparable version", normalizeProviderID(providerID))
		}
		lastErr = err
		if attempt < len(postUpdateCLIVersionBackoffs) {
			backoff := postUpdateCLIVersionBackoffs[attempt]
			m.logProviderUpdateRecheckRetry(providerID, attempt+1, backoff, err)
			if backoff > 0 {
				time.Sleep(backoff)
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("post-update version recheck failed")
	}
	return nil, redactSensitiveText(lastErr.Error())
}

func (m *Manager) logProviderUpdateRecheckRetry(providerID string, retry int, backoff time.Duration, err error) {
	if m.opts.Logf == nil {
		return
	}
	fields := map[string]any{
		"provider":  normalizeProviderID(providerID),
		"retry":     retry,
		"backoffMs": backoff.Milliseconds(),
		"timeoutMs": postUpdateCLIVersionTimeout.Milliseconds(),
	}
	if err != nil {
		fields["error"] = redactSensitiveText(err.Error())
	}
	m.opts.Logf("provider update version recheck retry", fields)
}

func copyCLIVersion(in *CLIVersion) *CLIVersion {
	if in == nil {
		return nil
	}
	out := *in
	out.Raw = redactSensitiveText(out.Raw)
	return &out
}

func parseSemverToken(raw string) string {
	match := semverTokenRE.FindStringSubmatch(strings.TrimSpace(raw))
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func (m *Manager) CheckProviderUpdates(ctx context.Context) ProviderUpdatesPayload {
	payload, _, _ := m.checkProviderUpdates(ctx)
	return payload
}

func (m *Manager) checkProviderUpdates(ctx context.Context) (ProviderUpdatesPayload, bool, bool) {
	payload, hasCandidates, hasFailures := m.providerUpdatesPayload(ctx)
	if hasCandidates {
		m.emit("providers:updates", payload)
	}
	return payload, hasCandidates, hasFailures
}

func (m *Manager) providerUpdatesPayload(ctx context.Context) (ProviderUpdatesPayload, bool, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	candidates := m.providerUpdateCandidates()
	payload := ProviderUpdatesPayload{
		CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Updates:   []ProviderUpdate{},
	}
	hasFailures := false
	for _, candidate := range candidates {
		latest, err := m.latestCLIVersion(ctx, candidate.spec)
		if err != nil {
			hasFailures = true
			if fallback, ok := m.failedProviderUpdateFallback(candidate.spec.ProviderID); ok {
				payload.Updates = append(payload.Updates, fallback)
			}
			m.logProviderUpdateSkip(candidate.spec.ProviderID, err)
			continue
		}
		cmp, ok := compareLenientSemver(candidate.installed, latest)
		if !ok {
			hasFailures = true
			m.logProviderUpdateSkip(candidate.spec.ProviderID, fmt.Errorf("version not comparable: installed=%q latest=%q", candidate.installed, latest))
			continue
		}
		update := ProviderUpdate{
			ProviderID:      candidate.spec.ProviderID,
			CLI:             candidate.spec.CLI,
			Installed:       candidate.installed,
			Latest:          latest,
			UpdateAvailable: cmp > 0,
			Hint:            candidate.spec.Hint,
		}
		m.decorateProviderUpdateFailure(&update)
		m.rememberProviderUpdate(update)
		m.notifyProviderUpdateAvailable(update)
		payload.Updates = append(payload.Updates, update)
	}
	return payload, len(candidates) > 0, hasFailures
}

func (m *Manager) providerUpdateCandidate(providerID string) (providerUpdateCandidate, bool) {
	providerID = normalizeProviderID(providerID)
	m.mu.Lock()
	defer m.mu.Unlock()
	runtime := m.providers[providerID]
	if runtime == nil || runtime.CLIVersion == nil || runtime.CLIVersion.Version == "" {
		return providerUpdateCandidate{}, false
	}
	spec, ok := cliUpdateSpecForProvider(providerID, m.opts.ProviderUpdateSources)
	if !ok {
		return providerUpdateCandidate{}, false
	}
	if _, ok := parseLenientSemver(runtime.CLIVersion.Version); !ok {
		return providerUpdateCandidate{}, false
	}
	return providerUpdateCandidate{spec: spec, installed: runtime.CLIVersion.Version}, true
}

func (m *Manager) providerUpdateCandidates() []providerUpdateCandidate {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]providerUpdateCandidate, 0, len(m.providerOrder))
	for _, id := range m.providerOrder {
		runtime := m.providers[id]
		if runtime == nil || runtime.CLIVersion == nil || runtime.CLIVersion.Version == "" {
			continue
		}
		spec, ok := cliUpdateSpecForProvider(id, m.opts.ProviderUpdateSources)
		if !ok {
			continue
		}
		if _, ok := parseLenientSemver(runtime.CLIVersion.Version); !ok {
			continue
		}
		out = append(out, providerUpdateCandidate{spec: spec, installed: runtime.CLIVersion.Version})
	}
	return out
}

func (m *Manager) decorateProviderUpdateFailure(update *ProviderUpdate) {
	if update == nil {
		return
	}
	m.updateMu.Lock()
	failure, ok := m.providerUpdateFailures[update.ProviderID]
	recheckError := m.providerUpdateRechecks[update.ProviderID]
	m.updateMu.Unlock()
	if ok {
		update.UpdateAvailable = true
		update.LastError = failure.LastError
		update.ExitCode = intPtr(failure.ExitCode)
		update.Tail = failure.Tail
	}
	if recheckError != "" {
		update.UpdateAvailable = true
		update.RecheckError = recheckError
	}
}

func (m *Manager) rememberProviderUpdate(update ProviderUpdate) {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	update.LastError = ""
	update.ExitCode = nil
	update.Tail = ""
	update.RecheckError = ""
	m.providerUpdateLastKnown[update.ProviderID] = update
}

// notifyProviderUpdateAvailable emits one user-facing availability event for a
// provider/version pair. The renderer turns that into a desktop notification
// when the user enabled them, or an in-app toast otherwise. The remembered
// version lives in the daemon-owned provider cache so repeating an hourly check
// (or restarting Workass) never produces notification spam.
func (m *Manager) notifyProviderUpdateAvailable(update ProviderUpdate) {
	if !update.UpdateAvailable || strings.TrimSpace(update.ProviderID) == "" || strings.TrimSpace(update.Latest) == "" {
		return
	}
	providerID := normalizeProviderID(update.ProviderID)
	latest := strings.TrimSpace(update.Latest)
	m.mu.Lock()
	runtime := m.providers[providerID]
	if runtime == nil || runtime.Config.LastUpdateNotice == latest {
		m.mu.Unlock()
		return
	}
	name := strings.TrimSpace(runtime.Config.Name)
	runtime.Config.LastUpdateNotice = latest
	providers := m.providerRecordsLocked()
	filePath := m.providerConfigFile
	m.mu.Unlock()
	if err := SaveProviderConfigs(filePath, providers); err != nil && m.opts.Logf != nil {
		m.opts.Logf("provider update notice persist failed", map[string]any{
			"provider": providerID,
			"error":    redactSensitiveText(err.Error()),
		})
	}
	if name == "" {
		name = strings.TrimSpace(update.CLI)
	}
	if name == "" {
		name = providerID
	}
	m.emit("notify", map[string]any{
		"title": name + " tiene una actualización",
		"body":  "Versión " + latest + " disponible (instalada " + update.Installed + ").",
	})
}

func (m *Manager) setProviderUpdateRecheckError(providerID, errText string) {
	errText = redactSensitiveText(strings.TrimSpace(errText))
	if errText == "" {
		errText = "post-update version recheck failed"
	}
	m.updateMu.Lock()
	m.providerUpdateRechecks[normalizeProviderID(providerID)] = errText
	m.updateMu.Unlock()
}

func (m *Manager) clearProviderUpdateRecheckError(providerID string) {
	m.updateMu.Lock()
	delete(m.providerUpdateRechecks, normalizeProviderID(providerID))
	m.updateMu.Unlock()
}

func (m *Manager) failedProviderUpdateFallback(providerID string) (ProviderUpdate, bool) {
	providerID = normalizeProviderID(providerID)
	m.updateMu.Lock()
	update, ok := m.providerUpdateLastKnown[providerID]
	_, failed := m.providerUpdateFailures[providerID]
	m.updateMu.Unlock()
	if !ok || !failed {
		return ProviderUpdate{}, false
	}
	m.decorateProviderUpdateFailure(&update)
	return update, true
}

func providerUpdateError(code, message string, fields map[string]any) error {
	return chatStructuredError{Code: code, Message: message, Fields: fields}
}

func (m *Manager) latestCLIVersion(ctx context.Context, spec providerUpdateSpec) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, m.opts.ProviderUpdateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, spec.Source, nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("registry status %d", res.StatusCode)
	}
	var body struct {
		Version string `json:"version"`
	}
	dec := json.NewDecoder(res.Body)
	if err := dec.Decode(&body); err != nil {
		return "", err
	}
	latest := parseSemverToken(body.Version)
	if latest == "" {
		return "", fmt.Errorf("registry version not comparable: %q", body.Version)
	}
	return latest, nil
}

func (m *Manager) logProviderUpdateSkip(providerID string, err error) {
	if m.opts.Logf == nil || err == nil {
		return
	}
	m.opts.Logf("provider update check skipped", map[string]any{
		"provider": providerID,
		"error":    redactSensitiveText(err.Error()),
	})
}

func compareLenientSemver(installed, latest string) (int, bool) {
	current, ok := parseLenientSemver(installed)
	if !ok {
		return 0, false
	}
	target, ok := parseLenientSemver(latest)
	if !ok {
		return 0, false
	}
	switch {
	case target.major != current.major:
		return compareInts(target.major, current.major), true
	case target.minor != current.minor:
		return compareInts(target.minor, current.minor), true
	case target.patch != current.patch:
		return compareInts(target.patch, current.patch), true
	default:
		return comparePrerelease(target.pre, current.pre), true
	}
}

func parseLenientSemver(raw string) (lenientSemver, bool) {
	token := parseSemverToken(raw)
	if token == "" {
		return lenientSemver{}, false
	}
	main := token
	if i := strings.IndexByte(main, '+'); i >= 0 {
		main = main[:i]
	}
	var pre []string
	if i := strings.IndexByte(main, '-'); i >= 0 {
		pre = strings.Split(main[i+1:], ".")
		main = main[:i]
	}
	parts := strings.Split(main, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return lenientSemver{}, false
	}
	nums := []int{0, 0, 0}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return lenientSemver{}, false
		}
		nums[i] = n
	}
	return lenientSemver{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

func compareInts(a, b int) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	default:
		return 0
	}
}

func comparePrerelease(latest, installed []string) int {
	if len(latest) == 0 && len(installed) == 0 {
		return 0
	}
	if len(latest) == 0 {
		return 1
	}
	if len(installed) == 0 {
		return -1
	}
	for i := 0; i < len(latest) && i < len(installed); i++ {
		cmp := comparePrereleaseIdentifier(latest[i], installed[i])
		if cmp != 0 {
			return cmp
		}
	}
	return compareInts(len(latest), len(installed))
}

func comparePrereleaseIdentifier(a, b string) int {
	an, aNum := parseNumericIdentifier(a)
	bn, bNum := parseNumericIdentifier(b)
	switch {
	case aNum && bNum:
		return compareInts(an, bn)
	case aNum:
		return -1
	case bNum:
		return 1
	case a > b:
		return 1
	case a < b:
		return -1
	default:
		return 0
	}
}

func parseNumericIdentifier(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil
}

func (m *Manager) emitMockAppUpdateFromEnv() {
	version, ok := os.LookupEnv(mockUpdateEnv)
	if !ok {
		return
	}
	version = redactSensitiveText(strings.TrimSpace(version))
	if version == "" {
		return
	}
	m.emit("app:update", map[string]any{"version": version, "mocked": true})
}
