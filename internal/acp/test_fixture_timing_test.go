package acp

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const fixtureExecutableSidecarSuffix = ".workass-exec-fixture.json"

type fixtureExecutableSidecar struct {
	Script string `json:"script"`
}

// Test-binary hardlinks avoid the macOS launch penalty for a freshly written
// executable shebang script. Keep the script itself as the command oracle and
// let /bin/sh run it with the original CLI arguments.
func writeFixtureExecutable(t *testing.T, path, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		writeExecutable(t, path, script)
		return
	}
	if info, err := os.Stat(path); err == nil {
		source, sourceErr := os.Stat(os.Args[0])
		_, sidecarErr := os.Stat(path + fixtureExecutableSidecarSuffix)
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
	if err := os.WriteFile(path+".script", []byte(script), 0o600); err != nil {
		t.Fatalf("write fixture script %s: %v", path, err)
	}
	config, err := json.Marshal(fixtureExecutableSidecar{Script: path + ".script"})
	if err != nil {
		t.Fatalf("encode fixture executable sidecar: %v", err)
	}
	if err := os.WriteFile(path+fixtureExecutableSidecarSuffix, config, 0o600); err != nil {
		t.Fatalf("write fixture executable sidecar %s: %v", path, err)
	}
}

func init() {
	path := os.Args[0] + fixtureExecutableSidecarSuffix
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var sidecar fixtureExecutableSidecar
	if json.Unmarshal(data, &sidecar) != nil || sidecar.Script == "" {
		os.Exit(126)
	}
	args := []string{"/bin/sh", "-c", ". " + shellQuote(sidecar.Script), os.Args[0]}
	args = append(args, os.Args[1:]...)
	if err := fixtureExecScript(args, os.Environ()); err != nil {
		os.Exit(126)
	}
}

func TestFixtureExecutableLauncherPreservesArgumentsAndExitStatus(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("fixture script launcher requires /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "fake-cli")
	argsFile := filepath.Join(t.TempDir(), "args")
	writeFixtureExecutable(t, path, "printf '%s\\n' \"$0\" \"$1\" \"$2\" > "+shellQuote(argsFile)+"\nexit 23\n")
	cmd := exec.Command(path, "--version", "argument with spaces")
	err := cmd.Run()
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
