//go:build !windows

package acp

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"workass/internal/procchild"
)

// The two gates that cannot be simulated, run against real processes:
//
//   - "killing the supervisor yields lost, never running" (§13 P4). A dead pid
//     with no settled event is the whole evidence model, and faking the pid
//     would test the fake.
//   - "child survives a daemon restart AND a self-update". The child is not a
//     daemon child, so this is a claim about process parentage: the only way to
//     show it is to replace the daemon while a real process keeps running.
//
// The supervisor runs in a re-exec of this test binary, which is the standard
// way to get a real external process out of a Go test. It is a real
// procchild.Run — the same code `workass run` calls — so what is proven here is
// the product's own supervisor, not a stand-in.

const supervisorHelperEnv = "WORKASS_TEST_SUPERVISOR_SPEC"

// TestProcessChildSupervisorHelper is the re-exec entry point, not a test. It
// runs only when the parent has asked for it.
func TestProcessChildSupervisorHelper(t *testing.T) {
	spec := strings.TrimSpace(os.Getenv(supervisorHelperEnv))
	if spec == "" {
		t.Skip("re-exec helper: the supervisor runs only when the parent asks for it")
	}
	parts := strings.SplitN(spec, "\n", 3)
	if len(parts) != 3 {
		t.Fatalf("helper spec = %q", spec)
	}
	result, err := procchild.Run(procchild.Spec{
		StateDir: parts[0], OperationID: parts[1], Label: "live job",
		Command:  []string{"/bin/sh", "-c", parts[2]},
		StopPoll: 10 * time.Millisecond, StalePoll: 10 * time.Millisecond,
		StopGrace: time.Second, Diagnostics: os.Stderr,
	})
	if err != nil {
		t.Fatalf("supervisor: %v", err)
	}
	os.Exit(result.ExitCode)
}

