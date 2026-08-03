package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
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
	defaultSoakMinutes      = 30
	defaultChatCount        = 10
	hibernateTTL            = 5 * time.Second
	rssSampleEvery          = 10 * time.Second
	procSampleEvery         = 10 * time.Second
	wsReplyTimeout          = 20 * time.Second
	jobTimeout              = 40 * time.Second
	accessTimeout           = 5 * time.Second
	crashRecoveryCooldown   = 5*time.Minute + 10*time.Second
	controllerReturnTimeout = 10 * time.Second
)

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner, err := newRunner()
	if err != nil {
		fmt.Fprintf(os.Stderr, "SOAK FAIL: %v\n", err)
		os.Exit(1)
	}
	ok := false
	defer func() {
		runner.cleanup()
		if !ok {
			runner.printReport(os.Stderr, false)
		}
	}()
	if err := runner.run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "SOAK FAIL: %v\n", err)
		os.Exit(1)
	}
	ok = true
	runner.printReport(os.Stdout, true)
}

type config struct {
	repoRoot string
	tempDir  string
	duration time.Duration
	chats    int
}

type runner struct {
	cfg      config
	stats    *stats
	daemon   *exec.Cmd
	daemonWG sync.WaitGroup
	logFile  string
	baseURL  string
	port     int

	clients      []*wsClient
	controller   *wsClient
	peer         *wsClient
	currentCtl   atomic.Pointer[wsClient]
	chats        []*chatState
	startedAt    time.Time
	deadline     time.Time
	cancelRun    context.CancelFunc
	stopSamplers context.CancelFunc

	failOnce sync.Once
	failErr  atomic.Value
}

type chatState struct {
	mu        sync.Mutex
	index     int
	chatID    string
	tabID     string
	sessionID string
	workspace string
	repoAlpha string
	repoBeta  string
	archiveAt int64
	lastCrash time.Time
	firstUser string
}

func newRunner() (*runner, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, "desktop", "acp", "mock-server.mjs")); err != nil {
		return nil, fmt.Errorf("mock ACP fixture not found from %s: %w", root, err)
	}
	minutes := float64(defaultSoakMinutes)
	if raw := strings.TrimSpace(os.Getenv("SOAK_MINUTES")); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid SOAK_MINUTES=%q", raw)
		}
		minutes = parsed
	}
	chats := defaultChatCount
	if raw := strings.TrimSpace(os.Getenv("SOAK_CHATS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < defaultChatCount {
			return nil, fmt.Errorf("invalid SOAK_CHATS=%q; need at least %d", raw, defaultChatCount)
		}
		chats = parsed
	}
	tempDir, err := os.MkdirTemp("", "workass-soak-*")
	if err != nil {
		return nil, err
	}
	return &runner{
		cfg: config{
			repoRoot: root,
			tempDir:  tempDir,
			duration: time.Duration(minutes * float64(time.Minute)),
			chats:    chats,
		},
		stats: newStats(),
	}, nil
}

func (r *runner) run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	r.cancelRun = cancel
	defer cancel()

	r.startedAt = time.Now()
	r.deadline = r.startedAt.Add(r.cfg.duration)
	fmt.Printf("SOAK start duration=%s chats=%d temp=%s\n", r.cfg.duration.Round(time.Second), r.cfg.chats, r.cfg.tempDir)

	if err := r.prepareFilesystem(); err != nil {
		return err
	}
	if err := r.startDaemon(ctx); err != nil {
		return err
	}
	if err := r.connectClients(ctx); err != nil {
		return err
	}
	if err := r.createChats(ctx); err != nil {
		return err
	}
	r.startSamplers(ctx)

	cycle := 0
	for time.Now().Before(r.deadline) {
		if err := r.failed(); err != nil {
			return err
		}
		cycle++
		if err := r.runCycle(ctx, cycle); err != nil {
			r.fail(err)
			return err
		}
		r.stats.inc("cycles.completed", 1)
		fmt.Printf("SOAK cycle=%d elapsed=%s compactions=%d cancels=%d/%d recoveries=%d\n",
			cycle,
			time.Since(r.startedAt).Round(time.Second),
			r.stats.get("compactions"),
			r.stats.get("cancels.succeeded"),
			r.stats.get("cancels.started"),
			r.stats.get("crash.recoveries"),
		)
	}
	if err := r.failed(); err != nil {
		return err
	}
	if err := r.finalAssertions(); err != nil {
		return err
	}
	return nil
}

func (r *runner) prepareFilesystem() error {
	renderer := filepath.Join(r.cfg.tempDir, "renderer")
	if err := os.MkdirAll(renderer, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body>workass soak</body>"), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.cfg.tempDir, "state"), 0o755); err != nil {
		return err
	}
	if err := r.writeSoakAppConfig(); err != nil {
		return err
	}
	for i := 0; i < r.cfg.chats; i++ {
		workspace := filepath.Join(r.cfg.tempDir, "fixtures", fmt.Sprintf("chat-%02d", i))
		alpha := filepath.Join(workspace, "alpha")
		beta := filepath.Join(workspace, "beta")
		if err := initGitRepo(alpha, map[string]string{"work.txt": fmt.Sprintf("chat %02d base\n", i)}); err != nil {
			return err
		}
		if err := initGitRepo(beta, map[string]string{"other.txt": fmt.Sprintf("chat %02d other\n", i)}); err != nil {
			return err
		}
		r.chats = append(r.chats, &chatState{
			index:     i,
			chatID:    fmt.Sprintf("soak-chat-%02d", i),
			tabID:     fmt.Sprintf("soak-tab-%02d", i),
			workspace: workspace,
			repoAlpha: alpha,
			repoBeta:  beta,
		})
	}
	return nil
}

func (r *runner) writeSoakAppConfig() error {
	cfg := map[string]any{
		"version": 1,
		"engine": map[string]any{
			"hibernateTtlMs":          hibernateTTL.Milliseconds(),
			"rssSampleIntervalMs":     int64(1000),
			"maxAgeMs":                int64((12 * time.Hour).Milliseconds()),
			"maxRssKb":                4 * 1024 * 1024,
			"spareSessions":           0,
			"compactionEnabled":       true,
			"compactionThresholdPct":  50,
			"compactionKeepLastTurns": 1,
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.cfg.tempDir, "app-config.json"), append(data, '\n'), 0o600)
}

func (r *runner) startDaemon(ctx context.Context) error {
	port, err := reservePort()
	if err != nil {
		return err
	}
	r.port = port
	r.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	r.logFile = filepath.Join(r.cfg.tempDir, "daemon.log")
	logf, err := os.Create(r.logFile)
	if err != nil {
		return err
	}

	var cmd *exec.Cmd
	args := r.daemonArgs(port)
	binaryPath, err := r.daemonBinaryPath(ctx, logf)
	if err != nil {
		_ = logf.Close()
		return err
	}
	cmd = exec.CommandContext(ctx, binaryPath, args...)
	cmd.Dir = r.cfg.repoRoot
	cmd.Env = append(os.Environ(),
		"WORKASS_MOCK_ACP_DELAY_MS=25",
		"WORKASS_MOCK_ACP_TRACE_FILE="+filepath.Join(r.cfg.tempDir, "mock-prompts.jsonl"),
	)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return err
	}
	r.daemon = cmd
	r.daemonWG.Add(1)
	go func() {
		defer r.daemonWG.Done()
		err := cmd.Wait()
		_ = logf.Close()
		if err != nil && r.cancelRun != nil && time.Now().Before(r.deadline) {
			r.fail(fmt.Errorf("daemon exited unexpectedly: %w", err))
		}
	}()
	if err := r.waitHealth(ctx, 20*time.Second); err != nil {
		return err
	}
	fmt.Printf("SOAK daemon pid=%d url=%s log=%s\n", cmd.Process.Pid, r.baseURL, r.logFile)
	return nil
}

