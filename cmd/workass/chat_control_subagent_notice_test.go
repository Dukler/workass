package main

import (
	"encoding/json"
	"strings"
	"testing"

	"workass/internal/acp"
)

// The items are decoded from the daemon's own wire encoding rather than built
// as literals: the notice builder consumes exactly what the ACP manager
// publishes, so the encoding is the contract under test.
func decodeSpawnedWorkItemsForTest(t *testing.T, encoded string) []acp.SpawnedWorkItem {
	t.Helper()
	var items []acp.SpawnedWorkItem
	if err := json.Unmarshal([]byte(encoded), &items); err != nil {
		t.Fatalf("decode spawned work items: %v", err)
	}
	return items
}

func TestSpawnedWorkNoticePointsSubagentsAtSubagentReceiptsWithModelAndResult(t *testing.T) {
	items := decodeSpawnedWorkItemsForTest(t, `[{
		"taskId": "wa-subagent-1",
		"kind": "subagent",
		"label": "research the wake path",
		"status": "exited",
		"modelLabel": "Opus4.8-xhigh",
		"resultExcerpt": "the dispatcher never checked for a running turn"
	}]`)
	notice := spawnedWorkServerNoticeText(items)

	for _, want := range []string{
		"[workass internal notice]",
		"Background work completed while no turn was active:",
		"research the wake path — exited",
		"model: Opus4.8-xhigh",
		"result: the dispatcher never checked for a running turn",
		// The spawned-work receipt carries a lane's output tail and never the
		// delegated agent's answer; a woken coordinator must be sent to the
		// receipt that actually holds the result.
		"workass_list_subagent_receipts",
		"This notice is not a user language preference",
		"Resume the work that depended on this completion.",
	} {
		if !strings.Contains(notice, want) {
			t.Fatalf("subagent wake notice is missing %q: %q", want, notice)
		}
	}
}

func TestSpawnedWorkNoticeForLanesKeepsSpawnedWorkReceiptPointer(t *testing.T) {
	items := decodeSpawnedWorkItemsForTest(t, `[{
		"taskId": "xw-1",
		"kind": "external",
		"label": "build lane",
		"status": "exited",
		"outputFile": "/tmp/lane.output"
	}]`)
	notice := spawnedWorkServerNoticeText(items)

	if !strings.Contains(notice, "Receipts: workass_list_spawned_work_receipts.") {
		t.Fatalf("lane wake notice lost its receipt pointer: %q", notice)
	}
	if strings.Contains(notice, "workass_list_subagent_receipts") {
		t.Fatalf("lane-only wake notice pointed at subagent receipts: %q", notice)
	}
	if !strings.Contains(notice, "output: /tmp/lane.output") {
		t.Fatalf("lane wake notice lost its output pointer: %q", notice)
	}
}

func TestSpawnedWorkNoticeFlattensMultiLineSubagentResult(t *testing.T) {
	items := decodeSpawnedWorkItemsForTest(t, `[{
		"taskId": "wa-subagent-2",
		"kind": "subagent",
		"label": "multi line",
		"status": "failed",
		"resultExcerpt": "first line\n\n- second line\n- third line"
	}]`)
	notice := spawnedWorkServerNoticeText(items)

	// One item is one bullet plus at most one result line: a multi-line child
	// result must not be able to forge extra bullets in the notice.
	if got := strings.Count(notice, "\n- "); got != 1 {
		t.Fatalf("subagent result forged %d bullets in the notice: %q", got, notice)
	}
	if !strings.Contains(notice, "result: first line - second line - third line") {
		t.Fatalf("subagent result was not flattened: %q", notice)
	}
}
