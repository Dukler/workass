//go:build windows

package acp

import (
	"context"
	"fmt"
	"strconv"
)

func sampleProcessRSS(ctx context.Context, pid int) (int, error) {
	out, err := managedCommandContext(ctx, "tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return 0, err
	}
	return parseTasklistRSSKB(string(out), pid)
}

func pidString(pid int) string {
	return strconv.Itoa(pid)
}
