package main

import (
	"strconv"

	"workass/internal/wire"
)

// First screens in two parts. A chat's first screen carries its ten newest
// rows (243 KB for c94k), but what a reader sees first is its last exchange
// (about 10 KB): parsing the rest held the first paint. A first-screen read
// with {exchangeFirst} answers only the last exchange, and one with {rest}
// the tail's rows before it; a renderer sends both at once, paints the
// first and lays out the second right behind it. Both parts are kept with
// the whole first screen (in memory, on disk, per chat revision) and framed
// as kept. Replies are the conditional form with an additive part:
//
//	{etag, messages, part: "exchange"}
//	{etag, messages, part: "rest", complete}
//
// complete false: the tail was too large to be the first screen whole, so
// its rows before the exchange are not here (the renderer pages them in, as
// before). A daemon that predates them answers the whole first screen to
// either, without a part.

const (
	firstScreenExchange = "exchange"
	firstScreenRest     = "rest"
)

// firstScreenParts splits a first-screen page; complete reports whether the
// rest is the tail's rows before the exchange.
func firstScreenParts(page archivePage, limit, ledgerCount int) (exchange, rest []byte, complete bool, ok bool) {
	if len(page.encoded) != len(page.messages) || len(page.messages) == 0 {
		return nil, nil, false, false
	}
	at := lastExchangeStart(page.messages)
	join := func(rows [][]byte) []byte {
		size := 2
		for _, row := range rows {
			size += len(row) + 1
		}
		out := make([]byte, 0, size)
		out = append(out, '[')
		for index, row := range rows {
			if index > 0 {
				out = append(out, ',')
			}
			out = append(out, row...)
		}
		return append(out, ']')
	}
	complete = len(page.messages) >= min(limit, ledgerCount)
	return join(page.encoded[at:]), join(page.encoded[:at]), complete, true
}

func firstScreenPartKey(chatID string, limit int, opts archiveReadOptions, part string) pageMemoKey {
	return pageMemoKey{chatID: chatID, before: "first-" + part + ":" + strconv.Itoa(limit), opts: opts}
}

// keepFirstScreenParts keeps both parts at revision; the rest's
// completeness rides in its etag's prefix ("c" or "i").
func (r *providerChatRuntime) keepFirstScreenParts(chatID string, revision uint64, limit int, opts archiveReadOptions, page archivePage, fast bool) {
	exchange, rest, complete, ok := firstScreenParts(page, limit, r.ledgerCount(chatID))
	if !ok {
		return
	}
	keep := keepReply
	if fast {
		keep = keepReplyFast
	}
	store := r.workGroupPacks()
	store.keepPageKept(firstScreenPartKey(chatID, limit, opts, firstScreenExchange), revision, keep(exchange))
	kept := keep(rest)
	if complete {
		kept.etag = "c" + kept.etag
	} else {
		kept.etag = "i" + kept.etag
	}
	store.keepPageKept(firstScreenPartKey(chatID, limit, opts, firstScreenRest), revision, kept)
}

func keepReplyFast(raw []byte) keptReply {
	deflated := wire.DeflateSegmentFast(raw)
	if deflated == nil {
		return keptReply{}
	}
	return keptReply{deflated: deflated, size: len(raw), etag: rowsEtag(raw)}
}

func (r *providerChatRuntime) ledgerCount(chatID string) int {
	if summary, closed := r.closedSummary(chatID); closed {
		return summary.LedgerCount
	}
	r.mu.Lock()
	actor := r.actors[chatID]
	r.mu.Unlock()
	if actor == nil {
		return 0
	}
	return actor.engine.LedgerLen()
}

// FirstScreenPartReply is a {exchangeFirst} or {rest} first-screen read.
func (r *providerChatRuntime) FirstScreenPartReply(tabID string, limit int, opts archiveReadOptions, part, ifNoneMatch string) (any, bool, error) {
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return nil, found, err
	}
	revision, known := r.chatRevision(chatID)
	if !known {
		return nil, false, nil
	}
	store := r.workGroupPacks()
	key := firstScreenPartKey(chatID, limit, opts, part)
	kept, ok := store.keptPage(key, revision)
	if !ok && part == firstScreenExchange {
		// A closed chat no process read at this revision: its last exchange
		// alone is read (two rows for c94k, not the tail's 8.4 MB); the rest
		// read right behind reads, and keeps, the whole first screen.
		kept, ok = r.closedExchangeOnly(chatID, revision, limit, opts)
		if ok {
			store.keepPageKept(key, revision, kept)
		}
	}
	if !ok {
		// Read it (FirstScreenReply keeps the whole and both parts).
		if _, found, err := r.FirstScreenReply(tabID, limit, opts, true, ""); err != nil || !found {
			return nil, found, err
		}
		if kept, ok = store.keptPage(key, revision); !ok {
			// No parts for this read (an empty chat): the whole, as before.
			return r.FirstScreenReply(tabID, limit, opts, true, ifNoneMatch)
		}
	}
	tail := `,"part":"` + part + `"}`
	etag := kept.etag
	if part == firstScreenRest {
		complete := etag != "" && etag[0] == 'c'
		tail = `,"part":"rest","complete":` + strconv.FormatBool(complete) + `}`
	}
	if ifNoneMatch != "" && ifNoneMatch == etag {
		return wire.EncodedResult(`{"etag":"` + etag + `","unchanged":true}`), true, nil
	}
	return wire.SegmentedResult{Parts: []wire.SegmentedPart{
		{Literal: []byte(`{"etag":"` + etag + `","messages":`)},
		{Deflated: kept.deflated, Size: kept.size},
		{Literal: []byte(tail)},
	}}, true, nil
}

// closedExchangeOnly is a closed chat's last exchange as its first-screen
// part, read from its own rows only.
func (r *providerChatRuntime) closedExchangeOnly(chatID string, revision uint64, limit int, opts archiveReadOptions) (keptReply, bool) {
	summary, closed := r.closedSummary(chatID)
	if !closed || summary.Revision != revision || summary.Deleted || summary.LedgerCount == 0 {
		return keptReply{}, false
	}
	tail, err := r.closedLastExchangeTail(summary, max(1, limit))
	if err != nil {
		return keptReply{}, false
	}
	_, finished, err := r.closedProjectionPage(summary, tail, []archiveReadOptions{opts})
	if err != nil || finished == nil {
		return keptReply{}, false
	}
	page := r.splitLongRows(chatID, revision, true, *finished, []archiveReadOptions{opts})
	if len(page.messages) == 0 || lastExchangeStart(page.messages) != 0 {
		// Not exactly one exchange (a byte-bounded tail): read it whole.
		return keptReply{}, false
	}
	kept := keepReplyFast(page.raw())
	return kept, kept.deflated != nil
}
