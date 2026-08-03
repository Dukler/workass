package acp

import (
	"sync/atomic"
	"time"
)

// Streaming-text instrumentation. "Text arrives slowly" has four candidate
// owners — the agent producing tokens, the daemon buffering them, per-chunk work
// inside the daemon, and the wire handing frames to the renderer — and only
// timing each one separately can tell them apart. Every counter here is atomic
// so reading them can never stall a flush.
type streamStats struct {
	chunks             uint64
	chunkBytes         uint64
	gaps               uint64
	gapNanosTotal      uint64
	gapNanosMax        uint64
	flushes            uint64
	flushBytes         uint64
	flushAgeNanosTotal uint64
	flushAgeNanosMax   uint64
	markdownScans      uint64
	markdownScanBytes  uint64
	markdownNanos      uint64
	markdownNanosMax   uint64
	imageResolves      uint64
	imageResolveNanos  uint64
	imageResolveMax    uint64
}

func storeMaxUint64(target *uint64, candidate uint64) {
	for {
		previous := atomic.LoadUint64(target)
		if candidate <= previous || atomic.CompareAndSwapUint64(target, previous, candidate) {
			return
		}
	}
}

// recordStdoutChunk measures one bridge's agent pace. The prior timestamp lives
// on the bridge, so staggered output from unrelated agents cannot manufacture a
// cross-agent gap in the aggregate.
func (b *Bridge) recordStdoutChunk(size int) {
	now := time.Now().UnixNano()
	atomic.AddUint64(&b.manager.stream.chunks, 1)
	atomic.AddUint64(&b.manager.stream.chunkBytes, uint64(size))
	previous := atomic.SwapInt64(&b.lastChunkUnixNano, now)
	if previous > 0 && now > previous {
		gap := uint64(now - previous)
		// A pause between turns is not a streaming gap; ignore anything above a
		// few seconds so one idle period cannot dominate the maximum.
		if gap < uint64(5*time.Second) {
			atomic.AddUint64(&b.manager.stream.gaps, 1)
			atomic.AddUint64(&b.manager.stream.gapNanosTotal, gap)
			storeMaxUint64(&b.manager.stream.gapNanosMax, gap)
		}
	}
}

// recordStdoutFlush measures the daemon's own buffering: age is how long the
// oldest byte in the batch waited before the renderer could see it.
func (m *Manager) recordStdoutFlush(size int, age time.Duration) {
	atomic.AddUint64(&m.stream.flushes, 1)
	atomic.AddUint64(&m.stream.flushBytes, uint64(size))
	if age > 0 {
		atomic.AddUint64(&m.stream.flushAgeNanosTotal, uint64(age))
		storeMaxUint64(&m.stream.flushAgeNanosMax, uint64(age))
	}
}

// recordMarkdownScan measures per-chunk work that grows with the answer. Once a
// chunk contains "![", every later chunk rescans the whole accumulated output
// until the reference closes, so scanBytes climbing far above chunkBytes is the
// signature of quadratic work inside a single turn.
func (m *Manager) recordMarkdownScan(scanBytes int, elapsed time.Duration) {
	atomic.AddUint64(&m.stream.markdownScans, 1)
	atomic.AddUint64(&m.stream.markdownScanBytes, uint64(scanBytes))
	atomic.AddUint64(&m.stream.markdownNanos, uint64(elapsed))
	storeMaxUint64(&m.stream.markdownNanosMax, uint64(elapsed))
}

// recordImageResolve measures the filesystem half of that scan.
func (m *Manager) recordImageResolve(elapsed time.Duration) {
	atomic.AddUint64(&m.stream.imageResolves, 1)
	atomic.AddUint64(&m.stream.imageResolveNanos, uint64(elapsed))
	storeMaxUint64(&m.stream.imageResolveMax, uint64(elapsed))
}

func averageMs(totalNanos, count uint64) float64 {
	if count == 0 {
		return 0
	}
	return float64(totalNanos) / float64(count) / 1e6
}

// Stats reports streaming-text and engine counters for GET /workass/metrics.
func (m *Manager) Stats() map[string]any {
	chunks := atomic.LoadUint64(&m.stream.chunks)
	gaps := atomic.LoadUint64(&m.stream.gaps)
	flushes := atomic.LoadUint64(&m.stream.flushes)
	scans := atomic.LoadUint64(&m.stream.markdownScans)
	resolves := atomic.LoadUint64(&m.stream.imageResolves)

	chunkBytes := atomic.LoadUint64(&m.stream.chunkBytes)
	scanBytes := atomic.LoadUint64(&m.stream.markdownScanBytes)
	amplification := 0.0
	if chunkBytes > 0 {
		amplification = float64(scanBytes) / float64(chunkBytes)
	}
	coalesced := 0.0
	if flushes > 0 {
		coalesced = float64(chunks) / float64(flushes)
	}

	engines := []map[string]any{}
	m.mu.Lock()
	for key, bridge := range m.bridges {
		if bridge == nil {
			continue
		}
		bridge.mu.Lock()
		pid := 0
		if bridge.child != nil && bridge.child.Process != nil {
			pid = bridge.child.Process.Pid
		}
		engines = append(engines, map[string]any{
			"key":   key,
			"state": string(bridge.state),
			"rssKb": bridge.rssKb,
			"pid":   pid,
		})
		bridge.mu.Unlock()
	}
	m.mu.Unlock()

	return map[string]any{
		"engines": engines,
		"stream": map[string]any{
			"chunks":            chunks,
			"chunkBytes":        chunkBytes,
			"chunkGaps":         gaps,
			"chunkGapAvgMs":     averageMs(atomic.LoadUint64(&m.stream.gapNanosTotal), gaps),
			"chunkGapMaxMs":     float64(atomic.LoadUint64(&m.stream.gapNanosMax)) / 1e6,
			"flushes":           flushes,
			"flushBytes":        atomic.LoadUint64(&m.stream.flushBytes),
			"flushAgeAvgMs":     averageMs(atomic.LoadUint64(&m.stream.flushAgeNanosTotal), flushes),
			"flushAgeMaxMs":     float64(atomic.LoadUint64(&m.stream.flushAgeNanosMax)) / 1e6,
			"chunksPerFlush":    coalesced,
			"markdownScans":     scans,
			"markdownScanBytes": scanBytes,
			"markdownScanAmp":   amplification,
			"markdownAvgMs":     averageMs(atomic.LoadUint64(&m.stream.markdownNanos), scans),
			"markdownMaxMs":     float64(atomic.LoadUint64(&m.stream.markdownNanosMax)) / 1e6,
			"imageResolves":     resolves,
			"imageResolveAvgMs": averageMs(atomic.LoadUint64(&m.stream.imageResolveNanos), resolves),
			"imageResolveMaxMs": float64(atomic.LoadUint64(&m.stream.imageResolveMax)) / 1e6,
		},
	}
}
