package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeReplayContentStripsClaudeCompactionMarkers(t *testing.T) {
	for _, marker := range replayCompactionExactMarkers {
		marker := marker
		t.Run(marker+"/whole-message", func(t *testing.T) {
			if got := sanitizeReplayContent(marker); got != "" {
				t.Fatalf("sanitizeReplayContent(%q) = %q, want empty", marker, got)
			}
		})
		t.Run(marker+"/standalone-line", func(t *testing.T) {
			got := sanitizeReplayContent("before\n\n" + marker + "\n\nafter")
			if got != "before\n\nafter" {
				t.Fatalf("standalone marker sanitized to %q, want before\\n\\nafter", got)
			}
		})
		t.Run(marker+"/leading-fragment", func(t *testing.T) {
			got := sanitizeReplayContent(marker + "real content")
			if got != "real content" {
				t.Fatalf("leading marker sanitized to %q, want real content", got)
			}
		})
		t.Run(marker+"/trailing-fragment", func(t *testing.T) {
			got := sanitizeReplayContent("real content" + marker)
			if got != "real content" {
				t.Fatalf("trailing marker sanitized to %q, want real content", got)
			}
		})
	}

	if got := sanitizeReplayContent("Compacting failed: adapter reported a compaction error"); got != "" {
		t.Fatalf("failed-with-reason marker sanitized to %q, want empty", got)
	}
	if got := sanitizeReplayContent("real contentCompacting failed: adapter reported a compaction error"); got != "real content" {
		t.Fatalf("trailing failed-with-reason marker sanitized to %q, want real content", got)
	}
}

func TestSanitizeReplayContentPreservesFormattingVerbatim(t *testing.T) {
	input := "intro paragraph\n\n```go\nfunc x() {\n\tif y {\n\t\treturn\n\t}\n\n\tdone()\n}\n```\n\noutro paragraph"
	if got := sanitizeReplayContent(input); got != input {
		t.Fatalf("marker-free content was reflowed:\ngot  %q\nwant %q", got, input)
	}
	withMarker := "intro paragraph\n\nCompacting completed.\n\n```go\nfunc x() {\n\tif y {\n\t\treturn\n\t}\n\n\tdone()\n}\n```"
	want := "intro paragraph\n\n```go\nfunc x() {\n\tif y {\n\t\treturn\n\t}\n\n\tdone()\n}\n```"
	if got := sanitizeReplayContent(withMarker); got != want {
		t.Fatalf("marker removal damaged formatting:\ngot  %q\nwant %q", got, want)
	}
}

func TestSanitizeReplayContentPreservesLegitimateCompactingProse(t *testing.T) {
	input := "We are compacting the local archive after the export.\nCompacting the index is normal maintenance prose."
	if got := sanitizeReplayContent(input); got != input {
		t.Fatalf("legitimate prose sanitized to %q, want %q", got, input)
	}
}

func TestSeedBuildersSkipSanitizedEmptyMessages(t *testing.T) {
	history := []historyMessage{
		{Role: "assistant", Content: "Compacting..."},
		{Role: "assistant", Content: "Compacting completed."},
		{Role: "assistant", Content: "Compacting failed: adapter reported a compaction error"},
	}
	if got := buildHistoryBlock(history, 24000); got != "" {
		t.Fatalf("buildHistoryBlock with marker-only history = %q, want empty", got)
	}
	if got, ok := buildNativeDeltaSeed(history, 24000); !ok || got != "" {
		t.Fatalf("buildNativeDeltaSeed with marker-only history = %q ok=%v, want empty ok", got, ok)
	}
}

func TestBuildHistoryBlockSanitizesArchivedCompactionMarkers(t *testing.T) {
	stateDir := t.TempDir()
	archiveDir := filepath.Join(stateDir, "chat-archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}
	path := filepath.Join(archiveDir, "sanitize-tab.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, record := range []map[string]any{
		{"role": "assistant", "content": "Compacting...", "at": "2026-07-16T00:00:00Z"},
		{"role": "assistant", "content": "answer before\n\nCompacting completed.", "at": "2026-07-16T00:00:01Z"},
		{"role": "user", "content": "real user message", "at": "2026-07-16T00:00:02Z"},
		{"role": "assistant", "content": "Compacting...assistant prefix content", "at": "2026-07-16T00:00:03Z"},
		{"role": "assistant", "content": "assistant suffix contentCompacting failed.", "at": "2026-07-16T00:00:04Z"},
		{"role": "assistant", "content": "Compacting failed: adapter reported a compaction error", "at": "2026-07-16T00:00:05Z"},
	} {
		if err := enc.Encode(record); err != nil {
			t.Fatalf("encode archive: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}

	history := promptHistoryForTab(stateDir, JobStartOptions{TabID: "sanitize-tab", HistoryCharBudget: 24000})
	block := buildHistoryBlock(history, 24000)
	assertNoReplayCompactionMarkers(t, block)
	for _, want := range []string{"answer before", "User: real user message", "assistant prefix content", "assistant suffix content"} {
		if !strings.Contains(block, want) {
			t.Fatalf("sanitized history block missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "Assistant: Compacting") {
		t.Fatalf("marker-only assistant message was not skipped:\n%s", block)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw archive: %v", err)
	}
	if !strings.Contains(string(raw), "Compacting completed.") {
		t.Fatalf("raw archive was unexpectedly sanitized: %s", string(raw))
	}
}

func TestBuildNativeDeltaSeedSanitizesCompactionMarkers(t *testing.T) {
	prompt, ok := buildNativeDeltaSeed([]historyMessage{
		{Role: "assistant", Content: "Compacting..."},
		{Role: "user", Content: "real user delta"},
		{Role: "assistant", Content: "real assistant delta\n\nCompacting completed."},
		{Role: "assistant", Content: "assistant suffix deltaCompacting failed."},
		{Role: "assistant", Content: "Compacting failed: adapter reported a compaction error"},
	}, 24000)
	if !ok {
		t.Fatal("delta seed unexpectedly exceeded budget")
	}
	assertNoReplayCompactionMarkers(t, prompt)
	for _, want := range []string{"User: real user delta", "Assistant: real assistant delta", "Assistant: assistant suffix delta"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("sanitized delta seed missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Assistant: Compacting") {
		t.Fatalf("marker-only assistant delta message was not skipped:\n%s", prompt)
	}
}

func assertNoReplayCompactionMarkers(t *testing.T, text string) {
	t.Helper()
	for _, marker := range replayCompactionExactMarkers {
		if strings.Contains(text, marker) {
			t.Fatalf("text contains replay compaction marker %q:\n%s", marker, text)
		}
	}
	if strings.Contains(text, replayCompactionMarkerFailedWithReasonPrefix) {
		t.Fatalf("text contains replay compaction failed-with-reason marker:\n%s", text)
	}
}
