package chat

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"workass/internal/provider"
)

// An open actor's states share one ledger array until a reducer copies it. An
// old row edited in place through that array (a field, a timeline entry, an
// object behind a pointer) must still be written by the next checkpoint and
// survive a reload; a checkpoint with nothing changed writes only the head.
func TestSQLCheckpointSavesAnOldRowEditedInPlace(t *testing.T) {
	const chatID = "in-place-chat"
	actors := openTestActors(t)
	if err := actors.Store(chatID).Save(richLedgerState(t, chatID, 200, 0, 0)); err != nil {
		t.Fatal(err)
	}
	// The handle of an open actor: its state comes from Load.
	sqlStore := actors.Store(chatID)
	state, found, err := sqlStore.Load(chatID)
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	shared := unsafe.SliceData(state.Ledger)

	commit := func(label string, next State, checkpoint func(State) error, wantChanges int64) {
		t.Helper()
		if unsafe.SliceData(next.Ledger) != shared {
			t.Fatalf("%s: the ledger no longer shares the loaded array", label)
		}
		before := totalChanges(t, actors.db)
		if err := checkpoint(next); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if changes := totalChanges(t, actors.db) - before; changes != wantChanges {
			t.Fatalf("%s wrote %d rows, want %d", label, changes, wantChanges)
		}
		assertRowsMatch(t, actors, chatID, next.Ledger)
		reloaded, found, err := actors.Store(chatID).Load(chatID)
		if err != nil || !found || reloaded.Revision != next.Revision || !reflect.DeepEqual(reloaded.Ledger, next.Ledger) {
			t.Fatalf("%s: the reload lost the edit: found=%v err=%v", label, found, err)
		}
	}

	// Rows at even indexes are assistant rows with a timeline: entries 0, 4,
	// 8 and 12 are thinking, the rest tools.
	edits := []struct {
		name string
		edit func(ledger []LedgerEvent)
	}{
		{"row field", func(ledger []LedgerEvent) { ledger[2].Text = "edited in place" }},
		{"timeline entry", func(ledger []LedgerEvent) { ledger[4].Timeline[3].Key = "renamed-in-place" }},
		{"tool behind a pointer", func(ledger []LedgerEvent) { ledger[6].Timeline[1].Tool.Status = "failed" }},
		{"thinking behind a pointer", func(ledger []LedgerEvent) { ledger[8].Timeline[4].Thinking.Text = "rethought" }},
		{"tool output grown", func(ledger []LedgerEvent) { ledger[10].Timeline[2].Tool.Output += " and more" }},
	}
	checkpoints := []struct {
		name string
		run  func(State) error
	}{{"save", sqlStore.Save}, {"journal checkpoint", sqlStore.journalCheckpoint}}
	for index, edit := range edits {
		checkpoint := checkpoints[index%len(checkpoints)]
		next := state
		next.Revision++
		edit.edit(next.Ledger)
		commit(edit.name+", then "+checkpoint.name, next, checkpoint.run, 2)
		state = next
	}
	for _, checkpoint := range checkpoints {
		next := state
		next.Revision++
		commit("unchanged "+checkpoint.name, next, checkpoint.run, 1)
		state = next
	}
}

// appendRichHistory appends rows shaped like a long real chat: user and
// assistant rows alternate, every assistant row owns a timeline of thinking
// and tool entries, and every bigEvery-th assistant row carries a tool output
// of bigBytes (each its own allocation, as decoded rows are).
func appendRichHistory(ledger []LedgerEvent, rows, bigEvery, bigBytes int) []LedgerEvent {
	input := strings.Repeat("i", 2_000)
	for added := 0; added < rows; added++ {
		sequence := len(ledger) + 1
		row := historyRow(sequence)
		if sequence%2 == 0 {
			row.Role = "user"
			ledger = append(ledger, row)
			continue
		}
		row.Text = fmt.Sprintf("assistant reply %d %s", sequence, strings.Repeat("r", 1_000))
		for entry := 0; entry < 16; entry++ {
			key := fmt.Sprintf("row-%d-entry-%d", sequence, entry)
			if entry%4 == 0 {
				row.Timeline = append(row.Timeline, TimelineEntry{Key: key, Kind: provider.EventThinkingUpdate,
					Thinking: &provider.ThinkingEvent{Text: fmt.Sprintf("thinking %d %s", entry, strings.Repeat("t", 500))}})
				continue
			}
			output := fmt.Sprintf("output %d %s", entry, strings.Repeat("o", 4_000))
			if entry == 1 && bigEvery > 0 && (sequence/2)%bigEvery == 0 {
				output = strings.Repeat("b", bigBytes) + output
			}
			row.Timeline = append(row.Timeline, TimelineEntry{Key: key, Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{
				ToolCallID: key, ToolKind: "execute", Title: "Run a command", Status: "completed", Command: "go test ./...",
				Input: input, Output: output, StartedAtUnixMS: int64(sequence), EndedAtUnixMS: int64(sequence + 1),
			}})
		}
		ledger = append(ledger, row)
	}
	return ledger
}

