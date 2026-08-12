package agentserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workass/internal/lmstudio"
)

const (
	ProtocolVersion = 1
	defaultFlush    = 16 * time.Millisecond
)

type Options struct {
	Client           *lmstudio.Client
	ClientConfig     lmstudio.Config
	Stdout           io.Writer
	Stderr           io.Writer
	FlushInterval    time.Duration
	Version          string
	SessionStorePath string
}

type Server struct {
	client           *lmstudio.Client
	stdout           io.Writer
	logger           *log.Logger
	flushInterval    time.Duration
	version          string
	sessionStorePath string

	mu         sync.Mutex
	sessions   map[string]*session
	sessionSeq int64
	writeMu    sync.Mutex
	persistMu  sync.Mutex
}

type session struct {
	id       string
	cwd      string
	model    string
	messages []lmstudio.Message

	mu       sync.Mutex
	promptMu sync.Mutex
	cancel   context.CancelFunc
	running  bool
}

func New(opts Options) (*Server, error) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	client := opts.Client
	if client == nil {
		var err error
		client, err = lmstudio.New(opts.ClientConfig)
		if err != nil {
			return nil, err
		}
	}
	flush := opts.FlushInterval
	if flush <= 0 {
		flush = defaultFlush
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = "0.1.0"
	}
	server := &Server{
		client:           client,
		stdout:           stdout,
		logger:           log.New(stderr, "workass-agent: ", log.LstdFlags),
		flushInterval:    flush,
		version:          version,
		sessionStorePath: strings.TrimSpace(opts.SessionStorePath),
		sessions:         make(map[string]*session),
	}
	if err := server.loadSessions(); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *Server) Serve(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		copied := append([]byte(nil), line...)
		s.acceptLine(ctx, copied)
	}
	if err := scanner.Err(); err != nil {
		s.logger.Printf("stdin read failed: %s", RedactSensitiveText(err.Error()))
		return err
	}
	return nil
}

func (s *Server) acceptLine(ctx context.Context, line []byte) {
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		_ = s.writeError(json.RawMessage("null"), -32700, "Parse error", nil)
		return
	}
	var jsonrpc string
	if raw := fields["jsonrpc"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &jsonrpc)
	}
	if jsonrpc != "2.0" {
		_ = s.writeError(idOrNull(fields["id"]), -32600, "Invalid Request", nil)
		return
	}
	var method string
	if raw := fields["method"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &method)
	}
	if method == "" {
		return
	}
	rawID, hasID := fields["id"]
	params := append(json.RawMessage(nil), fields["params"]...)
	if hasID {
		id := append(json.RawMessage(nil), rawID...)
		go s.handleRequest(ctx, id, method, params)
		return
	}
	s.handleNotification(method, params)
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(id)) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func (s *Server) handleRequest(ctx context.Context, id json.RawMessage, method string, rawParams json.RawMessage) {
	params, err := decodeParams(rawParams)
	if err != nil {
		_ = s.writeError(id, -32602, "Invalid params", nil)
		return
	}
	var result any
	var rpcErr *rpcError
	switch method {
	case "initialize":
		result = s.initialize(params)
	case "session/new":
		result, rpcErr = s.newSession(ctx, params)
	case "session/resume":
		result, rpcErr = s.resumeSession(ctx, params)
	case "session/prompt":
		result, rpcErr = s.prompt(ctx, params)
	case "session/cancel":
		result, rpcErr = s.cancel(params, true)
	case "session/set_config_option":
		result, rpcErr = s.setConfigOption(ctx, params)
	case "session/close":
		result, rpcErr = s.closeSession(params)
	default:
		rpcErr = &rpcError{Code: -32601, Message: "Method not found: " + method}
	}
	if rpcErr != nil {
		_ = s.writeRPCError(id, rpcErr)
		return
	}
	_ = s.writeResult(id, result)
}

func (s *Server) handleNotification(method string, rawParams json.RawMessage) {
	params, err := decodeParams(rawParams)
	if err != nil {
		s.logger.Printf("notification %s has invalid params", method)
		return
	}
	switch method {
	case "session/cancel":
		_, _ = s.cancel(params, false)
	default:
	}
}

func (s *Server) initialize(_ map[string]any) map[string]any {
	sessionCapabilities := map[string]any{"close": map[string]any{}}
	if s.sessionStorePath != "" {
		sessionCapabilities["resume"] = map[string]any{}
	}
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"agentInfo": map[string]any{
			"name":    "workass-agent",
			"version": s.version,
		},
		"agentCapabilities": map[string]any{
			"loadSession":         false,
			"sessionCapabilities": sessionCapabilities,
			"promptCapabilities": map[string]any{
				"image":           false,
				"audio":           false,
				"embeddedContext": false,
			},
			"mcpCapabilities": map[string]any{
				"http": false,
				"sse":  false,
			},
		},
		"authMethods": []any{},
	}
}

