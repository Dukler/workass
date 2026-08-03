package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionStoreQuarantinesLegacySubagentJournals(t *testing.T) {
	for _, shape := range []struct {
		name      string
		withStart bool
	}{
		{name: "prepare-only"},
		{name: "prepare-and-start", withStart: true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			stateDir := t.TempDir()
			statePath := filepath.Join(stateDir, sessionStateFilename)
			turnID := "legacy-" + shape.name
			tabID := subagentTabPrefix + "wa-subagent-legacy"
			records := []map[string]any{{
				"v": json.Number("1"), "seq": json.Number("1"), "turnId": turnID,
				"kind": "prepare", "tabId": tabID, "chatId": tabID,
				"prompt": "legacy internal prompt", "userId": "legacy-user",
				"assistantId": turnID, "startedAt": "2026-07-24T10:00:00Z",
				"chat": map[string]any{
					"id": tabID, "chatId": tabID, "title": "Nuevo chat",
				},
				"user": map[string]any{
					"id": "legacy-user", "role": "user", "content": "legacy internal prompt",
					"status": "done", "at": "2026-07-24T10:00:00Z", "events": []any{},
				},
				"assistant": map[string]any{
					"id": turnID, "role": "assistant", "content": "",
					"status": "running", "at": nil, "events": []any{},
				},
			}}
			if shape.withStart {
				records = append(records, map[string]any{
					"v": json.Number("1"), "seq": json.Number("2"), "turnId": turnID,
					"kind": "start", "job": map[string]any{
						"id": "legacy-subagent-job", "tabId": tabID, "chatId": tabID,
						"status": "running", "startedAt": "2026-07-24T10:00:00Z",
					},
				})
			}
			writer := &sessionStore{path: statePath}
			journalPath := writer.journalPath(turnID)
			if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
				t.Fatalf("create journal dir: %v", err)
			}
			file, err := os.OpenFile(journalPath, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatalf("create legacy journal: %v", err)
			}
			for _, record := range records {
				data, marshalErr := json.Marshal(record)
				if marshalErr != nil {
					file.Close()
					t.Fatalf("marshal legacy journal: %v", marshalErr)
				}
				if _, writeErr := file.Write(append(data, '\n')); writeErr != nil {
					file.Close()
					t.Fatalf("write legacy journal: %v", writeErr)
				}
			}
			if err := file.Close(); err != nil {
				t.Fatalf("close legacy journal: %v", err)
			}

			reloaded := newSessionStore(statePath)
			var quarantineErr *sessionJournalQuarantineError
			if err := reloaded.LoadError(); !errors.As(err, &quarantineErr) || quarantineErr.Count != 1 {
				t.Fatalf("load error = %v, want one quarantined internal journal", err)
			}
			if chats := anySlice(reloaded.Get().(map[string]any)["chats"]); len(chats) != 0 {
				t.Fatalf("legacy internal journal materialized chats: %#v", chats)
			}
			quarantinedPath := filepath.Join(
				stateDir, sessionJournalDirname, sessionJournalQuarantineDir, filepath.Base(journalPath),
			)
			if _, err := os.Stat(quarantinedPath); err != nil {
				t.Fatalf("quarantined internal journal missing: %v", err)
			}
			if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
				t.Fatalf("legacy journal remains at replay path: %v", err)
			}
		})
	}
}
