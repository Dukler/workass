package main

import (
	"regexp"
	"strings"
)

// Ajustes · General keeps four keys in the app-settings blob:
//
//   - "newChat": what a chat the app starts begins with when nothing more
//     specific is chosen. An empty providerId keeps the chat that is open as
//     the model (agent, model, effort and permissions); a folder of "last" is
//     the last project a chat was started in, "active" the open chat's folder,
//     and anything else one fixed folder. The app applies all of it.
//   - "whileWorking": what Enter does while a turn runs, "queue" or "steer".
//   - "notices": which events the app announces, and whether with a sound.
//   - "dictation": the language whisper is told; read here by voice:transcribe.
//
// settings:set replaces the blob, so each key is normalized on every write.

const (
	newChatFolderLast   = "last"
	newChatFolderActive = "active"
	whileWorkingQueue   = "queue"
	whileWorkingSteer   = "steer"
	dictationAuto       = "auto"
	dictationDefault    = "es"
	settingIDMax        = 200
	settingPathMax      = 4096
)

var dictationLangPattern = regexp.MustCompile(`^[a-z]{2,3}$`)

func applyGeneralSettings(next, input map[string]any) {
	next["newChat"] = normalizeNewChatSettings(input["newChat"])
	next["whileWorking"] = normalizeWhileWorking(input["whileWorking"])
	next["notices"] = normalizeNoticeSettings(input["notices"])
	next["dictation"] = normalizeDictationSettings(input["dictation"])
}

func normalizeNewChatSettings(value any) map[string]any {
	raw := mapFromAnyMain(value)
	out := map[string]any{"providerId": "", "model": "", "effort": "", "modeId": "", "folder": normalizeNewChatFolder(raw["folder"])}
	// Model, effort and permissions are ids from one agent's catalog: without
	// an agent they name nothing, and an effort belongs to one model.
	provider := settingID(raw["providerId"])
	if provider == "" {
		return out
	}
	out["providerId"] = provider
	out["modeId"] = settingID(raw["modeId"])
	if model := settingID(raw["model"]); model != "" {
		out["model"] = model
		out["effort"] = settingID(raw["effort"])
	}
	return out
}

func normalizeNewChatFolder(value any) string {
	folder := strings.TrimSpace(toString(value))
	switch {
	case folder == newChatFolderLast, folder == newChatFolderActive:
		return folder
	case len(folder) > settingPathMax:
		return newChatFolderLast
	case strings.HasPrefix(folder, "/"), windowsAbsolutePath(folder):
		return folder
	}
	return newChatFolderLast
}

func windowsAbsolutePath(path string) bool {
	if strings.HasPrefix(path, `\\`) {
		return true
	}
	return len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') &&
		((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z'))
}

func settingID(value any) string {
	id := strings.TrimSpace(toString(value))
	if len(id) > settingIDMax {
		return ""
	}
	return id
}

func normalizeWhileWorking(value any) string {
	if strings.TrimSpace(toString(value)) == whileWorkingSteer {
		return whileWorkingSteer
	}
	return whileWorkingQueue
}

func defaultNoticeSettings() map[string]any {
	return map[string]any{"needsYou": true, "turnEnded": true, "jobs": true, "sound": false}
}

func normalizeNoticeSettings(value any) map[string]any {
	out := defaultNoticeSettings()
	raw := mapFromAnyMain(value)
	for key := range out {
		if v, ok := boolFieldOK(raw, key); ok {
			out[key] = v
		}
	}
	return out
}

func normalizeDictationSettings(value any) map[string]any {
	lang := strings.ToLower(strings.TrimSpace(toString(mapFromAnyMain(value)["lang"])))
	if lang != dictationAuto && !dictationLangPattern.MatchString(lang) {
		lang = dictationDefault
	}
	return map[string]any{"lang": lang}
}

// dictationLanguage is what voice:transcribe tells whisper when the caller
// names no language: whisper-cli itself assumes English when told nothing.
func dictationLanguage(settings map[string]any) string {
	return toString(normalizeDictationSettings(settings["dictation"])["lang"])
}
