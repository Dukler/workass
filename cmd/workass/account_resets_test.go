package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestAccountResetConsentSurvivesTheSettingsReplace(t *testing.T) {
	t.Parallel()
	store := newAppSettingsStore(filepath.Join(t.TempDir(), "app-settings.json"))
	if got := store.get()[accountResetConsentKey]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("default consent = %#v, want an empty list", got)
	}
	saved, err := store.set(map[string]any{
		accountResetConsentKey: []any{" Acct ", "acct", "other-id", "", "bad id", 7, "../escape"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{"acct", "other-id"}
	if got := saved[accountResetConsentKey]; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized consent = %#v, want %#v", got, want)
	}
	if got := store.get()[accountResetConsentKey]; !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded consent = %#v, want %#v", got, want)
	}
	// Another surface's whole-blob write that omits the key drops consent:
	// renderers must round-trip it, which is why it lives in normalizeSettings.
	if roundTrip, _ := store.set(store.get()); !reflect.DeepEqual(roundTrip[accountResetConsentKey], want) {
		t.Fatalf("round trip lost consent: %#v", roundTrip[accountResetConsentKey])
	}
}

func TestAccountResetConsentGiven(t *testing.T) {
	t.Parallel()
	settings := normalizeSettings(map[string]any{accountResetConsentKey: []any{"acct"}})
	if !accountResetConsentGiven(settings, "ACCT") {
		t.Fatal("consented provider not recognized")
	}
	if accountResetConsentGiven(settings, "other") || accountResetConsentGiven(settings, "") || accountResetConsentGiven(nil, "acct") {
		t.Fatal("consent must be explicit per provider")
	}
	many := make([]any, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if got := normalizeAccountResetConsent(many); len(got) != maxAccountResetConsented {
		t.Fatalf("consent list is bounded: %d", len(got))
	}
}
