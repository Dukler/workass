package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/fleetclient"
	"workass/internal/headlessupdate"
	"workass/internal/lease"
	"workass/internal/procchild"
	"workass/internal/wire"
)

// B2, from the first live P1–P4 test (2026-10-30): `workass_get_update_status`
// and `workass_apply_update` refused THIS machine with "requires an exact
// machine_id", using the exact id `workass_list_update_targets` had just
// printed, while every remote machine worked.
//
// The cause was one line above the updater's own switch: a machine_id naming
// this machine is stripped so the call takes the local path, and the updater is
// the one family addressed by machine_id all the way down. The headless half
// put it back; the renderer half did not.
func TestSelfAddressedUpdateKeepsItsMachineID(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	remote := &stubAgentChatRemoteRouter{result: map[string]any{"machineId": "m-self", "ok": true}}
	harness.handler.agentControl.remoteChats = remote
	harness.handler.agentControl.selfMachineID = "m-self"

	status, response := harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_get_update_status", "arguments": map[string]any{"machine_id": "m-self"},
	})
	if status != http.StatusOK || response["error"] != nil || remote.method != "update.status" {
		t.Fatalf("self update status HTTP=%d response=%#v method=%q", status, response, remote.method)
	}
	if fieldString(remote.params, "machine_id") != "m-self" {
		t.Fatalf("the local updater route lost its machine_id: %#v", remote.params)
	}

	status, response = harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_apply_update",
		"arguments": map[string]any{
			"machine_id": "m-self", "expected_current_version": "0.1.237", "expected_target_version": "0.1.238",
			"authorization": "update m-self from 0.1.237 to 0.1.238", "operation_id": "update-self-0.1.238",
		},
	})
	if status != http.StatusOK || response["error"] != nil || remote.method != "update.apply" {
		t.Fatalf("self update apply HTTP=%d response=%#v method=%q", status, response, remote.method)
	}
	if fieldString(remote.params, "machine_id") != "m-self" {
		t.Fatalf("the local activation route lost its machine_id: %#v", remote.params)
	}
	// The owner capability still never travels, which is what it was before.
	if remote.params["owner_key"] != nil || remote.params["parent_chat_id"] != nil {
		t.Fatalf("owner capability reached the updater route: %#v", remote.params)
	}
}

// B5's local half: a supervised job on THIS machine is still `workass run` in
// the agent's own shell, and the refusal says so instead of starting one.
func TestProcessStartHereRefusesAndNamesTheCommand(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	harness.handler.agentControl.selfMachineID = "m-self"
	for _, arguments := range []map[string]any{
		{"operation_id": "start-1", "label": "x", "cwd": "/tmp", "command": []any{"true"}, "machine_id": "m-self"},
	} {
		status, response := harness.request(t, http.MethodPost, map[string]any{
			"name": "workass_start_process_child", "arguments": arguments,
		})
		if status != http.StatusOK {
			t.Fatalf("local process.start HTTP=%d response=%#v", status, response)
		}
		text := fleetToolFailureText(response)
		if !strings.Contains(text, "workass run") || !strings.Contains(text, "machine_id") {
			t.Fatalf("local process.start refusal = %q", text)
		}
	}
}

// B5, §11: work that arrived over a narrowed link may not run commands here,
// and the refusal names the capability it lacks rather than denying generically.
func TestProcessStartRefusesNarrowedWorkByName(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	control := harness.handler.agentControl
	for _, limit := range []string{fleetLimitRead, fleetLimitEdit} {
		_, err := control.startProcessChildHere(context.Background(), fleetForwardedWork{
			Limit:  limit,
			Origin: fleetCommandOrigin{Machine: "m-mac", ChatID: "app-chat-1"},
		}, map[string]any{"operation_id": "start-1", "label": "x", "cwd": os.TempDir(), "command": []any{"true"}})
		if err == nil || !strings.Contains(err.Error(), "process.start") || !strings.Contains(err.Error(), limit) {
			t.Fatalf("%s link: err=%v", limit, err)
		}
	}
	// And with no chat that asked, there is nobody the outcome could reach.
	if _, err := control.startProcessChildHere(context.Background(), fleetForwardedWork{Limit: fleetLimitFull},
		map[string]any{"operation_id": "start-1", "label": "x", "cwd": os.TempDir(), "command": []any{"true"}},
	); err == nil || !strings.Contains(err.Error(), "chat that asked") {
		t.Fatalf("origin-less start: err=%v", err)
	}
}

