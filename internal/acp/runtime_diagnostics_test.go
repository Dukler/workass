package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeDiagnosticsWriterDoesNotBlockProviderOrReads(t *testing.T) {
	m := &Manager{opts: Options{StateDir: t.TempDir()}}
	job := diagnosticTestJob(m)
	m.startDiagnosticWriter()
	m.diagnosticWriteMu.Lock() // A state volume that has stopped making progress.
	b := &Bridge{manager: m, jobsBySession: map[string]*Job{job.SessionID: job}}
	done := make(chan struct{})
	go func() {
		b.handleNotification("session/update", map[string]any{"sessionId": job.SessionID, "update": map[string]any{"sessionUpdate": "_workass_diagnostic", "schemaVersion": 1, "clientUserMessageId": "diag-input", "event": map[string]any{"kind": "error", "willRetry": true}}})
		m.TurnDiagnostics("diag-tab", "diag-chat", 1)
		// A following RPC reply uses the same reader; it must not await disk I/O.
		job.startupTiming.mark(startupTerminalReply)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		m.diagnosticWriteMu.Unlock()
		m.stopDiagnosticWriter()
		t.Fatal("diagnostic disk write blocked provider/read path")
	}
	m.diagnosticWriteMu.Unlock()
	job.startupTiming.finish("done")
	m.queueTurnDiagnostics(true)
	m.stopDiagnosticWriter()
	restarted := &Manager{opts: m.opts}
	restarted.loadTurnDiagnostics()
	if len(restarted.turnDiagnostics) != 1 || restarted.turnDiagnostics[0].timing.outcome.Load() != 2 {
		t.Fatal("writer shutdown lost completion")
	}
}

func TestRuntimeDiagnosticsAdmissionFailureFlushesOnReset(t *testing.T) {
	root := repoRoot(t)
	state := t.TempDir()
	m := NewManager(Options{RootDir: root, StateDir: state, Provider: ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root}})
	t.Cleanup(func() { m.Reset() })
	session := newMockSession(t, m, "admission-diag-tab")
	admissionCalled := false
	_, err := m.StartJob(context.Background(), JobStartOptions{Kind: "app-chat", SessionID: session.SessionID, TabID: "admission-diag-tab", ChatID: "chat-admission-diag-tab", Prompt: "PRIVATE_PROMPT", CommitAdmission: func(map[string]any) error { admissionCalled = true; return errors.New("PRIVATE_ADMISSION_ERROR") }})
	if err == nil || !admissionCalled {
		t.Fatal("fixture admission should fail")
	}
	m.Reset()
	restarted := &Manager{opts: Options{StateDir: state}}
	restarted.loadTurnDiagnostics()
	turns := restarted.TurnDiagnostics("admission-diag-tab", "chat-admission-diag-tab", 1)["turns"].([]any)
	if len(turns) != 1 || turns[0].(map[string]any)["outcome"] != "admission_failed" {
		t.Fatal(turns)
	}
}

func TestRuntimeDiagnosticsCrashStagingIsBounded(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".turn-diagnostics.pending")
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(staging, []byte("interrupted diagnostic write"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := &Manager{opts: Options{StateDir: root}}
		m.loadTurnDiagnostics()
		job := diagnosticTestJob(m)
		job.startupTiming.finish("done")
		m.persistTurnDiagnostics(true)
		if _, err := os.Stat(staging); !os.IsNotExist(err) {
			t.Fatal("stale staging artifact retained")
		}
		files, _ := os.ReadDir(root)
		if len(files) != 1 || files[0].Name() != turnDiagnosticFilename {
			t.Fatal("diagnostic files accumulated")
		}
	}
}

