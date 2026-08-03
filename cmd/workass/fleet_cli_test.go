package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/fleet"
)

func runFleet(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runFleetCommand(args, strings.NewReader(stdin), &stdout, &stderr)
	return stdout.String(), err
}

// The ceremony, end to end: one machine shows a key, another joins it, and the
// two agree on the key id — which is the only thing a client uses to recognise
// a machine as one of its own.
func TestFleetCLIMintsJoinsListsAndForgets(t *testing.T) {
	origin := t.TempDir()
	joiner := t.TempDir()

	minted, err := runFleet(t, "", "key", "--state-dir", origin)
	if err != nil {
		t.Fatalf("fleet key: %v", err)
	}
	secret := ""
	for _, line := range strings.Split(minted, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "wf-") {
			secret = strings.TrimSpace(line)
		}
	}
	if secret == "" {
		t.Fatalf("fleet key printed no key:\n%s", minted)
	}
	// Minting is idempotent: asking again shows the same key rather than
	// replacing the fleet under everything already joined to it.
	again, err := runFleet(t, "", "key", "--state-dir", origin)
	if err != nil || !strings.Contains(again, secret) || strings.Contains(again, "Minted") {
		t.Fatalf("second `fleet key` = %q err=%v", again, err)
	}

	// The key arrives on stdin, never argv: a command line is readable by every
	// process on the box and lands in shell history.
	joined, err := runFleet(t, secret+"\n", "join", "--state-dir", joiner)
	if err != nil {
		t.Fatalf("fleet join: %v", err)
	}
	listed, err := runFleet(t, "", "list", "--state-dir", joiner)
	if err != nil {
		t.Fatalf("fleet list: %v", err)
	}
	keyID := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(joined), "joined fleet"))
	keyID = strings.SplitN(keyID, "\n", 2)[0]
	if keyID == "" || !strings.Contains(minted, keyID) || !strings.Contains(listed, keyID) {
		t.Fatalf("key id disagreement: minted=%q joined=%q listed=%q", minted, keyID, listed)
	}
	if strings.Contains(listed, secret) {
		t.Fatalf("`fleet list` printed the secret:\n%s", listed)
	}

	// Joining twice is the same fleet, not two.
	if _, err := runFleet(t, secret+"\n", "join", "--state-dir", joiner); err != nil {
		t.Fatalf("second join: %v", err)
	}
	listedTwice, _ := runFleet(t, "", "list", "--state-dir", joiner)
	if strings.Count(listedTwice, keyID) != 1 {
		t.Fatalf("joining twice duplicated the key:\n%s", listedTwice)
	}

	if _, err := runFleet(t, "", "forget", keyID, "--state-dir", joiner); err != nil {
		t.Fatalf("fleet forget: %v", err)
	}
	gone, _ := runFleet(t, "", "list", "--state-dir", joiner)
	if strings.Contains(gone, keyID) {
		t.Fatalf("forgotten key still listed:\n%s", gone)
	}

	// The file that holds a fleet is readable by its owner and nobody else.
	info, err := os.Stat(filepath.Join(origin, "fleet.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("fleet.json mode = %v, want 0600", mode)
	}
}

func TestFleetCLIRefusesGarbageAndNamesTheCommandOrder(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := runFleet(t, "not-a-key\n", "join", "--state-dir", stateDir); err == nil {
		t.Fatal("a malformed key was accepted")
	}
	if _, err := runFleet(t, "", "join", "--state-dir", stateDir); err == nil {
		t.Fatal("an empty stdin was accepted as a key")
	}
	// Go's flag package stops at the first non-flag word. `fleet --state-dir X
	// key` would parse, then act on a state dir nobody named, so it is refused
	// rather than silently minting into the wrong machine.
	if _, err := runFleet(t, "", "--state-dir", stateDir, "key"); err == nil {
		t.Fatal("flags before the command were accepted; --state-dir would be ignored")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "fleet.json")); err == nil {
		t.Fatal("a refused command still wrote a fleet key")
	}
}

// `qr` reads like `list`. A mint hidden inside it creates a credential from a
// command that looks like a question — and then draws a code for a fleet no
// daemon is running, which scans perfectly and enrols nowhere. Reported from
// workass-mobile: three directories, three fleets, none of them the live one,
// and one live key left sitting untracked inside a git repo.
func TestFleetQRRefusesRatherThanMintingAFleetNobodyAskedFor(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	var stdout, stderr bytes.Buffer
	err := runFleetCommand([]string{"qr", "192.168.0.13", "--state-dir", stateDir},
		strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatalf("qr minted a key instead of refusing; output=%q", stdout.String())
	}
	if !strings.Contains(err.Error(), stateDir) {
		t.Fatalf("refusal does not name the path it looked at: %v", err)
	}
	if !strings.Contains(err.Error(), "fleet key") {
		t.Fatalf("refusal does not say what is missing: %v", err)
	}
	// Nothing was written. A read-shaped command must leave no credential behind.
	if _, statErr := os.Stat(filepath.Join(stateDir, fleet.FileName)); !os.IsNotExist(statErr) {
		t.Fatalf("qr created %s despite refusing", filepath.Join(stateDir, fleet.FileName))
	}
	if strings.Contains(stdout.String(), "workass://") {
		t.Fatalf("qr drew a code with no key: %q", stdout.String())
	}
}

// With a key present it draws, and it names the state directory it drew from —
// a code from the wrong one is indistinguishable from a correct one until a
// phone fails to enrol.
func TestFleetQRDrawsFromAnExistingKeyAndNamesItsSource(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	var minted bytes.Buffer
	if err := runFleetCommand([]string{"key", "--state-dir", stateDir},
		strings.NewReader(""), &minted, &minted); err != nil {
		t.Fatal(err)
	}
	store, err := fleet.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	keys := store.Keys()
	if len(keys) != 1 {
		t.Fatalf("expected one key, got %d", len(keys))
	}

	var stdout, stderr bytes.Buffer
	if err := runFleetCommand([]string{"qr", "192.168.0.13:18788", "--state-dir", stateDir},
		strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("qr: %v (%s)", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "workass://join?h=192.168.0.13:18788&k="+keys[0].Secret) {
		t.Fatalf("qr drew the wrong payload: %q", out)
	}
	if !strings.Contains(out, stateDir) {
		t.Fatalf("qr did not name the state directory it read: %q", out)
	}
}

// The old default was the relative path "state", so the command acted on the
// shell's cwd rather than on this machine.
func TestFleetStateDirDefaultsToTheRunningDaemonsState(t *testing.T) {
	t.Setenv(dataRootEnvVar, "/tmp/workass-test-root")
	if got, want := defaultFleetStateDir(), filepath.Join("/tmp/workass-test-root", "state"); got != want {
		t.Fatalf("default state dir = %q, want %q", got, want)
	}
}
