package acp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

// Ordinary chat lifecycle must never run Git, including when an older install
// left a repository baseline attached to the chat.
func TestChatLifecycleDoesNotRunAutomaticGit(t *testing.T) {
	requireGit(t)
	for _, managed := range []bool{false, true} {
		for _, restored := range []bool{false, true} {
			t.Run(fmt.Sprintf("managed=%t/restored=%t", managed, restored), func(t *testing.T) {
				workspace := t.TempDir()
				repo := filepath.Join(workspace, "repo")
				initTinyGitRepo(t, repo, map[string]string{"work.txt": "before\n"})
				root := repoRoot(t)
				events := newEventCollector()
				manager := NewManager(Options{
					RootDir: root, StateDir: t.TempDir(), RSSSampleInterval: time.Hour,
					Provider:  ProviderConfig{Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")}, CWD: root},
					Broadcast: events.Broadcast,
				})
				t.Cleanup(func() { manager.Reset() })
				initialized := make(chan struct{}, 1)
				manager.SetChatEnvObserver(func(env ChatEnvPayload) error {
					events.Broadcast("chat:env", env)
					initialized <- struct{}{}
					return nil
				})
				if restored {
					seedLegacyChatEnvFixture(t, manager, "old-session", "no-git-chat", "no-git-tab", workspace)
				}
				writeFile(t, filepath.Join(repo, "work.txt"), "changed\n")
				traceFile := filepath.Join(t.TempDir(), "git-trace.log")
				t.Setenv("GIT_TRACE", traceFile)
				session, err := manager.NewSession(context.Background(), SessionOptions{
					CWD: workspace, TabID: "no-git-tab", ChatID: "no-git-chat", ProviderLaneManaged: managed,
				})
				if err != nil {
					t.Fatal(err)
				}
				if managed {
					select {
					case <-initialized:
					case <-time.After(2 * time.Second):
						t.Fatal("session environment initialization did not finish")
					}
				}

				for i, prompt := range []string{"ordinary chat", "[mock:slow] stop after dispatch", "[mock:slow] immediate stop"} {
					id := ""
					if managed {
						id = fmt.Sprintf("no-git-job-%d", i)
					}
					started := time.Now()
					job, err := manager.StartJob(context.Background(), JobStartOptions{
						JobID: id, OperationID: "no-git-operation-" + fmt.Sprint(i), ProviderLaneManaged: managed,
						Kind: "app-chat", SessionID: session.SessionID, TabID: "no-git-tab", ChatID: "no-git-chat", CWD: workspace, Prompt: prompt,
					})
					if err != nil {
						t.Fatal(err)
					}
					id = jobID(job)
					if time.Since(started) > time.Second {
						t.Fatal("prompt admission blocked")
					}
					if i == 1 {
						events.waitJobType(t, id, "data", 2*time.Second)
					}
					if i > 0 {
						if result := manager.CancelJobResult(jobID(job)); !result.Cancelled {
							t.Fatalf("Stop: %#v", result)
						}
						assertJobStatus(t, events.waitJobEnd(t, id, time.Second), "failed", 130, "cancelled")
					} else {
						assertJobStatus(t, events.waitJobEnd(t, id, 3*time.Second), "done", 0, "end_turn")
					}
					manager.jobWG.Wait() // includes everything scheduled after terminal publication
				}
				if checkpoints := manager.ChatCheckpoints("no-git-chat", "no-git-tab"); len(checkpoints) != 0 {
					t.Fatal("chat created automatic checkpoints")
				}
				trace, err := os.ReadFile(traceFile)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if len(trace) != 0 {
					t.Fatalf("ordinary chat invoked Git: %s", trace)
				}
			})
		}
	}
}

func TestPreTurnCheckpointCapturesWorktreeOnce(t *testing.T) {
	requireGit(t)
	workspace := t.TempDir()
	repo := filepath.Join(workspace, "repo")
	initTinyGitRepo(t, repo, map[string]string{"work.txt": "before\n"})
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root, StateDir: t.TempDir(), RSSSampleInterval: time.Hour,
		Provider:  ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root},
		Broadcast: events.Broadcast,
	})
	t.Cleanup(func() { manager.Reset() })
	session, err := manager.NewSession(context.Background(), SessionOptions{CWD: workspace, TabID: "single-cp-tab", ChatID: "single-cp-chat"})
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyChatEnvFixture(t, manager, session.SessionID, "single-cp-chat", "single-cp-tab", workspace)
	writeFile(t, filepath.Join(repo, "work.txt"), "changed\n")
	traceFile := filepath.Join(t.TempDir(), "git-trace.log")
	t.Setenv("GIT_TRACE", traceFile)
	job := &Job{ID: "single-cp-job", SessionID: session.SessionID, TabID: "single-cp-tab", ChatID: "single-cp-chat", CWD: workspace}
	manager.beginChatTurnCheckpoint(context.Background(), job)
	trace, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(trace), " add -A -- ."); count != 1 {
		t.Fatalf("one pre-turn snapshot rebuilt the full worktree %d times; want 1", count)
	}
	snapshot, ok := manager.chatEnvSnapshot(job.SessionID, job.ChatID, job.TabID, job.ID)
	if !ok || len(snapshot.repos) != 1 || len(snapshot.preRefs) != 1 {
		t.Fatal("missing pre-turn snapshot")
	}
	if got := gitTreeishTree(context.Background(), repo, snapshot.preRefs[0].commit); got != snapshot.repos[0].tree {
		t.Fatal("checkpoint commit must use the exact captured baseline tree")
	}
}

