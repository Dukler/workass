package machineid

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMintsOnceAndIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if !strings.HasPrefix(first.MachineID, idPrefix) || len(first.MachineID) != len(idPrefix)+32 {
		t.Fatalf("unexpected machine id shape: %q", first.MachineID)
	}
	if first.DisplayName == "" || first.CreatedAt == "" {
		t.Fatalf("identity missing label or mint time: %+v", first)
	}
	for i := 0; i < 3; i++ {
		again, err := Load(dir)
		if err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("identity changed on restart %d: %+v then %+v", i, first, again)
		}
	}
}

func TestLoadGivesEachStateDirItsOwnIdentity(t *testing.T) {
	dev, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("dev load: %v", err)
	}
	prod, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("prod load: %v", err)
	}
	if dev.MachineID == prod.MachineID {
		t.Fatalf("two daemons on one host share an id: %q", dev.MachineID)
	}
}

func TestLoadMintsIntoAStateDirThatDoesNotExistYet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "created", "yet")
	minted, err := Load(dir)
	if err != nil {
		t.Fatalf("load into fresh dir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, FileName)); statErr != nil {
		t.Fatalf("identity was not persisted: %v", statErr)
	}
	if minted.MachineID == "" {
		t.Fatal("minted identity has no id")
	}
}

func TestSetDisplayNameRenamesWithoutReissuingTheID(t *testing.T) {
	dir := t.TempDir()
	original, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	renamed, err := SetDisplayName(dir, "  Builder  ")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.DisplayName != "Builder" {
		t.Fatalf("display name not trimmed/applied: %q", renamed.DisplayName)
	}
	if renamed.MachineID != original.MachineID {
		t.Fatalf("rename reissued the id: %q then %q", original.MachineID, renamed.MachineID)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DisplayName != "Builder" || reloaded.MachineID != original.MachineID {
		t.Fatalf("rename did not persist: %+v", reloaded)
	}
	if _, err := SetDisplayName(dir, "   "); err == nil {
		t.Fatal("an empty display name was accepted")
	}
}

// A hostname change must not reach the id or the label the user already knows.
// Load seeds the label from the hostname only at mint time, so a file that
// already carries a label is returned verbatim.
func TestLoadKeepsThePersistedLabelRatherThanRereadingTheHostname(t *testing.T) {
	dir := t.TempDir()
	stored := Identity{MachineID: idPrefix + strings.Repeat("a", 32), DisplayName: "Old name", CreatedAt: "2026-01-01T00:00:00Z"}
	writeIdentityFile(t, dir, stored)

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded != stored {
		t.Fatalf("identity was rewritten from the environment: %+v", loaded)
	}
}

func TestLoadBackfillsOnlyAMissingLabel(t *testing.T) {
	dir := t.TempDir()
	id := idPrefix + strings.Repeat("b", 32)
	writeIdentityFile(t, dir, Identity{MachineID: id, CreatedAt: "2026-01-01T00:00:00Z"})

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.MachineID != id {
		t.Fatalf("backfill changed the id: %q", loaded.MachineID)
	}
	if loaded.DisplayName == "" {
		t.Fatal("missing label was not backfilled")
	}
}

func TestLoadRefusesToReplaceACorruptIdentityFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
	}{
		{"not json", "{ this is not json"},
		{"no machine id", `{"displayName":"someone"}`},
		{"blank machine id", `{"machineId":"   ","displayName":"someone"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			if err := os.WriteFile(path, []byte(tc.contents), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := Load(dir); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("expected ErrCorrupt, got %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reread: %v", err)
			}
			if string(after) != tc.contents {
				t.Fatalf("a corrupt identity was silently replaced: %q", string(after))
			}
		})
	}
}

func TestWriteLeavesNoTemporaryFilesBehind(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := SetDisplayName(dir, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != FileName {
			t.Fatalf("unexpected leftover in state dir: %q", entry.Name())
		}
	}
}

func writeIdentityFile(t *testing.T, dir string, identity Identity) {
	t.Helper()
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o644); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
}