func TestRuntimeDiagnosticsCoalescedFailureGetsTrailingCheckpoint(t *testing.T) {
	m := &Manager{opts: Options{StateDir: t.TempDir()}}
	job := diagnosticTestJob(m)
	m.persistTurnDiagnostics(true)
	m.startDiagnosticWriter()
	defer m.stopDiagnosticWriter()
	for i := 0; i < 5; i++ {
		job.startupTiming.observeRuntimeDiagnostic(map[string]any{"kind": "error", "willRetry": true})
		m.queueTurnDiagnostics(false)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		restored := &Manager{opts: m.opts}
		restored.loadTurnDiagnostics()
		if len(restored.turnDiagnostics) == 1 && restored.turnDiagnostics[0].timing.runtime.Retries == 5 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("last coalesced error was never checkpointed without a later event")
}

func TestRuntimeDiagnosticsSeparatesHostTransportAndProviderError(t *testing.T) {
	for _, test := range []struct {
		err    error
		reason string
		reply  bool
	}{{io.EOF, "closed", false}, {context.Canceled, "cancelled", false}, {context.DeadlineExceeded, "deadline_exceeded", false}, {&acpError{Code: -32603, Msg: "PRIVATE_ERROR"}, "rpc_error", true}} {
		s := &turnStartupTiming{started: time.Now()}
		s.observeHostFailure(test.err)
		d := s.fields()["runtime"].(runtimeDiagnosticState)
		if d.HostErrors != 1 || d.Errors != 0 || d.Events[0]["reason"] != test.reason || d.Events[0]["rpcReply"] != test.reply {
			t.Fatal(d)
		}
		if (s.stages[startupTerminalReply].Load() != 0) != test.reply {
			t.Fatal("invented or omitted terminal reply")
		}
	}
}

func diagnosticTestJob(m *Manager) *Job {
	job := &Job{ID: "diag-job", TabID: "diag-tab", ChatID: "diag-chat", SessionID: "PRIVATE_NATIVE_SESSION", ProviderID: "mock", startupTiming: &turnStartupTiming{started: time.Now()}}
	job.startOpts.OperationID = "diag-input"
	m.retainTurnDiagnostic(job)
	return job
}

func TestRuntimeDiagnosticsBoundedContentFreeAndImmutable(t *testing.T) {
	s := &turnStartupTiming{started: time.Now()}
	s.observeRuntimeDiagnostic(map[string]any{"kind": "input", "inputBytes": 512, "textBytes": -1, "imageCount": math.NaN(), "imageDataBytes": 0.5, "hostInstanceId": "PRIVATE_NATIVE_SESSION", "historyMode": "https://private.example/password", "prompt": "PRIVATE_PROMPT"})
	for i := 0; i < 100; i++ {
		s.observeRuntimeDiagnostic(map[string]any{"kind": "error", "willRetry": true, "category": "stream_disconnected", "reason": "peer_closed", "httpStatus": 999, "message": "password=SECRET PRIVATE_PROMPT", "additionalDetails": "Bearer PRIVATE_NATIVE_SESSION"})
	}
	for i := 0; i < 100; i++ {
		s.observeRuntimeDiagnostic(map[string]any{"kind": "usage", "used": i, "size": 1000})
	}
	runtime := s.fields()["runtime"].(runtimeDiagnosticState)
	if runtime.Retries != 100 || runtime.Errors != 100 || len(runtime.Events) != maxRuntimeDiagnosticEvents || runtime.DroppedEvents != 69 || runtime.Usage["used"] != int64(99) {
		t.Fatal(runtime)
	}
	if runtime.Input["inputBytes"] != int64(512) || len(runtime.Input) != 4 {
		t.Fatal(runtime.Input)
	}
	encoded, _ := json.Marshal(runtime)
	for _, forbidden := range []string{"SECRET", "PRIVATE_", "password", "Bearer", "https:", "httpStatus", "textBytes", "imageCount", "imageDataBytes"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("diagnostic leaked %s", forbidden)
		}
	}
	runtime.Events[0]["reason"] = "MUTATED"
	runtime.Input["inputBytes"] = 0
	if s.fields()["runtime"].(runtimeDiagnosticState).Events[0]["reason"] != "peer_closed" {
		t.Fatal("read mutated retained evidence")
	}
	s.finish("done")
	s.observeRuntimeDiagnostic(map[string]any{"kind": "error", "willRetry": true})
	if s.fields()["runtime"].(runtimeDiagnosticState).Retries != 100 {
		t.Fatal("late error changed finished receipt")
	}
}

func TestRuntimeDiagnosticsExactTurnWithoutActivityOrConsumption(t *testing.T) {
	m := &Manager{}
	job := diagnosticTestJob(m)
	b := &Bridge{manager: m, jobsBySession: map[string]*Job{job.SessionID: job}}
	send := func(session, input string, version int) {
		b.handleNotification("session/update", map[string]any{"sessionId": session, "update": map[string]any{"sessionUpdate": "_workass_diagnostic", "schemaVersion": version, "clientUserMessageId": input, "event": map[string]any{"kind": "error", "willRetry": true, "category": "stream_disconnected"}}})
	}
	send("foreign-session", "diag-input", 1)
	send(job.SessionID, "old-input", 1)
	send(job.SessionID, "diag-input", 2)
	if job.startupTiming.runtime.Observed != 0 {
		t.Fatal("foreign diagnostic accepted")
	}
	send(job.SessionID, "diag-input", 1)
	if job.startupTiming.runtime.Retries != 1 {
		t.Fatal("exact diagnostic missing")
	}
	if job.startupTiming.lastUpdate.Load() != 0 || job.startupTiming.stages[startupUpdate].Load() != 0 || job.inputWasDispatched() {
		t.Fatal("diagnostic changed turn activity/admission")
	}
	next := diagnosticTestJob(&Manager{})
	next.startOpts.OperationID = "next-input"
	b.jobsBySession[job.SessionID] = next
	send(job.SessionID, "diag-input", 1)
	if next.startupTiming.runtime.Observed != 0 {
		t.Fatal("late event rehomed to next turn")
	}
}

