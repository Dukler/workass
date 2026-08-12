package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWorkassAgentHelper(t *testing.T) {
	if os.Getenv("WORKASS_AGENT_TEST_HELPER") != "1" {
		return
	}
	args := []string{}
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if err := run(args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "helper: %s\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestAgentHandshakePromptStreamsModelListAndStdoutPurity(t *testing.T) {
	fake := newFakeOpenAI(t)
	defer fake.Close()
	proc := startAgent(t, fake.URL()+"/v1", "fake-model")
	defer proc.Close(t)

	proc.Send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1}})
	init := proc.WaitFor(t, responseID("1"), 2*time.Second)
	initResult := asMap(init["result"])
	if intField(initResult["protocolVersion"]) != 1 {
		t.Fatalf("protocolVersion = %#v, want 1", initResult["protocolVersion"])
	}
	if asMap(initResult["agentInfo"])["name"] != "workass-agent" {
		t.Fatalf("agentInfo = %#v", initResult["agentInfo"])
	}

	proc.Send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": t.TempDir()}})
	newSession := proc.WaitFor(t, responseID("2"), 2*time.Second)
	sessionResult := asMap(newSession["result"])
	sessionID := stringField(sessionResult["sessionId"])
	if sessionID == "" {
		t.Fatalf("session/new result missing sessionId: %#v", sessionResult)
	}
	if !configOptionsContain(sessionResult["configOptions"], "fake-model") || !configOptionsContain(sessionResult["configOptions"], "other-model") {
		t.Fatalf("configOptions missing model catalog: %#v", sessionResult["configOptions"])
	}

	proc.Send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "session/set_config_option",
		"params":  map[string]any{"sessionId": sessionID, "configId": "model", "value": "other-model"},
	})
	setModel := proc.WaitFor(t, responseID("3"), 2*time.Second)
	if !configCurrentValue(asMap(setModel["result"])["configOptions"], "other-model") {
		t.Fatalf("set_config_option did not switch model: %#v", setModel["result"])
	}

	proc.Send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []any{map[string]any{"type": "text", "text": "hello agent"}},
		},
	})

	var chunks []string
	sawUsage := false
	var promptResult map[string]any
	for {
		msg := proc.WaitFor(t, func(map[string]any) bool { return true }, 2*time.Second)
		if responseID("4")(msg) {
			promptResult = asMap(msg["result"])
			break
		}
		if update, ok := sessionUpdate(msg, sessionID); ok {
			switch stringField(update["sessionUpdate"]) {
			case "agent_message_chunk":
				chunks = append(chunks, stringField(asMap(update["content"])["text"]))
			case "usage_update":
				sawUsage = true
				if intField(update["used"]) != 5 || intField(update["inputTokens"]) != 3 || intField(update["outputTokens"]) != 2 {
					t.Fatalf("usage_update = %#v", update)
				}
			}
		}
	}
	if promptResult["stopReason"] != "end_turn" {
		t.Fatalf("prompt result = %#v, want end_turn", promptResult)
	}
	if got := strings.Join(chunks, ""); got != "hello world" {
		t.Fatalf("streamed chunks = %q, want hello world", got)
	}
	if !sawUsage {
		t.Fatal("missing usage_update")
	}
	req := fake.LastChatRequest()
	if req.Model != "other-model" {
		t.Fatalf("chat model = %q, want other-model", req.Model)
	}
	if !req.Stream {
		t.Fatal("chat request did not set stream:true")
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != "hello agent" {
		t.Fatalf("chat messages = %#v", req.Messages)
	}
	proc.AssertStdoutPurity(t)
}

