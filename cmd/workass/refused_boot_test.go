package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func refusedBootTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = info.ModTime().String() + " " + string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// The real daemon entry point, in every profile: a state directory that says its
// chats live in SQLite refuses a start with SQLite off, and that start leaves the
// directory byte for byte as it was, corrupt diagnostics included. The checks run
// against the whole tree, so identity, certificates and config count too.
func TestRefusedBootWritesNothing(t *testing.T) {
	if os.Getenv("WORKASS_REFUSED_BOOT_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
				main()
				os.Exit(0)
			}
		}
		os.Exit(99)
	}
	for _, profile := range []string{"prod", "dev", "test"} {
		t.Run(profile, func(t *testing.T) {
			root := t.TempDir()
			state := filepath.Join(root, "state")
			backend, err := openChatActorBackend(state, true, daemonVersion, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.close(); err != nil {
				t.Fatal(err)
			}
			corrupt := []byte(`{"version":1,"turns":[{"tabId":"torn"`)
			diagnostics := filepath.Join(state, "turn-diagnostics.json")
			if err := os.WriteFile(diagnostics, corrupt, 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := os.Chtimes(diagnostics, old, old); err != nil {
				t.Fatal(err)
			}
			before := refusedBootTree(t, root)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRefusedBootWritesNothing$", "--",
				"--port", "1", "--beacon=false", "--state-dir", state, "--sqlite-stores=")
			cwd := t.TempDir()
			cmd.Dir = cwd
			home := t.TempDir()
			cmd.Env = append(os.Environ(), "WORKASS_REFUSED_BOOT_HELPER=1", "WORKASS_PROFILE="+profile, "WORKASS_TEST_ROOT=",
				"HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+home, "XDG_DATA_HOME="+home, "XDG_STATE_HOME="+home, "XDG_CACHE_HOME="+home)
			output, err := cmd.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 || !bytes.Contains(output, []byte("stale backup")) {
				t.Fatalf("the start was not refused on the marker: err=%v\n%s", err, output)
			}

			after := refusedBootTree(t, root)
			for name, was := range before {
				if after[name] != was {
					t.Errorf("%s changed", name)
				}
			}
			for name := range after {
				if _, ok := before[name]; !ok {
					t.Errorf("%s was created", name)
				}
			}
			for _, dir := range []string{cwd, home} {
				if files, _ := os.ReadDir(dir); len(files) != 0 {
					t.Errorf("%s gained %v", dir, files)
				}
			}
			if t.Failed() {
				t.Logf("daemon output:\n%s", strings.TrimSpace(string(output)))
			}
		})
	}
}
