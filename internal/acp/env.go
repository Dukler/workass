package acp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	chatEnvRepoLimit     = 12
	chatEnvFileLimit     = 200
	chatEnvGitTimeout    = 5 * time.Second
	chatEnvApproximation = "changes are computed from this chat's git status/numstat baseline; edits to already-dirty files can only be approximated"
)

// ChatEnvPayload is the additive data source for the Entorno rail. It reports
// current repo diffs against the chat's session-start baseline, not an
// authoritative authorship audit.
type ChatEnvPayload struct {
	ChatID         string        `json:"chatId"`
	TabID          string        `json:"tabId"`
	CWD            string        `json:"cwd"`
	Repos          []ChatEnvRepo `json:"repos"`
	Unchanged      []string      `json:"unchanged"`
	ReposTruncated bool          `json:"reposTruncated"`
	FilesTruncated bool          `json:"filesTruncated"`
	RepoLimit      int           `json:"repoLimit"`
	FileLimit      int           `json:"fileLimit"`
	Approximation  string        `json:"approximation"`
}

type ChatEnvRepo struct {
	Name           string        `json:"name"`
	Branch         string        `json:"branch"`
	Files          []ChatEnvFile `json:"files"`
	Adds           int           `json:"adds"`
	Dels           int           `json:"dels"`
	FilesTruncated bool          `json:"filesTruncated"`
}

type ChatEnvFile struct {
	Path string `json:"path"`
	Adds int    `json:"adds"`
	Dels int    `json:"dels"`
}

type chatEnvTracker struct {
	sessionID      string
	chatID         string
	tabID          string
	cwd            string
	repos          []gitRepoBaseline
	reposTruncated bool
	payload        ChatEnvPayload
	turnSeq        int
	pendingTurns   map[string]chatTurnCheckpoint
}

type chatEnvSnapshot struct {
	sessionID      string
	chatID         string
	tabID          string
	cwd            string
	repos          []gitRepoBaseline
	reposTruncated bool
	turnSeq        int
	jobID          string
	preRefs        []pendingCheckpointRepo
}

type gitRepoBaseline struct {
	name       string
	path       string
	branch     string
	tree       string
	status     []gitStatusEntry
	statusKeys map[string]struct{}
	numstat    map[string]gitNumstat
}

type chatTurnCheckpoint struct {
	turnSeq int
	jobID   string
	repos   []gitRepoBaseline
	preRefs []pendingCheckpointRepo
}

type pendingCheckpointRepo struct {
	name   string
	path   string
	branch string
	ref    string
	commit string
	err    string
}

type gitStatusEntry struct {
	code string
	path string
	orig string
}

func (e gitStatusEntry) key() string {
	return e.code + "\x00" + e.path + "\x00" + e.orig
}

type gitNumstat struct {
	path string
	adds int
	dels int
}

