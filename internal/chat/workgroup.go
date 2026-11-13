package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"workass/internal/provider"
)

// Work groups.
//
// A settled turn's calls are drawn folded, as one Worked row ("Worked · 3m ·
// 8,542 tools") that opens to list them, and a subagent's own calls only in
// the rail. A history read that asks for work groups (archive-load
// {workGroups}) sends such a row's folded calls as a summary and reads them
// when the row opens (archive-load {workGroup}). What shows outside the fold
// stays: a subagent's header (its card), a call that published a page, and
// a call's images (its gallery, from a stub holding only what the gallery
// draws). Each run of folded calls at one text offset keeps an anchor, so
// the text around them is laid out exactly as with every call present.
//
// One c94k row held 8,542 calls, 13.9 MB of a history page with every payload
// already cut to its head, and decoding it (json.Unmarshal of the whole row)
// was most of the half second its page took. DecodeLedgerEventForWorkGroup
// decodes such a row without decoding the calls a work group leaves out.

// WorkGroupMinCalls is how many folded calls a row must have before a work
// group leaves them out; a row with fewer sends them, so opening its Worked
// row needs no read.
const WorkGroupMinCalls = 8

// WorkGroupCall is what decides how a work group treats a call: the
// renderer's own rules (desktop/renderer2 subagent-layout.ts isSubagentHeader,
// isSubagentChild, extractSubagents; AssistantMessage ToolImageGallery,
// ToolHtmlPages).
type WorkGroupCall struct {
	ID         string
	ToolKind   string
	SubagentID string
	Header     bool
	// Media: the call has images (attachments), drawn as a gallery.
	Media bool
	// Page: the call published a page (ToolPublishesPage).
	Page bool
}

// WorkGroupFold is what a work group does with one call.
type WorkGroupFold uint8

const (
	// WorkGroupKeep: the call stays whole (a subagent header, a page).
	WorkGroupKeep WorkGroupFold = iota
	// WorkGroupChild: a subagent's own call, drawn only in the rail.
	WorkGroupChild
	// WorkGroupFolded: a main-thread call drawn only in the Worked row.
	WorkGroupFolded
	// WorkGroupMediaStub: a folded main-thread call whose images the
	// transcript draws: it stays as a stub of its key, offset, title and
	// images.
	WorkGroupMediaStub
)

// ClassifyWorkGroupCall decides one call. parents holds every subagent id the
// row's calls name: a call whose id is one is that subagent's header.
func ClassifyWorkGroupCall(call WorkGroupCall, parents map[string]struct{}) WorkGroupFold {
	if call.Header || call.ToolKind == "agent" || call.Page {
		return WorkGroupKeep
	}
	if call.SubagentID != "" {
		if call.SubagentID == call.ID {
			return WorkGroupKeep
		}
		return WorkGroupChild
	}
	if call.ID != "" {
		if _, header := parents[call.ID]; header {
			return WorkGroupKeep
		}
	}
	if call.Media {
		return WorkGroupMediaStub
	}
	return WorkGroupFolded
}

// Markers of a call that published a page: the renderer finds it by the tool
// name in its command or title, or by the reference the call printed.
const (
	workGroupPageToolName     = "workass_render_html"
	workGroupPageOutputMarker = `{"htmlRender":{`
)

// ToolPublishesPage reports whether a call published a page.
func ToolPublishesPage(title, command, input, output string) bool {
	return strings.Contains(title, workGroupPageToolName) || strings.Contains(command, workGroupPageToolName) ||
		strings.Contains(input, workGroupPageToolName) || strings.Contains(output, workGroupPageToolName) ||
		strings.Contains(output, workGroupPageOutputMarker)
}

// WorkGroupCallOf is the classification's view of a decoded call.
func WorkGroupCallOf(tool *provider.ToolEvent) WorkGroupCall {
	return WorkGroupCall{
		ID: tool.ToolCallID, ToolKind: tool.ToolKind, SubagentID: tool.SubagentID, Header: tool.SubagentHeader,
		Media: len(tool.Attachments) > 0, Page: ToolPublishesPage(tool.Title, tool.Command, tool.Input, tool.Output),
	}
}

