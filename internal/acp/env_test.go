package acp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderLaneManagedChatEnvInitializationDoesNotBlockSession(t *testing.T) {
	workspace := t.TempDir()
	manager, _ := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	entered := make(chan struct{})
	release := make(chan struct{})
	initialized := make(chan struct{}, 1)
	manager.SetChatEnvRestorer(func(_, _ string) error {
		close(entered)
		<-release
		return nil
	})
	manager.SetChatEnvObserver(func(ChatEnvPayload) error {
		initialized <- struct{}{}
		return nil
	})

	returned := make(chan struct{})
	go func() {
		manager.initChatEnvForSession(context.Background(), SessionOptions{
			CWD: workspace, TabID: "provider-lane-tab", ChatID: "provider-lane-chat", ProviderLaneManaged: true,
		}, SessionInfo{SessionID: "provider-lane-session", CWD: workspace})
		close(returned)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("provider-lane chat environment initialization did not start")
	}
	select {
	case <-returned:
	case <-time.After(250 * time.Millisecond):
		close(release)
		t.Fatal("provider-lane session creation waited for chat environment restoration")
	}
	close(release)
	select {
	case <-initialized:
	case <-time.After(2 * time.Second):
		t.Fatal("provider-lane chat environment initialization did not finish")
	}
}

func TestLateChatEnvRefreshCannotOverwriteOrPublishPastANewerTurn(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast})
	t.Cleanup(func() { manager.Reset() })
	newer := emptyChatEnvPayload("env-order-chat", "env-order-tab", "/newer")
	tracker := &chatEnvTracker{
		sessionID: "env-order-session", chatID: newer.ChatID, tabID: newer.TabID, cwd: newer.CWD,
		payload: newer, turnSeq: 2,
		pendingTurns: map[string]chatTurnCheckpoint{
			"env-order-old-job": {turnSeq: 1, jobID: "env-order-old-job"},
		},
	}
	manager.envMu.Lock()
	manager.storeChatEnvTrackerLocked(tracker)
	manager.envMu.Unlock()

	manager.refreshChatEnvAfterJobSnapshot(context.Background(), &Job{ID: "env-order-old-job"}, chatEnvSnapshot{
		sessionID: tracker.sessionID, chatID: tracker.chatID, tabID: tracker.tabID,
		cwd: "/older", turnSeq: 1, jobID: "env-order-old-job",
	})
	if got := manager.ChatEnvGet(tracker.chatID, tracker.tabID); got.CWD != newer.CWD {
		t.Fatalf("late environment refresh replaced newer state: %#v", got)
	}
	manager.envMu.Lock()
	_, pending := tracker.pendingTurns["env-order-old-job"]
	manager.envMu.Unlock()
	if pending {
		t.Fatal("late environment refresh did not release its own pending checkpoint")
	}
	for _, event := range events.snapshot() {
		if event.channel == "chat:env" {
			t.Fatalf("late environment refresh was published: %#v", event.payload)
		}
	}
}

func TestChatEnvTracksRepoChangesAfterTurn(t *testing.T) {
	t.Parallel()
	requireGit(t)
	workspace := t.TempDir()
	alpha := filepath.Join(workspace, "alpha")
	beta := filepath.Join(workspace, "beta")
	initTinyGitRepo(t, alpha, map[string]string{"work.txt": "one\n"})
	initTinyGitRepo(t, beta, map[string]string{"other.txt": "base\n"})

	manager, events := newFakeManager(t, "slow-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{CWD: workspace, TabID: "env-tab", ChatID: "chat-env"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	created := waitChatEnv(t, events, func(env ChatEnvPayload) bool {
		return env.ChatID == "chat-env" && env.TabID == "env-tab" && sameFilesystemPath(env.CWD, workspace)
	}, 2*time.Second)
	if len(created.Repos) != 0 || strings.Join(created.Unchanged, ",") != "alpha,beta" {
		t.Fatalf("session env = %#v", created)
	}

	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind:      "app-chat",
		SessionID: session.SessionID,
		ChatID:    "chat-env",
		TabID:     "env-tab",
		CWD:       workspace,
		Prompt:    "simulate edit",
	})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	if err := os.WriteFile(filepath.Join(alpha, "work.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatalf("edit alpha: %v", err)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	changed := waitChatEnv(t, events, func(env ChatEnvPayload) bool {
		return env.ChatID == "chat-env" && len(env.Repos) == 1 && env.Repos[0].Name == "alpha"
	}, 2*time.Second)
	if strings.Join(changed.Unchanged, ",") != "beta" {
		t.Fatalf("unchanged = %#v", changed.Unchanged)
	}
	repo := changed.Repos[0]
	if repo.Branch != "main" || repo.Adds != 2 || repo.Dels != 0 || repo.FilesTruncated {
		t.Fatalf("changed repo = %#v", repo)
	}
	if len(repo.Files) != 1 || repo.Files[0] != (ChatEnvFile{Path: "work.txt", Adds: 2, Dels: 0}) {
		t.Fatalf("changed files = %#v", repo.Files)
	}
	cached := manager.ChatEnvGet("chat-env", "")
	if len(cached.Repos) != 1 || cached.Repos[0].Adds != 2 || strings.Join(cached.Unchanged, ",") != "beta" {
		t.Fatalf("cached env = %#v", cached)
	}
}

