//go:build !windows

package acp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func sampleProcessRSS(ctx context.Context, pid int) (int, error) {
	out, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return 0, errors.New("empty ps rss output")
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0, errors.New("empty ps rss output")
	}
	rssKb, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("parse ps rss %q: %w", fields[0], err)
	}
	return rssKb, nil
}
