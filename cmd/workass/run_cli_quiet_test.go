package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"workass/internal/procchild"
)

// bug-20261111-dc171619: a --quiet supervisor stopped reporting after an app
// update and the reason went nowhere. Quiet keeps the lines off the caller's
// stream and puts them next to the job's output.
func TestAQuietRunKeepsItsDiagnosticsNextToTheOutput(t *testing.T) {
	state := t.TempDir()
	writer := quietSupervisorLog(state, "op-quiet-log", "nf-watch-mac", "")
	fmt.Fprintln(writer, "workass run: NOT REPORTING — refused for a reason worth keeping")
	if closer, ok := writer.(interface{ Close() error }); ok {
		_ = closer.Close()
	} else {
		t.Fatalf("a quiet run must get a log file, got %T", writer)
	}
	childID, err := procchild.IDForOperation("op-quiet-log")
	if err != nil {
		t.Fatal(err)
	}
	output, err := procchild.OutputPath(state, childID, "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(procchild.SupervisorLogPath(output))
	if err != nil {
		t.Fatalf("the supervisor log must exist beside the output: %v", err)
	}
	for _, want := range []string{childID, "nf-watch-mac", "NOT REPORTING"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("the log must hold %q, got %q", want, data)
		}
	}
	if info, err := os.Stat(procchild.SupervisorLogPath(output)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the log is private, got %v %v", info, err)
	}
}
