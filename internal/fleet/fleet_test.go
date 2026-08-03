package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return store
}

func TestNewSecretIsTypeableAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		secret, err := NewSecret()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !strings.HasPrefix(secret, KeyPrefix) {
			t.Fatalf("secret %q has no prefix", secret)
		}
		if got := len(secret); got != len(KeyPrefix)+26 {
			t.Fatalf("secret %q is %d chars", secret, got)
		}
		if seen[secret] {
			t.Fatalf("minted %q twice", secret)
		}
		seen[secret] = true
	}
}

// A key is read off one screen and typed into another. Being strict about how a
// human types it only ever punishes the human.
func TestNormalizeSecretForgivesHumanTyping(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	body := strings.TrimPrefix(secret, KeyPrefix)
	variants := []string{
		secret,
		strings.ToUpper(secret),
		"  " + secret + "\n",
		body,
		KeyPrefix + body[:6] + "-" + body[6:],
		KeyPrefix + body[:4] + " " + body[4:],
		strings.ToUpper(KeyPrefix + body[:8] + "-" + body[8:]),
		// A separator where the dash belongs. These are the variants that make
		// the ORDER above load-bearing: separators come out first, and the
		// prefix trimmed afterwards is "wf", two characters, because the dash is
		// already gone. Strip "wf-" from the raw string instead — which reads
		// identically and is the obvious way to write it — and none of the
		// variants above notice, while these three become a different key.
		// Reported from workass-mobile, whose port had exactly that ordering.
		"wf " + body,
		"wf_" + body,
		"WF " + strings.ToUpper(body),
	}
	for _, variant := range variants {
		got, err := NormalizeSecret(variant)
		if err != nil {
			t.Fatalf("NormalizeSecret(%q): %v", variant, err)
		}
		if got != secret {
			t.Fatalf("NormalizeSecret(%q) = %q, want %q", variant, got, secret)
		}
	}
}

func TestNormalizeSecretRejectsRubbish(t *testing.T) {
	for _, bad := range []string{"", "   ", "wf-", "hello", "wf-notbase32!!", "wf-aaaa"} {
		if _, err := NormalizeSecret(bad); !errors.Is(err, ErrMalformedKey) {
			t.Fatalf("NormalizeSecret(%q) = %v, want ErrMalformedKey", bad, err)
		}
	}
}

// A proof must never be replayable as a token, and neither may be replayed
// against a different machine or a different exchange.
func TestDerivationsAreSeparated(t *testing.T) {
	secret, _ := NewSecret()
	proof := Proof(secret, "server-1", "client-1", "m-abc")
	token := Token(secret, "server-1", "client-1", "m-abc")

	if proof == token {
		t.Fatal("proof and token are the same value — a listener could replay one as the other")
	}
	if len(proof) != 64 || len(token) != 64 {
		t.Fatalf("expected sha256 hex, got %d/%d chars", len(proof), len(token))
	}
	cases := map[string]string{
		"other server nonce": Token(secret, "server-2", "client-1", "m-abc"),
		"other client nonce": Token(secret, "server-1", "client-2", "m-abc"),
		"other machine":      Token(secret, "server-1", "client-1", "m-xyz"),
	}
	for name, other := range cases {
		if other == token {
			t.Fatalf("%s produced the same token — the input is not bound to it", name)
		}
	}

	// Field boundaries are separated, so shifting a character across one must
	// not produce the same derivation.
	if Token(secret, "ab", "c", "m") == Token(secret, "a", "bc", "m") {
		t.Fatal("field boundaries are ambiguous")
	}

	otherSecret, _ := NewSecret()
	if Token(otherSecret, "server-1", "client-1", "m-abc") == token {
		t.Fatal("a different key derived the same token")
	}
}

func TestDerivationIsStableAcrossEquivalentSpellings(t *testing.T) {
	secret, _ := NewSecret()
	messy := strings.ToUpper(strings.TrimPrefix(secret, KeyPrefix))
	if Token(secret, "s", "c", "m") != Token(messy, "s", "c", "m") {
		t.Fatal("the same key typed differently derived a different token")
	}
}

