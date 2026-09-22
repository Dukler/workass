package acp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPiBridgeSteersNativeSDKWithoutQueueOrInterrupt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable for native host fixture")
	}
	root, state := repoRoot(t), t.TempDir()
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root, StateDir: state, Broadcast: events.Broadcast,
		Provider: ProviderConfig{ID: "pi", Command: node, Args: []string{filepath.Join(root, "scripts", "pi-native-host.mjs")}, CWD: root,
			Env: map[string]string{"WORKASS_PI_SDK_MODULE": filepath.Join(root, "desktop", "acp", "mock-pi-sdk.mjs"), "WORKASS_PI_FIXTURE_DIR": state}},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newMockSession(t, manager, "pi-steer-tab")
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	capabilities := providerAdapterForID("pi").delivery.Capabilities(bridge)
	if !capabilities.LiveSteer || capabilities.SteerConsumptionReceipt {
		t.Fatalf("native Pi steering capabilities = %#v", capabilities)
	}
	job := startAppChatJob(t, manager, session.SessionID, "pi-steer-tab", "[fixture:wait]")
	journal := filepath.Join(state, session.SessionID+".started")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(journal)
		if strings.Contains(string(data), "[fixture:wait]") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native Pi prompt did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	result := manager.Steer(session.SessionID, "one direction", nil, "steer-user")
	if result["ok"] != true || result["live"] != true || result["queued"] != false || result["strategy"] != "pi-live" || asString(result["turnId"]) == "" {
		t.Fatalf("native Pi admission = %#v", result)
	}
	data, err := os.ReadFile(filepath.Join(state, "calls.jsonl"))
	if err != nil || strings.Count(string(data), `"method":"steer"`) != 1 || !strings.Contains(string(data), `"text":"one direction"`) {
		t.Fatalf("expected one native steer: %v\n%s", err, data)
	}
	rejected := manager.Steer(session.SessionID, "REJECT", nil, "rejected-user")
	if rejected["ok"] != false || rejected["queued"] != false || rejected["strategy"] != "rejected" {
		t.Fatalf("native Pi rejection = %#v", rejected)
	}
	if !manager.CancelJobResult(jobID(job)).Cancelled {
		t.Fatal("steering must leave the original turn running")
	}
	events.waitJobEnd(t, jobID(job), 3*time.Second)
}

func TestPiNativeHostContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable for native host fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "scripts/tests/pi-native-host.test.mjs")
	cmd.Dir = repoRoot(t)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Pi native host contract: %v\n%s", err, output)
	}
}

func TestPiDiscoveryUsesOfficialSDKHost(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable for native host fixture")
	}
	root, state := repoRoot(t), t.TempDir()
	cli := filepath.Join(state, executableName("pi"))
	writeExecutable(t, cli, nativeNoopScript())
	t.Setenv("WORKASS_PI", cli)
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_PI_HOST", filepath.Join(root, "scripts", "pi-native-host.mjs"))
	t.Setenv("WORKASS_PI_SDK_MODULE", filepath.Join(root, "desktop", "acp", "mock-pi-sdk.mjs"))
	t.Setenv("WORKASS_PI_FIXTURE_DIR", state)
	manager := NewManager(Options{RootDir: root, StateDir: state, InitTimeout: 2 * time.Second})
	t.Cleanup(func() { manager.Reset() })
	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "pi"})
	item := assertProviderListItem(t, manager.ProvidersList(), "pi", providerStatusReady, true)
	if item["resolvedCommand"] != cli {
		t.Fatalf("Pi lost its installed executable: %#v", item)
	}
	for _, group := range manager.CatalogSnapshotGroups() {
		if group.ProviderID == "pi" && len(group.Models) != 2 {
			t.Fatalf("Pi discovery lost native models: %#v", group)
		}
	}
	t.Setenv("WORKASS_PI_FIXTURE_NO_MODELS", "1")
	unauthenticated := NewManager(Options{RootDir: root, StateDir: t.TempDir(), InitTimeout: 2 * time.Second})
	t.Cleanup(func() { unauthenticated.Reset() })
	unauthenticated.DetectProviders(context.Background(), DetectOptions{ProviderID: "pi"})
	assertProviderListItem(t, unauthenticated.ProvidersList(), "pi", providerStatusNeedsLogin, false)
}

