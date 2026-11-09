package main

// chatLifecycle is the app-settings key for when the sidebar files quiet chats
// away by itself (renderer chat-lifecycle.ts, SidebarV2 resolveSettled and
// resolveArchived, and the paged shell in session_page.go). Both values are
// whole days, and 0 means never:
//
//   - settleAfterDays: a chat with no activity for this long shows under «En
//     reposo». Off by default: since 2026-09-21 nothing files by age unless the
//     owner asks for it.
//   - archiveAfterDays: a chat in «En reposo» leaves the list for the
//     searchable archive this long after it got there. Five days is the rule
//     the sidebar has always applied.
//
// Neither writes anything to a chat or deletes anything: both are read from
// when a chat was last active and when it was put to rest, a search still
// finds an archived chat, and Reactivar brings any of them back.
const (
	defaultChatArchiveAfterDays = 5
	maxChatLifecycleDays        = 365
)

func defaultChatLifecycleSettings() map[string]any {
	return map[string]any{"settleAfterDays": 0, "archiveAfterDays": defaultChatArchiveAfterDays}
}

// normalizeChatLifecycle keeps each value that is a whole number of days from
// 0 to a year; anything else falls back to its default.
func normalizeChatLifecycle(value any) map[string]any {
	out := defaultChatLifecycleSettings()
	raw := mapFromAnyMain(value)
	for _, key := range []string{"settleAfterDays", "archiveAfterDays"} {
		days, ok := intFieldOK(raw, key)
		if !ok || days < 0 || days > maxChatLifecycleDays {
			continue
		}
		if number, isFloat := raw[key].(float64); isFloat && number != float64(days) {
			continue
		}
		out[key] = days
	}
	return out
}