func (m *Manager) initChatEnvForSession(ctx context.Context, opts SessionOptions, info SessionInfo) {
	if opts.Spare || info.SessionID == "" {
		return
	}
	cwd := strings.TrimSpace(info.CWD)
	if cwd == "" {
		cwd = strings.TrimSpace(opts.CWD)
	}
	if cwd == "" {
		cwd = m.opts.RootDir
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if reused := m.reuseChatEnvTrackerForSession(opts, info, cwd); reused != nil {
		m.emit("chat:env", reused)
		return
	}
	repos, reposTruncated := discoverChatEnvRepos(ctx, cwd)
	tracker := &chatEnvTracker{
		sessionID:      info.SessionID,
		chatID:         strings.TrimSpace(opts.ChatID),
		tabID:          strings.TrimSpace(opts.TabID),
		cwd:            cwd,
		repos:          repos,
		reposTruncated: reposTruncated,
	}
	tracker.payload = tracker.initialPayload()

	m.envMu.Lock()
	m.storeChatEnvTrackerLocked(tracker)
	m.envMu.Unlock()
	m.emit("chat:env", cloneChatEnvPayload(tracker.payload))
}

func (m *Manager) reuseChatEnvTrackerForSession(opts SessionOptions, info SessionInfo, cwd string) map[string]any {
	chatID := strings.TrimSpace(opts.ChatID)
	tabID := strings.TrimSpace(opts.TabID)
	if info.SessionID == "" || (chatID == "" && tabID == "") {
		return nil
	}
	m.envMu.Lock()
	defer m.envMu.Unlock()
	tracker := m.chatEnvTrackerLocked("", chatID, tabID)
	if tracker == nil || tracker.sessionID == "" || tracker.sessionID == info.SessionID {
		return nil
	}
	delete(m.envBySession, tracker.sessionID)
	tracker.sessionID = info.SessionID
	if chatID != "" {
		tracker.chatID = chatID
	}
	if tabID != "" {
		tracker.tabID = tabID
	}
	if strings.TrimSpace(cwd) != "" {
		tracker.cwd = cwd
	}
	tracker.payload.ChatID = tracker.chatID
	tracker.payload.TabID = tracker.tabID
	tracker.payload.CWD = tracker.cwd
	m.storeChatEnvTrackerLocked(tracker)
	payload := cloneChatEnvPayload(tracker.payload)
	return map[string]any{
		"chatId":         payload.ChatID,
		"tabId":          payload.TabID,
		"cwd":            payload.CWD,
		"repos":          payload.Repos,
		"unchanged":      payload.Unchanged,
		"reposTruncated": payload.ReposTruncated,
		"filesTruncated": payload.FilesTruncated,
		"repoLimit":      payload.RepoLimit,
		"fileLimit":      payload.FileLimit,
		"approximation":  payload.Approximation,
	}
}

func (m *Manager) bindChatEnvToJob(sessionID, chatID, tabID, cwd string) {
	m.envMu.Lock()
	defer m.envMu.Unlock()
	tracker := m.chatEnvTrackerLocked(sessionID, chatID, tabID)
	if tracker == nil {
		return
	}
	if chatID = strings.TrimSpace(chatID); chatID != "" {
		tracker.chatID = chatID
		m.envByChatID[chatID] = tracker
	}
	if tabID = strings.TrimSpace(tabID); tabID != "" {
		tracker.tabID = tabID
		m.envByTabID[tabID] = tracker
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		tracker.cwd = cwd
	}
	tracker.payload.ChatID = tracker.chatID
	tracker.payload.TabID = tracker.tabID
	tracker.payload.CWD = tracker.cwd
}

func (m *Manager) refreshChatEnvAfterJob(ctx context.Context, job *Job) {
	if job == nil {
		return
	}
	snapshot, ok := m.chatEnvSnapshot(job.SessionID, job.ChatID, job.TabID, job.ID)
	if !ok {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, currentRepos := buildChatEnvPayloadAndRepos(ctx, snapshot)

	m.envMu.Lock()
	tracker := m.chatEnvTrackerLocked(snapshot.sessionID, snapshot.chatID, snapshot.tabID)
	if tracker != nil {
		tracker.chatID = snapshot.chatID
		tracker.tabID = snapshot.tabID
		tracker.cwd = snapshot.cwd
		tracker.payload = payload
		if currentRepos != nil {
			tracker.repos = currentRepos
		}
		if job.ID != "" && tracker.pendingTurns != nil {
			delete(tracker.pendingTurns, job.ID)
		}
		m.storeChatEnvTrackerLocked(tracker)
	}
	m.envMu.Unlock()

	m.recordCheckpointAfterTurn(ctx, snapshot, payload, currentRepos)
	m.emit("chat:env", payload)
}

func (m *Manager) ChatEnvGet(chatID, tabID string) ChatEnvPayload {
	chatID = strings.TrimSpace(chatID)
	tabID = strings.TrimSpace(tabID)
	m.envMu.Lock()
	defer m.envMu.Unlock()
	if tracker := m.chatEnvTrackerLocked("", chatID, tabID); tracker != nil {
		return cloneChatEnvPayload(tracker.payload)
	}
	return emptyChatEnvPayload(chatID, tabID, "")
}

func (m *Manager) forgetChatEnvSession(sessionID string) {
	if sessionID == "" {
		return
	}
	m.envMu.Lock()
	defer m.envMu.Unlock()
	tracker := m.envBySession[sessionID]
	if tracker == nil {
		return
	}
	delete(m.envBySession, sessionID)
	if tracker.chatID != "" && m.envByChatID[tracker.chatID] == tracker {
		delete(m.envByChatID, tracker.chatID)
	}
	if tracker.tabID != "" && m.envByTabID[tracker.tabID] == tracker {
		delete(m.envByTabID, tracker.tabID)
	}
}

func (m *Manager) resetChatEnv() {
	m.envMu.Lock()
	defer m.envMu.Unlock()
	m.envBySession = make(map[string]*chatEnvTracker)
	m.envByChatID = make(map[string]*chatEnvTracker)
	m.envByTabID = make(map[string]*chatEnvTracker)
}

func (m *Manager) beginChatTurnCheckpoint(ctx context.Context, job *Job) {
	if job == nil || strings.TrimSpace(job.ID) == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.envMu.Lock()
	tracker := m.chatEnvTrackerLocked(job.SessionID, job.ChatID, job.TabID)
	if tracker == nil {
		m.envMu.Unlock()
		return
	}
	if job.ChatID != "" {
		tracker.chatID = job.ChatID
	}
	if job.TabID != "" {
		tracker.tabID = job.TabID
	}
	if job.CWD != "" {
		tracker.cwd = job.CWD
	}
	chatID := tracker.chatID
	tabID := tracker.tabID
	cwd := tracker.cwd
	repos := make([]gitRepoBaseline, len(tracker.repos))
	copy(repos, tracker.repos)
	reposTruncated := tracker.reposTruncated
	m.envMu.Unlock()

	if strings.TrimSpace(chatID) == "" {
		return
	}
	maxSeq := m.maxCheckpointTurnSeq(chatID)

	m.envMu.Lock()
	tracker = m.chatEnvTrackerLocked(job.SessionID, chatID, tabID)
	if tracker == nil {
		m.envMu.Unlock()
		return
	}
	if tracker.turnSeq < maxSeq {
		tracker.turnSeq = maxSeq
	}
	tracker.turnSeq++
	turnSeq := tracker.turnSeq
	m.envMu.Unlock()

	currentRepos := make([]gitRepoBaseline, 0, len(repos))
	pendingRepos := make([]pendingCheckpointRepo, 0, len(repos))
	for _, repo := range repos {
		current := captureRepoBaseline(ctx, repo.path)
		currentRepos = append(currentRepos, current)
		ref := checkpointRef(chatID, turnSeq)
		commit, err := createWorktreeCheckpointCommit(ctx, repo.path, checkpointCommitMessage(chatID, turnSeq, job.ID))
		pending := pendingCheckpointRepo{name: current.name, path: current.path, branch: current.branch, ref: ref, commit: commit}
		if err != nil {
			pending.err = err.Error()
		}
		pendingRepos = append(pendingRepos, pending)
	}

	m.envMu.Lock()
	tracker = m.chatEnvTrackerLocked(job.SessionID, chatID, tabID)
	if tracker != nil {
		tracker.chatID = chatID
		tracker.tabID = tabID
		tracker.cwd = cwd
		tracker.repos = currentRepos
		tracker.reposTruncated = reposTruncated
		if tracker.pendingTurns == nil {
			tracker.pendingTurns = make(map[string]chatTurnCheckpoint)
		}
		tracker.pendingTurns[job.ID] = chatTurnCheckpoint{
			turnSeq: turnSeq,
			jobID:   job.ID,
			repos:   currentRepos,
			preRefs: pendingRepos,
		}
		m.storeChatEnvTrackerLocked(tracker)
	}
	m.envMu.Unlock()
}

func (m *Manager) chatEnvSnapshot(sessionID, chatID, tabID, jobID string) (chatEnvSnapshot, bool) {
	m.envMu.Lock()
	defer m.envMu.Unlock()
	tracker := m.chatEnvTrackerLocked(sessionID, chatID, tabID)
	if tracker == nil {
		return chatEnvSnapshot{}, false
	}
	if chatID = strings.TrimSpace(chatID); chatID != "" {
		tracker.chatID = chatID
		m.envByChatID[chatID] = tracker
	}
	if tabID = strings.TrimSpace(tabID); tabID != "" {
		tracker.tabID = tabID
		m.envByTabID[tabID] = tracker
	}
	repos := tracker.repos
	turnSeq := 0
	snapshotJobID := ""
	var preRefs []pendingCheckpointRepo
	if tracker.pendingTurns != nil {
		if turn, ok := tracker.pendingTurns[strings.TrimSpace(jobID)]; ok {
			repos = turn.repos
			turnSeq = turn.turnSeq
			snapshotJobID = turn.jobID
			preRefs = append([]pendingCheckpointRepo(nil), turn.preRefs...)
		}
	}
	reposCopy := make([]gitRepoBaseline, len(repos))
	copy(reposCopy, repos)
	return chatEnvSnapshot{
		sessionID:      tracker.sessionID,
		chatID:         tracker.chatID,
		tabID:          tracker.tabID,
		cwd:            tracker.cwd,
		repos:          reposCopy,
		reposTruncated: tracker.reposTruncated,
		turnSeq:        turnSeq,
		jobID:          snapshotJobID,
		preRefs:        preRefs,
	}, true
}

func (m *Manager) chatEnvTrackerLocked(sessionID, chatID, tabID string) *chatEnvTracker {
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		if tracker := m.envBySession[sessionID]; tracker != nil {
			return tracker
		}
	}
	if chatID = strings.TrimSpace(chatID); chatID != "" {
		if tracker := m.envByChatID[chatID]; tracker != nil {
			return tracker
		}
	}
	if tabID = strings.TrimSpace(tabID); tabID != "" {
		if tracker := m.envByTabID[tabID]; tracker != nil {
			return tracker
		}
	}
	return nil
}

func (m *Manager) storeChatEnvTrackerLocked(tracker *chatEnvTracker) {
	if tracker == nil {
		return
	}
	if tracker.sessionID != "" {
		m.envBySession[tracker.sessionID] = tracker
	}
	if tracker.chatID != "" {
		m.envByChatID[tracker.chatID] = tracker
	}
	if tracker.tabID != "" {
		m.envByTabID[tracker.tabID] = tracker
	}
}

func (t *chatEnvTracker) initialPayload() ChatEnvPayload {
	payload := emptyChatEnvPayload(t.chatID, t.tabID, t.cwd)
	payload.ReposTruncated = t.reposTruncated
	payload.Unchanged = make([]string, 0, len(t.repos))
	for _, repo := range t.repos {
		payload.Unchanged = append(payload.Unchanged, repo.name)
	}
	return payload
}

func buildChatEnvPayload(ctx context.Context, snapshot chatEnvSnapshot) ChatEnvPayload {
	payload, _ := buildChatEnvPayloadAndRepos(ctx, snapshot)
	return payload
}

func buildChatEnvPayloadAndRepos(ctx context.Context, snapshot chatEnvSnapshot) (ChatEnvPayload, []gitRepoBaseline) {
	payload := emptyChatEnvPayload(snapshot.chatID, snapshot.tabID, snapshot.cwd)
	payload.ReposTruncated = snapshot.reposTruncated
	currentRepos := make([]gitRepoBaseline, 0, len(snapshot.repos))
	remainingFiles := chatEnvFileLimit
	for _, baseline := range snapshot.repos {
		current := captureRepoCurrent(ctx, baseline)
		currentRepos = append(currentRepos, current)
		files := diffRepoFiles(baseline, current)
		changed := current.branch != baseline.branch || !statusEntrySetsEqual(baseline.statusKeys, statusKeySet(current.status)) || len(files) > 0
		if !changed {
			payload.Unchanged = append(payload.Unchanged, baseline.name)
			continue
		}
		repo := ChatEnvRepo{Name: baseline.name, Branch: firstNonEmpty(current.branch, baseline.branch), Files: []ChatEnvFile{}}
		for _, file := range files {
			repo.Adds += file.Adds
			repo.Dels += file.Dels
		}
		if remainingFiles <= 0 && len(files) > 0 {
			repo.FilesTruncated = true
			payload.FilesTruncated = true
		} else if len(files) > remainingFiles {
			repo.Files = append(repo.Files, files[:remainingFiles]...)
			repo.FilesTruncated = true
			payload.FilesTruncated = true
			remainingFiles = 0
		} else {
			repo.Files = append(repo.Files, files...)
			remainingFiles -= len(files)
		}
		payload.Repos = append(payload.Repos, repo)
	}
	return payload, currentRepos
}

func emptyChatEnvPayload(chatID, tabID, cwd string) ChatEnvPayload {
	return ChatEnvPayload{
		ChatID:        strings.TrimSpace(chatID),
		TabID:         strings.TrimSpace(tabID),
		CWD:           strings.TrimSpace(cwd),
		Repos:         []ChatEnvRepo{},
		Unchanged:     []string{},
		RepoLimit:     chatEnvRepoLimit,
		FileLimit:     chatEnvFileLimit,
		Approximation: chatEnvApproximation,
	}
}

func cloneChatEnvPayload(payload ChatEnvPayload) ChatEnvPayload {
	repos := make([]ChatEnvRepo, len(payload.Repos))
	copy(repos, payload.Repos)
	payload.Repos = repos
	for i := range payload.Repos {
		files := make([]ChatEnvFile, len(payload.Repos[i].Files))
		copy(files, payload.Repos[i].Files)
		payload.Repos[i].Files = files
	}
	unchanged := make([]string, len(payload.Unchanged))
	copy(unchanged, payload.Unchanged)
	payload.Unchanged = unchanged
	return payload
}

func discoverChatEnvRepos(ctx context.Context, cwd string) ([]gitRepoBaseline, bool) {
	absCWD, err := filepath.Abs(strings.TrimSpace(cwd))
	if err != nil {
		return nil, false
	}
	var repos []gitRepoBaseline
	truncated := false
	addRepo := func(path string) {
		if len(repos) >= chatEnvRepoLimit {
			truncated = true
			return
		}
		repos = append(repos, captureRepoBaseline(ctx, path))
	}
	if isGitRepoRoot(ctx, absCWD) {
		addRepo(absCWD)
	}
	entries, err := os.ReadDir(absCWD)
	if err != nil {
		return repos, truncated
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(absCWD, entry.Name())
		if isGitRepoRoot(ctx, path) {
			addRepo(path)
		}
	}
	return repos, truncated
}

func captureRepoBaseline(ctx context.Context, path string) gitRepoBaseline {
	status := gitStatus(ctx, path)
	return gitRepoBaseline{
		name:       filepath.Base(path),
		path:       path,
		branch:     gitCurrentBranch(ctx, path),
		tree:       gitWorktreeTree(ctx, path),
		status:     status,
		statusKeys: statusKeySet(status),
		numstat:    gitDiffNumstat(ctx, path),
	}
}

func captureRepoCurrent(ctx context.Context, baseline gitRepoBaseline) gitRepoBaseline {
	status := gitStatus(ctx, baseline.path)
	numstat := gitDiffNumstat(ctx, baseline.path)
	addUntrackedNumstat(ctx, baseline.path, status, baseline.statusKeys, numstat)
	return gitRepoBaseline{
		name:       baseline.name,
		path:       baseline.path,
		branch:     gitCurrentBranch(ctx, baseline.path),
		tree:       gitWorktreeTree(ctx, baseline.path),
		status:     status,
		statusKeys: statusKeySet(status),
		numstat:    numstat,
	}
}

func diffRepoFiles(baseline, current gitRepoBaseline) []ChatEnvFile {
	paths := make(map[string]struct{})
	for path := range current.numstat {
		paths[path] = struct{}{}
	}
	for path := range baseline.numstat {
		if _, ok := current.numstat[path]; ok {
			paths[path] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	files := make([]ChatEnvFile, 0, len(ordered))
	for _, path := range ordered {
		cur := current.numstat[path]
		base := baseline.numstat[path]
		adds := cur.adds - base.adds
		dels := cur.dels - base.dels
		if adds < 0 {
			adds = 0
		}
		if dels < 0 {
			dels = 0
		}
		if adds == 0 && dels == 0 {
			continue
		}
		files = append(files, ChatEnvFile{Path: path, Adds: adds, Dels: dels})
	}
	return files
}

func isGitRepoRoot(ctx context.Context, path string) bool {
	out, err := gitOutput(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	root := strings.TrimSpace(string(out))
	return sameFilesystemPath(root, path)
}

func gitCurrentBranch(ctx context.Context, repo string) string {
	out, err := gitOutput(ctx, repo, "branch", "--show-current")
	branch := strings.TrimSpace(string(out))
	if err == nil && branch != "" {
		return branch
	}
	out, err = gitOutput(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitStatus(ctx context.Context, repo string) []gitStatusEntry {
	out, err := gitOutput(ctx, repo, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil
	}
	return parseGitStatusZ(out)
}

func gitDiffNumstat(ctx context.Context, repo string) map[string]gitNumstat {
	out, err := gitOutput(ctx, repo, "diff", "--numstat", "HEAD", "--")
	if err != nil {
		return map[string]gitNumstat{}
	}
	return parseGitNumstat(out)
}

func addUntrackedNumstat(ctx context.Context, repo string, status []gitStatusEntry, baselineKeys map[string]struct{}, numstat map[string]gitNumstat) {
	for _, entry := range status {
		if entry.code != "??" {
			continue
		}
		if _, existed := baselineKeys[entry.key()]; existed {
			continue
		}
		if len(numstat) >= chatEnvFileLimit {
			return
		}
		if stat, ok := gitNoIndexNumstat(ctx, repo, entry.path); ok {
			numstat[entry.path] = stat
		}
	}
}

func gitNoIndexNumstat(ctx context.Context, repo, relPath string) (gitNumstat, bool) {
	cleanRel := filepath.Clean(filepath.FromSlash(relPath))
	if cleanRel == "." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanRel) {
		return gitNumstat{}, false
	}
	absPath := filepath.Join(repo, cleanRel)
	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() {
		return gitNumstat{}, false
	}
	out, err := gitOutputAllowExit(ctx, repo, map[int]struct{}{0: {}, 1: {}}, "diff", "--no-index", "--numstat", "--", "/dev/null", absPath)
	if err != nil {
		return gitNumstat{}, false
	}
	stats := parseGitNumstat(out)
	for _, stat := range stats {
		stat.path = relPath
		return stat, true
	}
	return gitNumstat{}, false
}

func parseGitStatusZ(out []byte) []gitStatusEntry {
	out = bytes.TrimRight(out, "\x00")
	if len(out) == 0 {
		return nil
	}
	tokens := bytes.Split(out, []byte{0})
	entries := make([]gitStatusEntry, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if len(token) < 4 {
			continue
		}
		entry := gitStatusEntry{
			code: string(token[:2]),
			path: string(token[3:]),
		}
		if strings.ContainsAny(entry.code, "RC") && i+1 < len(tokens) {
			i++
			entry.orig = string(tokens[i])
		}
		entries = append(entries, entry)
	}
	return entries
}

func parseGitNumstat(out []byte) map[string]gitNumstat {
	stats := map[string]gitNumstat{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		path := strings.TrimSpace(parts[2])
		if path == "" {
			continue
		}
		adds := parseNumstatCount(parts[0])
		dels := parseNumstatCount(parts[1])
		stats[path] = gitNumstat{path: path, adds: adds, dels: dels}
	}
	return stats
}

func parseNumstatCount(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "-" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func statusKeySet(entries []gitStatusEntry) map[string]struct{} {
	keys := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		keys[entry.key()] = struct{}{}
	}
	return keys
}

func statusEntrySetsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if _, ok := b[key]; !ok {
			return false
		}
	}
	return true
}

func gitOutput(ctx context.Context, repo string, args ...string) ([]byte, error) {
	return gitOutputAllowExit(ctx, repo, map[int]struct{}{0: {}}, args...)
}

func gitOutputAllowExit(ctx context.Context, repo string, allowed map[int]struct{}, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	gitCtx, cancel := context.WithTimeout(ctx, chatEnvGitTimeout)
	defer cancel()
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := exec.CommandContext(gitCtx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if gitCtx.Err() != nil {
		return out, gitCtx.Err()
	}
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if _, ok := allowed[exitErr.ExitCode()]; ok {
			return out, nil
		}
	}
	return out, err
}

func sameFilesystemPath(a, b string) bool {
	aa, err := filepath.Abs(strings.TrimSpace(a))
	if err != nil {
		aa = filepath.Clean(a)
	}
	bb, err := filepath.Abs(strings.TrimSpace(b))
	if err != nil {
		bb = filepath.Clean(b)
	}
	if eval, err := filepath.EvalSymlinks(aa); err == nil {
		aa = eval
	}
	if eval, err := filepath.EvalSymlinks(bb); err == nil {
		bb = eval
	}
	aa = filepath.Clean(aa)
	bb = filepath.Clean(bb)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}
