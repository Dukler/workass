package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"workass/internal/chat"
	"workass/internal/wire"
)

// Work group packs: a folded row's calls, encoded once.
//
// Opening the 8,542-call row of the c94k chat read and decoded the whole
// stored row (tens of megabytes) once per 1 MiB page, eleven times, then
// projected, finished, encoded and compressed every call again: 0.8 s in a
// fresh process, 0.17 s warm, for 10.4 MB the renderer waited for whole.
//
// A pack is that work done once per row (and read shape). Its calls are
// finished and encoded exactly as a {workGroup} page sends them, joined in
// chunks of about workGroupChunkBytes, and each chunk is compressed once
// (wire.DeflateSegment): a page is its chunks' segments framed as they are,
// without a decode, an encoding or a compression. Each chunk also keeps
// where every call in it ends, so a page starting inside one sends only the
// calls it asks for. The pack is published chunk by chunk while it is
// built, so the first rows of a row that was never opened wait only for
// their own chunk; the rest is built behind them.
//
// Packs are kept on disk (state/work-groups, one directory per chat, removed
// with the chat), so a later process reads a page with two small reads: the
// pack's index, then the chunks the page covers.
//
// A pack is the row as it was stored at a chat revision (its stamp: the
// row's sequence and the CRC-32C of its stored JSON). At another revision it
// is still the row when this process wrote no other version of it
// (chat.SQLActors.RowStamp) and, for a pack an earlier process wrote, when
// the stored bytes still have its CRC (compared once per process).

// workGroupChunkBytes is about how much of a row's encoded calls a chunk
// holds: about 40 rows of the Worked list, its first screen.
var workGroupChunkBytes = 48 << 10

const (
	// workGroupPackMemoryBytes bounds the compressed chunks kept in memory;
	// a chunk on disk is read again when needed.
	workGroupPackMemoryBytes = 64 << 20
	// workGroupPackDiskBytes bounds state/work-groups.
	workGroupPackDiskBytes = 512 << 20
	workGroupPackDir       = "work-groups"
	workGroupPackMagic     = "WGP1"
)

type workGroupPackKey struct {
	chatID, messageID, shape string
	compact                  bool
}

// workGroupShape names a read's options in a pack's key.
func workGroupShape(opts []archiveReadOptions, compact bool) string {
	var merged archiveReadOptions
	for _, opt := range opts {
		merged.deferMessageImages = merged.deferMessageImages || opt.deferMessageImages
		merged.leanTools = merged.leanTools || opt.leanTools
		merged.leanTitles = merged.leanTitles || opt.leanTitles
		merged.toolSummaries = merged.toolSummaries || opt.toolSummaries
	}
	flag := func(on bool) byte {
		if on {
			return '1'
		}
		return '0'
	}
	return string([]byte{'d', flag(merged.deferMessageImages), 'l', flag(merged.leanTools), 't', flag(merged.leanTitles), 's', flag(merged.toolSummaries), 'c', flag(compact)})
}

type workGroupChunk struct {
	from, count int
	size        int      // plain bytes
	ends        []uint32 // where each call's JSON ends in the plain bytes
	zlen        int
	fileOffset  int64
	deflated    []byte // nil while only on disk
}

func (c *workGroupChunk) start(index int) int {
	if index == 0 {
		return 0
	}
	return int(c.ends[index-1]) + 1 // after the comma
}

type workGroupPack struct {
	key   workGroupPackKey
	seq   int
	crc   uint32
	total int

	mu       sync.Mutex
	cond     *sync.Cond
	revision uint64
	verified bool // its stamp was compared, or it was built, in this process
	chunks   []*workGroupChunk
	done     bool
	err      error
	path     string
	used     atomic.Int64
}

func newWorkGroupPack(key workGroupPackKey) *workGroupPack {
	pack := &workGroupPack{key: key}
	pack.cond = sync.NewCond(&pack.mu)
	return pack
}

type workGroupPackStore struct {
	dir   string
	mu    sync.Mutex
	packs map[workGroupPackKey]*workGroupPack
	// memory is the compressed chunk bytes held by packs.
	memory int
	clock  atomic.Int64
	// building bounds concurrent builds.
	building chan struct{}
	// pages are large history pages kept per chat revision
	// (provider_chat_reply_memo.go).
	pages     map[pageMemoKey]*pageMemoEntry
	pageBytes int
}