func richLedgerState(t testing.TB, chatID string, rows, bigEvery, bigBytes int) State {
	t.Helper()
	state, err := NewState(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state.Ledger = appendRichHistory(state.Ledger, rows, bigEvery, bigBytes)
	state.Revision = 1
	if err := state.Validate(); err != nil {
		t.Fatal(err)
	}
	return state
}

func ledgerTextBytes(ledger []LedgerEvent) int {
	total := 0
	for _, row := range ledger {
		total += len(row.Text)
		for _, entry := range row.Timeline {
			if entry.Tool != nil {
				total += len(entry.Tool.Input) + len(entry.Tool.Output)
			}
			if entry.Thinking != nil {
				total += len(entry.Thinking.Text)
			}
		}
	}
	return total
}

// fillEveryField gives every reachable field of v a non-zero value: one
// element in every slice, a value behind every pointer.
func fillEveryField(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("value")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillEveryField(v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillEveryField(v.Index(0))
	case reflect.Struct:
		for index := 0; index < v.NumField(); index++ {
			if v.Field(index).CanSet() {
				fillEveryField(v.Field(index))
			}
		}
	}
}

type rowMutation struct {
	name  string
	path  []int // field index, or -1 through a pointer, or -2 into element 0
	apply func(reflect.Value)
}

func (m rowMutation) on(row *LedgerEvent) {
	v := reflect.ValueOf(row).Elem()
	for _, step := range m.path {
		switch step {
		case -1:
			v = v.Elem()
		case -2:
			v = v.Index(0)
		default:
			v = v.Field(step)
		}
	}
	m.apply(v)
}

// rowMutations lists one change to every leaf of v, plus nil, empty and one
// more element for every slice and nil for every pointer.
func rowMutations(v reflect.Value, name string, path []int, out []rowMutation) []rowMutation {
	at := func(suffix string, apply func(reflect.Value)) {
		out = append(out, rowMutation{name: name + suffix, path: append([]int(nil), path...), apply: apply})
	}
	switch v.Kind() {
	case reflect.String:
		at("", func(x reflect.Value) { x.SetString(x.String() + "!") })
	case reflect.Bool:
		at("", func(x reflect.Value) { x.SetBool(!x.Bool()) })
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		at("", func(x reflect.Value) { x.SetInt(x.Int() + 1) })
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		at("", func(x reflect.Value) { x.SetUint(x.Uint() + 1) })
	case reflect.Pointer:
		at("=nil", func(x reflect.Value) { x.SetZero() })
		out = rowMutations(v.Elem(), name+"*", append(path, -1), out)
	case reflect.Slice:
		at("=nil", func(x reflect.Value) { x.SetZero() })
		at("=empty", func(x reflect.Value) { x.Set(reflect.MakeSlice(x.Type(), 0, 0)) })
		at("+1", func(x reflect.Value) { x.Set(reflect.Append(x, reflect.Zero(x.Type().Elem()))) })
		out = rowMutations(v.Index(0), name+"[0]", append(path, -2), out)
	case reflect.Struct:
		for index := 0; index < v.NumField(); index++ {
			if v.Field(index).CanSet() {
				out = rowMutations(v.Field(index), name+"."+v.Type().Field(index).Name, append(path, index), out)
			}
		}
	}
	return out
}

// ledgerRowEqual must see a change anywhere in a row, and nothing else: an
// exact copy and a decoded copy (equal strings in other memory) are equal.
func TestLedgerRowEqualSeesEveryField(t *testing.T) {
	var base LedgerEvent
	fillEveryField(reflect.ValueOf(&base).Elem())
	copied := exactLedgerCopy([]LedgerEvent{base})[0]
	if !ledgerRowEqual(&copied, &base) {
		t.Fatal("an exact copy differs from its row")
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var decoded LedgerEvent
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, base) || !ledgerRowEqual(&decoded, &base) {
		t.Fatal("a decoded copy differs from its row")
	}

	mutations := rowMutations(reflect.ValueOf(&base).Elem(), "LedgerEvent", nil, nil)
	if len(mutations) < 150 {
		t.Fatalf("only %d mutations: the walk missed fields", len(mutations))
	}
	for _, mutation := range mutations {
		changed := exactLedgerCopy([]LedgerEvent{base})[0]
		mutation.on(&changed)
		if reflect.DeepEqual(changed, base) {
			t.Fatalf("%s: the mutation changed nothing", mutation.name)
		}
		if ledgerRowEqual(&changed, &base) || ledgerRowEqual(&base, &changed) {
			t.Fatalf("%s: the change was not seen", mutation.name)
		}
	}

	// nil and empty differ, as they do in the stored JSON.
	empty := base
	empty.Attachments, empty.Timeline = []provider.Attachment{}, []TimelineEntry{}
	none := empty
	none.Attachments, none.Timeline = nil, nil
	if ledgerRowEqual(&empty, &none) {
		t.Fatal("an empty slice compared equal to nil")
	}
}

type exactEqualProbe struct {
	Flag  bool
	Count int64 // seven bytes of padding before it
	Names map[string][]string
	Any   any
	Ratio float64
	Pair  [2]string
	Bytes []byte
	Next  *exactEqualProbe
}

// The compiled comparison of kinds a ledger row does not use today: maps and
// interfaces through reflect.DeepEqual, floats and arrays, a recursive type,
// and padding, which never counts.
func TestExactEqualPlanCoversOtherKinds(t *testing.T) {
	plan := compileExactEqual(reflect.TypeFor[exactEqualProbe]())
	build := func() *exactEqualProbe {
		return &exactEqualProbe{
			Flag: true, Count: 3, Names: map[string][]string{"a": {"b"}}, Any: []int{1}, Ratio: 0.5,
			Pair: [2]string{"left", strings.Repeat("right", 2)}, Bytes: []byte("bytes"),
			Next: &exactEqualProbe{Count: 4, Bytes: []byte{}},
		}
	}
	equal := func(a, b *exactEqualProbe) bool { return plan.equal(unsafe.Pointer(a), unsafe.Pointer(b)) }
	base := build()
	if other := build(); !equal(base, other) {
		t.Fatal("equal values in other memory differ")
	}
	padded := build()
	padding := unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(padded), 1)), unsafe.Offsetof(padded.Count)-1)
	for index := range padding {
		padding[index] = 0xff
	}
	if !equal(base, padded) {
		t.Fatal("padding bytes were compared")
	}
	changes := map[string]func(*exactEqualProbe){
		"flag":       func(p *exactEqualProbe) { p.Flag = false },
		"map entry":  func(p *exactEqualProbe) { p.Names["a"][0] = "c" },
		"map key":    func(p *exactEqualProbe) { p.Names["z"] = nil },
		"interface":  func(p *exactEqualProbe) { p.Any = []int{2} },
		"float sign": func(p *exactEqualProbe) { p.Ratio = -0.5 },
		"array":      func(p *exactEqualProbe) { p.Pair[1] = "other" },
		"byte":       func(p *exactEqualProbe) { p.Bytes[0] = 'B' },
		"recursion":  func(p *exactEqualProbe) { p.Next.Count = 5 },
		"nil bytes":  func(p *exactEqualProbe) { p.Next.Bytes = nil },
	}
	for name, change := range changes {
		changed := build()
		change(changed)
		if reflect.DeepEqual(base, changed) {
			t.Fatalf("%s: the change changed nothing", name)
		}
		if equal(base, changed) || equal(changed, base) {
			t.Fatalf("%s: the change was not seen", name)
		}
	}
}

