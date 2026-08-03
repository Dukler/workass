package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadChatArchiveRecordsAcceptsImageBearingRecordOverLegacyScannerLimit(t *testing.T) {
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

	records := loadChatArchiveRecords(stateDir, "image-tab")
	if len(records) != 3 {
		t.Fatalf("archive records = %d, want 3", len(records))
	}
	if fieldString(records[1], "id") != "assistant-with-images" || stringValue(records[1]["content"]) != "calibration images are ready" {
		t.Fatalf("large assistant archive record was not preserved")
	}
	if fieldString(records[2], "id") != "after" {
		t.Fatalf("reader did not continue after large record: %#v", records[2])
	}
}
