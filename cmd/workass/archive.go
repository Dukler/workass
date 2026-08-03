package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"workass/internal/wire"
)

var (
	archiveIndexesMu sync.Mutex
	archiveIndexes   = map[string]*archiveSeenIndex{}
)

type archiveSeenIndex struct {
	mu          sync.Mutex
	initialized bool
	info        os.FileInfo
	seenIDs     map[string]struct{}
	seenRecords map[string]struct{}
	rebuilds    int
}

func archiveIndexForPath(path string) *archiveSeenIndex {
	archiveIndexesMu.Lock()
	defer archiveIndexesMu.Unlock()
	if index := archiveIndexes[path]; index != nil {
		return index
	}
	index := &archiveSeenIndex{}
	archiveIndexes[path] = index
	return index
}

func (index *archiveSeenIndex) matches(info os.FileInfo, exists bool) bool {
	if !index.initialized {
		return false
	}
	if !exists {
		return index.info == nil
	}
	if index.info == nil {
		return false
	}
	return os.SameFile(index.info, info) &&
		index.info.Size() == info.Size() &&
		index.info.ModTime().UnixNano() == info.ModTime().UnixNano()
}

func (index *archiveSeenIndex) rebuild(stateDir, tabID string, info os.FileInfo, exists bool) {
	index.seenIDs = map[string]struct{}{}
	index.seenRecords = map[string]struct{}{}
	if exists {
		for _, record := range loadChatArchiveRecords(stateDir, tabID) {
			if id := strings.TrimSpace(toString(record["id"])); id != "" {
				index.seenIDs[id] = struct{}{}
			}
			index.seenRecords[archiveRecordKey(record)] = struct{}{}
		}
	}
	index.initialized = true
	index.info = info
	index.rebuilds++
}

func archiveFileInfo(path string) (os.FileInfo, bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return info, true, nil
}

func registerArchiveHandlers(hub *wire.Hub, state *daemonState) {
	hub.Register("chat:archive-append", func(args []any) (any, error) {
		arg := firstMapArg(args)
		tabID := fieldString(arg, "tabId")
		messages := sliceArg(arg["messages"])
		if tabID == "" || len(messages) == 0 {
			return false, nil
		}
		if err := appendChatArchive(state.stateDir, tabID, messages); err != nil {
			return false, nil
		}
		return true, nil
	})
	hub.Register("chat:archive-load", func(args []any) (any, error) {
		return loadChatArchive(state.stateDir, stringArg(args, 0)), nil
	})
}

func appendChatArchive(stateDir, tabID string, messages []any) error {
	path := chatArchivePath(stateDir, tabID)
	if path == "" {
		return os.ErrInvalid
	}
	index := archiveIndexForPath(path)
	index.mu.Lock()
	defer index.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	info, exists, err := archiveFileInfo(path)
	if err != nil {
		return err
	}
	if !index.matches(info, exists) {
		index.rebuild(stateDir, tabID, info, exists)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if info, statErr := f.Stat(); statErr == nil {
			index.info = info
			index.initialized = true
		} else {
			index.initialized = false
		}
		_ = f.Close()
	}()
	enc := json.NewEncoder(f)
	for _, raw := range messages {
		msg := archiveMessage(raw)
		emptyCancelledAssistant := toString(msg["role"]) == "assistant" && toString(msg["status"]) == "cancelled"
		if strings.TrimSpace(toString(msg["content"])) == "" && strings.TrimSpace(toString(msg["result"])) == "" && !emptyCancelledAssistant {
			continue
		}
		if id := strings.TrimSpace(toString(msg["id"])); id != "" {
			if _, duplicate := index.seenIDs[id]; duplicate {
				continue
			}
			if err := enc.Encode(msg); err != nil {
				return err
			}
			index.seenIDs[id] = struct{}{}
			index.seenRecords[archiveRecordKey(msg)] = struct{}{}
			continue
		}
		recordKey := archiveRecordKey(msg)
		if _, duplicate := index.seenRecords[recordKey]; duplicate {
			continue
		}
		if err := enc.Encode(msg); err != nil {
			return err
		}
		index.seenRecords[recordKey] = struct{}{}
	}
	return nil
}

func loadChatArchive(stateDir, tabID string) []any {
	records := loadChatArchiveRecords(stateDir, tabID)
	out := make([]any, 0, len(records))
	for _, item := range records {
		out = append(out, item)
	}
	return out
}

func loadChatArchiveRecords(stateDir, tabID string) []map[string]any {
	path := chatArchivePath(stateDir, tabID)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]any
	visitArchiveJSONLRecords(f, func(line []byte) {
		var item map[string]any
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&item); err == nil && item != nil {
			out = append(out, item)
		}
	})
	if len(out) == 0 {
		return out
	}
	// Archive consumers receive the same physical chronology as the canonical
	// mirror. This repairs offset-anchored records from the abandoned UI scheme
	// before history hydration, provider replay, forking, or deduplication.
	messages := make([]any, 0, len(out))
	for _, record := range out {
		messages = append(messages, record)
	}
	snapshot := map[string]any{"chats": []any{map[string]any{"id": tabID, "messages": messages}}}
	if !migrateLegacySteerChronology(snapshot) {
		return out
	}
	migrated := messageSlice(mapFromAnyMain(anySlice(snapshot["chats"])[0]))
	out = make([]map[string]any, 0, len(migrated))
	for _, raw := range migrated {
		out = append(out, mapFromAnyMain(raw))
	}
	return out
}

