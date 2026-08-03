package main

import (
	"os"
	"testing"
)

// The steady-state read is the one that matters: the daemon serves session:get
// constantly, and on an image-heavy session the buffer is megabytes. This
// benchmark exists so the capacity estimate can be changed with evidence rather
// than argument — B/op should stay one result-sized allocation, and allocs/op
// must not climb, or the buffer is being grown and copied mid-encode.
//
// Point it at an isolated copy of a real session-state.json with its images
// directory beside it; a synthetic fixture cannot show the cost being defended.
func BenchmarkSessionRawSteadyStateRead(b *testing.B) {
	path := os.Getenv("WORKASS_COST_SNAPSHOT")
	if path == "" {
		b.Skip("set WORKASS_COST_SNAPSHOT to an isolated real session-state.json copy")
	}
	store := newSessionStore(path)
	if err := store.LoadError(); err != nil {
		b.Fatalf("load real fixture: %v", err)
	}
	// Warm wireByteEstimate so the benchmark measures the repeated read rather
	// than the cold one.
	warm := store.GetRawWithLiveSessions(nil)
	if len(warm) == 0 {
		b.Fatal("real fixture produced no wire result")
	}
	b.Logf("resultBytes=%d", len(warm))
	b.SetBytes(int64(len(warm)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		raw := store.GetRawWithLiveSessions(nil)
		if len(raw) == 0 {
			b.Fatal("wire result went empty under repeated reads")
		}
	}
}