func TestAgentDurableSessionResumesExactThreadAfterHostRestart(t *testing.T) {
	fake := newFakeOpenAI(t)
	defer fake.Close()
	storePath := filepath.Join(t.TempDir(), "provider-native", "lmstudio-sessions.json")
	cwd := t.TempDir()

	first := startAgentWithStore(t, fake.URL()+"/v1", "fake-model", storePath)
	first.Send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	initialized := asMap(first.WaitFor(t, responseID("1"), 2*time.Second)["result"])
	capabilities := asMap(asMap(initialized["agentCapabilities"])["sessionCapabilities"])
	if _, ok := capabilities["resume"]; !ok {
		t.Fatalf("durable workass-agent did not advertise exact resume: %#v", initialized)
	}
	first.Send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": cwd}})
	sessionID := stringField(asMap(first.WaitFor(t, responseID("2"), 2*time.Second)["result"])["sessionId"])
	if sessionID == "" {
		t.Fatal("durable session/new returned no native thread identity")
	}
	first.Send(t, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": "first turn"}}},
	})
	_ = first.WaitFor(t, responseID("3"), 2*time.Second)
	first.Send(t, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "session/close", "params": map[string]any{"sessionId": sessionID}})
	_ = first.WaitFor(t, responseID("4"), 2*time.Second)
	first.Close(t)

	second := startAgentWithStore(t, fake.URL()+"/v1", "fake-model", storePath)
	defer second.Close(t)
	second.Send(t, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "initialize", "params": map[string]any{}})
	_ = second.WaitFor(t, responseID("5"), 2*time.Second)
	second.Send(t, map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "session/resume",
		"params": map[string]any{"sessionId": sessionID, "cwd": cwd},
	})
	resumed := asMap(second.WaitFor(t, responseID("6"), 2*time.Second)["result"])
	if got := stringField(resumed["sessionId"]); got != sessionID {
		t.Fatalf("session/resume changed native identity: got %q want %q", got, sessionID)
	}
	second.Send(t, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "session/prompt",
		"params": map[string]any{"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": "second turn"}}},
	})
	_ = second.WaitFor(t, responseID("7"), 2*time.Second)

	request := fake.LastChatRequest()
	if len(request.Messages) != 3 ||
		request.Messages[0].Role != "user" || request.Messages[0].Content != "first turn" ||
		request.Messages[1].Role != "assistant" || request.Messages[1].Content != "hello world" ||
		request.Messages[2].Role != "user" || request.Messages[2].Content != "second turn" {
		t.Fatalf("exact resume did not preserve native model context: %#v", request.Messages)
	}
	second.AssertStdoutPurity(t)
}

func TestAgentCancelAbortsInFlightHTTP(t *testing.T) {
	fake := newFakeOpenAI(t)
	fake.cancelMode = true
	defer fake.Close()
	proc := startAgent(t, fake.URL()+"/v1", "fake-model")
	defer proc.Close(t)

	proc.Send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	_ = proc.WaitFor(t, responseID("1"), 2*time.Second)
	proc.Send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{}})
	sessionID := stringField(asMap(proc.WaitFor(t, responseID("2"), 2*time.Second)["result"])["sessionId"])
	if sessionID == "" {
		t.Fatal("missing sessionId")
	}

	proc.Send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "session/prompt",
		"params":  map[string]any{"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": "please cancel"}}},
	})
	_ = proc.WaitFor(t, func(msg map[string]any) bool {
		update, ok := sessionUpdate(msg, sessionID)
		return ok && stringField(update["sessionUpdate"]) == "agent_message_chunk"
	}, 2*time.Second)
	proc.Send(t, map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessionID}})
	cancelResult := asMap(proc.WaitFor(t, responseID("3"), 2*time.Second)["result"])
	if cancelResult["stopReason"] != "cancelled" {
		t.Fatalf("cancelled prompt result = %#v", cancelResult)
	}
	select {
	case <-fake.cancelDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake OpenAI server did not observe request cancellation")
	}
	proc.AssertStdoutPurity(t)
}

func TestAgentMalformedRequestReturnsJSONRPCErrorAndStdoutPurity(t *testing.T) {
	fake := newFakeOpenAI(t)
	defer fake.Close()
	proc := startAgent(t, fake.URL()+"/v1", "fake-model")
	defer proc.Close(t)

	_, err := io.WriteString(proc.stdin, "{not-json}\n")
	if err != nil {
		t.Fatalf("write malformed request: %v", err)
	}
	msg := proc.WaitFor(t, func(msg map[string]any) bool {
		errObj := asMap(msg["error"])
		return intField(errObj["code"]) == -32700
	}, 2*time.Second)
	if _, ok := msg["id"]; !ok || msg["id"] != nil {
		t.Fatalf("parse error id = %#v, want null", msg["id"])
	}
	proc.AssertStdoutPurity(t)
}

