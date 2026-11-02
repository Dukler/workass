package main

import (
	"bytes"
	"context"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeSourceCheckout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "cmd", "workass")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveDaemonStateDir(t *testing.T) {
	checkout := fakeSourceCheckout(t)
	plainDir := t.TempDir()
	root := t.TempDir()
	home := t.TempDir()
	installed := filepath.Join(home, "Library", "Application Support", "Workass")
	if err := os.MkdirAll(installed, 0o755); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(t.TempDir(), "state")

	cases := []struct {
		name      string
		req       daemonStateDirRequest
		want      string
		wantError string
	}{
		{name: "the test root implies the state directory", req: daemonStateDirRequest{Profile: "test", TestRoot: root, CWD: checkout, Flag: "state"}, want: filepath.Join(root, "state")},
		{name: "a state directory inside the test root is kept", req: daemonStateDirRequest{Profile: "test", TestRoot: root, CWD: checkout, Flag: filepath.Join(root, "a", "state"), FlagSet: true}, want: filepath.Join(root, "a", "state")},
		{name: "a test daemon cannot name the checkout state", req: daemonStateDirRequest{Profile: "test", TestRoot: root, CWD: checkout, Flag: "state", FlagSet: true}, wantError: "outside"},
		{name: "a state directory equal to the root would write above it", req: daemonStateDirRequest{Profile: "test", TestRoot: root, CWD: checkout, Flag: root, FlagSet: true}, wantError: "outside"},
		{name: "the test profile requires a root or a directory", req: daemonStateDirRequest{Profile: "test", CWD: checkout}, wantError: "requires WORKASS_TEST_ROOT"},
		{name: "the canary's explicit scratch state without a root is kept", req: daemonStateDirRequest{Profile: "test", CWD: checkout, Flag: explicit, FlagSet: true}, want: explicit},
		{name: "a relative test root is refused", req: daemonStateDirRequest{Profile: "test", TestRoot: "scratch", CWD: plainDir}, wantError: "must be absolute"},
		{name: "a source checkout is not a test root", req: daemonStateDirRequest{Profile: "test", TestRoot: checkout, CWD: plainDir}, wantError: "source checkout"},
		{name: "the home directory is not a test root", req: daemonStateDirRequest{Profile: "test", TestRoot: home, Home: home, CWD: plainDir}, wantError: "holds the home directory"},
		{name: "installed data is not a test root", req: daemonStateDirRequest{Profile: "test", TestRoot: installed, ProtectedRoots: []string{installed}, CWD: plainDir}, wantError: "overlaps"},
		{name: "a directory inside installed data is not a test root", req: daemonStateDirRequest{Profile: "test", TestRoot: filepath.Join(installed, "t"), ProtectedRoots: []string{installed}, CWD: plainDir}, wantError: "overlaps"},
		{name: "a test root without the test profile is refused", req: daemonStateDirRequest{Profile: "prod", TestRoot: root, CWD: plainDir}, wantError: "WORKASS_PROFILE"},
		{name: "a stray test root does not move an explicit production state", req: daemonStateDirRequest{Profile: "prod", TestRoot: root, CWD: plainDir, Flag: explicit, FlagSet: true}, want: explicit},
		{name: "the dev profile has no default", req: daemonStateDirRequest{Profile: "dev", CWD: plainDir}, wantError: "no default state directory"},
		{name: "the dev profile takes an explicit directory", req: daemonStateDirRequest{Profile: "dev", CWD: checkout, Flag: explicit, FlagSet: true}, want: explicit},
		{name: "no profile in a checkout is refused", req: daemonStateDirRequest{Profile: "prod", CWD: checkout}, wantError: "source checkout"},
		{name: "an empty flag is no choice at all", req: daemonStateDirRequest{Profile: "prod", CWD: checkout, Flag: " ", FlagSet: true}, wantError: "source checkout"},
		{name: "a deliberate relative directory in a checkout is kept", req: daemonStateDirRequest{Profile: "prod", CWD: checkout, Flag: "scratch/state", FlagSet: true}, want: filepath.Join(checkout, "scratch", "state")},
		{name: "an unpacked release keeps its default", req: daemonStateDirRequest{Profile: "prod", CWD: plainDir}, want: filepath.Join(plainDir, "state")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDaemonStateDir(tc.req)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("got %q, %v; want an error containing %q", got, err, tc.wantError)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestStateDirInsideRootFollowsSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "real-root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !pathStrictlyInside(root, filepath.Join(alias, "not-yet", "state")) {
		t.Fatal("a path through a symlink to the root must count as inside it")
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(base, escape); err != nil {
		t.Fatal(err)
	}
	if pathStrictlyInside(root, filepath.Join(escape, "state")) {
		t.Fatal("a symlink out of the root must not count as inside it")
	}
	got, err := resolveDaemonStateDir(daemonStateDirRequest{Profile: "test", TestRoot: root, CWD: base, Flag: filepath.Join(escape, "state"), FlagSet: true})
	if err == nil {
		t.Fatalf("a state directory through an escaping symlink was accepted: %q", got)
	}
}

func TestApplyTestRootEnvDropsInheritedInstalledPaths(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(t.TempDir(), "Workass")
	t.Setenv(dataRootEnvVar, real)
	t.Setenv("WORKASS_LOG_ROOT", filepath.Join(real, "logs"))
	t.Setenv("WORKASS_BROWSER_CONTROL_FILE", filepath.Join(real, "run", "browser-control.json"))
	t.Setenv("WORKASS_TOOL_CONTEXT", filepath.Join(real, "state", "context.json"))
	t.Setenv("WORKASS_INSTRUCTIONS_FILE", filepath.Join(real, "state", "instructions.md"))
	t.Setenv("WORKASS_TOOLS_GUIDE", filepath.Join(real, "state", "guide.md"))
	t.Setenv("WORKASS_RUN_TOKEN_FILE", filepath.Join(root, "own-token"))
	t.Setenv("HOME", real)
	t.Setenv("USERPROFILE", real)
	t.Setenv("APPDATA", real)
	t.Setenv("LOCALAPPDATA", real)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(real, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(real, ".codex"))
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, real)
	}
	if err := applyTestRootEnv(root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if got := os.Getenv(name); !pathStrictlyInside(root, got) {
			t.Fatalf("%s = %q, outside the test root", name, got)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "home")); err != nil || !info.IsDir() {
		t.Fatalf("the test home does not exist: %v", err)
	}
	if got := os.Getenv(dataRootEnvVar); got != root {
		t.Fatalf("data root = %q", got)
	}
	if got := os.Getenv("WORKASS_LOG_ROOT"); got != filepath.Join(root, "logs") {
		t.Fatalf("log root = %q", got)
	}
	if got := os.Getenv("WORKASS_BROWSER_CONTROL_FILE"); got != filepath.Join(root, "run", "browser-control.json") {
		t.Fatalf("browser control file = %q", got)
	}
	for _, name := range []string{"WORKASS_TOOL_CONTEXT", "WORKASS_INSTRUCTIONS_FILE", "WORKASS_TOOLS_GUIDE", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		if got := os.Getenv(name); got != "" {
			t.Fatalf("%s kept the inherited %q", name, got)
		}
	}
	if got := os.Getenv("WORKASS_RUN_TOKEN_FILE"); got != filepath.Join(root, "own-token") {
		t.Fatalf("a credential inside the root was dropped: %q", got)
	}
}