func TestChatEnvNonGitCwdIsEmpty(t *testing.T) {
	t.Parallel()
	requireGit(t)
	workspace := t.TempDir()
	manager, events := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, SessionOptions{CWD: workspace, TabID: "non-git-tab", ChatID: "chat-non-git"}); err != nil {
		t.Fatalf("new session: %v", err)
	}
	env := waitChatEnv(t, events, func(env ChatEnvPayload) bool {
		return env.ChatID == "chat-non-git"
	}, 2*time.Second)
	if len(env.Repos) != 0 || len(env.Unchanged) != 0 || !sameFilesystemPath(env.CWD, workspace) {
		t.Fatalf("non-git env = %#v", env)
	}
	cached := manager.ChatEnvGet("", "non-git-tab")
	if len(cached.Repos) != 0 || len(cached.Unchanged) != 0 || !sameFilesystemPath(cached.CWD, workspace) {
		t.Fatalf("cached non-git env = %#v", cached)
	}
}

func TestChatEnvTruncationFlags(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const fixtureTimeout = 8 * time.Second
	t.Run("repo discovery", func(t *testing.T) {
		workspace := t.TempDir()
		for i := 0; i < chatEnvRepoLimit+1; i++ {
			initEmptyGitRepo(t, filepath.Join(workspace, fmt.Sprintf("repo%02d", i)))
		}
		manager, events := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
		t.Cleanup(func() { manager.Reset() })
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		if _, err := manager.NewSession(ctx, SessionOptions{CWD: workspace, TabID: "trunc-repo-tab", ChatID: "chat-trunc-repo"}); err != nil {
			t.Fatalf("new session: %v", err)
		}
		env := waitChatEnv(t, events, func(env ChatEnvPayload) bool {
			return env.ChatID == "chat-trunc-repo"
		}, fixtureTimeout)
		if !env.ReposTruncated || len(env.Unchanged) != chatEnvRepoLimit || len(env.Repos) != 0 {
			t.Fatalf("repo truncation env = %#v", env)
		}
	})

	t.Run("changed files", func(t *testing.T) {
		workspace := t.TempDir()
		repoDir := filepath.Join(workspace, "large")
		promptGate := filepath.Join(workspace, "prompt-gate")
		t.Cleanup(func() {
			_ = os.WriteFile(promptGate+".release", []byte("release\n"), 0o600)
		})
		files := map[string]string{}
		for i := 0; i < chatEnvFileLimit+1; i++ {
			files[fmt.Sprintf("file%03d.txt", i)] = "base\n"
		}
		initTinyGitRepo(t, repoDir, files)
		manager, events := newFakeManager(t, "slow-prompt", Options{
			Provider:          ProviderConfig{Env: map[string]string{"WORKASS_FAKE_ACP_PROMPT_GATE": promptGate}},
			RSSSampleInterval: time.Hour,
		})
		t.Cleanup(func() { manager.Reset() })
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		session, err := manager.NewSession(ctx, SessionOptions{CWD: repoDir, TabID: "trunc-file-tab", ChatID: "chat-trunc-file"})
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		_ = waitChatEnv(t, events, func(env ChatEnvPayload) bool {
			return env.ChatID == "chat-trunc-file" && len(env.Unchanged) == 1
		}, fixtureTimeout)
		job, err := manager.StartJob(context.Background(), JobStartOptions{
			Kind:      "app-chat",
			SessionID: session.SessionID,
			ChatID:    "chat-trunc-file",
			TabID:     "trunc-file-tab",
			CWD:       repoDir,
			Prompt:    "simulate many edits",
		})
		if err != nil {
			t.Fatalf("start job: %v", err)
		}
		waitForFakeACPProbeGate(t, promptGate)
		for i := 0; i < chatEnvFileLimit+1; i++ {
			path := filepath.Join(repoDir, fmt.Sprintf("file%03d.txt", i))
			if err := os.WriteFile(path, []byte("base\nchat\n"), 0o644); err != nil {
				t.Fatalf("edit %s: %v", path, err)
			}
		}
		if err := os.WriteFile(promptGate+".release", []byte("release\n"), 0o600); err != nil {
			t.Fatalf("release prompt gate: %v", err)
		}
		assertJobStatus(t, events.waitJobEnd(t, jobID(job), fixtureTimeout), "done", 0, "end_turn")
		env := waitChatEnv(t, events, func(env ChatEnvPayload) bool {
			return env.ChatID == "chat-trunc-file" && len(env.Repos) == 1 && env.Repos[0].FilesTruncated
		}, fixtureTimeout)
		repo := env.Repos[0]
		if !env.FilesTruncated || len(repo.Files) != chatEnvFileLimit || repo.Adds != chatEnvFileLimit+1 || repo.Dels != 0 {
			t.Fatalf("file truncation env = %#v", env)
		}
	})
}

func waitChatEnv(t *testing.T, events *eventCollector, pred func(ChatEnvPayload) bool, timeout time.Duration) ChatEnvPayload {
	t.Helper()
	ev := events.waitFor(t, timeout, func(ev collectedEvent) bool {
		if ev.channel != "chat:env" {
			return false
		}
		env, ok := ev.payload.(ChatEnvPayload)
		return ok && pred(env)
	})
	return ev.payload.(ChatEnvPayload)
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
}

func initEmptyGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runCommandFixture(t, "", "git", "init", dir)
	runGitFixture(t, dir, "checkout", "-b", "main")
}

func initTinyGitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	initEmptyGitRepo(t, dir)
	runGitFixture(t, dir, "config", "user.email", "workass-test@example.com")
	runGitFixture(t, dir, "config", "user.name", "Workass Test")
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir file parent: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
	}
	runGitFixture(t, dir, "add", ".")
	runGitFixture(t, dir, "commit", "-m", "init")
}

func runGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	runCommandFixture(t, dir, "git", append([]string{"-C", dir}, args...)...)
}

func runCommandFixture(t *testing.T, dir, command string, args ...string) {
	t.Helper()
	cmd := exec.Command(command, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", command, strings.Join(args, " "), err, out)
	}
}