var workGroupPackStores sync.Map // state dir -> *workGroupPackStore

func (r *providerChatRuntime) workGroupPacks() *workGroupPackStore {
	stateDir := filepath.Dir(r.sessions.path)
	if store, ok := workGroupPackStores.Load(stateDir); ok {
		return store.(*workGroupPackStore)
	}
	store, _ := workGroupPackStores.LoadOrStore(stateDir, &workGroupPackStore{
		dir: filepath.Join(stateDir, workGroupPackDir), packs: make(map[workGroupPackKey]*workGroupPack),
		building: make(chan struct{}, 2),
	})
	return store.(*workGroupPackStore)
}

func hashName(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))[:32]
}

func (s *workGroupPackStore) chatDir(chatID string) string {
	return filepath.Join(s.dir, hashName("chat", chatID))
}

func (s *workGroupPackStore) packPath(key workGroupPackKey) string {
	return filepath.Join(s.chatDir(key.chatID), hashName(key.messageID, key.shape)+".wgp")
}

// forgetChat drops every pack of a chat, in memory and on disk.
func (s *workGroupPackStore) forgetChat(chatID string) {
	s.mu.Lock()
	for key, pack := range s.packs {
		if key.chatID == chatID {
			s.memory -= pack.memoryBytes()
			delete(s.packs, key)
		}
	}
	for key, entry := range s.pages {
		if key.chatID == chatID {
			s.pageBytes -= len(entry.kept.deflated)
			delete(s.pages, key)
		}
	}
	s.mu.Unlock()
	_ = os.RemoveAll(s.chatDir(chatID))
}

func (p *workGroupPack) memoryBytes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, chunk := range p.chunks {
		total += len(chunk.deflated)
	}
	return total
}

// rowNow is what a pack's stamp is compared with: the chat's revision and,
// when the chat is open, where the row sits.
type rowNow struct {
	revision uint64
	seq      int // 0 when not known without a read
}

// workGroupRowNow is the row's current revision, or found false when the
// chat or row is gone.
func (r *providerChatRuntime) workGroupRowNow(chatID, messageID string) (rowNow, bool, error) {
	if summary, closed := r.closedSummary(chatID); closed {
		if summary.Deleted {
			return rowNow{}, false, nil
		}
		return rowNow{revision: summary.Revision}, true, nil
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return rowNow{}, false, err
	}
	seq, revision, ok := actor.engine.MessageStamp(messageID)
	if !ok {
		return rowNow{}, false, nil
	}
	return rowNow{revision: revision, seq: seq}, true, nil
}

// current reports whether the pack is still the row now is of.
func (r *providerChatRuntime) packCurrent(pack *workGroupPack, now rowNow) bool {
	pack.mu.Lock()
	defer pack.mu.Unlock()
	if pack.err != nil {
		return false
	}
	if pack.revision == now.revision {
		return true
	}
	if now.seq != 0 && now.seq != pack.seq {
		return false
	}
	if r.ledgers == nil {
		// Packs built from an open actor's memory hold for one revision.
		return false
	}
	if stamp, written := r.ledgers.RowStamp(pack.key.chatID, pack.seq); written {
		if stamp != pack.crc {
			return false
		}
	} else if !pack.verified {
		raw, found, err := r.ledgers.ReadLedgerRowAt(pack.key.chatID, pack.seq, pack.key.messageID)
		if err != nil || !found || chat.RowCRC(raw) != pack.crc {
			return false
		}
	}
	pack.verified = true
	pack.revision = now.revision
	return true
}