func (s *Server) newSession(ctx context.Context, params map[string]any) (map[string]any, *rpcError) {
	cwd := strings.TrimSpace(asString(params["cwd"]))
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	models, _ := s.listModels(ctx)
	model := s.client.DefaultModel()
	if model == "" && len(models) > 0 {
		model = models[0].ID
	}
	if len(models) > 0 && model != "" && !modelInCatalog(models, model) {
		model = models[0].ID
	}
	seq := atomic.AddInt64(&s.sessionSeq, 1)
	sess := &session{
		id:    fmt.Sprintf("workass-agent-%d-%d", os.Getpid(), seq),
		cwd:   cwd,
		model: model,
	}
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	if err := s.persistSessions(); err != nil {
		s.mu.Lock()
		delete(s.sessions, sess.id)
		s.mu.Unlock()
		return nil, &rpcError{Code: -32001, Message: RedactSensitiveText(err.Error())}
	}
	return map[string]any{
		"sessionId":     sess.id,
		"configOptions": s.configOptions(sess, models),
	}, nil
}

func (s *Server) resumeSession(ctx context.Context, params map[string]any) (map[string]any, *rpcError) {
	if s.sessionStorePath == "" {
		return nil, &rpcError{Code: -32601, Message: "session/resume is unavailable without durable session storage"}
	}
	sessionID := strings.TrimSpace(asString(params["sessionId"]))
	if sessionID == "" {
		return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
	}
	sess := s.getSession(sessionID)
	if sess == nil {
		return nil, &rpcError{Code: -32000, Message: "Unknown durable ACP session."}
	}
	if cwd := strings.TrimSpace(asString(params["cwd"])); cwd != "" {
		sess.mu.Lock()
		sess.cwd = cwd
		sess.mu.Unlock()
		if err := s.persistSessions(); err != nil {
			return nil, &rpcError{Code: -32001, Message: RedactSensitiveText(err.Error())}
		}
	}
	models, _ := s.listModels(ctx)
	return map[string]any{"sessionId": sessionID, "configOptions": s.configOptions(sess, models)}, nil
}

func (s *Server) setConfigOption(ctx context.Context, params map[string]any) (map[string]any, *rpcError) {
	sessionID := asString(params["sessionId"])
	configID := asString(params["configId"])
	value := strings.TrimSpace(asString(params["value"]))
	if sessionID == "" || configID == "" {
		return nil, &rpcError{Code: -32602, Message: "sessionId and configId are required"}
	}
	if configID != "model" {
		return nil, &rpcError{Code: -32602, Message: "unsupported config option: " + configID}
	}
	if value == "" {
		return nil, &rpcError{Code: -32602, Message: "model value is required"}
	}
	sess := s.getSession(sessionID)
	if sess == nil {
		return nil, &rpcError{Code: -32000, Message: "Unknown ACP session."}
	}
	models, _ := s.listModels(ctx)
	if len(models) > 0 && !modelInCatalog(models, value) {
		return nil, &rpcError{Code: -32602, Message: "unknown model: " + value}
	}
	sess.mu.Lock()
	sess.model = value
	sess.mu.Unlock()
	if err := s.persistSessions(); err != nil {
		return nil, &rpcError{Code: -32001, Message: RedactSensitiveText(err.Error())}
	}
	return map[string]any{"configOptions": s.configOptions(sess, models)}, nil
}

func (s *Server) closeSession(params map[string]any) (map[string]any, *rpcError) {
	sessionID := asString(params["sessionId"])
	if sessionID == "" {
		return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
	}
	s.mu.Lock()
	sess := s.sessions[sessionID]
	if s.sessionStorePath == "" {
		delete(s.sessions, sessionID)
	}
	s.mu.Unlock()
	if sess != nil {
		sess.mu.Lock()
		if sess.cancel != nil {
			sess.cancel()
		}
		sess.mu.Unlock()
	}
	return map[string]any{}, nil
}