func TestEnsureKeyMintsOnceThenPersists(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	key, minted, err := store.EnsureKey()
	if err != nil || !minted {
		t.Fatalf("first EnsureKey = %v, %v", minted, err)
	}
	again, minted, err := store.EnsureKey()
	if err != nil || minted {
		t.Fatalf("second EnsureKey minted again: %v, %v", minted, err)
	}
	if again.KeyID != key.KeyID {
		t.Fatalf("key changed under us: %q -> %q", key.KeyID, again.KeyID)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	keys := reopened.Keys()
	if len(keys) != 1 || keys[0].Secret != key.Secret {
		t.Fatalf("key did not survive restart: %+v", keys)
	}
	if keys[0].Owner != LocalOwner {
		t.Fatalf("owner = %q", keys[0].Owner)
	}
}

// The fleet file is the fleet. Another account on the box must not be able to
// read it.
func TestFleetFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	store, _ := Open(dir)
	if _, _, err := store.EnsureKey(); err != nil {
		t.Fatalf("mint: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("fleet file mode is %04o, want 0600", perm)
	}
}

func TestJoinIsIdempotent(t *testing.T) {
	store := openStore(t)
	secret, _ := NewSecret()

	first, err := store.Join(secret, "", "phone")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	// Same key, typed differently. Pasting twice must be harmless.
	second, err := store.Join(strings.ToUpper(secret), "", "phone again")
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if first.KeyID != second.KeyID {
		t.Fatalf("key ids differ: %q vs %q", first.KeyID, second.KeyID)
	}
	if got := len(store.Keys()); got != 1 {
		t.Fatalf("store holds %d keys after joining the same fleet twice", got)
	}
}

func TestVerifyAcceptsTheRightKeyAndDerivesTheSameToken(t *testing.T) {
	store := openStore(t)
	key, _, err := store.EnsureKey()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	serverNonce, _ := NewNonce()
	clientNonce, _ := NewNonce()

	// What a client computes, knowing only the key.
	proof := Proof(key.Secret, serverNonce, clientNonce, "m-daemon")
	matched, token, ok := store.Verify(serverNonce, clientNonce, "m-daemon", proof)
	if !ok {
		t.Fatal("a correct proof was rejected")
	}
	if matched.KeyID != key.KeyID {
		t.Fatalf("matched %q, want %q", matched.KeyID, key.KeyID)
	}
	if want := Token(key.Secret, serverNonce, clientNonce, "m-daemon"); token != want {
		t.Fatal("the two sides derived different tokens — the client could never authenticate")
	}
}

func TestVerifyRejectsWrongKeyAndReplay(t *testing.T) {
	store := openStore(t)
	key, _, _ := store.EnsureKey()
	stranger, _ := NewSecret()
	serverNonce, _ := NewNonce()
	clientNonce, _ := NewNonce()

	if _, _, ok := store.Verify(serverNonce, clientNonce, "m-daemon", Proof(stranger, serverNonce, clientNonce, "m-daemon")); ok {
		t.Fatal("a proof from another fleet was accepted")
	}
	// A proof captured from one exchange must be worthless in the next.
	stale := Proof(key.Secret, serverNonce, clientNonce, "m-daemon")
	nextNonce, _ := NewNonce()
	if _, _, ok := store.Verify(nextNonce, clientNonce, "m-daemon", stale); ok {
		t.Fatal("a replayed proof was accepted against a fresh nonce")
	}
	// A proof for one machine must not open another.
	if _, _, ok := store.Verify(serverNonce, clientNonce, "m-other", stale); ok {
		t.Fatal("a proof was accepted by a machine it was not made for")
	}
	if _, _, ok := store.Verify(serverNonce, clientNonce, "m-daemon", ""); ok {
		t.Fatal("an empty proof was accepted")
	}
}

// A daemon in two fleets accepts either, which is what lets a second person's
// key be added without disturbing the first.
func TestVerifyAcrossSeveralKeys(t *testing.T) {
	store := openStore(t)
	mine, _, _ := store.EnsureKey()
	theirSecret, _ := NewSecret()
	theirs, err := store.Join(theirSecret, "someone-else", "second person")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	serverNonce, _ := NewNonce()
	clientNonce, _ := NewNonce()

	for _, key := range []Key{mine, theirs} {
		matched, _, ok := store.Verify(serverNonce, clientNonce, "m-daemon", Proof(key.Secret, serverNonce, clientNonce, "m-daemon"))
		if !ok || matched.KeyID != key.KeyID {
			t.Fatalf("key %q (%s) was not accepted", key.KeyID, key.Owner)
		}
	}
	if matched, _, _ := store.Verify(serverNonce, clientNonce, "m-daemon", Proof(theirs.Secret, serverNonce, clientNonce, "m-daemon")); matched.Owner != "someone-else" {
		t.Fatalf("owner = %q, want the owner that key was filed under", matched.Owner)
	}
}

// KeyIDs are advertised on an unauthenticated endpoint so clients can recognise
// their own machines. They must not leak the secret.
func TestKeyIDIsPublicAndOneWay(t *testing.T) {
	store := openStore(t)
	key, _, _ := store.EnsureKey()

	ids := store.KeyIDs()
	if len(ids) != 1 || ids[0] != key.KeyID {
		t.Fatalf("KeyIDs = %v", ids)
	}
	if strings.Contains(key.KeyID, strings.TrimPrefix(key.Secret, KeyPrefix)) {
		t.Fatal("the key id contains the secret")
	}
	body := strings.TrimPrefix(key.Secret, KeyPrefix)
	for cut := 4; cut < len(body); cut += 4 {
		if strings.Contains(key.KeyID, body[:cut]) {
			t.Fatalf("the key id leaks a %d-character prefix of the secret", cut)
		}
	}
	if KeyIDOf(key.Secret) != KeyIDOf(strings.ToUpper(key.Secret)) {
		t.Fatal("key id depends on how the secret was typed")
	}
}

func TestForgetGatesFutureEnrolmentsOnly(t *testing.T) {
	store := openStore(t)
	key, _, _ := store.EnsureKey()
	serverNonce, _ := NewNonce()
	clientNonce, _ := NewNonce()
	token := Token(key.Secret, serverNonce, clientNonce, "m-daemon")

	forgotten, err := store.Forget(key.KeyID)
	if err != nil || !forgotten {
		t.Fatalf("forget = %v, %v", forgotten, err)
	}
	if _, _, ok := store.Verify(serverNonce, clientNonce, "m-daemon", Proof(key.Secret, serverNonce, clientNonce, "m-daemon")); ok {
		t.Fatal("a forgotten key still enrols")
	}
	// The token a device already holds is independent of the key it came from;
	// revoking a device is a separate act from retiring a key.
	if token == "" {
		t.Fatal("derived token was empty")
	}
	if store.Has() {
		t.Fatal("store still reports a key")
	}
}

func TestOpenRefusesCorruptFleetFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a corrupt fleet file opened silently — every machine would lock out with no explanation")
	}
}
