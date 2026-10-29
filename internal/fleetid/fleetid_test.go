package fleetid

import (
	"errors"
	"strings"
	"testing"
)

// realLocalIDs are ids in the exact shapes this product mints today, one per
// entity kind. They are the point of the whole design: a global id must carry
// them through unchanged, because every ledger row, receipt, archive file and
// renderer key already holds these strings.
var realLocalIDs = map[Type][]string{
	Chat:     {"app-chat-1792187130561-3", "snapshot-parity-chat", "tab-1", "chat-1"},
	Turn:     {"srv-turn-17c0f1a2b3c4-5", "snapshot-native-turn", "subagent-job-1792187130561-8"},
	Subagent: {"wa-subagent-1792187130561-8", "wa-subagent-1792180415563-2"},
	Process:  {"xw6qc-MsJIdH_X", "xw-6f12aa", "spare-1792187130561-4"},
	Wait:     {"sched-9b1c0d2e3f405162"},
	Fork:     {"fp-1792187130561-1"},
	ForkRun:  {"fr-1792187130561-2"},
	Peer:     {"m-91be04"},
}

func TestEveryEntityKindRoundTripsItsRealLocalIDs(t *testing.T) {
	const home = "m-7f3ad2"
	for _, kind := range Types() {
		locals := realLocalIDs[kind]
		if len(locals) == 0 {
			t.Fatalf("entity kind %q has no real local id sample; add one so its global form is covered", kind)
		}
		for _, local := range locals {
			built, err := New(home, kind, local)
			if err != nil {
				t.Fatalf("New(%q, %q, %q): %v", home, kind, local, err)
			}
			want := "wa:" + home + "/" + string(kind) + "/" + local
			if got := built.String(); got != want {
				t.Fatalf("global id = %q, want %q", got, want)
			}
			parsed, err := Parse(built.String())
			if err != nil {
				t.Fatalf("Parse(%q): %v", built.String(), err)
			}
			if parsed != built {
				t.Fatalf("round trip = %#v, want %#v", parsed, built)
			}
			// The identifier law in one assertion: the local id comes back byte
			// for byte, so nothing that stores it has to change.
			if parsed.Local != local {
				t.Fatalf("local id changed in the round trip: %q -> %q", local, parsed.Local)
			}
			if again := parsed.String(); again != built.String() {
				t.Fatalf("re-rendered id = %q, want %q", again, built.String())
			}
		}
	}
}

func TestLocalIDWithASlashSurvivesTheRoundTrip(t *testing.T) {
	// Nothing mints one today, but the parser must not be the reason a future
	// id shape is unrepresentable: everything after the second separator is the
	// local id.
	built, err := New("m-7f3ad2", Process, "xw-6f12aa/attempt-2")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(built.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Local != "xw-6f12aa/attempt-2" {
		t.Fatalf("local id = %q", parsed.Local)
	}
}

func TestParseRefusesTheShapesThatCannotAddressAnything(t *testing.T) {
	for _, value := range []string{
		"",
		"app-chat-1792187130561",
		"M~m-7f3ad2~app-chat-1",
		"wa:",
		"wa:m-7f3ad2",
		"wa:m-7f3ad2/chat",
		"wa:/chat/app-chat-1",
		"wa:m-7f3ad2//app-chat-1",
		"wa:m-7f3ad2/chat/",
		"wa:m-7f3ad2/tab/tab-1",
		"wa:m-7f3ad2/CHAT/app-chat-1",
	} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("Parse(%q) was accepted", value)
		}
	}
	if _, err := Parse("app-chat-1"); !errors.Is(err, ErrNotGlobal) {
		t.Fatalf("a bare local id must report ErrNotGlobal, got %v", err)
	}
	if _, err := Parse("wa:m-7f3ad2/chat"); errors.Is(err, ErrNotGlobal) {
		t.Fatal("a malformed global id must not be reported as 'not global'")
	}
}