// WorkGroupRow is a settled assistant row as a work group read sends it.
type WorkGroupRow struct {
	// Event is the row without its timeline (Timeline is nil).
	Event LedgerEvent
	// Items is the timeline in order: the entries that stay, the anchors of
	// folded calls and the stubs of folded calls with images.
	Items []WorkGroupItem
	Work  WorkSummary
}

// WorkGroupItem is one item of a WorkGroupRow's timeline; exactly one field
// is set.
type WorkGroupItem struct {
	Entry  *TimelineEntry `json:",omitempty"`
	Anchor *WorkAnchor    `json:",omitempty"`
	Media  *WorkMedia     `json:",omitempty"`
}

// WorkAnchor stands for the folded main-thread calls at one text offset: the
// first one's key and the offset.
type WorkAnchor struct {
	Key string
	At  int
}

// WorkMedia stands for folded calls with images: what their gallery draws.
// Consecutive such calls at one offset, with nothing the transcript draws
// between them, share one stub (their images in order: the gallery they made
// together). A call with an image that has no name keeps a stub of its own
// with its title, which that image's alternative text is made of.
type WorkMedia struct {
	Key         string
	At          int
	Title       string `json:",omitempty"`
	Attachments []provider.Attachment
}

// mediaNamed reports whether every attachment has a name.
func mediaNamed(attachments []provider.Attachment) bool {
	for _, attachment := range attachments {
		if attachment.Name == "" {
			return false
		}
	}
	return true
}

// WorkSummary is what the folded row shows of the calls left out.
type WorkSummary struct {
	// Calls is the main-thread calls left out (drawn in the Worked row);
	// SubagentCalls the subagents' own calls left out (drawn in the rail).
	Calls         int
	SubagentCalls int
	// Failed counts the main-thread calls left out whose status is failed.
	Failed int
	// StartedAt and EndedAt bound the main-thread calls left out (Unix ms;
	// zero when none has one).
	StartedAt int64
	EndedAt   int64
	// Kinds counts the main-thread calls left out by tool kind.
	Kinds map[string]int
	// Entries is the whole timeline's length.
	Entries int
}

// ReduceLedgerEventForWorkGroup is the row a work group read sends, when the
// row folds work: a settled assistant row with at least WorkGroupMinCalls
// calls left out. A call is read only through the fields
// DecodeLedgerEventForWorkGroup keeps of a call it decodes as a shell.
func ReduceLedgerEventForWorkGroup(event LedgerEvent) (WorkGroupRow, bool) {
	if event.Role != "assistant" || event.Status == "running" || event.Status == "pending" {
		return WorkGroupRow{}, false
	}
	parents := map[string]struct{}{}
	for _, entry := range event.Timeline {
		if entry.Tool != nil && entry.Tool.SubagentID != "" {
			parents[entry.Tool.SubagentID] = struct{}{}
		}
	}
	folds := make([]WorkGroupFold, len(event.Timeline))
	left := 0
	for index, entry := range event.Timeline {
		if entry.Kind != provider.EventToolUpdate || entry.Tool == nil {
			continue
		}
		folds[index] = ClassifyWorkGroupCall(WorkGroupCallOf(entry.Tool), parents)
		if folds[index] != WorkGroupKeep {
			left++
		}
	}
	if left < WorkGroupMinCalls {
		return WorkGroupRow{}, false
	}
	row := WorkGroupRow{Event: event, Items: make([]WorkGroupItem, 0, 16), Work: WorkSummary{Entries: len(event.Timeline)}}
	row.Event.Timeline = nil
	anchored := map[int]struct{}{}
	// The stub the next named media call at its offset joins, until an entry
	// the transcript draws comes between.
	var shared *WorkMedia
	for index := range event.Timeline {
		entry := &event.Timeline[index]
		switch folds[index] {
		case WorkGroupKeep:
			row.Items = append(row.Items, WorkGroupItem{Entry: entry})
			shared = nil
		case WorkGroupChild:
			row.Work.SubagentCalls++
		case WorkGroupFolded, WorkGroupMediaStub:
			tool := entry.Tool
			row.Work.Calls++
			if tool.Status == "failed" {
				row.Work.Failed++
			}
			if tool.StartedAtUnixMS > 0 && (row.Work.StartedAt == 0 || tool.StartedAtUnixMS < row.Work.StartedAt) {
				row.Work.StartedAt = tool.StartedAtUnixMS
			}
			if tool.EndedAtUnixMS > row.Work.EndedAt {
				row.Work.EndedAt = tool.EndedAtUnixMS
			}
			if tool.ToolKind != "" {
				if row.Work.Kinds == nil {
					row.Work.Kinds = map[string]int{}
				}
				row.Work.Kinds[tool.ToolKind]++
			}
			if folds[index] == WorkGroupMediaStub {
				anchored[entry.At] = struct{}{}
				if !mediaNamed(tool.Attachments) {
					row.Items = append(row.Items, WorkGroupItem{Media: &WorkMedia{
						Key: entry.Key, At: entry.At, Title: tool.Title, Attachments: tool.Attachments,
					}})
					shared = nil
					continue
				}
				if shared != nil && shared.At == entry.At {
					shared.Attachments = append(shared.Attachments, tool.Attachments...)
					continue
				}
				shared = &WorkMedia{Key: entry.Key, At: entry.At, Attachments: append([]provider.Attachment(nil), tool.Attachments...)}
				row.Items = append(row.Items, WorkGroupItem{Media: shared})
				continue
			}
			if _, done := anchored[entry.At]; !done {
				anchored[entry.At] = struct{}{}
				row.Items = append(row.Items, WorkGroupItem{Anchor: &WorkAnchor{Key: entry.Key, At: entry.At}})
			}
		}
	}
	return row, true
}

