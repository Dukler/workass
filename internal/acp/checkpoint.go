package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	providercontract "workass/internal/provider"
)

const (
	checkpointStateVersion = 1
	checkpointLimit        = 30
	checkpointFileLimit    = chatEnvFileLimit
	chatDiffByteLimit      = 200 * 1024
)

type ChatCheckpoint struct {
	TurnSeq int                  `json:"turnSeq"`
	JobID   string               `json:"jobId"`
	Ts      string               `json:"ts"`
	Repos   []ChatCheckpointRepo `json:"repos"`
}

type ChatCheckpointRepo struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Branch       string `json:"branch,omitempty"`
	Ref          string `json:"ref,omitempty"`
	Commit       string `json:"commit,omitempty"`
	ObservedTree string `json:"observedTree,omitempty"`
	ChangedFiles int    `json:"changedFiles"`
	Skipped      bool   `json:"skipped,omitempty"`
	SkipReason   string `json:"skipReason,omitempty"`
}

type chatCheckpointFile struct {
	Version     int              `json:"version"`
	ChatID      string           `json:"chatId"`
	UpdatedAt   string           `json:"updatedAt"`
	Checkpoints []ChatCheckpoint `json:"checkpoints"`
}

type chatStructuredError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

func (e chatStructuredError) Error() string {
	data, err := json.Marshal(e)
	if err != nil {
		return e.Code + ": " + e.Message
	}
	return string(data)
}

func (m *Manager) ChatCheckpoints(chatID, tabID string) []ChatCheckpoint {
	chatID = m.resolveCheckpointChatID(chatID, tabID)
	if chatID == "" {
		return []ChatCheckpoint{}
	}
	state, err := m.loadCheckpointState(chatID)
	if err != nil {
		return []ChatCheckpoint{}
	}
	return cloneChatCheckpoints(state.Checkpoints)
}

func (m *Manager) restoreChatCheckpointTarget(ctx context.Context, chatID string, turnSeq int, checkpoint ChatCheckpoint) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" || turnSeq <= 0 || checkpoint.TurnSeq != turnSeq {
		return nil, structuredChatError("chat:rewind-invalid", "checkpoint target does not match the requested turn", map[string]any{"chatId": chatID, "turnSeq": turnSeq})
	}
	restorable := make([]ChatCheckpointRepo, 0, len(checkpoint.Repos))
	for _, repo := range checkpoint.Repos {
		if repo.Skipped || repo.Ref == "" || strings.TrimSpace(repo.Path) == "" {
			continue
		}
		expected, ok := m.trackedRepoBaseline(chatID, repo.Path)
		if !ok || expected.tree == "" {
			return nil, structuredChatError("chat:rewind-untracked", "repo is not tracked by Entorno", map[string]any{
				"chatId":  chatID,
				"turnSeq": turnSeq,
				"repo":    repo.Name,
				"path":    repo.Path,
			})
		}
		dirty, reason := repoChangedSinceBaseline(ctx, expected)
		if dirty {
			return nil, structuredChatError("chat:rewind-outside-modification", "repo was modified outside this chat", map[string]any{
				"chatId":  chatID,
				"turnSeq": turnSeq,
				"repo":    repo.Name,
				"path":    repo.Path,
				"reason":  reason,
			})
		}
		restorable = append(restorable, repo)
	}

	restored := make([]ChatCheckpointRepo, 0, len(restorable))
	for _, repo := range restorable {
		paths, err := changedPathsFromTreeishToWorktree(ctx, repo.Path, repo.Ref)
		if err != nil {
			return nil, err
		}
		for _, rel := range paths {
			if gitTreeHasPath(ctx, repo.Path, repo.Ref, rel) {
				if _, err := gitOutput(ctx, repo.Path, "restore", "--worktree", "--source", repo.Ref, "--", rel); err != nil {
					return nil, err
				}
				continue
			}
			if err := removeWorktreePath(repo.Path, rel); err != nil {
				return nil, err
			}
		}
		restored = append(restored, repo)
	}

	m.updateTrackedReposAfterRewind(ctx, chatID, restored)
	return map[string]any{"ok": true, "chatId": chatID, "turnSeq": turnSeq, "repos": checkpointReposPublic(restored)}, nil
}