// workGroupPack is the row's current pack, built (in the background, chunk by
// chunk) when there is none.
func (r *providerChatRuntime) workGroupPack(chatID, messageID string, compact bool, opts []archiveReadOptions) (*workGroupPack, bool, error) {
	store := r.workGroupPacks()
	key := workGroupPackKey{chatID: chatID, messageID: messageID, shape: workGroupShape(opts, compact), compact: compact}
	now, found, err := r.workGroupRowNow(chatID, messageID)
	if err != nil || !found {
		return nil, found, err
	}
	store.mu.Lock()
	pack := store.packs[key]
	store.mu.Unlock()
	if pack == nil && r.ledgers != nil {
		if loaded, err := store.load(key); err == nil {
			pack = loaded
			store.mu.Lock()
			if existing := store.packs[key]; existing != nil {
				pack = existing
			} else {
				store.packs[key] = pack
			}
			store.mu.Unlock()
		}
	}
	if pack != nil && r.packCurrent(pack, now) {
		pack.used.Store(store.clock.Add(1))
		return pack, true, nil
	}
	pack = newWorkGroupPack(key)
	pack.used.Store(store.clock.Add(1))
	store.mu.Lock()
	if previous := store.packs[key]; previous != nil {
		store.memory -= previous.memoryBytes()
	}
	store.packs[key] = pack
	store.mu.Unlock()
	timeline, found, err := r.workGroupSource(pack, chatID, messageID)
	if err != nil || !found {
		store.mu.Lock()
		if store.packs[key] == pack {
			delete(store.packs, key)
		}
		store.mu.Unlock()
		return nil, found, err
	}
	go r.buildWorkGroupPack(store, pack, timeline, opts)
	return pack, true, nil
}

// workGroupSource reads the row a pack is built from and stamps the pack.
func (r *providerChatRuntime) workGroupSource(pack *workGroupPack, chatID, messageID string) (rowTimeline, bool, error) {
	if r.ledgers != nil {
		raw, seq, revision, found, err := r.ledgers.ReadLedgerMessageForPack(chatID, messageID)
		if err != nil || !found {
			return rowTimeline{}, found, err
		}
		stored, err := chat.ParseStoredTimeline(raw)
		if err != nil {
			return rowTimeline{}, false, err
		}
		pack.seq, pack.crc, pack.revision, pack.verified, pack.total = seq, chat.RowCRC(raw), revision, true, stored.Len()
		return rowTimeline{stored: stored, closed: true}, true, nil
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return rowTimeline{}, false, err
	}
	event, seq, revision, found := actor.engine.ReadMessageAt(messageID)
	if !found {
		return rowTimeline{}, false, nil
	}
	pack.seq, pack.revision, pack.verified, pack.total = seq, revision, true, len(event.Timeline)
	return rowTimeline{entries: event.Timeline}, true, nil
}

// buildWorkGroupPack finishes and encodes the row's entries, publishing each
// chunk as it is done, then writes the pack to disk.
func (r *providerChatRuntime) buildWorkGroupPack(store *workGroupPackStore, pack *workGroupPack, timeline rowTimeline, opts []archiveReadOptions) {
	store.building <- struct{}{}
	defer func() { <-store.building }()
	err := r.encodeWorkGroupChunks(pack, timeline, opts, func(chunk *workGroupChunk) {
		pack.mu.Lock()
		pack.chunks = append(pack.chunks, chunk)
		pack.mu.Unlock()
		pack.cond.Broadcast()
		store.mu.Lock()
		store.memory += len(chunk.deflated)
		store.mu.Unlock()
	})
	pack.mu.Lock()
	pack.done, pack.err = true, err
	pack.mu.Unlock()
	pack.cond.Broadcast()
	if err != nil {
		store.mu.Lock()
		if store.packs[pack.key] == pack {
			store.memory -= pack.memoryBytes()
			delete(store.packs, pack.key)
		}
		store.mu.Unlock()
		return
	}
	if r.ledgers != nil {
		if path, err := store.write(pack); err == nil {
			pack.mu.Lock()
			pack.path = path
			pack.mu.Unlock()
		}
	}
	store.trimMemory()
}

// workGroupCompactNulls are the fields every projected call has
// (projectTimeline) that a compact pack leaves out when they are null.
var workGroupCompactNulls = []string{"id", "toolKind", "command", "terminalId", "input", "output", "location"}