func (s *Server) cancel(params map[string]any, strict bool) (map[string]any, *rpcError) {
	sessionID := asString(params["sessionId"])
	if sessionID == "" {
		if strict {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		return map[string]any{}, nil
	}
	sess := s.getSession(sessionID)
	if sess == nil {
		if strict {
			return nil, &rpcError{Code: -32000, Message: "Unknown ACP session."}
		}
		return map[string]any{}, nil
	}
	sess.mu.Lock()
	cancel := sess.cancel
	sess.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return map[string]any{}, nil
}

func (s *Server) prompt(ctx context.Context, params map[string]any) (map[string]any, *rpcError) {
	sessionID := asString(params["sessionId"])
	if sessionID == "" {
		return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
	}
	sess := s.getSession(sessionID)
	if sess == nil {
		return nil, &rpcError{Code: -32000, Message: "Unknown ACP session."}
	}
	text := promptText(params["prompt"])
	sess.promptMu.Lock()
	defer sess.promptMu.Unlock()

	sess.mu.Lock()
	model := sess.model
	history := append([]lmstudio.Message(nil), sess.messages...)
	sess.mu.Unlock()
	if strings.TrimSpace(model) == "" {
		return nil, &rpcError{Code: -32602, Message: "No model configured. Set OPENAI_MODEL or select a model from /v1/models."}
	}
	userMsg := lmstudio.Message{Role: "user", Content: text}
	messages := append(history, userMsg)
	promptCtx, cancel := context.WithCancel(ctx)
	sess.mu.Lock()
	sess.cancel = cancel
	sess.running = true
	sess.mu.Unlock()
	defer func() {
		cancel()
		sess.mu.Lock()
		sess.cancel = nil
		sess.running = false
		sess.mu.Unlock()
	}()

	coalescer := newChunkCoalescer(s.flushInterval, func(chunk string) {
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": chunk},
		})
	})
	var assistant strings.Builder
	err := s.client.StreamChat(promptCtx, lmstudio.ChatRequest{Model: model, Messages: messages}, func(event lmstudio.StreamEvent) error {
		if event.Content != "" {
			assistant.WriteString(event.Content)
			coalescer.Add(event.Content)
		}
		if event.Usage != nil {
			coalescer.Flush()
			s.notifyUsage(sessionID, event.Usage)
		}
		return nil
	})
	coalescer.Close()
	if err != nil {
		if errors.Is(promptCtx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return map[string]any{"stopReason": "cancelled"}, nil
		}
		return nil, &rpcError{Code: -32001, Message: RedactSensitiveText(err.Error())}
	}
	sess.mu.Lock()
	sess.messages = append(sess.messages, userMsg, lmstudio.Message{Role: "assistant", Content: assistant.String()})
	sess.mu.Unlock()
	if err := s.persistSessions(); err != nil {
		return nil, &rpcError{Code: -32001, Message: RedactSensitiveText(err.Error())}
	}
	return map[string]any{"stopReason": "end_turn"}, nil
}

type persistedSession struct {
	ID       string             `json:"id"`
	CWD      string             `json:"cwd"`
	Model    string             `json:"model"`
	Messages []lmstudio.Message `json:"messages"`
}

type persistedSessionFile struct {
	Version  int                `json:"v"`
	Sessions []persistedSession `json:"sessions"`
}

func (s *Server) loadSessions() error {
	if s == nil || s.sessionStorePath == "" {
		return nil
	}
	raw, err := os.ReadFile(s.sessionStorePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read durable ACP sessions: %w", err)
	}
	var file persistedSessionFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return fmt.Errorf("decode durable ACP sessions: %w", err)
	}
	if file.Version != 1 {
		return fmt.Errorf("unsupported durable ACP session schema %d", file.Version)
	}
	for _, record := range file.Sessions {
		id := strings.TrimSpace(record.ID)
		if id == "" {
			return errors.New("durable ACP session is missing its identity")
		}
		if _, exists := s.sessions[id]; exists {
			return fmt.Errorf("duplicate durable ACP session %q", id)
		}
		s.sessions[id] = &session{
			id: id, cwd: strings.TrimSpace(record.CWD), model: strings.TrimSpace(record.Model),
			messages: append([]lmstudio.Message(nil), record.Messages...),
		}
	}
	return nil
}

func (s *Server) persistSessions() error {
	if s == nil || s.sessionStorePath == "" {
		return nil
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	items := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		items = append(items, sess)
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })
	file := persistedSessionFile{Version: 1, Sessions: make([]persistedSession, 0, len(items))}
	for _, sess := range items {
		sess.mu.Lock()
		record := persistedSession{
			ID: sess.id, CWD: sess.cwd, Model: sess.model,
			Messages: append([]lmstudio.Message(nil), sess.messages...),
		}
		sess.mu.Unlock()
		file.Sessions = append(file.Sessions, record)
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("encode durable ACP sessions: %w", err)
	}
	dir := filepath.Dir(s.sessionStorePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create durable ACP session directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".workass-agent-sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("create durable ACP session file: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect durable ACP sessions: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		return fmt.Errorf("write durable ACP sessions: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close durable ACP sessions: %w", err)
	}
	if err := os.Rename(temporaryPath, s.sessionStorePath); err != nil {
		return fmt.Errorf("commit durable ACP sessions: %w", err)
	}
	committed = true
	return nil
}

