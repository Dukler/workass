package wire

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Boot reads, compact: ?b1=<value> stands for the phone's reconnect reads
// (?boot=<JSON frames>, boot_reads.go) as a template plus what varies.
//
// Why: the URL is not deflated. The JSON frames are ~1.6 KB on every phone
// reconnect, almost all of it the same each time (channels, read options, omit
// lists, the page three times), which put the phone's upload per connect at
// 3.9 KB against 2.9 KB before boot reads (t3-fix phone stream lane,
// 2026-11-12). The compact form is ~150 bytes.
//
//	b1=<first id>.<settledBefore>.<idleBefore>.<read>.<read>…
//
// Numbers are base 36 (idleBefore empty when absent); ids are consecutive from
// the first. Each read is a letter, plus the read's etag in base64url (the 32
// bytes of the SHA-256 hex etag) when it is conditional:
//
//	d  state:digest            {page, omit: digestOmit1, ifNoneMatch?}
//	s  session:get             {sessionRead1..., page, delta: true, ifNoneMatch?}
//	p  chat:permissions-pending
//	w  spawned-work:summary    {page, ifNoneMatch?}
//	m  machines:list
//	g  settings:get
//	a  app:meta
//
// Template 1 is frozen: it is the phone's read set of 2026-11-12
// (workass-mobile src/wire/bootCompact.ts, which mirrors this expansion and
// only sends b1 when its own expansion equals the frames it would send). A
// different read set is a new template (b2), never an edit of this one. Both
// sides check the same fixture (testdata/boot-compact.json).
//
// Additive: older daemons ignore b1 (no `boot` count in the approval) and the
// phone sends the frames itself, then names the JSON form on the next connect.
// The expansion goes through bootReadsFromQuery, so the same rules hold: token
// connections only, read-only channels, at most 8, unique ids. Anything
// malformed expands to nothing.

var (
	digestOmit1 = map[string]any{
		"root": []string{"catalogHash", "procHash"},
		"chat": []string{"actorRevision", "currentModeId", "currentModelId", "messageCount", "queueHeadId", "queueLen"},
	}
	sessionOmit1 = map[string]any{
		"root": []string{"deferredChats", "removedWorkspaces"},
		"chat": []string{"contextUsageByProvider", "planLatest", "historyComplete", "sessionProviderId", "actorRevision", "sessionId", "titleLocked", "draft", "pane", "pending", "messageCount"},
	}
	bootCompactCodes = map[byte]string{
		'd': "state:digest",
		's': "session:get",
		'p': "chat:permissions-pending",
		'w': "spawned-work:summary",
		'm': "machines:list",
		'g': "settings:get",
		'a': "app:meta",
	}
	bootCompactInt  = regexp.MustCompile(`^-?[0-9a-z]{1,12}$`)
	bootCompactETag = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

func bootCompactConditional(letter byte) bool {
	return letter == 'd' || letter == 's' || letter == 'w'
}

func parseBootInt36(text string) (int64, bool) {
	if !bootCompactInt.MatchString(text) {
		return 0, false
	}
	n, err := strconv.ParseInt(text, 36, 64)
	return n, err == nil
}

// expandBootCompact returns the JSON array of invoke frames a b1 value stands
// for, or false when it is malformed.
func expandBootCompact(value string) ([]byte, bool) {
	if value == "" || len(value) > bootReadBytes {
		return nil, false
	}
	parts := strings.Split(value, ".")
	if len(parts) < 4 || len(parts) > 3+bootReadLimit {
		return nil, false
	}
	first, ok := parseBootInt36(parts[0])
	if !ok || first < 0 {
		return nil, false
	}
	settledBefore, ok := parseBootInt36(parts[1])
	if !ok {
		return nil, false
	}
	page := map[string]any{"settledBefore": settledBefore, "include": []string{}}
	if parts[2] != "" {
		idleBefore, ok := parseBootInt36(parts[2])
		if !ok {
			return nil, false
		}
		page["idleBefore"] = idleBefore
	}
	frames := make([]map[string]any, 0, len(parts)-3)
	for i, read := range parts[3:] {
		if read == "" {
			return nil, false
		}
		letter := read[0]
		channel, known := bootCompactCodes[letter]
		if !known {
			return nil, false
		}
		etag := ""
		if tag := read[1:]; tag != "" {
			if !bootCompactConditional(letter) || !bootCompactETag.MatchString(tag) {
				return nil, false
			}
			sum, err := base64.RawURLEncoding.DecodeString(tag)
			if err != nil || len(sum) != 32 {
				return nil, false
			}
			etag = hex.EncodeToString(sum)
		}
		args := []any{}
		switch letter {
		case 'd':
			arg := map[string]any{"page": page, "omit": digestOmit1}
			if etag != "" {
				arg["ifNoneMatch"] = etag
			}
			args = []any{arg}
		case 's':
			arg := map[string]any{
				"shell": true, "leanTools": true, "leanTitles": true, "toolSummaries": true, "deferImages": true,
				"runningTail": "stub", "omit": sessionOmit1, "page": page, "delta": true,
			}
			if etag != "" {
				arg["ifNoneMatch"] = etag
			}
			args = []any{arg}
		case 'w':
			arg := map[string]any{"page": page}
			if etag != "" {
				arg["ifNoneMatch"] = etag
			}
			args = []any{arg}
		}
		frames = append(frames, map[string]any{"t": "invoke", "id": first + int64(i), "channel": channel, "args": args})
	}
	raw, err := json.Marshal(frames)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// bootReadsFromCompact is bootReadsFromQuery for a b1 value.
func bootReadsFromCompact(value string) []invokeMessage {
	raw, ok := expandBootCompact(value)
	if !ok || len(raw) > bootReadBytes {
		return nil
	}
	return bootReadsFromQuery(string(raw))
}