// compactWorkGroupEvent is a call as a compact page sends it ({workGroup:
// {compact: true}}): without the fields every call has that are null, without
// its key when that is "timeline:tool:" and its id, and with titleIs:
// "command" instead of a title equal to its command. The reader restores
// exactly the event (desktop/renderer2 chat/work-group.ts expandWorkEvent).
func compactWorkGroupEvent(event map[string]any) {
	if fieldString(event, "kind") != "tool" {
		return
	}
	for _, field := range workGroupCompactNulls {
		if value, present := event[field]; present && value == nil {
			delete(event, field)
		}
	}
	if id, ok := event["id"].(string); ok && event["key"] == "timeline:tool:"+id {
		delete(event, "key")
	}
	if title, ok := event["title"].(string); ok && title != "" && event["command"] == title {
		delete(event, "title")
		event["titleIs"] = "command"
	}
}

// workGroupFirstRange is how many entries the first range holds, in chunks
// of workGroupChunkBytes: the Worked list's first screen and one ahead
// (WORK_GROUP_FIRST_PAGE in desktop/renderer2), published before anything
// else is built.
var workGroupFirstRange = 128

// workGroupRange is how many entries each later range holds; ranges are
// built in parallel and published in order.
const workGroupRange = 512

// workGroupLaterChunkBytes: past the first screen, chunks are larger, so the
// compressor keeps its ratio (a chunk starts a fresh window).
var workGroupLaterChunkBytes = 256 << 10