func TestPiHostUsesInstalledCommandAndSharedNode(t *testing.T) {
	root := t.TempDir()
	runtimeDir := t.TempDir()
	bundle := filepath.Join(runtimeDir, "frontier-hosts", runtime.GOOS+"-"+runtime.GOARCH)
	host := filepath.Join(bundle, "pi-native-host.mjs")
	for _, file := range []string{host, filepath.Join(root, "scripts", "pi-native-host.mjs")} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, file, "// fixture\n")
	}
	node := filepath.Join(bundle, executableName("node"))
	writeExecutable(t, node, nativeNoopScript())
	pip := filepath.Join(runtimeDir, executableName("pi"))
	writeExecutable(t, pip, nativeNoopScript())
	for _, key := range []string{"WORKASS_PI_HOST", "WORKASS_PI_SDK_MODULE", "WORKASS_BUN", "WORKASS_NODE"} {
		t.Setenv(key, "")
	}
	t.Setenv("WORKASS_NODE", node)
	input := ProviderConfig{ID: "pi", Command: pip, Args: []string{"acp"}, Env: map[string]string{"FIXTURE": "keep"}}
	got, err := piNativeHostLaunch(input, Options{RootDir: root}, filepath.Join(runtimeDir, "workass"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != node || len(got.Args) != 1 || got.Args[0] != host || got.Env["WORKASS_PI_EXECUTABLE"] != pip || got.Env["FIXTURE"] != "keep" {
		t.Fatalf("incorrect native launch: %#v", got)
	}
	if input.Env["WORKASS_PI_EXECUTABLE"] != "" {
		t.Fatal("mutated provider environment")
	}
}

func TestPiRejectsMissingInstalledExecutable(t *testing.T) {
	root := t.TempDir()
	host := filepath.Join(root, "scripts", "pi-native-host.mjs")
	os.MkdirAll(filepath.Dir(host), 0o755)
	writeFile(t, host, "// fixture")
	t.Setenv("WORKASS_PI_HOST", host)
	t.Setenv("WORKASS_PI_SDK_MODULE", filepath.Join(root, "sdk.mjs"))
	t.Setenv("WORKASS_BUN", filepath.Join(root, "bun"))
	if _, err := piNativeHostLaunch(ProviderConfig{ID: "pi", Command: filepath.Join(root, "missing-pi")}, Options{RootDir: root}, filepath.Join(root, "workass")); err == nil {
		t.Fatal("missing installed Pi executable was accepted")
	}
}

func TestPiNativeInstructionsAndPermissionMapping(t *testing.T) {
	root := t.TempDir()
	opts := Options{StateDir: root, Provider: ProviderConfig{ID: "pi"}, WorkassToolsOrigin: "https://tools.localhost:8788", WorkassToolsCommand: filepath.Join(root, "workass"), WorkassToolsCAFile: filepath.Join(root, "ca.pem")}
	m := NewManager(opts)
	t.Cleanup(func() { m.Reset() })
	b := newBridge("pi-instructions", opts, m)
	prepared, err := b.prepareNativeInstructions(ProviderConfig{Env: map[string]string{"WORKASS_PI_EXECUTABLE": "/fixture/pi"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(b.nativeInstructionsDir) })
	data, err := os.ReadFile(prepared.Env["WORKASS_INSTRUCTIONS_FILE"])
	if err != nil || string(data) != workassNativeInstructions {
		t.Fatal("native Pi did not receive central instructions", err)
	}
	adapter := providerAdapterForID("pi")
	policy := adapter.permission
	if policy.Intent("native") != "full" || policy.Intent("default") != "" || len(policy.Candidates("read")) != 0 || len(policy.Candidates("edit")) != 0 {
		t.Fatal("Pi invented native permission modes")
	}
	if !adapter.creation.DeferredUntilInput || adapter.input.StandardACPActivity() {
		t.Fatal("Pi creation must wait for its explicit durable receipt")
	}
}