// B5's whole safety claim: the job's environment is BUILT, never inherited, so
// nothing of the daemon's own environment — no provider credential, no token,
// no session path — can reach a command the caller named.
func TestProcessStartBuildsTheJobEnvironmentFromNothing(t *testing.T) {
	t.Setenv("WORKASS_SECRET_FIXTURE", "super-secret-value")
	t.Setenv("PATH", "/usr/bin:/bin")
	environment, err := processStartEnvironment(map[string]any{
		"env": map[string]any{"CUDA_VISIBLE_DEVICES": "0", "BUILD_TAG": "nightly"},
	})
	if err != nil {
		t.Fatalf("build job environment: %v", err)
	}
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "WORKASS_SECRET_FIXTURE") || strings.Contains(joined, "super-secret-value") {
		t.Fatalf("the daemon's own environment reached the job: %q", joined)
	}
	for _, expected := range []string{"PATH=/usr/bin:/bin", "BUILD_TAG=nightly", "CUDA_VISIBLE_DEVICES=0"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("job environment is missing %q: %q", expected, joined)
		}
	}
	// A secret-shaped variable is refused rather than forwarded, and the job's
	// own credential is this daemon's to set.
	for _, declared := range []map[string]any{
		{"env": map[string]any{"HF_TOKEN": "x"}},
		{"env": map[string]any{"HARMLESS": "bearer abc"}},
		{"env": map[string]any{procchild.TokenEnvVar: "/tmp/other.json"}},
		{"env": map[string]any{"not a name": "x"}},
	} {
		if _, err := processStartEnvironment(declared); err == nil {
			t.Errorf("job environment accepted %#v", declared)
		}
	}
}

// B5's credential: one job, one owner, one call. A token that leaks out of the
// job's own file authorizes nothing else.
func TestRunCredentialAuthorizesOnlyItsOwnRegistration(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	control := harness.handler.agentControl
	const owner = "wa:m-away/chat/mcp-chat"
	token := harness.manager.MintRunToken("xwjob00000001", owner, "start-1")
	if token == "" {
		t.Fatal("no run credential was minted")
	}
	if again := harness.manager.MintRunToken("xwjob00000001", owner, "start-1"); again != token {
		t.Fatal("a replayed start minted a second credential for one job")
	}
	own := map[string]any{"operation_id": "start-1", "work_id": "xwjob00000001", "owner": owner}
	if err := control.fenceRunCredential("process.register", token, "", "", own); err != nil {
		t.Fatalf("the job's own registration was refused: %v", err)
	}
	withField := func(field, value string) map[string]any {
		out := copyAnyMap(own)
		out[field] = value
		return out
	}
	for _, item := range []struct {
		name   string
		method string
		tabID  string
		chatID string
		params map[string]any
	}{
		{"another method", "chat.send", "", "", own},
		{"another spawn", "agent.spawn", "", "", own},
		{"another operation", "process.register", "", "", withField("operation_id", "start-2")},
		{"another job", "process.register", "", "", withField("work_id", "xwjob00000002")},
		{"another owner", "process.register", "", "", withField("owner", "wa:m-away/chat/other")},
		{"no owner", "process.register", "", "", withField("owner", "")},
		{"a chat on this machine", "process.register", "mcp-tab", "mcp-chat", own},
		{"a targeted chat", "process.register", "", "", withField("chat_id", "mcp-chat")},
	} {
		if err := control.fenceRunCredential(item.method, token, item.tabID, item.chatID, item.params); err == nil {
			t.Errorf("a one-shot job credential authorized %s", item.name)
		}
	}
	// It is nobody: no path that authorizes by chat can accept it, even for the
	// empty chat it names.
	if harness.manager.ValidateAgentOwner(token, "", "") {
		t.Fatal("a job credential authenticated as a chat")
	}
	// An ordinary owner key is untouched by the fence.
	if err := control.fenceRunCredential("chat.send", "mcp-owner", "mcp-tab", "mcp-chat", map[string]any{}); err != nil {
		t.Fatalf("the fence rejected an ordinary owner key: %v", err)
	}
	harness.manager.CloseRunToken(token, "exited", procchild.OutcomeSettled)
	if binding, live := harness.manager.RunTokenFor(token); !live || !binding.Closed() {
		t.Fatalf("a closed job credential reads %#v, %v", binding, live)
	}
	if err := control.fenceRunCredential("chat.send", token, "", "", map[string]any{"operation_id": "start-1"}); err == nil {
		t.Fatal("a closed job credential authorized another method")
	}
	if harness.manager.ValidateAgentOwner(token, "", "") {
		t.Fatal("a closed job credential authenticated as a chat")
	}
}

