package procchild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The refusals §13 P4 asks for: "preflight refusal in seconds for a missing
// input / dangling symlink / missing tool". Seconds is the assertion that
// matters as much as the refusal — the whole point is that a lane that would
// have died in hour three dies before it queues.

func TestPreflightRefusesMissingInputDanglingSymlinkAndMissingTool(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.txt")
	if err := os.WriteFile(present, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "never-existed"), dangling); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "job.output")

	started := time.Now()
	refusals := Preflight(PreflightRequest{
		Command:    []string{"workass-tool-that-is-not-installed-anywhere"},
		Inputs:     []string{present, empty, dangling, filepath.Join(dir, "absent.txt")},
		OutputFile: output,
	})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("preflight took %s; a refusal must arrive in seconds, not after the work would have", elapsed)
	}
	byCheck := map[string][]Refusal{}
	for _, refusal := range refusals {
		byCheck[refusal.Check] = append(byCheck[refusal.Check], refusal)
	}
	if len(byCheck["tool"]) != 1 {
		t.Fatalf("want one tool refusal, got %v", refusals)
	}
	if len(byCheck["symlink"]) != 1 || !strings.Contains(byCheck["symlink"][0].Subject, "dangling") {
		t.Fatalf("want one dangling-symlink refusal naming the link, got %v", refusals)
	}
	inputs := map[string]string{}
	for _, refusal := range byCheck["input"] {
		inputs[filepath.Base(refusal.Subject)] = refusal.Reason
	}
	if reason := inputs["absent.txt"]; !strings.Contains(reason, "does not exist") {
		t.Fatalf("a missing input must be refused as missing, got %q", reason)
	}
	if reason := inputs["empty.txt"]; !strings.Contains(reason, "empty") {
		// The staging failure every existence check passes.
		t.Fatalf("a zero-byte input must be refused as empty, got %q", reason)
	}
	if _, refused := inputs["present.txt"]; refused {
		t.Fatalf("a real non-empty input must not be refused: %v", refusals)
	}
}

func TestPreflightAcceptsAResolvedSymlinkAndANonEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.bin")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	populated := filepath.Join(dir, "shards")
	if err := os.MkdirAll(populated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(populated, "0"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if refusals := Preflight(PreflightRequest{
		Command: []string{"sh"}, Inputs: []string{link, populated},
		OutputFile: filepath.Join(dir, "out"),
	}); len(refusals) > 0 {
		t.Fatalf("a resolved symlink and a populated directory must pass: %v", refusals)
	}
}

func TestPreflightRefusesAnEmptyDirectoryAndAnUnwritableOutput(t *testing.T) {
	dir := t.TempDir()
	emptyDir := filepath.Join(dir, "shards")
	if err := os.MkdirAll(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	refusals := Preflight(PreflightRequest{
		Command: []string{"sh"}, Inputs: []string{emptyDir},
		OutputFile: filepath.Join(dir, "no-such-directory", "out"),
	})
	checks := map[string]bool{}
	for _, refusal := range refusals {
		checks[refusal.Check] = true
	}
	if !checks["input"] || !checks["output"] {
		t.Fatalf("want an empty-directory and an output-directory refusal, got %v", refusals)
	}
}

func TestPreflightRefusesBelowTheDeclaredDiskFloor(t *testing.T) {
	dir := t.TempDir()
	// A floor no machine can satisfy, so this asserts the comparison rather
	// than the host's free space.
	refusals := Preflight(PreflightRequest{
		Command: []string{"sh"}, OutputFile: filepath.Join(dir, "out"),
		MinFreeDiskBytes: 1 << 62,
	})
	if len(refusals) != 1 || refusals[0].Check != "disk" {
		t.Fatalf("want one disk refusal, got %v", refusals)
	}
	if refusals := Preflight(PreflightRequest{
		Command: []string{"sh"}, OutputFile: filepath.Join(dir, "out"), MinFreeDiskBytes: 1,
	}); len(refusals) > 0 {
		t.Fatalf("a floor of one byte must pass on any machine: %v", refusals)
	}
}

func TestParseBytesReadsHumanFloors(t *testing.T) {
	for value, want := range map[string]uint64{
		"": 0, "512": 512, "50G": 50 << 30, "500M": 500 << 20,
		"1T": 1 << 40, "2Gi": 2 << 30, "4k": 4 << 10,
	} {
		got, err := ParseBytes(value)
		if err != nil || got != want {
			t.Fatalf("ParseBytes(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	if _, err := ParseBytes("plenty"); err == nil {
		t.Fatal("a floor that is not a byte size must be refused, not read as zero")
	}
}
