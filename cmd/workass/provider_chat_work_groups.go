package main

import (
	"errors"
	"strings"

	"workass/internal/chat"
)

// Work groups (internal/chat/workgroup.go): a history read with the additive
// {workGroups: true} option sends a settled row's folded calls as a summary.
// The row's message then carries
//
//	workSummary: {tools, subagentCalls, failed, startedAt?, endedAt?, kinds?, events}
//
// and its events keep, in order, every entry that is not a folded call, one
// anchor per text offset of folded main-thread calls
//
//	{kind: "tool", key, at, id: null, title: "", status: "", workStub: true}
//
// (so the prose is cut exactly where it was) and stubs of the folded calls
// with images, which the transcript draws as a gallery
//
//	{kind: "tool", key, at, images, workStub: true}
//
// (one stub for consecutive such calls at one offset; a call with an unnamed
// image has its own, with its title).
//
// The calls themselves are read when the Worked row opens, a page at a time:
// archive-load(tab, {tail: 1, workGroup: {messageId, offset}, ...read
// options}) answers {messageId, offset, next, total, events}, the row's
// whole timeline entries [offset, next) under the read's options, with next
// == total on the last page. tail: 1 makes a daemon that predates it answer
// one bounded row instead of the complete archive, which a reader tells
// apart by its shape (an array). A read without workGroups is unchanged byte
// for byte.

// workGroupPageBytes bounds one {workGroup} page's encoded events. A row's
// events are read in one page up to this size (a 8,542-call row is about
// 2.5 MB under the first screen's options, so three pages).
var workGroupPageBytes = 1 << 20

// workGroupHistoryPageBytes bounds a work group history page. With its calls
// folded a row is a few kilobytes, so the 8 MiB page of whole rows would
// carry dozens of rows, each read and decoded from disk (megabytes apiece
// on a long chat) for one scroll: a page is cut at 1 MiB of what it sends,
// and the client reads the next as it scrolls, as after any bounded page.
const workGroupHistoryPageBytes = 1 << 20

// historyPageBytes is the reply budget of a history page under opts.
func historyPageBytes(opts []archiveReadOptions) int64 {
	if workGroupsRead(opts) {
		return workGroupHistoryPageBytes
	}
	return actorHistoryPageBytes
}

func workGroupsRead(opts []archiveReadOptions) bool {
	for _, opt := range opts {
		if opt.workGroups {
			return true
		}
	}
	return false
}

// decodeLedgerRowFor is the closed-row decoder of a read: a work group read
// does not decode the calls it leaves out.
func decodeLedgerRowFor(opts []archiveReadOptions) func([]byte) (chat.LedgerEvent, error) {
	if workGroupsRead(opts) {
		return chat.DecodeLedgerEventForWorkGroup
	}
	return nil
}

// projectLedgerMessageFor is projectLedgerMessage under the read's options.
func projectLedgerMessageFor(event chat.LedgerEvent, opts []archiveReadOptions) (map[string]any, error) {
	if workGroupsRead(opts) {
		if row, ok := chat.ReduceLedgerEventForWorkGroup(event); ok {
			return projectWorkGroupRow(row)
		}
	}
	return projectLedgerMessage(event)
}

// projectWorkGroupRow is a reduced row as its message.
func projectWorkGroupRow(row chat.WorkGroupRow) (map[string]any, error) {
	message, err := projectLedgerMessage(row.Event)
	if err != nil {
		return nil, err
	}
	events, err := projectWorkGroupItems(row.Items)
	if err != nil {
		return nil, err
	}
	message["events"] = events
	message["workSummary"] = projectWorkSummary(row.Work)
	return message, nil
}

func projectWorkGroupItems(items []chat.WorkGroupItem) ([]any, error) {
	out := make([]any, 0, len(items))
	for _, item := range items {
		switch {
		case item.Entry != nil:
			projected, err := projectTimeline([]chat.TimelineEntry{*item.Entry})
			if err != nil {
				return nil, err
			}
			out = append(out, projected...)
		case item.Anchor != nil:
			out = append(out, map[string]any{
				"kind": "tool", "key": item.Anchor.Key, "at": item.Anchor.At,
				"id": nil, "title": "", "status": "", "workStub": true,
			})
		case item.Media != nil:
			images, err := projectionAttachments(item.Media.Attachments)
			if err != nil {
				return nil, err
			}
			stub := map[string]any{"kind": "tool", "key": item.Media.Key, "at": item.Media.At, "images": images, "workStub": true}
			if item.Media.Title != "" {
				stub["title"] = item.Media.Title
			}
			out = append(out, stub)
		default:
			return nil, errors.New("work group item is empty")
		}
	}
	return out, nil
}

