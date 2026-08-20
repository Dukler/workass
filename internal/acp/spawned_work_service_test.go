package acp

import (
	"os"
	"testing"
	"time"
)

// seedBackgroundServiceBash reproduces the exact path a dev server takes into
// the registry today: Claude runs it with run_in_background, so it arrives as
// passively observed background Bash with an output file and no declaration.
func seedBackgroundServiceBash(t *testing.T, manager *Manager, taskID, output, command string) {
	t.Helper()
	observeRawSpawnToolFixture(t, manager, spawnToolObservation{
		SessionID: "session-1", TabID: "tab-1", ChatID: "chat-1", ProviderID: "claude", ToolCallID: "tool-" + taskID,
		Title: "Bash", Command: command,
	}, providerRawToolObservation{
		Title: "Bash", Command: command, RawInput: map[string]any{"run_in_background": true},
		Meta: map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}},
	})
	observeRawSpawnToolFixture(t, manager, spawnToolObservation{
		SessionID: "session-1", TabID: "tab-1", ChatID: "chat-1", ProviderID: "claude", ToolCallID: "tool-" + taskID,
		Title: "Bash",
	}, providerRawToolObservation{
		Title: "Bash", Meta: map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}},
		Output: "Command running in background with ID: " + taskID + "\nOutput is being written to: " + output,
	})
}

func serviceProbeManager(t *testing.T, output string, listening *bool) *Manager {
	t.Helper()
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) {
			return map[string][]int{output: {5150}}, true
		},
		SpawnedWorkListenProbe: func(pids []int) (map[int]bool, bool) {
			out := make(map[int]bool, len(pids))
			for _, pid := range pids {
				out[pid] = *listening
			}
			return out, true
		},
	})
	t.Cleanup(func() { manager.Reset() })
	return manager
}

func appendServiceOutput(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("still building\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func backdateServiceDwell(t *testing.T, manager *Manager, taskID string) {
	t.Helper()
	manager.spawnedWorkMu.Lock()
	defer manager.spawnedWorkMu.Unlock()
	rec := manager.spawnedWork[spawnedWorkKey("tab-1", "chat-1", taskID)]
	if rec == nil {
		t.Fatalf("no record for %q", taskID)
	}
	if rec.ListeningQuietSince.IsZero() {
		t.Fatalf("%q has no dwell clock running; nothing to backdate", taskID)
	}
	rec.ListeningQuietSince = rec.ListeningQuietSince.Add(-serviceClassifyDwell - time.Second)
}

func roleOf(t *testing.T, manager *Manager, taskID string) string {
	t.Helper()
	for _, item := range manager.ListSpawnedWork("tab-1", "chat-1") {
		if item.TaskID == taskID {
			if item.Status != "running" {
				t.Fatalf("%q settled unexpectedly: %#v", taskID, item)
			}
			return item.Role
		}
	}
	t.Fatalf("no item for %q", taskID)
	return ""
}

func TestQuietListenerBecomesAServiceAndStopsReportingTheChatBusy(t *testing.T) {
	listening := true
	output := spawnedWorkTestOutput(t, "bash-expo")
	manager := serviceProbeManager(t, output, &listening)
	seedBackgroundServiceBash(t, manager, "bash-expo", output, "npx expo start")

	if !manager.hasRunningSpawnedWorkForChat("tab-1", "chat-1") {
		t.Fatal("a freshly spawned background process must count as work until it is classified")
	}
	// One pass records a size, the next has something to compare it against.
	manager.reconcileSpawnedWork()
	manager.reconcileSpawnedWork()
	if role := roleOf(t, manager, "bash-expo"); role != "" {
		t.Fatalf("classified before the dwell elapsed: role = %q", role)
	}

	backdateServiceDwell(t, manager, "bash-expo")
	manager.reconcileSpawnedWork()
	if role := roleOf(t, manager, "bash-expo"); role != spawnedWorkRoleService {
		t.Fatalf("a quiet listener held past the dwell must be a service: role = %q", role)
	}
	if manager.hasRunningSpawnedWorkForChat("tab-1", "chat-1") {
		t.Fatal("a service must not pin the chat's engine awake")
	}

	// Sticky: a server logging a request is a server doing its job, not a chat
	// that went back to work.
	appendServiceOutput(t, output)
	manager.reconcileSpawnedWork()
	manager.reconcileSpawnedWork()
	if role := roleOf(t, manager, "bash-expo"); role != spawnedWorkRoleService {
		t.Fatalf("service classification must be one-way: role = %q", role)
	}
}

func TestListenerInAChildProcessStillClassifiesTheLane(t *testing.T) {
	output := spawnedWorkTestOutput(t, "bash-npx")
	// `npx expo start`: the wrapper that owns the record binds nothing, and the
	// node child that inherited its stdout is the one holding the port. Only the
	// wrapper's own PID would miss this entirely.
	const wrapper, child = 4001, 4000
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) {
			return map[string][]int{output: {child, wrapper}}, true
		},
		SpawnedWorkListenProbe: func(pids []int) (map[int]bool, bool) {
			out := make(map[int]bool, len(pids))
			for _, pid := range pids {
				out[pid] = pid == child
			}
			return out, true
		},
	})
	t.Cleanup(func() { manager.Reset() })
	seedBackgroundServiceBash(t, manager, "bash-npx", output, "npx expo start")

	manager.reconcileSpawnedWork()
	manager.reconcileSpawnedWork()
	for _, item := range manager.ListSpawnedWork("tab-1", "chat-1") {
		if item.PID == nil || *item.PID != wrapper {
			t.Fatalf("test premise broken: the record should hold the non-listening pid, got %#v", item.PID)
		}
	}
	backdateServiceDwell(t, manager, "bash-npx")
	manager.reconcileSpawnedWork()
	if role := roleOf(t, manager, "bash-npx"); role != spawnedWorkRoleService {
		t.Fatalf("a lane whose child holds the port is a service: role = %q", role)
	}
}