// RestoreChatCheckpoint is the executor-only filesystem boundary used by the
// durable chat actor. It consumes the exact checkpoint bytes selected by the
// actor; it never reloads the manager checkpoint file by turn sequence. It
// emits no semantic event; the actor commits the result before any renderer
// invalidation is published.
func (m *Manager) RestoreChatCheckpoint(ctx context.Context, chatID string, turnSeq int, checkpointPayload json.RawMessage, checkpointDigest string, operationID providercontract.OperationID) (map[string]any, error) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	checkpointDigest = strings.ToLower(strings.TrimSpace(checkpointDigest))
	if operationID == "" || len(checkpointPayload) == 0 || checkpointDigest == "" {
		return nil, errors.New("checkpoint restore requires a stable operation and immutable target")
	}
	sum := sha256.Sum256(checkpointPayload)
	if hex.EncodeToString(sum[:]) != checkpointDigest {
		return nil, errors.New("checkpoint restore target digest does not match its bytes")
	}
	var checkpoint ChatCheckpoint
	if err := json.Unmarshal(checkpointPayload, &checkpoint); err != nil {
		return nil, fmt.Errorf("decode checkpoint restore target: %w", err)
	}
	result, err := m.restoreChatCheckpointTarget(ctx, chatID, turnSeq, checkpoint)
	if err != nil {
		return nil, err
	}
	result["operationId"] = string(operationID)
	return result, nil
}

func (m *Manager) ChatDiff(ctx context.Context, chatID, repoName, relPath string) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	chatID = strings.TrimSpace(chatID)
	repoName = strings.TrimSpace(repoName)
	rel, err := cleanGitRelPath(relPath)
	if err != nil {
		return nil, structuredChatError("chat:diff-invalid-path", err.Error(), nil)
	}
	repo, baseTurn, ok := m.latestCheckpointRepo(chatID, repoName)
	if !ok {
		return nil, structuredChatError("chat:diff-not-found", "checkpoint repo not found", map[string]any{"chatId": chatID, "repo": repoName})
	}
	diff, err := diffPathFromTreeish(ctx, repo.Path, repo.Ref, rel)
	if err != nil {
		return nil, err
	}
	diff = redactSensitiveText(diff)
	truncated := false
	if len(diff) > chatDiffByteLimit {
		diff = diff[:chatDiffByteLimit]
		truncated = true
	}
	return map[string]any{
		"chatId":    chatID,
		"turnSeq":   baseTurn,
		"repo":      repo.Name,
		"path":      rel,
		"text":      diff,
		"truncated": truncated,
	}, nil
}

// ChatDiffFromCheckpoints is the narrow filesystem executor used by the
// actor-owned chat runtime. The caller supplies the actor-selected metadata;
// Manager does not resolve a chat, tab, or latest checkpoint from its caches.
func (m *Manager) ChatDiffFromCheckpoints(ctx context.Context, chatID string, checkpoints []ChatCheckpoint, repoName, relPath string) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	chatID = strings.TrimSpace(chatID)
	repoName = strings.TrimSpace(repoName)
	rel, err := cleanGitRelPath(relPath)
	if err != nil {
		return nil, structuredChatError("chat:diff-invalid-path", err.Error(), nil)
	}
	var repo ChatCheckpointRepo
	baseTurn := 0
	found := false
	for i := len(checkpoints) - 1; i >= 0 && !found; i-- {
		checkpoint := checkpoints[i]
		for _, candidate := range checkpoint.Repos {
			if candidate.Skipped || candidate.Ref == "" || !repoMatches(candidate, repoName) {
				continue
			}
			repo, baseTurn, found = candidate, checkpoint.TurnSeq, true
			break
		}
	}
	if !found {
		return nil, structuredChatError("chat:diff-not-found", "checkpoint repo not found", map[string]any{"chatId": chatID, "repo": repoName})
	}
	diff, err := diffPathFromTreeish(ctx, repo.Path, repo.Ref, rel)
	if err != nil {
		return nil, err
	}
	diff = redactSensitiveText(diff)
	truncated := false
	if len(diff) > chatDiffByteLimit {
		diff = diff[:chatDiffByteLimit]
		truncated = true
	}
	return map[string]any{
		"chatId": chatID, "turnSeq": baseTurn, "repo": repo.Name,
		"path": rel, "text": diff, "truncated": truncated,
	}, nil
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