func projectWorkSummary(work chat.WorkSummary) map[string]any {
	out := map[string]any{
		"tools": work.Calls, "subagentCalls": work.SubagentCalls, "failed": work.Failed, "events": work.Entries,
	}
	if work.StartedAt > 0 {
		out["startedAt"] = work.StartedAt
	}
	if work.EndedAt > 0 {
		out["endedAt"] = work.EndedAt
	}
	if len(work.Kinds) > 0 {
		kinds := make(map[string]any, len(work.Kinds))
		for kind, count := range work.Kinds {
			kinds[kind] = count
		}
		out["kinds"] = kinds
	}
	return out
}

// applyWorkGroups replaces, in projected messages, the events of every
// settled row that folds work with its work group (the resident and the
// whole-window paths, which project from decoded ledger rows).
func applyWorkGroups(messages []any, ledger []chat.LedgerEvent) error {
	if len(messages) == 0 || len(ledger) == 0 {
		return nil
	}
	byID := make(map[string]int, len(ledger))
	for index := range ledger {
		byID[ledger[index].MessageID] = index
	}
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		index, found := byID[fieldString(message, "id")]
		if !found {
			continue
		}
		switch fieldString(message, "status") {
		case "running", "pending":
			continue
		}
		row, ok := chat.ReduceLedgerEventForWorkGroup(ledger[index])
		if !ok {
			continue
		}
		events, err := projectWorkGroupItems(row.Items)
		if err != nil {
			return err
		}
		message["events"] = events
		message["workSummary"] = projectWorkSummary(row.Work)
	}
	return nil
}

type workGroupRequest struct {
	messageID string
	offset    int
	// limit (additive): at least this many entries, in whole chunks.
	limit int
	// compact (additive): calls as compactWorkGroupEvent sends them.
	compact bool
	// ifNoneMatch (additive): a page's etag the reader holds; the reply is
	// then {etag, unchanged: true} while the page is still those bytes.
	ifNoneMatch string
}

func parseWorkGroupRequest(raw any) (workGroupRequest, error) {
	options := mapFromAnyMain(raw)
	request := workGroupRequest{
		messageID:   strings.TrimSpace(fieldString(options, "messageId")),
		offset:      intValue(options["offset"]),
		limit:       intValue(options["limit"]),
		compact:     options["compact"] == true,
		ifNoneMatch: fieldString(options, "ifNoneMatch"),
	}
	if request.messageID == "" {
		return workGroupRequest{}, errors.New("work group requires a message id")
	}
	if request.offset < 0 || request.limit < 0 {
		return workGroupRequest{}, errors.New("work group offset must not be negative")
	}
	return request, nil
}

// ProjectWorkGroupByTab is one page of a row's whole timeline (see the top of
// this file): its entries from request.offset on, each projected and
// finished under opts as a history page sends it, read from the row's pack
// (provider_chat_work_group_pack.go). A page holds at least request.limit
// entries when that is set, else up to workGroupPageBytes, of what the pack
// has built so far.
func (r *providerChatRuntime) ProjectWorkGroupByTab(tabID string, request workGroupRequest, opts ...archiveReadOptions) (any, bool, error) {
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return nil, found, err
	}
	store := r.workGroupPacks()
	for attempt := 0; ; attempt++ {
		pack, found, err := r.workGroupPack(chatID, request.messageID, request.compact, opts)
		if err != nil || !found {
			return nil, found, err
		}
		page, err := store.page(pack, request.offset, request.limit, workGroupPageBytes, request.ifNoneMatch)
		if err == nil {
			return page, true, nil
		}
		// A pack whose file went away (or whose build failed) is built again.
		store.drop(pack)
		if attempt > 0 {
			return nil, false, err
		}
	}
}

// rowTimeline is a committed row's timeline; a closed chat's is decoded from
// its stored JSON one entry at a time.
type rowTimeline struct {
	stored  chat.StoredTimeline
	entries []chat.TimelineEntry
	closed  bool
}

func (t rowTimeline) length() int {
	if t.closed {
		return t.stored.Len()
	}
	return len(t.entries)
}

func (t rowTimeline) entry(index int) (chat.TimelineEntry, error) {
	if t.closed {
		return t.stored.Entry(index)
	}
	return t.entries[index], nil
}
