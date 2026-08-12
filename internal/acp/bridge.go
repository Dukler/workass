package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Bridge struct {
	key          string
	providerID   string
	providerName string
	manager      *Manager
	opts         Options

	mu                sync.Mutex
	child             *exec.Cmd
	childExited       chan struct{}
	processTree       processTreeHandle
	stdin             io.WriteCloser
	nextID            int64
	pending           map[string]*pendingRequest
	closed            bool
	startedAt         time.Time
	finishedAt        time.Time
	stderrTail        []byte
	initialized       bool
	initializingMu    sync.Mutex
	state             EngineState
	pinned            bool
	lastActivity      time.Time
	lastChunkUnixNano int64
	rssKb             int
	recycleAtIdle     bool
	recycleReason     string
	spare             bool
	procID            string
	tabID             string
	chatID            string
	cwd               string
	agentOwnerKey     string

	// Subagent labeling: toolCallId → title, remembered so any child tool events
	// (subagent calls that carry _meta.claudeCode.parentToolUseId == this id) can
	// be labeled with the spawning call's title (e.g. a Task's description).
	// Guarded by mu because request and notification handling can overlap.
	subagentTitles map[string]string

	agentName    string
	agentCaps    map[string]any
	agentMeta    map[string]any
	models       []Model
	modes        []Mode
	currentModel *string
	currentMode  *string
	// Claude's adapter can report currentValue "default" for a synthetic row
	// that aliases one explicit model. This is populated only from a unique
	// metadata match in the unfiltered provider catalog and is guarded by mu.
	syntheticDefaultModelAlias string
	// Frontier adapters expose reasoning effort as a SEPARATE config option
	// (Claude: "effort", Codex: "reasoning_effort") orthogonal to the model.
	// Workass composes base[effort] only in its persisted/UI selection, then splits
	// it back into the adapter-native writes. Config ids are retained because an
	// option may be categorized as mode/thought_level without literally using the
	// ids "mode" or "effort".
	efforts        []string
	currentEffort  *string
	effortConfigID string
	modeConfigID   string
	imageSupport   bool
	// Effort config options are model-specific. A present key with an empty
	// slice means the adapter authoritatively omitted the effort axis for that
	// model (Claude Haiku); absence means the model has not been observed yet.
	axisEffortsByModel map[string][]string
	// Some adapters encode effort directly in model ids instead of exposing a
	// separate config axis. Keep that provenance separate so m[high] continues
	// to be written as a model id while Claude/Codex write model + effort.
	variantEffortsByModel map[string][]string
	modelWriteInFlight    map[string]int
	lastWorkassModelWrite map[string]string
	durableModelSelection map[string]string

	sessions       map[string]struct{}
	seededSessions map[string]struct{}
	jobsBySession  map[string]*Job
	promptMu       sync.Mutex
	writeMu        sync.Mutex
}

type pendingRequest struct {
	method  string
	timer   *time.Timer
	resolve chan rpcResult
}

type rpcResult struct {
	value map[string]any
	err   error
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcCall struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`
	Params  any    `json:"params,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

type acpError struct {
	Code int
	Data any
	Msg  string
}

var errBridgeHibernated = errors.New("ACP bridge hibernated; session restore required")

func (e *acpError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("ACP error %d", e.Code)
}

func newBridge(key string, opts Options, manager *Manager) *Bridge {
	if key == "" {
		key = "default"
	}
	opts = opts.withDefaults()
	providerID := normalizeProviderID(opts.Provider.ID)
	if providerID == "" {
		providerID = normalizeProviderID(opts.DefaultProviderID)
	}
	if providerID == "" {
		providerID = defaultFixtureProviderID()
	}
	providerName := firstNonEmpty(opts.Provider.Name, opts.Provider.Label, providerID)
	now := time.Now()
	return &Bridge{
		key:                   key,
		providerID:            providerID,
		providerName:          providerName,
		manager:               manager,
		opts:                  opts,
		pending:               make(map[string]*pendingRequest),
		agentName:             firstNonEmpty(opts.Provider.Label, opts.Provider.Name),
		agentCaps:             make(map[string]any),
		agentMeta:             make(map[string]any),
		axisEffortsByModel:    make(map[string][]string),
		variantEffortsByModel: make(map[string][]string),
		modelWriteInFlight:    make(map[string]int),
		lastWorkassModelWrite: make(map[string]string),
		durableModelSelection: make(map[string]string),
		state:                 StateWarm,
		lastActivity:          now,
		procID:                "engine-" + safeProcessID(key),
		sessions:              make(map[string]struct{}),
		seededSessions:        make(map[string]struct{}),
		jobsBySession:         make(map[string]*Job),
	}
}

func (b *Bridge) Key() string {
	return b.key
}

func (b *Bridge) ProviderID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.providerID
}

func (b *Bridge) Closed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *Bridge) Hibernated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == StateHibernated
}