// DecodeLedgerEventForWorkGroup decodes a stored ledger row for a work group
// read: exactly as json.Unmarshal does, except that the calls a work group
// leaves out with nothing of them (WorkGroupChild, WorkGroupFolded) are
// decoded as shells, with only the fields ReduceLedgerEventForWorkGroup reads
// of them (id, tool kind, status, subagent identity, start and end) and never
// their title, command, input or output. A row that does not fold work is
// decoded whole. A shell must never be projected as a call: only
// ReduceLedgerEventForWorkGroup may read the event.
func DecodeLedgerEventForWorkGroup(raw []byte) (LedgerEvent, error) {
	var event LedgerEvent
	shells, err := skimLedgerEvent(raw)
	if err != nil || shells == nil {
		// Not a row whose work folds, or a shape the skim does not follow:
		// the ordinary decode, which also reports any real error.
		if err := json.Unmarshal(raw, &event); err != nil {
			return LedgerEvent{}, err
		}
		return event, nil
	}
	// The row without its timeline, then each entry.
	head := make([]byte, 0, shells.timelineStart+len("null")+len(raw)-shells.timelineEnd)
	head = append(head, raw[:shells.timelineStart]...)
	head = append(head, "null"...)
	head = append(head, raw[shells.timelineEnd:]...)
	if err := json.Unmarshal(head, &event); err != nil {
		return LedgerEvent{}, err
	}
	event.Timeline = make([]TimelineEntry, len(shells.entries))
	decode := func(from, to int) error {
		for index := from; index < to; index++ {
			entry := &shells.entries[index]
			switch {
			case entry.shell != nil:
				event.Timeline[index] = TimelineEntry{Key: entry.key, At: entry.at, Kind: provider.EventToolUpdate, Tool: entry.shell}
			case entry.decoded != nil:
				event.Timeline[index] = *entry.decoded
			default:
				if err := json.Unmarshal(raw[entry.start:entry.end], &event.Timeline[index]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	spans := make([][2]int, len(shells.entries))
	for index, entry := range shells.entries {
		spans[index] = [2]int{entry.start, entry.end}
	}
	if err := runSkimRanges(spans, shells.timelineEnd-shells.timelineStart >= skimParallelBytes, decode); err != nil {
		return LedgerEvent{}, err
	}
	return event, nil
}

// StoredTimeline is a stored row's timeline, decoded one entry at a time.
type StoredTimeline struct {
	raw   []byte
	spans [][2]int
}

// ParseStoredTimeline finds the entries of a stored row's timeline without
// decoding them.
func ParseStoredTimeline(raw []byte) (StoredTimeline, error) {
	spans, err := ledgerTimelineSpans(raw)
	if err != nil {
		return StoredTimeline{}, err
	}
	return StoredTimeline{raw: raw, spans: spans}, nil
}

// Len is the timeline's length.
func (t StoredTimeline) Len() int { return len(t.spans) }

// Entry decodes entry index.
func (t StoredTimeline) Entry(index int) (TimelineEntry, error) {
	var entry TimelineEntry
	err := json.Unmarshal(t.raw[t.spans[index][0]:t.spans[index][1]], &entry)
	return entry, err
}

type skimEntry struct {
	start, end int
	key        string
	at         int
	shell      *provider.ToolEvent
	decoded    *TimelineEntry
}

type skimmedRow struct {
	timelineStart, timelineEnd int
	entries                    []skimEntry
}

var errSkim = errors.New("ledger row shape not followed by the skim")

func shellOf(tool *provider.ToolEvent) *provider.ToolEvent {
	return &provider.ToolEvent{
		ToolCallID: tool.ToolCallID, ToolKind: tool.ToolKind, Status: tool.Status, SubagentID: tool.SubagentID,
		SubagentHeader: tool.SubagentHeader, StartedAtUnixMS: tool.StartedAtUnixMS, EndedAtUnixMS: tool.EndedAtUnixMS,
	}
}

// skimLedgerEvent finds a settled assistant row's timeline entries and, for
// every call, the fields that classify it. It returns nil (no error) when
// the row does not fold work.
func skimLedgerEvent(raw []byte) (*skimmedRow, error) {
	s := skimmer{data: raw, row: true}
	var role, status string
	timelineStart, timelineEnd := -1, -1
	err := s.object(func(key []byte, start, end int) error {
		switch string(key) {
		case "Role":
			role = s.smallString(start, end)
		case "Status":
			status = s.smallString(start, end)
		case "Timeline":
			timelineStart, timelineEnd = start, end
		}
		return nil
	})
	if err == nil {
		err = s.err
	}
	if err != nil {
		return nil, err
	}
	if role != "assistant" || status == "running" || status == "pending" || timelineStart < 0 || raw[timelineStart] != '[' {
		return nil, nil
	}
	row := &skimmedRow{timelineStart: timelineStart, timelineEnd: timelineEnd}
	spans, parallel := timelineElementSpans(raw, timelineStart, timelineEnd)
	if !parallel {
		spans = spans[:0]
		walk := skimmer{data: raw, pos: timelineStart}
		if err := walk.array(func(start, end int) error {
			spans = append(spans, [2]int{start, end})
			return nil
		}); err != nil {
			return nil, err
		}
		if walk.pos != timelineEnd {
			return nil, errSkim
		}
	}
	row.entries = make([]skimEntry, len(spans))
	hasCall := make([]bool, len(spans))
	callOf := make([]WorkGroupCall, len(spans))
	skimRange := func(from, to int) error {
		for index := from; index < to; index++ {
			entry, call, isCall, err := skimElement(raw, spans[index][0], spans[index][1])
			if err != nil {
				return err
			}
			row.entries[index], callOf[index], hasCall[index] = entry, call, isCall
		}
		return nil
	}
	if err := runSkimRanges(spans, parallel, skimRange); err != nil {
		return nil, err
	}
	calls := make([]WorkGroupCall, 0, len(spans))
	callEntry := make([]int, 0, len(spans))
	parents := map[string]struct{}{}
	for index := range spans {
		if !hasCall[index] {
			continue
		}
		if callOf[index].SubagentID != "" {
			parents[callOf[index].SubagentID] = struct{}{}
		}
		calls = append(calls, callOf[index])
		callEntry = append(callEntry, index)
	}
	left := 0
	for index, call := range calls {
		entry := &row.entries[callEntry[index]]
		switch ClassifyWorkGroupCall(call, parents) {
		case WorkGroupChild, WorkGroupFolded:
			left++
			if entry.decoded != nil {
				// A decoded call left out is kept as nothing but its shell,
				// like any other.
				entry.shell = shellOf(entry.decoded.Tool)
				entry.key, entry.at, entry.decoded = entry.decoded.Key, entry.decoded.At, nil
			}
		case WorkGroupMediaStub:
			// Its stub keeps its title and images: decoded whole.
			left++
			entry.shell = nil
		default:
			entry.shell = nil
		}
	}
	if left < WorkGroupMinCalls {
		return nil, nil
	}
	return row, nil
}

// skimElement reads one timeline entry: its key and offset, and for a call
// the fields that classify it, kept as a shell; a call that may have
// published a page is decoded whole.
func skimElement(raw []byte, start, end int) (skimEntry, WorkGroupCall, bool, error) {
	entry := skimEntry{start: start, end: end}
	var kind string
	toolStart := -1
	element := skimmer{data: raw, pos: start}
	if err := element.object(func(key []byte, vs, ve int) error {
		switch string(key) {
		case "Key":
			entry.key = element.smallString(vs, ve)
		case "At":
			at, err := strconv.Atoi(string(raw[vs:ve]))
			if err != nil {
				return errSkim
			}
			entry.at = at
		case "Kind":
			kind = element.smallString(vs, ve)
		case "Tool":
			toolStart = vs
		}
		return nil
	}); err != nil {
		return skimEntry{}, WorkGroupCall{}, false, err
	}
	if element.err != nil || element.pos != end {
		return skimEntry{}, WorkGroupCall{}, false, errSkim
	}
	if kind != string(provider.EventToolUpdate) || toolStart < 0 || raw[toolStart] != '{' {
		return entry, WorkGroupCall{}, false, nil
	}
	body := raw[start:end]
	if bytes.Contains(body, []byte(workGroupPageToolName)) || bytes.Contains(body, []byte("htmlRender")) {
		// Maybe a page: decided on the decoded strings.
		var decoded TimelineEntry
		if err := json.Unmarshal(body, &decoded); err != nil || decoded.Tool == nil {
			return skimEntry{}, WorkGroupCall{}, false, errSkim
		}
		entry.decoded = &decoded
		return entry, WorkGroupCallOf(decoded.Tool), true, nil
	}
	shell := &provider.ToolEvent{}
	var media bool
	tool := skimmer{data: raw, pos: toolStart}
	if err := tool.object(func(key []byte, vs, ve int) error {
		switch string(key) {
		case "ToolCallID":
			shell.ToolCallID = tool.smallString(vs, ve)
		case "ToolKind":
			shell.ToolKind = tool.smallString(vs, ve)
		case "Status":
			shell.Status = tool.smallString(vs, ve)
		case "SubagentID":
			shell.SubagentID = tool.smallString(vs, ve)
		case "SubagentHeader":
			shell.SubagentHeader = string(raw[vs:ve]) == "true"
		case "StartedAtUnixMS":
			value, err := strconv.ParseInt(string(raw[vs:ve]), 10, 64)
			if err != nil {
				return errSkim
			}
			shell.StartedAtUnixMS = value
		case "EndedAtUnixMS":
			value, err := strconv.ParseInt(string(raw[vs:ve]), 10, 64)
			if err != nil {
				return errSkim
			}
			shell.EndedAtUnixMS = value
		case "Attachments":
			value := raw[vs:ve]
			media = !bytes.Equal(value, []byte("null")) && !bytes.Equal(value, []byte("[]"))
		}
		return nil
	}); err != nil {
		return skimEntry{}, WorkGroupCall{}, false, err
	}
	if tool.err != nil {
		return skimEntry{}, WorkGroupCall{}, false, tool.err
	}
	entry.shell = shell
	return entry, WorkGroupCall{ID: shell.ToolCallID, ToolKind: shell.ToolKind, SubagentID: shell.SubagentID, Header: shell.SubagentHeader, Media: media}, true, nil
}

// skimParallelBytes: a timeline at least this large is read on several
// cores, its entries split by bytes.
const skimParallelBytes = 1 << 20

// timelineElementSpans finds the entries of the timeline array at [start, end)
// by where each begins: every entry is a TimelineEntry object, whose first
// member is "Key" and which no other object in it has, and an unescaped
// {"Key": cannot occur inside a string. It reports false (use the sequential
// walk) for a small timeline or a shape it does not expect; each entry is
// still parsed and must end exactly where the next begins.
func timelineElementSpans(raw []byte, start, end int) ([][2]int, bool) {
	if end-start < skimParallelBytes || raw[start] != '[' || raw[end-1] != ']' {
		return nil, false
	}
	var starts []int
	for at := start + 1; at < end; {
		k := bytes.IndexByte(raw[at:end], 'K')
		if k < 0 {
			break
		}
		k += at
		at = k + 1
		p := k - 2
		if p <= start || k+5 > end || raw[p] != '{' || raw[p+1] != '"' || raw[k+1] != 'e' || raw[k+2] != 'y' || raw[k+3] != '"' || raw[k+4] != ':' {
			continue
		}
		switch {
		case raw[p-1] == '[' && p-1 == start:
		case raw[p-1] == ',' && raw[p-2] == '}':
		default:
			continue
		}
		starts = append(starts, p)
	}
	if len(starts) == 0 || starts[0] != start+1 {
		return nil, false
	}
	spans := make([][2]int, len(starts))
	for index, from := range starts {
		to := end - 1
		if index+1 < len(starts) {
			to = starts[index+1] - 1
		}
		spans[index] = [2]int{from, to}
	}
	return spans, true
}

// runSkimRanges runs skim over [0, len(spans)), on several goroutines (ranges
// of about equal bytes) when parallel.
func runSkimRanges(spans [][2]int, parallel bool, skim func(from, to int) error) error {
	workers := min(runtime.GOMAXPROCS(0), 12)
	if !parallel || workers < 2 || len(spans) < 2*workers {
		return skim(0, len(spans))
	}
	total := spans[len(spans)-1][1] - spans[0][0]
	share := total/workers + 1
	var group sync.WaitGroup
	errs := make([]error, workers)
	from := 0
	for worker := 0; worker < workers && from < len(spans); worker++ {
		to := from + 1
		limit := spans[from][0] + share
		for to < len(spans) && (spans[to][1] <= limit || worker == workers-1) {
			to++
		}
		group.Add(1)
		go func(worker, from, to int) {
			defer group.Done()
			errs[worker] = skim(from, to)
		}(worker, from, to)
		from = to
	}
	group.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if from < len(spans) {
		return skim(from, len(spans))
	}
	return nil
}

// ledgerTimelineSpans are the byte spans of a stored row's timeline entries.
func ledgerTimelineSpans(raw []byte) ([][2]int, error) {
	s := skimmer{data: raw, row: true}
	timelineStart, timelineStop := -1, -1
	if err := s.object(func(key []byte, start, end int) error {
		if string(key) == "Timeline" {
			timelineStart, timelineStop = start, end
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if timelineStart < 0 || raw[timelineStart] != '[' {
		return nil, nil
	}
	// Each entry is decoded on its own, which checks its span.
	if spans, ok := timelineElementSpans(raw, timelineStart, timelineStop); ok {
		return spans, nil
	}
	var spans [][2]int
	walk := skimmer{data: raw, pos: timelineStart}
	err := walk.array(func(start, end int) error {
		spans = append(spans, [2]int{start, end})
		return nil
	})
	if err == nil && walk.pos != timelineStop {
		err = errSkim
	}
	return spans, err
}

// skimmer walks JSON written by encoding/json without decoding it: string
// bodies are skipped with bytes.IndexByte, so a row's megabytes of payload
// cost a scan for quotes.
type skimmer struct {
	data []byte
	pos  int
	err  error
	// row: a stored row's top level, whose timeline is not walked to find
	// where it ends (timelineEnd).
	row bool
}

// jumpTo is where the value of member key at s.pos ends, when it is found
// without walking it, else -1.
func (s *skimmer) jumpTo(key []byte) int {
	if !s.row || string(key) != "Timeline" {
		return -1
	}
	return timelineEnd(s.data, s.pos)
}

// timelineEnd finds where a stored row's timeline (at start) ends: the row's
// last member, Permission, follows it, and no other object in a row has a
// Permission member (an unescaped ,"Permission": cannot occur in a string).
// The entries are then parsed up to exactly that end, so a wrong guess only
// sends the row to the ordinary decode.
func timelineEnd(raw []byte, start int) int {
	if start >= len(raw) || raw[start] != '[' {
		return -1
	}
	marker := bytes.LastIndex(raw, []byte(`,"Permission":`))
	if marker <= start || raw[marker-1] != ']' {
		return -1
	}
	return marker
}

func (s *skimmer) ws() {
	for s.pos < len(s.data) {
		switch s.data[s.pos] {
		case ' ', '\n', '\r', '\t':
			s.pos++
		default:
			return
		}
	}
}

// str skips a string starting at s.pos ('"') and returns its body span. It
// jumps from quote to quote (bytes.IndexByte); a quote ends the string when
// the run of backslashes before it is even.
func (s *skimmer) str() (int, int, error) {
	data := s.data
	if s.pos >= len(data) || data[s.pos] != '"' {
		return 0, 0, errSkim
	}
	start := s.pos + 1
	at := start
	for {
		quote := bytes.IndexByte(data[at:], '"')
		if quote < 0 {
			return 0, 0, errSkim
		}
		end := at + quote
		k := end - 1
		for k >= start && data[k] == '\\' {
			k--
		}
		if (end-1-k)%2 == 0 {
			s.pos = end + 1
			return start, end, nil
		}
		at = end + 1
	}
}

// value skips one value and returns its span.
func (s *skimmer) value() (int, int, error) {
	s.ws()
	if s.pos >= len(s.data) {
		return 0, 0, errSkim
	}
	start := s.pos
	switch s.data[s.pos] {
	case '"':
		if _, _, err := s.str(); err != nil {
			return 0, 0, err
		}
	case '{', '[':
		depth := 0
		for s.pos < len(s.data) {
			switch s.data[s.pos] {
			case '"':
				if _, _, err := s.str(); err != nil {
					return 0, 0, err
				}
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			s.pos++
			if depth == 0 {
				return start, s.pos, nil
			}
		}
		return 0, 0, errSkim
	default:
		for s.pos < len(s.data) {
			switch s.data[s.pos] {
			case ',', '}', ']', ' ', '\n', '\r', '\t':
				return start, s.pos, nil
			}
			s.pos++
		}
	}
	return start, s.pos, nil
}

// object visits each member of the object at s.pos with its key and value span.
func (s *skimmer) object(visit func(key []byte, start, end int) error) error {
	s.ws()
	if s.pos >= len(s.data) || s.data[s.pos] != '{' {
		return errSkim
	}
	s.pos++
	for {
		s.ws()
		if s.pos < len(s.data) && s.data[s.pos] == '}' {
			s.pos++
			return nil
		}
		keyStart, keyEnd, err := s.str()
		if err != nil {
			return err
		}
		s.ws()
		if s.pos >= len(s.data) || s.data[s.pos] != ':' {
			return errSkim
		}
		s.pos++
		s.ws()
		var start, end int
		if jumped := s.jumpTo(s.data[keyStart:keyEnd]); jumped > s.pos {
			start, end = s.pos, jumped
			s.pos = jumped
		} else if start, end, err = s.value(); err != nil {
			return err
		}
		if err := visit(s.data[keyStart:keyEnd], start, end); err != nil {
			return err
		}
		s.ws()
		if s.pos < len(s.data) && s.data[s.pos] == ',' {
			s.pos++
			continue
		}
		if s.pos < len(s.data) && s.data[s.pos] == '}' {
			s.pos++
			return nil
		}
		return errSkim
	}
}

// array visits each element of the array at s.pos with its span.
func (s *skimmer) array(visit func(start, end int) error) error {
	s.ws()
	if s.pos >= len(s.data) || s.data[s.pos] != '[' {
		return errSkim
	}
	s.pos++
	for {
		s.ws()
		if s.pos < len(s.data) && s.data[s.pos] == ']' {
			s.pos++
			return nil
		}
		start, end, err := s.value()
		if err != nil {
			return err
		}
		if err := visit(start, end); err != nil {
			return err
		}
		s.ws()
		if s.pos < len(s.data) && s.data[s.pos] == ',' {
			s.pos++
			continue
		}
		if s.pos < len(s.data) && s.data[s.pos] == ']' {
			s.pos++
			return nil
		}
		return errSkim
	}
}

// smallString decodes the string value at [start, end) (quotes included);
// null is "". A value that is not a string records errSkim.
func (s *skimmer) smallString(start, end int) string {
	value := s.data[start:end]
	if bytes.Equal(value, []byte("null")) {
		return ""
	}
	if len(value) < 2 || value[0] != '"' {
		s.err = errSkim
		return ""
	}
	body := value[1 : len(value)-1]
	if bytes.IndexByte(body, '\\') < 0 {
		return string(body)
	}
	var out string
	if err := json.Unmarshal(value, &out); err != nil {
		s.err = fmt.Errorf("%w: %v", errSkim, err)
	}
	return out
}