func TestListenerThatKeepsWritingStaysWork(t *testing.T) {
	listening := true
	output := spawnedWorkTestOutput(t, "bash-tests")
	manager := serviceProbeManager(t, output, &listening)
	// `go test ./internal/httpserve` binds a listener for its whole run. Only the
	// output it keeps writing separates it from a booted server.
	seedBackgroundServiceBash(t, manager, "bash-tests", output, "go test ./internal/httpserve")

	for i := 0; i < 4; i++ {
		manager.reconcileSpawnedWork()
		appendServiceOutput(t, output)
	}
	if role := roleOf(t, manager, "bash-tests"); role != "" {
		t.Fatalf("a listener that never stops writing is work: role = %q", role)
	}
	if !manager.hasRunningSpawnedWorkForChat("tab-1", "chat-1") {
		t.Fatal("a running test suite must keep reporting the chat busy")
	}
}

func TestQuietProcessWithoutAListenerStaysWork(t *testing.T) {
	listening := false
	output := spawnedWorkTestOutput(t, "bash-link")
	manager := serviceProbeManager(t, output, &listening)
	// A long link step is silent for minutes without being a server.
	seedBackgroundServiceBash(t, manager, "bash-link", output, "cargo build --release")

	manager.reconcileSpawnedWork()
	manager.reconcileSpawnedWork()
	manager.spawnedWorkMu.Lock()
	rec := manager.spawnedWork[spawnedWorkKey("tab-1", "chat-1", "bash-link")]
	dwellRunning := rec != nil && !rec.ListeningQuietSince.IsZero()
	manager.spawnedWorkMu.Unlock()
	if dwellRunning {
		t.Fatal("silence alone must not start the service dwell")
	}
	if role := roleOf(t, manager, "bash-link"); role != "" {
		t.Fatalf("a quiet non-listener is work: role = %q", role)
	}
}

func TestFailedListenProbeNeverClassifies(t *testing.T) {
	output := spawnedWorkTestOutput(t, "bash-unprobed")
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) {
			return map[string][]int{output: {5150}}, true
		},
		SpawnedWorkListenProbe: func([]int) (map[int]bool, bool) { return nil, false },
	})
	t.Cleanup(func() { manager.Reset() })
	seedBackgroundServiceBash(t, manager, "bash-unprobed", output, "npx expo start")

	for i := 0; i < 4; i++ {
		manager.reconcileSpawnedWork()
	}
	if role := roleOf(t, manager, "bash-unprobed"); role != "" {
		t.Fatalf("an unavailable probe is not evidence: role = %q", role)
	}
}

