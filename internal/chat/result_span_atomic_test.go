package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// Consecutive final-answer chunks extend one result span. The streamed-event
// transition shares the committed timeline, so extending the span in place
// changed the committed state even when the commit then failed: the span
// claimed text the chat never committed. A failed commit must leave the
// committed state byte for byte as it was, and a successful one must equal
// the pure reducer's.
func TestFailedCommitOfAFinalChunkLeavesTheResultSpanAlone(t *testing.T) {
	base, lane := largeJournalReadyState(t)
	store := &failingProviderEventStore{}
	engine := &Engine{state: base, store: store}
	if err := engine.Apply(journalAssistantEvent(lane, 1, "first part, ")); err != nil {
		t.Fatal(err)
	}
	committed := engine.state
	span := committed.Foreground.Timeline[len(committed.Foreground.Timeline)-1].Result
	if span == nil || span.End == 0 {
		t.Fatalf("the first final chunk opened no result span: %#v", committed.Foreground.Timeline)
	}
	before, err := json.Marshal(committed)
	if err != nil {
		t.Fatal(err)
	}

	second := journalAssistantEvent(lane, 2, "second part")
	expected, _, err := Reduce(committed.Clone(), second)
	if err != nil {
		t.Fatalf("reference: %v", err)
	}
	store.err = errors.New("injected commit failure")
	if err := engine.Apply(second); !errors.Is(err, store.err) {
		t.Fatalf("commit failure = %v", err)
	}
	after, err := json.Marshal(engine.state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed commit of a final chunk changed the committed state")
	}

	store.err = nil
	if err := engine.Apply(second); err != nil {
		t.Fatal(err)
	}
	if got := engine.state.Foreground.Timeline; !reflect.DeepEqual(got, expected.Foreground.Timeline) {
		t.Fatalf("the committed timeline diverged from the reducer: %#v", got)
	}
	if engine.state.Foreground.AssistantResult != "first part, second part" {
		t.Fatalf("result = %q", engine.state.Foreground.AssistantResult)
	}
}
