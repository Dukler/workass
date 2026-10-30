package headlessupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// release builds one headless release archive whose staged daemon is a shell
// script reporting reportedVersion, so the activation contract is exercised
// end to end without a real cross-compiled binary.
func release(t *testing.T, version, reportedVersion string, extra map[string]string) []byte {
	t.Helper()
	raw := &byteSink{}
	gz := gzip.NewWriter(raw)
	writer := tar.NewWriter(gz)
	top := fmt.Sprintf("Workass-%s-linux-amd64", version)
	add := func(name, body string, mode int64) {
		t.Helper()
		if err := writer.WriteHeader(&tar.Header{Name: top + "/" + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	daemon := fmt.Sprintf("#!/bin/sh\nprintf '{\"product\":\"Workass\",\"version\":\"%s\"}\\n'\n", reportedVersion)
	add("workass", daemon, 0o755)
	add("workass-tools", "#!/bin/sh\nexit 0\n", 0o755)
	add("frontier-hosts/linux-amd64/claude-native-host.mjs", "// host "+version+"\n", 0o644)
	add("node/linux-amd64/bin/node", "#!/bin/sh\nexit 0\n", 0o755)
	add("manifest.json", fmt.Sprintf(`{"schemaVersion":2,"platform":"linux","arch":"amd64","version":%q,"headless":true}`+"\n", version), 0o644)
	for name, body := range extra {
		add(name, body, 0o644)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.bytes
}

type byteSink struct{ bytes []byte }

func (s *byteSink) Write(p []byte) (int, error) {
	s.bytes = append(s.bytes, p...)
	return len(p), nil
}

type harness struct {
	server      *httptest.Server
	installRoot string
	stateDir    string
	manager     *Manager
	restarts    int
	commands    [][]string
}

func newHarness(t *testing.T, currentVersion, releaseVersion, reportedVersion string, restart string) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the headless service updater targets POSIX service hosts")
	}
	root := t.TempDir()
	installRoot := filepath.Join(root, "runtime")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(installRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// The install the daemon is running from: the binary plus the trees the
	// release owns.
	write(t, filepath.Join(installRoot, "workass"), "#!/bin/sh\nexit 0\n", 0o755)
	write(t, filepath.Join(installRoot, "workass-tools"), "old tools", 0o755)
	write(t, filepath.Join(installRoot, "frontier-hosts", "linux-amd64", "claude-native-host.mjs"), "// old host\n", 0o644)
	write(t, filepath.Join(installRoot, "node", "linux-amd64", "bin", "npm"), "operator npm", 0o755)

	archive := release(t, releaseVersion, reportedVersion, nil)
	digest := sha256.Sum256(archive)
	h := &harness{installRoot: installRoot, stateDir: stateDir}
	mux := http.NewServeMux()
	archiveName := fmt.Sprintf("Workass-%s-linux-amd64.tar.gz", releaseVersion)
	mux.HandleFunc("/"+archiveName, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	h.server = httptest.NewServer(mux)
	manifest := map[string]any{
		"schemaVersion": 1, "product": "Workass", "version": releaseVersion,
		"platform": "linux", "arch": "amd64", "headless": true, "service": "systemd-user",
		"artifacts": map[string]any{"update": map[string]any{
			"name": archiveName, "url": h.server.URL + "/" + archiveName,
			"sha256": hex.EncodeToString(digest[:]), "size": len(archive),
		}},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(encoded) })

	h.manager = New(Options{
		Executable: filepath.Join(installRoot, "workass"),
		StateDir:   stateDir,
		Version:    currentVersion,
		GOOS:       "linux",
		GOARCH:     "amd64",
		FeedURL:    h.server.URL + "/feed.json",
		Env:        func(string) string { return "" },
		ReadFile: func(name string) ([]byte, error) {
			if name == "/proc/self/cgroup" {
				return []byte("0::/user.slice/user-1000.slice/user@1000.service/app.slice/workass.service\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Run: func(name string, args ...string) ([]byte, error) {
			h.commands = append(h.commands, append([]string{name}, args...))
			return []byte(restart + "\n"), nil
		},
		RequestRestart: func() { h.restarts++ },
	})
	t.Cleanup(h.server.Close)
	return h
}

// rebootAs is the next start of the service: the same install root and state
// dir, a daemon that now reports version. It is how the boot arbiter is tested
// without restarting a real unit.
func (h *harness) rebootAs(t *testing.T, version string) *Manager {
	t.Helper()
	return New(Options{
		Executable: filepath.Join(h.installRoot, "workass"),
		StateDir:   h.stateDir,
		Version:    version,
		GOOS:       "linux",
		GOARCH:     "amd64",
		FeedURL:    h.server.URL + "/feed.json",
		Env:        func(string) string { return "" },
		ReadFile: func(name string) ([]byte, error) {
			if name == "/proc/self/cgroup" {
				return []byte("0::/user.slice/user-1000.slice/user@1000.service/app.slice/workass.service\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Run: func(string, ...string) ([]byte, error) { return []byte("on-failure\n"), nil },
	})
}

func write(t *testing.T, file, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func authorized(machine, current, target, operation string) ApplyRequest {
	return ApplyRequest{
		OperationID:            operation,
		MachineID:              machine,
		ExpectedCurrentVersion: current,
		ExpectedTargetVersion:  target,
		Authorization:          fmt.Sprintf("update %s from %s to %s", machine, current, target),
	}
}

func TestLinuxArtifactSelectionBindsTheExactPlatformFeed(t *testing.T) {
	if got := FeedName("linux", "amd64"); got != "workass-linux-amd64-release.json" {
		t.Fatalf("linux feed name = %q", got)
	}
	if got := FeedName("linux", "arm64"); got != "workass-linux-arm64-release.json" {
		t.Fatalf("linux arm feed name = %q", got)
	}
	url, err := FeedURL("", "linux", "amd64")
	if err != nil || url != DefaultFeedRoot+"workass-linux-amd64-release.json" {
		t.Fatalf("default feed url = %q err=%v", url, err)
	}
	if _, err := FeedURL("file:///etc/passwd", "linux", "amd64"); err == nil {
		t.Fatal("a non-http feed override must be refused")
	}

	base := map[string]any{
		"schemaVersion": 1, "product": "Workass", "version": "1.2.4",
		"platform": "linux", "arch": "amd64", "headless": true,
		"artifacts": map[string]any{"update": map[string]any{
			"name": "a.tar.gz", "url": "https://example.invalid/a.tar.gz",
			"sha256": strings.Repeat("a", 64), "size": 10,
		}},
	}
	encode := func(mutate func(map[string]any)) []byte {
		copied := map[string]any{}
		for key, value := range base {
			copied[key] = value
		}
		mutate(copied)
		raw, err := json.Marshal(copied)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if _, err := ValidateManifest(encode(func(map[string]any) {}), "linux", "amd64"); err != nil {
		t.Fatalf("valid linux manifest rejected: %v", err)
	}
	// The whole point of per-platform feeds: a Linux daemon must never accept
	// the Windows artifact, and an arm64 host must never accept the amd64 one.
	for name, mutate := range map[string]func(map[string]any){
		"windows platform": func(m map[string]any) { m["platform"] = "windows" },
		"other arch":       func(m map[string]any) { m["arch"] = "arm64" },
		"not headless":     func(m map[string]any) { m["headless"] = false },
		"bad digest": func(m map[string]any) {
			m["artifacts"] = map[string]any{"update": map[string]any{"name": "a", "url": "https://x/a", "sha256": "nope", "size": 10}}
		},
		"zero size": func(m map[string]any) {
			m["artifacts"] = map[string]any{"update": map[string]any{"name": "a", "url": "https://x/a", "sha256": strings.Repeat("a", 64), "size": 0}}
		},
	} {
		if _, err := ValidateManifest(encode(mutate), "linux", "amd64"); err == nil {
			t.Fatalf("%s manifest was accepted", name)
		}
	}
	if _, err := ValidateManifest(encode(func(m map[string]any) { m["arch"] = "arm64" }), "linux", "arm64"); err != nil {
		t.Fatalf("valid linux arm64 manifest rejected: %v", err)
	}
}

func TestSystemdUserUnitDiscoveryAndRestartPolicy(t *testing.T) {
	cgroup := func(name string) ([]byte, error) {
		if name == "/proc/self/cgroup" {
			return []byte("0::/user.slice/user-1000.slice/user@1000.service/app.slice/workass.service\n"), nil
		}
		return nil, os.ErrNotExist
	}
	unit, user, err := detectUnit(func(string) string { return "" }, cgroup)
	if err != nil || unit != "workass.service" || !user {
		t.Fatalf("unit=%q user=%v err=%v", unit, user, err)
	}
	if _, _, err := detectUnit(func(string) string { return "../../etc/passwd" }, cgroup); err == nil {
		t.Fatal("a bogus unit override must be refused")
	}
	if _, _, err := detectUnit(func(string) string { return "" }, func(string) ([]byte, error) { return nil, os.ErrNotExist }); err == nil {
		t.Fatal("a process outside a unit must not claim one")
	}
	// Restart= is the activation mechanism, so Restart=no is an unsupported
	// install rather than a machine that gets taken down and left there.
	h := newHarness(t, "1.2.3", "1.2.4", "1.2.4", "no")
	if h.manager.Supported() || !strings.Contains(h.manager.UnsupportedReason(), "Restart=no") {
		t.Fatalf("supported=%v reason=%q", h.manager.Supported(), h.manager.UnsupportedReason())
	}
	if _, err := h.manager.Apply(context.Background(), authorized("m-1", "1.2.3", "1.2.4", "op-restart-no")); err == nil {
		t.Fatal("an install with no restart authority must refuse to activate")
	}
}

func TestHeadlessUpdaterIsUnsupportedOffLinuxServiceInstalls(t *testing.T) {
	manager := New(Options{
		Executable: filepath.Join(t.TempDir(), "workass"),
		StateDir:   t.TempDir(),
		Version:    "1.2.3",
		GOOS:       "darwin",
		GOARCH:     "arm64",
		Env:        func(string) string { return "" },
	})
	if manager.Supported() || !strings.Contains(manager.UnsupportedReason(), "only on Linux") {
		t.Fatalf("darwin supported=%v reason=%q", manager.Supported(), manager.UnsupportedReason())
	}
	status := manager.Status(context.Background())
	state, _ := status["state"].(map[string]any)
	if state["supported"] != false || state["phase"] != "unavailable" {
		t.Fatalf("unsupported status = %#v", state)
	}
}

func TestApplyActivatesSwapsAndSettlesHealthy(t *testing.T) {
	h := newHarness(t, "1.2.3", "1.2.4", "1.2.4", "on-failure")
	if !h.manager.Supported() {
		t.Fatalf("supported=false reason=%q", h.manager.UnsupportedReason())
	}
	if _, err := h.manager.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	status := h.manager.Status(context.Background())
	state := status["state"].(map[string]any)
	if state["availableVersion"] != "1.2.4" || state["targetVersion"] != "1.2.4" || state["phase"] != "available" {
		t.Fatalf("discovery state = %#v", state)
	}

	result, err := h.manager.Apply(context.Background(), authorized("m-duklerver", "1.2.3", "1.2.4", "op-duklerver-1"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result["replayed"] != false {
		t.Fatalf("first apply replayed=%v", result["replayed"])
	}
	// The daemon, the tools client and the frontier hosts are the release's to
	// replace; the operator's fuller Node install is not.
	for file, want := range map[string]string{
		filepath.Join(h.installRoot, "workass-tools"):                                           "exit 0",
		filepath.Join(h.installRoot, "frontier-hosts", "linux-amd64", "claude-native-host.mjs"): "host 1.2.4",
	} {
		body, readErr := os.ReadFile(file)
		if readErr != nil || !strings.Contains(string(body), want) {
			t.Fatalf("%s = %q err=%v", file, string(body), readErr)
		}
	}
	if body, readErr := os.ReadFile(filepath.Join(h.installRoot, "node", "linux-amd64", "bin", "npm")); readErr != nil || string(body) != "operator npm" {
		t.Fatalf("the bundled Node must not replace an existing install: %q err=%v", string(body), readErr)
	}

	receipt := h.manager.readReceipt()
	if receipt == nil || receipt.Phase != "activating" || receipt.PreviousVersion != "1.2.3" || receipt.TargetVersion != "1.2.4" {
		t.Fatalf("receipt after apply = %#v", receipt)
	}
	if receipt.Strategy != Strategy || receipt.Unit != "workass.service" {
		t.Fatalf("receipt identity = %#v", receipt)
	}
	// The reply goes out before the process does.
	deadline := time.Now().Add(5 * time.Second)
	for h.restarts == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if h.restarts != 1 {
		t.Fatalf("restart requests = %d", h.restarts)
	}

	// A lost reply re-asks with the same operation_id and may only read back.
	replay, err := h.manager.Apply(context.Background(), authorized("m-duklerver", "1.2.3", "1.2.4", "op-duklerver-1"))
	if err != nil || replay["replayed"] != true {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}

	// The next boot is the new version: it settles without a restart, and
	// serving is what makes it healthy.
	booted := h.rebootAs(t, "1.2.4")
	if booted.Settle() {
		t.Fatal("a successful activation must not ask for another restart")
	}
	if got := booted.readReceipt(); got.Phase != "activated" || got.BootAttempts != 1 {
		t.Fatalf("receipt after boot = %#v", got)
	}
	booted.Confirm()
	got := booted.readReceipt()
	if got.Phase != "healthy" || got.InstalledVersion != "1.2.4" || !got.Activated {
		t.Fatalf("receipt after confirm = %#v", got)
	}
	if _, err := os.Stat(filepath.Join(h.installRoot, StagingDirName, "staged")); !os.IsNotExist(err) {
		t.Fatalf("staging was not pruned: %v", err)
	}
	if _, err := os.Stat(got.BackupRoot); err != nil {
		t.Fatalf("the rollback backup must survive: %v", err)
	}
}

func TestApplyRefusesUnauthorizedOrMismatchedActivation(t *testing.T) {
	h := newHarness(t, "1.2.3", "1.2.4", "1.2.4", "on-failure")
	if _, err := h.manager.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cases := map[string]ApplyRequest{
		"wrong authorization text": {OperationID: "op-a1b2c3d4", MachineID: "m-1", ExpectedCurrentVersion: "1.2.3", ExpectedTargetVersion: "1.2.4", Authorization: "please update"},
		"loose version":            authorized("m-1", "1.2.3", "v1.2.4", "op-a1b2c3d4"),
		"short operation id":       authorized("m-1", "1.2.3", "1.2.4", "op"),
		"no machine":               authorized("", "1.2.3", "1.2.4", "op-a1b2c3d4"),
		"stale current version":    authorized("m-1", "1.2.2", "1.2.4", "op-a1b2c3d4"),
		"unoffered target":         authorized("m-1", "1.2.3", "1.2.9", "op-a1b2c3d4"),
		"downgrade":                authorized("m-1", "1.2.3", "1.2.3", "op-a1b2c3d4"),
	}
	for name, request := range cases {
		if _, err := h.manager.Apply(context.Background(), request); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if receipt := h.manager.readReceipt(); receipt != nil {
		t.Fatalf("a refused activation must leave no receipt: %#v", receipt)
	}
	if body, err := os.ReadFile(filepath.Join(h.installRoot, "workass-tools")); err != nil || string(body) != "old tools" {
		t.Fatalf("a refused activation must not touch the install: %q err=%v", string(body), err)
	}
}

func TestApplyRefusesAStagedTreeThatDoesNotRunHere(t *testing.T) {
	// The staged daemon reports a different version: the exact shape a wrong
	// architecture, a truncated extract or a mislabelled release takes.
	h := newHarness(t, "1.2.3", "1.2.4", "9.9.9", "on-failure")
	if _, err := h.manager.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	_, err := h.manager.Apply(context.Background(), authorized("m-1", "1.2.3", "1.2.4", "op-preflight-1"))
	if err == nil || !strings.Contains(err.Error(), "reports") {
		t.Fatalf("preflight error = %v", err)
	}
	if body, readErr := os.ReadFile(filepath.Join(h.installRoot, "workass-tools")); readErr != nil || string(body) != "old tools" {
		t.Fatalf("a failed preflight must not replace anything: %q err=%v", string(body), readErr)
	}
	if h.restarts != 0 {
		t.Fatalf("a failed preflight must not restart the service: %d", h.restarts)
	}
}

func TestCrashLoopRollsBackToThePreviousVersion(t *testing.T) {
	h := newHarness(t, "1.2.3", "1.2.4", "1.2.4", "on-failure")
	if _, err := h.manager.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.Apply(context.Background(), authorized("m-1", "1.2.3", "1.2.4", "op-rollback-1")); err != nil {
		t.Fatal(err)
	}
	newTools, err := os.ReadFile(filepath.Join(h.installRoot, "workass-tools"))
	if err != nil || strings.Contains(string(newTools), "old tools") {
		t.Fatalf("swap did not happen: %q err=%v", string(newTools), err)
	}
	// Boot after boot of the new version that never reaches Confirm.
	var restartRequired bool
	for attempt := 1; attempt <= maxBootAttempts+1; attempt++ {
		booted := h.rebootAs(t, "1.2.4")
		restartRequired = booted.Settle()
	}
	if !restartRequired {
		t.Fatal("a crash loop must end in a rollback that asks for one more restart")
	}
	receipt := h.manager.readReceipt()
	if receipt.Phase != "rolled_back" || receipt.InstalledVersion != "1.2.3" || receipt.RollbackError != "" {
		t.Fatalf("rollback receipt = %#v", receipt)
	}
	if body, readErr := os.ReadFile(filepath.Join(h.installRoot, "workass-tools")); readErr != nil || string(body) != "old tools" {
		t.Fatalf("rollback did not restore the install: %q err=%v", string(body), readErr)
	}
	if body, readErr := os.ReadFile(filepath.Join(h.installRoot, "frontier-hosts", "linux-amd64", "claude-native-host.mjs")); readErr != nil || !strings.Contains(string(body), "old host") {
		t.Fatalf("rollback did not restore the hosts: %q err=%v", string(body), readErr)
	}
}

func TestArchiveExtractionRefusesEscapesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	build := func(headers ...*tar.Header) []byte {
		sink := &byteSink{}
		gz := gzip.NewWriter(sink)
		writer := tar.NewWriter(gz)
		for _, header := range headers {
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return sink.bytes
	}
	for name, headers := range map[string][]*tar.Header{
		"parent escape": {{Name: "bundle/../../escape", Mode: 0o644, Typeflag: tar.TypeReg}},
		"absolute path": {{Name: "/etc/passwd", Mode: 0o644, Typeflag: tar.TypeReg}},
		"symlink":       {{Name: "bundle/link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}},
		"two roots": {
			{Name: "bundle/workass", Mode: 0o755, Typeflag: tar.TypeReg},
			{Name: "other/workass", Mode: 0o755, Typeflag: tar.TypeReg},
		},
	} {
		if _, err := ExtractTarGZ(strings.NewReader(string(build(headers...))), root); err == nil {
			t.Fatalf("%s was extracted", name)
		}
	}
}

func TestVersionComparisonRejectsLooseVersions(t *testing.T) {
	if _, err := CompareVersions("1.2", "1.2.3"); err == nil {
		t.Fatal("a two-field version must be refused")
	}
	if order, err := CompareVersions("0.1.232", "0.1.233"); err != nil || order != -1 {
		t.Fatalf("order=%d err=%v", order, err)
	}
	if order, err := CompareVersions("0.2.0", "0.1.999"); err != nil || order != 1 {
		t.Fatalf("order=%d err=%v", order, err)
	}
	if !ValidVersion("0.1.232") || ValidVersion("0.1.0232") || ValidVersion("v1.2.3") {
		t.Fatal("version validation accepted a non-release string")
	}
}

func TestDescribeRunningJobsNamesWhatWouldDie(t *testing.T) {
	jobs := []RunningJob{
		{WorkID: "xwa", Label: "train", Protected: false, Note: "shares the cgroup"},
		{WorkID: "xwb", Label: "sleep", Protected: true, Note: "own scope"},
	}
	described := DescribeRunningJobs(jobs)
	summary, _ := described["summary"].(string)
	if !strings.Contains(summary, "WOULD KILL 1") || !strings.Contains(summary, "train (xwa)") || !strings.Contains(summary, "1 more would survive") {
		t.Fatalf("summary: %q", summary)
	}
	if got := DescribeRunningJobs(jobs[1:])["summary"]; !strings.Contains(got.(string), "protected") {
		t.Fatalf("summary: %v", got)
	}
	if got := DescribeRunningJobs(nil)["summary"]; !strings.Contains(got.(string), "no supervised job") {
		t.Fatalf("summary: %v", got)
	}
}
