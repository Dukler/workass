package main

import (
	"strings"
	"time"
)

// spawned-work:list {brief: true, keep?: [workId]} is the list a chat's first
// paint needs. A long chat's list held every agent it ever ran, finished
// weeks ago: 341-905 rows and 0.5-1.2 MB (most of it their result
// summaries), read whenever the chat opened, while the screen draws the
// running work, what finished today and the cards of its loaded turns. The
// brief reply is
//
//	{brief: true, items, settled: [[workId, status], ...], omitted, obligation?}
//
// items: every row, whole, that runs or waits, that finished within
// briefSpawnedWorkRecent, or whose work id is in keep (the finished agents
// the client's rail is drawing). settled: for each other agent row, its work
// id (what a subagent card is matched by: the row id of a "subagent" row,
// else its tool call id, else its row id) and status, so a card in loaded
// history still takes its child's end. omitted: how many rows items leaves
// out. A client reads the whole list (no brief) when it shows older finished
// work (the rail's history, a finished subagent's result). A daemon that
// predates brief ignores it and sends the whole list.

// briefSpawnedWorkRecent covers "finished today" in any time zone.
const briefSpawnedWorkRecent = 48 * time.Hour

func briefSpawnedWorkReply(items []map[string]any, keep map[string]struct{}, now time.Time) map[string]any {
	kept := make([]map[string]any, 0, 8)
	settled := make([]any, 0, len(items))
	omitted := 0
	for _, item := range items {
		workID := spawnedWorkCardID(item)
		_, named := keep[workID]
		if named || fieldString(item, "status") == "running" || finishedWithin(item, now, briefSpawnedWorkRecent) {
			kept = append(kept, item)
			continue
		}
		omitted++
		switch fieldString(item, "kind") {
		case "agent", "subagent":
			settled = append(settled, []any{workID, fieldString(item, "status")})
		}
	}
	return map[string]any{"brief": true, "items": kept, "settled": settled, "omitted": omitted}
}

// spawnedWorkCardID is the renderer's subagentWorkId.
func spawnedWorkCardID(item map[string]any) string {
	if fieldString(item, "kind") == "subagent" {
		return fieldString(item, "id")
	}
	if id := fieldString(item, "toolCallId"); id != "" {
		return id
	}
	return fieldString(item, "id")
}

// finishedWithin reports whether a finished row ended less than window ago;
// a row with no readable end counts as recent (it is kept whole).
func finishedWithin(item map[string]any, now time.Time, window time.Duration) bool {
	finished := strings.TrimSpace(fieldString(item, "finishedAt"))
	if finished == "" {
		finished = strings.TrimSpace(fieldString(item, "updatedAt"))
	}
	at, err := time.Parse(time.RFC3339Nano, finished)
	if err != nil {
		return true
	}
	return now.Sub(at) < window
}

func stringSet(raw any) map[string]struct{} {
	out := map[string]struct{}{}
	for _, value := range anySlice(raw) {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			out[strings.TrimSpace(text)] = struct{}{}
		}
	}
	return out
}
