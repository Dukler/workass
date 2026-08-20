package acp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var claudeBackgroundResultPattern = regexp.MustCompile(`(?is)running in background with ID:\s*([A-Za-z0-9._-]+).*?output is being written to:\s*([^\r\n]+?\.output)\b`)

type claudeProviderSpawnedWorkStrategy struct{}

func (claudeProviderSpawnedWorkStrategy) Supported() bool { return true }

func (strategy claudeProviderSpawnedWorkStrategy) DecodeTool(raw providerRawToolObservation) (providerSpawnToolSignal, bool) {
	providerMeta := mapFromAny(raw.Meta["claudeCode"])
	signal := providerSpawnToolSignal{
		ProviderTool:     firstNonEmpty(asString(providerMeta["toolName"]), raw.Title),
		RunsInBackground: boolFromMap(raw.RawInput, "run_in_background"),
	}
	match := claudeBackgroundResultPattern.FindStringSubmatch(raw.Output)
	if len(match) == 3 {
		signal.FallbackTaskID = normalizeSpawnedWorkTaskID(match[1])
		if safe, ok := strategy.ValidateOutputPath(signal.FallbackTaskID, strings.TrimSpace(match[2])); ok {
			signal.FallbackOutputFile = safe
		}
	}
	return signal, signal.RunsInBackground || signal.FallbackTaskID != ""
}

func (strategy claudeProviderSpawnedWorkStrategy) DecodeLifecycle(raw any) (providerSpawnedWorkUpdate, bool) {
	event, ok := raw.(map[string]any)
	if !ok || event == nil {
		return providerSpawnedWorkUpdate{}, false
	}
	kind := strings.TrimSpace(asString(event["type"]))
	update := providerSpawnedWorkUpdate{Kind: kind}
	if kind == "snapshot" {
		values, ok := event["tasks"].([]any)
		if !ok {
			return update, true
		}
		update.TasksKnown = true
		update.Tasks = make([]providerSpawnedWorkTask, 0, len(values))
		for _, value := range values {
			if task, valid := strategy.decodeTask(mapFromAny(value)); valid {
				update.Tasks = append(update.Tasks, task)
			}
		}
		return update, true
	}
	task, _ := strategy.decodeTask(event)
	patch := mapFromAny(event["patch"])
	task.Description = firstNonEmpty(task.Description, asString(patch["description"]))
	task.Summary = firstNonEmpty(asString(event["summary"]), asString(patch["error"]))
	task.Status = firstNonEmpty(asString(event["status"]), asString(patch["status"]))
	update.Task = task
	return update, true
}

func (strategy claudeProviderSpawnedWorkStrategy) decodeTask(raw map[string]any) (providerSpawnedWorkTask, bool) {
	task := providerSpawnedWorkTask{
		TaskID:       normalizeSpawnedWorkTaskID(asString(raw["taskId"])),
		ToolCallID:   strings.TrimSpace(asString(raw["toolCallId"])),
		Description:  asString(raw["description"]),
		TaskType:     asString(raw["taskType"]),
		SubagentType: asString(raw["subagentType"]),
		Summary:      asString(raw["summary"]),
		LastToolName: asString(raw["lastToolName"]),
		Status:       asString(raw["status"]),
	}
	if path := asString(raw["outputFile"]); path != "" {
		if safe, ok := strategy.ValidateOutputPath(task.TaskID, path); ok {
			task.OutputFile = safe
		}
	}
	return task, task.TaskID != ""
}

func (claudeProviderSpawnedWorkStrategy) ValidateOutputPath(taskID, raw string) (string, bool) {
	taskID = normalizeSpawnedWorkTaskID(taskID)
	if taskID == "" || strings.ContainsAny(taskID, `/\\`) {
		return "", false
	}
	path := filepath.Clean(strings.TrimSpace(raw))
	if !filepath.IsAbs(path) || filepath.Base(path) != taskID+".output" || filepath.Base(filepath.Dir(path)) != "tasks" {
		return "", false
	}
	allowedRoots := []string{os.TempDir()}
	if filepath.Separator == '/' {
		allowedRoots = append(allowedRoots, "/private/tmp", "/tmp")
	}
	allowed := false
	for _, root := range allowedRoots {
		rel, err := filepath.Rel(filepath.Clean(root), path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", false
	}
	hasProviderDir := false
	for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if strings.HasPrefix(strings.ToLower(filepath.Base(dir)), "claude-") {
			hasProviderDir = true
			break
		}
	}
	if !hasProviderDir {
		return "", false
	}
	return path, true
}
