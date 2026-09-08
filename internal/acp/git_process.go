package acp

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// Git may start filters or credential helpers that inherit its output pipes.
// Killing only Git leaves Output waiting for those descendants. Own the whole
// short-lived command tree so Stop also releases the preparation worker.
func gitCommandOutput(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	configureGitProcess(cmd)
	var output, stderr bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	cmd.WaitDelay = 100 * time.Millisecond
	tree, err := startProcessTree(cmd)
	if err != nil {
		return nil, err
	}
	defer releaseProcessTree(tree)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		if errors.Is(err, exec.ErrWaitDelay) {
			_ = stopGitProcessTree(cmd.Process, tree)
		}
	case <-ctx.Done():
		_ = stopGitProcessTree(cmd.Process, tree)
		<-done
		return output.Bytes(), ctx.Err()
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitErr.Stderr = stderr.Bytes()
	}
	return output.Bytes(), err
}