func (s *Server) notifyUsage(sessionID string, usage *lmstudio.Usage) {
	if usage == nil {
		return
	}
	used := usage.TotalTokens
	if used <= 0 {
		used = usage.PromptTokens + usage.CompletionTokens
	}
	s.notify(sessionID, map[string]any{
		"sessionUpdate": "usage_update",
		"used":          used,
		"size":          0,
		"inputTokens":   usage.PromptTokens,
		"outputTokens":  usage.CompletionTokens,
		"_meta": map[string]any{
			"workass.agent/inputTokens":  usage.PromptTokens,
			"workass.agent/outputTokens": usage.CompletionTokens,
			"workass.agent/totalTokens":  used,
		},
	})
}

func (s *Server) listModels(ctx context.Context) ([]lmstudio.Model, error) {
	catalogCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	models, err := s.client.ListModels(catalogCtx)
	if err != nil {
		s.logger.Printf("model catalog unavailable: %s", RedactSensitiveText(err.Error()))
		return nil, err
	}
	return models, nil
}

func (s *Server) configOptions(sess *session, models []lmstudio.Model) []any {
	current := ""
	if sess != nil {
		sess.mu.Lock()
		current = sess.model
		sess.mu.Unlock()
	}
	options := make([]any, 0, len(models)+1)
	seen := map[string]bool{}
	for _, model := range models {
		if model.ID == "" || seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		options = append(options, map[string]any{"value": model.ID, "name": model.ID})
	}
	if current != "" && !seen[current] && len(options) == 0 {
		options = append(options, map[string]any{"value": current, "name": current})
	}
	return []any{
		map[string]any{
			"id":           "model",
			"category":     "model",
			"name":         "Model",
			"type":         "select",
			"currentValue": current,
			"options":      options,
		},
	}
}

func (s *Server) getSession(sessionID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[sessionID]
}

func modelInCatalog(models []lmstudio.Model, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}

type chunkCoalescer struct {
	interval time.Duration
	flush    func(string)

	mu     sync.Mutex
	buffer strings.Builder
	timer  *time.Timer
	closed bool
}

func newChunkCoalescer(interval time.Duration, flush func(string)) *chunkCoalescer {
	if interval <= 0 {
		interval = defaultFlush
	}
	return &chunkCoalescer{interval: interval, flush: flush}
}

func (c *chunkCoalescer) Add(text string) {
	if text == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.buffer.WriteString(text)
	if c.timer == nil {
		c.timer = time.AfterFunc(c.interval, c.flushFromTimer)
	}
}

func (c *chunkCoalescer) Flush() {
	chunk := c.take()
	if chunk != "" {
		c.flush(chunk)
	}
}

func (c *chunkCoalescer) Close() {
	c.mu.Lock()
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	chunk := c.buffer.String()
	c.buffer.Reset()
	c.mu.Unlock()
	if chunk != "" {
		c.flush(chunk)
	}
}

func (c *chunkCoalescer) flushFromTimer() {
	chunk := c.take()
	if chunk != "" {
		c.flush(chunk)
	}
}

func (c *chunkCoalescer) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	chunk := c.buffer.String()
	c.buffer.Reset()
	return chunk
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcOutbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) notify(sessionID string, update any) {
	_ = s.write(rpcOutbound{
		JSONRPC: "2.0",
		Method:  "session/update",
		Params:  map[string]any{"sessionId": sessionID, "update": update},
	})
}

func (s *Server) writeResult(id json.RawMessage, result any) error {
	return s.write(rpcOutbound{JSONRPC: "2.0", ID: idOrNull(id), Result: result})
}

func (s *Server) writeRPCError(id json.RawMessage, rpcErr *rpcError) error {
	if rpcErr == nil {
		return nil
	}
	rpcErr.Message = RedactSensitiveText(rpcErr.Message)
	return s.write(rpcOutbound{JSONRPC: "2.0", ID: idOrNull(id), Error: rpcErr})
}

func (s *Server) writeError(id json.RawMessage, code int, message string, data any) error {
	return s.writeRPCError(id, &rpcError{Code: code, Message: message, Data: data})
}

func (s *Server) write(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdout.Write(data)
	return err
}

func decodeParams(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, nil
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		return map[string]any{}, nil
	}
	return out, nil
}

func promptText(raw any) string {
	blocks, ok := raw.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		m, _ := block.(map[string]any)
		if asString(m["type"]) != "text" {
			continue
		}
		text := asString(m["text"])
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return strings.Trim(strings.TrimSpace(string(b)), `"`)
	}
}

var secretTextRE = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+|((?:api[_-]?key|token|secret|password|credential)\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}]+)`)

func RedactSensitiveText(text string) string {
	return secretTextRE.ReplaceAllStringFunc(text, func(match string) string {
		lower := strings.ToLower(match)
		if strings.HasPrefix(lower, "bearer ") {
			return match[:7] + "[redacted]"
		}
		if i := strings.IndexAny(match, ":="); i >= 0 {
			return match[:i+1] + "[redacted]"
		}
		return "[redacted]"
	})
}
