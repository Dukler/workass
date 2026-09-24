package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const wireFixtureExecutableSidecarSuffix = ".workass-exec-fixture.json"

type wireFixtureExecutableSidecar struct {
	Script string `json:"script"`
}

// Hardlink the test binary and run the fixture script from a sidecar so macOS
// does not pay the fresh-shebang launch penalty. Keep the script as the oracle.
func writeWireFixtureExecutable(t *testing.T, path, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("write executable %s: %v", path, err)
		}
		return
	}
	if info, err := os.Stat(path); err == nil {
		source, sourceErr := os.Stat(os.Args[0])
		_, sidecarErr := os.Stat(path + wireFixtureExecutableSidecarSuffix)
		if sourceErr != nil || sidecarErr != nil || !os.SameFile(info, source) {
			t.Fatalf("fixture executable path already exists and is not this test binary: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect fixture executable %s: %v", path, err)
	} else if err := os.Link(os.Args[0], path); err != nil {
		if linkErr := os.Symlink(os.Args[0], path); linkErr != nil {
			t.Fatalf("link fixture test executable %s: hardlink: %v; symlink: %v", path, err, linkErr)
		}
	}
	scriptPath := path + ".script"
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatalf("write fixture script %s: %v", scriptPath, err)
	}
	data, err := json.Marshal(wireFixtureExecutableSidecar{Script: scriptPath})
	if err != nil {
		t.Fatalf("encode fixture executable sidecar: %v", err)
	}
	if err := os.WriteFile(path+wireFixtureExecutableSidecarSuffix, data, 0o600); err != nil {
		t.Fatalf("write fixture executable sidecar %s: %v", path, err)
	}
}

func init() {
	data, err := os.ReadFile(os.Args[0] + wireFixtureExecutableSidecarSuffix)
	if err != nil {
		return
	}
	var sidecar wireFixtureExecutableSidecar
	if json.Unmarshal(data, &sidecar) != nil || sidecar.Script == "" {
		os.Exit(126)
	}
	args := []string{"/bin/sh", "-c", ". " + shellQuoteForWire(sidecar.Script), os.Args[0]}
	args = append(args, os.Args[1:]...)
	if err := wireFixtureExecScript(args, os.Environ()); err != nil {
		os.Exit(126)
	}
}

func TestWireFixtureExecutableLauncherPreservesArgumentsAndExitStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-binary fixture launcher uses the Unix exec boundary")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("fixture script launcher requires /bin/sh")
	}
	root := t.TempDir()
	path := filepath.Join(root, "fake-cli")
	argsFile := filepath.Join(root, "args")
	writeWireFixtureExecutable(t, path, "printf '%s\\n' \"$0\" \"$1\" \"$2\" > "+shellQuoteForWire(argsFile)+"\nexit 23\n")
	err := exec.Command(path, "--version", "argument with spaces").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("fixture executable exit = %v, want status 23", err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read forwarded fixture arguments: %v", err)
	}
	if string(got) != path+"\n--version\nargument with spaces\n" {
		t.Fatalf("fixture arguments = %q", got)
	}
}