func TestFleetAndRunUseTheTestRootOverAnInheritedDataRoot(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	t.Setenv(profileEnvVar, "test")
	t.Setenv(testRootEnvVar, root)
	t.Setenv(dataRootEnvVar, real)
	t.Setenv("WORKASS_TOOL_CONTEXT", "")
	t.Setenv("WORKASS_RUN_TOKEN_FILE", "")
	if got := defaultFleetStateDir(); got != filepath.Join(root, "state") {
		t.Fatalf("fleet state dir = %q", got)
	}
	var stderr bytes.Buffer
	stateDir, _, err := resolveRunContext("", "", true, &stderr)
	if err != nil || stateDir != filepath.Join(root, "state") {
		t.Fatalf("run state dir = %q, %v", stateDir, err)
	}
}

type treeEntry struct {
	mode    fs.FileMode
	size    int64
	modTime time.Time
}

func snapshotTree(t *testing.T, dir string) map[string]treeEntry {
	t.Helper()
	out := map[string]treeEntry{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[path] = treeEntry{mode: info.Mode(), size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTreeUnchanged(t *testing.T, before, after map[string]treeEntry) {
	t.Helper()
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("removed: %s", path)
		} else if got != want {
			t.Errorf("rewritten: %s (%v -> %v)", path, want, got)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("created: %s", path)
		}
	}
}

func daemonHelperCommand(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	command := append([]string{"-test.run=^TestDaemonStateStaysUnderTestRoot$", "--"}, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], command...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "WORKASS_STATE_DIR_DAEMON_HELPER=1", "PATH=/nonexistent")
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

// A real daemon start, from a checkout whose git-ignored state/ is stale: the
// shape of the 2026-11-01 report. The seeded directory is a fixture and the rule
// under test is that nothing in the working directory moves.
func TestDaemonStateStaysUnderTestRoot(t *testing.T) {
	if os.Getenv("WORKASS_STATE_DIR_DAEMON_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
				main()
				os.Exit(0)
			}
		}
		os.Exit(99)
	}
	if runtime.GOOS == "windows" {
		t.Skip("stops the daemon with SIGTERM")
	}
	checkout := fakeSourceCheckout(t)
	stale := filepath.Join(checkout, "state")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "turn-diagnostics.json"), []byte(`{"version":99,"turns":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "providers.json"), []byte(`{"not":"this one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	real := t.TempDir()
	root := t.TempDir()
	before, realBefore := snapshotTree(t, checkout), snapshotTree(t, real)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := daemonHelperCommand(ctx, checkout, []string{
		"WORKASS_PROFILE=test", "WORKASS_TEST_ROOT=" + root,
		// What a Workass agent's shell passes down.
		"WORKASS_DATA_ROOT=" + real, "WORKASS_LOG_ROOT=" + filepath.Join(real, "logs"),
		"WORKASS_BROWSER_CONTROL_FILE=" + filepath.Join(real, "run", "browser-control.json"),
		"HOME=" + real, "CODEX_HOME=" + filepath.Join(real, ".codex"),
	},
		// A stand-in for a provider CLI: what it writes under $HOME is what a real
		// one writes during detection.
		"--port", strconv.Itoa(port), "--beacon=false", "--hibernate-ttl=1h",
		"--acp-command", "/bin/sh", "--acp-args", `["-c","echo probe > \"$HOME/provider-probe\""]`)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		select {
		case waitErr := <-exited:
			t.Fatalf("the daemon stopped before listening: %v\n%s", waitErr, logs.String())
		case <-time.After(25 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never listened\n%s", logs.String())
		}
	}
	probe := filepath.Join(root, "home", "provider-probe")
	for {
		if _, statErr := os.Stat(probe); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("the provider stand-in never wrote %s", probe)
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatalf("the daemon did not stop\n%s", logs.String())
	}

	assertTreeUnchanged(t, before, snapshotTree(t, checkout))
	assertTreeUnchanged(t, realBefore, snapshotTree(t, real))
	for _, name := range []string{"state/machine-id.json", "app-config.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("expected %s under the test root: %v", name, err)
		}
	}
	if t.Failed() {
		t.Logf("daemon log:\n%s", logs.String())
	}
}

func TestDaemonRefusalWritesNothing(t *testing.T) {
	checkout := fakeSourceCheckout(t)
	before := snapshotTree(t, checkout)
	for _, env := range [][]string{
		{"WORKASS_PROFILE=prod", "WORKASS_TEST_ROOT="},
		{"WORKASS_PROFILE=dev", "WORKASS_TEST_ROOT="},
		{"WORKASS_PROFILE=test", "WORKASS_TEST_ROOT="},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := daemonHelperCommand(ctx, checkout, env, "--port", "1", "--beacon=false")
		output, err := cmd.CombinedOutput()
		cancel()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 2 {
			t.Fatalf("%v: err=%v output=%s", env, err, output)
		}
		assertTreeUnchanged(t, before, snapshotTree(t, checkout))
	}
}
