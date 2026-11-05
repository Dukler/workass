package acp

// Legacy per-turn Entorno and checkpoint fixtures.
//
// The daemon once captured a Git baseline before every turn and diffed and
// checkpointed every repo after it. That ran Git on the send and turn path,
// and production stopped calling it. What remains here builds the data those
// turns used to leave behind, so the live surfaces that still read it — a
// stored Entorno reference, chat:diff, chat:rewind — keep their tests. None
// of it is compiled into the daemon.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// legacyTurns holds the pending pre-turn checkpoints the fixtures record,
// per tracker; production trackers no longer carry them.
var legacyTurns = struct {
	sync.Mutex
	byTracker map[*chatEnvTracker]map[string]chatTurnCheckpoint
}{byTracker: map[*chatEnvTracker]map[string]chatTurnCheckpoint{}}

func legacyPendingTurns(tracker *chatEnvTracker) map[string]chatTurnCheckpoint {
	legacyTurns.Lock()
	defer legacyTurns.Unlock()
	turns := legacyTurns.byTracker[tracker]
	if turns == nil {
		turns = map[string]chatTurnCheckpoint{}
		legacyTurns.byTracker[tracker] = turns
	}
	return turns
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
		if ctx.Err() != nil {
			return
		}
		current := captureRepoBaseline(ctx, repo.path)
		if ctx.Err() != nil {
			return
		}
		currentRepos = append(currentRepos, current)
		ref := checkpointRef(chatID, turnSeq)
		// The baseline already captured the complete worktree. Committing a
		// second snapshot both delays dispatch and can give rewind a different
		// tree from the baseline used to attribute the turn's changes.
		commit, err := createCheckpointCommitFromTree(ctx, repo.path, current.tree, checkpointCommitMessage(chatID, turnSeq, job.ID))
		pending := pendingCheckpointRepo{name: current.name, path: current.path, branch: current.branch, ref: ref, commit: commit}
		if err != nil {
			pending.err = err.Error()
		}
		pendingRepos = append(pendingRepos, pending)
	}
	if ctx.Err() != nil {
		return
	}

	m.envMu.Lock()
	tracker = m.chatEnvTrackerLocked(job.SessionID, chatID, tabID)
	if tracker != nil {
		tracker.chatID = chatID
		tracker.tabID = tabID
		tracker.cwd = cwd
		tracker.repos = currentRepos
		tracker.reposTruncated = reposTruncated
		legacyPendingTurns(tracker)[job.ID] = chatTurnCheckpoint{
			turnSeq: turnSeq,
			jobID:   job.ID,
			repos:   currentRepos,
			preRefs: pendingRepos,
		}
		m.storeChatEnvTrackerLocked(tracker)
	}
	m.envMu.Unlock()
}

func (m *Manager) discardChatTurnCheckpoint(job *Job) {
	if job == nil {
		return
	}
	m.envMu.Lock()
	defer m.envMu.Unlock()
	if tracker := m.chatEnvTrackerLocked(job.SessionID, job.ChatID, job.TabID); tracker != nil {
		delete(legacyPendingTurns(tracker), job.ID)
	}
}

func (m *Manager) refreshChatEnvAfterJob(ctx context.Context, job *Job) {
	if job == nil {
		return
	}
	snapshot, ok := m.chatEnvSnapshot(job.SessionID, job.ChatID, job.TabID, job.ID)
	if !ok {
		return
	}
	m.refreshChatEnvAfterJobSnapshot(ctx, job, snapshot)
}