func TestNewRefusesAHomeThatWouldNotParseBack(t *testing.T) {
	for _, home := range []string{"", "   ", "m-7f3/ad2", "m-7f3~ad2", "m 7f3ad2"} {
		if _, err := New(home, Chat, "app-chat-1"); err == nil {
			t.Fatalf("New accepted home %q", home)
		}
	}
	if _, err := New("m-7f3ad2", Type("process-child"), "xw-1"); err == nil {
		t.Fatal("New accepted an unknown entity type")
	}
	if _, err := New("m-7f3ad2", Chat, "  "); err == nil {
		t.Fatal("New accepted an empty local id")
	}
}

func TestZeroIDStringifiesToNothing(t *testing.T) {
	if got := (ID{}).String(); got != "" {
		t.Fatalf("zero id = %q", got)
	}
	if got := (ID{}).Legacy(); got != "" {
		t.Fatalf("zero legacy id = %q", got)
	}
	if got := (ID{Home: "m-7f3ad2", Type: Chat}).String(); got != "" {
		t.Fatalf("half-built id = %q", got)
	}
}

func TestLegacyTagIsAcceptedAndEmittedForTheSameEntity(t *testing.T) {
	built, err := New("m-91be04", Chat, "app-chat-1792187130561")
	if err != nil {
		t.Fatal(err)
	}
	legacy := built.Legacy()
	if legacy != "M~m-91be04~app-chat-1792187130561" {
		t.Fatalf("legacy alias = %q", legacy)
	}
	home, local, homed, err := Split(legacy, Chat)
	if err != nil || !homed {
		t.Fatalf("Split(%q) = homed=%v err=%v", legacy, homed, err)
	}
	if home != built.Home || local != built.Local {
		t.Fatalf("legacy alias resolved to %q/%q, want %q/%q", home, local, built.Home, built.Local)
	}
	// Both spellings of one entity must resolve identically. That is the whole
	// claim behind calling the tag an alias rather than a second id.
	globalHome, globalLocal, _, err := Split(built.String(), Chat)
	if err != nil {
		t.Fatal(err)
	}
	if globalHome != home || globalLocal != local {
		t.Fatalf("global %q and legacy %q disagree", built.String(), legacy)
	}
}

func TestSplitAcceptsAllThreeShapes(t *testing.T) {
	for _, item := range []struct {
		value string
		kind  Type
		home  string
		local string
		homed bool
	}{
		{"", Chat, "", "", false},
		{"app-chat-1", Chat, "", "app-chat-1", false},
		{"  app-chat-1  ", Chat, "", "app-chat-1", false},
		{"M~m-san~tab-1", Chat, "m-san", "tab-1", true},
		{"M~m-san~M~nested~tab-1", Chat, "m-san", "M~nested~tab-1", true},
		{"wa:m-san/chat/app-chat-1", Chat, "m-san", "app-chat-1", true},
		{"wa:m-san/chat/app-chat-1", "", "m-san", "app-chat-1", true},
		{"wa:m-san/subagent/wa-subagent-1-2", Subagent, "m-san", "wa-subagent-1-2", true},
	} {
		home, local, homed, err := Split(item.value, item.kind)
		if err != nil {
			t.Fatalf("Split(%q, %q): %v", item.value, item.kind, err)
		}
		if home != item.home || local != item.local || homed != item.homed {
			t.Fatalf("Split(%q, %q) = %q/%q/%v, want %q/%q/%v",
				item.value, item.kind, home, local, homed, item.home, item.local, item.homed)
		}
	}
}

func TestSplitRefusesAGlobalIDOfTheWrongKind(t *testing.T) {
	_, _, _, err := Split("wa:m-san/subagent/wa-subagent-1-2", Chat)
	if err == nil {
		t.Fatal("a subagent id was accepted where a chat id belongs")
	}
	if !strings.Contains(err.Error(), "subagent") || !strings.Contains(err.Error(), "chat") {
		t.Fatalf("refusal must name both kinds: %v", err)
	}
}