func (m *Manager) loadCheckpointState(chatID string) (chatCheckpointFile, error) {
	m.checkpointMu.Lock()
	defer m.checkpointMu.Unlock()
	return m.loadCheckpointStateUnlocked(chatID)
}

func (m *Manager) loadCheckpointStateUnlocked(chatID string) (chatCheckpointFile, error) {
	chatID = strings.TrimSpace(chatID)
	state := chatCheckpointFile{Version: checkpointStateVersion, ChatID: chatID, Checkpoints: []ChatCheckpoint{}}
	if chatID == "" {
		return state, nil
	}
	data, err := os.ReadFile(m.checkpointStatePath(chatID))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	var stored chatCheckpointFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return chatCheckpointFile{}, err
	}
	state = stored
	if state.Version != checkpointStateVersion {
		return chatCheckpointFile{}, fmt.Errorf("unsupported checkpoint state version %d", state.Version)
	}
	if state.Checkpoints == nil {
		return chatCheckpointFile{}, errors.New("checkpoint state is missing checkpoints")
	}
	if strings.TrimSpace(state.ChatID) == "" || state.ChatID != chatID {
		return chatCheckpointFile{}, errors.New("checkpoint state has invalid chat ownership")
	}
	sort.SliceStable(state.Checkpoints, func(i, j int) bool { return state.Checkpoints[i].TurnSeq < state.Checkpoints[j].TurnSeq })
	return state, nil
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

func (m *Manager) checkpointStatePath(chatID string) string {
	return filepath.Join(m.opts.StateDir, "checkpoints", checkpointFileName(chatID)+".json")
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

func (m *Manager) resolveCheckpointChatID(chatID, tabID string) string {
	chatID = strings.TrimSpace(chatID)
	if chatID != "" {
		return chatID
	}
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return ""
	}
	m.envMu.Lock()
	defer m.envMu.Unlock()
	if tracker := m.chatEnvTrackerLocked("", "", tabID); tracker != nil {
		return tracker.chatID
	}
	return ""
}

func (m *Manager) trackedRepoBaseline(chatID, repoPath string) (gitRepoBaseline, bool) {
	m.envMu.Lock()
	defer m.envMu.Unlock()
	tracker := m.chatEnvTrackerLocked("", chatID, "")
	if tracker == nil {
		return gitRepoBaseline{}, false
	}
	for _, repo := range tracker.repos {
		if sameFilesystemPath(repo.path, repoPath) {
			return repo, true
		}
	}
	return gitRepoBaseline{}, false
}

func (m *Manager) updateTrackedReposAfterRewind(ctx context.Context, chatID string, restored []ChatCheckpointRepo) {
	if len(restored) == 0 {
		return
	}
	restoredByPath := map[string]gitRepoBaseline{}
	for _, repo := range restored {
		tree := gitTreeishTree(ctx, repo.Path, repo.Ref)
		status := gitStatus(ctx, repo.Path)
		numstat := gitDiffNumstat(ctx, repo.Path)
		addUntrackedNumstat(ctx, repo.Path, status, nil, numstat)
		restoredByPath[repo.Path] = gitRepoBaseline{
			name:       repo.Name,
			path:       repo.Path,
			branch:     gitCurrentBranch(ctx, repo.Path),
			tree:       tree,
			status:     status,
			statusKeys: statusKeySet(status),
			numstat:    numstat,
		}
	}
	m.envMu.Lock()
	defer m.envMu.Unlock()
	tracker := m.chatEnvTrackerLocked("", chatID, "")
	if tracker == nil {
		return
	}
	for i := range tracker.repos {
		if baseline, ok := restoredByPath[tracker.repos[i].path]; ok {
			tracker.repos[i] = baseline
		}
	}
	tracker.payload = tracker.initialPayload()
	m.storeChatEnvTrackerLocked(tracker)
}