// visitArchiveJSONLRecords has no Scanner token ceiling because canonical
// records may contain the bounded inline raster media supported by Workass.
// The archive is local daemon-owned state; callers still validate every JSON
// object before using it.
func visitArchiveJSONLRecords(r *os.File, visit func([]byte)) {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			visit(line)
		}
		if err != nil {
			return
		}
	}
}

func copyChatArchivePrefix(stateDir, sourceTabID, targetTabID string, atTurn int, hasAtTurn bool) (int, error) {
	sourceTabID = strings.TrimSpace(sourceTabID)
	targetTabID = strings.TrimSpace(targetTabID)
	if sourceTabID == "" || targetTabID == "" || sourceTabID == targetTabID {
		return 0, os.ErrInvalid
	}
	records := loadChatArchiveRecords(stateDir, sourceTabID)
	prefix, effectiveTurn := archivePrefixThroughTurn(records, atTurn, hasAtTurn)
	path := chatArchivePath(stateDir, targetTabID)
	if path == "" {
		return 0, os.ErrInvalid
	}
	targetIndex := archiveIndexForPath(path)
	targetIndex.mu.Lock()
	defer targetIndex.mu.Unlock()
	targetIndex.initialized = false
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, record := range prefix {
		if err := enc.Encode(record); err != nil {
			return 0, err
		}
	}
	return effectiveTurn, nil
}

func archivePrefixThroughTurn(records []map[string]any, atTurn int, hasAtTurn bool) ([]map[string]any, int) {
	totalTurns := countArchiveTurns(records)
	if !hasAtTurn {
		return records, totalTurns
	}
	if atTurn <= 0 {
		return nil, 0
	}
	if atTurn > totalTurns {
		atTurn = totalTurns
	}
	if atTurn == 0 {
		return nil, 0
	}
	out := make([]map[string]any, 0, len(records))
	seenTurns := 0
	for _, record := range records {
		if isArchiveTurnUser(record) {
			seenTurns++
			if seenTurns > atTurn {
				break
			}
		}
		out = append(out, record)
	}
	return out, atTurn
}

func countArchiveTurns(records []map[string]any) int {
	turns := 0
	for _, record := range records {
		if isArchiveTurnUser(record) {
			turns++
		}
	}
	return turns
}

func isArchiveTurnUser(record map[string]any) bool {
	if strings.TrimSpace(toString(record["role"])) != "user" {
		return false
	}
	// Steers are messages inside an existing provider turn, not new turn
	// owners. Accept both the canonical state marker and the legacy anchor so
	// pre-migration archives retain the same turn-count semantics.
	return strings.TrimSpace(toString(record["steerState"])) == "" &&
		strings.TrimSpace(toString(mapFromAnyMain(record["steerAnchor"])["assistantMessageId"])) == ""
}

func archiveMessage(raw any) map[string]any {
	m := mapFromAnyMain(redactSessionValue(raw))
	role := strings.TrimSpace(toString(m["role"]))
	if role != "user" && role != "assistant" {
		role = "assistant"
	}
	out := map[string]any{
		"role":    role,
		"content": toString(m["content"]),
		"status":  firstNonEmptyString(toString(m["status"]), "done"),
		"at":      m["at"],
	}
	if result := toString(m["result"]); result != "" {
		out["result"] = result
	}
	if id := strings.TrimSpace(toString(m["id"])); id != "" {
		out["id"] = id
	}
	if jobID := strings.TrimSpace(toString(m["jobId"])); jobID != "" {
		out["jobId"] = jobID
	}
	if events, ok := m["events"]; ok {
		out["events"] = events
	}
	if images := anySlice(m["images"]); len(images) > 0 {
		out["images"] = images
	}
	if steerState := strings.TrimSpace(toString(m["steerState"])); steerState != "" {
		out["steerState"] = steerState
	}
	if anchor := mapFromAnyMain(m["steerAnchor"]); strings.TrimSpace(toString(anchor["assistantMessageId"])) != "" {
		out["steerAnchor"] = anchor
	}
	if rootID := strings.TrimSpace(toString(m["turnRootId"])); rootID != "" {
		out["turnRootId"] = rootID
	}
	if terminal, ok := m["turnTerminal"].(bool); ok {
		out["turnTerminal"] = terminal
	}
	return out
}

func archiveRecordKey(record map[string]any) string {
	data, _ := json.Marshal([]any{record["role"], record["content"], record["result"], record["status"], record["at"]})
	return string(data)
}

func chatArchivePath(stateDir, tabID string) string {
	stateDir = strings.TrimSpace(stateDir)
	tabID = strings.TrimSpace(tabID)
	if stateDir == "" || tabID == "" {
		return ""
	}
	return filepath.Join(stateDir, "chat-archive", safeArchiveNameMain(tabID)+".jsonl")
}

func safeArchiveNameMain(tabID string) string {
	var b strings.Builder
	for _, r := range tabID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
