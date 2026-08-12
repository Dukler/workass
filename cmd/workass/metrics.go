package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"workass/internal/acp"
	"workass/internal/chat"
	"workass/internal/wire"
)

// Daemon-owned counters for GET /workass/metrics. The renderer boot that pulled
// every chat's archive through this process took it from 38MB to 614MB resident
// and nothing reported that, so "the app is slow" had no measurable backend
// side. Everything here is a stat or a walk over already-resident state: the
// endpoint must never become the reason the daemon is busy.
func daemonMetrics(providerChats *providerChatRuntime, stateDir string, hub *wire.Hub, manager *acp.Manager) map[string]any {
	out := map[string]any{}

	if hub != nil {
		out["wire"] = hub.Stats()
	}
	if manager != nil {
		out["acp"] = manager.Stats()
	}

	if providerChats != nil {
		session := actorSessionInventory(providerChats)
		session["snapshotBytes"] = fileBytes(filepath.Join(stateDir, "session-state.json"))
		out["session"] = session
	}

	// Archive volume is the single best predictor of a memory storm: answering
	// chat:archive-load materializes the file into Go maps and marshals it
	// again for the wire, several times its size on disk.
	archiveDir := filepath.Join(stateDir, "chat-archive")
	if entries, err := os.ReadDir(archiveDir); err == nil {
		type archiveInfo struct {
			name  string
			bytes int64
		}
		infos := make([]archiveInfo, 0, len(entries))
		var total int64
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			total += info.Size()
			infos = append(infos, archiveInfo{name: entry.Name(), bytes: info.Size()})
		}
		sort.Slice(infos, func(a, b int) bool { return infos[a].bytes > infos[b].bytes })
		largest := make([]map[string]any, 0, 3)
		for _, info := range infos {
			if len(largest) == 3 {
				break
			}
			largest = append(largest, map[string]any{"file": info.name, "bytes": info.bytes})
		}
		out["archives"] = map[string]any{
			"files":      len(infos),
			"totalBytes": total,
			"largest":    largest,
		}
	}

	return out
}

// actorSessionInventory reports the meaningful session counters from the
// authoritative chat actors. The old session mirror is deliberately absent:
// after cutover it is a rebuildable presentation cache/global-settings file,
// not semantic chat state. Walk the typed actor state directly rather than
// projecting a second complete renderer snapshot merely to count it.
func actorSessionInventory(runtime *providerChatRuntime) map[string]any {
	out := map[string]any{
		"chats": 0, "messages": 0, "events": 0, "messageImages": 0,
		"messageBytes": 0, "inlineImageBytes": 0, "heaviestChat": map[string]any{},
	}
	if runtime == nil {
		return out
	}
	chatIDs, err := runtime.knownChatIDs()
	if err != nil {
		// Metrics must remain non-blocking and must never recover by consulting
		// the retired session mirror. A runtime boot/reconciliation error is
		// surfaced through the normal daemon startup path; the endpoint returns
		// a safe zero snapshot while that path is unavailable.
		return out
	}

	chatCount, messageCount, eventCount, imageCount, messageBytes := 0, 0, 0, 0, 0
	heaviestEvents := -1
	heaviest := map[string]any{}
	for _, chatID := range chatIDs {
		actor, err := runtime.actor(chatID)
		if err != nil {
			continue
		}
		state := actor.engine.Snapshot()
		if state.Deleted || !state.Initialized {
			continue
		}

		chatMessages, chatEvents, chatImages, chatTextBytes := actorStateInventory(state)
		chatCount++
		messageCount += chatMessages
		eventCount += chatEvents
		imageCount += chatImages
		messageBytes += chatTextBytes
		if chatEvents > heaviestEvents {
			heaviestEvents = chatEvents
			heaviest = map[string]any{
				"id": state.Presentation.TabID, "events": chatEvents, "messages": chatMessages,
			}
		}
	}
	out["chats"], out["messages"], out["events"] = chatCount, messageCount, eventCount
	out["messageImages"], out["messageBytes"] = imageCount, messageBytes
	// Actor attachments are immutable content-addressed references. Their
	// payload bytes are not embedded in the actor state, so there are no inline
	// image bytes to attribute to daemon heap usage after the cutover.
	out["inlineImageBytes"] = 0
	out["heaviestChat"] = heaviest
	return out
}

// actorStateInventory mirrors the renderer projection's message identity
// rules without allocating the projection maps. The foreground and pending
// steer rows are included because they are actor-owned visible messages even
// before their terminal ledger receipt is committed.
func actorStateInventory(state chat.State) (messages, events, images, textBytes int) {
	seen := make(map[string]struct{}, len(state.Ledger)+4)
	add := func(id, text string, attachmentCount, eventCount int) {
		id = strings.TrimSpace(id)
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		messages++
		textBytes += len(text)
		images += attachmentCount
		events += eventCount
	}
	for _, row := range state.Ledger {
		add(row.MessageID, row.Text, len(row.Attachments), len(row.Timeline))
	}
	if foreground := state.Foreground; foreground != nil {
		if !foreground.UserConsumed {
			userID := strings.TrimSpace(foreground.Input.Presentation.UserMessageID)
			if userID == "" {
				userID = "message:" + string(foreground.OperationID) + ":user"
			}
			add(userID, foreground.Input.Text, len(foreground.Input.Attachments), 0)
		}
		assistantID := strings.TrimSpace(foreground.CurrentAssistantMessageID)
		add(assistantID, foreground.AssistantContent, len(foreground.AssistantAttachments), len(foreground.Timeline))
		if pending := state.PendingSteer; pending != nil {
			userID := strings.TrimSpace(pending.Presentation.UserMessageID)
			if userID == "" {
				userID = "message:" + string(pending.OperationID) + ":user"
			}
			add(userID, pending.Text, len(pending.Attachments), 0)
			continuationID := strings.TrimSpace(pending.Presentation.AssistantMessageID)
			if continuationID == "" {
				continuationID = strings.TrimSpace(foreground.RootAssistantMessageID) + "~after~" + string(pending.OperationID)
			}
			add(continuationID, "", 0, 0)
		}
	}
	return messages, events, images, textBytes
}

func fileBytes(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
