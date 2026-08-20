package acp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProviderSpawnedWorkAdapterDecodesTypedSignals(t *testing.T) {
	strategy := providerAdapterForID("claude").spawnedWork
	output := filepath.Join(os.TempDir(), "claude-fixture", "project", "session", "tasks", "task-42.output")
	signal, ok := strategy.DecodeTool(providerRawToolObservation{
		Title: "Bash",
		RawInput: map[string]any{
			"run_in_background": true,
		},
		Meta:   map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}},
		Output: "Command running in background with ID: task-42. Output is being written to: " + output,
	})
	if !ok || !signal.RunsInBackground || signal.ProviderTool != "Bash" || signal.FallbackTaskID != "task-42" || signal.FallbackOutputFile != output {
		t.Fatalf("tool signal = %#v, recognized=%v", signal, ok)
	}

	update, ok := strategy.DecodeLifecycle(map[string]any{
		"type": "snapshot",
		"tasks": []any{map[string]any{
			"taskId": "task-42", "toolCallId": "tool-9", "taskType": "bash", "status": "running", "outputFile": output,
		}},
	})
	if !ok || update.Kind != "snapshot" || !update.TasksKnown || len(update.Tasks) != 1 || update.Tasks[0].TaskID != "task-42" || update.Tasks[0].ToolCallID != "tool-9" || update.Tasks[0].OutputFile != output {
		t.Fatalf("lifecycle update = %#v, recognized=%v", update, ok)
	}
}

func TestProviderSpawnedWorkAdaptersRejectUnregisteredAndUnsafeData(t *testing.T) {
	generic := providerAdapterForID("custom").spawnedWork
	if generic.Supported() {
		t.Fatal("generic ACP adapter advertised provider-owned spawned work")
	}
	if _, ok := generic.DecodeTool(providerRawToolObservation{
		RawInput: map[string]any{"run_in_background": true},
	}); ok {
		t.Fatal("generic ACP adapter consumed vendor tool input")
	}
	if _, ok := generic.DecodeLifecycle(map[string]any{"type": "started", "taskId": "task-1"}); ok {
		t.Fatal("generic ACP adapter consumed vendor lifecycle data")
	}

	strategy := providerAdapterForID("claude").spawnedWork
	unsafe := filepath.Join(os.TempDir(), "unrelated", "tasks", "task-1.output")
	if path, ok := strategy.ValidateOutputPath("task-1", unsafe); ok || path != "" {
		t.Fatalf("adapter accepted unowned output path %q", path)
	}
	if path, ok := strategy.ValidateOutputPath("../task-1", filepath.Join(os.TempDir(), "claude-fixture", "tasks", "task-1.output")); ok || path != "" {
		t.Fatalf("adapter accepted unsafe task id %q", path)
	}
}
