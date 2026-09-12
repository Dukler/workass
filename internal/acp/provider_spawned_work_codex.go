package acp

import (
	"regexp"
	"strings"
)

var codexNativeAgentID = regexp.MustCompile(`^codex-agent-[0-9a-f]{24}$`)
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

func (codexProviderSpawnedWorkStrategy) ValidateOutputPath(string, string) (string, bool) {
	return "", false
}