func TestNodeProbeAgainstGoRun(t *testing.T) {
	fake := newFakeOpenAI(t)
	defer fake.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	// Keep compilation outside the Node probe's five-second initialize deadline.
	// The probe must start the real agent executable, rather than the in-process
	// test helper, so this remains an end-to-end ACP initialize oracle.
	agentPath := filepath.Join(t.TempDir(), "workass-agent")
	build := exec.Command("go", "build", "-o", agentPath, "./cmd/workass-agent")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workass-agent for ACP probe: %v\n%s", err, output)
	}
	cmd := exec.Command("node", "desktop/scripts/probe-acp.mjs", agentPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"OPENAI_BASE_URL="+fake.URL()+"/v1",
		"OPENAI_MODEL=fake-model",
		"OPENAI_API_KEY=test-secret",
	)
	out, err := cmd.CombinedOutput()
	t.Logf("node desktop/scripts/probe-acp.mjs %s\n%s", agentPath, out)
	if err != nil {
		t.Fatalf("node probe failed: %v", err)
	}
	var result map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&result); err != nil {
		t.Fatalf("decode probe output: %v", err)
	}
	if result["ok"] != true {
		t.Fatalf("probe result = %#v", result)
	}
	if intField(result["protocolVersion"]) != 1 {
		t.Fatalf("probe protocolVersion = %#v, want 1", result["protocolVersion"])
	}
	if stringField(asMap(result["agentInfo"])["name"]) != "workass-agent" {
		t.Fatalf("probe agentInfo = %#v", result["agentInfo"])
	}
}

func TestProbeWritesSelfTestToStderrOnly(t *testing.T) {
	fake := newFakeOpenAI(t)
	defer fake.Close()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run([]string{"--probe", "--openai-base-url", fake.URL() + "/v1", "--model", "fake-model", "--api-key", "test-secret"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("probe run: %v stderr=%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("probe wrote to stdout: %q", stdout.String())
	}
	var result map[string]any
	dec := json.NewDecoder(bytes.NewReader(stderr.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&result); err != nil {
		t.Fatalf("decode probe stderr: %v text=%q", err, stderr.String())
	}
	if result["ok"] != true || intField(result["modelCount"]) != 2 || result["apiKeyConfigured"] != true {
		t.Fatalf("probe result = %#v", result)
	}
}

func TestProbeFailureWritesSingleJSONToStderrOnly(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run([]string{"--probe", "--openai-base-url", "127.0.0.1:bad"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("probe failure should be represented in JSON, got err=%v stderr=%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("probe wrote to stdout: %q", stdout.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("probe stderr lines = %d, want 1: %q", len(lines), stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &result); err != nil {
		t.Fatalf("decode probe failure JSON: %v text=%q", err, stderr.String())
	}
	if result["ok"] != false || stringField(result["error"]) == "" {
		t.Fatalf("probe failure result = %#v", result)
	}
}

func TestRealLMStudioStreamTransport(t *testing.T) {
	if os.Getenv("WORKASS_AGENT_REAL_LMSTUDIO") != "1" {
		t.Skip("set WORKASS_AGENT_REAL_LMSTUDIO=1 to run against a live local OpenAI-compatible server")
	}
	baseURL := getenvDefault("OPENAI_BASE_URL", "http://127.0.0.1:1234/v1")
	model := getenvDefault("OPENAI_MODEL", "workass-dev")
	apiKey := getenvDefault("OPENAI_API_KEY", "lm-studio")
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	proc := startGoRunAgent(t, root, baseURL, model, apiKey)
	defer proc.Close(t)
	proc.Send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1}})
	_ = proc.WaitFor(t, responseID("1"), 10*time.Second)
	proc.Send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{}})
	sessionID := stringField(asMap(proc.WaitFor(t, responseID("2"), 10*time.Second)["result"])["sessionId"])
	if sessionID == "" {
		t.Fatal("missing sessionId")
	}
	proc.Send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []any{map[string]any{"type": "text", "text": "Reply with one short sentence for a transport smoke test."}},
		},
	})
	var chunks []string
	var sawUsage bool
	var stopReason string
	for {
		msg := proc.WaitFor(t, func(map[string]any) bool { return true }, 60*time.Second)
		if responseID("3")(msg) {
			stopReason = stringField(asMap(msg["result"])["stopReason"])
			break
		}
		if update, ok := sessionUpdate(msg, sessionID); ok {
			switch stringField(update["sessionUpdate"]) {
			case "agent_message_chunk":
				chunk := stringField(asMap(update["content"])["text"])
				chunks = append(chunks, chunk)
				t.Logf("trace chunk %d bytes=%d text=%q", len(chunks), len(chunk), chunk)
			case "usage_update":
				sawUsage = true
				t.Logf("trace usage used=%d size=%d input=%d output=%d", intField(update["used"]), intField(update["size"]), intField(update["inputTokens"]), intField(update["outputTokens"]))
			}
		}
	}
	output := strings.Join(chunks, "")
	t.Logf("trace stopReason=%s chunks=%d bytes=%d usage=%v", stopReason, len(chunks), len(output), sawUsage)
	if stopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", stopReason)
	}
	if len(output) == 0 {
		t.Fatal("expected at least one streamed assistant chunk")
	}
	proc.AssertStdoutPurity(t)
}

type agentProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr bytes.Buffer

	mu          sync.Mutex
	lines       []string
	parseErrs   []string
	pending     []map[string]any
	messageChan chan map[string]any
}

func startAgent(t *testing.T, baseURL, model string) *agentProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestWorkassAgentHelper", "--")
	cmd.Env = append(os.Environ(),
		"WORKASS_AGENT_TEST_HELPER=1",
		"OPENAI_BASE_URL="+baseURL,
		"OPENAI_MODEL="+model,
		"OPENAI_API_KEY=test-secret",
	)
	return startAgentProcess(t, cmd)
}

func startAgentWithStore(t *testing.T, baseURL, model, storePath string) *agentProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestWorkassAgentHelper", "--")
	cmd.Env = append(os.Environ(),
		"WORKASS_AGENT_TEST_HELPER=1",
		"WORKASS_AGENT_SESSION_STORE="+storePath,
		"OPENAI_BASE_URL="+baseURL,
		"OPENAI_MODEL="+model,
		"OPENAI_API_KEY=test-secret",
	)
	return startAgentProcess(t, cmd)
}

func startGoRunAgent(t *testing.T, root, baseURL, model, apiKey string) *agentProcess {
	t.Helper()
	cmd := exec.Command("go", "run", "./cmd/workass-agent")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"OPENAI_BASE_URL="+baseURL,
		"OPENAI_MODEL="+model,
		"OPENAI_API_KEY="+apiKey,
	)
	return startAgentProcess(t, cmd)
}

func startAgentProcess(t *testing.T, cmd *exec.Cmd) *agentProcess {
	t.Helper()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	proc := &agentProcess{cmd: cmd, stdin: stdin, messageChan: make(chan map[string]any, 128)}
	cmd.Stderr = &proc.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	go proc.readStdout(stdout)
	return proc
}

func getenvDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func (p *agentProcess) readStdout(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		p.mu.Lock()
		p.lines = append(p.lines, line)
		p.mu.Unlock()
		if line == "" {
			continue
		}
		var msg map[string]any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&msg); err != nil {
			p.mu.Lock()
			p.parseErrs = append(p.parseErrs, fmt.Sprintf("%q: %v", line, err))
			p.mu.Unlock()
			continue
		}
		p.messageChan <- msg
	}
}

