package acp

import (
	"testing"
	"time"
)

func TestStreamStatsSeparateAgentPaceFromDaemonWork(t *testing.T) {
	m := &Manager{}
	bridge := newBridge("metrics-pace", Options{StdoutFlushInterval: time.Hour}, m)

	// Two chunks 40ms apart: the gap is the agent's pace, not Workass's.
	bridge.recordStdoutChunk(10)
	bridge.lastChunkUnixNano = time.Now().Add(-40 * time.Millisecond).UnixNano()
	bridge.recordStdoutChunk(30)
	m.recordStdoutFlush(40, 16*time.Millisecond)
	// One markdown rescan of a 4000-byte answer triggered by 40 bytes of input:
	// the amplification is the quadratic-work signature.
	m.recordMarkdownScan(4000, 3*time.Millisecond)
	m.recordImageResolve(7 * time.Millisecond)

	stats, ok := m.Stats()["stream"].(map[string]any)
	if !ok {
		t.Fatalf("stream stats missing: %v", m.Stats())
	}
	if stats["chunks"] != uint64(2) || stats["chunkBytes"] != uint64(40) {
		t.Fatalf("chunk counters wrong: %v", stats)
	}
	if stats["chunkGaps"] != uint64(1) {
		t.Fatalf("accepted gap count = %v, want 1", stats["chunkGaps"])
	}
	if gap, _ := stats["chunkGapMaxMs"].(float64); gap < 30 || gap > 200 {
		t.Fatalf("chunk gap max = %v, want roughly 40ms", stats["chunkGapMaxMs"])
	}
	if gap, _ := stats["chunkGapAvgMs"].(float64); gap < 30 || gap > 200 {
		t.Fatalf("chunk gap average = %v, want roughly 40ms", stats["chunkGapAvgMs"])
	}
	if flushes := stats["flushes"]; flushes != uint64(1) {
		t.Fatalf("flushes = %v, want 1", flushes)
	}
	if age, _ := stats["flushAgeAvgMs"].(float64); age < 15 || age > 17 {
		t.Fatalf("flush age = %v, want 16ms", stats["flushAgeAvgMs"])
	}
	if amp, _ := stats["markdownScanAmp"].(float64); amp != 100 {
		t.Fatalf("markdown amplification = %v, want 4000/40 = 100", stats["markdownScanAmp"])
	}
	if resolve, _ := stats["imageResolveMaxMs"].(float64); resolve < 6 || resolve > 8 {
		t.Fatalf("image resolve max = %v, want 7ms", stats["imageResolveMaxMs"])
	}
	if _, ok := m.Stats()["engines"]; !ok {
		t.Fatalf("engine inventory missing: %v", m.Stats())
	}
}

func TestStreamStatsDoNotMixIndependentBridgeGaps(t *testing.T) {
	m := &Manager{}
	first := newBridge("metrics-first", Options{StdoutFlushInterval: time.Hour}, m)
	second := newBridge("metrics-second", Options{StdoutFlushInterval: time.Hour}, m)
	first.queueStdout(&Job{internal: true}, "a", "")
	time.Sleep(25 * time.Millisecond)
	second.queueStdout(&Job{internal: true}, "b", "")

	stats := m.Stats()["stream"].(map[string]any)
	if gap, _ := stats["chunkGapMaxMs"].(float64); gap != 0 {
		t.Fatalf("first chunks from independent bridges produced a cross-agent gap: %v", gap)
	}
	if gap, _ := stats["chunkGapAvgMs"].(float64); gap != 0 {
		t.Fatalf("independent bridge gap polluted the average: %v", gap)
	}
}

func TestStreamStatsIgnoreIdleGapsBetweenTurns(t *testing.T) {
	m := &Manager{}
	bridge := newBridge("metrics-idle", Options{StdoutFlushInterval: time.Hour}, m)
	bridge.recordStdoutChunk(5)
	// A long pause between turns is not a streaming stall and must not become
	// the reported maximum.
	bridge.lastChunkUnixNano = time.Now().Add(-30 * time.Second).UnixNano()
	bridge.recordStdoutChunk(5)
	stats := m.Stats()["stream"].(map[string]any)
	if gap, _ := stats["chunkGapMaxMs"].(float64); gap != 0 {
		t.Fatalf("idle gap leaked into streaming metrics: %v", gap)
	}
}
