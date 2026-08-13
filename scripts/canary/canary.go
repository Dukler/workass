package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	lmStudioBaseURL       = "http://127.0.0.1:1234/v1"
	qwenModelID           = "workass-dev"
	qwenPrompt            = "Respondé con una sola palabra: hola"
	qwenCancelPrompt      = "Escribí los números del 1 al 5000 separados por coma. No resumas."
	mockConcurrentPrompt  = "mock concurrent isolation canary"
	hibernateTTL          = 1500 * time.Millisecond
	healthTimeout         = 20 * time.Second
	accessTimeout         = 5 * time.Second
	catalogTimeout        = 150 * time.Second
	invokeTimeout         = 30 * time.Second
	realTurnTimeout       = 4 * time.Minute
	cancelTurnTimeout     = 3 * time.Minute
	cancelWarmupMaxWait   = 12 * time.Second
	lmStudioStartTimeout  = 20 * time.Second
	lmStudioRequestTimout = 4 * time.Second
)

func main() {
	os.Exit(runMain())
}

func runMain() int {
	daemonFlag := flag.String("daemon", "", "daemon executable; defaults to dist-bin/workass-${GOOS}-${GOARCH}, then go run ./cmd/workass")
	goRunDaemon := flag.Bool("go-run-daemon", false, "run the daemon with go run ./cmd/workass instead of a dist binary")
	stateDirFlag := flag.String("state-dir", "", "scratch state dir; defaults to a new temp dir and must not live inside the repo")
	keepState := flag.Bool("keep-state", false, "keep the scratch state directory")
	skipPreflight := flag.Bool("skip-lmstudio-preflight", false, "skip LM Studio /v1/models preflight")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := newCanary(*daemonFlag, *goRunDaemon, *stateDirFlag, *keepState, !*skipPreflight)
	if err != nil {
		fmt.Fprintf(os.Stderr, "CANARY FAIL: %v\n", err)
		return 1
	}
	passed := false
	defer func() {
		if !passed {
			c.printReport(os.Stderr, false)
		}
		c.cleanup()
	}()
	if err := c.run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "CANARY FAIL: %v\n", err)
		return 1
	}
	passed = true
	c.printReport(os.Stdout, true)
	return 0
}

type canary struct {
	repoRoot     string
	tempRoot     string
	stateDir     string
	rendererDir  string
	daemonPath   string
	daemonViaGo  bool
	keepState    bool
	doPreflight  bool
	port         int
	baseURL      string
	logFile      string
	startedAt    time.Time
	daemon       *exec.Cmd
	daemonCancel context.CancelFunc
	daemonWG     sync.WaitGroup
	client       *wsClient
	steps        []stepReceipt
}

type stepReceipt struct {
	Name    string
	Elapsed time.Duration
	Details string
}

type chatRef struct {
	chatID    string
	tabID     string
	sessionID string
	cwd       string
	provider  string
	archiveAt int64
}

type jobReceipt struct {
	Job        map[string]any
	Chunks     int
	Bytes      int
	StartedAt  time.Time
	EndedAt    time.Time
	FirstChunk *time.Time
}

func newCanary(daemonFlag string, goRunDaemon bool, stateDirFlag string, keepState, doPreflight bool) (*canary, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		return nil, fmt.Errorf("run from repository root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "desktop", "acp", "mock-server.mjs")); err != nil {
		return nil, fmt.Errorf("mock ACP fixture missing: %w", err)
	}
	if _, err := exec.LookPath("qwen"); err != nil {
		return nil, fmt.Errorf("qwen command not on PATH: %w", err)
	}

	tempRoot := ""
	stateDir := strings.TrimSpace(stateDirFlag)
	if stateDir == "" {
		tempRoot, err = os.MkdirTemp("", "workass-b2-canary-*")
		if err != nil {
			return nil, err
		}
		stateDir = filepath.Join(tempRoot, "state")
	} else {
		stateDir, err = filepath.Abs(stateDir)
		if err != nil {
			return nil, err
		}
		if insidePath(root, stateDir) {
			return nil, fmt.Errorf("--state-dir must be scratch outside the repo: %s", stateDir)
		}
		tempRoot = filepath.Dir(stateDir)
	}

	daemonPath := strings.TrimSpace(daemonFlag)
	daemonViaGo := goRunDaemon
	if daemonPath == "" && !daemonViaGo {
		daemonPath = strings.TrimSpace(os.Getenv("CANARY_DAEMON_PATH"))
	}
	if daemonViaGo {
		daemonPath = "go"
	}
	if daemonPath == "" {
		dist := filepath.Join(root, "dist-bin", fmt.Sprintf("workass-%s-%s", runtime.GOOS, runtime.GOARCH))
		if runtime.GOOS == "windows" {
			dist += ".exe"
		}
		if st, err := os.Stat(dist); err == nil && !st.IsDir() {
			daemonPath = dist
		} else {
			daemonPath = "go"
			daemonViaGo = true
		}
	}
	if daemonPath != "go" && !filepath.IsAbs(daemonPath) {
		daemonPath, err = filepath.Abs(daemonPath)
		if err != nil {
			return nil, err
		}
	}

	return &canary{
		repoRoot:    root,
		tempRoot:    tempRoot,
		stateDir:    stateDir,
		rendererDir: filepath.Join(tempRoot, "renderer"),
		daemonPath:  daemonPath,
		daemonViaGo: daemonViaGo,
		keepState:   keepState,
		doPreflight: doPreflight,
	}, nil
}

