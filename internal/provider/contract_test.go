package provider

import (
	"context"
	"errors"
	"testing"
)

type testRealmResolver struct{}

func (testRealmResolver) ResolveRealm(_ context.Context, request RealmRequest) (Realm, error) {
	return Realm{ProviderID: request.ProviderID, MachineID: request.MachineID}, nil
}

type testLaneFactory struct{}

func (testLaneFactory) Create(context.Context, CreateLaneRequest) (Lane, ThreadRef, error) {
	return nil, ThreadRef{}, errors.New("not used")
}

func (testLaneFactory) Resume(context.Context, ResumeLaneRequest) (Lane, error) {
	return nil, errors.New("not used")
}

type testAuthenticationStrategy struct{}

func (testAuthenticationStrategy) IsAuthenticationFailure(error) bool { return false }
func (testAuthenticationStrategy) LoginHint() string                  { return "" }

func TestLaneIdentityIsDeterministicAndRealmScoped(t *testing.T) {
	realm := Realm{ProviderID: "Codex", MachineID: "machine-a", AccountScope: "account-a", InstallScope: "official"}
	first := LaneIdentity{ChatID: "chat-1", Realm: realm, WorkspaceEpoch: "workspace-1"}.Normalize()
	second := LaneIdentity{ChatID: "chat-1", Realm: realm, WorkspaceEpoch: "workspace-1"}.Normalize()
	if err := first.Validate(); err != nil {
		t.Fatalf("lane identity: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("lane id is not deterministic: %q != %q", first.ID, second.ID)
	}
	changed := LaneIdentity{ChatID: "chat-1", Realm: realm, WorkspaceEpoch: "workspace-2"}.Normalize()
	if first.ID == changed.ID {
		t.Fatal("workspace epoch did not change lane identity")
	}
}

func TestThreadRefCannotCrossProvider(t *testing.T) {
	ref := ThreadRef{ProviderID: "codex", RootID: "native-thread", HeadID: "native-thread", Lineage: 1}
	if err := ref.Validate("codex"); err != nil {
		t.Fatalf("valid thread ref: %v", err)
	}
	if err := ref.Validate("claude"); err == nil {
		t.Fatal("cross-provider native thread was accepted")
	}
}

func TestThreadHeadAdvancesOnlyWithSameLineageAndProof(t *testing.T) {
	current := ThreadRef{ProviderID: "claude", RootID: "root", HeadID: "head-1", Lineage: 1}
	valid := ThreadRef{ProviderID: "claude", RootID: "root", HeadID: "head-2", Lineage: 2, Proof: "provider-attestation"}
	if !current.CanAdvanceTo(valid) {
		t.Fatal("attested same-lineage head advance was rejected")
	}
	for _, invalid := range []ThreadRef{
		{ProviderID: "claude", RootID: "other", HeadID: "head-2", Lineage: 2, Proof: "provider-attestation"},
		{ProviderID: "claude", RootID: "root", HeadID: "head-2", Lineage: 2},
		{ProviderID: "claude", RootID: "root", HeadID: "head-2", Lineage: 1, Proof: "provider-attestation"},
	} {
		if current.CanAdvanceTo(invalid) {
			t.Fatalf("invalid lineage advance accepted: %#v", invalid)
		}
	}
}

func TestRegistryRejectsDuplicateAndIncompleteDefinitions(t *testing.T) {
	registry := NewRegistry()
	definition := Definition{
		Identity:       ProviderIdentity{ID: "custom.acp", DisplayName: "Custom ACP"},
		Realm:          testRealmResolver{},
		Runtime:        testLaneFactory{},
		Authentication: testAuthenticationStrategy{},
	}
	if err := registry.Register(definition); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(definition); err == nil {
		t.Fatal("duplicate provider registration was accepted")
	}
	if _, ok := registry.Resolve("custom-acp"); !ok {
		t.Fatal("normalized provider id did not resolve")
	}
	if err := registry.Register(Definition{Identity: ProviderIdentity{ID: "broken", DisplayName: "Broken"}}); err == nil {
		t.Fatal("definition without runtime facets was accepted")
	}
	if err := registry.Register(Definition{
		Identity: ProviderIdentity{ID: "missing-auth", DisplayName: "Missing auth"},
		Realm:    testRealmResolver{}, Runtime: testLaneFactory{},
	}); err == nil {
		t.Fatal("definition without authentication strategy was accepted")
	}
}

func TestProviderEventRequiresTypedPayload(t *testing.T) {
	identity := EventIdentity{ChatID: "chat", LaneID: "lane", Sequence: 1}
	if err := (Event{Kind: EventTurnTerminal, Identity: identity}).Validate(); err == nil {
		t.Fatal("terminal event without terminal payload was accepted")
	}
	if err := (Event{Kind: EventTurnTerminal, Identity: identity, Terminal: &TerminalEvent{Status: "done"}}).Validate(); err != nil {
		t.Fatalf("valid terminal event: %v", err)
	}
}

func TestTypedErrorClassification(t *testing.T) {
	err := &Error{Kind: ErrorAcceptanceAmbiguous, Operation: "op-1", Message: "delivery uncertain"}
	if !ErrorIs(err, ErrorAcceptanceAmbiguous) {
		t.Fatal("typed provider error was not classified")
	}
	if ErrorIs(err, ErrorAdmissionRejected) {
		t.Fatal("typed provider error matched the wrong class")
	}
}
