package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"

	"workass/internal/wire"
)

// A changed session shell is sent as a delta against the shell the client
// already holds (session:get {shell, page, ifNoneMatch, delta}), not whole.
// Opening a chat changes the shell's selection, its unread flag and maybe
// the page itself, so a reload after it got the whole shell again even though
// one field of one row and one root key had changed (2026-10-25: 297 KB for a
// reload that t3code answers with 5 KB).
//
// The daemon keeps, for each of the last few shells it sent, the content hash
// of every root key and of every field of every chat row. A client that names
// one of them gets {etag, delta: {base, set, unset, lists, patch, chats, ids}}:
// the root keys whose value changed (set) or disappeared (unset), the chat
// order as an edit of the kept one (lists: runs [start, count] of the kept
// list and the ids it lacks), the changed fields of rows it already holds
// (patch: [{id, set, unset}]), the rows it does not hold yet (chats), and
// every listed row's id in order (ids, only when that list changed). Rebuilt
// on the client (desktop/renderer2/src/store/session-page.ts) from the shell it kept under
// base, that is exactly the shell named by etag. A client that names a shell
// the daemon no longer remembers, or a delta that would not be smaller, gets
// the whole shell as before; a client that does not ask for a delta never
// gets one.
const shellDeltaCacheEntries = 32

type shellDeltaRow map[string]uint64

type shellDeltaEntry struct {
	root  map[string]uint64
	rows  map[string]shellDeltaRow
	order []string
	// lists keeps the root keys that are lists of ids (chatOrder), whose
	// change is sent as an edit of the kept list (delta.lists).
	lists map[string][]string
}

// shellListKeys are the root lists of ids a delta edits rather than resends:
// the chat order gains or loses a few ids when the page changes, and resent
// whole it was most of every delta that carried it.
var shellListKeys = map[string]bool{"chatOrder": true}

// listEdit is the new list as runs of the kept one ([start, count]) and the
// ids the kept one lacks; the client rebuilds the list from it.
func listEdit(kept, next []string) []any {
	at := make(map[string]int, len(kept))
	for index, id := range kept {
		if _, dup := at[id]; !dup {
			at[id] = index
		}
	}
	ops := make([]any, 0)
	for i := 0; i < len(next); {
		start, ok := at[next[i]]
		if !ok {
			ops = append(ops, next[i])
			i++
			continue
		}
		count := 1
		for i+count < len(next) && start+count < len(kept) && kept[start+count] == next[i+count] {
			count++
		}
		ops = append(ops, []int{start, count})
		i += count
	}
	return ops
}

type shellDeltaCache struct {
	mu      sync.Mutex
	entries map[string]*shellDeltaEntry
	recent  []string
}

var sessionShellDeltas = &shellDeltaCache{entries: map[string]*shellDeltaEntry{}}

func (c *shellDeltaCache) remember(etag string, entry *shellDeltaEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[etag]; ok {
		for i, kept := range c.recent {
			if kept == etag {
				c.recent = append(c.recent[:i], c.recent[i+1:]...)
				break
			}
		}
	}
	c.entries[etag] = entry
	c.recent = append(c.recent, etag)
	for len(c.recent) > shellDeltaCacheEntries {
		delete(c.entries, c.recent[0])
		c.recent = c.recent[1:]
	}
}

func (c *shellDeltaCache) lookup(etag string) *shellDeltaEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[etag]
}

func shellHash(raw []byte) uint64 {
	sum := sha256.Sum256(raw)
	return binary.BigEndian.Uint64(sum[:8])
}

type shellParts struct {
	root   map[string]json.RawMessage
	rows   []map[string]json.RawMessage
	rawRow []json.RawMessage
	ids    []string
	entry  *shellDeltaEntry
}