// B3: the coordinator's own id `update-duklerver-0.1.237-20261030` was refused
// for its dots alone, and the refusal did not say so. The product-wide contract
// and the Electron shell both accepted that alphabet already.
func TestAuthorizedUpdateOperationIDAcceptsTheProductAlphabet(t *testing.T) {
	manager := headlessupdate.New(headlessupdate.Options{
		Executable: filepath.Join(t.TempDir(), "workass"), StateDir: t.TempDir(), Version: "0.1.237",
	})
	refused := func(operationID string) string {
		_, err := manager.Apply(context.Background(), headlessupdate.ApplyRequest{
			OperationID: operationID, MachineID: "m-duklerver",
			ExpectedCurrentVersion: "0.1.237", ExpectedTargetVersion: "0.1.238",
			Authorization: "update m-duklerver from 0.1.237 to 0.1.238",
		})
		if err == nil {
			return ""
		}
		return err.Error()
	}
	// The live id now passes the id check and is refused for a later, honest
	// reason (this fixture supports no activation at all).
	if message := refused("update-duklerver-0.1.237-20261030"); strings.Contains(message, "operation_id") {
		t.Fatalf("the live operation id is still refused for its shape: %s", message)
	}
	if message := refused("updatedukl20261030a"); strings.Contains(message, "operation_id") {
		t.Fatalf("the accepted id regressed: %s", message)
	}
	// A genuinely malformed id is still refused — and now states the format.
	message := refused("short")
	if !strings.Contains(message, "operation_id") {
		t.Fatalf("a too-short id was accepted: %s", message)
	}
	for _, expected := range []string{"8 to 96", "_ . : / + = @ -"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("the refusal does not state the format: %s", message)
		}
	}
}

