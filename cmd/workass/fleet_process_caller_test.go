package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/procchild"
)

// bug-20261116-7ab17943, end to end on one machine: the real supervisor reports
// through the real tool into the real chat actor. A `workass run` whose caller
// was waiting handed that caller its outcome, and the chat gets no second copy
// of it as a queued turn. A run whose caller is gone (`&`, a killed shell) is
// still the chat's only way to find out, and still notifies exactly once.

// harnessReporter is the supervisor's daemon client, pointed at the harness.
// The payload is the supervisor's own constructor's, never hand-built.
type harnessReporter struct {
	t       *testing.T
	harness statelessMCPTestHarness
}

func (r harnessReporter) Report(request procchild.Report) (procchild.Acknowledgement, error) {
	status, response := r.harness.request(r.t, http.MethodPost, map[string]any{
		"name": "workass_register_process_child", "arguments": procchild.ReportArguments(request),
	})
	if status != http.StatusOK || response["error"] != nil {
		r.t.Errorf("report for %s: status=%d %#v", request.ChildID, status, response)
		return procchild.Acknowledgement{}, nil
	}
	result := mapFromAnyMain(response["result"])
	return procchild.Acknowledgement{
		ChildID: request.ChildID, Status: fieldString(result, "status"),
		AckThrough: uint64(ackThroughOf(result)),
	}, nil
}

func TestAJobWhoseCallerWaitedIsNotNotifiedAgainAndADetachedOneIs(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", newFleetTestSecret(t), harness.handler.agentControl)
	fleet := installTestFleetHome(t, "m-home", harness, home)
	harness.manager.SetProcessLifecycleObserver(fleet.ObserveProcess)
	watcher := installTestFleetWatcher(t, "m-home", harness, nil)
	fleet.SetLocalDelivery(watcher.IngestLocalEvent)
	watcher.localHome = fleet

	stateDir := harness.manager.StateDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "external-work"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(label string, callerWaiting func() bool, command ...string) procchild.Result {
		t.Helper()
		outbox, err := procchild.OpenOutbox(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		defer outbox.Close()
		operation := "op-" + strings.ReplaceAll(label, " ", "-")
		childID, err := procchild.IDForOperation(operation)
		if err != nil {
			t.Fatal(err)
		}
		result, err := procchild.Run(procchild.Spec{
			StateDir: stateDir, OperationID: operation, Label: label, Command: command,
			OutputFile: filepath.Join(stateDir, "external-work", childID+".output"),
			Reporter:   harnessReporter{t: t, harness: harness}, Outbox: outbox,
			Diagnostics: &bytes.Buffer{}, StopPoll: 5 * time.Millisecond, StalePoll: 5 * time.Millisecond,
			ReportRetry: 20 * time.Millisecond, ReportDeadline: 5 * time.Second,
			CallerWaiting: callerWaiting,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Reporting.Delivered {
			t.Fatalf("%s: the outcome was not reported: %+v", label, result.Reporting)
		}
		return result
	}
	noticesFor := func(label string) int {
		count := 0
		for _, notice := range chatNotices(t, harness, "mcp-chat") {
			if notice.NoticeTitle == label {
				count++
			}
		}
		return count
	}

	// The real probe: this test process's parent is alive, so the caller is
	// here to read the receipt, exactly as an agent's foreground shell is.
	waited := run("waited deploy", procchild.WatchCaller(), "/bin/sh", "-c", "exit 0")
	waitedFailed := run("waited failing deploy", procchild.WatchCaller(), "/bin/sh", "-c", "exit 1")
	if waited.Outcome != procchild.OutcomeSettled || waitedFailed.ExitCode != 1 {
		t.Fatalf("outcomes: %+v / %+v", waited, waitedFailed)
	}
	// The caller is gone: the shell returned, or a tool timeout killed it.
	run("detached deploy", func() bool { return false }, "/bin/sh", "-c", "exit 0")

	awaitOneProcessNotice(t, harness, "detached deploy")
	// Give a stray notice for the waited jobs every chance to arrive.
	time.Sleep(300 * time.Millisecond)
	for _, label := range []string{"waited deploy", "waited failing deploy"} {
		if got := noticesFor(label); got != 0 {
			t.Fatalf("%q: the caller already had its outcome, yet the chat got %d notice(s)", label, got)
		}
	}
	// The outcome is still on record for anyone who reads the chat's receipts.
	receipts, err := harness.runtime.ListSpawnedWorkReceipts("mcp-owner", "mcp-chat", "mcp-tab", "mcp-chat", "mcp-tab", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, receipt := range receipts {
		found[receipt.Label] = receipt.Status
	}
	if found["waited deploy"] != "exited" || found["waited failing deploy"] != "failed" {
		t.Fatalf("a suppressed notice must keep its receipt, got %#v", found)
	}
}