func (b *Bridge) State() EngineState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Bridge) Initialize(ctx context.Context) (InitResult, error) {
	b.initializingMu.Lock()
	defer b.initializingMu.Unlock()

	b.mu.Lock()
	if b.state == StateHibernated {
		b.mu.Unlock()
		return InitResult{}, errBridgeHibernated
	}
	if b.initialized && !b.closed {
		res := InitResult{ProtocolVersion: ProtocolVersion, AgentName: b.agentName}
		b.mu.Unlock()
		return res, nil
	}
	b.mu.Unlock()

	if err := b.start(); err != nil {
		return InitResult{}, err
	}

	res, err := b.request(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientInfo": map[string]any{
			"name":    "workass",
			"version": b.opts.Version,
		},
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  false,
				"writeTextFile": false,
			},
			"terminal": false,
		},
	}, b.opts.InitTimeout)
	if err != nil {
		err = b.withStderrTail(err)
		b.Close(false, err)
		return InitResult{}, err
	}

	agentName := firstNonEmpty(b.opts.Provider.Label, b.opts.Provider.Name, b.providerName)
	if info, ok := res["agentInfo"].(map[string]any); ok {
		if name := asString(info["name"]); name != "" {
			agentName = name
		}
	}
	protocolVersion := ProtocolVersion
	if n, ok := res["protocolVersion"].(json.Number); ok {
		if v, err := strconv.Atoi(n.String()); err == nil {
			protocolVersion = v
		}
	} else if f, ok := res["protocolVersion"].(float64); ok {
		protocolVersion = int(f)
	}
	agentCaps := mapFromAny(res["agentCapabilities"])
	agentMeta := mapFromAny(res["_meta"])
	if len(agentMeta) == 0 {
		agentMeta = mapFromAny(agentCaps["_meta"])
	}
	imageSupport := false
	if caps := agentCaps; caps != nil {
		if prompt, ok := caps["promptCapabilities"].(map[string]any); ok {
			if v, ok := prompt["image"].(bool); ok {
				imageSupport = v
			}
		}
	}

	b.mu.Lock()
	b.agentName = agentName
	b.agentCaps = agentCaps
	b.agentMeta = agentMeta
	b.imageSupport = imageSupport
	b.initialized = true
	b.mu.Unlock()

	return InitResult{ProtocolVersion: protocolVersion, AgentName: agentName, Raw: res}, nil
}

