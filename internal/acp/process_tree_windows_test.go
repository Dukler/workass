//go:build windows

package acp

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	processTreeTestMode = "WORKASS_PROCESS_TREE_TEST_MODE"
	processTreeTestPID  = "WORKASS_PROCESS_TREE_TEST_PID_FILE"
	waitObjectZero      = 0
	processSynchronize  = 0x00100000
)

// TestWindowsJobObjectTerminatesDescendants is both a test and its own helper
// executable. The outer test starts a parent under startProcessTree; that
// parent starts a child normally, proving ordinary descendants inherit the
// Job Object before TerminateJobObject is asked to close the tree.
func TestWindowsJobObjectTerminatesDescendants(t *testing.T) {
	switch os.Getenv(processTreeTestMode) {
	case "parent":
		child := managedCommand(os.Args[0], "-test.run=^TestWindowsJobObjectTerminatesDescendants$")
		child.Env = processTreeTestEnvironment("child", os.Getenv(processTreeTestPID))
		if err := child.Start(); err != nil {
			t.Fatalf("start descendant: %v", err)
		}
		_ = child.Wait()
		return
	case "child":
		pidFile := os.Getenv(processTreeTestPID)
		if pidFile == "" {
			t.Fatal("descendant PID file is unavailable")
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatalf("write descendant PID: %v", err)
		}
		time.Sleep(30 * time.Second)
		return
	}

	pidFile := t.TempDir() + `\descendant.pid`
	root := managedCommand(os.Args[0], "-test.run=^TestWindowsJobObjectTerminatesDescendants$")
	root.Env = processTreeTestEnvironment("parent", pidFile)
	tree, err := startProcessTree(root)
	if err != nil {
		t.Fatalf("start managed process tree: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = stopProcessTree(root.Process, tree)
			_ = root.Wait()
		}
	}()

	childPID, err := waitForProcessTreeTestPID(pidFile, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	childHandle, err := syscall.OpenProcess(processSynchronize, false, uint32(childPID))
	if err != nil {
		t.Fatalf("open descendant %d: %v", childPID, err)
	}
	defer syscall.CloseHandle(childHandle)

	if err := stopProcessTree(root.Process, tree); err != nil {
		t.Fatalf("terminate managed process tree: %v", err)
	}
	stopped = true
	_ = root.Wait()
	result, err := syscall.WaitForSingleObject(childHandle, 5_000)
	if err != nil {
		t.Fatalf("wait for descendant termination: %v", err)
	}
	if result != waitObjectZero {
		t.Fatalf("descendant %d survived Job Object termination (wait result %#x)", childPID, result)
	}
}

func processTreeTestEnvironment(mode, pidFile string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, processTreeTestMode+"=") || strings.HasPrefix(entry, processTreeTestPID+"=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, processTreeTestMode+"="+mode, processTreeTestPID+"="+pidFile)
}

func waitForProcessTreeTestPID(path string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, fmt.Errorf("descendant did not publish its PID within %s", timeout)
}