func TestChatCheckpointsDiffRewindAndOutsideGuard(t *testing.T) {
	t.Parallel()
	requireGit(t)
	workspace := t.TempDir()
	repoDir := filepath.Join(workspace, "alpha")
	initTinyGitRepo(t, repoDir, map[string]string{"work.txt": "one\n"})
	stateDir := filepath.Join(t.TempDir(), "state")
	manager, events := newFakeManager(t, "slow-prompt", Options{StateDir: stateDir, RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{CWD: workspace, TabID: "cp-tab", ChatID: "chat-cp"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	seedLegacyChatEnvFixture(t, manager, session.SessionID, "chat-cp", "cp-tab", workspace)

	workPath := filepath.Join(repoDir, "work.txt")
	hashA := fileSHA256(t, workPath)
	job1, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind:      "app-chat",
		SessionID: session.SessionID,
		ChatID:    "chat-cp",
		TabID:     "cp-tab",
		CWD:       workspace,
		Prompt:    "turn one",
	})
	if err != nil {
		t.Fatalf("start job1: %v", err)
	}
	cpJob1 := beginLegacyCheckpointFixture(manager, job1, session.SessionID, "chat-cp", "cp-tab", workspace)
	writeFile(t, workPath, "one\ntwo\nthree\n")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job1), 2*time.Second), "done", 0, "end_turn")
	manager.refreshChatEnvAfterJob(context.Background(), cpJob1)
	waitChatCheckpointCount(t, manager, "chat-cp", 1, 2*time.Second)
	hashB := fileSHA256(t, workPath)

	job2, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind:      "app-chat",
		SessionID: session.SessionID,
		ChatID:    "chat-cp",
		TabID:     "cp-tab",
		CWD:       workspace,
		Prompt:    "turn two",
	})
	if err != nil {
		t.Fatalf("start job2: %v", err)
	}
	cpJob2 := beginLegacyCheckpointFixture(manager, job2, session.SessionID, "chat-cp", "cp-tab", workspace)
	writeFile(t, workPath, "one\ntwo\nthree\nfour\n")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job2), 2*time.Second), "done", 0, "end_turn")
	manager.refreshChatEnvAfterJob(context.Background(), cpJob2)
	hashC := fileSHA256(t, workPath)

	checkpoints := waitChatCheckpointCount(t, manager, "chat-cp", 2, 2*time.Second)
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoints = %#v", checkpoints)
	}
	if checkpoints[0].TurnSeq != 1 || checkpoints[1].TurnSeq != 2 || checkpoints[0].Repos[0].Skipped || checkpoints[1].Repos[0].Skipped {
		t.Fatalf("checkpoint metadata = %#v", checkpoints)
	}
	if checkpoints[0].Repos[0].ChangedFiles != 1 || checkpoints[1].Repos[0].ChangedFiles != 1 {
		t.Fatalf("changed file counts = %#v", checkpoints)
	}
	verifyGitRef(t, repoDir, checkpoints[0].Repos[0].Ref, checkpoints[0].Repos[0].Commit)
	verifyGitRef(t, repoDir, checkpoints[1].Repos[0].Ref, checkpoints[1].Repos[0].Commit)
	t.Logf("trace checkpoint recorded chat=chat-cp turnSeqs=%d,%d refs=%s,%s hashes=%s,%s,%s", checkpoints[0].TurnSeq, checkpoints[1].TurnSeq, checkpoints[0].Repos[0].Ref, checkpoints[1].Repos[0].Ref, hashA, hashB, hashC)

	diff, err := manager.ChatDiff(context.Background(), "chat-cp", "alpha", "work.txt")
	if err != nil {
		t.Fatalf("chat diff: %v", err)
	}
	diffText := diff["text"].(string)
	if diff["truncated"] != false || !strings.Contains(diffText, "@@") || !strings.Contains(diffText, "+four") {
		t.Fatalf("diff result = %#v", diff)
	}
	t.Logf("trace diff chat=chat-cp repo=alpha path=work.txt turnSeq=%v contains=%q", diff["turnSeq"], "+four")

	restore := func(turnSeq int, operationID providercontract.OperationID) (map[string]any, error) {
		checkpoint, ok := checkpointByTurn(checkpoints, turnSeq)
		if !ok {
			t.Fatalf("checkpoint %d disappeared", turnSeq)
		}
		payload, err := json.Marshal(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		return manager.RestoreChatCheckpoint(context.Background(), "chat-cp", turnSeq, payload, fmt.Sprintf("%x", digest), operationID)
	}
	if _, err := restore(1, "rewind-turn-1"); err != nil {
		t.Fatalf("rewind turn 1: %v", err)
	}
	if got := fileSHA256(t, workPath); got != hashA {
		t.Fatalf("rewind turn1 hash=%s want=%s", got, hashA)
	}
	if _, err := restore(2, "rewind-turn-2"); err != nil {
		t.Fatalf("rewind turn 2: %v", err)
	}
	if got := fileSHA256(t, workPath); got != hashB {
		t.Fatalf("rewind turn2 hash=%s want=%s", got, hashB)
	}
	t.Logf("trace rewind hashes turn1=%s turn2=%s", hashA, hashB)

	writeFile(t, workPath, "outside\n")
	_, err = restore(1, "rewind-outside-modification")
	if err == nil || structuredErrorCode(err) != "chat:rewind-outside-modification" {
		t.Fatalf("outside modification error = %v code=%s", err, structuredErrorCode(err))
	}
	t.Logf("trace rewind refused code=%s", structuredErrorCode(err))
}

