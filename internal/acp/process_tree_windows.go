//go:build windows

package acp

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// stopProcessTree owns the whole ACP subprocess tree. Windows Process.Kill
// terminates only the entry process, while native CLIs may leave helper
// descendants behind. taskkill /T closes the exact tree rooted at that PID.
func stopProcessTree(process *os.Process) error {
	if process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "taskkill.exe", "/PID", strconv.Itoa(process.Pid), "/T", "/F").Run(); err == nil {
		return nil
	}
	return process.Kill()
}
