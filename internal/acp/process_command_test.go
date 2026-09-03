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
	processTreeSource, err := os.ReadFile(filepath.Join(dir, "process_tree_windows.go"))
	if err != nil {
		t.Fatal(err)
	}
	treeText := string(processTreeSource)
	for _, required := range []string{
		"CreateJobObjectW",
		"SetInformationJobObject",
		"AssignProcessToJobObject",
		"JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE",
		"cmd.SysProcAttr.CreationFlags |= createSuspended",
		"ResumeThread",
		"TerminateJobObject",
	} {
		if !strings.Contains(treeText, required) {
			t.Fatalf("Windows process-tree ownership is missing %q", required)
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
		legacyTreeKiller := strings.Join([]string{"task", "kill"}, "")
		if strings.Contains(strings.ToLower(source), legacyTreeKiller) {
			t.Errorf("%s launches or references the legacy shell tree killer instead of native process ownership", name)
		}
		if strings.Contains(source, "exec.Command(") || strings.Contains(source, "exec.CommandContext(") {
			t.Errorf("%s bypasses the managed subprocess boundary", name)
		}
	}
}

func TestWindowsCommandScriptInvocationUsesHiddenShellBoundary(t *testing.T) {
	command, args := windowsCommandScriptInvocation(
		`C:\\Users\\Example User\\AppData\\Roaming\\npm\\omp.cmd`,
		[]string{"acp"},
		`C:\\Windows\\System32\\cmd.exe`,
	)
	if command != `C:\\Windows\\System32\\cmd.exe` {
		t.Fatalf("command shell = %q", command)
	}
	want := []string{
		"/d", "/s", "/c",
		`call "C:\\Users\\Example User\\AppData\\Roaming\\npm\\omp.cmd" "acp"`,
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Windows command-script argv = %#v, want %#v", args, want)
	}
}
