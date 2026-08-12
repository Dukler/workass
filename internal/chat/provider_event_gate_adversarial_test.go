package chat

import (
	"math/rand"
	"testing"

	"workass/internal/provider"
)

func TestPhaseCProviderEventIngressRejectsDuplicateGapAndStaleWithoutStateMutation(t *testing.T) {
	const (
		chatID     = "phase-c-ingest-chat"
		generation = uint64(7)
	)
	lane := testLane(chatID, "codex")
	state, err := NewState(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID:               lane.ID,
		Thread:               provider.ThreadRef{ProviderID: lane.Realm.ProviderID, RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: generation,
		Context:              exactContext(provider.ContextImportUnsupported),
	})

	state = phaseCApplyUsage(t, state, lane, generation, 1)
	baseline := state.Clone()
	invalid := []struct {
		name       string
		sequence   uint64
		connection uint64
	}{
		{name: "duplicate", sequence: 1, connection: generation},
		{name: "gap", sequence: 3, connection: generation},
		{name: "stale-generation", sequence: 2, connection: generation + 1},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			next, _, err := Reduce(state, ProviderEventReceived{
				ConnectionGeneration: test.connection,
				Event:                phaseCUsageEvent(chatID, lane, test.sequence, test.sequence),
			})
			if err == nil {
				t.Fatalf("invalid provider event sequence=%d generation=%d was accepted", test.sequence, test.connection)
			}
			if next.Revision != baseline.Revision ||
				next.Lanes[lane.ID].LastEventSequence != baseline.Lanes[lane.ID].LastEventSequence ||
				next.Usage[lane.ID] != baseline.Usage[lane.ID] {
				t.Fatalf("rejected event mutated durable state: revision=%d sequence=%d usage=%#v", next.Revision, next.Lanes[lane.ID].LastEventSequence, next.Usage[lane.ID])
			}
		})
	}

	// A rejected event must not consume the sequence number. This randomized
	// walk alternates valid commits with duplicate, gap, and stale-generation
	// attempts, which makes the invariant independent of one fixed fixture.
	random := rand.New(rand.NewSource(0xC0DEC))
	for step := 0; step < 128; step++ {
		expected := state.Lanes[lane.ID].LastEventSequence + 1
		kind := random.Intn(3)
		badSequence, badGeneration := expected, generation
		switch kind {
		case 0: // duplicate
			badSequence = expected - 1
		case 1: // gap
			badSequence = expected + 1
		case 2: // stale generation
			badGeneration = generation + 1
		}
		before := state.Clone()
		next, _, err := Reduce(state, ProviderEventReceived{
			ConnectionGeneration: badGeneration,
			Event:                phaseCUsageEvent(chatID, lane, badSequence, uint64(step+1000)),
		})
		if err == nil {
			t.Fatalf("random invalid event kind=%d sequence=%d generation=%d was accepted", kind, badSequence, badGeneration)
		}
		if next.Lanes[lane.ID].LastEventSequence != before.Lanes[lane.ID].LastEventSequence ||
			next.Usage[lane.ID] != before.Usage[lane.ID] {
			t.Fatalf("random rejected event advanced state: before=%d after=%d", before.Lanes[lane.ID].LastEventSequence, next.Lanes[lane.ID].LastEventSequence)
		}

		state = phaseCApplyUsage(t, state, lane, generation, expected)
	}
	if got := state.Lanes[lane.ID].LastEventSequence; got != 129 {
		t.Fatalf("contiguous event walk ended at sequence %d, want 129", got)
	}
}

func phaseCApplyUsage(t *testing.T, state State, lane provider.LaneIdentity, generation, sequence uint64) State {
	t.Helper()
	next, _, err := Reduce(state, ProviderEventReceived{
		ConnectionGeneration: generation,
		Event:                phaseCUsageEvent(state.ChatID, lane, sequence, sequence),
	})
	if err != nil {
		t.Fatalf("valid provider event sequence=%d rejected: %v", sequence, err)
	}
	return next
}

func phaseCUsageEvent(chatID string, lane provider.LaneIdentity, sequence, used uint64) provider.Event {
	return provider.Event{
		Kind: provider.EventUsageUpdated,
		Identity: provider.EventIdentity{
			ChatID: chatID, LaneID: lane.ID, Sequence: sequence,
		},
		Usage: &provider.UsageEvent{Used: int(used), Size: 1000},
	}
}
