package main

import (
	"encoding/json"
)

// {omit: {root: [...], chat: [...]}} on session:get {shell} and on
// state:digest is an additive projection: the named root keys, and the named
// fields of every chat row, are left out of the reply. A client asks for it
// only for fields it never reads; the phone's chat list reads about two thirds
// of the shell and half of each digest row, and on a fresh install the rest
// was 19 KB of its 69 KB launch (2026-11-12, t3-fix phone lane).
//
// The projection is applied BEFORE the etag is computed and before the shell
// is remembered for deltas, so a client that always asks the same way gets
// etags, {unchanged} receipts and deltas over exactly the projected form. An
// older client sends no omit and gets byte-identical replies; an older daemon
// ignores the option and sends the whole reply, which such a client reads the
// same way (it never reads the omitted fields).
//
// `chats` and `etag` can never be omitted, nor a row's identity (id, tabId,
// chatId): deltas and every reader key on them.
const readOmitLimit = 64

type readOmit struct {
	root map[string]bool
	chat map[string]bool
}

var readOmitProtected = map[string]bool{"chats": true, "etag": true, "id": true, "tabId": true, "chatId": true}

func readOmitFrom(options map[string]any) *readOmit {
	spec := mapFromAnyMain(options["omit"])
	if spec == nil {
		return nil
	}
	pick := func(value any) map[string]bool {
		list, _ := value.([]any)
		out := map[string]bool{}
		for _, item := range list {
			key, _ := item.(string)
			if key == "" || readOmitProtected[key] || len(out) >= readOmitLimit {
				continue
			}
			out[key] = true
		}
		return out
	}
	omit := &readOmit{root: pick(spec["root"]), chat: pick(spec["chat"])}
	if len(omit.root) == 0 && len(omit.chat) == 0 {
		return nil
	}
	return omit
}

// apply returns raw without the omitted keys. Anything it cannot parse as a
// JSON object with a `chats` list is returned unchanged.
func (o *readOmit) apply(raw []byte) []byte {
	if o == nil {
		return raw
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return raw
	}
	for key := range o.root {
		delete(root, key)
	}
	if len(o.chat) > 0 && len(root["chats"]) > 0 {
		var rows []json.RawMessage
		if err := json.Unmarshal(root["chats"], &rows); err == nil {
			for i, row := range rows {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(row, &fields); err != nil || fields == nil {
					continue
				}
				dropped := false
				for key := range o.chat {
					if _, ok := fields[key]; ok {
						delete(fields, key)
						dropped = true
					}
				}
				if !dropped {
					continue
				}
				if out, err := json.Marshal(fields); err == nil {
					rows[i] = out
				}
			}
			if out, err := json.Marshal(rows); err == nil {
				root["chats"] = out
			}
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
}