func (p *agentProcess) Send(t *testing.T, msg any) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func (p *agentProcess) WaitFor(t *testing.T, pred func(map[string]any) bool, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		p.mu.Lock()
		for i, msg := range p.pending {
			if pred(msg) {
				p.pending = append(p.pending[:i], p.pending[i+1:]...)
				p.mu.Unlock()
				return msg
			}
		}
		p.mu.Unlock()
		select {
		case msg := <-p.messageChan:
			if pred(msg) {
				return msg
			}
			p.mu.Lock()
			p.pending = append(p.pending, msg)
			p.mu.Unlock()
		case <-deadline:
			p.mu.Lock()
			lines := append([]string(nil), p.lines...)
			pending := append([]map[string]any(nil), p.pending...)
			p.mu.Unlock()
			t.Fatalf("timeout waiting for agent message; stdout=%v pending=%#v stderr=%s", lines, pending, p.stderr.String())
		}
	}
}

func (p *agentProcess) AssertStdoutPurity(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.parseErrs) > 0 {
		t.Fatalf("stdout contained non-JSON lines: %v", p.parseErrs)
	}
	for _, line := range p.lines {
		var msg map[string]any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&msg); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", line, err)
		}
		if msg["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line is not JSON-RPC 2.0: %q", line)
		}
		if _, hasMethod := msg["method"]; !hasMethod {
			if _, hasResult := msg["result"]; !hasResult {
				if _, hasError := msg["error"]; !hasError {
					t.Fatalf("stdout JSON-RPC line lacks method/result/error: %q", line)
				}
			}
		}
	}
}

func (p *agentProcess) Close(t *testing.T) {
	t.Helper()
	_ = p.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent exited with error: %v stderr=%s", err, p.stderr.String())
		}
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatalf("agent did not exit after stdin close; stderr=%s", p.stderr.String())
	}
}

func responseID(id string) func(map[string]any) bool {
	return func(msg map[string]any) bool {
		return rpcID(msg["id"]) == id
	}
}

func rpcID(v any) string {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	case string:
		return x
	default:
		return ""
	}
}

func sessionUpdate(msg map[string]any, sessionID string) (map[string]any, bool) {
	if msg["method"] != "session/update" {
		return nil, false
	}
	params := asMap(msg["params"])
	if stringField(params["sessionId"]) != sessionID {
		return nil, false
	}
	return asMap(params["update"]), true
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func stringField(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func intField(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	default:
		return 0
	}
}

func configOptionsContain(raw any, modelID string) bool {
	for _, option := range asSlice(raw) {
		cfg := asMap(option)
		if cfg["id"] != "model" {
			continue
		}
		for _, item := range asSlice(cfg["options"]) {
			if asMap(item)["value"] == modelID {
				return true
			}
		}
	}
	return false
}

func configCurrentValue(raw any, modelID string) bool {
	for _, option := range asSlice(raw) {
		cfg := asMap(option)
		if cfg["id"] == "model" {
			return cfg["currentValue"] == modelID
		}
	}
	return false
}

func asSlice(v any) []any {
	items, _ := v.([]any)
	return items
}

type fakeOpenAI struct {
	server     *httptest.Server
	cancelMode bool
	cancelDone chan struct{}
	cancelOnce sync.Once

	mu       sync.Mutex
	requests []chatRequest
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

func newFakeOpenAI(t *testing.T) *fakeOpenAI {
	t.Helper()
	fake := &fakeOpenAI{cancelDone: make(chan struct{})}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	return fake
}

func (f *fakeOpenAI) URL() string {
	return f.server.URL
}

func (f *fakeOpenAI) Close() {
	f.server.Close()
}

func (f *fakeOpenAI) LastChatRequest() chatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return chatRequest{}
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeOpenAI) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"fake-model"},{"id":"other-model"}]}`)
	case "/v1/chat/completions":
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		cancelMode := f.cancelMode
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		if cancelMode {
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			<-r.Context().Done()
			f.cancelOnce.Do(func() { close(f.cancelDone) })
			return
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hello "}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(5 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"world"}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(5 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	default:
		http.NotFound(w, r)
	}
}
