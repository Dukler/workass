package acp

import (
	"testing"
	"time"
)

// Every streamed chunk says where it starts in the turn's output, in bytes,
// so a client that missed one (a phone whose socket died while locked) can
// tell and re-read the turn instead of showing a reply with a hole in it.
func TestStdoutChunksCarryTheirOffsetInTheTurnOutput(t *testing.T) {
	t.Parallel()
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast, StdoutFlushInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	bridge := newBridge("offset-test", Options{StdoutFlushInterval: time.Hour}, manager)
	job := &Job{ID: "offset-job"}

	bridge.queueStdout(job, "año uno", "")
	bridge.queueStdout(job, " y dos", "")
	bridge.flushJobBuffers(job)
	bridge.queueStdout(job, "tres", "")
	bridge.flushJobBuffers(job)

	data := events.jobEvents(job.ID, "data")
	if len(data) != 2 {
		t.Fatalf("data events = %d, want 2: %#v", len(data), data)
	}
	first, second := data[0], data[1]
	if first["chunk"] != "año uno y dos" || first["offset"] != 0 {
		t.Fatalf("first chunk = %#v", first)
	}
	// Bytes, not characters: "ñ" is two.
	if want := len("año uno y dos"); second["chunk"] != "tres" || second["offset"] != want {
		t.Fatalf("second chunk = %#v, want offset %d", second, want)
	}
	if got := job.output.String(); got != "año uno y dostres" {
		t.Fatalf("output = %q", got)
	}
}
