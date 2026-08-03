package acp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAllChatHistoryAcceptsImageBearingRecordOverLegacyScannerLimit(t *testing.T) {
	stateDir := t.TempDir()
	archiveDir := filepath.Join(stateDir, "chat-archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("create archive dir: %v", err)
	}

	encodedImage := strings.Repeat("a", 1400*1024)
	largeAssistant := map[string]any{
		"id":      "assistant-with-images",
		"role":    "assistant",
		"status":  "done",
		"at":      "2026-07-21T00:00:01Z",
		"content": "calibration images are ready",
		"images": []any{
			map[string]any{"mimeType": "image/png", "data": encodedImage},
			map[string]any{"mimeType": "image/png", "data": encodedImage},
			map[string]any{"mimeType": "image/png", "data": encodedImage},
		},
	}
	largeRecord, err := json.Marshal(largeAssistant)
	if err != nil {
		t.Fatalf("marshal large assistant: %v", err)
	}
	if len(largeRecord) <= 4*1024*1024 {
		t.Fatalf("fixture record = %d bytes, want over legacy 4 MiB scanner limit", len(largeRecord))
	}

	var archive bytes.Buffer
	encoder := json.NewEncoder(&archive)
	for _, record := range []any{
		map[string]any{"id": "before", "role": "user", "status": "done", "at": "2026-07-21T00:00:00Z", "content": "show the images"},
		largeAssistant,
		map[string]any{"id": "after", "role": "user", "status": "done", "at": "2026-07-21T00:00:02Z", "content": "continue"},
	} {
		if err := encoder.Encode(record); err != nil {
			t.Fatalf("encode archive record: %v", err)
		}
	}
	archivePath := filepath.Join(archiveDir, "image-tab.jsonl")
	if err := os.WriteFile(archivePath, archive.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	history, err := readAllChatHistory(stateDir, "image-tab")
	if err != nil {
		t.Fatalf("read image-bearing archive: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history records = %d, want 3", len(history))
	}
	if history[1].ID != "assistant-with-images" || history[1].Content != "calibration images are ready" {
		t.Fatalf("large assistant history = %#v", history[1])
	}
	if history[2].ID != "after" {
		t.Fatalf("reader did not continue after large record: %#v", history[2])
	}

	bounded := readChatArchive(stateDir, "image-tab", 120000)
	if len(bounded) != 3 || bounded[1].ID != "assistant-with-images" || bounded[2].ID != "after" {
		t.Fatalf("prompt archive reader truncated image-bearing history: %#v", bounded)
	}
}