func (r *runner) daemonBinaryPath(ctx context.Context, logf *os.File) (string, error) {
	if override := strings.TrimSpace(os.Getenv("SOAK_DAEMON_PATH")); override != "" {
		return override, nil
	}
	distPath := filepath.Join(r.cfg.repoRoot, "dist-bin", fmt.Sprintf("workass-%s-%s", runtime.GOOS, runtime.GOARCH))
	if runtime.GOOS == "windows" {
		distPath += ".exe"
	}
	if truthy(os.Getenv("SOAK_USE_DIST")) {
		if st, err := os.Stat(distPath); err == nil && !st.IsDir() {
			return distPath, nil
		}
		return "", fmt.Errorf("SOAK_USE_DIST=1 but %s is not executable", distPath)
	}
	out := filepath.Join(r.cfg.tempDir, "workass-soak-daemon")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	fmt.Fprintf(logf, "[soak] building fresh daemon: go build -trimpath -o %s ./cmd/workass\n", out)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/workass")
	cmd.Dir = r.cfg.repoRoot
	cmd.Env = os.Environ()
	buildOut, err := cmd.CombinedOutput()
	if len(buildOut) > 0 {
		_, _ = logf.Write(buildOut)
		if buildOut[len(buildOut)-1] != '\n' {
			_, _ = logf.WriteString("\n")
		}
	}
	if err != nil {
		return "", fmt.Errorf("build fresh daemon: %w\n%s", err, buildOut)
	}
	return out, nil
}

func (r *runner) daemonArgs(port int) []string {
	mockPath := filepath.Join(r.cfg.repoRoot, "desktop", "acp", "mock-server.mjs")
	acpArgs, _ := json.Marshal([]string{mockPath})
	return []string{
		"--port", strconv.Itoa(port),
		"--bind", "localhost",
		"--renderer-dir", filepath.Join(r.cfg.tempDir, "renderer"),
		"--state-dir", filepath.Join(r.cfg.tempDir, "state"),
		"--acp-command", "node",
		"--acp-args", string(acpArgs),
		"--trust-localhost", "true",
		"--hibernate-ttl", hibernateTTL.String(),
		"--rss-sample-interval", "1s",
		"--engine-max-age", "12h",
		"--engine-max-rss-kb", "4194304",
		"--spare-sessions", "0",
		"--compaction-enabled", "true",
		"--compaction-threshold-pct", "50",
		"--compaction-keep-last-turns", "1",
	}
}

func truthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (r *runner) waitHealth(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/workass/health", nil)
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
	return fmt.Errorf("daemon health did not become ready; log tail:\n%s", tailFile(r.logFile, 80))
}

func (r *runner) connectClients(ctx context.Context) error {
	for i := 0; i < r.cfg.chats; i++ {
		name := fmt.Sprintf("ws-%02d", i)
		c, err := dialWS(ctx, name, r.port, fmt.Sprintf("soak-%02d", i), "", r.stats)
		if err != nil {
			return err
		}
		r.clients = append(r.clients, c)
		state, err := c.waitAccessState(ctx, accessTimeout)
		if err != nil {
			return err
		}
		if state["state"] != "approved" {
			return fmt.Errorf("%s access state = %#v", name, state)
		}
		if i == 0 {
			r.controller = c
			r.currentCtl.Store(c)
			if state["controller"] != true {
				if _, err := r.callOK(ctx, c, "lan:take-control"); err != nil {
					return err
				}
			}
		}
		if i == 1 {
			r.peer = c
		}
	}
	if r.peer == nil {
		return errors.New("peer WS client missing")
	}
	return nil
}

func (r *runner) createChats(ctx context.Context) error {
	for _, chat := range r.chats {
		mark := r.controller.mark()
		res, err := r.callOK(ctx, r.controller, "app-chat:new-session", map[string]any{
			"tabId":      chat.tabID,
			"chatId":     chat.chatID,
			"cwd":        chat.workspace,
			"providerId": "mock",
		})
		if err != nil {
			return err
		}
		m, err := asMap(res)
		if err != nil {
			return err
		}
		sessionID := stringField(m, "sessionId")
		if sessionID == "" {
			return fmt.Errorf("new-session for %s returned %#v", chat.chatID, m)
		}
		chat.setSession(sessionID)
		_, _ = r.controller.waitEventSince(ctx, mark, "chat:env", 3*time.Second, func(payload map[string]any) bool {
			return stringField(payload, "chatId") == chat.chatID
		})
	}
	return nil
}

func (r *runner) startSamplers(ctx context.Context) {
	sctx, cancel := context.WithCancel(ctx)
	r.stopSamplers = cancel
	go r.daemonRSSSampler(sctx)
	go r.procSampler(sctx)
}

