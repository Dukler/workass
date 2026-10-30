//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	"workass/internal/fleetclient"
	"workass/internal/procchild"
	"workass/internal/tlscert"
)

// A job this machine runs for a chat on another machine, with no chat here.
//
// The operator's reading of the old shape was "it shouldnt have this split": an
// empty chat on duklerver holding the row of work it had nothing to do with.
// These run the whole of the replacement with two real daemons, a real link, and
// a REAL `workass run`: the supervisor really is launched by the host, really
// reads the credential it was handed, and really reports over the pinned TLS
// endpoint, because the test binary answers `run` itself (see TestMain). The
// host has no chats at all, so a path that still reached for one fails instead of
// quietly finding a fixture's.

// TestMain makes this binary the `workass` the daemon under test re-executes.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "run" {
		os.Exit(runProcessChildCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

type hostedJobFleet struct {
	hosting statelessMCPTestHarness
	asking  statelessMCPTestHarness
}

// newChatlessHostForTest is a daemon on which no chat has ever existed.
func newChatlessHostForTest(t *testing.T, mutate func(*acp.Options)) statelessMCPTestHarness {
	t.Helper()
	options := acp.Options{
		RootDir: repoRoot(t), StateDir: filepath.Join(t.TempDir(), "state"), RuntimeProfile: "test",
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	}
	mutate(&options)
	manager := acp.NewManager(options)
	runtime := newTestProviderChatRuntime(t, manager, newSessionStore(filepath.Join(manager.StateDir(), sessionStateFilename)), manager.StateDir())
	control, err := newAgentControlHandler(manager, nil, newChatControlCoordinator(manager, nil, runtime))
	if err != nil {
		manager.Reset()
		t.Fatal(err)
	}
	handler := newWorkassToolHandler(manager, control, "", runtime)
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
		manager.Reset()
	})
	return statelessMCPTestHarness{manager: manager, runtime: runtime, handler: handler}
}

const hostedJobOwner = "wa:m-away/chat/mcp-chat"

// newHostedJobFleet is H, whose tool endpoint a supervisor can really reach, and
// P, whose chat asks.
func newHostedJobFleet(t *testing.T) hostedJobFleet {
	t.Helper()
	secret := newFleetTestSecret(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	var certificate tlscert.Certificate
	hosting := newChatlessHostForTest(t, func(options *acp.Options) {
		if err := os.MkdirAll(options.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if certificate, err = tlscert.Ensure(options.StateDir); err != nil {
			t.Fatal(err)
		}
		options.WorkassToolsOrigin = "https://tools.localhost:" + strconv.Itoa(port)
		options.WorkassToolsCAFile = filepath.Join(options.StateDir, tlscert.CertFileName)
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		options.WorkassToolsCommand = executable
	})
	leaf, err := tlscert.IssueLoopbackServerCertificate(certificate, "tools.localhost")
	if err != nil {
		t.Fatal(err)
	}
	tools := httptest.NewUnstartedServer(hosting.handler)
	tools.Listener.Close()
	tools.Listener = listener
	tools.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13}
	tools.StartTLS()
	t.Cleanup(tools.Close)

	home := serveMeshHome(t, "m-home", secret, hosting.handler.agentControl)
	hostingFleet := installTestFleetHome(t, "m-home", hosting, home)
	hosting.manager.SetProcessLifecycleObserver(hostingFleet.ObserveProcess)

	asking := newStatelessMCPTestHarness(t)
	away := serveMeshHome(t, "m-away", secret, asking.handler.agentControl)
	var watcher *fleetWatcher
	mesh := monitorMeshDialer(t, "m-away", secret, func(peerID, channel string, payload any) {
		watcher.Ingest(peerID, channel, payload)
	})
	watcher = installTestFleetWatcher(t, "m-away", asking, mesh)
	if !watcher.RegisterOfferChannel(away.hub) {
		t.Fatal("the watcher did not register its monitor-offer channel")
	}
	if err := mesh.AddRoute(home.machineID, "home", fleetclient.KindLAN, home.address, fleetclient.SourceManual); err != nil {
		t.Fatal(err)
	}
	hostingMesh := monitorMeshDialer(t, "m-home", secret, func(string, string, any) {})
	if err := hostingMesh.AddRoute(away.machineID, "away", fleetclient.KindLAN, away.address, fleetclient.SourceManual); err != nil {
		t.Fatal(err)
	}
	hosting.handler.agentControl.fleetMesh = hostingMesh
	if _, err := mesh.EnsureLink(context.Background(), home.machineID); err != nil {
		t.Fatal(err)
	}
	asking.handler.agentControl.remoteChats = newFleetMeshRouter(mesh, &refusingRelay{t: t}, "m-away", t.Logf)

	return hostedJobFleet{hosting: hosting, asking: asking}
}

// start is the call an agent makes: the tool, through the validator, naming a
// machine and a job and nothing else.
func (f hostedJobFleet) start(t *testing.T, arguments map[string]any) map[string]any {
	t.Helper()
	arguments = copyAnyMap(arguments)
	arguments["machine_id"] = "m-home"
	status, response := f.asking.request(t, http.MethodPost, map[string]any{
		"name": "workass_start_process_child", "arguments": arguments,
	})
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("start status=%d response=%#v", status, response)
	}
	return mapFromAnyMain(response["result"])
}