func (r *providerChatRuntime) encodeWorkGroupChunks(pack *workGroupPack, timeline rowTimeline, opts []archiveReadOptions, publish func(*workGroupChunk)) error {
	total := timeline.length()
	type span struct{ from, to int }
	var spans []span
	for from := 0; from < total; {
		size := workGroupRange
		if from == 0 {
			size = workGroupFirstRange
		}
		to := min(total, from+size)
		spans = append(spans, span{from, to})
		from = to
	}
	type built struct {
		chunks []*workGroupChunk
		err    error
	}
	results := make([]chan built, len(spans))
	for index := range results {
		results[index] = make(chan built, 1)
	}
	workers := max(2, min(8, runtime.GOMAXPROCS(0)/2))
	jobs := make(chan int)
	stop := make(chan struct{})
	defer close(stop)
	for range workers {
		go func() {
			resolved := make(map[string]string)
			for index := range jobs {
				target := workGroupLaterChunkBytes
				if index == 0 {
					target = workGroupChunkBytes
				}
				chunks, err := r.encodeWorkGroupRange(pack.key.messageID, timeline, spans[index].from, spans[index].to, target, pack.key.compact, opts, resolved)
				results[index] <- built{chunks, err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := range spans {
			select {
			case jobs <- index:
			case <-stop:
				return
			}
		}
	}()
	for index := range spans {
		result := <-results[index]
		if result.err != nil {
			return result.err
		}
		for _, chunk := range result.chunks {
			publish(chunk)
		}
	}
	return nil
}

// encodeWorkGroupRange finishes, encodes and compresses entries [from, to)
// into chunks of about target plain bytes. Entries are projected one at a
// time (as a page always did) and finished in batches, which finish each
// call on its own.
func (r *providerChatRuntime) encodeWorkGroupRange(messageID string, timeline rowTimeline, from, to, target int, compact bool, opts []archiveReadOptions, resolved map[string]string) ([]*workGroupChunk, error) {
	stateDir := filepath.Dir(r.sessions.path)
	var chunks []*workGroupChunk
	var plain bytes.Buffer
	chunk := &workGroupChunk{from: from}
	flush := func(next int) {
		if chunk.count == 0 {
			return
		}
		chunk.size = plain.Len()
		chunk.deflated = wire.DeflateSegment(plain.Bytes())
		chunk.zlen = len(chunk.deflated)
		chunks = append(chunks, chunk)
		plain.Reset()
		chunk = &workGroupChunk{from: next}
	}
	const batch = 64
	for start := from; start < to; start += batch {
		end := min(to, start+batch)
		events := make([]any, 0, end-start)
		for index := start; index < end; index++ {
			entry, err := timeline.entry(index)
			if err != nil {
				return nil, err
			}
			projected, err := projectTimeline([]chat.TimelineEntry{entry})
			if err != nil {
				return nil, err
			}
			if len(projected) != 1 {
				return nil, fmt.Errorf("work group entry %d projected to %d events", index, len(projected))
			}
			events = append(events, projected[0])
		}
		holder := []any{map[string]any{"id": messageID, "role": "assistant", "status": "done", "events": events}}
		for _, opt := range opts {
			opt.beforeBudget(holder)
		}
		if err := finishArchiveRowValue(holder, stateDir, opts, resolved); err != nil {
			return nil, err
		}
		finished := anySlice(mapFromAnyMain(holder[0])["events"])
		if len(finished) != end-start {
			return nil, fmt.Errorf("work group batch %d finished to %d events", start, len(finished))
		}
		for offset, event := range finished {
			if compact {
				compactWorkGroupEvent(mapFromAnyMain(event))
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				return nil, err
			}
			if chunk.count > 0 {
				plain.WriteByte(',')
			}
			plain.Write(encoded)
			chunk.ends = append(chunk.ends, uint32(plain.Len()))
			chunk.count++
			if plain.Len() >= target {
				flush(start + offset + 1)
			}
		}
	}
	flush(to)
	return chunks, nil
}

var errWorkGroupPackGone = errors.New("work group pack was dropped while it was read")

// page is the {workGroup} reply from offset: at least limit entries (whole
// chunks) when limit > 0, else up to budget plain bytes, of what is built.
func (s *workGroupPackStore) page(pack *workGroupPack, offset, limit, budget int, ifNoneMatch string) (wire.SegmentedResult, error) {
	pack.mu.Lock()
	// A page with a limit (the Worked list's first screen) waits only for the
	// chunk it starts in; a page by budget waits until it can be full.
	for !pack.done && !packCovers(pack, offset) || !pack.done && limit <= 0 && packBuiltBytes(pack, offset) < budget {
		pack.cond.Wait()
	}
	if pack.err != nil {
		err := pack.err
		pack.mu.Unlock()
		return wire.SegmentedResult{}, err
	}
	if offset > pack.total {
		pack.mu.Unlock()
		return wire.SegmentedResult{}, fmt.Errorf("work group offset %d is past the row's %d entries", offset, pack.total)
	}
	chunks := append([]*workGroupChunk(nil), pack.chunks...)
	path, total := pack.path, pack.total
	pack.mu.Unlock()

	// The chunks the page sends: from offset's on, until limit entries or
	// budget bytes.
	first := sort.Search(len(chunks), func(i int) bool { return chunks[i].from+chunks[i].count > offset })
	last, next, sent, plainBytes := first, offset, 0, 0
	for ; last < len(chunks); last++ {
		chunk := chunks[last]
		if sent > 0 && (limit > 0 && sent >= limit || limit <= 0 && plainBytes+chunk.size > budget) {
			break
		}
		inner := max(0, offset-chunk.from)
		sent += chunk.count - inner
		plainBytes += chunk.size - chunk.start(inner)
		next = chunk.from + chunk.count
	}
	// The page is named by the pack's stamp and its bounds: the same name is
	// the same bytes.
	etag := fmt.Sprintf("wg1-%08x-%d-%s-%d-%d", pack.crc, pack.seq, pack.key.shape, offset, next)
	if ifNoneMatch != "" && ifNoneMatch == etag {
		return wire.SegmentedResult{Parts: []wire.SegmentedPart{{Literal: []byte(`{"etag":"` + etag + `","unchanged":true}`)}}}, nil
	}
	parts := []wire.SegmentedPart{{Literal: []byte(`{"etag":"` + etag + `","events":[`)}}
	for index := first; index < last; index++ {
		chunk := chunks[index]
		deflated, err := s.chunkBody(pack, chunk, path)
		if err != nil {
			return wire.SegmentedResult{}, err
		}
		if index > first {
			parts = append(parts, wire.SegmentedPart{Literal: []byte{','}})
		}
		if offset > chunk.from {
			// A page starting inside a chunk: only the calls it asks for.
			plain, err := wire.InflateSegment(nil, deflated)
			if err != nil {
				return wire.SegmentedResult{}, err
			}
			parts = append(parts, wire.SegmentedPart{Literal: plain[chunk.start(offset-chunk.from):]})
		} else {
			parts = append(parts, wire.SegmentedPart{Deflated: deflated, Size: chunk.size})
		}
	}
	id, _ := json.Marshal(pack.key.messageID)
	tail := make([]byte, 0, 96+len(id))
	tail = append(tail, `],"messageId":`...)
	tail = append(tail, id...)
	tail = append(tail, `,"next":`...)
	tail = strconv.AppendInt(tail, int64(next), 10)
	tail = append(tail, `,"offset":`...)
	tail = strconv.AppendInt(tail, int64(offset), 10)
	tail = append(tail, `,"total":`...)
	tail = strconv.AppendInt(tail, int64(total), 10)
	tail = append(tail, '}')
	parts = append(parts, wire.SegmentedPart{Literal: tail})
	return wire.SegmentedResult{Parts: parts}, nil
}

// packBuiltBytes is how many plain bytes are built from offset's chunk on.
func packBuiltBytes(pack *workGroupPack, offset int) int {
	total := 0
	for _, chunk := range pack.chunks {
		if chunk.from+chunk.count > offset {
			total += chunk.size
		}
	}
	return total
}

func packCovers(pack *workGroupPack, offset int) bool {
	if len(pack.chunks) == 0 {
		return offset == 0 && pack.total == 0
	}
	last := pack.chunks[len(pack.chunks)-1]
	return offset < last.from+last.count
}

// chunkBody is a chunk's segment, read from the pack's file when only there.
func (s *workGroupPackStore) chunkBody(pack *workGroupPack, chunk *workGroupChunk, path string) ([]byte, error) {
	pack.mu.Lock()
	body := chunk.deflated
	pack.mu.Unlock()
	if body != nil {
		return body, nil
	}
	if path == "" {
		return nil, errWorkGroupPackGone
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body = make([]byte, chunk.zlen)
	if _, err := file.ReadAt(body, chunk.fileOffset); err != nil {
		return nil, err
	}
	pack.mu.Lock()
	if chunk.deflated == nil {
		chunk.deflated = body
		s.mu.Lock()
		s.memory += len(body)
		s.mu.Unlock()
	}
	pack.mu.Unlock()
	s.trimMemory()
	return body, nil
}

// trimMemory drops the chunks of the least recently used packs that are on
// disk, and packs that are not, until the store is within its budget.
func (s *workGroupPackStore) trimMemory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.memory <= workGroupPackMemoryBytes {
		return
	}
	packs := make([]*workGroupPack, 0, len(s.packs))
	for _, pack := range s.packs {
		packs = append(packs, pack)
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].used.Load() < packs[j].used.Load() })
	for _, pack := range packs {
		if s.memory <= workGroupPackMemoryBytes/2 {
			break
		}
		pack.mu.Lock()
		if !pack.done {
			pack.mu.Unlock()
			continue
		}
		freed := 0
		for _, chunk := range pack.chunks {
			freed += len(chunk.deflated)
			if pack.path != "" {
				chunk.deflated = nil
			}
		}
		onDisk := pack.path != ""
		pack.mu.Unlock()
		s.memory -= freed
		if !onDisk {
			delete(s.packs, pack.key)
		}
	}
}

type workGroupPackHeader struct {
	ChatID    string  `json:"chatId"`
	MessageID string  `json:"messageId"`
	Shape     string  `json:"shape"`
	Seq       int     `json:"seq"`
	CRC       uint32  `json:"crc"`
	Revision  uint64  `json:"revision"`
	Total     int     `json:"total"`
	Chunks    [][]int `json:"chunks"` // [from, count, size, zlen]
}

// write stores a built pack: magic, header length, header JSON, every
// chunk's call ends (uint32 each), then the chunks' segments in order.
func (s *workGroupPackStore) write(pack *workGroupPack) (string, error) {
	pack.mu.Lock()
	header := workGroupPackHeader{
		ChatID: pack.key.chatID, MessageID: pack.key.messageID, Shape: pack.key.shape,
		Seq: pack.seq, CRC: pack.crc, Revision: pack.revision, Total: pack.total,
	}
	chunks := append([]*workGroupChunk(nil), pack.chunks...)
	pack.mu.Unlock()
	var body bytes.Buffer
	for _, chunk := range chunks {
		header.Chunks = append(header.Chunks, []int{chunk.from, chunk.count, chunk.size, chunk.zlen})
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	body.WriteString(workGroupPackMagic)
	_ = binary.Write(&body, binary.LittleEndian, uint32(len(encoded)))
	body.Write(encoded)
	for _, chunk := range chunks {
		_ = binary.Write(&body, binary.LittleEndian, chunk.ends)
	}
	offset := int64(body.Len())
	offsets := make([]int64, len(chunks))
	for index, chunk := range chunks {
		offsets[index] = offset
		body.Write(chunk.deflated)
		offset += int64(len(chunk.deflated))
	}
	path := s.packPath(pack.key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, body.Bytes(), 0o600); err != nil {
		return "", err
	}
	pack.mu.Lock()
	for index, chunk := range chunks {
		chunk.fileOffset = offsets[index]
	}
	pack.mu.Unlock()
	s.trimDisk(path)
	return path, nil
}

// load reads a pack's index from disk; its chunks are read when a page needs
// them.
func (s *workGroupPackStore) load(key workGroupPackKey) (*workGroupPack, error) {
	path := s.packPath(key)
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lead [8]byte
	if _, err := io.ReadFull(file, lead[:]); err != nil {
		return nil, err
	}
	if string(lead[:4]) != workGroupPackMagic {
		return nil, errors.New("not a work group pack")
	}
	headerLen := binary.LittleEndian.Uint32(lead[4:])
	if headerLen > 64<<20 {
		return nil, errors.New("work group pack header too large")
	}
	encoded := make([]byte, headerLen)
	if _, err := io.ReadFull(file, encoded); err != nil {
		return nil, err
	}
	var header workGroupPackHeader
	if err := json.Unmarshal(encoded, &header); err != nil {
		return nil, err
	}
	if header.ChatID != key.chatID || header.MessageID != key.messageID || header.Shape != key.shape {
		return nil, errors.New("work group pack is another row's")
	}
	calls := 0
	for _, fields := range header.Chunks {
		if len(fields) != 4 || fields[1] <= 0 {
			return nil, errors.New("work group pack chunk is malformed")
		}
		calls += fields[1]
	}
	if calls != header.Total {
		return nil, errors.New("work group pack does not cover its row")
	}
	ends := make([]uint32, calls)
	if err := binary.Read(file, binary.LittleEndian, ends); err != nil {
		return nil, err
	}
	pack := newWorkGroupPack(key)
	pack.seq, pack.crc, pack.revision, pack.total, pack.done, pack.path = header.Seq, header.CRC, header.Revision, header.Total, true, path
	offset := int64(8) + int64(headerLen) + int64(4*calls)
	at, next := 0, 0
	for _, fields := range header.Chunks {
		if fields[0] != next {
			return nil, errors.New("work group pack chunks are not contiguous")
		}
		chunk := &workGroupChunk{from: fields[0], count: fields[1], size: fields[2], zlen: fields[3], fileOffset: offset, ends: ends[at : at+fields[1]]}
		pack.chunks = append(pack.chunks, chunk)
		offset += int64(fields[3])
		at += fields[1]
		next += fields[1]
	}
	return pack, nil
}

// trimDisk removes the least recently written packs past the disk budget,
// never the one just written.
func (s *workGroupPackStore) trimDisk(keep string) {
	type file struct {
		path string
		size int64
		mod  int64
	}
	var files []file
	var total int64
	_ = filepath.WalkDir(s.dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files = append(files, file{path: path, size: info.Size(), mod: info.ModTime().UnixNano()})
		total += info.Size()
		return nil
	})
	if total <= workGroupPackDiskBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod < files[j].mod })
	for _, f := range files {
		if total <= workGroupPackDiskBytes*3/4 {
			break
		}
		if f.path == keep {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

// writeFileAtomic writes a file whole or not at all.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	_, err = temp.Write(data)
	if err == nil {
		err = temp.Chmod(mode)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// drop forgets a pack that could not be read.
func (s *workGroupPackStore) drop(pack *workGroupPack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.packs[pack.key] == pack {
		s.memory -= pack.memoryBytes()
		delete(s.packs, pack.key)
	}
	pack.mu.Lock()
	path := pack.path
	pack.mu.Unlock()
	if path != "" {
		_ = os.Remove(path)
	}
}
