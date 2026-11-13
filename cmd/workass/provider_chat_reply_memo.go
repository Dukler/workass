package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"workass/internal/wire"
)

// Kept replies: history rows a cache holds compressed once.
//
// A kept first screen was copied, hashed for its etag and compressed again
// for every read (3.6 ms of a warm repeat of a 243 KB first screen). A
// keptReply holds the rows as one DEFLATE segment with their size and etag:
// a read frames the segment as it is (wire.SegmentedResult), and holds about
// a fifth of the memory, so a cache can keep the first screens of two read
// shapes (the current renderer's and an older one's) in less than one took.

type keptReply struct {
	deflated []byte
	size     int
	// etag is etagReply's for the rows' conditional reply ({"messages":rows}).
	etag string
}

// keepReply compresses a reply's rows; a zero keptReply when it cannot.
func keepReply(raw []byte) keptReply {
	deflated := wire.DeflateSegment(raw)
	if deflated == nil {
		return keptReply{}
	}
	return keptReply{deflated: deflated, size: len(raw), etag: rowsEtag(raw)}
}

// rowsEtag is the etag conditionalRowsReply names rows by.
func rowsEtag(rows []byte) string {
	sum := sha256.New()
	sum.Write([]byte(`{"messages":`))
	sum.Write(rows)
	sum.Write([]byte{'}'})
	return hex.EncodeToString(sum.Sum(nil))
}

// plain is the rows themselves, in a buffer the hub may frame in place.
func (k keptReply) plain() (wire.EncodedResult, error) {
	out, err := wire.InflateSegment(make([]byte, 0, k.size+rawReplySpare), k.deflated)
	return out, err
}

// result is the rows as the plain reply: the bytes of the array.
func (k keptReply) result() wire.SegmentedResult {
	return wire.SegmentedResult{Parts: []wire.SegmentedPart{{Deflated: k.deflated, Size: k.size}}}
}

// conditional is conditionalRowsReply of the rows: a receipt when the reader
// holds them, else {"etag":E,"messages":rows}, byte for byte.
func (k keptReply) conditional(ifNoneMatch string) any {
	if ifNoneMatch != "" && ifNoneMatch == k.etag {
		return wire.EncodedResult(`{"etag":"` + k.etag + `","unchanged":true}`)
	}
	return wire.SegmentedResult{Parts: []wire.SegmentedPart{
		{Literal: []byte(`{"etag":"` + k.etag + `","messages":`)},
		{Deflated: k.deflated, Size: k.size},
		{Literal: []byte{'}'}},
	}}
}

// The reply memo: a chat's first screen, as its reply, encoded once per chat
// revision and read shape, for open chats as for closed ones (whose first
// screens the warm cache also holds). A reader opening, reopening or
// refreshing a chat that has not changed gets the kept segment.

const replyMemoBytes = 16 << 20

type replyMemoKey struct {
	chatID string
	limit  int
	opts   archiveReadOptions
}

type replyMemoEntry struct {
	revision uint64
	kept     keptReply
	used     uint64
}

type replyMemo struct {
	mu      sync.Mutex
	entries map[replyMemoKey]*replyMemoEntry
	bytes   int
	clock   uint64
}

func (m *replyMemo) get(key replyMemoKey, revision uint64) (keptReply, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok || entry.revision != revision {
		return keptReply{}, false
	}
	m.clock++
	entry.used = m.clock
	return entry.kept, true
}

func (m *replyMemo) put(key replyMemoKey, revision uint64, kept keptReply) {
	if kept.deflated == nil || len(kept.deflated) > replyMemoBytes/8 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[replyMemoKey]*replyMemoEntry)
	}
	if previous, ok := m.entries[key]; ok {
		if previous.revision > revision {
			return
		}
		m.bytes -= len(previous.kept.deflated)
	}
	m.clock++
	m.entries[key] = &replyMemoEntry{revision: revision, kept: kept, used: m.clock}
	m.bytes += len(kept.deflated)
	for m.bytes > replyMemoBytes {
		var oldest replyMemoKey
		var oldestUse uint64
		found := false
		for candidate, entry := range m.entries {
			if !found || entry.used < oldestUse {
				oldest, oldestUse, found = candidate, entry.used, true
			}
		}
		if !found {
			break
		}
		m.bytes -= len(m.entries[oldest].kept.deflated)
		delete(m.entries, oldest)
	}
}

// chatRevision is a chat's revision without opening it: a closed chat's
// summary, an open chat's actor.
func (r *providerChatRuntime) chatRevision(chatID string) (uint64, bool) {
	chatID = strings.TrimSpace(chatID)
	r.mu.Lock()
	if summary, closed := r.summaries[chatID]; closed {
		r.mu.Unlock()
		return summary.Revision, !summary.Deleted
	}
	actor := r.actors[chatID]
	r.mu.Unlock()
	if actor == nil {
		return 0, false
	}
	return actor.engine.Revision(), true
}