// BenchmarkSQLLedgerChangeDetection is the cost of deciding "the ledger is
// unchanged" on a large actor (4,000 rows, 100 of them 3 MiB): per streamed
// provider event (ledgerChanged with the committed slice), comparing every
// row (what a checkpoint and a reducer clone pay), for a head-only and a
// one-row checkpoint, and for a whole streamed chunk through the engine.
func BenchmarkSQLLedgerChangeDetection(b *testing.B) {
	const chatID = "change-detection-chat"
	actors := openTestActors(b)
	sqlStore := actors.Store(chatID)
	state := richLedgerState(b, chatID, 4_000, 20, 3<<20)
	if err := sqlStore.Save(state); err != nil {
		b.Fatal(err)
	}
	b.Logf("%d rows, %d MiB of text", len(state.Ledger), ledgerTextBytes(state.Ledger)>>20)

	b.Run("streamed-event-shared-slice", func(b *testing.B) {
		for range b.N {
			if sqlStore.ledgerChanged(state) {
				b.Fatal("an unchanged ledger was reported changed")
			}
		}
	})
	b.Run("every-row-compare", func(b *testing.B) {
		// What a checkpoint and a reducer clone pay: an equal ledger in a new
		// array (exact: State.Clone turns a nil Timeline into an empty one,
		// which does change a row's JSON).
		cloned := exactLedgerCopy(state.Ledger)
		b.ResetTimer()
		for range b.N {
			if !sqlStore.cache.sameLedger(cloned) {
				b.Fatal("an unchanged ledger was reported changed")
			}
		}
	})
	b.Run("head-only-checkpoint", func(b *testing.B) {
		next := state
		for range b.N {
			next.Revision++
			if err := sqlStore.journalCheckpoint(next); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("append-one-row-checkpoint", func(b *testing.B) {
		next := state
		next.Ledger = append([]LedgerEvent(nil), state.Ledger...)
		for range b.N {
			next.Revision++
			next.Ledger = append(next.Ledger, historyRow(len(next.Ledger)+1))
			if err := sqlStore.journalCheckpoint(next); err != nil {
				b.Fatal(err)
			}
		}
	})
	engine, lane := bigStreamingSQLEngine(b, actors)
	sequence := uint64(0)
	b.Run("engine-streamed-chunk", func(b *testing.B) {
		for range b.N {
			sequence++
			if err := engine.Apply(journalAssistantEvent(lane, sequence, "streamed words ")); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// bigStreamingSQLEngine is newJournalReadyEngine over the SQLite store, its
// ledger grown to the benchmark's size behind the live turn and checkpointed.
// (Reopening the actor would interrupt the turn.)
func bigStreamingSQLEngine(b *testing.B, actors *SQLActors) (*Engine, provider.LaneIdentity) {
	b.Helper()
	const chatID = "journal-chat"
	sqlStore := actors.Store(chatID)
	engine, err := NewDurableEngine(chatID, sqlStore)
	if err != nil {
		b.Fatal(err)
	}
	lane := testLane(chatID, "codex")
	openReadyDurableLane(b, engine, lane)
	if err := engine.Apply(Submit{
		OperationID: "journal-turn", Text: "question",
		Presentation: provider.TurnPresentation{UserMessageID: "journal-user", AssistantMessageID: "journal-assistant", Origin: "human"},
	}); err != nil {
		b.Fatal(err)
	}
	if _, ok, err := engine.ClaimEffect(startTurnEffectID("journal-turn")); err != nil || !ok {
		b.Fatalf("claim start turn: ok=%v err=%v", ok, err)
	}
	if err := engine.Apply(TurnAdmitted{OperationID: "journal-turn", Accepted: true,
		Turn: provider.TurnRef{OperationID: "journal-turn", NativeID: "journal-native-turn"}}); err != nil {
		b.Fatal(err)
	}
	state := engine.Snapshot()
	state.Ledger = appendRichHistory(state.Ledger, 4_000, 20, 3<<20)
	state.Revision++
	if err := sqlStore.Save(state); err != nil {
		b.Fatal(err)
	}
	engine.mu.Lock()
	engine.installCommittedState(state)
	engine.mu.Unlock()
	return engine, lane
}
