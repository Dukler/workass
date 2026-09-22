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

func TestOMPBridgeSteersNativeSDKWithoutQueueOrInterrupt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable for native host fixture")
	}
	root, state := repoRoot(t), t.TempDir()
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root, StateDir: state, Broadcast: events.Broadcast,
		Provider: ProviderConfig{ID: "omp", Command: node, Args: []string{filepath.Join(root, "scripts", "omp-native-host.mjs")}, CWD: root,
			Env: map[string]string{"WORKASS_OMP_SDK_MODULE": filepath.Join(root, "desktop", "acp", "mock-omp-sdk.mjs"), "WORKASS_OMP_FIXTURE_DIR": state}},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newMockSession(t, manager, "omp-steer-tab")
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	capabilities := providerAdapterForID("omp").delivery.Capabilities(bridge)
	if !capabilities.LiveSteer || capabilities.SteerConsumptionReceipt {
		t.Fatalf("native OMP steering capabilities = %#v", capabilities)
	}
	job := startAppChatJob(t, manager, session.SessionID, "omp-steer-tab", "[fixture:wait]")
	journal := filepath.Join(state, session.SessionID+".jsonl")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(journal)
		if strings.Contains(string(data), "[fixture:wait]") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native OMP prompt did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	result := manager.Steer(session.SessionID, "one direction", nil, "steer-user")
	if result["ok"] != true || result["live"] != true || result["queued"] != false || result["strategy"] != "omp-live" || asString(result["turnId"]) == "" {
		t.Fatalf("native OMP admission = %#v", result)
	}
	data, err := os.ReadFile(journal)
	if err != nil || strings.Count(string(data), `"steering":true`) != 1 || strings.Count(string(data), `"role":"user"`) != 2 {
		t.Fatalf("expected one initial prompt and one native steer: %v\n%s", err, data)
	}
	rejected := manager.Steer(session.SessionID, "REJECT", nil, "rejected-user")
	if rejected["ok"] != false || rejected["queued"] != false || rejected["strategy"] != "rejected" {
		t.Fatalf("native OMP rejection = %#v", rejected)
	}
	if !manager.CancelJobResult(jobID(job)).Cancelled {
		t.Fatal("steering must leave the original turn running")
	}
	events.waitJobEnd(t, jobID(job), 3*time.Second)
}

func TestOMPNativeHostContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable for native host fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "scripts/tests/omp-native-host.test.mjs")
	cmd.Dir = repoRoot(t)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("OMP native host contract: %v\n%s", err, output)
	}
}

func TestOMPInstalledHostContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable for installed host fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "scripts/tests/omp-native-host.test.mjs")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "WORKASS_TEST_INSTALLED_OMP=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("OMP installed host contract: %v\n%s", err, output)
	}
}

func TestOMPHostUsesInstalledCommandAndSharedNode(t *testing.T) {
	root := t.TempDir()
	runtimeDir := t.TempDir()
	bundle := filepath.Join(runtimeDir, "frontier-hosts", runtime.GOOS+"-"+runtime.GOARCH)
	host := filepath.Join(bundle, "omp-installed-host.mjs")
	for _, file := range []string{host, filepath.Join(root, "scripts", "omp-installed-host.mjs")} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, file, "// fixture\n")
	}
	node := filepath.Join(bundle, executableName("node"))
	writeExecutable(t, node, nativeNoopScript())
	ompp := filepath.Join(runtimeDir, executableName("omp"))
	writeExecutable(t, ompp, nativeNoopScript())
	for _, key := range []string{"WORKASS_OMP_HOST", "WORKASS_OMP_SDK_MODULE", "WORKASS_BUN", "WORKASS_NODE"} {
		t.Setenv(key, "")
	}
	t.Setenv("WORKASS_NODE", node)
	input := ProviderConfig{ID: "omp", Command: ompp, Args: []string{"acp"}, Env: map[string]string{"FIXTURE": "keep"}}
	got, err := ompNativeHostLaunch(input, Options{RootDir: root}, filepath.Join(runtimeDir, "workass"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != node || len(got.Args) != 1 || got.Args[0] != host || got.Env["WORKASS_OMP_EXECUTABLE"] != ompp || got.Env["FIXTURE"] != "keep" {
		t.Fatalf("incorrect native launch: %#v", got)
	}
	if input.Env["WORKASS_OMP_EXECUTABLE"] != "" {
		t.Fatal("mutated provider environment")
	}
}

func TestOMPRejectsMissingInstalledExecutable(t *testing.T) {
	root := t.TempDir()
	host := filepath.Join(root, "scripts", "omp-native-host.mjs")
	os.MkdirAll(filepath.Dir(host), 0o755)
	writeFile(t, host, "// fixture")
	t.Setenv("WORKASS_OMP_HOST", host)
	t.Setenv("WORKASS_OMP_SDK_MODULE", filepath.Join(root, "sdk.mjs"))
	t.Setenv("WORKASS_BUN", filepath.Join(root, "bun"))
	if _, err := ompNativeHostLaunch(ProviderConfig{ID: "omp", Command: filepath.Join(root, "missing-omp")}, Options{RootDir: root}, filepath.Join(root, "workass")); err == nil {
		t.Fatal("missing installed OMP executable was accepted")
	}
}

func TestOMPNativeInstructionsAndPermissionMapping(t *testing.T) {
	root := t.TempDir()
	opts := Options{StateDir: root, Provider: ProviderConfig{ID: "omp"}, WorkassToolsOrigin: "https://tools.localhost:8788", WorkassToolsCommand: filepath.Join(root, "workass"), WorkassToolsCAFile: filepath.Join(root, "ca.pem")}
	m := NewManager(opts)
	t.Cleanup(func() { m.Reset() })
	b := newBridge("omp-instructions", opts, m)
	prepared, err := b.prepareNativeInstructions(ProviderConfig{Env: map[string]string{"WORKASS_OMP_SDK_MODULE": "/fixture/sdk.mjs"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(b.nativeInstructionsDir) })
	data, err := os.ReadFile(prepared.Env["WORKASS_INSTRUCTIONS_FILE"])
	if err != nil || string(data) != workassNativeInstructions {
		t.Fatal("native OMP did not receive central instructions", err)
	}
	policy := providerAdapterForID("omp").permission
	if policy.Intent("default") != "" || policy.Intent("yolo") != "full" || policy.Intent("plan") != "read" {
		t.Fatal("OMP native defaults misrepresented permissions")
	}
	if got := policy.Candidates("read"); len(got) != 1 || got[0] != "plan" {
		t.Fatal("read-only can silently fall back to default")
	}
}