// ownersRow is the job as the chat that owns it reads it: the list it would call,
// addressed to nobody.
func (f hostedJobFleet) ownersRow(t *testing.T, workID string) map[string]any {
	t.Helper()
	listed := callProcessControl(t, f.asking.handler.agentControl, "process.list", map[string]any{
		"owner_key": "mcp-owner", "parent_chat_id": "mcp-chat", "parent_tab_id": "mcp-tab",
	})
	for _, raw := range anySlice(listed["children"]) {
		if row := mapFromAnyMain(raw); fieldString(row, "work_id") == workID {
			return row
		}
	}
	return nil
}

func (f hostedJobFleet) hostsRow(workID string) (acp.ProcessChildView, bool) {
	for _, row := range f.hosting.manager.ListProcessChildren() {
		if row.WorkID == workID {
			return row, true
		}
	}
	return acp.ProcessChildView{}, false
}

func awaitHostedJob(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The main bug, whole: a remote start that names only a machine and a job, every
// terminal state told to the chat that asked, and the job visible from both ends.
func TestARemoteJobNeedsNoChatOnItsHostAndEveryOutcomeReachesItsOwner(t *testing.T) {
	fleet := newHostedJobFleet(t)
	cwd := t.TempDir()
	release := filepath.Join(cwd, "release-running-job")

	t.Run("ok", func(t *testing.T) {
		started := fleet.start(t, map[string]any{
			"label": "hosted ok", "cwd": cwd, "operation_id": "hosted-ok",
			"command": []any{"/bin/sh", "-c", "while [ ! -e " + release + " ]; do sleep 0.02; done; echo finished"},
		})
		workID := fieldString(started, "workId")
		if started["ok"] != true || workID == "" || fieldString(started, "owner") != hostedJobOwner {
			t.Fatalf("start = %#v", started)
		}
		for _, gone := range []string{"hostTabId", "hostChatId", "startedAt"} {
			if _, present := started[gone]; present {
				t.Fatalf("the start answered %s: %#v", gone, started)
			}
		}
		awaitHostedJob(t, "the job to be running on its host", func() bool {
			row, found := fleet.hostsRow(workID)
			return found && row.Status == "running"
		})
		// The host's own list shows it, under no chat, naming its owner.
		host, _ := fleet.hostsRow(workID)
		if host.ChatID != "" || host.TabID != "" || host.Owner != hostedJobOwner {
			t.Fatalf("the host's row = %#v", host)
		}
		// The owner's list shows it, with where it runs.
		row := fleet.ownersRow(t, workID)
		if row == nil || fieldString(row, "host_machine") != "m-home" || fieldString(row, "status") != "running" {
			t.Fatalf("the owner's row = %#v", row)
		}
		if _, present := row["chat_id"]; present {
			t.Fatalf("the owner was told of a chat on the host: %#v", row)
		}

		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		notice := awaitOneProcessNotice(t, fleet.asking, "hosted ok")
		assertHostedNotice(t, notice, "done", "<exit-code>0</exit-code>", "<run-time>")

		// A replay of a finished job is that job, not a second run.
		replay := fleet.start(t, map[string]any{
			"label": "hosted ok", "cwd": cwd, "operation_id": "hosted-ok", "command": []any{"/bin/false"},
		})
		if replay["replayed"] != true || fieldString(replay, "workId") != workID || fieldString(replay, "status") != "exited" {
			t.Fatalf("a replayed start = %#v", replay)
		}
		time.Sleep(200 * time.Millisecond)
		awaitOneProcessNotice(t, fleet.asking, "hosted ok")
		// A finished job leaves the list and can still be read by its id.
		if _, listed := fleet.hostsRow(workID); listed {
			t.Fatal("a finished job is still in the host's list")
		}
		read, err := fleet.hosting.manager.ReadProcessChild(workID, 0, 0)
		if err != nil || !strings.Contains(fieldString(read, "output_tail"), "finished") {
			t.Fatalf("reading a finished job: %#v, %v", read, err)
		}
	})

	t.Run("refused", func(t *testing.T) {
		fleet.start(t, map[string]any{
			"label": "hosted refused", "cwd": cwd, "operation_id": "hosted-refused",
			"command": []any{"/bin/true"}, "inputs": []any{filepath.Join(cwd, "no-such-input")},
		})
		notice := awaitOneProcessNotice(t, fleet.asking, "hosted refused")
		assertHostedNotice(t, notice, "failed", "preflight refused")
		if strings.Contains(notice.Text, "<run-time>") {
			t.Fatalf("a job that never ran reported a run time:\n%s", notice.Text)
		}
	})

	t.Run("timed_out", func(t *testing.T) {
		fleet.start(t, map[string]any{
			"label": "hosted timed out", "cwd": cwd, "operation_id": "hosted-timeout",
			"command": []any{"/bin/sleep", "30"}, "max_runtime_ms": 200,
		})
		notice := awaitOneProcessNotice(t, fleet.asking, "hosted timed out")
		assertHostedNotice(t, notice, "timed_out", "<run-time>")
	})

	t.Run("lost", func(t *testing.T) {
		started := fleet.start(t, map[string]any{
			"label": "hosted lost", "cwd": cwd, "operation_id": "hosted-lost",
			"command": []any{"/bin/sleep", "30"},
		})
		workID := fieldString(started, "workId")
		supervisor := intField(started, "supervisorPid")
		awaitHostedJob(t, "the job to start", func() bool {
			row, found := fleet.hostsRow(workID)
			return found && row.Status == "running" && row.PID != nil
		})
		row, _ := fleet.hostsRow(workID)
		t.Cleanup(func() { _ = syscall.Kill(*row.PID, syscall.SIGKILL) })
		if err := syscall.Kill(supervisor, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		awaitHostedJob(t, "the supervisor to be gone", func() bool { return !procchild.PIDAlive(supervisor) })
		fleet.hosting.manager.ReconcileProcessChildren(time.Now().UTC())
		fleet.hosting.manager.ReconcileProcessChildren(time.Now().UTC().Add(2 * time.Hour))
		notice := awaitOneProcessNotice(t, fleet.asking, "hosted lost")
		assertHostedNotice(t, notice, "lost", "unknown")
	})

	t.Run("a stale host chat from an older owner is ignored", func(t *testing.T) {
		// What a daemon on the old version forwards. The host no longer reads
		// these fields, so an owner updated second can still start jobs.
		result, err := fleet.hosting.handler.agentControl.startProcessChildHere(context.Background(), fleetForwardedWork{
			Limit: fleetLimitFull, Origin: fleetCommandOrigin{Machine: "m-away", ChatID: "mcp-chat"},
		}, map[string]any{
			"operation_id": "hosted-old-owner", "label": "hosted old owner", "cwd": cwd,
			"command": []any{"/bin/true"}, "host_tab_id": "mcp-tab", "host_chat_id": "mcp-chat",
		})
		if err != nil || mapFromAnyMain(result)["ok"] != true {
			t.Fatalf("start = %#v, %v", result, err)
		}
		awaitOneProcessNotice(t, fleet.asking, "hosted old owner")
	})

	chats, err := fleet.hosting.handler.agentControl.chats.list()
	if err != nil {
		t.Fatal(err)
	}
	if listed := anySlice(mapFromAnyMain(chats)["chats"]); len(listed) != 0 {
		t.Fatalf("a chat appeared on the host: %#v", listed)
	}
}

// The real examples behind bugs 2 and 3: a job waiting behind another for gpu0.
func TestAQueuedRemoteJobIsQueuedUntilExecAndItsNoticeSeparatesTheWait(t *testing.T) {
	fleet := newHostedJobFleet(t)
	cwd := t.TempDir()
	release := filepath.Join(cwd, "release-holder")

	holder := fleet.start(t, map[string]any{
		"label": "gpu holder", "cwd": cwd, "operation_id": "gpu-holder", "resource": "gpu0",
		"command": []any{"/bin/sh", "-c", "while [ ! -e " + release + " ]; do sleep 0.02; done"},
	})
	holderID := fieldString(holder, "workId")
	awaitHostedJob(t, "the holder to start", func() bool {
		row, found := fleet.hostsRow(holderID)
		return found && row.Status == "running"
	})
	// stale_after is far shorter than the wait: nothing may call waiting a stall.
	waiter := fleet.start(t, map[string]any{
		"label": "gpu waiter", "cwd": cwd, "operation_id": "gpu-waiter", "resource": "gpu0",
		"stale_after_ms": 50, "max_hold_ms": 50,
		"command": []any{"/bin/sh", "-c", "sleep 0.1; echo ran"},
	})
	workID := fieldString(waiter, "workId")
	awaitHostedJob(t, "the waiter to queue", func() bool {
		row, found := fleet.hostsRow(workID)
		return found && row.QueuePosition == 2
	})
	host, _ := fleet.hostsRow(workID)
	if host.Status != "queued" || host.StartedAt != "" || host.QueuedAt == "" || host.PID != nil {
		t.Fatalf("a job waiting at position 2 reads %#v", host)
	}
	row := fleet.ownersRow(t, workID)
	if row == nil || fieldString(row, "status") != "queued" || fieldString(row, "started_at") != "" ||
		fieldString(row, "queued_at") == "" || intField(row, "queue_position") != 2 {
		t.Fatalf("the owner reads a waiting job as %#v", row)
	}
	if _, isNumber := row["queued_ms"].(int64); !isNumber {
		t.Fatalf("a forwarded queued_ms must be a number like a local one, got %#v", row["queued_ms"])
	}
	// The running holder is the contrast, and reads as started.
	if running, _ := fleet.hostsRow(holderID); running.Status != "running" || running.StartedAt == "" {
		t.Fatalf("the holder reads %#v", running)
	}
	// Long past stale_after and into a reportable wait. The reconcile pass is the
	// daemon's half: nothing it can reach may raise a notice for a waiting job.
	time.Sleep(1200 * time.Millisecond)
	fleet.hosting.manager.ReconcileProcessChildren(time.Now().UTC())
	for _, notice := range chatNotices(t, fleet.asking, "mcp-chat") {
		if notice.NoticeTitle == "gpu waiter" {
			t.Fatalf("a queued job raised a notice:\n%s", notice.Text)
		}
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	finished := awaitOneProcessNotice(t, fleet.asking, "gpu waiter")
	assertHostedNotice(t, finished, "done", "<run-time>", "<queued>")
	queued := hostedNoticeDuration(t, finished.Text, "queued")
	if queued < time.Second {
		t.Fatalf("it waited over a second and the notice says %s:\n%s", queued, finished.Text)
	}
	if run := hostedNoticeDuration(t, finished.Text, "run-time"); run >= queued {
		t.Fatalf("the run time (%s) still includes the wait (%s):\n%s", run, queued, finished.Text)
	}
	// A job that never waited says nothing about a queue.
	unqueued := awaitOneProcessNotice(t, fleet.asking, "gpu holder")
	if strings.Contains(unqueued.Text, "<queued>") {
		t.Fatalf("a job that never waited reported a queue:\n%s", unqueued.Text)
	}
}

func assertHostedNotice(t *testing.T, notice chat.LedgerEvent, status string, contains ...string) {
	t.Helper()
	if notice.NoticeStatus != status {
		t.Fatalf("notice status = %q, want %q:\n%s", notice.NoticeStatus, status, notice.Text)
	}
	if !strings.HasPrefix(notice.Text, "<task-notification>") {
		t.Fatalf("not a task notification:\n%s", notice.Text)
	}
	for _, expected := range contains {
		if !strings.Contains(notice.Text, expected) {
			t.Fatalf("the notice does not say %q:\n%s", expected, notice.Text)
		}
	}
}

func hostedNoticeDuration(t *testing.T, text, tag string) time.Duration {
	t.Helper()
	_, rest, found := strings.Cut(text, "<"+tag+">")
	value, _, closed := strings.Cut(rest, "</"+tag+">")
	if !found || !closed {
		t.Fatalf("no <%s> in:\n%s", tag, text)
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		t.Fatalf("<%s>%s: %v", tag, value, err)
	}
	return duration
}

// The DepthVision notice, exactly. It said elapsed 5h45m35s beside a result that
// said 2h47m22s, because the first number included the wait.
func TestTheNoticeStatesRunTimeAndQueueTimeApart(t *testing.T) {
	zero := 0
	notice := fleetMonitorNotice{Label: "DepthVision test E: baseline A seed 6", Outcome: "settled",
		ProcessReceipt: &acp.SpawnedWorkReceipt{
			TaskID: "xwdepth", ExitCode: &zero, Summary: "Finished with exit 0 after 2h47m22s",
			ElapsedMs: 10042000, RunMs: 10042000, QueuedMs: 10693000,
		}}
	text := formatFleetProcessExit(notice)
	for _, expected := range []string{"<run-time>2h47m22s</run-time>", "<queued>2h58m13s</queued>"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %s:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "<elapsed>") || strings.Contains(text, "5h45m35s") {
		t.Fatalf("the combined number survived:\n%s", text)
	}
	// Never waited: no queue line at all.
	notice.ProcessReceipt.QueuedMs = 400
	if text := formatFleetProcessExit(notice); strings.Contains(text, "<queued>") {
		t.Fatalf("a wait that rounds to nothing was reported:\n%s", text)
	}
	// From a host that predates the split, the number is not called a run time.
	notice.ProcessReceipt.RunMs, notice.ProcessReceipt.QueuedMs = 0, 0
	notice.ProcessReceipt.ElapsedMs = 20735000
	if text := formatFleetProcessExit(notice); strings.Contains(text, "<run-time>") || !strings.Contains(text, "<elapsed>5h45m35s</elapsed>") {
		t.Fatalf("an older host's elapsed time:\n%s", text)
	}
	// And the split survives the wire, which hands numbers back as strings.
	receipt := spawnedWorkReceiptFromPayload(roundTripToolArgumentsForTest(t, spawnedWorkReceiptPayload(acp.SpawnedWorkReceipt{
		TaskID: "xwdepth", RunMs: 10042000, ElapsedMs: 10042000, QueuedMs: 10693000, QueuedAt: "2026-10-30T01:00:00Z",
	})))
	if receipt.RunMs != 10042000 || receipt.QueuedMs != 10693000 || receipt.QueuedAt != "2026-10-30T01:00:00Z" {
		t.Fatalf("across the link = %#v", receipt)
	}
}

// jobCredentialRequest is the supervisor's own request: its token, no chat.
func jobCredentialRequest(t *testing.T, harness statelessMCPTestHarness, method, token, chatID, tabID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	encoded, _ := json.Marshal(body)
	request, err := http.NewRequest(method, harness.server.URL+toolsPath, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if chatID != "" {
		request.Header.Set("X-Workass-Chat-ID", chatID)
		request.Header.Set("X-Workass-Tab-ID", tabID)
	}
	request.Header.Set("Content-Type", "application/json")
	reply, err := harness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	var response map[string]any
	_ = json.NewDecoder(reply.Body).Decode(&response)
	return reply.StatusCode, response
}

// 14b condition 4, rescoped and no wider: over the real endpoint, a job's
// credential does the one thing and is dead once the job is.
func TestAHostedJobsCredentialReportsThatJobAndNothingElse(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	harness.handler.agentControl.selfMachineID = "m-home"
	const operation, owner = "hosted-report-job", "wa:m-away/chat/some-chat"
	workID, err := procchild.IDForOperation(operation)
	if err != nil {
		t.Fatal(err)
	}
	token := harness.manager.MintRunToken(workID, owner, operation)
	report := func(events []procchild.Event) map[string]any {
		return map[string]any{"name": "workass_register_process_child", "arguments": procchild.ReportArguments(procchild.Report{
			Spec:    procchild.Spec{OperationID: operation, Label: "credential job", Owner: owner, Command: []string{"true"}},
			ChildID: workID, OutputFile: filepath.Join(harness.manager.StateDir(), "external-work", workID+".output"),
			SupervisorPID: os.Getpid(), Events: events, Heartbeat: procchild.HeartbeatInterval,
		})}
	}
	now := time.Now().UTC()
	status, response := jobCredentialRequest(t, harness, http.MethodPost, token, "", "", report([]procchild.Event{
		{Seq: 1, At: now, Kind: procchild.KindCreated, Payload: map[string]any{"label": "credential job"}},
	}))
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("its own report: status=%d %#v", status, response)
	}
	if ack := intField(mapFromAnyMain(response["result"]), "ackThrough"); ack != 1 {
		t.Fatalf("ackThrough = %d", ack)
	}
	if view, err := harness.manager.ReadProcessChild(workID, 0, 0); err != nil || view == nil {
		t.Fatalf("the job has no row: %v", err)
	}
	if items := harness.manager.ListSpawnedWork("mcp-tab", "mcp-chat"); len(items) != 0 {
		t.Fatalf("a chat on this machine holds the job: %#v", items)
	}

	if status, _ := jobCredentialRequest(t, harness, http.MethodGet, token, "", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("it read the catalog: %d", status)
	}
	if status, _ := jobCredentialRequest(t, harness, http.MethodPost, token, "mcp-chat", "mcp-tab", report(nil)); status != http.StatusUnauthorized {
		t.Fatalf("it acted for a chat: %d", status)
	}
	if status, _ := jobCredentialRequest(t, harness, http.MethodPost, token, "", "", map[string]any{
		"name": "workass_list_chats", "arguments": map[string]any{},
	}); status != http.StatusForbidden {
		t.Fatalf("it reached another tool: %d", status)
	}
	for _, field := range []string{"work_id", "owner", "operation_id"} {
		forged := report(nil)
		arguments := copyAnyMap(mapFromAnyMain(forged["arguments"]))
		arguments[field] = map[string]string{
			"work_id": "xwotherjob001", "owner": "wa:m-away/chat/other", "operation_id": "other-op",
		}[field]
		forged["arguments"] = arguments
		_, response := jobCredentialRequest(t, harness, http.MethodPost, token, "", "", forged)
		if response["error"] == nil {
			t.Fatalf("a report with another %s was accepted: %#v", field, response)
		}
	}

	status, response = jobCredentialRequest(t, harness, http.MethodPost, token, "", "", report([]procchild.Event{
		{Seq: 2, At: now, Kind: procchild.KindSettled, Payload: map[string]any{"outcome": procchild.OutcomeSettled, "exitCode": 0}},
	}))
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("its closing report: status=%d %#v", status, response)
	}
	if status, _ := jobCredentialRequest(t, harness, http.MethodPost, token, "", "", report(nil)); status != http.StatusUnauthorized {
		t.Fatalf("its credential outlived it: %d", status)
	}
}

// A job started before this change is still running somewhere, holding a token
// bound to a chat on its host, with its row in that chat. An update must not cut
// it off: that is the 3-hour run on duklerver, and every run like it.
func TestAJobStartedBeforeTheChangeKeepsReportingWithTheCredentialItHolds(t *testing.T) {
	const token, operation = "wrt_minted-by-the-previous-version", "pre-update-job"
	workID, err := procchild.IDForOperation(operation)
	if err != nil {
		t.Fatal(err)
	}
	harness := newStatelessMCPTestHarnessWithOptions(t, nil, func(options *acp.Options) {
		dir := filepath.Join(options.StateDir, "tool-contexts")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		record, _ := json.Marshal([]map[string]string{{
			"token": token, "chatId": "mcp-chat", "tabId": "mcp-tab", "operationId": operation,
			"issuedAt": time.Now().UTC().Format(time.RFC3339),
		}})
		if err := os.WriteFile(filepath.Join(dir, "run-tokens.json"), record, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	harness.handler.agentControl.selfMachineID = "m-home"
	owner := "wa:m-away/chat/some-chat"
	arguments := procchild.ReportArguments(procchild.Report{
		Spec:    procchild.Spec{OperationID: operation, Label: "pre-update job", Owner: owner, Command: []string{"train"}},
		ChildID: workID, OutputFile: filepath.Join(harness.manager.StateDir(), "external-work", workID+".output"),
		SupervisorPID: os.Getpid(), Heartbeat: procchild.HeartbeatInterval,
		Events: []procchild.Event{{Seq: 1, At: time.Now().UTC(), Kind: procchild.KindCreated}},
	})
	status, response := jobCredentialRequest(t, harness, http.MethodPost, token, "mcp-chat", "mcp-tab", map[string]any{
		"name": "workass_register_process_child", "arguments": arguments,
	})
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("its report: status=%d %#v", status, response)
	}
	found := false
	for _, item := range harness.manager.ListSpawnedWork("mcp-tab", "mcp-chat") {
		found = found || item.ID == workID
	}
	if !found {
		t.Fatal("its row left the chat that has always held it")
	}
	if err := harness.handler.agentControl.fenceRunCredential("chat.send", token, "mcp-tab", "mcp-chat",
		map[string]any{"operation_id": operation}); err == nil {
		t.Fatal("an older credential gained another method")
	}
}

// Owner updated first: the host still demands a chat, and its answer must not
// send an agent off to find or create one.
func TestAHostStillOnTheOldVersionIsRefusedByName(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	harness.handler.agentControl.selfMachineID = "m-away"
	for _, refusal := range []string{
		"process.start needs host_tab_id + host_chat_id: the exact chat on this machine that will hold the job's local row. Call workass_list_chats with this machine_id, or create one with workass_create_chat machine_id",
		"unknown machine-scoped agent control method",
	} {
		harness.handler.agentControl.remoteChats = &refusingRelay{t: t, allow: true, err: errors.New(refusal)}
		status, response := harness.request(t, http.MethodPost, map[string]any{
			"name": "workass_start_process_child", "arguments": map[string]any{
				"machine_id": "m-old", "label": "x", "cwd": "/tmp", "command": []any{"true"}, "operation_id": "old-host",
			},
		})
		text := fleetToolFailureText(response)
		if status != http.StatusOK || !strings.Contains(text, "m-old") || !strings.Contains(text, "older Workass") ||
			!strings.Contains(text, "nothing was started") {
			t.Fatalf("status=%d refusal = %q", status, text)
		}
		if strings.Contains(text, "host_tab_id") || strings.Contains(text, "workass_create_chat") {
			t.Fatalf("the refusal still sends the caller after a chat: %q", text)
		}
	}
	// Any other refusal from the host is that host's own words.
	harness.handler.agentControl.remoteChats = &refusingRelay{t: t, allow: true, err: errors.New("process.start cwd is not a readable directory on this machine: /nope")}
	_, response := harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_start_process_child", "arguments": map[string]any{
			"machine_id": "m-new", "label": "x", "cwd": "/nope", "command": []any{"true"}, "operation_id": "new-host",
		},
	})
	if text := fleetToolFailureText(response); !strings.Contains(text, "/nope") {
		t.Fatalf("a real refusal was rewritten: %q", text)
	}
}

func TestTheStartToolNoLongerHasAHostChat(t *testing.T) {
	for _, tool := range agentMCPTools() {
		if tool["name"] != "workass_start_process_child" {
			continue
		}
		encoded, _ := json.Marshal(tool)
		if strings.Contains(string(encoded), "host_tab_id") || strings.Contains(string(encoded), "host_chat_id") {
			t.Fatalf("the schema still names a host chat: %s", encoded)
		}
		return
	}
	t.Fatal("workass_start_process_child is gone")
}
