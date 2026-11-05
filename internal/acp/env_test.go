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

func copyFixtureTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := dst
		if rel != "." {
			target = filepath.Join(dst, rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if info.IsDir() {
			if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported fixture file mode %s: %s", info.Mode(), path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chmod(target, info.Mode().Perm())
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
	template := t.TempDir()
	runCommandFixture(t, "", "git", "init", "--template="+template, "--initial-branch=main", dir)
}

// uniformLargeGitFixture is opt-in for fixtures whose every tracked file is
// deliberately identical and always mutated together. Its shared content
// file lives outside the repository; unsupported hardlinks use ordinary files.
type uniformLargeGitFixture struct {
	shared   string
	paths    []string
	hardlink bool
}

func initUniformLargeGitRepo(t *testing.T, dir string, count int) uniformLargeGitFixture {
	t.Helper()
	initEmptyGitRepo(t, dir)
	runGitFixture(t, dir, "config", "user.email", "workass-test@example.com")
	runGitFixture(t, dir, "config", "user.name", "Workass Test")
	shared := filepath.Join(filepath.Dir(dir), "uniform-fixture-content")
	if err := os.WriteFile(shared, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write shared fixture content: %v", err)
	}
	fixture := uniformLargeGitFixture{shared: shared, hardlink: true}
	for i := 0; i < count; i++ {
		path := filepath.Join(dir, fmt.Sprintf("file%03d.txt", i))
		if err := os.Link(shared, path); err != nil {
			fixture.hardlink = false
		}
		fixture.paths = append(fixture.paths, path)
		if !fixture.hardlink {
			break
		}
	}
	if !fixture.hardlink {
		for _, path := range fixture.paths {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatalf("remove partial hardlink %s: %v", path, err)
			}
		}
		fixture.paths = fixture.paths[:0]
		for i := 0; i < count; i++ {
			path := filepath.Join(dir, fmt.Sprintf("file%03d.txt", i))
			if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
				t.Fatalf("write fallback fixture file %s: %v", path, err)
			}
			fixture.paths = append(fixture.paths, path)
		}
	}
	runGitFixture(t, dir, "add", ".")
	runGitFixture(t, dir, "commit", "-m", "init")
	return fixture
}

func (fixture uniformLargeGitFixture) writeAll(t *testing.T, content string) {
	t.Helper()
	if fixture.hardlink {
		if err := os.WriteFile(fixture.shared, []byte(content), 0o644); err != nil {
			t.Fatalf("mutate shared fixture content: %v", err)
		}
		return
	}
	for _, path := range fixture.paths {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("mutate fixture file %s: %v", path, err)
		}
	}
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

// Seed historical snapshot data explicitly. Production chat lifecycle no longer
// creates it; legacy diff/rewind and persisted-data compatibility remain tested.
func seedLegacyChatEnvFixture(t *testing.T, manager *Manager, sessionID, chatID, tabID, cwd string) {
	t.Helper()
	repos, truncated := discoverChatEnvRepos(context.Background(), cwd)
	tracker := &chatEnvTracker{sessionID: sessionID, chatID: chatID, tabID: tabID, cwd: cwd, repos: repos, reposTruncated: truncated}
	tracker.payload = tracker.initialPayload()
	manager.envMu.Lock()
	manager.storeChatEnvTrackerLocked(tracker)
	manager.envMu.Unlock()
	manager.emit("chat:env", cloneChatEnvPayload(tracker.payload))
}

func beginLegacyCheckpointFixture(manager *Manager, public map[string]any, sessionID, chatID, tabID, cwd string) *Job {
	job := &Job{ID: jobID(public), SessionID: sessionID, ChatID: chatID, TabID: tabID, CWD: cwd}
	manager.beginChatTurnCheckpoint(context.Background(), job)
	return job
}