func TestSplitKeepsTheExistingWordingForAMalformedLegacyTag(t *testing.T) {
	for _, value := range []string{"M~", "M~broken", "M~m-san~", "M~~tab-1"} {
		_, _, _, err := Split(value, Chat)
		if err == nil {
			t.Fatalf("Split(%q) was accepted", value)
		}
		if err.Error() != "malformed machine-tagged chat id" {
			t.Fatalf("Split(%q) error = %q; the edge's existing wording must not change", value, err)
		}
	}
}

func TestRouteSendsThisDaemonsOwnEntitiesInProcess(t *testing.T) {
	router := NewRouter("m-7f3ad2")
	if router.Self() != "m-7f3ad2" {
		t.Fatalf("Self() = %q", router.Self())
	}
	for _, item := range []struct {
		name  string
		value string
		local string
		self  bool
		homed bool
	}{
		// M0: an id that names no home is this daemon's, which is how every
		// entity that already exists is at home here with nothing rewritten.
		{"a bare local id", "app-chat-1792187130561", "app-chat-1792187130561", true, false},
		{"a global id naming this daemon", "wa:m-7f3ad2/chat/app-chat-1792187130561", "app-chat-1792187130561", true, true},
		{"the legacy tag naming this daemon", "M~m-7f3ad2~app-chat-1792187130561", "app-chat-1792187130561", true, true},
		{"a global id naming another daemon", "wa:m-91be04/chat/app-chat-7", "app-chat-7", false, true},
		{"the legacy tag naming another daemon", "M~m-91be04~app-chat-7", "app-chat-7", false, true},
	} {
		resolved, err := router.Route(item.value, Chat)
		if err != nil {
			t.Fatalf("%s: %v", item.name, err)
		}
		if resolved.Self != item.self {
			t.Fatalf("%s: Self = %v, want %v", item.name, resolved.Self, item.self)
		}
		if resolved.Local != item.local {
			t.Fatalf("%s: Local = %q, want %q", item.name, resolved.Local, item.local)
		}
		if resolved.Homed != item.homed {
			t.Fatalf("%s: Homed = %v, want %v", item.name, resolved.Homed, item.homed)
		}
	}
}

func TestAllThreeSpellingsOfOneLocalEntityResolveIdentically(t *testing.T) {
	router := NewRouter("m-7f3ad2")
	id, err := router.ID(Subagent, "wa-subagent-1792187130561-8")
	if err != nil {
		t.Fatal(err)
	}
	if id.Home != "m-7f3ad2" || id.Local != "wa-subagent-1792187130561-8" {
		t.Fatalf("stamped id = %#v", id)
	}
	for _, spelling := range []string{id.Local, id.String(), id.Legacy()} {
		resolved, err := router.Route(spelling, Subagent)
		if err != nil {
			t.Fatalf("Route(%q): %v", spelling, err)
		}
		if !resolved.Self || resolved.Local != id.Local {
			t.Fatalf("Route(%q) = %#v; every spelling must reach the same in-process entity", spelling, resolved)
		}
		address, err := router.Address(spelling, Subagent)
		if err != nil {
			t.Fatalf("Address(%q): %v", spelling, err)
		}
		if address != id.Local {
			t.Fatalf("Address(%q) = %q, want the bare local id %q", spelling, address, id.Local)
		}
	}
}

func TestAddressDowngradesAnotherHomeToTheTagItsRelayUnderstands(t *testing.T) {
	router := NewRouter("m-7f3ad2")
	address, err := router.Address("wa:m-91be04/chat/app-chat-7", Chat)
	if err != nil {
		t.Fatal(err)
	}
	if address != "M~m-91be04~app-chat-7" {
		t.Fatalf("Address = %q", address)
	}
	// Already-legacy and unhomed ids travel untouched, so no existing caller
	// sees a different string than it sees today.
	for _, value := range []string{"M~m-91be04~app-chat-7", "app-chat-7", ""} {
		got, err := router.Address(value, Chat)
		if err != nil {
			t.Fatalf("Address(%q): %v", value, err)
		}
		if got != value {
			t.Fatalf("Address(%q) = %q; an id in today's shape must not change", value, got)
		}
	}
}