// FirstScreenReply is the archive-load reply of a chat's first screen
// ({tail, lastExchange}), conditional with ifNoneMatch: from the memo when
// the chat has not changed since it was encoded, else read, and kept for
// the next reader (compressed after this reply, not before it).
func (r *providerChatRuntime) FirstScreenReply(tabID string, limit int, opts archiveReadOptions, conditional bool, ifNoneMatch string) (any, bool, error) {
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return nil, found, err
	}
	// A closed chat's first screen read ahead by the warm cache.
	if summary, closed := r.closedSummary(chatID); closed {
		if kept, ok := r.cachedFirstScreen(summary, limit, opts); ok {
			if conditional {
				return kept.conditional(ifNoneMatch), true, nil
			}
			return kept.result(), true, nil
		}
	}
	revision, known := r.chatRevision(chatID)
	key := replyMemoKey{chatID: chatID, limit: limit, opts: opts}
	if known {
		if kept, ok := r.replies.get(key, revision); ok {
			if conditional {
				return kept.conditional(ifNoneMatch), true, nil
			}
			return kept.result(), true, nil
		}
	}
	raw, found, err := r.ProjectLastExchangeByTabRaw(tabID, limit, opts)
	if err != nil || !found {
		return nil, found, err
	}
	if !known {
		if conditional {
			return conditionalRowsReply(raw, ifNoneMatch), true, nil
		}
		return raw, true, nil
	}
	// Compressed and hashed once, here: the reply goes out as the kept
	// segment (no second compression on the socket, no second hash), and the
	// next reader of this revision gets it as it is.
	// A closed chat's read was just kept, compressed, by the warm cache.
	kept, cached := keptReply{}, false
	if summary, closed := r.closedSummary(chatID); closed {
		kept, cached = r.cachedFirstScreen(summary, limit, opts)
	}
	if !cached {
		kept = keepReply(raw)
	}
	if kept.deflated == nil {
		if conditional {
			return conditionalRowsReply(raw, ifNoneMatch), true, nil
		}
		return raw, true, nil
	}
	r.replies.put(key, revision, kept)
	if conditional {
		return kept.conditional(ifNoneMatch), true, nil
	}
	return kept.result(), true, nil
}

// History page memo: a large page (an older renderer's page holding one row
// of thousands of calls is 13.9 MB, read, decoded, finished, encoded and
// compressed in 0.3 s, every time) is kept compressed per chat revision, in
// memory and on disk with the chat's work group packs, so a later read, in
// this process or the next, frames the kept segment. A chat's revision
// moves with every commit, so a kept page is only ever the page it was.

// pageMemoMinBytes: smaller pages are read faster than kept.
var pageMemoMinBytes = 512 << 10

const (
	// pageMemoMemoryBytes bounds the kept pages held in memory.
	pageMemoMemoryBytes = 32 << 20
	pageMemoMagic       = "WHP1"
)

type pageMemoKey struct {
	chatID, before string
	limit          int
	opts           archiveReadOptions
}

func (k pageMemoKey) name() string {
	return hashName("page", k.before, strconv.Itoa(k.limit), workGroupShape([]archiveReadOptions{k.opts}, false), strconv.FormatBool(k.opts.workGroups), strconv.FormatBool(k.opts.longRows)) + ".whp"
}

type pageMemoEntry struct {
	revision uint64
	kept     keptReply
	used     uint64
}

func (s *workGroupPackStore) pagePath(key pageMemoKey) string {
	return filepath.Join(s.chatDir(key.chatID), key.name())
}

// keptPage is the page kept at revision, from memory or disk.
func (s *workGroupPackStore) keptPage(key pageMemoKey, revision uint64) (keptReply, bool) {
	s.mu.Lock()
	if entry, ok := s.pages[key]; ok {
		if entry.revision == revision {
			s.clock.Add(1)
			entry.used = uint64(s.clock.Load())
			s.mu.Unlock()
			return entry.kept, true
		}
	}
	s.mu.Unlock()
	data, err := os.ReadFile(s.pagePath(key))
	if err != nil || len(data) < 20 || string(data[:4]) != pageMemoMagic {
		return keptReply{}, false
	}
	stored := binary.LittleEndian.Uint64(data[4:12])
	size := int(binary.LittleEndian.Uint32(data[12:16]))
	etagLen := int(binary.LittleEndian.Uint32(data[16:20]))
	if stored != revision || len(data) < 20+etagLen {
		return keptReply{}, false
	}
	kept := keptReply{etag: string(data[20 : 20+etagLen]), size: size, deflated: data[20+etagLen:]}
	s.rememberPage(key, revision, kept)
	return kept, true
}

func (s *workGroupPackStore) rememberPage(key pageMemoKey, revision uint64, kept keptReply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pages == nil {
		s.pages = make(map[pageMemoKey]*pageMemoEntry)
	}
	if previous, ok := s.pages[key]; ok {
		s.pageBytes -= len(previous.kept.deflated)
	}
	s.pages[key] = &pageMemoEntry{revision: revision, kept: kept, used: uint64(s.clock.Add(1))}
	s.pageBytes += len(kept.deflated)
	for s.pageBytes > pageMemoMemoryBytes {
		var oldest pageMemoKey
		var oldestUse uint64
		found := false
		for candidate, entry := range s.pages {
			if !found || entry.used < oldestUse {
				oldest, oldestUse, found = candidate, entry.used, true
			}
		}
		if !found {
			break
		}
		s.pageBytes -= len(s.pages[oldest].kept.deflated)
		delete(s.pages, oldest)
	}
}

// keepPage compresses and keeps a page read at revision (after its reply).
func (s *workGroupPackStore) keepPage(key pageMemoKey, revision uint64, raw []byte) {
	s.keepPageKept(key, revision, keepReply(raw))
}

// keepPageKept keeps an already compressed reply.
func (s *workGroupPackStore) keepPageKept(key pageMemoKey, revision uint64, kept keptReply) {
	if kept.deflated == nil {
		return
	}
	s.rememberPage(key, revision, kept)
	var out bytes.Buffer
	out.WriteString(pageMemoMagic)
	_ = binary.Write(&out, binary.LittleEndian, revision)
	_ = binary.Write(&out, binary.LittleEndian, uint32(kept.size))
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(kept.etag)))
	out.WriteString(kept.etag)
	out.Write(kept.deflated)
	path := s.pagePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if writeFileAtomic(path, out.Bytes(), 0o600) == nil {
		s.trimDisk(path)
	}
}