func (b *Bridge) start() error {
	b.mu.Lock()
	if b.state == StateHibernated {
		b.mu.Unlock()
		return errBridgeHibernated
	}
	if b.child != nil && !b.closed {
		b.mu.Unlock()
		return nil
	}
	if b.closed {
		b.mu.Unlock()
		return errors.New("ACP bridge closed")
	}
	providerConfig := b.opts.Provider
	b.mu.Unlock()
	provider, err := providerAdapterForID(b.providerID).launch.Prepare(providerConfig, b.opts)
	if err != nil {
		return err
	}

	cmd := managedCommand(provider.Command, provider.Args...)
	cmd.Dir = provider.CWD
	env := provider.Env
	if strings.HasPrefix(strings.TrimSpace(b.chatID), subagentChatIDPrefix) {
		// A delegated child is not the user. It must not inherit the user's
		// personal instruction file or their auto-memory: the memory index is
		// theirs, one entry of it is explicitly marked "private — never put it
		// in a subagent brief", and the brief honoured that while the harness
		// delivered it anyway. The repo's own CLAUDE.md still loads, because
		// the binding laws a child works under are a property of the repo.
		env = copyStringMap(env)
		if env == nil {
			env = map[string]string{}
		}
		env["WORKASS_SUBAGENT"] = "1"
		env["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] = "1"
	}
	cmd.Env = mergedEnv(env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	processTree, err := startProcessTree(cmd)
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return fmt.Errorf("attach ACP process tree: %w", err)
	}

	childExited := make(chan struct{})
	b.mu.Lock()
	b.child = cmd
	b.childExited = childExited
	b.processTree = processTree
	b.stdin = stdin
	b.startedAt = time.Now()
	b.finishedAt = time.Time{}
	b.stderrTail = nil
	b.initialized = false
	b.state = StateWarm
	b.pinned = false
	b.lastActivity = b.startedAt
	b.rssKb = 0
	b.recycleAtIdle = false
	b.recycleReason = ""
	b.mu.Unlock()
	go b.readStdout(stdout)
	go b.readStderr(stderr)
	go b.waitChild(cmd, childExited)
	b.manager.bridgeChanged(b, "spawn")
	return nil
}

func mergedEnv(extra map[string]string) []string {
	env := map[string]string{}
	for _, item := range os.Environ() {
		if k, v, ok := strings.Cut(item, "="); ok {
			env[k] = v
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func (b *Bridge) readStdout(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			b.acceptLine(bytes.TrimSpace(line))
		}
		if err != nil {
			return
		}
	}
}

func (b *Bridge) readStderr(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.appendStderrTail(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (b *Bridge) appendStderrTail(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stderrTail = append(b.stderrTail, chunk...)
	if len(b.stderrTail) > b.opts.StderrTailBytes {
		b.stderrTail = append([]byte(nil), b.stderrTail[len(b.stderrTail)-b.opts.StderrTailBytes:]...)
	}
}

func (b *Bridge) waitChild(cmd *exec.Cmd, childExited chan struct{}) {
	err := cmd.Wait()
	code, signal := exitCodeSignal(cmd.ProcessState, err)
	uptime := time.Duration(0)
	processTree := processTreeHandle{}
	b.mu.Lock()
	if !b.startedAt.IsZero() {
		uptime = time.Since(b.startedAt)
	}
	tail := redactSensitiveText(string(b.stderrTail))
	alreadyClosed := b.closed || b.child != cmd || b.state == StateHibernated
	if b.child == cmd {
		processTree = b.processTree
		b.processTree = processTreeHandle{}
	}
	rssKb := b.rssKb
	b.mu.Unlock()
	_ = releaseProcessTree(processTree)
	// Close is the durable teardown receipt. Publish it only after Wait has
	// reaped the native host and the process-tree handle has been released, so
	// callers cannot remove StateDir while a provider is still finishing a
	// checkpoint write. This happens before unexpected-exit handling because
	// that path calls Bridge.Close and must never wait on its own goroutine.
	close(childExited)

	fields := map[string]any{
		"key":      b.key,
		"code":     code,
		"signal":   signal,
		"uptimeMs": uptime.Milliseconds(),
		"rssKb":    rssKb,
	}
	if tail != "" {
		fields["stderrTail"] = tail
		fields["stderrTailBytes"] = len(tail)
	}
	b.opts.Logf("acp engine exited", fields)
	if !alreadyClosed {
		cause := fmt.Errorf("ACP server exited (%s%d)", signalPrefix(signal), code)
		cause = b.withStderrTail(cause)
		if b.manager != nil {
			b.manager.handleUnexpectedBridgeExit(b, cause)
		} else {
			b.Close(false, cause)
		}
	}
}

func signalPrefix(signal string) string {
	if signal == "" {
		return "code "
	}
	return signal + " "
}

func exitCodeSignal(state *os.ProcessState, err error) (int, string) {
	if state == nil {
		return -1, ""
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return state.ExitCode(), ""
	}
	if status.Signaled() {
		return -1, status.Signal().String()
	}
	return status.ExitStatus(), ""
}

func (b *Bridge) acceptLine(line []byte) {
	if len(line) == 0 {
		return
	}
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		b.opts.Logf("acp stdout invalid json", map[string]any{"key": b.key, "bytes": len(line)})
		return
	}
	var method string
	if raw := fields["method"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &method)
	}
	rawID, hasID := fields["id"]
	if hasID && method == "" {
		b.handleResponse(rawID, fields["result"], fields["error"])
		return
	}
	if hasID && method != "" {
		params := decodeObject(fields["params"])
		go b.handleRequest(rawID, method, params)
		return
	}
	if method != "" {
		b.handleNotification(method, decodeObject(fields["params"]))
	}
}

func (b *Bridge) handleResponse(rawID, rawResult, rawError json.RawMessage) {
	key := string(rawID)
	b.mu.Lock()
	pending := b.pending[key]
	if pending != nil {
		delete(b.pending, key)
	}
	b.mu.Unlock()
	if pending == nil {
		return
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}
	if len(rawError) > 0 && !bytes.Equal(bytes.TrimSpace(rawError), []byte("null")) {
		var rpcErr rpcError
		dec := json.NewDecoder(bytes.NewReader(rawError))
		dec.UseNumber()
		_ = dec.Decode(&rpcErr)
		pending.resolve <- rpcResult{err: &acpError{Code: rpcErr.Code, Data: rpcErr.Data, Msg: rpcErr.Message}}
		return
	}
	pending.resolve <- rpcResult{value: decodeObject(rawResult)}
}

func decodeObject(raw json.RawMessage) map[string]any {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]any{}
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func (b *Bridge) request(ctx context.Context, method string, params any, timeout time.Duration) (map[string]any, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("ACP server closed")
	}
	b.nextID++
	id := b.nextID
	key := strconv.FormatInt(id, 10)
	resCh := make(chan rpcResult, 1)
	pending := &pendingRequest{method: method, resolve: resCh}
	if timeout > 0 {
		pending.timer = time.AfterFunc(timeout, func() {
			b.mu.Lock()
			if b.pending[key] == pending {
				delete(b.pending, key)
				b.mu.Unlock()
				resCh <- rpcResult{err: fmt.Errorf("ACP timeout: %s", method)}
				return
			}
			b.mu.Unlock()
		})
	}
	b.pending[key] = pending
	b.mu.Unlock()

	if err := b.write(rpcCall{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		b.mu.Lock()
		if b.pending[key] == pending {
			delete(b.pending, key)
		}
		b.mu.Unlock()
		if pending.timer != nil {
			pending.timer.Stop()
		}
		return nil, err
	}
	select {
	case <-ctx.Done():
		b.mu.Lock()
		if b.pending[key] == pending {
			delete(b.pending, key)
		}
		b.mu.Unlock()
		if pending.timer != nil {
			pending.timer.Stop()
		}
		return nil, ctx.Err()
	case res := <-resCh:
		return res.value, res.err
	}
}

func (b *Bridge) notify(method string, params any) bool {
	if err := b.write(rpcCall{JSONRPC: "2.0", Method: method, Params: params}); err != nil {
		b.opts.Logf("acp notify dropped", map[string]any{"key": b.key, "method": method, "error": err.Error()})
		return false
	}
	return true
}

func (b *Bridge) write(payload any) error {
	b.mu.Lock()
	closed := b.closed
	stdin := b.stdin
	b.mu.Unlock()
	if closed || stdin == nil {
		return errors.New("ACP stdin closed")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_, err = stdin.Write(data)
	return err
}

func (b *Bridge) handleRequest(id json.RawMessage, method string, params map[string]any) {
	if job := b.jobForSession(asString(params["sessionId"])); job != nil {
		job.touchActivity()
	}
	var result any
	var err error
	switch method {
	case "session/request_permission":
		result, err = b.handlePermissionRequest(params)
	case "fs/read_text_file", "fs/write_text_file":
		err = &acpError{Code: -32601, Msg: "File-system client capability is disabled"}
	default:
		err = &acpError{Code: -32601, Msg: "Unsupported ACP client method: " + method}
	}
	if err != nil {
		code := -32603
		if ae, ok := err.(*acpError); ok {
			code = ae.Code
		}
		_ = b.write(rpcCall{JSONRPC: "2.0", ID: json.RawMessage(id), Error: rpcError{Code: code, Message: err.Error()}})
		return
	}
	_ = b.write(rpcCall{JSONRPC: "2.0", ID: json.RawMessage(id), Result: result})
}

func (b *Bridge) handlePermissionRequest(params map[string]any) (any, error) {
	sessionID := asString(params["sessionId"])
	job := b.jobForSession(sessionID)
	options, _ := params["options"].([]any)
	if job == nil || job.cancelled {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
	job.waitingPermission.Store(true)
	defer func() {
		job.waitingPermission.Store(false)
		job.touchActivity()
	}()
	toolCall := mapFromAny(params["toolCall"])
	permissionTitle := firstNonEmpty(asString(toolCall["title"]), asString(toolCall["kind"]), "action")
	b.manager.notifySubagentPermissionForJob(job, permissionTitle)
	defer b.manager.resolveSubagentPermissionForJob(job)

	var fallback string
	for _, raw := range options {
		opt, _ := raw.(map[string]any)
		kind := asString(opt["kind"])
		name := asString(opt["name"])
		if kind == "reject_once" || kind == "reject_always" || strings.Contains(strings.ToLower(kind+" "+name), "reject") || strings.Contains(strings.ToLower(kind+" "+name), "deny") {
			fallback = asString(opt["optionId"])
			break
		}
	}
	if fallback == "" && len(options) > 0 {
		if opt, ok := options[0].(map[string]any); ok {
			fallback = asString(opt["optionId"])
		}
	}
	optionID := b.manager.requestPermission(permissionRequest{
		JobID:             firstNonEmpty(job.VisibleJobID, job.ID),
		SessionID:         sessionID,
		ToolCall:          toolCall,
		Options:           options,
		FallbackOptionID:  fallback,
		PermissionTimeout: b.opts.PermissionTimeout,
		Subagent:          job.SubagentID != "",
	})
	if optionID == "" {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}, nil
}

func mapFromAny(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func (b *Bridge) handleNotification(method string, params map[string]any) {
	if method != "session/update" {
		return
	}
	sessionID := asString(params["sessionId"])
	job := b.jobForSession(sessionID)
	update := mapFromAny(params["update"])
	kind := asString(update["sessionUpdate"])
	if job != nil {
		job.touchActivity()
	}
	switch kind {
	case "agent_message_chunk":
		if job != nil {
			if text := textFromContent(update["content"]); text != "" {
				b.manager.updateSubagentActivityForJob(job, "working", "Writing response")
				b.queueStdout(job, text, providerAdapterForID(b.providerID).delivery.AssistantPhase(update))
			}
			b.publishAssistantImages(job, toolImagesFromContent(update["content"]))
		}
	case "agent_thought_chunk":
		if text := strings.TrimSpace(textFromContent(update["content"])); text != "" && job != nil && !job.internal {
			b.manager.updateSubagentActivityForJob(job, "thinking", "Reasoning")
			b.queueThinking(job, text)
		}
	case "tool_call", "tool_call_update":
		if job != nil && !job.internal {
			parent := metaParentToolUseID(mapFromAny(update["_meta"]), mapFromAny(params["_meta"]))
			b.emitToolEvent(job, kind, update, parent)
		}
	case "_workass_claude_spawned_work":
		// The Claude native host sees the official SDK's structured task lifecycle but the
		// upstream adapter intentionally drops it. Workass's exact-version patch
		// forwards only the bounded task fields in this additive update. It may
		// arrive after the user turn ended, so it must not require a live job.
		if b.manager.acceptsNativeSpawnedWorkProvider(b.ProviderID()) {
			tabID, chatID := b.chatIdentity()
			b.manager.observeClaudeSpawnedWork(tabID, chatID, sessionID, mapFromAny(update["event"]))
		}
	case "_workass_claude_turn":
		// The harness's own turn lifecycle. Like spawned work above it must not
		// require a live job — a turn the harness starts on its own is precisely
		// the case where no job exists yet, and refusing it here is what made
		// that turn invisible.
		if b.manager.acceptsNativeSpawnedWorkProvider(b.ProviderID()) {
			tabID, chatID := b.chatIdentity()
			b.manager.observeClaudeTurn(b, tabID, chatID, sessionID, update)
		}
	case "_workass_claude_commands":
		// Claude's slash-command catalog changed mid-session (commands_changed
		// push or an engine restart re-announcing its truth). Chat-level data:
		// like _workass_claude_turn above it must never require a live job — it
		// arrives between turns. applyCommandCatalog gates on the claude
		// provider and keys by chat identity; old daemons fall into default:
		// and ignore the kind, which is what keeps this additive-safe.
		b.applyCommandCatalog(sessionID, update["commandCatalog"])
	case "_workass_claude_provider_session":
		// Claude's fork family (/clear, forkSession, conversation_reset) moved
		// the conversation under a new provider id; record it so the next
		// native restore resumes the transcript that actually has the
		// post-fork turns (hostile-fixture finding, 2026-07-28).
		if providerSessionID := strings.TrimSpace(asString(update["providerSessionId"])); providerSessionID != "" {
			tabID, chatID := b.chatIdentity()
			if lane := b.manager.providerLaneForSessionID(sessionID); lane != nil {
				if err := lane.advanceLineage(
					asString(update["previousProviderSessionId"]), providerSessionID,
					uint64(numberOrZero(update["lineageGeneration"])), asString(update["lineageProof"]),
				); err != nil {
					b.opts.Logf("provider lineage rejected", map[string]any{"providerId": b.ProviderID(), "error": err.Error()})
					go b.Close(false, err)
				}
			} else if strings.HasPrefix(chatID, subagentChatIDPrefix) && b.manager.nativeSessions != nil {
				// Delegated child engines are executor-owned and do not have a
				// user-chat actor. Keep their exact native binding isolated here.
				b.manager.nativeSessions.adoptProviderSession(
					tabID, chatID, b.ProviderID(), sessionID,
					asString(update["previousProviderSessionId"]), providerSessionID,
					uint64(numberOrZero(update["lineageGeneration"])), asString(update["lineageProof"]),
				)
			} else {
				err := errors.New("chat-scoped provider lineage has no authoritative actor lane")
				b.opts.Logf("provider lineage rejected", map[string]any{"providerId": b.ProviderID(), "error": err.Error()})
				go b.Close(false, err)
			}
		}
	case "_workass_claude_turn_heartbeat":
		// Turn liveness: a max-effort turn can think silently for minutes and
		// was indistinguishable from a dead chat (2026-07-27). Additive job
		// event; a renderer that does not know the kind ignores it.
		if job != nil && !job.internal {
			b.manager.emit("job:event", map[string]any{
				"type": "acp", "id": job.ID,
				"event": map[string]any{
					"kind":         "turn-heartbeat",
					"elapsedMs":    update["elapsedMs"],
					"outputTokens": update["outputTokens"],
					"phase":        update["phase"],
					"toolName":     update["toolName"],
					"retry":        update["retry"],
				},
			})
		}
	case "plan":
		if entries, ok := update["entries"].([]any); ok && job != nil && !job.internal {
			b.manager.updateSubagentActivityForJob(job, "planning", "Updating plan")
			out := make([]any, 0, len(entries))
			for _, raw := range entries {
				entry := mapFromAny(raw)
				out = append(out, map[string]any{"status": entry["status"], "content": entry["content"]})
			}
			b.manager.emit("job:event", map[string]any{"type": "acp", "id": job.ID, "event": map[string]any{"kind": "plan", "entries": out}})
		}
	case "usage_update":
		payload := b.manager.recordUsageUpdate(sessionID, b.ProviderID(), update)
		tabID, chatID := b.chatIdentity()
		payload["tabId"] = nullableString(tabID)
		payload["chatId"] = nullableString(chatID)
		if job == nil || !job.internal {
			b.manager.emit("job:event", payload)
		}
	case "config_options_update":
		b.applyConfigOptionsForSession(sessionID, update["configOptions"], true, false)
	case "_workass_codex_steer_consumed":
		// Codex's turn/steer response acknowledges admission. The canonical
		// userMessage item is the later proof that the active turn actually
		// consumed that input. Persist the receipt on the job as well as emitting
		// it so reconnecting controllers cannot strand an acknowledged bubble.
		clientUserMessageID := strings.TrimSpace(asString(update["clientUserMessageId"]))
		if job != nil && job.markSteerConsumed(clientUserMessageID) && !job.internal {
			b.manager.emit("job:event", map[string]any{
				"type": "acp", "id": job.ID,
				"event": map[string]any{
					"kind": "steer-consumed", "clientUserMessageId": clientUserMessageID,
				},
			})
		}
	case "_workass_claude_steer_consumed":
		// Claude live steering is accepted when the adapter pushes the direction
		// into the SDK streaming input. The later echoed user message proves the
		// live query actually consumed it.
		clientUserMessageID := strings.TrimSpace(asString(update["clientUserMessageId"]))
		if job != nil && job.markSteerConsumed(clientUserMessageID) && !job.internal {
			b.manager.emit("job:event", map[string]any{
				"type": "acp", "id": job.ID,
				"event": map[string]any{
					"kind": "steer-consumed", "clientUserMessageId": clientUserMessageID,
				},
			})
		}
	case "_workass_input_consumed":
		clientUserMessageID := strings.TrimSpace(asString(update["clientUserMessageId"]))
		if job != nil && clientUserMessageID != "" && !job.internal {
			if b.manager.nativeSessions != nil {
				b.manager.nativeSessions.markOperationConsumed(
					job.TabID, job.ChatID, job.ProviderID, sessionID,
					clientUserMessageID,
					firstNonEmpty(asString(update["nativeTurnId"]), asString(update["turnId"])),
				)
			}
			b.manager.emit("job:event", map[string]any{
				"type": "acp", "id": job.ID,
				"event": map[string]any{
					"kind": "input-consumed", "clientUserMessageId": clientUserMessageID,
				},
			})
		}
	case "_workass_compaction":
		event := map[string]any{
			"kind": "compaction", "phase": strings.TrimSpace(asString(update["phase"])),
			"checkpointId": strings.TrimSpace(asString(update["checkpointId"])),
			"digest":       strings.TrimSpace(asString(update["digest"])),
		}
		if coverage := numberOrZero(update["coverage"]); coverage > 0 {
			event["coverage"] = coverage
		}
		if job != nil && !job.internal {
			b.manager.emit("job:event", map[string]any{"type": "acp", "id": job.ID, "event": event})
		} else {
			b.manager.observeProviderLaneCompaction(sessionID, event)
		}
	default:
		if _, ok := update["configOptions"]; ok {
			b.applyConfigOptionsForSession(sessionID, update["configOptions"], true, false)
		}
	}
}

func numberOrZero(v any) int {
	switch x := v.(type) {
	case json.Number:
		n, _ := strconv.Atoi(x.String())
		return n
	case float64:
		return int(x)
	case int:
		return x
	default:
		return 0
	}
}

func textFromContent(content any) string {
	if items, ok := content.([]any); ok {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			if text := textFromContent(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	}
	m, ok := content.(map[string]any)
	if !ok {
		return ""
	}
	if asString(m["type"]) == "text" {
		return asString(m["text"])
	}
	if inner, ok := m["content"]; ok {
		return textFromContent(inner)
	}
	if asString(m["type"]) == "diff" {
		return "[diff] " + asString(m["path"]) + "\n"
	}
	if asString(m["type"]) == "terminal" {
		return "[terminal] " + asString(m["terminalId"]) + "\n"
	}
	return ""
}

const (
	maxToolResultImages     = 6
	maxToolResultImageBytes = 8 * 1024 * 1024
	maxToolResultTotalBytes = 16 * 1024 * 1024
)

// toolImagesFromContent preserves raster images returned by MCP tools instead
// of silently discarding them while extracting the text result. The payload is
// bounded before it reaches the wire/session mirror; remote URLs and SVG are
// intentionally excluded so rendering never performs a hidden fetch or embeds
// active document content.
func toolImagesFromContent(content any) []any {
	images := make([]any, 0, 2)
	total := 0
	var walk func(any)
	walk = func(value any) {
		if len(images) >= maxToolResultImages || total >= maxToolResultTotalBytes {
			return
		}
		switch item := value.(type) {
		case []any:
			for _, child := range item {
				walk(child)
				if len(images) >= maxToolResultImages || total >= maxToolResultTotalBytes {
					return
				}
			}
		case map[string]any:
			if strings.EqualFold(strings.TrimSpace(asString(item["type"])), "image") {
				mimeType := strings.ToLower(strings.TrimSpace(firstNonEmpty(asString(item["mimeType"]), asString(item["mime_type"]))))
				data := strings.TrimSpace(asString(item["data"]))
				if strings.HasPrefix(data, "data:") {
					if comma := strings.IndexByte(data, ','); comma > 5 && strings.Contains(strings.ToLower(data[:comma]), ";base64") {
						if mimeType == "" {
							mimeType = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(data[:comma], "data:"), ";base64")))
						}
						data = data[comma+1:]
					}
				}
				if !safeToolImageMIME(mimeType) || data == "" || len(data) > maxToolResultImageBytes || total+len(data) > maxToolResultTotalBytes {
					return
				}
				image := map[string]any{"mimeType": mimeType, "data": data}
				if name := compactText(firstNonEmpty(asString(item["name"]), asString(item["alt"]), asString(item["title"])), 160); name != "" {
					image["name"] = name
				}
				images = append(images, image)
				total += len(data)
				return
			}
			if nested, ok := item["content"]; ok {
				walk(nested)
			}
		}
	}
	walk(content)
	return images
}

func safeToolImageMIME(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func (b *Bridge) emitToolEvent(job *Job, acpKind string, update map[string]any, parentToolUseID string) {
	rawInput := update["rawInput"]
	outText := textFromContent(update["content"])
	images := toolImagesFromContent(update["content"])
	toolCall := mapFromAny(update["toolCall"])
	toolCallID := firstNonEmpty(asString(update["toolCallId"]), asString(toolCall["toolCallId"]))
	title := firstNonEmpty(asString(update["title"]), asString(toolCall["title"]), asString(update["kind"]), "tool")
	b.manager.observeSpawnToolEvent(spawnToolObservation{
		SessionID: job.SessionID, TabID: job.TabID, ChatID: job.ChatID,
		ProviderID: b.ProviderID(), ToolCallID: toolCallID, Title: title,
		Command: execCommandFrom(rawInput), RawInput: rawInput,
		Meta: mapFromAny(update["_meta"]), Output: outText,
		JobID: job.ID, OperationID: job.startOpts.OperationID,
	})
	b.manager.updateSubagentActivityForJob(job, "tool", title)
	// Remember this call's title so child subagent events (which carry
	// parentToolUseId == this toolCallId) can be labeled with it. Bounded reset
	// guards against unbounded growth over a long-lived engine.
	if toolCallID != "" && !strings.EqualFold(title, "tool") {
		b.mu.Lock()
		if b.subagentTitles == nil {
			b.subagentTitles = map[string]string{}
		}
		if len(b.subagentTitles) > 1024 {
			b.subagentTitles = map[string]string{}
		}
		b.subagentTitles[toolCallID] = title
		b.mu.Unlock()
	}
	visibleJobID := job.ID
	eventToolCallID := toolCallID
	terminalID := terminalIDFromContent(update["content"])
	if job.SubagentID != "" {
		if job.suppressVisible.Load() {
			return
		}
		visibleJobID = firstNonEmpty(job.VisibleJobID, job.ID)
		parentToolUseID = job.SubagentID
		if eventToolCallID != "" {
			eventToolCallID = job.SubagentID + ":" + eventToolCallID
		}
		if terminalID != "" {
			terminalID = job.SubagentID + ":" + terminalID
		}
	}
	event := map[string]any{
		"kind":       "tool",
		"toolKind":   firstNonEmpty(asString(update["kind"]), asString(toolCall["kind"])),
		"id":         nullableString(eventToolCallID),
		"title":      title,
		"status":     firstNonEmpty(asString(update["status"]), asString(toolCall["status"]), defaultToolStatus(acpKind)),
		"command":    nullableString(execCommandFrom(rawInput)),
		"terminalId": nullableString(terminalID),
		"input":      nullableString(compactJSON(rawInput, 400)),
		"output":     nullableString(compactText(outText, 600)),
		"location":   nullableString(firstLocation(update["locations"])),
	}
	if len(images) > 0 {
		event["images"] = images
	}
	// Subagent linkage (additive — the renderer nests tool calls by these fields
	// and ignores them when absent, so the frozen wire contract holds). The Claude
	// adapter stamps _meta.claudeCode.parentToolUseId on every tool event emitted
	// inside a Task subagent; it is empty on the main thread.
	if parentToolUseID != "" {
		event["subagentId"] = parentToolUseID
		label := ""
		if job.SubagentID != "" {
			label = job.SubagentLabel
		} else {
			b.mu.Lock()
			label = b.subagentTitles[parentToolUseID]
			b.mu.Unlock()
		}
		if label != "" {
			event["subagentLabel"] = label
		}
		brand := firstNonEmpty(job.SubagentProvider, brandForProvider(b.providerID))
		if brand != "" {
			event["subagentProvider"] = brand
		}
		if job.SubagentModel != "" {
			event["subagentModel"] = job.SubagentModel
		}
	}
	b.manager.emit("job:event", map[string]any{"type": "acp", "id": visibleJobID, "event": event})
}

func (b *Bridge) chatIdentity() (string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tabID, b.chatID
}

// metaParentToolUseID pulls the subagent→parent linkage the Claude adapter
// stamps on tool events. It lives at _meta.claudeCode.parentToolUseId, which may
// ride on the update object or on the notification params; check both.
func metaParentToolUseID(metas ...map[string]any) string {
	for _, meta := range metas {
		if id := asString(mapFromAny(meta["claudeCode"])["parentToolUseId"]); id != "" {
			return id
		}
	}
	return ""
}

// brandForProvider reads the renderer brand from the provider registration.
// Branding is presentation metadata, never a provider-family guess or a
// capability probe.
func brandForProvider(providerID string) string {
	return providerAdapterForID(providerID).model.AssistantBrand
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func defaultToolStatus(kind string) string {
	if kind == "tool_call" {
		return "pending"
	}
	return "update"
}

func compactJSON(v any, max int) string {
	if v == nil {
		return ""
	}
	var text string
	if s, ok := v.(string); ok {
		text = s
	} else {
		text = toJSON(v)
	}
	return compactText(text, max)
}

func compactText(text string, max int) string {
	text = redactSensitiveText(text)
	if text == "" {
		return ""
	}
	if len(text) <= max {
		return text
	}
	if max <= 1 {
		return text[:max]
	}
	return text[:max-1] + "…"
}

func execCommandFrom(raw any) string {
	m, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"command", "cmd", "commandLine", "script", "shell_command"} {
		if s := asString(m[key]); s != "" {
			return compactText(s, 400)
		}
	}
	if commands, ok := m["commands"].([]any); ok {
		parts := make([]string, 0, len(commands))
		for _, c := range commands {
			parts = append(parts, asString(c))
		}
		return compactText(strings.Join(parts, " && "), 400)
	}
	return ""
}

func terminalIDFromContent(content any) string {
	switch x := content.(type) {
	case map[string]any:
		if asString(x["type"]) == "terminal" && asString(x["terminalId"]) != "" {
			return asString(x["terminalId"])
		}
		if inner, ok := x["content"]; ok {
			return terminalIDFromContent(inner)
		}
	case []any:
		for _, item := range x {
			if id := terminalIDFromContent(item); id != "" {
				return id
			}
		}
	}
	return ""
}

func firstLocation(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	loc := mapFromAny(items[0])
	path := asString(loc["path"])
	if path == "" {
		return ""
	}
	if line := asString(loc["line"]); line != "" {
		return path + ":" + line
	}
	return path
}

func (b *Bridge) queueStdout(job *Job, text, phase string) {
	text = redactSensitiveText(text)
	// Never coalesce the last commentary bytes and first final-answer bytes into
	// one event: the renderer/session store need a stable ownership boundary.
	b.manager.jobMu.Lock()
	priorPhase := job.stdoutPhase
	hasPrior := job.stdoutBuf.Len() > 0
	b.manager.jobMu.Unlock()
	if hasPrior && priorPhase != phase {
		b.flushStdout(job)
	}
	b.recordStdoutChunk(len(text))
	b.manager.jobMu.Lock()
	priorEndedWithBang := strings.HasSuffix(job.output.String(), "!")
	job.output.WriteString(text)
	if strings.Contains(text, "![") || (priorEndedWithBang && strings.HasPrefix(text, "[")) {
		job.assistantMarkdownPending = true
	}
	scanMarkdown := job.assistantMarkdownPending
	markdown := ""
	if scanMarkdown {
		// This copies and rescans the whole answer so far, on every chunk, until
		// the image reference closes. Metrics exist so its real cost is visible.
		scanStartedAt := time.Now()
		markdown = job.output.String()
		job.assistantMarkdownPending = assistantMarkdownImagePending(markdown)
		b.manager.recordMarkdownScan(len(markdown), time.Since(scanStartedAt))
	}
	if job.internal {
		b.manager.jobMu.Unlock()
		return
	}
	if job.stdoutBuf.Len() == 0 {
		job.stdoutPhase = phase
		job.stdoutBufStartedAt = time.Now()
	}
	job.stdoutBuf.WriteString(text)
	if job.stdoutTimer == nil {
		job.stdoutTimer = time.AfterFunc(b.opts.StdoutFlushInterval, func() {
			b.flushStdout(job)
		})
	}
	b.manager.jobMu.Unlock()
	if scanMarkdown {
		// The filesystem half of the same per-chunk work.
		resolveStartedAt := time.Now()
		resolved := ResolveAssistantMarkdownImages(markdown, job.CWD)
		b.manager.recordImageResolve(time.Since(resolveStartedAt))
		b.publishAssistantImages(job, resolved)
	}
}

// publishAssistantImages makes assistant media live at the same shared ACP
// boundary as text. Only newly accepted bounded images are broadcast; Job.Public
// still carries the complete set at terminal seal for reconnect compatibility.
func (b *Bridge) publishAssistantImages(job *Job, images []any) {
	added := job.addAssistantImages(images)
	if len(added) == 0 || job == nil || job.internal {
		return
	}
	// Keep the authored Markdown/text ahead of the media that resolves it in
	// both the visible stream and crash journal. Structured image-only chunks
	// have no pending text and simply emit the media event.
	b.flushStdout(job)
	b.manager.emit("job:event", map[string]any{
		"type": "assistant-media", "id": job.ID, "images": added,
	})
}

func (b *Bridge) queueThinking(job *Job, text string) {
	text = redactSensitiveText(text)
	b.manager.jobMu.Lock()
	if job.thoughtBuf == "" {
		job.thoughtBuf = text
	} else {
		job.thoughtBuf += " " + text
	}
	if job.thinkTimer == nil {
		job.thinkTimer = time.AfterFunc(b.opts.ThoughtFlushInterval, func() {
			b.flushThinking(job)
		})
	}
	b.manager.jobMu.Unlock()
}

func (b *Bridge) flushStdout(job *Job) {
	b.manager.jobMu.Lock()
	timer := job.stdoutTimer
	chunk := job.stdoutBuf.String()
	phase := job.stdoutPhase
	bufferedSince := job.stdoutBufStartedAt
	job.stdoutBuf.Reset()
	job.stdoutPhase = ""
	job.stdoutTimer = nil
	job.stdoutBufStartedAt = time.Time{}
	b.manager.jobMu.Unlock()
	if chunk != "" {
		age := time.Duration(0)
		if !bufferedSince.IsZero() {
			age = time.Since(bufferedSince)
		}
		b.manager.recordStdoutFlush(len(chunk), age)
	}
	if timer != nil {
		timer.Stop()
	}
	if chunk != "" {
		payload := map[string]any{"type": "data", "id": job.ID, "stream": "stdout", "chunk": chunk}
		if phase != "" {
			payload["phase"] = phase
		}
		b.manager.emit("job:event", payload)
	}
}

func (b *Bridge) flushThinking(job *Job) {
	b.manager.jobMu.Lock()
	text := job.thoughtBuf
	job.thoughtBuf = ""
	job.thinkTimer = nil
	b.manager.jobMu.Unlock()
	if text != "" {
		b.manager.emit("job:event", map[string]any{"type": "acp", "id": job.ID, "event": map[string]any{"kind": "thinking", "text": text}})
	}
}

func (b *Bridge) flushJobBuffers(job *Job) {
	b.manager.jobMu.Lock()
	stdoutTimer := job.stdoutTimer
	thinkTimer := job.thinkTimer
	job.stdoutTimer = nil
	job.thinkTimer = nil
	stdout := job.stdoutBuf.String()
	stdoutPhase := job.stdoutPhase
	thinking := job.thoughtBuf
	job.stdoutBuf.Reset()
	job.stdoutPhase = ""
	job.thoughtBuf = ""
	b.manager.jobMu.Unlock()
	if stdoutTimer != nil {
		stdoutTimer.Stop()
	}
	if thinkTimer != nil {
		thinkTimer.Stop()
	}
	if stdout != "" {
		payload := map[string]any{"type": "data", "id": job.ID, "stream": "stdout", "chunk": stdout}
		if stdoutPhase != "" {
			payload["phase"] = stdoutPhase
		}
		b.manager.emit("job:event", payload)
	}
	if thinking != "" {
		b.manager.emit("job:event", map[string]any{"type": "acp", "id": job.ID, "event": map[string]any{"kind": "thinking", "text": thinking}})
	}
}

func (b *Bridge) jobForSession(sessionID string) *Job {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.jobsBySession[sessionID]
}

func (b *Bridge) setJobForSession(sessionID string, job *Job) {
	now := time.Now()
	b.mu.Lock()
	b.jobsBySession[sessionID] = job
	changed := false
	if job != nil {
		changed = b.state != StateActive || !b.pinned
		b.state = StateActive
		b.pinned = true
		b.lastActivity = now
	}
	b.mu.Unlock()
	if changed {
		b.manager.bridgeChanged(b, "active")
	}
}

func (b *Bridge) clearJobForSession(sessionID string, job *Job) {
	now := time.Now()
	b.mu.Lock()
	if job == nil || b.jobsBySession[sessionID] == job {
		b.jobsBySession[sessionID] = nil
	}
	changed := false
	shouldRecycle := false
	recycleReason := ""
	lastActivity := b.lastActivity
	if !b.hasRunningJobLocked() && b.state != StateHibernated && !b.closed {
		changed = b.state != StateIdle || b.pinned
		b.state = StateIdle
		b.pinned = false
		b.lastActivity = now
		lastActivity = now
		if b.recycleAtIdle {
			shouldRecycle = true
			recycleReason = firstNonEmpty(b.recycleReason, "recycle")
		}
	}
	b.mu.Unlock()
	if changed {
		b.manager.bridgeChanged(b, "idle")
	}
	if shouldRecycle {
		go b.manager.hibernateBridgeIfEligible(b, recycleReason, 0, lastActivity, now, nil)
	}
}

func (b *Bridge) Close(intentional bool, cause error) {
	safeCause := redactedError(cause)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	pending := b.pending
	b.pending = make(map[string]*pendingRequest)
	child := b.child
	childExited := b.childExited
	processTree := b.processTree
	stdin := b.stdin
	b.child = nil
	b.childExited = nil
	b.processTree = processTreeHandle{}
	b.stdin = nil
	b.finishedAt = time.Now()
	b.pinned = false
	tabID := b.tabID
	chatID := b.chatID
	sessions := make([]string, 0, len(b.sessions))
	lanes := make(map[*managerLane]struct{})
	for sessionID := range b.sessions {
		sessions = append(sessions, sessionID)
		if b.manager != nil {
			if lane := b.manager.providerLaneForSessionID(sessionID); lane != nil {
				lanes[lane] = struct{}{}
			}
		}
	}
	b.mu.Unlock()

	for _, p := range pending {
		if p.timer != nil {
			p.timer.Stop()
		}
		p.resolve <- rpcResult{err: firstErr(safeCause, errors.New("ACP server closed"))}
	}
	for _, sessionID := range sessions {
		b.manager.cancelPermissionsForSession(sessionID)
		b.manager.forgetSession(sessionID, b)
	}
	for lane := range lanes {
		lane.attachmentClosed()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if child != nil && child.Process != nil {
		_ = stopProcessTree(child.Process, processTree)
	}
	b.waitForChildExit(childExited, "close")
	if b.manager != nil {
		b.manager.orphanInProcessSpawnedWorkForChat(tabID, chatID, firstNonEmpty(errString(safeCause), "bridge-close"))
	}
	b.opts.Logf("acp bridge closed", map[string]any{"key": b.key, "intentional": intentional, "error": errString(safeCause)})
	if b.manager != nil {
		b.manager.bridgeChanged(b, "closed")
	}
}

const bridgeChildExitWait = 5 * time.Second

func (b *Bridge) waitForChildExit(childExited <-chan struct{}, operation string) {
	if childExited == nil {
		return
	}
	timer := time.NewTimer(bridgeChildExitWait)
	defer timer.Stop()
	select {
	case <-childExited:
		return
	case <-timer.C:
		b.opts.Logf("ACP child exit receipt timed out", map[string]any{
			"key":       b.key,
			"operation": operation,
			"timeoutMs": bridgeChildExitWait.Milliseconds(),
		})
	}
}

func firstErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func redactedError(err error) error {
	if err == nil {
		return nil
	}
	message := redactSensitiveText(err.Error())
	if message == err.Error() {
		return err
	}
	return errors.New(message)
}

func (b *Bridge) withStderrTail(err error) error {
	if err == nil {
		return nil
	}
	err = redactedError(err)
	b.mu.Lock()
	tail := redactSensitiveText(string(b.stderrTail))
	b.mu.Unlock()
	if strings.TrimSpace(tail) == "" {
		return err
	}
	return fmt.Errorf("%w; stderr: %s", err, tail)
}