func TestARouterWithNoIdentityClaimsNothingAsLocal(t *testing.T) {
	// Until internal/machineid has answered, resolving an id as local would
	// address this machine's chat with another machine's id.
	router := NewRouter("")
	resolved, err := router.Route("app-chat-1", Chat)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Self {
		t.Fatal("a router with no identity claimed a bare id as local")
	}
	if resolved.Local != "app-chat-1" {
		t.Fatalf("Local = %q", resolved.Local)
	}
	if _, err := router.ID(Chat, "app-chat-1"); err == nil {
		t.Fatal("a router with no identity minted a global id")
	}
}

func TestAnEmptyIDAddressesNothing(t *testing.T) {
	router := NewRouter("m-7f3ad2")
	resolved, err := router.Route("   ", Chat)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != (Resolution{}) {
		t.Fatalf("empty id resolved to %#v", resolved)
	}
}

func TestHomeReportsTheOwningMachine(t *testing.T) {
	router := NewRouter("m-7f3ad2")
	for value, want := range map[string]string{
		"app-chat-1":                  "m-7f3ad2",
		"wa:m-91be04/chat/app-chat-1": "m-91be04",
		"M~m-91be04~app-chat-1":       "m-91be04",
	} {
		home, err := router.Home(value, Chat)
		if err != nil {
			t.Fatalf("Home(%q): %v", value, err)
		}
		if home != want {
			t.Fatalf("Home(%q) = %q, want %q", value, home, want)
		}
	}
	if _, err := router.Home("M~broken", Chat); err == nil {
		t.Fatal("Home accepted a malformed tag")
	}
}

func TestTypeSetIsClosedAndComplete(t *testing.T) {
	// The spec names eight kinds (fleet-runtime.md §3). A ninth added to the
	// constants without being listed would be silently unroutable.
	if len(Types()) != 8 {
		t.Fatalf("Types() has %d kinds, want the spec's 8", len(Types()))
	}
	seen := map[Type]bool{}
	for _, kind := range Types() {
		if !kind.Valid() {
			t.Fatalf("Types() lists %q, which Valid() rejects", kind)
		}
		if seen[kind] {
			t.Fatalf("Types() lists %q twice", kind)
		}
		seen[kind] = true
	}
	for _, kind := range []Type{"", "tab", "job", "work", "Chat", "process-child"} {
		if kind.Valid() {
			t.Fatalf("%q must not be a valid entity kind", kind)
		}
	}
}

func TestNamesHomeIsStrictWhereIsHomedIsLoose(t *testing.T) {
	// IsHomed guards an addressing field, where a value that opens with the
	// scheme must be reported rather than resolved. NamesHome guards a scan
	// that also reaches message text, where only a value that really names a
	// machine may be refused.
	for _, item := range []struct {
		value string
		homed bool
		names bool
	}{
		{"wa:m-7f3ad2/chat/app-chat-1", true, true},
		{"M~m-7f3ad2~app-chat-1", true, true},
		{"wa: is a scheme, not an id", true, false},
		{"wa:nothing", true, false},
		{"wa:m-7f3ad2/tab/tab-1", true, false},
		{"app-chat-1", false, false},
		{"M is a letter", false, false},
		{"", false, false},
	} {
		if got := IsHomed(item.value); got != item.homed {
			t.Fatalf("IsHomed(%q) = %v, want %v", item.value, got, item.homed)
		}
		if got := NamesHome(item.value); got != item.names {
			t.Fatalf("NamesHome(%q) = %v, want %v", item.value, got, item.names)
		}
	}
}