// B4: `workass fleet links` reported every paired peer as "known, not linked"
// while the peer itself showed the link up. The display read a REDACTED copy,
// whose token had been blanked, and then asked Linked(), which is
// "DeviceToken != "". Nothing was wrong with the link.
func TestRedactedPeerStillReportsItsPairing(t *testing.T) {
	stateDir := t.TempDir()
	store, err := fleetclient.Open(stateDir)
	if err != nil {
		t.Fatalf("open peer store: %v", err)
	}
	if _, err := store.AddRoute("m-duklerver", "duklerver", fleetclient.KindLAN, "192.168.0.134:8788", fleetclient.SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := store.SaveLink("m-duklerver", "device-1", "key-1", "a-derived-token", "fingerprint", "duklerver"); err != nil {
		t.Fatalf("save link: %v", err)
	}
	redacted := store.Redacted()
	if len(redacted) != 1 {
		t.Fatalf("redacted peers = %d", len(redacted))
	}
	if redacted[0].DeviceToken != "" {
		t.Fatal("the redacted peer still carries its credential")
	}
	if !redacted[0].Linked() {
		t.Fatal("a paired peer read as unlinked once its token was redacted")
	}
	var out strings.Builder
	if err := printFleetLinks(stateDir, &out); err != nil {
		t.Fatalf("print fleet links: %v", err)
	}
	if !strings.Contains(out.String(), "paired") || strings.Contains(out.String(), "known, not linked") {
		t.Fatalf("fleet links output = %q", out.String())
	}
	if strings.Contains(out.String(), "a-derived-token") {
		t.Fatalf("fleet links printed a credential: %q", out.String())
	}
}

// B4's other half: a live link's heartbeat is written down, coalesced, so
// `lastUse` means "this link was alive then" and not "it was paired then".
func TestNoteBeatCoalescesWithoutFreezingLastUse(t *testing.T) {
	stateDir := t.TempDir()
	store, err := fleetclient.Open(stateDir)
	if err != nil {
		t.Fatalf("open peer store: %v", err)
	}
	if _, err := store.AddRoute("m-duklerver", "duklerver", fleetclient.KindLAN, "192.168.0.134:8788", fleetclient.SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if err := store.NoteBeat("m-duklerver", 0); err != nil {
		t.Fatalf("first beat: %v", err)
	}
	first := store.Peers()[0].LastUseAt
	if first == "" {
		t.Fatal("a beat recorded no last use at all")
	}
	// A beat inside the coalescing window writes nothing, which is what keeps
	// the one timer in the runtime from becoming a disk write per minute.
	if err := store.NoteBeat("m-duklerver", time.Duration(fleetclient.BeatRecordInterval())); err != nil {
		t.Fatalf("coalesced beat: %v", err)
	}
	if store.Peers()[0].LastUseAt != first {
		t.Fatal("a coalesced beat wrote anyway")
	}
}

// B4's cause for the OTHER direction: a dialler never stated the port it
// listens on, so the receiving daemon — which only ever learns an address from
// an inbound socket — recorded no peer at all and truthfully reported "this
// machine has dialled no peer" for a machine it was linked to.
func TestPeerEnrolmentStatesThisDaemonsOwnPort(t *testing.T) {
	stateDir := t.TempDir()
	store, err := fleetclient.Open(stateDir)
	if err != nil {
		t.Fatalf("open peer store: %v", err)
	}
	mesh := fleetclient.New(fleetclient.Options{
		SelfMachineID: "m-mac", SelfName: "mac", SelfPort: 8788, Store: store,
	})
	payload := mesh.PeerEnrolmentPayloadForTest()
	if payload["port"] != "8788" {
		t.Fatalf("enrolment payload = %#v", payload)
	}
	if payload["machineId"] != "m-mac" {
		t.Fatalf("enrolment payload lost this daemon's id: %#v", payload)
	}
	// The receiving side joins that port to the host its OWN socket reports, so
	// a peer can never name somebody else's address.
	quiet := fleetclient.New(fleetclient.Options{SelfMachineID: "m-mac", Store: store})
	if _, stated := quiet.PeerEnrolmentPayloadForTest()["port"]; stated {
		t.Fatal("a daemon with no known port stated one anyway")
	}
}

// And the receiver's half, end to end through the lease store the hub writes:
// a peer that linked in becomes a route this daemon can dial back.
func TestInboundPeerLinkBecomesADialableRoute(t *testing.T) {
	stateDir := t.TempDir()
	leases, err := lease.NewManager(lease.Options{StateDir: stateDir})
	if err != nil {
		t.Fatalf("lease manager: %v", err)
	}
	_ = leases
	store, err := fleetclient.Open(stateDir)
	if err != nil {
		t.Fatalf("open peer store: %v", err)
	}
	mesh := fleetclient.New(fleetclient.Options{SelfMachineID: "m-duklerver", Store: store})
	service := &fleetMeshService{mesh: mesh, logger: log.New(io.Discard, "", 0)}
	service.Observe(wire.PeerLinkEvent{Kind: "paired", Peer: "m-mac", Name: "mac", Address: "192.168.0.50:8788"})
	peers := store.Peers()
	if len(peers) != 1 || peers[0].MachineID != "m-mac" {
		t.Fatalf("inbound link learned no peer: %#v", peers)
	}
	if len(peers[0].Routes) != 1 || peers[0].Routes[0].Address != "192.168.0.50:8788" {
		t.Fatalf("inbound link learned no route: %#v", peers[0].Routes)
	}
}

// fleetToolFailureText is whichever of the two refusal shapes a Workass tool
// call answered with: the transport's `error`, or the MCP error result.
func fleetToolFailureText(response map[string]any) string {
	if message := strings.TrimSpace(fieldString(response, "error")); message != "" {
		return message
	}
	result := mapFromAnyMain(response["result"])
	parts := []string{}
	for _, raw := range anySlice(result["content"]) {
		parts = append(parts, fieldString(mapFromAnyMain(raw), "text"))
	}
	return strings.Join(parts, " ")
}

// B5's supervisor command line: the owner is the chat that ASKED, the
// declarations are the flags `workass run` already has, and the credential is
// a path and never a value.
func TestProcessStartArgumentsNameTheAskingChatAndNeverACredential(t *testing.T) {
	arguments := processStartArguments(
		"train the model", "wa:m-mac/chat/app-chat-1", "start-1", "/srv/work", "/var/run/cred/context.json",
		"/data/workass/state", "/data/workass/state/external-work/xw1.output",
		map[string]any{
			"resource": "gpu0", "role": "work", "max_runtime_ms": 3600000,
			"stale_after_ms": 600000, "inputs": []any{"/srv/work/in.bin"}, "min_free_disk": "50G",
		},
		[]string{"python", "-u", "train.py", "--epochs", "3"},
	)
	joined := strings.Join(arguments, " ")
	for _, expected := range []string{
		// NOT --quiet: this daemon captures the supervisor's own stream to a
		// file, and quiet threw away the one sentence that names a job nobody
		// was told about.
		"run --label train the model --owner wa:m-mac/chat/app-chat-1",
		"--operation-id start-1", "--cwd /srv/work", "--token-file /var/run/cred/context.json",
		"--role work", "--resource gpu0", "--min-free-disk 50G", "--inputs /srv/work/in.bin",
		"--max-runtime 1h0m0s", "--stale-after 10m0s",
		// Stated, never derived. A supervisor that resolved its own state
		// directory from a default resolved `state` against the JOB's working
		// directory, and four jobs reported into /tmp/state where no daemon
		// would ever look.
		"--state-dir /data/workass/state",
		// Resolved by the daemon, so the row it registers and the file the
		// supervisor writes are one path rather than two agreeing defaults.
		"--output-file /data/workass/state/external-work/xw1.output",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("supervisor argv is missing %q: %s", expected, joined)
		}
	}
	// Everything after `--` is the job, verbatim, with no shell anywhere.
	separator := -1
	for index, value := range arguments {
		if value == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		t.Fatalf("supervisor argv has no command separator: %s", joined)
	}
	job := strings.Join(arguments[separator+1:], "\x00")
	if job != "python\x00-u\x00train.py\x00--epochs\x003" {
		t.Fatalf("the job's argv was rewritten: %q", job)
	}
	// A quoted, globbed or piped string stays one argument, because there is no
	// shell to interpret it.
	command, err := processStartCommand(map[string]any{"command": []any{"sh", "-c", "echo $HOME | tee /tmp/x"}})
	if err != nil || len(command) != 3 || command[2] != "echo $HOME | tee /tmp/x" {
		t.Fatalf("command items = %#v err=%v", command, err)
	}
	for _, refused := range []map[string]any{
		{},
		{"command": []any{}},
		{"command": []any{""}},
		{"command": []any{"true", 7}},
	} {
		if _, err := processStartCommand(refused); err == nil {
			t.Errorf("process.start accepted command %#v", refused)
		}
	}
}