func TestRuntimeDiagnosticsCheckpointSurvivesRestartWithoutInventedCompletion(t *testing.T) {
	root := t.TempDir()
	m := &Manager{opts: Options{StateDir: root}}
	job := diagnosticTestJob(m)
	job.startupTiming.mark(startupWritten)
	job.startupTiming.recordInput(map[string]int64{"preparedTextBytes": 3456, "requestTextBytes": 27})
	job.startupTiming.observeRuntimeDiagnostic(map[string]any{"kind": "input", "resumed": true, "resumeReplyBytes": 777, "hostInstanceId": "c577cf0c-342b-43b2-83bb-d52d7a434aac"})
	job.startupTiming.observeRuntimeDiagnostic(map[string]any{"kind": "error", "willRetry": true, "category": "stream_disconnected", "reason": "peer_closed"})
	m.persistTurnDiagnostics(false)
	name := filepath.Join(root, turnDiagnosticFilename)
	stat, err := os.Stat(name)
	if err != nil || stat.Size() > maxTurnDiagnosticStoreBytes || stat.Mode().Perm() != 0o600 {
		t.Fatalf("unsafe receipt: %v %v", stat, err)
	}
	restarted := &Manager{opts: Options{StateDir: root}}
	restarted.loadTurnDiagnostics()
	read := func() map[string]any {
		return restarted.TurnDiagnostics("diag-tab", "diag-chat", 1)["turns"].([]any)[0].(map[string]any)
	}
	d := read()
	if d["active"] != false || d["historical"] != true || d["phase"] != "previous_daemon_observation" || d["outcome"] != nil || d["finishedMs"] != nil {
		t.Fatal(d)
	}
	if d["runtime"].(runtimeDiagnosticState).Retries != 1 || d["input"].(map[string]int64)["preparedTextBytes"] != 3456 {
		t.Fatal(d)
	}
	elapsed := d["managerElapsedMs"]
	time.Sleep(2 * time.Millisecond)
	if read()["managerElapsedMs"] != elapsed {
		t.Fatal("historical timing continued to advance")
	}
	if restarted.TurnDiagnostics("other", "diag-chat", 1)["available"] != false {
		t.Fatal("persisted target fence failed")
	}
	job.startupTiming.finish("done")
	m.persistTurnDiagnostics(true)
	restarted = &Manager{opts: Options{StateDir: root}}
	restarted.loadTurnDiagnostics()
	if read()["outcome"] != "completed" || read()["phase"] != "finished" {
		t.Fatal(read())
	}
	data, _ := os.ReadFile(name)
	if strings.Contains(string(data), job.SessionID) || strings.Contains(string(data), "diag-input") {
		t.Fatal("native identity or input correlation leaked to disk")
	}
}

func TestRuntimeDiagnosticsConcurrentCheckpointAndReaders(t *testing.T) {
	m := &Manager{opts: Options{StateDir: t.TempDir()}}
	job := diagnosticTestJob(m)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				job.startupTiming.observeRuntimeDiagnostic(map[string]any{"kind": "error", "willRetry": true, "reason": "peer_closed"})
				m.persistTurnDiagnostics(false)
				m.TurnDiagnostics("diag-tab", "diag-chat", 1)
			}
		}()
	}
	wg.Wait()
	job.startupTiming.finish("failed")
	m.persistTurnDiagnostics(true)
	restarted := &Manager{opts: m.opts}
	restarted.loadTurnDiagnostics()
	if got := restarted.turnDiagnostics[0].timing.runtime.Retries; got != 160 {
		t.Fatalf("lost retries: %d", got)
	}
}

func TestRuntimeDiagnosticsRejectUnsafeStoreAndExposeWriteFailure(t *testing.T) {
	for _, kind := range []string{"oversized", "symlink", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			name := filepath.Join(root, turnDiagnosticFilename)
			switch kind {
			case "oversized":
				f, _ := os.Create(name)
				if err := f.Truncate(maxTurnDiagnosticStoreBytes + 1); err != nil {
					t.Fatal(err)
				}
				f.Close()
			case "symlink":
				target := filepath.Join(root, "target")
				os.WriteFile(target, []byte(`{"version":1,"turns":[]}`), 0o600)
				if err := os.Symlink(target, name); err != nil {
					t.Skip("symlinks unavailable")
				}
			case "invalid":
				os.WriteFile(name, []byte(`{"version":99,"turns":[]}`), 0o600)
			}
			m := &Manager{opts: Options{StateDir: root}}
			m.loadTurnDiagnostics()
			if len(m.turnDiagnostics) != 0 || m.turnDiagnosticPersistence()["error"] == nil {
				t.Fatal("unsafe store accepted")
			}
		})
	}
	root := filepath.Join(t.TempDir(), "file")
	os.WriteFile(root, []byte("not a directory"), 0o600)
	m := &Manager{opts: Options{StateDir: root}}
	job := diagnosticTestJob(m)
	job.startupTiming.finish("failed")
	m.persistTurnDiagnostics(true)
	if m.turnDiagnosticPersistence()["error"] != "write_failed" || m.TurnDiagnostics("diag-tab", "diag-chat", 1)["available"] != true {
		t.Fatal("write failure hid live evidence")
	}
}
