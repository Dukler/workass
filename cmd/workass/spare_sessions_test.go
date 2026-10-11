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
