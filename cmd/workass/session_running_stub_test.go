package main

import (
	"reflect"
	"testing"
)

// A shell read with {runningTail: "stub"} keeps only a running chat's
// running and pending rows, with their identity and timing, no content.
func TestRunningMessageStubsKeepOnlyWhatAListNeeds(t *testing.T) {
	messages := []any{
		map[string]any{"id": "u1", "role": "user", "status": "done", "content": "hi"},
		map[string]any{"id": "a1", "role": "assistant", "status": "done", "content": "long reply", "events": []any{"tool"}},
		map[string]any{"id": "u2", "role": "user", "status": "pending", "content": "queued", "at": "2026-11-12T17:00:00Z"},
		map[string]any{"id": "a2", "role": "assistant", "status": "running", "jobId": "app-chat-1", "turnStartedAt": float64(1731430000000), "content": "so far", "events": []any{"big"}},
	}
	got := runningMessageStubs(messages)
	want := []any{
		map[string]any{"id": "u2", "role": "user", "status": "pending", "at": "2026-11-12T17:00:00Z", "content": ""},
		map[string]any{"id": "a2", "role": "assistant", "status": "running", "jobId": "app-chat-1", "turnStartedAt": float64(1731430000000), "content": ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stubs = %#v\nwant %#v", got, want)
	}
	if opts := archiveReadOptionsFrom(map[string]any{"shell": true, "runningTail": "stub"}); !opts.runningStub || !opts.sessionShell {
		t.Fatalf("options = %#v, want shell with running stubs", opts)
	}
	if opts := archiveReadOptionsFrom(map[string]any{"shell": true}); opts.runningStub {
		t.Fatal("a read that does not ask must keep the running tail")
	}
}