func TestDeclaredWorkIsNeverReclassifiedByInference(t *testing.T) {
	stateDir := t.TempDir()
	pid := 5150
	manager := NewManager(Options{
		StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour,
		// A soak lane that runs a listener and then falls silent looks exactly
		// like a booted server to the probes. The declaration must win.
		SpawnedWorkListenProbe: func(pids []int) (map[int]bool, bool) {
			out := make(map[int]bool, len(pids))
			for _, p := range pids {
				out[p] = true
			}
			return out, true
		},
	})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-ext", "chat-ext", "tab-ext", "mock")

	if _, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-ext", ParentChatID: "chat-ext", ParentTabID: "tab-ext",
		Label: "soak lane", Role: "work", PID: &pid,
	}); err != nil {
		t.Fatalf("register declared work: %v", err)
	}

	for i := 0; i < 4; i++ {
		manager.reconcileSpawnedWork()
	}
	items := manager.ListSpawnedWork("tab-ext", "chat-ext")
	if len(items) != 1 || items[0].Role != spawnedWorkRoleWork {
		t.Fatalf("declared work must survive a quiet listener verdict: %#v", items)
	}
	manager.spawnedWorkMu.Lock()
	rec := manager.spawnedWork[spawnedWorkKey("tab-ext", "chat-ext", items[0].TaskID)]
	dwellRunning := rec != nil && !rec.ListeningQuietSince.IsZero()
	manager.spawnedWorkMu.Unlock()
	if dwellRunning {
		t.Fatal("a declared record must not even accumulate classification evidence")
	}
}

func TestDeclaredServiceIsCarriedFromRegistrationAndSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-ext", "chat-ext", "tab-ext", "mock")

	if _, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-ext", ParentChatID: "chat-ext", ParentTabID: "tab-ext",
		Label: "expo start", Role: "service",
	}); err != nil {
		t.Fatalf("register declared service: %v", err)
	}
	if _, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-ext", ParentChatID: "chat-ext", ParentTabID: "tab-ext",
		Label: "build lane",
	}); err != nil {
		t.Fatalf("register lane: %v", err)
	}

	roles := map[string]string{}
	for _, item := range manager.ListSpawnedWork("tab-ext", "chat-ext") {
		roles[item.Label] = item.Role
	}
	if roles["expo start"] != spawnedWorkRoleService || roles["build lane"] != "" {
		t.Fatalf("declared roles = %#v", roles)
	}

	// A restart reloads from the snapshot; the declaration must not be lost, or
	// every daemon restart resurrects the false working state.
	reloaded := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { reloaded.Reset() })
	roles = map[string]string{}
	for _, item := range reloaded.ListSpawnedWork("tab-ext", "chat-ext") {
		roles[item.Label] = item.Role
	}
	if roles["expo start"] != spawnedWorkRoleService || roles["build lane"] != "" {
		t.Fatalf("roles after restart = %#v", roles)
	}
}

// The registered-lane shape, which is how a dev server actually reaches the
// registry when an agent launches it: kind "external", no PID on the record,
// the port held by a grandchild of the wrapper. This shipped broken — external
// lanes were excluded from the PID-by-path probe, so the very case service
// classification exists for could never become a candidate.
//
// The probe stub answers only for paths it was actually asked about. The
// earlier stubs returned their map unconditionally, which answered for a path
// production never requested and hid the gap.
func TestRegisteredExternalServiceLaneIsClassifiedFromItsListeningChild(t *testing.T) {
	const child = 7100
	asked := [][]string{}
	manager := NewManager(Options{
		StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func(paths []string) (map[string][]int, bool) {
			asked = append(asked, append([]string(nil), paths...))
			out := map[string][]int{}
			for _, path := range paths {
				out[path] = []int{child}
			}
			return out, true
		},
		SpawnedWorkListenProbe: func(pids []int) (map[int]bool, bool) {
			out := make(map[int]bool, len(pids))
			for _, pid := range pids {
				out[pid] = pid == child
			}
			return out, true
		},
	})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-ext", "chat-1", "tab-1", "claude")

	result, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-ext", ParentChatID: "chat-1", ParentTabID: "tab-1", Label: "npx expo start --host lan",
	})
	if err != nil {
		t.Fatalf("register external lane: %v", err)
	}
	workID, output := result["workId"].(string), result["outputFile"].(string)
	// External lanes never pinned the bridge (spawned_work.go:877), so the
	// consumer that matters here is park evidence: an unclassified lane is
	// something that will resume the chat, a dev server never is.
	if !manager.chatHasLiveParkEvidence("tab-1", "chat-1") {
		t.Fatal("a freshly registered lane must count as work until it is classified")
	}

	manager.reconcileSpawnedWork()
	manager.reconcileSpawnedWork()
	probed := false
	for _, paths := range asked {
		for _, path := range paths {
			if path == output {
				probed = true
			}
		}
	}
	if !probed {
		t.Fatalf("the lane's output file was never probed; asked for %#v", asked)
	}
	backdateServiceDwell(t, manager, workID)
	manager.reconcileSpawnedWork()

	if role := roleOf(t, manager, workID); role != spawnedWorkRoleService {
		t.Fatalf("a quiet registered lane whose child holds a port is a service: role = %q", role)
	}
	if manager.chatHasLiveParkEvidence("tab-1", "chat-1") {
		t.Fatal("a dev server owes the chat nothing and must not hold it parked")
	}
}
