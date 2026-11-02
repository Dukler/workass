package acp

import (
	"regexp"
	"strings"
)

var codexNativeAgentID = regexp.MustCompile(`^codex-agent-[0-9a-f]{24}$`)

// A command the app-server reports as still in progress when its turn ended
// (unified exec): the host names it after its item id.
var codexBackgroundExecID = regexp.MustCompile(`^codex-exec-[A-Za-z0-9_.:-]{1,88}$`)
var codexNativeWorkRunID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The native host attests child identity and lifecycle from app-server events.
// No prose parsing, output-file ownership, or Workass subagent control is inferred.
type codexProviderSpawnedWorkStrategy struct{}

func (codexProviderSpawnedWorkStrategy) Supported() bool { return true }

func (codexProviderSpawnedWorkStrategy) DecodeTool(raw providerRawToolObservation) (providerSpawnToolSignal, bool) {
	meta := mapFromAny(raw.Meta["workassSubagent"])
	if !boolFromAny(meta["header"]) || !codexNativeAgentID.MatchString(asString(meta["id"])) {
		return providerSpawnToolSignal{}, false
	}
	return providerSpawnToolSignal{ProviderTool: "agent", RunsInBackground: true}, true
}

func (codexProviderSpawnedWorkStrategy) DecodeLifecycle(raw any) (providerSpawnedWorkUpdate, bool) {
	event := mapFromAny(raw)
	kind := asString(event["type"])
	if kind != "started" && kind != "progress" {
		return providerSpawnedWorkUpdate{}, false
	}
	taskID, toolID := asString(event["taskId"]), asString(event["toolCallId"])
	if codexBackgroundExecID.MatchString(taskID) {
		return decodeCodexBackgroundExec(event, kind, taskID, toolID)
	}
	if !codexNativeAgentID.MatchString(toolID) || !strings.HasPrefix(taskID, toolID+"-run-") || len(taskID) > 100 {
		return providerSpawnedWorkUpdate{}, false
	}
	if !codexNativeWorkRunID.MatchString(strings.TrimPrefix(taskID, toolID+"-run-")) {
		return providerSpawnedWorkUpdate{}, false
	}
	status := asString(event["status"])
	switch status {
	case "running", "completed", "failed", "stopped":
	default:
		return providerSpawnedWorkUpdate{}, false
	}
	return providerSpawnedWorkUpdate{Kind: kind, Task: providerSpawnedWorkTask{
		TaskID: taskID, ToolCallID: toolID, TaskType: "agent", Status: status,
		Description:  compactText(redactSensitiveText(asString(event["description"])), 240),
		ModelLabel:   compactText(redactSensitiveText(asString(event["modelLabel"])), 160),
		Summary:      compactText(redactSensitiveText(asString(event["summary"])), 1000),
		LastToolName: compactText(redactSensitiveText(asString(event["lastToolName"])), 120),
	}}, true
}

func decodeCodexBackgroundExec(event map[string]any, kind, taskID, toolID string) (providerSpawnedWorkUpdate, bool) {
	if toolID == "" || "codex-exec-"+toolID != taskID {
		return providerSpawnedWorkUpdate{}, false
	}
	status := asString(event["status"])
	switch status {
	case "running", "completed", "failed", "stopped":
	default:
		return providerSpawnedWorkUpdate{}, false
	}
	return providerSpawnedWorkUpdate{Kind: kind, Task: providerSpawnedWorkTask{
		TaskID: taskID, ToolCallID: toolID, TaskType: "local_bash", Status: status, Backgrounded: true,
		Description: compactText(redactSensitiveText(asString(event["description"])), 240),
		Summary:     compactText(redactSensitiveText(asString(event["summary"])), 1000),
	}}, true
}

// In a chat, a command that outlived its turn would be a new rail row, and one
// nothing classifies as a service: a dev server would pin the engine awake for good.
// There it only holds the engine, for a bounded time (engineHoldsLiveWork).
func (codexProviderSpawnedWorkStrategy) ChildSessionOnly(task providerSpawnedWorkTask) bool {
	return codexBackgroundExecID.MatchString(task.TaskID)
}

func (codexProviderSpawnedWorkStrategy) ValidateOutputPath(string, string) (string, bool) {
	return "", false
}
