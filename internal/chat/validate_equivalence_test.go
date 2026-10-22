package chat

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"workass/internal/provider"
)

// State.Validate answers provider activity owners from a one-pass ledger
// index instead of scanning the ledger for every owner. These tests run the
// indexed validators and the original nested-scan validators
// (validate_legacy_test.go) over many generated states and require identical
// answers: the same acceptance and, wherever the original is deterministic,
// the same error.

const equivalenceChatID = "equivalence-chat"
const equivalenceTabID = "equivalence-tab"

type equivalenceWorld struct {
	r     *rand.Rand
	lanes []provider.LaneIdentity
}

func newEquivalenceWorld(seed uint64) *equivalenceWorld {
	return &equivalenceWorld{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

func (w *equivalenceWorld) pick(values ...string) string { return values[w.r.IntN(len(values))] }

// Operation and native-turn ids repeat across rows and carry outer whitespace
// on some rows, because owners match after normalization.
func (w *equivalenceWorld) operationID() string {
	return w.pick("op-1", "op-2", "op-3", "op-4", " op-1", "op-2 ", "op-5")
}

func (w *equivalenceWorld) nativeTurnID() string {
	return w.pick("", "", "turn-1", " turn-1 ", "turn-2", "turn-3", "turn-4")
}

func (w *equivalenceWorld) laneID() provider.LaneID {
	return w.lanes[w.r.IntN(len(w.lanes))].ID
}

// state builds a chat with 1-3 lanes, a ledger whose rows belong to random
// lanes, operations and native turns (some excluded from context or
// inherited), and sometimes a running foreground turn. The state is valid
// except where corrupt asks for one structural fault outside the owners.
func (w *equivalenceWorld) state(corrupt bool) State {
	state, err := NewState(equivalenceChatID)
	if err != nil {
		panic(err)
	}
	state.Initialized = true
	state.Presentation.TabID = equivalenceTabID
	w.lanes = w.lanes[:0]
	for index := range 1 + w.r.IntN(3) {
		identity := testLane(equivalenceChatID, fmt.Sprintf("provider-%d", index))
		w.lanes = append(w.lanes, identity)
		state.Lanes[identity.ID] = LaneState{Identity: identity, Phase: LaneReady, ConnectionGeneration: uint64(1 + w.r.IntN(3))}
	}
	rows := w.r.IntN(48)
	for index := range rows {
		event := LedgerEvent{
			Sequence: uint64(index + 1), EventID: fmt.Sprintf("event-%d", index), MessageID: fmt.Sprintf("message-%d", index),
			OperationID: provider.OperationID(w.operationID()), Role: w.pick("user", "assistant"), NativeTurnID: w.nativeTurnID(),
		}
		switch roll := w.r.IntN(20); {
		case roll < 2:
			event.ContextExcluded = true
		case roll < 4:
			event.Inherited = true
		default:
			event.LaneID = w.laneID()
			event.ProviderID = "provider"
		}
		state.Ledger = append(state.Ledger, event)
	}
	if w.r.IntN(3) == 0 {
		laneID := w.laneID()
		lane := state.Lanes[laneID]
		lane.Phase = LaneRunning
		state.Lanes[laneID] = lane
		state.Foreground = &ForegroundTurn{
			OperationID: provider.OperationID(w.operationID()), LaneID: laneID, Status: ForegroundRunning,
			Input:                  QueueEntry{Presentation: provider.TurnPresentation{Origin: "human"}},
			RootAssistantMessageID: "assistant-root", CurrentAssistantMessageID: "assistant-current",
			Turn: provider.TurnRef{NativeID: w.nativeTurnID()},
		}
	}
	if corrupt && len(state.Ledger) > 0 {
		row := &state.Ledger[w.r.IntN(len(state.Ledger))]
		switch w.r.IntN(4) {
		case 0:
			row.ContextExcluded, row.LaneID = true, w.laneID()
		case 1:
			row.LaneID = "lane-missing"
		case 2:
			row.ProviderID = ""
		case 3:
			if state.Foreground != nil {
				lane := state.Lanes[state.Foreground.LaneID]
				lane.Phase = LaneReady
				state.Lanes[state.Foreground.LaneID] = lane
			} else {
				row.Sequence++
			}
		}
	}
	return state
}

// owner draws from every shape an owner field can take: real and unknown
// lanes, padded ids, empty fields, operations and native turns the ledger or
// foreground does or does not hold, and generations in and out of range.
func (w *equivalenceWorld) owner(state State) ProviderActivityOwner {
	owner := ProviderActivityOwner{}
	switch roll := w.r.IntN(12); {
	case roll < 8:
		owner.LaneID = w.laneID()
	case roll < 9:
		owner.LaneID = " " + w.laneID() + " "
	case roll < 10:
		owner.LaneID = ""
	default:
		owner.LaneID = "lane-missing"
	}
	owner.OperationID = provider.OperationID(w.pick("op-1", "op-2", "op-3", "op-4", "op-5", " op-3 ", "", "op-missing"))
	owner.TurnID = w.pick("", "", "turn-1", " turn-1", "turn-2", "turn-3", "turn-4", "turn-missing")
	generation := uint64(0)
	if lane, ok := state.Lanes[provider.LaneID(strings.TrimSpace(string(owner.LaneID)))]; ok {
		generation = lane.ConnectionGeneration
	}
	owner.ConnectionGeneration = []uint64{0, 1, generation, generation, generation + 1}[w.r.IntN(5)]
	if foreground := state.Foreground; foreground != nil && w.r.IntN(4) == 0 {
		owner.LaneID, owner.OperationID, owner.TurnID = foreground.LaneID, foreground.OperationID, foreground.Turn.NativeID
	}
	return owner
}

// validOwner is an owner the original validator accepts for state.
func (w *equivalenceWorld) validOwner(state State) (ProviderActivityOwner, bool) {
	for range 64 {
		if owner := w.owner(state); legacyValidateProviderActivityOwner(state, owner) == nil {
			return owner, true
		}
	}
	return ProviderActivityOwner{}, false
}

func (w *equivalenceWorld) backgroundAction(state State, owner ProviderActivityOwner, index int) BackgroundAction {
	action := BackgroundAction{
		OperationID: provider.OperationID(fmt.Sprintf("background-op-%d", index)),
		Owner:       owner, ChatID: equivalenceChatID, TabID: equivalenceTabID,
	}
	workIDs := make([]string, 0, len(state.Background))
	for id := range state.Background {
		workIDs = append(workIDs, id)
	}
	sort.Strings(workIDs)
	workID := "work-missing"
	if len(workIDs) > 0 && w.r.IntN(4) != 0 {
		workID = workIDs[w.r.IntN(len(workIDs))]
	}
	switch w.r.IntN(4) {
	case 0:
		action.Kind, action.Spawn = BackgroundSpawnAgent, &SpawnAgentAction{Prompt: "work"}
	case 1:
		action.Kind, action.Register = BackgroundRegisterExternal, &RegisterExternalAction{Label: "lane"}
	case 2:
		action.Kind, action.Message = BackgroundMessageAgent, &MessageAgentAction{WorkID: workID, Message: "more"}
	default:
		action.Kind, action.Stop = BackgroundStopWork, &StopWorkAction{WorkID: workID}
	}
	return action
}

// activitySlot is one place State.Validate checks an owner.
type activitySlot int

const (
	slotTool activitySlot = iota
	slotPlan
	slotPermission
	slotBackground
	slotCompaction
	slotOutboxAction
	slotCount
)

// place puts owner into slot; an outbox action is built here unless the
// caller supplies an already-checked one.
func (w *equivalenceWorld) place(state *State, slot activitySlot, owner ProviderActivityOwner, index int, action *BackgroundAction) {
	switch slot {
	case slotTool:
		id := fmt.Sprintf("tool-%d", index)
		if w.r.IntN(40) == 0 {
			id = " "
		}
		state.Tools[id] = ToolState{Owner: owner}
	case slotPlan:
		key := owner.OperationID
		if w.r.IntN(10) == 0 {
			key = "op-other"
		}
		state.Plans[key] = PlanState{Owner: owner}
	case slotPermission:
		state.Permissions[fmt.Sprintf("permission-%d", index)] = PermissionState{Owner: owner}
	case slotBackground:
		state.Background[fmt.Sprintf("work-%d", index)] = BackgroundState{Owner: owner}
	case slotCompaction:
		key := owner.LaneID
		if w.r.IntN(10) == 0 {
			key = w.laneID()
		}
		state.Compactions[key] = CompactionState{Owner: owner}
	case slotOutboxAction:
		if action == nil {
			built := w.backgroundAction(*state, owner, index)
			action = &built
		}
		state.Outbox = append(state.Outbox, OutboxEntry{
			ID: fmt.Sprintf("background-effect-%d", index), Kind: EffectBackground, Status: OutboxPending,
			LaneID: w.laneID(), OperationID: action.OperationID, Background: action,
		})
	}
}

func errorText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// Every owner check alone: the index and the original scan agree on match,
// on validation and on the error text, for every generated owner shape.
func TestProviderActivityOwnerIndexMatchesLedgerScan(t *testing.T) {
	outcomes := map[string]int{}
	for seed := range uint64(400) {
		w := newEquivalenceWorld(seed)
		state := w.state(false)
		index := newProviderActivityOwnerIndex(state)
		for range 200 {
			owner := w.owner(state)
			legacyMatch := legacyProviderActivityOwnerMatches(state, owner)
			if got := index.matches(owner); got != legacyMatch {
				t.Fatalf("seed %d owner %+v: index matches=%v, ledger scan matches=%v", seed, owner, got, legacyMatch)
			}
			want := errorText(legacyValidateProviderActivityOwner(state, owner))
			if got := errorText(index.validate(owner)); got != want {
				t.Fatalf("seed %d owner %+v: index validate %q, ledger scan %q", seed, owner, got, want)
			}
			if got := errorText(validateProviderActivityOwner(state, owner)); got != want {
				t.Fatalf("seed %d owner %+v: validateProviderActivityOwner %q, ledger scan %q", seed, owner, got, want)
			}
			path := "ledger"
			if foreground := state.Foreground; foreground != nil && foreground.LaneID == provider.LaneID(strings.TrimSpace(string(owner.LaneID))) &&
				provider.NormalizeOperationID(string(foreground.OperationID)) == provider.NormalizeOperationID(string(owner.OperationID)) {
				path = "foreground"
			}
			turn := "turn"
			if strings.TrimSpace(owner.TurnID) == "" {
				turn = "no-turn"
			}
			outcomes[fmt.Sprintf("%s/%s/%v", path, turn, legacyMatch)]++
			outcomes[want]++
		}
	}
	// The generator must reach every outcome the original can produce.
	for _, required := range []string{
		"ok",
		"provider activity is missing lane ownership",
		"provider activity belongs to an unknown lane",
		"provider activity has an invalid connection generation",
		"provider activity is missing operation ownership",
		"provider activity does not match an exact current or historical actor turn",
		"ledger/turn/true", "ledger/turn/false", "ledger/no-turn/true", "ledger/no-turn/false",
		"foreground/turn/true", "foreground/turn/false", "foreground/no-turn/false",
	} {
		if outcomes[required] == 0 {
			t.Errorf("generator never produced %q (outcomes %v)", required, outcomes)
		}
	}
}

// The whole validator over generated states: one arbitrary owner placed among
// owners the original accepts, so the original's error is deterministic and
// the text must match exactly; plus states with many arbitrary owners, where
// map order makes the original's choice of error random, so acceptance must
// match.
func TestStateValidateMatchesLegacyValidator(t *testing.T) {
	messages := map[string]int{}
	accepted, rejected := 0, 0
	for seed := range uint64(3000) {
		w := newEquivalenceWorld(seed)
		corrupt := seed%10 == 9
		state := w.state(corrupt)
		single := seed%3 != 0
		entries := 1 + w.r.IntN(24)
		odd := w.r.IntN(entries)
		oddSlot := activitySlot(w.r.IntN(int(slotCount)))
		for index := range entries {
			slot := activitySlot(w.r.IntN(int(slotCount)))
			owner := w.owner(state)
			var action *BackgroundAction
			if single {
				if index == odd {
					slot = oddSlot
					// Half of the odd owners are themselves accepted ones, so the
					// whole state is accepted as often as it is rejected.
					if w.r.IntN(2) == 0 {
						if valid, ok := w.validOwner(state); ok {
							owner = valid
						}
					}
				} else {
					valid, ok := w.validOwner(state)
					if !ok {
						continue
					}
					owner = valid
					// Accepted owners only; an action may still fail on its target,
					// so keep only the actions the original accepts.
					if slot == slotOutboxAction {
						built := w.backgroundAction(state, owner, index)
						if legacyBackgroundActionValidate(built, state) != nil {
							slot = slotTool
						} else {
							action = &built
						}
					}
					if slot == slotPlan || slot == slotCompaction || slot == slotTool {
						slot = slotPermission
					}
				}
			}
			w.place(&state, slot, owner, index, action)
		}
		want := errorText(legacyValidate(state))
		got := errorText(state.Validate())
		if single || want == "ok" {
			if got != want {
				t.Fatalf("seed %d: Validate %q, legacy %q", seed, got, want)
			}
		} else if (got == "ok") != (want == "ok") {
			t.Fatalf("seed %d: Validate %q, legacy %q", seed, got, want)
		}
		if want == "ok" {
			accepted++
		} else {
			rejected++
		}
		messages[want]++
	}
	if testing.Verbose() {
		t.Logf("%d accepted, %d rejected: %v", accepted, rejected, messages)
	}
	if accepted < 300 || rejected < 300 {
		t.Fatalf("generator is lopsided: %d accepted, %d rejected", accepted, rejected)
	}
	for _, required := range []string{
		"tool activity is missing its provider id",
		"provider activity does not match an exact current or historical actor turn",
		"provider activity has an invalid connection generation",
		"plan activity operation ownership mismatch",
		"compaction activity lane ownership mismatch",
		"background outbox effect: background action owner: provider activity does not match an exact current or historical actor turn",
		"background outbox effect: background work \"work-missing\" is not actor-owned",
		"context-excluded or inherited ledger event claims child lane ownership",
		"ledger event belongs to an unknown lane",
	} {
		if messages[required] == 0 {
			t.Errorf("generator never produced %q", required)
		}
	}
}

// canonicalBackgroundSnapshot validates each reported item's owner; it uses
// the same index and must accept and reject exactly what it did.
func TestCanonicalBackgroundSnapshotMatchesLegacy(t *testing.T) {
	for seed := range uint64(400) {
		w := newEquivalenceWorld(seed)
		state := w.state(false)
		items := make([]BackgroundState, 1+w.r.IntN(3))
		for index := range items {
			items[index] = BackgroundState{Owner: w.owner(state), Event: provider.BackgroundEvent{WorkID: fmt.Sprintf("work-%d", index)}}
		}
		_, wantErr := legacyCanonicalBackgroundSnapshot(state, items)
		_, gotErr := canonicalBackgroundSnapshot(state, items)
		if errorText(gotErr) != errorText(wantErr) {
			t.Fatalf("seed %d: snapshot %q, legacy %q", seed, errorText(gotErr), errorText(wantErr))
		}
	}
}

// longChatState is shaped like the longest real chat: 1,434 rows on one lane,
// 85 tool calls per turn (~61k tool owners), and background work with outbox
// actions that name historical turns.
func longChatState(tb testing.TB) State {
	tb.Helper()
	state, err := NewState(equivalenceChatID)
	if err != nil {
		tb.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = equivalenceTabID
	identity := testLane(equivalenceChatID, "claude")
	state.Lanes[identity.ID] = LaneState{Identity: identity, Phase: LaneReady, ConnectionGeneration: 4}
	const rows, toolsPerTurn = 1434, 85
	for index := range rows {
		turn := index / 2
		operationID := provider.OperationID(fmt.Sprintf("operation-%d", turn))
		nativeTurnID := fmt.Sprintf("native-turn-%d", turn)
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		state.Ledger = append(state.Ledger, LedgerEvent{
			Sequence: uint64(index + 1), EventID: fmt.Sprintf("event-%d", index), MessageID: fmt.Sprintf("message-%d", index),
			OperationID: operationID, Role: role, NativeTurnID: nativeTurnID, LaneID: identity.ID, ProviderID: "claude",
		})
		if role != "assistant" {
			continue
		}
		owner := ProviderActivityOwner{LaneID: identity.ID, OperationID: operationID, TurnID: nativeTurnID, ConnectionGeneration: 4}
		for tool := range toolsPerTurn {
			state.Tools[fmt.Sprintf("toolu-%d-%d", turn, tool)] = ToolState{Owner: owner}
		}
		if turn%4 == 0 {
			workID := fmt.Sprintf("work-%d", turn)
			state.Background[workID] = BackgroundState{Owner: owner}
			action := BackgroundAction{
				Kind: BackgroundStopWork, OperationID: provider.OperationID("stop-" + workID), Owner: owner,
				ChatID: equivalenceChatID, TabID: equivalenceTabID, Stop: &StopWorkAction{WorkID: workID},
			}
			state.Outbox = append(state.Outbox, OutboxEntry{
				ID: "effect-" + workID, Kind: EffectBackground, Status: OutboxCompleted,
				LaneID: identity.ID, OperationID: action.OperationID, Background: &action,
			})
		}
	}
	if err := state.Validate(); err != nil {
		tb.Fatal(err)
	}
	return state
}

func BenchmarkStateValidateLongChat(b *testing.B) {
	state := longChatState(b)
	b.Run("indexed", func(b *testing.B) {
		for range b.N {
			if err := state.Validate(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("legacy-ledger-scan", func(b *testing.B) {
		for range b.N {
			if err := legacyValidate(state); err != nil {
				b.Fatal(err)
			}
		}
	})
}
