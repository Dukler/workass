package acp

import (
	"sync/atomic"
	"time"
)

type startupStage uint8

const (
	startupWorker startupStage = iota
	startupControls
	startupPrepared
	startupWritten
	startupUpdate
	startupContent
	startupTool
	startupStageCount
)

// One bounded, content-free receipt per completed turn. Measuring at the wire
// boundary distinguishes host preparation from provider silence and from a
// provider already thinking before its first tool. No per-chunk log writes.
// Offsets start at Manager.StartJob, not at the controller's Send click.
type turnStartupTiming struct {
	started time.Time
	stages  [startupStageCount]atomic.Int64
}

func (s *turnStartupTiming) mark(stage startupStage) {
	if s == nil || s.stages[stage].Load() != 0 {
		return
	}
	s.stages[stage].CompareAndSwap(0, int64(time.Since(s.started))+1)
}

func (s *turnStartupTiming) fields() map[string]any {
	if s == nil {
		return nil
	}
	fields := map[string]any{"managerElapsedMs": time.Since(s.started).Milliseconds()}
	names := [...]string{"workerStartedMs", "controlsFinishedMs", "promptPreparedMs", "promptWrittenMs", "firstUpdateMs", "firstContentMs", "firstToolMs"}
	for stage, name := range names {
		if offset := s.stages[stage].Load(); offset != 0 {
			fields[name] = time.Duration(offset - 1).Milliseconds()
		}
	}
	return fields
}