func (c *canary) run(ctx context.Context) error {
	c.startedAt = time.Now()
	fmt.Printf("CANARY start repo=%s state=%s\n", c.repoRoot, c.stateDir)
	if c.doPreflight {
		if err := c.step("lmstudio", func() (string, error) {
			models, err := ensureLMStudio(ctx)
			if err != nil {
				return "", err
			}
			return "models=" + strings.Join(models, ","), nil
		}); err != nil {
			return err
		}
	}
	if err := c.step("prepare", func() (string, error) {
		if err := c.prepareFilesystem(); err != nil {
			return "", err
		}
		return "providers=mock,qwen renderer=" + c.rendererDir, nil
	}); err != nil {
		return err
	}
	if err := c.step("daemon", func() (string, error) {
		if err := c.startDaemon(ctx); err != nil {
			return "", err
		}
		return fmt.Sprintf("pid=%d url=%s log=%s", c.daemon.Process.Pid, c.baseURL, c.logFile), nil
	}); err != nil {
		return err
	}
	if err := c.step("ws-connect", func() (string, error) {
		client, err := dialWS(ctx, "canary", c.port, "b2-canary", "")
		if err != nil {
			return "", err
		}
		c.client = client
		state, err := client.waitAccessState(ctx, accessTimeout)
		if err != nil {
			return "", err
		}
		if state["state"] != "approved" {
			return "", fmt.Errorf("access state = %#v", state)
		}
		if state["controller"] != true {
			if _, err := c.callOK(ctx, "lan:take-control"); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("state=%s controller=%v", state["state"], state["controller"]), nil
	}); err != nil {
		return err
	}
	if err := c.assertEngineConfig(ctx); err != nil {
		return err
	}

	if err := c.assertCatalog(ctx); err != nil {
		return err
	}

	qwenChat, err := c.newChatSession(ctx, "qwen-main", "qwen-tab", "qwen", filepath.Join(c.tempRoot, "workspaces", "qwen"))
	if err != nil {
		return err
	}
	mockChat, err := c.newChatSession(ctx, "mock-main", "mock-tab", "mock", filepath.Join(c.tempRoot, "workspaces", "mock"))
	if err != nil {
		return err
	}

	if err := c.step("real-prompt-and-mock-isolation", func() (string, error) {
		qwenMark := c.client.mark()
		qwenJob, err := c.startJob(ctx, qwenChat, qwenPrompt)
		if err != nil {
			return "", err
		}
		mockMark := c.client.mark()
		mockJob, err := c.startJob(ctx, mockChat, mockConcurrentPrompt)
		if err != nil {
			return "", err
		}

		type result struct {
			name string
			rec  jobReceipt
			err  error
		}
		results := make(chan result, 2)
		go func() {
			rec, err := c.collectJob(ctx, qwenMark, stringField(qwenJob, "id"), realTurnTimeout)
			results <- result{name: "qwen", rec: rec, err: err}
		}()
		go func() {
			rec, err := c.collectJob(ctx, mockMark, stringField(mockJob, "id"), realTurnTimeout)
			results <- result{name: "mock", rec: rec, err: err}
		}()

		var qwenRec, mockRec jobReceipt
		for i := 0; i < 2; i++ {
			res := <-results
			if res.err != nil {
				return "", fmt.Errorf("%s job: %w", res.name, res.err)
			}
			if res.name == "qwen" {
				qwenRec = res.rec
			} else {
				mockRec = res.rec
			}
		}
		if err := assertDoneEndTurn("qwen real prompt", qwenRec, "qwen"); err != nil {
			return "", err
		}
		if err := assertDoneEndTurn("mock concurrent prompt", mockRec, "mock"); err != nil {
			return "", err
		}
		if qwenRec.Chunks == 0 {
			return "", errors.New("qwen real prompt produced no stdout data chunks")
		}
		if mockRec.Chunks == 0 {
			return "", errors.New("mock concurrent prompt produced no stdout data chunks")
		}
		overlap := mockRec.StartedAt.Before(qwenRec.EndedAt) && qwenRec.StartedAt.Before(mockRec.EndedAt)
		if !overlap {
			return "", fmt.Errorf("mock job did not overlap qwen job qwen=[%s,%s] mock=[%s,%s]",
				qwenRec.StartedAt.Format(time.RFC3339Nano), qwenRec.EndedAt.Format(time.RFC3339Nano),
				mockRec.StartedAt.Format(time.RFC3339Nano), mockRec.EndedAt.Format(time.RFC3339Nano))
		}
		qwenChat.sessionID = stringField(qwenRec.Job, "sessionId")
		mockChat.sessionID = stringField(mockRec.Job, "sessionId")
		if err := c.appendArchive(ctx, qwenChat, qwenPrompt, stringField(qwenRec.Job, "result")); err != nil {
			return "", err
		}
		return fmt.Sprintf("qwenJob=%s qwenChunks=%d qwenBytes=%d qwenStop=%s mockJob=%s mockProvider=%s overlap=%v",
			stringField(qwenRec.Job, "id"), qwenRec.Chunks, qwenRec.Bytes, stringField(qwenRec.Job, "stopReason"),
			stringField(mockRec.Job, "id"), stringField(mockRec.Job, "providerId"), overlap), nil
	}); err != nil {
		return err
	}

	if err := c.assertCancel(ctx, qwenChat); err != nil {
		return err
	}
	return nil
}

func (c *canary) step(name string, fn func() (string, error)) error {
	start := time.Now()
	details, err := fn()
	elapsed := time.Since(start)
	if err != nil {
		fmt.Printf("ASSERT step=%s ok=false elapsed=%s error=%s\n", name, elapsed.Round(time.Millisecond), err)
		return fmt.Errorf("%s: %w", name, err)
	}
	c.steps = append(c.steps, stepReceipt{Name: name, Elapsed: elapsed, Details: details})
	fmt.Printf("ASSERT step=%s ok=true elapsed=%s %s\n", name, elapsed.Round(time.Millisecond), details)
	return nil
}

func ensureLMStudio(ctx context.Context) ([]string, error) {
	models, err := fetchLMStudioModels(ctx)
	if err == nil {
		return models, nil
	}
	lmsPath := filepath.Join(os.Getenv("HOME"), ".lmstudio", "bin", "lms")
	if _, statErr := os.Stat(lmsPath); statErr != nil {
		if lookedUp, lookErr := exec.LookPath("lms"); lookErr == nil {
			lmsPath = lookedUp
		}
	}
	cmd := exec.CommandContext(ctx, lmsPath, "server", "start")
	out, startErr := cmd.CombinedOutput()
	if startErr != nil {
		return nil, fmt.Errorf("LM Studio /v1/models unavailable (%v), and lms server start failed: %w\n%s", err, startErr, out)
	}
	deadline := time.Now().Add(lmStudioStartTimeout)
	var last error
	for time.Now().Before(deadline) {
		models, last = fetchLMStudioModels(ctx)
		if last == nil {
			return models, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("LM Studio server started but /v1/models did not respond: %v", last)
}

func fetchLMStudioModels(ctx context.Context) ([]string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, lmStudioRequestTimout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, lmStudioBaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models status %s", resp.Status)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if strings.TrimSpace(item.ID) != "" {
			models = append(models, item.ID)
		}
	}
	sort.Strings(models)
	for _, id := range models {
		if id == qwenModelID {
			return models, nil
		}
	}
	return nil, fmt.Errorf("LM Studio model %q not loaded; models=%s", qwenModelID, strings.Join(models, ","))
}

func (c *canary) prepareFilesystem() error {
	if err := os.MkdirAll(c.rendererDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.rendererDir, "index.html"), []byte("<!doctype html><head></head><body>workass b2 canary</body>"), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(c.stateDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(c.tempRoot, "workspaces", "qwen"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(c.tempRoot, "workspaces", "mock"), 0o755); err != nil {
		return err
	}
	if err := c.writeAppConfig(); err != nil {
		return err
	}
	providers := []map[string]any{
		{
			"id":      "mock",
			"name":    "Workass Mock ACP",
			"command": "node",
			"args":    []string{filepath.Join(c.repoRoot, "desktop", "acp", "mock-server.mjs")},
			"enabled": true,
			"badge":   "dev",
		},
		{
			"id":      "qwen",
			"name":    "Qwen Code ACP",
			"command": "qwen",
			"args":    []string{"--acp"},
			"env": map[string]string{
				"OPENAI_BASE_URL": lmStudioBaseURL,
				"OPENAI_API_KEY":  "lm-studio",
				"OPENAI_MODEL":    qwenModelID,
			},
			"enabled": true,
			"badge":   "agent",
		},
	}
	data, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(filepath.Dir(c.stateDir), "providers.json"), append(data, '\n'), 0o600)
}

func (c *canary) writeAppConfig() error {
	cfg := map[string]any{
		"version": 1,
		"engine": map[string]any{
			"hibernateTtlMs":          hibernateTTL.Milliseconds(),
			"rssSampleIntervalMs":     500,
			"maxAgeMs":                int64((12 * time.Hour).Milliseconds()),
			"maxRssKb":                4 * 1024 * 1024,
			"spareSessions":           0,
			"compactionEnabled":       false,
			"compactionThresholdPct":  80,
			"compactionKeepLastTurns": 4,
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(filepath.Dir(c.stateDir), "app-config.json"), append(data, '\n'), 0o600)
}

func (c *canary) startDaemon(ctx context.Context) error {
	port, err := reservePort()
	if err != nil {
		return err
	}
	c.port = port
	c.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	c.logFile = filepath.Join(c.tempRoot, "daemon.log")
	logf, err := os.Create(c.logFile)
	if err != nil {
		return err
	}

	args := c.daemonArgs(port)
	command := c.daemonPath
	if c.daemonViaGo {
		args = append([]string{"run", "./cmd/workass"}, args...)
		command = "go"
	}
	daemonCtx, cancel := context.WithCancel(ctx)
	c.daemonCancel = cancel
	cmd := exec.CommandContext(daemonCtx, command, args...)
	cmd.Dir = c.repoRoot
	cmd.Env = withToolPath(os.Environ())
	cmd.Stdout = logf
	cmd.Stderr = logf
	if runtime.GOOS != "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		cancel()
		return err
	}
	c.daemon = cmd
	c.daemonWG.Add(1)
	go func() {
		defer c.daemonWG.Done()
		_ = cmd.Wait()
		_ = logf.Close()
	}()
	return c.waitHealth(ctx, healthTimeout)
}

func (c *canary) daemonArgs(port int) []string {
	return []string{
		"--port", strconv.Itoa(port),
		"--bind", "localhost",
		"--renderer-dir", c.rendererDir,
		"--state-dir", c.stateDir,
		"--trust-localhost", "true",
		"--hibernate-ttl", hibernateTTL.String(),
		"--rss-sample-interval", "500ms",
		"--engine-max-age", "12h",
		"--engine-max-rss-kb", "4194304",
		"--spare-sessions", "0",
		"--compaction-enabled=false",
	}
}

func (c *canary) assertEngineConfig(ctx context.Context) error {
	return c.step("engine-config", func() (string, error) {
		res, err := c.callOK(ctx, "config:get")
		if err != nil {
			return "", err
		}
		cfg, err := asMap(res)
		if err != nil {
			return "", err
		}
		engine, _ := cfg["engine"].(map[string]any)
		ttl := intField(engine, "hibernateTtlMs")
		compactionEnabled := fmt.Sprint(engine["compactionEnabled"])
		if ttl <= 0 || ttl > int((5*time.Second).Milliseconds()) {
			return "", fmt.Errorf("hibernateTtlMs=%d; expected tiny TTL", ttl)
		}
		if compactionEnabled != "false" {
			return "", fmt.Errorf("compactionEnabled=%s; expected false", compactionEnabled)
		}
		return fmt.Sprintf("hibernateTtlMs=%d compactionEnabled=%s", ttl, compactionEnabled), nil
	})
}

func (c *canary) waitHealth(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/workass/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon health did not become ready; log tail:\n%s", tailFile(c.logFile, 80))
}

func (c *canary) assertCatalog(ctx context.Context) error {
	return c.step("catalog", func() (string, error) {
		payload, err := c.client.waitEventSince(ctx, 0, "chat:catalog", catalogTimeout, func(payload map[string]any) bool {
			return catalogGroup(payload, "qwen") != nil
		})
		if err != nil {
			return "", err
		}
		qwenGroup := catalogGroup(payload, "qwen")
		if qwenGroup == nil {
			return "", errors.New("catalog missing qwen group")
		}
		if status := stringField(qwenGroup, "status"); status == "error" {
			return "", fmt.Errorf("qwen catalog status=error: %s", stringField(qwenGroup, "error"))
		}
		return "groups=" + catalogSummary(payload), nil
	})
}

func (c *canary) newChatSession(ctx context.Context, chatID, tabID, providerID, cwd string) (*chatRef, error) {
	chat := &chatRef{chatID: chatID, tabID: tabID, cwd: cwd, provider: providerID}
	err := c.step("new-session-"+providerID, func() (string, error) {
		res, err := c.callOK(ctx, "app-chat:new-session", map[string]any{
			"chatId":     chatID,
			"tabId":      tabID,
			"cwd":        cwd,
			"providerId": providerID,
		})
		if err != nil {
			return "", err
		}
		m, err := asMap(res)
		if err != nil {
			return "", err
		}
		if errText := stringField(m, "error"); errText != "" {
			return "", errors.New(errText)
		}
		sessionID := stringField(m, "sessionId")
		if sessionID == "" {
			return "", fmt.Errorf("new-session missing sessionId: %#v", m)
		}
		if got := stringField(m, "providerId"); got != providerID {
			return "", fmt.Errorf("new-session providerId=%q, want %q", got, providerID)
		}
		chat.sessionID = sessionID
		return fmt.Sprintf("sessionId=%s providerId=%s agent=%s models=%d modes=%d",
			sessionID, stringField(m, "providerId"), stringField(m, "agent"), len(anySlice(m["models"])), len(anySlice(m["modes"]))), nil
	})
	if err != nil {
		return nil, err
	}
	return chat, nil
}

func (c *canary) assertCancel(ctx context.Context, chat *chatRef) error {
	return c.step("cancel", func() (string, error) {
		mark := c.client.mark()
		job, err := c.startJob(ctx, chat, qwenCancelPrompt)
		if err != nil {
			return "", err
		}
		jobID := stringField(job, "id")
		if err := c.waitForRunningJobBeforeCancel(ctx, mark, jobID); err != nil {
			return "", err
		}
		cancelRes, err := c.callOK(ctx, "job:cancel", jobID)
		if err != nil {
			return "", err
		}
		if cancelRes != true {
			return "", fmt.Errorf("job:cancel result=%#v", cancelRes)
		}
		rec, err := c.collectJob(ctx, mark, jobID, cancelTurnTimeout)
		if err != nil {
			return "", err
		}
		chat.sessionID = stringField(rec.Job, "sessionId")
		if stringField(rec.Job, "status") != "failed" || intField(rec.Job, "code") != 130 || stringField(rec.Job, "stopReason") != "cancelled" {
			return "", fmt.Errorf("cancel end job=%#v", rec.Job)
		}
		return fmt.Sprintf("job=%s cancelReply=true chunksBeforeOrDuring=%d bytes=%d status=%s code=%d stopReason=%s",
			jobID, rec.Chunks, rec.Bytes, stringField(rec.Job, "status"), intField(rec.Job, "code"), stringField(rec.Job, "stopReason")), nil
	})
}

func (c *canary) waitForRunningJobBeforeCancel(ctx context.Context, mark int, jobID string) error {
	deadline := time.Now().Add(cancelWarmupMaxWait)
	for {
		events := c.client.eventsSince(mark)
		for _, msg := range events {
			if msg.Channel != "job:event" {
				continue
			}
			payload, _ := msg.Payload.(map[string]any)
			if payload == nil {
				continue
			}
			switch payload["type"] {
			case "data":
				if stringField(payload, "id") == jobID {
					return nil
				}
			case "end":
				job, _ := payload["job"].(map[string]any)
				if stringField(job, "id") == jobID {
					return fmt.Errorf("job ended before cancel could run: %#v", job)
				}
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		case <-c.client.closed:
			return errors.New("websocket closed")
		}
	}
}

func (c *canary) startJob(ctx context.Context, chat *chatRef, prompt string) (map[string]any, error) {
	res, err := c.callOK(ctx, "job:start", map[string]any{
		"kind":       "app-chat",
		"chatId":     chat.chatID,
		"tabId":      chat.tabID,
		"sessionId":  chat.sessionID,
		"cwd":        chat.cwd,
		"providerId": chat.provider,
		"prompt":     prompt,
	})
	if err != nil {
		return nil, err
	}
	job, err := asMap(res)
	if err != nil {
		return nil, err
	}
	if stringField(job, "id") == "" {
		return nil, fmt.Errorf("job:start missing id: %#v", job)
	}
	if got := stringField(job, "providerId"); got != "" && got != chat.provider {
		return nil, fmt.Errorf("job:start providerId=%q want %q", got, chat.provider)
	}
	return job, nil
}

func (c *canary) collectJob(ctx context.Context, mark int, jobID string, timeout time.Duration) (jobReceipt, error) {
	deadline := time.Now().Add(timeout)
	rec := jobReceipt{}
	next := mark
	for {
		c.client.mu.Lock()
		events := append([]wsMessage(nil), c.client.events...)
		c.client.mu.Unlock()
		for next < len(events) {
			msg := events[next]
			next++
			if msg.Channel != "job:event" {
				continue
			}
			payload, _ := msg.Payload.(map[string]any)
			if payload == nil {
				continue
			}
			switch payload["type"] {
			case "start":
				job, _ := payload["job"].(map[string]any)
				if stringField(job, "id") == jobID && rec.StartedAt.IsZero() {
					rec.StartedAt = parseTimeOrNow(stringField(job, "startedAt"))
				}
			case "data":
				if stringField(payload, "id") != jobID || stringField(payload, "stream") != "stdout" {
					continue
				}
				chunk := stringField(payload, "chunk")
				if chunk == "" {
					continue
				}
				if rec.FirstChunk == nil {
					now := time.Now()
					rec.FirstChunk = &now
				}
				rec.Chunks++
				rec.Bytes += len(chunk)
			case "end":
				job, _ := payload["job"].(map[string]any)
				if stringField(job, "id") != jobID {
					continue
				}
				rec.Job = job
				rec.EndedAt = parseTimeOrNow(stringField(job, "finishedAt"))
				if rec.StartedAt.IsZero() {
					rec.StartedAt = rec.EndedAt
				}
				return rec, nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return rec, fmt.Errorf("timed out waiting for job %s after %s; chunks=%d bytes=%d", jobID, timeout, rec.Chunks, rec.Bytes)
		}
		select {
		case <-ctx.Done():
			return rec, ctx.Err()
		case <-time.After(minDuration(remaining, 50*time.Millisecond)):
		case <-c.client.closed:
			return rec, errors.New("websocket closed")
		}
	}
}

func assertDoneEndTurn(label string, rec jobReceipt, providerID string) error {
	if stringField(rec.Job, "status") != "done" || intField(rec.Job, "code") != 0 || stringField(rec.Job, "stopReason") != "end_turn" {
		return fmt.Errorf("%s end job=%#v", label, rec.Job)
	}
	if got := stringField(rec.Job, "providerId"); got != providerID {
		return fmt.Errorf("%s providerId=%q want %q", label, got, providerID)
	}
	return nil
}

func (c *canary) appendArchive(ctx context.Context, chat *chatRef, userPrompt, assistant string) error {
	at := atomic.AddInt64(&chat.archiveAt, 2)
	base := c.startedAt
	messages := []any{
		map[string]any{"role": "user", "content": userPrompt, "status": "done", "at": base.Add(time.Duration(at-1) * time.Millisecond).UTC().Format(time.RFC3339Nano)},
		map[string]any{"role": "assistant", "content": assistant, "status": "done", "at": base.Add(time.Duration(at) * time.Millisecond).UTC().Format(time.RFC3339Nano)},
	}
	res, err := c.callOK(ctx, "chat:archive-append", map[string]any{"tabId": chat.tabID, "messages": messages})
	if err != nil {
		return err
	}
	if res != true {
		return fmt.Errorf("chat:archive-append returned %#v", res)
	}
	return nil
}

func (c *canary) callOK(ctx context.Context, channel string, args ...any) (any, error) {
	msg, err := c.client.invoke(ctx, invokeTimeout, channel, args...)
	if err != nil {
		return nil, err
	}
	if msg.Error != nil && strings.TrimSpace(*msg.Error) != "" {
		return nil, fmt.Errorf("%s reply error: %s", channel, *msg.Error)
	}
	return msg.Result, nil
}

func (c *canary) cleanup() {
	if c.client != nil {
		_, _ = c.callOK(context.Background(), "proc:kill-all")
		c.client.close()
	}
	if c.daemonCancel != nil {
		c.daemonCancel()
	}
	if c.daemon != nil && c.daemon.Process != nil && runtime.GOOS != "windows" {
		_ = syscall.Kill(-c.daemon.Process.Pid, syscall.SIGKILL)
	}
	c.daemonWG.Wait()
	if !c.keepState && c.tempRoot != "" {
		_ = os.RemoveAll(c.tempRoot)
	}
}

func (c *canary) printReport(w io.Writer, passed bool) {
	verdict := "FAIL"
	if passed {
		verdict = "PASS"
	}
	fmt.Fprintf(w, "\nCANARY REPORT verdict=%s elapsed=%s\n", verdict, time.Since(c.startedAt).Round(time.Millisecond))
	fmt.Fprintf(w, "state_dir=%s temp_root=%s kept=%v daemon_log=%s\n", c.stateDir, c.tempRoot, c.keepState, c.logFile)
	for _, step := range c.steps {
		fmt.Fprintf(w, "step=%s elapsed=%s %s\n", step.Name, step.Elapsed.Round(time.Millisecond), step.Details)
	}
	if c.keepState && c.logFile != "" {
		fmt.Fprintf(w, "daemon_log_tail:\n%s\n", tailFile(c.logFile, 40))
	}
}

type wsClient struct {
	name      string
	conn      net.Conn
	reader    *bufio.Reader
	writeMu   sync.Mutex
	seq       atomic.Int64
	closed    chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	pending map[string]chan wsMessage
	events  []wsMessage
}

type wsMessage struct {
	T       string
	ID      any
	Channel string
	Result  any
	Error   *string
	Payload any
}

func dialWS(ctx context.Context, name string, port int, deviceName, token string) (*wsClient, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	path := "/?deviceName=" + url.QueryEscape(deviceName)
	if token != "" {
		path += "&deviceToken=" + url.QueryEscape(token)
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nUser-Agent: workass-b2-canary\r\n\r\n", path, addr, key)
	if _, err := io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if strings.TrimSpace(status) != "HTTP/1.1 101 Switching Protocols" {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket handshake status %q", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	c := &wsClient{name: name, conn: conn, reader: reader, pending: map[string]chan wsMessage{}, closed: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

func (c *wsClient) readLoop() {
	defer c.close()
	for {
		payload, err := readServerFrame(c.reader)
		if err != nil {
			return
		}
		var msg wsMessage
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		if err := dec.Decode(&msg); err != nil {
			continue
		}
		if msg.T == "reply" {
			id := idString(msg.ID)
			c.mu.Lock()
			ch := c.pending[id]
			if ch != nil {
				delete(c.pending, id)
			}
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
			continue
		}
		if msg.T == "event" {
			c.mu.Lock()
			c.events = append(c.events, msg)
			c.mu.Unlock()
		}
	}
}

func (c *wsClient) invoke(ctx context.Context, timeout time.Duration, channel string, args ...any) (wsMessage, error) {
	id := c.seq.Add(1)
	idKey := strconv.FormatInt(id, 10)
	replyCh := make(chan wsMessage, 1)
	c.mu.Lock()
	c.pending[idKey] = replyCh
	c.mu.Unlock()
	payload, err := json.Marshal(map[string]any{"t": "invoke", "id": id, "channel": channel, "args": args})
	if err != nil {
		return wsMessage{}, err
	}
	if err := c.writeFrame(maskedFrame(payload)); err != nil {
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()
		return wsMessage{}, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case msg, ok := <-replyCh:
		if !ok {
			return wsMessage{}, fmt.Errorf("%s websocket closed", c.name)
		}
		return msg, nil
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()
		return wsMessage{}, fmt.Errorf("%s invoke %s timed out after %s", c.name, channel, timeout)
	case <-ctx.Done():
		return wsMessage{}, ctx.Err()
	case <-c.closed:
		return wsMessage{}, fmt.Errorf("%s websocket closed", c.name)
	}
}

func (c *wsClient) writeFrame(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := c.conn.Write(frame)
	return err
}

func (c *wsClient) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func (c *wsClient) eventsSince(mark int) []wsMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if mark < 0 {
		mark = 0
	}
	if mark > len(c.events) {
		mark = len(c.events)
	}
	return append([]wsMessage(nil), c.events[mark:]...)
}

func (c *wsClient) waitAccessState(ctx context.Context, timeout time.Duration) (map[string]any, error) {
	return c.waitEventSince(ctx, 0, "lan:access-state", timeout, func(payload map[string]any) bool {
		return payload["state"] != nil
	})
}

func (c *wsClient) waitEventSince(ctx context.Context, mark int, channel string, timeout time.Duration, pred func(map[string]any) bool) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		for i := mark; i < len(c.events); i++ {
			msg := c.events[i]
			if msg.Channel != channel {
				continue
			}
			payload, _ := msg.Payload.(map[string]any)
			if payload == nil {
				continue
			}
			if pred == nil || pred(payload) {
				c.mu.Unlock()
				return payload, nil
			}
		}
		c.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("%s timed out waiting for event %s after %s", c.name, channel, timeout)
		}
		select {
		case <-time.After(minDuration(remaining, 50*time.Millisecond)):
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, fmt.Errorf("%s websocket closed", c.name)
		}
	}
}

func (c *wsClient) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
		c.mu.Lock()
		for id, ch := range c.pending {
			delete(c.pending, id)
			close(ch)
		}
		c.mu.Unlock()
	})
}