func (m *Manager) latestCheckpointRepo(chatID, repoName string) (ChatCheckpointRepo, int, bool) {
	state, err := m.loadCheckpointState(chatID)
	if err != nil {
		return ChatCheckpointRepo{}, 0, false
	}
	for i := len(state.Checkpoints) - 1; i >= 0; i-- {
		checkpoint := state.Checkpoints[i]
		for _, repo := range checkpoint.Repos {
			if repo.Skipped || repo.Ref == "" {
				continue
			}
			if repoMatches(repo, repoName) {
				return repo, checkpoint.TurnSeq, true
			}
		}
	}
	return ChatCheckpointRepo{}, 0, false
}

func checkpointByTurn(checkpoints []ChatCheckpoint, turnSeq int) (ChatCheckpoint, bool) {
	for _, checkpoint := range checkpoints {
		if checkpoint.TurnSeq == turnSeq {
			return checkpoint, true
		}
	}
	return ChatCheckpoint{}, false
}

func checkpointReposPublic(repos []ChatCheckpointRepo) []map[string]any {
	out := make([]map[string]any, 0, len(repos))
	for _, repo := range repos {
		out = append(out, map[string]any{
			"name":         repo.Name,
			"path":         repo.Path,
			"branch":       nullableString(repo.Branch),
			"ref":          nullableString(repo.Ref),
			"commit":       nullableString(repo.Commit),
			"changedFiles": repo.ChangedFiles,
			"skipped":      repo.Skipped,
			"skipReason":   nullableString(repo.SkipReason),
		})
	}
	return out
}

func cloneChatCheckpoints(in []ChatCheckpoint) []ChatCheckpoint {
	out := make([]ChatCheckpoint, len(in))
	copy(out, in)
	for i := range out {
		repos := make([]ChatCheckpointRepo, len(out[i].Repos))
		copy(repos, out[i].Repos)
		out[i].Repos = repos
	}
	return out
}

func repoMatches(repo ChatCheckpointRepo, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	return repo.Name == want || sameFilesystemPath(repo.Path, want)
}

func checkpointFileName(chatID string) string {
	return sanitizeToken(chatID)
}

func checkpointRef(chatID string, turnSeq int) string {
	return "refs/workass/checkpoints/" + sanitizeToken(chatID) + "/" + strconv.Itoa(turnSeq)
}

func checkpointCommitMessage(chatID string, turnSeq int, jobID string) string {
	return fmt.Sprintf("workass checkpoint turn %d job %s", turnSeq, sanitizeToken(jobID))
}

func sanitizeToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "_"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "_"
	}
	if strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	return out
}

