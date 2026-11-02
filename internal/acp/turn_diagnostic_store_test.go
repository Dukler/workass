package acp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const healthyDiagnosticStore = `{ "version": 1,
  "turns": [{"tabId":"t","chatId":"c","jobId":"j","started":"2026-11-01T00:00:00Z","elapsed":5,"stages":[0,0,0,0,0,0,0,0,0,0,0,0],"lastUpdate":0,"outcome":2,"runtime":{}}] }`

var diagnosticStoreOldTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func writeDiagnosticStore(t *testing.T, content []byte) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, turnDiagnosticFilename)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, diagnosticStoreOldTime, diagnosticStoreOldTime); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func assertDiagnosticStoreUntouched(t *testing.T, dir, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the store changed:\n got %q\nwant %q", got, want)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !stat.ModTime().Equal(diagnosticStoreOldTime) {
		t.Fatalf("the store was rewritten with the same bytes at %v", stat.ModTime())
	}
	if files, _ := os.ReadDir(dir); len(files) != 1 {
		t.Fatalf("expected the store alone, found %v", files)
	}
}

// The real manager, started and stopped with no turn in between: what a refused
// boot does. It writes nothing whatever it found.
func TestManagerStartedAndResetWritesNoDiagnostics(t *testing.T) {
	startManager := func(state string) *Manager {
		root := repoRoot(t)
		return NewManager(Options{RootDir: root, StateDir: state, DeferProviderStartup: true, RSSSampleInterval: time.Hour,
			Provider: ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root}})
	}
	for name, content := range map[string][]byte{
		"torn":    []byte(`{"version":1,"turns":[{"tabId":"torn"`),
		"healthy": []byte(healthyDiagnosticStore),
	} {
		t.Run(name, func(t *testing.T) {
			dir, path := writeDiagnosticStore(t, content)
			startManager(dir).Reset()
			assertDiagnosticStoreUntouched(t, dir, path, content)
		})
	}
	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		startManager(dir).Reset()
		if _, err := os.Lstat(filepath.Join(dir, turnDiagnosticFilename)); !os.IsNotExist(err) {
			t.Fatalf("a daemon that ran no turn created the store: %v", err)
		}
	})
}

// A store this build cannot read whole stays as it is even while real turns run.
func TestUnreadableDiagnosticStoreIsNeverReplaced(t *testing.T) {
	oversized := append(append([]byte(`{"version":1,"turns":[`), bytes.Repeat([]byte(" "), maxTurnDiagnosticStoreBytes)...), []byte(`]}`)...)
	oneBadRecord := []byte(strings.Replace(healthyDiagnosticStore, `[{"tabId"`, `[{"tabId":"bad"},{"tabId"`, 1))
	for _, tc := range []struct {
		name, want string
		content    []byte
	}{
		{"torn json", "invalid_store", []byte(`{"version":1,"turns":[{"tabId":"torn"`)},
		{"not json", "invalid_store", []byte("\x00\xff garbage")},
		{"another version", "invalid_store", []byte(`{"version":2,"turns":[],"future":true}`)},
		{"one bad record among good ones", "invalid_store", oneBadRecord},
		{"oversized", "unreadable_store", oversized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, path := writeDiagnosticStore(t, tc.content)
			m := &Manager{opts: Options{StateDir: dir}}
			m.loadTurnDiagnostics()
			job := diagnosticTestJob(m)
			job.startupTiming.finish("done")
			m.persistTurnDiagnostics(true)
			m.startDiagnosticWriter()
			m.queueTurnDiagnostics(true)
			m.stopDiagnosticWriter()
			assertDiagnosticStoreUntouched(t, dir, path, tc.content)
			if status := m.turnDiagnosticPersistence(); status["error"] != tc.want {
				t.Fatalf("status = %#v, want error %s", status, tc.want)
			}
			if turns := m.TurnDiagnostics("diag-tab", "diag-chat", 1)["turns"].([]any); len(turns) != 1 {
				t.Fatalf("the run lost its diagnostics in memory: %v", turns)
			}
		})
	}

	t.Run("a symlink is not followed or replaced", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(target, []byte(healthyDiagnosticStore), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, turnDiagnosticFilename)
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		m := &Manager{opts: Options{StateDir: dir}}
		m.loadTurnDiagnostics()
		diagnosticTestJob(m).startupTiming.finish("done")
		m.persistTurnDiagnostics(true)
		if stat, err := os.Lstat(path); err != nil || stat.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the link was replaced: %v %v", stat, err)
		}
	})

	t.Run("moving the store away restores persistence on the next start", func(t *testing.T) {
		dir, path := writeDiagnosticStore(t, []byte("torn"))
		if err := os.Rename(path, path+".saved"); err != nil {
			t.Fatal(err)
		}
		m := &Manager{opts: Options{StateDir: dir}}
		m.loadTurnDiagnostics()
		diagnosticTestJob(m).startupTiming.finish("done")
		m.persistTurnDiagnostics(true)
		if status := m.turnDiagnosticPersistence(); status["error"] != nil {
			t.Fatalf("status = %#v", status)
		}
		restarted := &Manager{opts: m.opts}
		restarted.loadTurnDiagnostics()
		if len(restarted.turnDiagnostics) != 1 {
			t.Fatalf("restored %d turns", len(restarted.turnDiagnostics))
		}
	})
}

// Skipping an unchanged flush must not cost a real one.
func TestHealthyDiagnosticStoreIsRewrittenOnlyWhenSomethingChanged(t *testing.T) {
	dir, path := writeDiagnosticStore(t, []byte(healthyDiagnosticStore))
	m := &Manager{opts: Options{StateDir: dir}}
	m.loadTurnDiagnostics()
	if len(m.turnDiagnostics) != 1 {
		t.Fatalf("fixture restored %d turns: %v", len(m.turnDiagnostics), m.turnDiagnosticPersistence())
	}
	m.persistTurnDiagnostics(true)
	assertDiagnosticStoreUntouched(t, dir, path, []byte(healthyDiagnosticStore))

	job := diagnosticTestJob(m)
	m.persistTurnDiagnostics(true) // Live: its elapsed time moves.
	job.startupTiming.finish("done")
	m.persistTurnDiagnostics(true)
	restarted := &Manager{opts: Options{StateDir: dir}}
	restarted.loadTurnDiagnostics()
	if len(restarted.turnDiagnostics) != 2 || restarted.turnDiagnostics[1].timing.outcome.Load() != job.startupTiming.outcome.Load() {
		t.Fatalf("restored %d turns after a real change", len(restarted.turnDiagnostics))
	}
	settled := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, settled, settled); err != nil {
		t.Fatal(err)
	}
	m.persistTurnDiagnostics(true)
	if stat, err := os.Stat(path); err != nil || !stat.ModTime().Equal(settled) {
		t.Fatalf("a finished turn was written twice: %v %v", stat, err)
	}
}