func readServerFrame(reader *bufio.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	opcode := header[0] & 0x0f
	if opcode == 0x8 {
		return nil, io.EOF
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(b[:])
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	if opcode != 0x1 && opcode != 0x0 {
		return nil, fmt.Errorf("unexpected websocket opcode %d", opcode)
	}
	return payload, nil
}

func maskedFrame(payload []byte) []byte {
	headerLen := 2
	switch {
	case len(payload) < 126:
	case len(payload) <= 0xffff:
		headerLen = 4
	default:
		headerLen = 10
	}
	out := make([]byte, headerLen+4+len(payload))
	out[0] = 0x81
	switch {
	case len(payload) < 126:
		out[1] = 0x80 | byte(len(payload))
	case len(payload) <= 0xffff:
		out[1] = 0x80 | 126
		binary.BigEndian.PutUint16(out[2:4], uint16(len(payload)))
	default:
		out[1] = 0x80 | 127
		binary.BigEndian.PutUint64(out[2:10], uint64(len(payload)))
	}
	maskStart := headerLen
	mask := out[maskStart : maskStart+4]
	if _, err := rand.Read(mask); err != nil {
		copy(mask, []byte{1, 2, 3, 4})
	}
	for i, b := range payload {
		out[maskStart+4+i] = b ^ mask[i&3]
	}
	return out
}

func reservePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func catalogGroup(payload map[string]any, providerID string) map[string]any {
	for _, raw := range anySlice(payload["groups"]) {
		group, _ := raw.(map[string]any)
		if stringField(group, "providerId") == providerID {
			return group
		}
	}
	return nil
}

func catalogSummary(payload map[string]any) string {
	var parts []string
	for _, raw := range anySlice(payload["groups"]) {
		group, _ := raw.(map[string]any)
		if group == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%s:models=%d:modes=%d:latencyMs=%s",
			stringField(group, "providerId"),
			stringField(group, "status"),
			len(anySlice(group["models"])),
			len(anySlice(group["modes"])),
			stringField(group, "latencyMs")))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func asMap(v any) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil, fmt.Errorf("expected object, got %#v", v)
	}
	return m, nil
}

func stringField(m map[string]any, key string) string {
	if m == nil || m[key] == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func intField(m map[string]any, key string) int {
	if m == nil || m[key] == nil {
		return 0
	}
	switch v := m[key].(type) {
	case json.Number:
		n, _ := strconv.Atoi(v.String())
		return n
	case float64:
		return int(v)
	case int:
		return v
	default:
		n, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
		return n
	}
}

func anySlice(v any) []any {
	items, _ := v.([]any)
	if items == nil {
		return []any{}
	}
	return items
}

func idString(v any) string {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func parseTimeOrNow(raw string) time.Time {
	if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
		return ts
	}
	return time.Now()
}

func tailFile(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	parts := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func insidePath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..")
}

func withToolPath(env []string) []string {
	current := os.Getenv("PATH")
	prefixes := []string{"/opt/homebrew/bin", filepath.Join(os.Getenv("HOME"), ".lmstudio", "bin")}
	path := strings.Join(append(prefixes, current), string(os.PathListSeparator))
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			out = append(out, "PATH="+path)
			replaced = true
			continue
		}
		out = append(out, item)
	}
	if !replaced {
		out = append(out, "PATH="+path)
	}
	return out
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
