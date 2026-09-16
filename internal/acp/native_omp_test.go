package acp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

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