func TestChatCheckpointRotationAndLargeRepoSkip(t *testing.T) {
	t.Parallel()
	requireGit(t)
	t.Run("rotation", func(t *testing.T) {
		for _, existing := range []int{checkpointLimit - 1, checkpointLimit} {
			t.Run(fmt.Sprintf("cap-from-%d", existing), func(t *testing.T) {
				manager := NewManager(Options{StateDir: filepath.Join(t.TempDir(), "state")})
				t.Cleanup(func() { manager.Reset() })
				seed := chatCheckpointFile{Version: checkpointStateVersion, ChatID: "chat-cap", Checkpoints: []ChatCheckpoint{}}
				for turnSeq := 1; turnSeq <= existing; turnSeq++ {
					seed.Checkpoints = append(seed.Checkpoints, ChatCheckpoint{TurnSeq: turnSeq, JobID: fmt.Sprintf("job-%d", turnSeq)})
				}
				if err := manager.saveCheckpointStateUnlocked(seed); err != nil {
					t.Fatalf("seed %d checkpoints: %v", existing, err)
				}
				if err := manager.appendCheckpoint("chat-cap", ChatCheckpoint{TurnSeq: existing + 1, JobID: "job-boundary"}); err != nil {
					t.Fatalf("append checkpoint at cap boundary: %v", err)
				}
				got := manager.ChatCheckpoints("chat-cap", "")
				wantLen := existing + 1
				wantFirst := 1
				if wantLen > checkpointLimit {
					wantLen = checkpointLimit
					wantFirst = 2
				}
				if len(got) != wantLen || got[0].TurnSeq != wantFirst || got[len(got)-1].TurnSeq != existing+1 {
					t.Fatalf("cap from %d checkpoints = %#v", existing, got)
				}
			})
		}

		workspace := t.TempDir()
		repoDir := filepath.Join(workspace, "alpha")
		initTinyGitRepo(t, repoDir, map[string]string{"work.txt": "base\n"})
		manager := NewManager(Options{StateDir: filepath.Join(t.TempDir(), "state")})
		t.Cleanup(func() { manager.Reset() })
		commitBytes, err := gitOutput(context.Background(), repoDir, "rev-parse", "HEAD")
		if err != nil {
			t.Fatalf("resolve rotation commit: %v", err)
		}
		commit := strings.TrimSpace(string(commitBytes))
		oldRef := checkpointRef("chat-rotate", 1)
		newRef := checkpointRef("chat-rotate", checkpointLimit+1)
		for _, ref := range []string{oldRef, newRef} {
			if _, err := gitOutput(context.Background(), repoDir, "update-ref", ref, commit); err != nil {
				t.Fatalf("seed rotation ref %s: %v", ref, err)
			}
		}
		seed := chatCheckpointFile{Version: checkpointStateVersion, ChatID: "chat-rotate", Checkpoints: []ChatCheckpoint{}}
		for turnSeq := 1; turnSeq <= checkpointLimit; turnSeq++ {
			repo := ChatCheckpointRepo{Name: "alpha", Path: repoDir}
			if turnSeq == 1 {
				repo.Ref = oldRef
				repo.Commit = commit
			}
			seed.Checkpoints = append(seed.Checkpoints, ChatCheckpoint{
				TurnSeq: turnSeq, JobID: fmt.Sprintf("job-%d", turnSeq), Repos: []ChatCheckpointRepo{repo},
			})
		}
		if err := manager.saveCheckpointStateUnlocked(seed); err != nil {
			t.Fatalf("seed rotation ledger: %v", err)
		}
		if err := manager.appendCheckpoint("chat-rotate", ChatCheckpoint{
			TurnSeq: checkpointLimit + 1, JobID: "job-latest",
			Repos: []ChatCheckpointRepo{{Name: "alpha", Path: repoDir, Ref: newRef, Commit: commit}},
		}); err != nil {
			t.Fatalf("append rotating checkpoint: %v", err)
		}
		checkpoints := manager.ChatCheckpoints("chat-rotate", "")
		if len(checkpoints) != checkpointLimit || checkpoints[0].TurnSeq != 2 || checkpoints[len(checkpoints)-1].TurnSeq != checkpointLimit+1 {
			t.Fatalf("rotated checkpoints = %#v", checkpoints)
		}
		if gitRefExists(t, repoDir, checkpointRef("chat-rotate", 1)) {
			t.Fatalf("rotated ref still exists")
		}
		if !gitRefExists(t, repoDir, checkpointRef("chat-rotate", checkpointLimit+1)) {
			t.Fatalf("latest ref missing")
		}
		t.Logf("trace checkpoint rotation count=%d first=%d last=%d", len(checkpoints), checkpoints[0].TurnSeq, checkpoints[len(checkpoints)-1].TurnSeq)
	})

	t.Run("large repo skip", func(t *testing.T) {
		workspace := t.TempDir()
		repoDir := filepath.Join(workspace, "large")
		files := map[string]string{}
		for i := 0; i < checkpointFileLimit+1; i++ {
			files[fmt.Sprintf("file%03d.txt", i)] = "base\n"
		}
		initTinyGitRepo(t, repoDir, files)
		manager, events := newFakeManager(t, "slow-prompt", Options{StateDir: filepath.Join(t.TempDir(), "state"), RSSSampleInterval: time.Hour})
		t.Cleanup(func() { manager.Reset() })
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		session, err := manager.NewSession(ctx, SessionOptions{CWD: repoDir, TabID: "large-tab", ChatID: "chat-large"})
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		seedLegacyChatEnvFixture(t, manager, session.SessionID, "chat-large", "large-tab", repoDir)
		job, err := manager.StartJob(context.Background(), JobStartOptions{
			Kind:      "app-chat",
			SessionID: session.SessionID,
			ChatID:    "chat-large",
			TabID:     "large-tab",
			CWD:       repoDir,
			Prompt:    "large change",
		})
		if err != nil {
			t.Fatalf("start large job: %v", err)
		}
		cpJob := beginLegacyCheckpointFixture(manager, job, session.SessionID, "chat-large", "large-tab", repoDir)
		for i := 0; i < checkpointFileLimit+1; i++ {
			writeFile(t, filepath.Join(repoDir, fmt.Sprintf("file%03d.txt", i)), "base\nchanged\n")
		}
		assertJobStatus(t, events.waitJobEnd(t, jobID(job), 3*time.Second), "done", 0, "end_turn")
		manager.refreshChatEnvAfterJob(context.Background(), cpJob)
		checkpoints := waitChatCheckpointCount(t, manager, "chat-large", 1, 3*time.Second)
		if len(checkpoints) != 1 || len(checkpoints[0].Repos) != 1 {
			t.Fatalf("large checkpoints = %#v", checkpoints)
		}
		repo := checkpoints[0].Repos[0]
		if !repo.Skipped || repo.SkipReason != "changed-file-limit" || repo.ChangedFiles != checkpointFileLimit+1 {
			t.Fatalf("large checkpoint repo = %#v", repo)
		}
		if gitRefExists(t, repoDir, repo.Ref) {
			t.Fatalf("skipped large ref exists: %s", repo.Ref)
		}
		t.Logf("trace checkpoint skipped repo=%s changedFiles=%d reason=%s", repo.Name, repo.ChangedFiles, repo.SkipReason)
	})
}

