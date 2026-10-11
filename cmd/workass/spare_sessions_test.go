package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestDaemonSpareSessionsAreInertAndLogged(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	if got := daemonSpareSessions(0, logf); got != 0 || len(logs) != 0 {
		t.Fatalf("zero spares: got %d logs %v", got, logs)
	}
	if got := daemonSpareSessions(2, logf); got != 0 {
		t.Fatalf("configured spares must be inert, got %d", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "spare-sessions=2 ignored") {
		t.Fatalf("expected one ignore log, got %v", logs)
	}
}

// 2026-10-11 QA lanes B-12: providers:update {} reported providerId "map".
func TestStringArgIgnoresObjects(t *testing.T) {
	if got := stringArg([]any{map[string]any{}}, 0); got != "" {
		t.Fatalf("object argument read as %q", got)
	}
	if got := stringArg([]any{" claude "}, 0); got != "claude" {
		t.Fatalf("string argument read as %q", got)
	}
}