// refreshChatEnvAfterJobSnapshot performs the filesystem-heavy half of the
// post-turn environment refresh. The caller may capture snapshot before
// publishing the terminal provider event, then run this work afterwards: the
// actor is free to clear its foreground turn immediately without racing the
// checkpoint inputs out from under this observer.
func (m *Manager) refreshChatEnvAfterJobSnapshot(ctx context.Context, job *Job, snapshot chatEnvSnapshot) {
	if job == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, currentRepos := buildChatEnvPayloadAndRepos(ctx, snapshot)

	m.envMu.Lock()
	publish := false
	tracker := m.chatEnvTrackerLocked(snapshot.sessionID, snapshot.chatID, snapshot.tabID)
	if tracker != nil {
		// A newer turn may begin as soon as the provider terminal is durable. Do
		// not let this older filesystem scan overwrite that turn's baseline or
		// publish stale environment state after it. Its immutable checkpoint is
		// still recorded below.
		if snapshot.turnSeq >= tracker.turnSeq {
			tracker.chatID = snapshot.chatID
			tracker.tabID = snapshot.tabID
			tracker.cwd = snapshot.cwd
			tracker.payload = payload
			if currentRepos != nil {
				tracker.repos = currentRepos
			}
			m.storeChatEnvTrackerLocked(tracker)
			publish = true
		}
		if job.ID != "" {
			delete(legacyPendingTurns(tracker), job.ID)
		}
	}
	m.envMu.Unlock()

	m.recordCheckpointAfterTurn(ctx, snapshot, payload, currentRepos)
	if !publish {
		return
	}
	if m.observeChatEnv(payload, job.startOpts.ProviderLaneManaged) {
		return
	}
	m.emit("chat:env", payload)
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

func (m *Manager) recordCheckpointAfterTurn(ctx context.Context, snapshot chatEnvSnapshot, payload ChatEnvPayload, currentRepos []gitRepoBaseline) {
	if snapshot.turnSeq <= 0 || snapshot.jobID == "" || snapshot.chatID == "" || len(payload.Repos) == 0 {
		return
	}
	touched := map[string]struct{}{}
	for _, repo := range payload.Repos {
		touched[repo.Name] = struct{}{}
	}
	currentByPath := map[string]gitRepoBaseline{}
	for _, repo := range currentRepos {
		currentByPath[repo.path] = repo
	}
	checkpoint := ChatCheckpoint{
		TurnSeq: snapshot.turnSeq,
		JobID:   snapshot.jobID,
		Ts:      time.Now().UTC().Format(time.RFC3339Nano),
		Repos:   []ChatCheckpointRepo{},
	}
	for _, pending := range snapshot.preRefs {
		if _, ok := touched[pending.name]; !ok {
			continue
		}
		repo := ChatCheckpointRepo{
			Name:   pending.name,
			Path:   pending.path,
			Branch: pending.branch,
			Ref:    pending.ref,
			Commit: pending.commit,
		}
		if current, ok := currentByPath[pending.path]; ok {
			repo.ObservedTree = current.tree
		}
		if pending.err != "" || pending.commit == "" {
			repo.Skipped = true
			repo.SkipReason = firstNonEmpty(pending.err, "checkpoint-create-failed")
			checkpoint.Repos = append(checkpoint.Repos, repo)
			continue
		}
		count, err := changedPathCountFromTreeishToWorktree(ctx, pending.path, pending.commit)
		if err != nil {
			repo.Skipped = true
			repo.SkipReason = "changed-count-failed: " + err.Error()
			checkpoint.Repos = append(checkpoint.Repos, repo)
			continue
		}
		repo.ChangedFiles = count
		if count > checkpointFileLimit {
			repo.Skipped = true
			repo.SkipReason = "changed-file-limit"
			checkpoint.Repos = append(checkpoint.Repos, repo)
			continue
		}
		if _, err := gitOutput(ctx, pending.path, "update-ref", pending.ref, pending.commit); err != nil {
			repo.Skipped = true
			repo.SkipReason = "update-ref-failed: " + err.Error()
		}
		checkpoint.Repos = append(checkpoint.Repos, repo)
	}
	if len(checkpoint.Repos) == 0 {
		return
	}
	if err := m.appendCheckpoint(snapshot.chatID, checkpoint); err != nil {
		m.opts.Logf("checkpoint record failed", map[string]any{"chatId": snapshot.chatID, "turnSeq": snapshot.turnSeq, "error": err.Error()})
	}
}

func createWorktreeCheckpointCommit(ctx context.Context, repo, message string) (string, error) {
	tree, err := writeWorktreeTree(ctx, repo)
	if err != nil {
		return "", err
	}
	return createCheckpointCommitFromTree(ctx, repo, tree, message)
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
	{
		if turn, ok := legacyPendingTurns(tracker)[strings.TrimSpace(jobID)]; ok {
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

func isGitRepoRoot(ctx context.Context, path string) bool {
	out, err := gitOutput(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	root := strings.TrimSpace(string(out))
	return sameFilesystemPath(root, path)
}

func createCheckpointCommitFromTree(ctx context.Context, repo, tree, message string) (string, error) {
	if tree == "" {
		return "", errors.New("checkpoint baseline tree is unavailable")
	}
	args := []string{"commit-tree", tree}
	if head, ok := gitHead(ctx, repo); ok {
		args = append(args, "-p", head)
	}
	args = append(args, "-m", message)
	out, err := gitOutputWithEnv(ctx, repo, gitCommitIdentityEnv(time.Now().UTC()), args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func checkpointCommitMessage(chatID string, turnSeq int, jobID string) string {
	return fmt.Sprintf("workass checkpoint turn %d job %s", turnSeq, sanitizeToken(jobID))
}

func (m *Manager) maxCheckpointTurnSeq(chatID string) int {
	state, err := m.loadCheckpointState(chatID)
	if err != nil {
		return 0
	}
	maxSeq := 0
	for _, checkpoint := range state.Checkpoints {
		if checkpoint.TurnSeq > maxSeq {
			maxSeq = checkpoint.TurnSeq
		}
	}
	return maxSeq
}

func (m *Manager) appendCheckpoint(chatID string, checkpoint ChatCheckpoint) error {
	m.checkpointMu.Lock()
	defer m.checkpointMu.Unlock()
	state, err := m.loadCheckpointStateUnlocked(chatID)
	if err != nil {
		return err
	}
	state.Checkpoints = append(state.Checkpoints, checkpoint)
	sort.SliceStable(state.Checkpoints, func(i, j int) bool { return state.Checkpoints[i].TurnSeq < state.Checkpoints[j].TurnSeq })
	var rotated []ChatCheckpoint
	if len(state.Checkpoints) > checkpointLimit {
		rotated = append(rotated, state.Checkpoints[:len(state.Checkpoints)-checkpointLimit]...)
		state.Checkpoints = append([]ChatCheckpoint(nil), state.Checkpoints[len(state.Checkpoints)-checkpointLimit:]...)
	}
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := m.saveCheckpointStateUnlocked(state); err != nil {
		return err
	}
	for _, old := range rotated {
		for _, repo := range old.Repos {
			if repo.Ref != "" && repo.Path != "" {
				_, _ = gitOutput(context.Background(), repo.Path, "update-ref", "-d", repo.Ref)
			}
		}
	}
	return nil
}

func checkpointRef(chatID string, turnSeq int) string {
	return "refs/workass/checkpoints/" + sanitizeToken(chatID) + "/" + strconv.Itoa(turnSeq)
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

func gitWorktreeTree(ctx context.Context, repo string) string {
	tree, err := writeWorktreeTree(ctx, repo)
	if err != nil {
		return ""
	}
	return tree
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

func writeWorktreeTree(ctx context.Context, repo string) (string, error) {
	tmp, err := os.MkdirTemp("", "workass-git-index-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if head, ok := gitHead(ctx, repo); ok {
		if _, err := gitOutputWithEnv(ctx, repo, env, "read-tree", head); err != nil {
			return "", err
		}
	} else if _, err := gitOutputWithEnv(ctx, repo, env, "read-tree", "--empty"); err != nil {
		return "", err
	}
	if _, err := gitOutputWithEnv(ctx, repo, env, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	out, err := gitOutputWithEnv(ctx, repo, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitCommitIdentityEnv(ts time.Time) []string {
	when := ts.Format(time.RFC3339)
	return []string{
		"GIT_AUTHOR_NAME=workass",
		"GIT_AUTHOR_EMAIL=workass@example.invalid",
		"GIT_COMMITTER_NAME=workass",
		"GIT_COMMITTER_EMAIL=workass@example.invalid",
		"GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_DATE=" + when,
	}
}

func gitOutputWithEnv(ctx context.Context, repo string, extraEnv []string, args ...string) ([]byte, error) {
	return gitOutputAllowExitWithEnv(ctx, repo, map[int]struct{}{0: {}}, extraEnv, args...)
}

func gitHead(ctx context.Context, repo string) (string, bool) {
	out, err := gitOutput(ctx, repo, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", false
	}
	head := strings.TrimSpace(string(out))
	return head, head != ""
}

func gitOutputAllowExitWithEnv(ctx context.Context, repo string, allowed map[int]struct{}, extraEnv []string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	gitCtx, cancel := context.WithTimeout(ctx, chatEnvGitTimeout)
	defer cancel()
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := managedCommand("git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := gitCommandOutput(gitCtx, cmd)
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

func changedPathCountFromTreeishToWorktree(ctx context.Context, repo, treeish string) (int, error) {
	paths, err := changedPathsFromTreeishToWorktree(ctx, repo, treeish)
	if err != nil {
		return 0, err
	}
	return len(paths), nil
}

func checkpointByTurn(checkpoints []ChatCheckpoint, turnSeq int) (ChatCheckpoint, bool) {
	for _, checkpoint := range checkpoints {
		if checkpoint.TurnSeq == turnSeq {
			return checkpoint, true
		}
	}
	return ChatCheckpoint{}, false
}

func (m *Manager) saveCheckpointStateUnlocked(state chatCheckpointFile) error {
	dir := filepath.Join(m.opts.StateDir, "checkpoints")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	state.Version = checkpointStateVersion
	path := m.checkpointStatePath(state.ChatID)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
