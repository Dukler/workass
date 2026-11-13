package main

import (
	"bytes"
	"context"
	"testing"
)

// workass_read_chat with a limit reads only the rows it returns, from a
// closed chat's rows without opening it, and answers exactly what projecting
// the whole chat did.
func TestReadChatReadsOnlyTheRowsItReturns(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	workGroupTestChat(t, stateDir, backend, "rc")
	runtime := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	defer func() { _ = runtime.Close(context.Background()) }()
	defer func() { readChatProjectsWhole = false }()
	for _, limit := range []int{1, 3, 9, 50} {
		for _, events := range []bool{false, true} {
			closed, err := runtime.ReadChat("tab-rc", "rc", limit, events)
			if err != nil {
				t.Fatal(err)
			}
			if open, _ := runtime.lazyActorCounts(); open != 0 {
				t.Fatal("a bounded read opened the chat")
			}
			readChatProjectsWhole = true
			whole, err := runtime.ReadChat("tab-rc", "rc", limit, events)
			readChatProjectsWhole = false
			if err != nil {
				t.Fatal(err)
			}
			open, err := runtime.ReadChat("tab-rc", "rc", limit, events)
			if err != nil {
				t.Fatal(err)
			}
			want := mustJSONBytes(t, whole)
			if !bytes.Equal(mustJSONBytes(t, closed), want) || !bytes.Equal(mustJSONBytes(t, open), want) {
				t.Fatalf("limit %d events %v: bounded reads differ from the whole projection\n%s\n%s", limit, events, mustJSONBytes(t, closed), want)
			}
			// Close it again for the next round.
			if err := runtime.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			runtime = startLazyTestRuntime(t, stateDir, backend, "tab-none")
		}
	}
	if _, err := runtime.ReadChat("tab-other", "rc", 3, false); err == nil {
		t.Fatal("a closed chat was read through another tab")
	}
}