func createWorktreeCheckpointCommit(ctx context.Context, repo, message string) (string, error) {
	tree, err := writeWorktreeTree(ctx, repo)
	if err != nil {
		return "", err
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

func gitWorktreeTree(ctx context.Context, repo string) string {
	tree, err := writeWorktreeTree(ctx, repo)
	if err != nil {
		return ""
	}
	return tree
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

func gitHead(ctx context.Context, repo string) (string, bool) {
	out, err := gitOutput(ctx, repo, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", false
	}
	head := strings.TrimSpace(string(out))
	return head, head != ""
}

func gitTreeishTree(ctx context.Context, repo, treeish string) string {
	out, err := gitOutput(ctx, repo, "rev-parse", treeish+"^{tree}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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

func gitOutputAllowExitWithEnv(ctx context.Context, repo string, allowed map[int]struct{}, extraEnv []string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	gitCtx, cancel := context.WithTimeout(ctx, chatEnvGitTimeout)
	defer cancel()
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := managedCommandContext(gitCtx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	cmd.Env = append(cmd.Env, extraEnv...)
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

func repoChangedSinceBaseline(ctx context.Context, baseline gitRepoBaseline) (bool, string) {
	if baseline.path == "" || baseline.tree == "" {
		return true, "missing-baseline"
	}
	if branch := gitCurrentBranch(ctx, baseline.path); baseline.branch != "" && branch != baseline.branch {
		return true, "branch-changed"
	}
	code, err := gitExitCode(ctx, baseline.path, "diff", "--quiet", "--no-ext-diff", baseline.tree, "--")
	if err != nil {
		return true, err.Error()
	}
	if code == 1 {
		return true, "worktree-diff"
	}
	if code != 0 {
		return true, "git-diff-exit-" + strconv.Itoa(code)
	}
	untracked, err := untrackedPathsAbsentFromTree(ctx, baseline.path, baseline.tree)
	if err != nil {
		return true, err.Error()
	}
	if len(untracked) > 0 {
		return true, "untracked:" + untracked[0]
	}
	return false, ""
}

func gitExitCode(ctx context.Context, repo string, args ...string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	gitCtx, cancel := context.WithTimeout(ctx, chatEnvGitTimeout)
	defer cancel()
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := managedCommandContext(gitCtx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	err := cmd.Run()
	if gitCtx.Err() != nil {
		return -1, gitCtx.Err()
	}
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

func changedPathCountFromTreeishToWorktree(ctx context.Context, repo, treeish string) (int, error) {
	paths, err := changedPathsFromTreeishToWorktree(ctx, repo, treeish)
	if err != nil {
		return 0, err
	}
	return len(paths), nil
}

func changedPathsFromTreeishToWorktree(ctx context.Context, repo, treeish string) ([]string, error) {
	out, err := gitOutput(ctx, repo, "diff", "--name-only", "-z", "--no-ext-diff", treeish, "--")
	if err != nil {
		return nil, err
	}
	paths := map[string]struct{}{}
	for _, path := range parseNULPaths(out) {
		paths[path] = struct{}{}
	}
	untracked, err := untrackedPathsAbsentFromTree(ctx, repo, treeish)
	if err != nil {
		return nil, err
	}
	for _, path := range untracked {
		paths[path] = struct{}{}
	}
	outPaths := make([]string, 0, len(paths))
	for path := range paths {
		outPaths = append(outPaths, path)
	}
	sort.Strings(outPaths)
	return outPaths, nil
}

func untrackedPathsAbsentFromTree(ctx context.Context, repo, treeish string) ([]string, error) {
	out, err := gitOutput(ctx, repo, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var absent []string
	for _, path := range parseNULPaths(out) {
		if !gitTreeHasPath(ctx, repo, treeish, path) {
			absent = append(absent, path)
		}
	}
	sort.Strings(absent)
	return absent, nil
}

func gitTreeHasPath(ctx context.Context, repo, treeish, rel string) bool {
	rel, err := cleanGitRelPath(rel)
	if err != nil {
		return false
	}
	_, err = gitOutput(ctx, repo, "cat-file", "-e", treeish+":"+rel)
	return err == nil
}

func diffPathFromTreeish(ctx context.Context, repo, treeish, rel string) (string, error) {
	if gitTreeHasPath(ctx, repo, treeish, rel) {
		out, err := gitOutputAllowExit(ctx, repo, map[int]struct{}{0: {}, 1: {}}, "diff", "--no-color", "--no-ext-diff", treeish, "--", rel)
		return string(out), err
	}
	abs := filepath.Join(repo, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", structuredChatError("chat:diff-directory", "path is a directory", map[string]any{"path": rel})
	}
	out, err := gitOutputAllowExit(ctx, repo, map[int]struct{}{0: {}, 1: {}}, "diff", "--no-color", "--no-ext-diff", "--no-index", "--", "/dev/null", abs)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func removeWorktreePath(repo, rel string) error {
	clean, err := cleanGitRelPath(rel)
	if err != nil {
		return err
	}
	abs := filepath.Join(repo, filepath.FromSlash(clean))
	if !sameFilesystemPath(filepath.Dir(abs), repo) && !strings.HasPrefix(filepath.Clean(abs), filepath.Clean(repo)+string(filepath.Separator)) {
		return fmt.Errorf("path escapes repo: %s", rel)
	}
	err = os.Remove(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func cleanGitRelPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("path is required")
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes repo: %s", raw)
	}
	return filepath.ToSlash(clean), nil
}

func parseNULPaths(out []byte) []string {
	out = bytes.TrimRight(out, "\x00")
	if len(out) == 0 {
		return nil
	}
	parts := bytes.Split(out, []byte{0})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		path := strings.TrimSpace(string(part))
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func structuredChatError(code, message string, fields map[string]any) error {
	return chatStructuredError{Code: code, Message: message, Fields: fields}
}