func splitShell(raw []byte) (shellParts, bool) {
	var parts shellParts
	if err := json.Unmarshal(raw, &parts.root); err != nil || parts.root == nil {
		return parts, false
	}
	if err := json.Unmarshal(parts.root["chats"], &parts.rawRow); err != nil {
		return parts, false
	}
	parts.entry = &shellDeltaEntry{root: map[string]uint64{}, rows: map[string]shellDeltaRow{}, order: make([]string, 0, len(parts.rawRow)), lists: map[string][]string{}}
	for key, value := range parts.root {
		if key == "chats" || key == "etag" {
			continue
		}
		parts.entry.root[key] = shellHash(value)
		if shellListKeys[key] {
			var list []string
			if json.Unmarshal(value, &list) == nil {
				parts.entry.lists[key] = list
			}
		}
	}
	for _, rawRow := range parts.rawRow {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(rawRow, &row); err != nil || row == nil {
			return parts, false
		}
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil || id == "" {
			return parts, false
		}
		if _, dup := parts.entry.rows[id]; dup {
			return parts, false
		}
		fields := make(shellDeltaRow, len(row))
		for key, value := range row {
			fields[key] = shellHash(value)
		}
		parts.ids = append(parts.ids, id)
		parts.rows = append(parts.rows, row)
		parts.entry.rows[id] = fields
		parts.entry.order = append(parts.entry.order, id)
	}
	return parts, true
}

// changedKeys is what differs between a kept hash set and the current values:
// the keys to set (sorted, for a deterministic reply) and the keys to drop.
func changedKeys(base map[string]uint64, current map[string]json.RawMessage, skip func(string) bool) (map[string]json.RawMessage, []string) {
	set := map[string]json.RawMessage{}
	for key, value := range current {
		if skip != nil && skip(key) {
			continue
		}
		if hash, ok := base[key]; !ok || hash != shellHash(value) {
			set[key] = value
		}
	}
	unset := []string{}
	for key := range base {
		if _, ok := current[key]; !ok {
			unset = append(unset, key)
		}
	}
	sort.Strings(unset)
	return set, unset
}

// sessionShellDeltaReply is sessionShellReply for a client that accepts a
// delta.
func sessionShellDeltaReply(raw []byte, ifNoneMatch string) wire.EncodedResult {
	sum := sha256.Sum256(raw)
	etag := hex.EncodeToString(sum[:])
	if ifNoneMatch != "" && ifNoneMatch == etag {
		return etagReply(raw, ifNoneMatch)
	}
	parts, ok := splitShell(raw)
	if !ok {
		return etagReply(raw, ifNoneMatch)
	}
	sessionShellDeltas.remember(etag, parts.entry)
	base := sessionShellDeltas.lookup(ifNoneMatch)
	if ifNoneMatch == "" || base == nil {
		return etagReply(raw, ifNoneMatch)
	}
	delta := map[string]any{"base": ifNoneMatch}
	set, unset := changedKeys(base.root, parts.root, func(key string) bool { return key == "chats" || key == "etag" })
	lists := map[string]any{}
	for key := range shellListKeys {
		kept, wasList := base.lists[key]
		next, isList := parts.entry.lists[key]
		if _, changed := set[key]; !changed || !wasList || !isList {
			continue
		}
		edit := listEdit(kept, next)
		if encoded, err := json.Marshal(edit); err == nil && len(encoded) < len(set[key]) {
			lists[key] = edit
			delete(set, key)
		}
	}
	if len(lists) > 0 {
		delta["lists"] = lists
	}
	if len(set) > 0 {
		delta["set"] = set
	}
	if len(unset) > 0 {
		delta["unset"] = unset
	}
	patches := make([]any, 0)
	added := make([]json.RawMessage, 0)
	for index, id := range parts.ids {
		kept, held := base.rows[id]
		if !held {
			added = append(added, parts.rawRow[index])
			continue
		}
		rowSet, rowUnset := changedKeys(kept, parts.rows[index], nil)
		if len(rowSet) == 0 && len(rowUnset) == 0 {
			continue
		}
		patch := map[string]any{"id": id}
		if len(rowSet) > 0 {
			patch["set"] = rowSet
		}
		if len(rowUnset) > 0 {
			patch["unset"] = rowUnset
		}
		patches = append(patches, patch)
	}
	if len(patches) > 0 {
		delta["patch"] = patches
	}
	if len(added) > 0 {
		delta["chats"] = added
	}
	// ids only when the listed rows or their order changed.
	if !sameShellOrder(base.order, parts.ids) {
		delta["ids"] = parts.ids
	}
	reply, err := json.Marshal(map[string]any{"etag": etag, "delta": delta})
	if err != nil || len(reply) >= len(raw) {
		return etagReply(raw, ifNoneMatch)
	}
	return wire.EncodedResult(bytes.TrimSpace(reply))
}

func sameShellOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
