package acp

import (
	"context"
	"os/exec"
)

// managedCommand is the only launch boundary for daemon-owned subprocesses.
// Windows specializes it so console executables never create a visible window;
// other platforms keep Go's ordinary exec behavior.
func managedCommand(name string, args ...string) *exec.Cmd {
	return configureManagedCommand(exec.Command(name, args...))
}

func managedCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return configureManagedCommand(exec.CommandContext(ctx, name, args...))
}
