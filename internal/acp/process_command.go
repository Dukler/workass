package acp

import (
	"context"
	"os/exec"
	"strings"
)

// managedCommand is the only launch boundary for daemon-owned subprocesses.
// Windows specializes it so console executables never create a visible window;
// other platforms keep Go's ordinary exec behavior.
func managedCommand(name string, args ...string) *exec.Cmd {
	name, args = managedCommandInvocation(name, args)
	return configureManagedCommand(exec.Command(name, args...))
}

func managedCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	name, args = managedCommandInvocation(name, args)
	return configureManagedCommand(exec.CommandContext(ctx, name, args...))
}

// windowsCommandScriptInvocation is kept platform-agnostic so its exact argv
// contract is testable on the Mac release builder. CreateProcess cannot execute
// text .cmd/.bat launchers directly; cmd.exe owns only that file-extension seam
// and receives no provider prompt or other dynamic turn content.
func windowsCommandScriptInvocation(command string, args []string, commandShell string) (string, []string) {
	commandShell = strings.TrimSpace(commandShell)
	if commandShell == "" {
		commandShell = "cmd.exe"
	}
	parts := make([]string, 0, len(args)+2)
	parts = append(parts, "call", quoteWindowsCommandScriptArg(command))
	for _, arg := range args {
		parts = append(parts, quoteWindowsCommandScriptArg(arg))
	}
	return commandShell, []string{"/d", "/s", "/c", strings.Join(parts, " ")}
}

func quoteWindowsCommandScriptArg(value string) string {
	// A Windows path cannot contain a quote. Provider argv is configuration, not
	// model-authored text; doubling quotes preserves the only remaining literal
	// form accepted by cmd's quoted CALL syntax.
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
