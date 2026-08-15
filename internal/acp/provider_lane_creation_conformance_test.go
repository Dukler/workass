package acp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func newCodexCandidateManager(t *testing.T, stateDir, threadID string, missingResume bool) *Manager {
	t.Helper()
	root := repoRoot(t)
	appServerArgs, err := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"WORKASS_CODEX_EXECUTABLE":        "node",
		"WORKASS_CODEX_APP_SERVER_ARGS":   string(appServerArgs),
		"WORKASS_CODEX_FIXTURE_THREAD_ID": threadID,
	}
	if missingResume {
		env["WORKASS_CODEX_FIXTURE_MISSING_RESUME"] = "1"
	}
	return NewManager(Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: ProviderConfig{
			ID: "codex", Command: "node", Args: []string{filepath.Join(root, "scripts", "codex-native-host.mjs")},
			CWD: root, Enabled: true, Env: env,
		},
		// This fixture starts a Node host and a nested mock app-server several
		// times. Package-parallel repository gates can delay process scheduling;
		// keep the test deadline about protocol failure, not host startup load.
		DefaultProviderID: "codex", InitTimeout: 10 * time.Second, RSSSampleInterval: time.Hour,
	})
}

func TestDeferredCodexCreatesAgainOnlyForAProvablyEmptyLane(t *testing.T) {
	stateDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request := func(manager *Manager, reconcile, createAfterAbsence bool, identity providercontract.LaneIdentity) (providercontract.Lane, providercontract.ThreadRef, providercontract.LaneIdentity, error) {
		t.Helper()
		if identity.ID == "" {
			selection, err := manager.ResolveProviderLaneSelection(ctx, SessionOptions{
				TabID: "candidate-tab", ChatID: "candidate-chat", ProviderID: "codex", CWD: manager.opts.RootDir,
			})
			if err != nil {
				t.Fatal(err)
			}
			identity = selection.Identity
		}
		definition, err := manager.ProviderDefinition("codex")
		if err != nil {
			t.Fatal(err)
		}
		lane, thread, err := definition.Runtime.Create(ctx, providercontract.CreateLaneRequest{
			Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "candidate-tab"},
			CWD: manager.opts.RootDir, Reconcile: reconcile,
			CreateAfterCandidateAbsence: createAfterAbsence,
		})
		if err != nil {
			return nil, providercontract.ThreadRef{}, identity, err
		}
		return lane, thread, lane.Identity(), nil
	}

	firstManager := newCodexCandidateManager(t, stateDir, "candidate-one", false)
	firstLane, first, identity, err := request(firstManager, false, true, providercontract.LaneIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	firstReceipt, ok := firstLane.(providercontract.ThreadCreationReceipt)
	if !ok || firstReceipt.ThreadCreationCommitted() || firstReceipt.PreviousCandidateAbsent() {
		t.Fatalf("initial Codex candidate receipt = %#v", firstReceipt)
	}
	if first.HeadID != "candidate-one" {
		t.Fatalf("initial Codex candidate = %#v", first)
	}
	firstManager.Reset()

	secondManager := newCodexCandidateManager(t, stateDir, "candidate-two", true)
	t.Cleanup(func() { secondManager.Reset() })
	secondLane, second, _, err := request(secondManager, true, true, identity)
	if err != nil {
		t.Fatal(err)
	}
	secondReceipt, ok := secondLane.(providercontract.ThreadCreationReceipt)
	if !ok || secondReceipt.ThreadCreationCommitted() || !secondReceipt.PreviousCandidateAbsent() {
		t.Fatalf("replacement candidate receipt = %#v", secondReceipt)
	}
	if second.HeadID != "candidate-two" || second.Equal(first) {
		t.Fatalf("authoritative absence did not replace only the provisional candidate: first=%#v second=%#v", first, second)
	}
	binding, ok := secondManager.nativeSessions.getForLane(identity)
	if !ok || binding.ThreadCommitted || bindingCurrentThreadID(binding) != "candidate-two" {
		t.Fatalf("replacement candidate store = ok:%v binding:%#v", ok, binding)
	}

	protectedStateDir := t.TempDir()
	protectedFirst := newCodexCandidateManager(t, protectedStateDir, "protected-one", false)
	_, _, protectedIdentity, err := request(protectedFirst, false, true, providercontract.LaneIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	protectedFirst.Reset()
	protectedSecond := newCodexCandidateManager(t, protectedStateDir, "protected-two", true)
	t.Cleanup(func() { protectedSecond.Reset() })
	if _, _, _, err := request(protectedSecond, true, false, protectedIdentity); !providercontract.ErrorIs(err, providercontract.ErrorNativeThreadMissing) {
		t.Fatalf("historical candidate missing error = %v", err)
	}
	protected, ok := protectedSecond.nativeSessions.getForLane(protectedIdentity)
	if !ok || protected.ThreadCommitted || bindingCurrentThreadID(protected) != "protected-one" {
		t.Fatalf("historical candidate changed after missing resume: ok=%v binding=%#v", ok, protected)
	}
}