// startLiveSupervisor re-execs this binary as a supervisor and waits until the
// job it supervises has really started.
func startLiveSupervisor(t *testing.T, stateDir, operationID, command string) (*exec.Cmd, string, int) {
	t.Helper()
	childID, err := procchild.IDForOperation(operationID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestProcessChildSupervisorHelper", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), supervisorHelperEnv+"="+strings.Join([]string{stateDir, operationID, command}, "\n"))
	// Its own process group, so the parent can kill the supervisor and its job
	// together without the test binary going with them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		events := liveOutboxEvents(stateDir, childID)
		for _, event := range events {
			if event.Kind == procchild.KindStarted {
				pid, _ := event.Payload["pid"].(float64)
				if pid == 0 {
					if whole, ok := event.Payload["pid"].(int); ok {
						pid = float64(whole)
					}
				}
				return cmd, childID, int(pid)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the supervisor never started its job; its outbox holds %d event(s)", len(events))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// liveOutboxEvents reads one child's queue, tolerating an outbox that does not
// exist yet: a supervisor between fork and its first write has written nothing.
func liveOutboxEvents(stateDir, childID string) []procchild.Event {
	outbox, err := procchild.OpenOutboxForReading(stateDir)
	if err != nil {
		return nil
	}
	defer outbox.Close()
	events, err := outbox.All(childID, 0, 0)
	if err != nil {
		return nil
	}
	return events
}

func registerLiveChild(t *testing.T, manager *Manager, childID string, supervisorPID int) {
	t.Helper()
	pid := supervisorPID
	if _, err := manager.RegisterProcessChild(ProcessChildRegistrationOptions{
		OwnerKey: "owner-proc", ParentChatID: "chat-proc", ParentTabID: "tab-proc",
		WorkID: childID, Label: "live job",
		OutputFile:    liveOutputPath(t, manager.opts.StateDir, childID),
		SupervisorPID: &pid, Isolation: string(procchild.GroupProcessGroup),
		OriginLaneID: "lane-1", OriginOperationID: "op-1", OriginTurnID: "turn-1",
	}); err != nil {
		t.Fatalf("register the live child: %v", err)
	}
}

func liveOutputPath(t *testing.T, stateDir, childID string) string {
	t.Helper()
	// The supervisor chose this path itself; the daemon validates it rather
	// than inventing one.
	return stateDir + "/external-work/" + childID + ".output"
}

func TestAJobKilledExternallyIsLostAndItsOwnerIsNotified(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	state := manager.opts.StateDir
	supervisor, childID, childPID := startLiveSupervisor(t, state, "op-live-kill", "sleep 120")
	registerLiveChild(t, manager, childID, supervisor.Process.Pid)
	manager.reconcileProcessChildren(time.Now().UTC())
	if item := itemFor(t, manager, childID); item.Status != "running" {
		t.Fatalf("a live job must read as running, got %q", item.Status)
	}

	// Killed externally, the way a supervisor really dies: SIGKILL to its
	// group, so nothing gets the chance to write anything.
	if err := syscall.Kill(-supervisor.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = supervisor.Wait()
	t.Cleanup(func() { _ = syscall.Kill(-childPID, syscall.SIGKILL) })
	deadline := time.Now().Add(10 * time.Second)
	for procchild.PIDAlive(supervisor.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatal("the supervisor outlived SIGKILL")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The JOB is still running, and that is not a flaw in the test: the child
	// is in its own process group precisely so a signal aimed at the supervisor
	// does not reach it. This is exactly why `lost` means UNKNOWN rather than
	// dead — the work may still be going, and nobody is left who could say.
	if !procchild.PIDAlive(childPID) {
		t.Fatal("killing the supervisor's group reached the job; the child was not isolated")
	}
	for _, event := range liveOutboxEvents(state, childID) {
		if procchild.Terminal(event.Kind) {
			t.Fatalf("a SIGKILLed supervisor cannot have recorded an outcome, yet its outbox holds %q", event.Kind)
		}
	}

	now := time.Now().UTC()
	manager.reconcileProcessChildren(now)
	if item := itemFor(t, manager, childID); item.Status != "running" {
		t.Fatalf("a vanished supervisor gets its grace first, got %q", item.Status)
	}
	manager.reconcileProcessChildren(now.Add(supervisorMissingGrace + time.Second))
	receipt := receiptFor(t, manager, childID)
	if receipt.Outcome != procchild.OutcomeLost {
		t.Fatalf("outcome = %q, want lost", receipt.Outcome)
	}
	if receipt.Status == "running" {
		t.Fatal("a killed job must never still read as running")
	}
	// The owner is told, and told the honest thing.
	events2 := lifecycle.snapshot()
	if len(events2) != 1 || events2[0].Kind != ProcessLifecycleExit || events2[0].Outcome != procchild.OutcomeLost {
		t.Fatalf("the owner must be notified exactly once, with lost: %#v", events2)
	}
	if events2[0].NoticeSuppressed {
		t.Fatal("a lost job is actionable and must not be suppressed")
	}
	if !strings.Contains(events2[0].Receipt.Summary, "unknown") {
		t.Fatalf("the receipt must say what the job did is unknown, got %q", events2[0].Receipt.Summary)
	}
}

func TestAJobSurvivesTheDaemonBeingReplacedMidRun(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	state := manager.opts.StateDir
	// A job that outlives the daemon's whole replacement, then exits with a
	// code the NEW daemon must deliver.
	supervisor, childID, childPID := startLiveSupervisor(t, state, "op-live-restart", "sleep 0.8; exit 5")
	registerLiveChild(t, manager, childID, supervisor.Process.Pid)
	manager.reconcileProcessChildren(time.Now().UTC())
	if err := manager.persistSpawnedWorkSnapshot("tab-proc"); err != nil {
		t.Fatal(err)
	}

	// The daemon goes away — a restart, or exit 75 and a systemd self-update.
	// The job is not its child, so it keeps running; the test proves that by
	// probing the real pid rather than by asserting it.
	manager.Reset()
	if !procchild.PIDAlive(childPID) || !procchild.PIDAlive(supervisor.Process.Pid) {
		t.Fatal("replacing the daemon killed the job it was only watching")
	}

	restarted := NewManager(Options{StateDir: state, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { restarted.Reset() })
	bindExternalWorkOwnerForTest(restarted, "owner-proc", "chat-proc", "tab-proc", "mock")
	restartedLifecycle := &recordedLifecycle{}
	restarted.SetProcessLifecycleObserver(restartedLifecycle.observe)
	if item := itemFor(t, restarted, childID); item.Status != "running" || !item.Supervised() {
		t.Fatalf("the new daemon must re-attach to the running job, got %#v", item)
	}

	if err := supervisor.Wait(); err == nil {
		t.Fatal("the job exited 5, so its supervisor must exit non-zero too")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		restarted.reconcileProcessChildren(time.Now().UTC())
		if got := restartedLifecycle.kinds(); got != "" {
			if got != "exit:failed" {
				t.Fatalf("the new daemon's lifecycle events = %q, want one failed exit", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the new daemon never ingested the outcome its predecessor never saw")
		}
		time.Sleep(50 * time.Millisecond)
	}
	receipt := receiptFor(t, restarted, childID)
	if receipt.ExitCode == nil || *receipt.ExitCode != 5 {
		t.Fatalf("exit code = %#v, want the job's own 5", receipt.ExitCode)
	}
	if got := lifecycle.kinds(); got != "" {
		t.Fatalf("the replaced daemon emitted %q after it was gone", got)
	}
}