func waitChatCheckpointCount(t *testing.T, manager *Manager, chatID string, count int, timeout time.Duration) []ChatCheckpoint {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		checkpoints := manager.ChatCheckpoints(chatID, "")
		if len(checkpoints) >= count {
			return checkpoints
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat %s checkpoints = %#v, want at least %d", chatID, checkpoints, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hash file: %v", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func verifyGitRef(t *testing.T, repo, ref, wantCommit string) {
	t.Helper()
	out, err := gitOutput(context.Background(), repo, "rev-parse", "--verify", ref)
	if err != nil {
		t.Fatalf("verify ref %s: %v", ref, err)
	}
	got := strings.TrimSpace(string(out))
	if got != wantCommit {
		t.Fatalf("ref %s = %s want %s", ref, got, wantCommit)
	}
}

func gitRefExists(t *testing.T, repo, ref string) bool {
	t.Helper()
	_, err := gitOutput(context.Background(), repo, "rev-parse", "--verify", ref)
	return err == nil
}

func structuredErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal([]byte(err.Error()), &payload) != nil {
		return ""
	}
	code, _ := payload["code"].(string)
	return code
}

func TestCheckpointLoaderRejectsUnversionedAndUnownedState(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{StateDir: filepath.Join(t.TempDir(), "state")})
	t.Cleanup(func() { manager.Reset() })
	path := manager.checkpointStatePath("chat-checkpoint-strict")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"unversioned":   `{"chatId":"chat-checkpoint-strict","checkpoints":[]}`,
		"missing-owner": `{"version":1,"checkpoints":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.loadCheckpointState("chat-checkpoint-strict"); err == nil {
				t.Fatal("accepted invalid checkpoint state")
			}
		})
	}
}