func (r *runner) daemonRSSSampler(ctx context.Context) {
	ticker := time.NewTicker(rssSampleEvery)
	defer ticker.Stop()
	for {
		r.sampleDaemonRSS()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *runner) procSampler(ctx context.Context) {
	ticker := time.NewTicker(procSampleEvery)
	defer ticker.Stop()
	for {
		r.sampleProcList(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *runner) sampleDaemonRSS() {
	if r.daemon == nil || r.daemon.Process == nil {
		return
	}
	rss, err := processRSSKB(r.daemon.Process.Pid)
	if err != nil {
		r.stats.addNote("daemon rss sample failed: " + err.Error())
		return
	}
	r.stats.addDaemonRSS(rssSample{At: time.Now(), RSSKB: rss})
}

func (r *runner) sampleProcList(ctx context.Context) {
	ctl := r.currentController()
	if ctl == nil {
		return
	}
	res, err := r.callOKWithTimeout(ctx, ctl, "proc:list", 5*time.Second)
	if err != nil {
		r.stats.addNote("proc:list sample failed: " + err.Error())
		return
	}
	m, err := asMap(res)
	if err != nil {
		r.stats.addNote("proc:list shape failed: " + err.Error())
		return
	}
	items, _ := m["processes"].([]any)
	sample := procSample{At: time.Now(), Total: len(items)}
	for _, raw := range items {
		p, _ := raw.(map[string]any)
		if p == nil {
			continue
		}
		if p["engine"] == true {
			sample.Engines++
			switch stringField(p, "state") {
			case "active":
				sample.Active++
			case "idle":
				sample.Idle++
			case "hibernated":
				sample.Hibernated++
			case "warm":
				sample.Warm++
			}
			sample.EngineRSSKB += int64(intField(p, "rssKb"))
		}
	}
	r.stats.addProcSample(sample)
}

func (r *runner) runCycle(ctx context.Context, cycle int) error {
	type cycleScenario struct {
		chat *chatState
		name string
		fn   func(context.Context, *chatState, int) error
	}
	scenarios := []cycleScenario{
		{r.chats[0], "normal", r.scenarioNormal},
		{r.chats[1], "cancel", r.scenarioCancel},
		{r.chats[2], "steer", r.scenarioSteer},
		{r.chats[3], "queue-flush", r.scenarioQueueFlush},
		{r.chats[4], "permission-approve", func(ctx context.Context, c *chatState, n int) error {
			return r.scenarioPermission(ctx, c, n, "allow-once")
		}},
		{r.chats[5], "permission-reject", func(ctx context.Context, c *chatState, n int) error {
			return r.scenarioPermission(ctx, c, n, "reject")
		}},
		{r.chats[6], "compaction", r.scenarioCompaction},
		{r.chats[7], "crash", r.scenarioCrashOrNormal},
		{r.chats[8], "hibernate", r.scenarioHibernate},
		{r.chats[9], "checkpoint", r.scenarioCheckpoint},
	}

	errCh := make(chan error, len(scenarios))
	var wg sync.WaitGroup
	for _, sc := range scenarios {
		sc := sc
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			if err := sc.fn(ctx, sc.chat, cycle); err != nil {
				errCh <- fmt.Errorf("cycle %d scenario %s chat %s: %w", cycle, sc.name, sc.chat.chatID, err)
				return
			}
			r.stats.addScenario(sc.name, time.Since(start))
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	if err := r.scenarioFork(ctx, r.chats[0], cycle); err != nil {
		return fmt.Errorf("cycle %d fork: %w", cycle, err)
	}
	if err := r.scenarioControllerHandoff(ctx, r.chats[4], cycle); err != nil {
		return fmt.Errorf("cycle %d controller handoff: %w", cycle, err)
	}
	return nil
}

func (r *runner) scenarioNormal(ctx context.Context, chat *chatState, cycle int) error {
	prompt := fmt.Sprintf("normal cycle %d chat %02d", cycle, chat.index)
	end, err := r.runTurn(ctx, chat, prompt, expectDone, true)
	if err != nil {
		return err
	}
	if !strings.Contains(stringField(end, "result"), prompt) {
		return fmt.Errorf("normal result missing prompt: %#v", end)
	}
	if chat.firstUser == "" {
		chat.firstUser = prompt
	}
	return nil
}

func (r *runner) scenarioCancel(ctx context.Context, chat *chatState, cycle int) error {
	if err := r.ensureLive(ctx, chat, fmt.Sprintf("cancel warmup cycle %d", cycle)); err != nil {
		return err
	}
	ctl := r.currentController()
	mark := ctl.mark()
	prompt := fmt.Sprintf("[mock:slow] cancel cycle %d", cycle)
	job, err := r.startJob(ctx, ctl, chat, prompt)
	if err != nil {
		return err
	}
	jobID := stringField(job, "id")
	if jobID == "" {
		return fmt.Errorf("cancel job missing id: %#v", job)
	}
	r.stats.inc("cancels.started", 1)
	time.Sleep(80 * time.Millisecond)
	res, err := r.callOK(ctx, ctl, "job:cancel", jobID)
	if err != nil {
		return err
	}
	if res != true {
		return fmt.Errorf("job:cancel returned %#v", res)
	}
	end, err := r.waitJobEnd(ctx, ctl, mark, jobID)
	if err != nil {
		return err
	}
	chat.setSessionIfNonEmpty(stringField(end, "sessionId"))
	if stringField(end, "status") != "failed" || intField(end, "code") != 130 {
		return fmt.Errorf("cancel end = %#v", end)
	}
	r.stats.inc("cancels.succeeded", 1)
	return nil
}

func (r *runner) scenarioSteer(ctx context.Context, chat *chatState, cycle int) error {
	if err := r.openFreshSession(ctx, chat); err != nil {
		return err
	}
	ctl := r.currentController()
	mark := ctl.mark()
	prompt := fmt.Sprintf("[mock:slow] [mock:steer] steer base cycle %d", cycle)
	job, err := r.startJob(ctx, ctl, chat, prompt)
	if err != nil {
		return err
	}
	jobID := stringField(job, "id")
	time.Sleep(100 * time.Millisecond)
	sessionID := chat.session()
	if sessionID == "" {
		return errors.New("steer missing session after start")
	}
	steerText := fmt.Sprintf("steer follow-up cycle %d", cycle)
	res, err := r.callOK(ctx, ctl, "app-chat:steer", map[string]any{"sessionId": sessionID, "prompt": steerText, "images": []any{}})
	if err != nil {
		return err
	}
	m, err := asMap(res)
	if err != nil {
		return err
	}
	if m["ok"] != true || m["live"] != true || m["queued"] != false {
		return fmt.Errorf("steer reply = %#v", m)
	}
	end, err := r.waitJobEnd(ctx, ctl, mark, jobID)
	if err != nil {
		return err
	}
	chat.setSessionIfNonEmpty(stringField(end, "sessionId"))
	result := stringField(end, "result")
	if stringField(end, "status") != "done" || !strings.Contains(result, "Steer input: "+steerText+".") {
		return fmt.Errorf("steer end = %#v", end)
	}
	return r.appendArchive(ctx, ctl, chat, prompt, result)
}

func (r *runner) ensureLive(ctx context.Context, chat *chatState, prompt string) error {
	state, err := r.chatEngineState(ctx, chat)
	if err != nil {
		return err
	}
	if state != "hibernated" {
		return nil
	}
	end, err := r.runTurn(ctx, chat, prompt, expectDone, true)
	if err != nil {
		return err
	}
	if !strings.Contains(stringField(end, "result"), prompt) {
		return fmt.Errorf("warmup result missing prompt: %#v", end)
	}
	return nil
}

func (r *runner) openFreshSession(ctx context.Context, chat *chatState) error {
	ctl := r.currentController()
	mark := ctl.mark()
	res, err := r.callOK(ctx, ctl, "app-chat:new-session", map[string]any{
		"tabId":      chat.tabID,
		"chatId":     chat.chatID,
		"cwd":        chat.workspace,
		"providerId": "mock",
	})
	if err != nil {
		return err
	}
	m, err := asMap(res)
	if err != nil {
		return err
	}
	if msg := stringField(m, "error"); msg != "" {
		return fmt.Errorf("new session for steer failed: %s", msg)
	}
	sessionID := stringField(m, "sessionId")
	if sessionID == "" {
		return fmt.Errorf("new session missing sessionId: %#v", m)
	}
	chat.setSession(sessionID)
	_, _ = ctl.waitEventSince(ctx, mark, "chat:env", 2*time.Second, func(payload map[string]any) bool {
		return stringField(payload, "chatId") == chat.chatID
	})
	return nil
}

func (r *runner) chatEngineState(ctx context.Context, chat *chatState) (string, error) {
	res, err := r.callOKWithTimeout(ctx, r.currentController(), "proc:list", 4*time.Second)
	if err != nil {
		return "", err
	}
	m, err := asMap(res)
	if err != nil {
		return "", err
	}
	items, _ := m["processes"].([]any)
	for _, raw := range items {
		p, _ := raw.(map[string]any)
		if p != nil && stringField(p, "chatId") == chat.chatID {
			return stringField(p, "state"), nil
		}
	}
	return "", nil
}

func (r *runner) scenarioQueueFlush(ctx context.Context, chat *chatState, cycle int) error {
	first := fmt.Sprintf("[mock:slow] queue running cycle %d", cycle)
	queued := fmt.Sprintf("queued flush cycle %d", cycle)
	end, err := r.runTurn(ctx, chat, first, expectDone, true)
	if err != nil {
		return err
	}
	if !strings.Contains(stringField(end, "result"), first) {
		return fmt.Errorf("queue first result missing prompt: %#v", end)
	}
	r.stats.inc("queue.local.enqueued", 1)
	next, err := r.runTurn(ctx, chat, queued, expectDone, true)
	if err != nil {
		return err
	}
	if !strings.Contains(stringField(next, "result"), queued) {
		return fmt.Errorf("queue flushed result missing prompt: %#v", next)
	}
	r.stats.inc("queue.local.flushed", 1)
	return nil
}

func (r *runner) scenarioPermission(ctx context.Context, chat *chatState, cycle int, optionID string) error {
	ctl := r.currentController()
	mark := ctl.mark()
	prompt := fmt.Sprintf("[mock:permission] %s cycle %d", optionID, cycle)
	job, err := r.startJob(ctx, ctl, chat, prompt)
	if err != nil {
		return err
	}
	jobID := stringField(job, "id")
	req, err := ctl.waitEventSince(ctx, mark, "chat:permission-request", 10*time.Second, func(payload map[string]any) bool {
		return stringField(payload, "jobId") == jobID
	})
	if err != nil {
		return err
	}
	permID := stringField(req, "id")
	if permID == "" {
		return fmt.Errorf("permission request missing id: %#v", req)
	}
	if _, err := r.callOK(ctx, ctl, "chat:permission-decide", map[string]any{"id": permID, "optionId": optionID}); err != nil {
		return err
	}
	end, err := r.waitJobEnd(ctx, ctl, mark, jobID)
	if err != nil {
		return err
	}
	chat.setSessionIfNonEmpty(stringField(end, "sessionId"))
	result := stringField(end, "result")
	if stringField(end, "status") != "done" || !strings.Contains(result, "selected "+optionID) {
		return fmt.Errorf("permission end = %#v", end)
	}
	r.stats.inc("permissions."+optionID, 1)
	return r.appendArchive(ctx, ctl, chat, prompt, result)
}

func (r *runner) scenarioCompaction(ctx context.Context, chat *chatState, cycle int) error {
	ctl := r.currentController()
	mark := ctl.mark()
	oldSession := chat.session()
	prompt := fmt.Sprintf("[mock:bigusage] compact cycle %d", cycle)
	job, err := r.startJob(ctx, ctl, chat, prompt)
	if err != nil {
		return err
	}
	jobID := stringField(job, "id")
	if _, err := ctl.waitEventSince(ctx, mark, "job:event", 10*time.Second, func(payload map[string]any) bool {
		return payload["type"] == "usage" && stringField(payload, "sessionId") != "" && intField(payload, "usedPct") >= 50
	}); err != nil {
		return err
	}
	compacted, err := ctl.waitEventSince(ctx, mark, "chat:compacted", 20*time.Second, func(payload map[string]any) bool {
		return stringField(payload, "tabId") == chat.tabID
	})
	if err != nil {
		return err
	}
	newSession := stringField(compacted, "sessionId")
	if newSession == "" || newSession == oldSession {
		return fmt.Errorf("bad compacted payload old=%s payload=%#v", oldSession, compacted)
	}
	chat.setSession(newSession)
	r.stats.inc("compactions", 1)
	end, err := r.waitJobEnd(ctx, ctl, mark, jobID)
	if err != nil {
		return err
	}
	if stringField(end, "status") != "done" {
		return fmt.Errorf("compaction source end = %#v", end)
	}
	if err := r.appendArchive(ctx, ctl, chat, prompt, stringField(end, "result")); err != nil {
		return err
	}
	nextPrompt := fmt.Sprintf("after compact cycle %d", cycle)
	next, err := r.runTurn(ctx, chat, nextPrompt, expectDone, true)
	if err != nil {
		return err
	}
	if !strings.Contains(stringField(next, "result"), nextPrompt) {
		return fmt.Errorf("post-compact result missing prompt: %#v", next)
	}
	return nil
}

func (r *runner) scenarioCrashOrNormal(ctx context.Context, chat *chatState, cycle int) error {
	chat.mu.Lock()
	last := chat.lastCrash
	chat.mu.Unlock()
	if !last.IsZero() && time.Since(last) < crashRecoveryCooldown {
		return r.scenarioNormal(ctx, chat, cycle)
	}
	ctl := r.currentController()
	mark := ctl.mark()
	oldSession := chat.session()
	prompt := fmt.Sprintf("[mock:crash] crash cycle %d", cycle)
	job, err := r.startJob(ctx, ctl, chat, prompt)
	if err != nil {
		return err
	}
	jobID := stringField(job, "id")
	recovered, err := ctl.waitEventSince(ctx, mark, "chat:engine-recovered", 20*time.Second, func(payload map[string]any) bool {
		return stringField(payload, "oldSessionId") == oldSession || stringField(payload, "tabId") == chat.tabID
	})
	if err != nil {
		return err
	}
	newSession := stringField(recovered, "sessionId")
	if newSession == "" || newSession == oldSession {
		return fmt.Errorf("bad recovery payload old=%s payload=%#v", oldSession, recovered)
	}
	chat.setSession(newSession)
	chat.mu.Lock()
	chat.lastCrash = time.Now()
	chat.mu.Unlock()
	r.stats.inc("crash.recoveries", 1)
	end, err := r.waitJobEnd(ctx, ctl, mark, jobID)
	if err != nil {
		return err
	}
	if stringField(end, "status") != "failed" || stringField(end, "stopReason") != "engine-crash" || end["crashInterrupted"] != true {
		return fmt.Errorf("crash end = %#v", end)
	}
	nextPrompt := fmt.Sprintf("after crash recovery cycle %d", cycle)
	next, err := r.runTurn(ctx, chat, nextPrompt, expectDone, true)
	if err != nil {
		return err
	}
	if !strings.Contains(stringField(next, "result"), nextPrompt) {
		return fmt.Errorf("post-crash result missing prompt: %#v", next)
	}
	return nil
}

func (r *runner) scenarioHibernate(ctx context.Context, chat *chatState, cycle int) error {
	beforePrompt := fmt.Sprintf("hibernate-before cycle %d", cycle)
	before, err := r.runTurn(ctx, chat, beforePrompt, expectDone, true)
	if err != nil {
		return err
	}
	if !strings.Contains(stringField(before, "result"), beforePrompt) {
		return fmt.Errorf("hibernate before result missing prompt: %#v", before)
	}
	oldSession := chat.session()
	if err := r.waitHibernated(ctx, chat, 9*time.Second); err != nil {
		return err
	}
	r.stats.inc("hibernations", 1)
	afterPrompt := fmt.Sprintf("after hibernate cycle %d", cycle)
	after, err := r.runTurn(ctx, chat, afterPrompt, expectDone, true)
	if err != nil {
		return err
	}
	newSession := chat.session()
	if newSession == "" || newSession == oldSession {
		return fmt.Errorf("hibernate did not replace session old=%s new=%s end=%#v", oldSession, newSession, after)
	}
	result := stringField(after, "result")
	if !strings.Contains(result, "Mock ACP turn 1:") || !strings.Contains(result, "Previous conversation") || !strings.Contains(result, beforePrompt) || !strings.Contains(result, afterPrompt) {
		return fmt.Errorf("hibernate replay result = %q", result)
	}
	sessionAfterReplay := chat.session()
	nextPrompt := fmt.Sprintf("after hibernate second cycle %d", cycle)
	next, err := r.runTurn(ctx, chat, nextPrompt, expectDone, true)
	if err != nil {
		return err
	}
	nextResult := stringField(next, "result")
	if chat.session() == sessionAfterReplay {
		if !strings.Contains(nextResult, "Mock ACP turn 2:") || strings.Contains(nextResult, "Previous conversation") {
			return fmt.Errorf("hibernate same-session double-seed result = %q", nextResult)
		}
	} else {
		if !strings.Contains(nextResult, "Mock ACP turn 1:") || !strings.Contains(nextResult, "Previous conversation") || !strings.Contains(nextResult, nextPrompt) {
			return fmt.Errorf("hibernate replacement replay result = %q", nextResult)
		}
	}
	r.stats.inc("replay.once.assertions", 1)
	return nil
}

func (r *runner) scenarioCheckpoint(ctx context.Context, chat *chatState, cycle int) error {
	ctl := r.currentController()
	mark := ctl.mark()
	prompt := fmt.Sprintf("[mock:slow] checkpoint cycle %d", cycle)
	job, err := r.startJob(ctx, ctl, chat, prompt)
	if err != nil {
		return err
	}
	jobID := stringField(job, "id")
	filePath := filepath.Join(chat.repoAlpha, "work.txt")
	baseHash, err := fileSHA256(filePath)
	if err != nil {
		return err
	}
	content := fmt.Sprintf("chat %02d base\ncycle %d line one\ncycle %d line two\n", chat.index, cycle, cycle)
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		return err
	}
	end, err := r.waitJobEnd(ctx, ctl, mark, jobID)
	if err != nil {
		return err
	}
	chat.setSessionIfNonEmpty(stringField(end, "sessionId"))
	if stringField(end, "status") != "done" {
		return fmt.Errorf("checkpoint turn end = %#v", end)
	}
	if err := r.appendArchive(ctx, ctl, chat, prompt, stringField(end, "result")); err != nil {
		return err
	}
	env, err := ctl.waitEventSince(ctx, mark, "chat:env", 8*time.Second, func(payload map[string]any) bool {
		if stringField(payload, "chatId") != chat.chatID {
			return false
		}
		return envHasRepoFile(payload, "alpha", "work.txt")
	})
	if err != nil {
		return err
	}
	if !envHasRepoFile(env, "alpha", "work.txt") {
		return fmt.Errorf("chat:env missing alpha/work.txt: %#v", env)
	}
	cpsRaw, err := r.callOK(ctx, ctl, "chat:checkpoints", map[string]any{"chatId": chat.chatID, "tabId": chat.tabID})
	if err != nil {
		return err
	}
	cp, turnSeq, err := latestCheckpoint(cpsRaw, "alpha")
	if err != nil {
		return err
	}
	if turnSeq <= 0 || stringField(cp, "jobId") == "" {
		return fmt.Errorf("bad checkpoint = %#v", cp)
	}
	diffRaw, err := r.callOK(ctx, ctl, "chat:diff", map[string]any{"chatId": chat.chatID, "repo": "alpha", "path": "work.txt"})
	if err != nil {
		return err
	}
	diff, err := asMap(diffRaw)
	if err != nil {
		return err
	}
	diffText := stringField(diff, "text")
	if !strings.Contains(diffText, "+cycle "+strconv.Itoa(cycle)+" line one") || diff["truncated"] != false {
		return fmt.Errorf("bad diff = %#v", diff)
	}
	rewindRaw, err := r.callOK(ctx, ctl, "chat:rewind", map[string]any{"chatId": chat.chatID, "turnSeq": turnSeq})
	if err != nil {
		return err
	}
	rewind, err := asMap(rewindRaw)
	if err != nil {
		return err
	}
	if rewind["ok"] != true {
		return fmt.Errorf("rewind reply = %#v", rewind)
	}
	if _, err := ctl.waitEventSince(ctx, mark, "chat:checkpoint-restored", 5*time.Second, func(payload map[string]any) bool {
		return stringField(payload, "chatId") == chat.chatID && intField(payload, "turnSeq") == turnSeq
	}); err != nil {
		return err
	}
	restoredHash, err := fileSHA256(filePath)
	if err != nil {
		return err
	}
	if restoredHash != baseHash {
		return fmt.Errorf("rewind hash=%s want=%s", restoredHash, baseHash)
	}
	r.stats.inc("checkpoints.rewinds", 1)
	return nil
}

func (r *runner) scenarioFork(ctx context.Context, chat *chatState, cycle int) error {
	if chat.firstUser == "" {
		if err := r.scenarioNormal(ctx, chat, cycle); err != nil {
			return err
		}
	}
	ctl := r.currentController()
	forkTab := fmt.Sprintf("%s-fork", chat.tabID)
	forkChat := fmt.Sprintf("%s-fork", chat.chatID)
	res, err := r.callOK(ctx, ctl, "app-chat:fork", map[string]any{
		"tabId":     chat.tabID,
		"newTabId":  forkTab,
		"chatId":    chat.chatID,
		"newChatId": forkChat,
		"atTurn":    1,
		"cwd":       chat.workspace,
	})
	if err != nil {
		return err
	}
	m, err := asMap(res)
	if err != nil {
		return err
	}
	sessionID := stringField(m, "sessionId")
	if sessionID == "" || sessionID == chat.session() {
		return fmt.Errorf("fork reply = %#v", m)
	}
	temp := &chatState{index: chat.index, chatID: forkChat, tabID: forkTab, sessionID: sessionID, workspace: chat.workspace}
	prompt := fmt.Sprintf("fork continuation cycle %d", cycle)
	end, err := r.runTurn(ctx, temp, prompt, expectDone, false)
	if err != nil {
		return err
	}
	result := stringField(end, "result")
	if !strings.Contains(result, chat.firstUser) || !strings.Contains(result, prompt) {
		return fmt.Errorf("fork result missing seed/prompt first=%q result=%q", chat.firstUser, result)
	}
	if _, err := r.callOK(ctx, ctl, "app-chat:close-session", temp.session()); err != nil {
		return err
	}
	r.stats.inc("forks", 1)
	return nil
}

func (r *runner) scenarioControllerHandoff(ctx context.Context, chat *chatState, cycle int) error {
	if _, err := r.callExpectErrorCode(ctx, r.peer, "app-chat:new-session", "lan:not-controller", map[string]any{"tabId": "intentional-not-controller"}); err != nil {
		return err
	}
	markPeer := r.peer.mark()
	if _, err := r.callOK(ctx, r.peer, "lan:take-control"); err != nil {
		return err
	}
	if _, err := r.peer.waitEventSince(ctx, markPeer, "lan:controller-changed", controllerReturnTimeout, func(payload map[string]any) bool {
		return stringField(payload, "deviceId") != ""
	}); err != nil {
		return err
	}
	r.currentCtl.Store(r.peer)

	peerMark := r.peer.mark()
	primaryMark := r.controller.mark()
	prompt := fmt.Sprintf("[mock:permission] peer controller cycle %d", cycle)
	job, err := r.startJob(ctx, r.peer, chat, prompt)
	if err != nil {
		return err
	}
	jobID := stringField(job, "id")
	req, err := r.peer.waitEventSince(ctx, peerMark, "chat:permission-request", 10*time.Second, func(payload map[string]any) bool {
		return stringField(payload, "jobId") == jobID
	})
	if err != nil {
		return err
	}
	// The lease decides who ANSWERS, never who watches. The device that lost it
	// still receives the card — that is what lets a phone show a 3am prompt
	// without stealing the desktop you are working at.
	if _, err := r.controller.waitEventSince(ctx, primaryMark, "chat:permission-request", 10*time.Second, func(payload map[string]any) bool {
		return stringField(payload, "jobId") == jobID
	}); err != nil {
		return err
	}
	if _, err := r.callOK(ctx, r.peer, "chat:permission-decide", map[string]any{"id": stringField(req, "id"), "optionId": "allow-once"}); err != nil {
		return err
	}
	end, err := r.waitJobEnd(ctx, r.peer, peerMark, jobID)
	if err != nil {
		return err
	}
	chat.setSessionIfNonEmpty(stringField(end, "sessionId"))
	if stringField(end, "status") != "done" {
		return fmt.Errorf("peer permission end = %#v", end)
	}
	if err := r.appendArchive(ctx, r.peer, chat, prompt, stringField(end, "result")); err != nil {
		return err
	}

	markPrimary := r.controller.mark()
	if _, err := r.callOK(ctx, r.controller, "lan:take-control"); err != nil {
		return err
	}
	if _, err := r.controller.waitEventSince(ctx, markPrimary, "lan:controller-changed", controllerReturnTimeout, func(payload map[string]any) bool {
		return stringField(payload, "deviceId") != ""
	}); err != nil {
		return err
	}
	r.currentCtl.Store(r.controller)
	r.stats.inc("controller.handoffs", 1)
	return nil
}

type turnExpectation int

const (
	expectDone turnExpectation = iota
)

func (r *runner) runTurn(ctx context.Context, chat *chatState, prompt string, expectation turnExpectation, archive bool) (map[string]any, error) {
	ctl := r.currentController()
	mark := ctl.mark()
	job, err := r.startJob(ctx, ctl, chat, prompt)
	if err != nil {
		return nil, err
	}
	jobID := stringField(job, "id")
	end, err := r.waitJobEnd(ctx, ctl, mark, jobID)
	if err != nil {
		return nil, err
	}
	chat.setSessionIfNonEmpty(stringField(end, "sessionId"))
	if expectation == expectDone && stringField(end, "status") != "done" {
		return nil, fmt.Errorf("job %s ended = %#v", jobID, end)
	}
	if archive {
		if err := r.appendArchive(ctx, ctl, chat, prompt, stringField(end, "result")); err != nil {
			return nil, err
		}
	}
	return end, nil
}

func (r *runner) startJob(ctx context.Context, client *wsClient, chat *chatState, prompt string) (map[string]any, error) {
	res, err := r.callOK(ctx, client, "job:start", map[string]any{
		"kind":      "app-chat",
		"chatId":    chat.chatID,
		"tabId":     chat.tabID,
		"sessionId": chat.session(),
		"cwd":       chat.workspace,
		"prompt":    prompt,
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
	r.stats.inc("jobs.started", 1)
	return job, nil
}

func (r *runner) waitJobEnd(ctx context.Context, client *wsClient, mark int, jobID string) (map[string]any, error) {
	payload, err := client.waitEventSince(ctx, mark, "job:event", jobTimeout, func(payload map[string]any) bool {
		if payload["type"] != "end" {
			return false
		}
		job, _ := payload["job"].(map[string]any)
		return stringField(job, "id") == jobID
	})
	if err != nil {
		return nil, err
	}
	job, err := asMap(payload["job"])
	if err != nil {
		return nil, err
	}
	r.stats.inc("jobs.ended", 1)
	return job, nil
}

func (r *runner) appendArchive(ctx context.Context, client *wsClient, chat *chatState, userPrompt, assistant string) error {
	if strings.TrimSpace(userPrompt) == "" && strings.TrimSpace(assistant) == "" {
		return nil
	}
	at := atomic.AddInt64(&chat.archiveAt, 2)
	messages := []any{
		map[string]any{"role": "user", "content": userPrompt, "status": "done", "at": r.archiveTime(at - 1)},
		map[string]any{"role": "assistant", "content": assistant, "status": "done", "at": r.archiveTime(at)},
	}
	res, err := r.callOK(ctx, client, "chat:archive-append", map[string]any{"tabId": chat.tabID, "messages": messages})
	if err != nil {
		return err
	}
	if res != true {
		return fmt.Errorf("chat:archive-append returned %#v", res)
	}
	return nil
}

func (r *runner) archiveTime(offset int64) string {
	return r.startedAt.Add(time.Duration(offset) * time.Millisecond).UTC().Format(time.RFC3339Nano)
}

func (r *runner) waitHibernated(ctx context.Context, chat *chatState, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := r.callOKWithTimeout(ctx, r.currentController(), "proc:list", 4*time.Second)
		if err == nil {
			m, _ := asMap(res)
			items, _ := m["processes"].([]any)
			for _, raw := range items {
				p, _ := raw.(map[string]any)
				if p == nil {
					continue
				}
				if stringField(p, "chatId") == chat.chatID && stringField(p, "state") == "hibernated" {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("chat %s did not hibernate within %s", chat.chatID, timeout)
}

func (r *runner) callOK(ctx context.Context, c *wsClient, channel string, args ...any) (any, error) {
	return r.callOKWithTimeout(ctx, c, channel, wsReplyTimeout, args...)
}

func (r *runner) callOKWithTimeout(ctx context.Context, c *wsClient, channel string, timeout time.Duration, args ...any) (any, error) {
	if c == nil {
		return nil, errors.New("nil websocket client")
	}
	reply, err := c.invoke(ctx, timeout, channel, args...)
	if err != nil {
		return nil, err
	}
	if reply.Error != nil {
		r.stats.addUnexpectedReplyError(channel, *reply.Error)
		return nil, fmt.Errorf("%s reply.error: %s", channel, *reply.Error)
	}
	return reply.Result, nil
}

func (r *runner) callExpectErrorCode(ctx context.Context, c *wsClient, channel, code string, args ...any) (any, error) {
	reply, err := c.invoke(ctx, wsReplyTimeout, channel, args...)
	if err != nil {
		return nil, err
	}
	if reply.Error == nil {
		return nil, fmt.Errorf("%s expected reply.error code %s, got result %#v", channel, code, reply.Result)
	}
	got := structuredErrorCode(*reply.Error)
	if got != code {
		r.stats.addUnexpectedReplyError(channel, *reply.Error)
		return nil, fmt.Errorf("%s expected reply.error code %s, got %s: %s", channel, code, got, *reply.Error)
	}
	r.stats.addIntentionalReplyError(code)
	return nil, nil
}

func (r *runner) currentController() *wsClient {
	if c := r.currentCtl.Load(); c != nil {
		return c
	}
	return r.controller
}

func (r *runner) finalAssertions() error {
	if r.stopSamplers != nil {
		r.stopSamplers()
	}
	r.sampleDaemonRSS()
	r.sampleProcList(context.Background())
	if err := r.failed(); err != nil {
		return err
	}
	var checks []string
	if r.stats.get("cycles.completed") <= 0 {
		checks = append(checks, "no cycles completed")
	}
	if r.stats.get("compactions") < r.stats.get("cycles.completed") {
		checks = append(checks, fmt.Sprintf("compactions=%d cycles=%d", r.stats.get("compactions"), r.stats.get("cycles.completed")))
	}
	if r.stats.get("cancels.started") != r.stats.get("cancels.succeeded") {
		checks = append(checks, fmt.Sprintf("cancels started=%d succeeded=%d", r.stats.get("cancels.started"), r.stats.get("cancels.succeeded")))
	}
	if r.stats.get("crash.recoveries") <= 0 {
		checks = append(checks, "no crash recoveries counted")
	}
	if r.stats.get("replay.once.assertions") <= 0 {
		checks = append(checks, "no replay-once assertions counted")
	}
	if r.stats.getIntentional("lan:not-controller") <= 0 {
		checks = append(checks, "intentional lan:not-controller rejection was not observed")
	}
	if errors := r.stats.unexpectedReplyErrors(); len(errors) > 0 {
		checks = append(checks, fmt.Sprintf("unexpected reply.errors=%d first=%s", len(errors), errors[0]))
	}
	if err := r.assertTraceSeeds(); err != nil {
		checks = append(checks, err.Error())
	}
	if err := r.assertRSS(); err != nil {
		checks = append(checks, err.Error())
	}
	if len(checks) > 0 {
		return errors.New(strings.Join(checks, "; "))
	}
	return nil
}

func (r *runner) assertTraceSeeds() error {
	path := filepath.Join(r.cfg.tempDir, "mock-prompts.jsonl")
	records, err := readTracePrompts(path)
	if err != nil {
		return err
	}
	seedCounts := map[string]int{}
	for _, rec := range records {
		if strings.Contains(rec.Text, "Previous conversation") {
			seedCounts[rec.SessionID]++
		}
	}
	for sessionID, count := range seedCounts {
		if count > 1 {
			return fmt.Errorf("replay seed appeared %d times for session %s", count, sessionID)
		}
	}
	r.stats.set("trace.prompts", int64(len(records)))
	r.stats.set("trace.replay.seeded.sessions", int64(len(seedCounts)))
	if len(seedCounts) == 0 {
		return errors.New("trace had no replay seed prompts")
	}
	return nil
}

func (r *runner) assertRSS() error {
	samples := r.stats.daemonRSSSamples()
	if len(samples) < 2 {
		return errors.New("not enough daemon RSS samples")
	}
	baselineAfter := r.startedAt.Add(5 * time.Minute)
	if r.cfg.duration < 5*time.Minute {
		baselineAfter = r.startedAt.Add(minDuration(time.Minute, r.cfg.duration/2))
	}
	var baseline rssSample
	for _, sample := range samples {
		if !sample.At.Before(baselineAfter) {
			baseline = sample
			break
		}
	}
	if baseline.RSSKB == 0 {
		baseline = samples[0]
	}
	end := samples[len(samples)-1]
	r.stats.setRSSBaseline(baseline, end)
	limit := int64(math.Ceil(float64(baseline.RSSKB) * 1.25))
	if end.RSSKB > limit {
		return fmt.Errorf("daemon RSS grew beyond 25%% baseline: baseline=%dKiB end=%dKiB limit=%dKiB", baseline.RSSKB, end.RSSKB, limit)
	}
	return nil
}

func (r *runner) fail(err error) {
	if err == nil {
		return
	}
	r.failOnce.Do(func() {
		r.failErr.Store(err)
		if r.cancelRun != nil {
			r.cancelRun()
		}
	})
}

func (r *runner) failed() error {
	if v := r.failErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

func (r *runner) cleanup() {
	if r.stopSamplers != nil {
		r.stopSamplers()
	}
	for _, c := range r.clients {
		c.close()
	}
	if r.daemon != nil && r.daemon.Process != nil {
		_ = r.daemon.Process.Kill()
		done := make(chan struct{})
		go func() {
			r.daemonWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}

func (r *runner) printReport(w io.Writer, passed bool) {
	r.stats.printReport(w, passed, r.startedAt, r.cfg.duration, r.cfg.tempDir, r.logFile)
	if !passed && r.logFile != "" {
		fmt.Fprintf(w, "\nDaemon log tail:\n%s\n", tailFile(r.logFile, 120))
	}
}

func (c *chatState) session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *chatState) setSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = strings.TrimSpace(sessionID)
}

func (c *chatState) setSessionIfNonEmpty(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	c.setSession(sessionID)
}

func (c *chatState) updateSessionFromReplacement(ctx context.Context, client *wsClient, mark int, oldSession string, timeout time.Duration) {
	payload, err := client.waitEventSince(ctx, mark, "chat:session-replaced", timeout, func(payload map[string]any) bool {
		return stringField(payload, "oldSessionId") == oldSession || stringField(payload, "tabId") == c.tabID
	})
	if err != nil {
		return
	}
	session, _ := payload["session"].(map[string]any)
	if sessionID := stringField(session, "sessionId"); sessionID != "" {
		c.setSession(sessionID)
	}
}

type stats struct {
	mu                sync.Mutex
	counters          map[string]int64
	scenarioCounts    map[string]int64
	scenarioDurations map[string]time.Duration
	eventCounts       map[string]int64
	clientEvents      map[string]int64
	clientReplies     map[string]int64
	intentionalErrors map[string]int64
	unexpectedErrors  []string
	notes             []string
	daemonRSS         []rssSample
	procSamples       []procSample
	rssBaseline       rssSample
	rssEnd            rssSample
}

type rssSample struct {
	At    time.Time
	RSSKB int64
}

type procSample struct {
	At          time.Time
	Total       int
	Engines     int
	Active      int
	Idle        int
	Warm        int
	Hibernated  int
	EngineRSSKB int64
}

func newStats() *stats {
	return &stats{
		counters:          map[string]int64{},
		scenarioCounts:    map[string]int64{},
		scenarioDurations: map[string]time.Duration{},
		eventCounts:       map[string]int64{},
		clientEvents:      map[string]int64{},
		clientReplies:     map[string]int64{},
		intentionalErrors: map[string]int64{},
	}
}

func (s *stats) inc(key string, delta int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters[key] += delta
}

func (s *stats) set(key string, value int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters[key] = value
}

func (s *stats) get(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counters[key]
}

func (s *stats) addScenario(name string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenarioCounts[name]++
	s.scenarioDurations[name] += d
}

func (s *stats) recordEvent(client, channel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventCounts[channel]++
	s.clientEvents[client]++
}

func (s *stats) recordReply(client string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters["replies.total"]++
	s.clientReplies[client]++
}

func (s *stats) addUnexpectedReplyError(channel, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unexpectedErrors = append(s.unexpectedErrors, channel+": "+errText)
}

func (s *stats) unexpectedReplyErrors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.unexpectedErrors...)
}

func (s *stats) addIntentionalReplyError(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intentionalErrors[code]++
}

func (s *stats) getIntentional(code string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intentionalErrors[code]
}

func (s *stats) addNote(note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.notes) < 20 {
		s.notes = append(s.notes, note)
	}
}

func (s *stats) addDaemonRSS(sample rssSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.daemonRSS = append(s.daemonRSS, sample)
}

func (s *stats) daemonRSSSamples() []rssSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]rssSample(nil), s.daemonRSS...)
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func (s *stats) setRSSBaseline(baseline, end rssSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rssBaseline = baseline
	s.rssEnd = end
}

func (s *stats) addProcSample(sample procSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.procSamples = append(s.procSamples, sample)
}

func (s *stats) printReport(w io.Writer, passed bool, started time.Time, duration time.Duration, tempDir, logFile string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	verdict := "FAIL"
	if passed {
		verdict = "PASS"
	}
	fmt.Fprintf(w, "\nSOAK REPORT verdict=%s\n", verdict)
	fmt.Fprintf(w, "duration_config=%s elapsed=%s started=%s\n", duration.Round(time.Second), time.Since(started).Round(time.Second), started.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "temp_dir=%s daemon_log=%s\n", tempDir, logFile)
	fmt.Fprintf(w, "cycles=%d jobs_started=%d jobs_ended=%d compactions=%d crash_recoveries=%d hibernations=%d forks=%d rewinds=%d\n",
		s.counters["cycles.completed"],
		s.counters["jobs.started"],
		s.counters["jobs.ended"],
		s.counters["compactions"],
		s.counters["crash.recoveries"],
		s.counters["hibernations"],
		s.counters["forks"],
		s.counters["checkpoints.rewinds"],
	)
	fmt.Fprintf(w, "cancels=%d/%d permissions_allow=%d permissions_reject=%d replay_assertions=%d\n",
		s.counters["cancels.succeeded"],
		s.counters["cancels.started"],
		s.counters["permissions.allow-once"],
		s.counters["permissions.reject"],
		s.counters["replay.once.assertions"],
	)
	fmt.Fprintf(w, "queue_enqueued=%d queue_flushed=%d controller_handoffs=%d\n",
		s.counters["queue.local.enqueued"],
		s.counters["queue.local.flushed"],
		s.counters["controller.handoffs"],
	)
	fmt.Fprintf(w, "reply_total=%d unexpected_reply_errors=%d intentional_errors=%s\n", s.counters["replies.total"], len(s.unexpectedErrors), formatCounterMap(s.intentionalErrors))
	fmt.Fprintf(w, "events=%s\n", formatCounterMap(s.eventCounts))
	fmt.Fprintf(w, "client_replies=%s\n", formatCounterMap(s.clientReplies))
	fmt.Fprintf(w, "client_events=%s\n", formatCounterMap(s.clientEvents))
	fmt.Fprintf(w, "scenarios=%s\n", formatCounterMap(s.scenarioCounts))
	if len(s.daemonRSS) > 0 {
		minRSS, maxRSS := s.daemonRSS[0].RSSKB, s.daemonRSS[0].RSSKB
		for _, sample := range s.daemonRSS {
			if sample.RSSKB < minRSS {
				minRSS = sample.RSSKB
			}
			if sample.RSSKB > maxRSS {
				maxRSS = sample.RSSKB
			}
		}
		fmt.Fprintf(w, "daemon_rss_samples=%d min=%dKiB max=%dKiB baseline=%dKiB@%s end=%dKiB@%s\n",
			len(s.daemonRSS),
			minRSS,
			maxRSS,
			s.rssBaseline.RSSKB,
			relTime(started, s.rssBaseline.At),
			s.rssEnd.RSSKB,
			relTime(started, s.rssEnd.At),
		)
	}
	if len(s.procSamples) > 0 {
		last := s.procSamples[len(s.procSamples)-1]
		maxEngines, maxHibernated, maxEngineRSS := 0, 0, int64(0)
		for _, sample := range s.procSamples {
			if sample.Engines > maxEngines {
				maxEngines = sample.Engines
			}
			if sample.Hibernated > maxHibernated {
				maxHibernated = sample.Hibernated
			}
			if sample.EngineRSSKB > maxEngineRSS {
				maxEngineRSS = sample.EngineRSSKB
			}
		}
		fmt.Fprintf(w, "proc_samples=%d last_engines=%d active=%d idle=%d warm=%d hibernated=%d max_engines=%d max_hibernated=%d max_engine_rss=%dKiB\n",
			len(s.procSamples), last.Engines, last.Active, last.Idle, last.Warm, last.Hibernated, maxEngines, maxHibernated, maxEngineRSS)
	}
	if s.counters["trace.prompts"] > 0 {
		fmt.Fprintf(w, "trace_prompts=%d trace_replay_seeded_sessions=%d\n", s.counters["trace.prompts"], s.counters["trace.replay.seeded.sessions"])
	}
	if len(s.unexpectedErrors) > 0 {
		fmt.Fprintln(w, "unexpected_reply_errors:")
		for _, item := range s.unexpectedErrors {
			fmt.Fprintf(w, "  %s\n", item)
		}
	}
	if len(s.notes) > 0 {
		fmt.Fprintln(w, "notes:")
		for _, item := range s.notes {
			fmt.Fprintf(w, "  %s\n", item)
		}
	}
}

type wsClient struct {
	name      string
	conn      net.Conn
	reader    *bufio.Reader
	writeMu   sync.Mutex
	seq       atomic.Int64
	stats     *stats
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

func dialWS(ctx context.Context, name string, port int, deviceName, token string, st *stats) (*wsClient, error) {
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
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nUser-Agent: workass-soak\r\n\r\n", path, addr, key)
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
	c := &wsClient{name: name, conn: conn, reader: reader, stats: st, pending: map[string]chan wsMessage{}, closed: make(chan struct{})}
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
			c.stats.recordReply(c.name)
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
			c.stats.recordEvent(c.name, msg.Channel)
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

func (c *wsClient) expectNoEventSince(ctx context.Context, mark int, channel string, timeout time.Duration, pred func(map[string]any) bool) error {
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
				return fmt.Errorf("%s unexpectedly received event %s payload=%#v", c.name, channel, payload)
			}
		}
		c.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		select {
		case <-time.After(minDuration(remaining, 50*time.Millisecond)):
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return nil
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

func initGitRepo(dir string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := runCmd("", "git", "init", dir); err != nil {
		return err
	}
	if err := runCmd(dir, "git", "-C", dir, "checkout", "-b", "main"); err != nil {
		return err
	}
	if err := runCmd(dir, "git", "-C", dir, "config", "user.email", "workass-soak@example.com"); err != nil {
		return err
	}
	if err := runCmd(dir, "git", "-C", dir, "config", "user.name", "Workass Soak"); err != nil {
		return err
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	if err := runCmd(dir, "git", "-C", dir, "add", "."); err != nil {
		return err
	}
	return runCmd(dir, "git", "-C", dir, "commit", "-m", "init")
}

func runCmd(dir, command string, args ...string) error {
	cmd := exec.Command(command, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", command, strings.Join(args, " "), err, out)
	}
	return nil
}

func reservePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func processRSSKB(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("ps rss: %w: %s", err, out)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return 0, errors.New("empty ps rss")
	}
	fields := strings.Fields(raw)
	v, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:]), nil
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

func structuredErrorCode(errText string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(errText), &m); err != nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(m["code"]))
}

func envHasRepoFile(payload map[string]any, repoName, relPath string) bool {
	repos, _ := payload["repos"].([]any)
	for _, raw := range repos {
		repo, _ := raw.(map[string]any)
		if stringField(repo, "name") != repoName {
			continue
		}
		files, _ := repo["files"].([]any)
		for _, fileRaw := range files {
			file, _ := fileRaw.(map[string]any)
			if stringField(file, "path") == relPath {
				return true
			}
		}
	}
	return false
}

func latestCheckpoint(raw any, repoName string) (map[string]any, int, error) {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil, 0, fmt.Errorf("no checkpoints: %#v", raw)
	}
	for i := len(items) - 1; i >= 0; i-- {
		cp, _ := items[i].(map[string]any)
		if cp == nil {
			continue
		}
		repos, _ := cp["repos"].([]any)
		for _, rawRepo := range repos {
			repo, _ := rawRepo.(map[string]any)
			if stringField(repo, "name") == repoName && repo["skipped"] != true && stringField(repo, "ref") != "" {
				return cp, intField(cp, "turnSeq"), nil
			}
		}
	}
	return nil, 0, fmt.Errorf("checkpoint for repo %s not found in %#v", repoName, raw)
}

type tracePrompt struct {
	SessionID string
	Text      string
}

func readTracePrompts(path string) ([]tracePrompt, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []tracePrompt
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var item map[string]any
		dec := json.NewDecoder(strings.NewReader(scanner.Text()))
		dec.UseNumber()
		if err := dec.Decode(&item); err != nil {
			return nil, err
		}
		out = append(out, tracePrompt{SessionID: fmt.Sprint(item["sessionId"]), Text: fmt.Sprint(item["text"])})
	}
	return out, scanner.Err()
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

func formatCounterMap(m map[string]int64) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func relTime(start, at time.Time) string {
	if at.IsZero() {
		return "n/a"
	}
	return at.Sub(start).Round(time.Second).String()
}
