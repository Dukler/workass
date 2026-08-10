package acp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsManagedProcessBoundaryOwnsEveryPortableCommand(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	dir := filepath.Dir(current)
	windowsSource, err := os.ReadFile(filepath.Join(dir, "process_command_windows.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(windowsSource)
	for _, required := range []string{
		"cmd.SysProcAttr.HideWindow = true",
		"cmd.SysProcAttr.CreationFlags |= createNoWindow",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Windows managed-process policy is missing %q", required)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			name == "process_command.go" || strings.HasSuffix(name, "_unix.go") || strings.HasSuffix(name, "_nonwindows.go") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		source := string(data)
		if strings.Contains(source, "exec.Command(") || strings.Contains(source, "exec.CommandContext(") {
			t.Errorf("%s bypasses the managed subprocess boundary", name)
		}
	}
}
