//go:build windows

package acp

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

func sampleProcessRSS(ctx context.Context, pid int) (int, error) {
	out, err := exec.CommandContext(ctx, "tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return 0, err
	}
	return parseTasklistRSSKB(string(out), pid)
}

func pidString(pid int) string {
	return strconv.Itoa(pid)
}
