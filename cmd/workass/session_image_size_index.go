package main

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
)

// The pixel size of a session image never changes (the file is named by its
// content digest), and rows written before attachments recorded their size
// make every history read look it up in the file's header. A fresh process
// had none cached: one first screen of 1,007 screenshots spent 9-14 ms
// opening files on every core before its first reply. Sizes read from
// headers are therefore also kept on disk, in image-sizes.idx in the state
// directory, and a process reads that index once instead of the headers.
//
// The index is a cache. Each record is 40 bytes: the 32-byte digest, then
// width and height (uint32, little endian). Records are only appended; a cut
// record at the end is ignored, the first record of a digest wins, and a
// missing, unreadable or foreign file is an empty index.

const (
	sessionImageSizeIndexFile   = "image-sizes.idx"
	sessionImageSizeIndexRecord = 40
	// sessionImageSizeIndexMax bounds what is read (and kept) of the index.
	sessionImageSizeIndexMax = 1 << 20
)

type sessionImageSizeIndex struct {
	mu      sync.Mutex
	loaded  map[string]bool // state dir -> index read
	sizes   map[[32]byte]sessionImageSize
	pending map[string][]byte // state dir -> records not yet appended
}

var sessionImageSizesOnDisk = &sessionImageSizeIndex{
	loaded: map[string]bool{}, sizes: map[[32]byte]sessionImageSize{}, pending: map[string][]byte{},
}

// lookup returns a size the state directory's index holds, reading the
// index the first time.
func (x *sessionImageSizeIndex) lookup(stateDir, name string) (sessionImageSize, bool) {
	digest, ok := sessionImageDigest(name)
	if !ok || stateDir == "" {
		return sessionImageSize{}, false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.loaded[stateDir] {
		x.loaded[stateDir] = true
		x.readLocked(stateDir)
	}
	size, found := x.sizes[digest]
	return size, found
}

// preload reads the state directory's index if no lookup has yet.
func (x *sessionImageSizeIndex) preload(stateDir string) {
	if stateDir == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.loaded[stateDir] {
		x.loaded[stateDir] = true
		x.readLocked(stateDir)
	}
}

func (x *sessionImageSizeIndex) readLocked(stateDir string) {
	data, err := os.ReadFile(filepath.Join(stateDir, sessionImageSizeIndexFile))
	if err != nil {
		return
	}
	for offset := 0; offset+sessionImageSizeIndexRecord <= len(data) && len(x.sizes) < sessionImageSizeIndexMax; offset += sessionImageSizeIndexRecord {
		var digest [32]byte
		copy(digest[:], data[offset:offset+32])
		if _, seen := x.sizes[digest]; seen {
			continue
		}
		x.sizes[digest] = sessionImageSize{
			width:  int(binary.LittleEndian.Uint32(data[offset+32:])),
			height: int(binary.LittleEndian.Uint32(data[offset+36:])),
		}
	}
}

// remember queues a size read from a header for the state directory's index.
// A size of zero (an unreadable header) is not kept: it may be repaired.
func (x *sessionImageSizeIndex) remember(stateDir, name string, size sessionImageSize) {
	digest, ok := sessionImageDigest(name)
	if !ok || stateDir == "" || size.width <= 0 || size.height <= 0 {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, known := x.sizes[digest]; known || len(x.sizes) >= sessionImageSizeIndexMax {
		return
	}
	x.sizes[digest] = size
	var record [sessionImageSizeIndexRecord]byte
	copy(record[:32], digest[:])
	binary.LittleEndian.PutUint32(record[32:], uint32(size.width))
	binary.LittleEndian.PutUint32(record[36:], uint32(size.height))
	x.pending[stateDir] = append(x.pending[stateDir], record[:]...)
}

// flush appends the queued records in one write.
func (x *sessionImageSizeIndex) flush(stateDir string) {
	x.mu.Lock()
	records := x.pending[stateDir]
	delete(x.pending, stateDir)
	x.mu.Unlock()
	if len(records) == 0 {
		return
	}
	file, err := os.OpenFile(filepath.Join(stateDir, sessionImageSizeIndexFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	// Records stay aligned after a cut one.
	if info, err := file.Stat(); err == nil && info.Size()%sessionImageSizeIndexRecord != 0 {
		if err := file.Truncate(info.Size() - info.Size()%sessionImageSizeIndexRecord); err != nil {
			return
		}
	}
	// A short write leaves a cut record, which a read ignores; the sizes stay
	// in memory either way.
	_, _ = file.Write(records)
}

func sessionImageDigest(name string) ([32]byte, bool) {
	var digest [32]byte
	if len(name) != 64 {
		return digest, false
	}
	if _, err := hex.Decode(digest[:], []byte(name)); err != nil {
		return digest, false
	}
	return digest, true
}
