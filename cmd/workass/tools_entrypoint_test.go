package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/toolcli"
)

func TestDaemonToolsEntrypointDispatchesBeforeStartup(t *testing.T) {
	if os.Getenv("WORKASS_DAEMON_TOOLS_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
				main()
				os.Exit(0)
			}
		}
		os.Exit(99)
	}
	for _, command := range []string{"guide", "invalid"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			guide := filepath.Join(root, "guide.md")
			const wantGuide = "fixture Workass tools guide\n"
			if err := os.WriteFile(guide, []byte(wantGuide), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonToolsEntrypointDispatchesBeforeStartup$", "--", "tools", command)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "WORKASS_DAEMON_TOOLS_HELPER=1", "WORKASS_TOOLS_GUIDE="+guide, "WORKASS_TOOL_CONTEXT=", "WORKASS_PROFILE=test", "WORKASS_TEST_ROOT="+root)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if command == "guide" {
				if err != nil || stdout.String() != wantGuide || stderr.Len() != 0 {
					t.Fatalf("guide: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				}
			} else {
				var response toolcli.Response
				if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 || stdout.Len() != 0 || json.Unmarshal(stderr.Bytes(), &response) != nil || response.Error != "unknown tools command: use list or call" {
					t.Fatalf("invalid command: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "guide.md" {
				t.Fatalf("tools command created daemon startup files: %v, %v", entries, err)
			}
		})
	}
}
