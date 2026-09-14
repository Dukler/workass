package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestDeferredDevinCandidateAbsencePreservesCommittedThreadProtection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		committed   bool
		allowCreate bool
		wantCreate  bool
	}{
		{"unused candidate may recover", false, true, true},
		{"candidate without absence authority stays fixed", false, false, false},
		{"committed thread stays fixed despite absence authority", true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPersistentMockFixture(t, "load")
			newManager := func() (*Manager, *eventCollector) {
				return fixture.newManagerTuned(func(opts *Options) {
					opts.RuntimeProfile = "dev"
					opts.Provider.ID = "devin"
					opts.DefaultProviderID = "devin"
					opts.Provider.Env["WORKASS_MOCK_ACP_DEVIN_LOAD_ABSENCE"] = "1"
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			firstManager, _ := newManager()
			t.Cleanup(func() { firstManager.Reset() })
			selection, err := firstManager.ResolveProviderLaneSelection(ctx, SessionOptions{
				TabID: "candidate-tab", ChatID: "candidate-chat", ProviderID: "devin", CWD: fixture.root,
			})
			if err != nil {
				t.Fatal(err)
			}
			definition, err := firstManager.ProviderDefinition("devin")
			if err != nil {
				t.Fatal(err)
			}
			request := providercontract.CreateLaneRequest{
				Identity: selection.Identity, Owner: providercontract.AttachmentOwner{TabID: "candidate-tab"}, CWD: fixture.root,
			}
			firstLane, firstThread, err := definition.Runtime.Create(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			request.Identity = firstLane.Identity()
			if firstLane.(providercontract.ThreadCreationReceipt).ThreadCreationCommitted() {
				t.Fatal("Devin session/new unexpectedly committed the candidate")
			}
			if test.committed {
				managed := firstLane.(*managerLane)
				terminal := make(chan struct{})
				t.Cleanup(func() { firstLane.Detach(context.Background()) })
				go func() {
					for event := range firstLane.Events() {
						managed.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
						if event.Kind == providercontract.EventTurnTerminal {
							close(terminal)
						}
					}
				}()
				_, err := firstLane.Delivery().StartTurn(ctx, providercontract.TurnInput{
					OperationID: "commit-candidate", Text: "commit the deterministic candidate",
				})
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-terminal:
				case <-ctx.Done():
					t.Fatal("mock turn did not finish")
				}
			}
			before, ok := firstManager.nativeSessions.getForLane(request.Identity)
			if !ok || before.ThreadCommitted != test.committed {
				t.Fatal("fixture did not establish the expected commitment boundary")
			}
			firstManager.Reset()
			// Reproduce Devin's exact load error through the real stdio bridge,
			// including both the provisional and committed-thread lifecycle paths.
			if err := os.Remove(fixture.sessionFile); err != nil {
				t.Fatal(err)
			}
			secondManager, _ := newManager()
			t.Cleanup(func() { secondManager.Reset() })
			definition, err = secondManager.ProviderDefinition("devin")
			if err != nil {
				t.Fatal(err)
			}
			request.Reconcile = true
			request.CreateAfterCandidateAbsence = test.allowCreate
			secondLane, secondThread, err := definition.Runtime.Create(ctx, request)
			after, ok := secondManager.nativeSessions.getForLane(request.Identity)
			if !ok {
				t.Fatal("absence erased the lane binding")
			}
			if test.wantCreate {
				if err != nil {
					t.Fatal(err)
				}
				receipt := secondLane.(providercontract.ThreadCreationReceipt)
				if receipt.ThreadCreationCommitted() || !receipt.PreviousCandidateAbsent() || secondThread.Equal(firstThread) || bindingCurrentThreadID(after) != secondThread.HeadID {
					t.Fatal("authoritative absence did not produce one fresh provisional candidate")
				}
			} else if !providercontract.ErrorIs(err, providercontract.ErrorNativeThreadMissing) || bindingCurrentThreadID(after) != firstThread.HeadID || after.ThreadCommitted != before.ThreadCommitted {
				t.Fatalf("protected thread changed or lost its absence error: %v", err)
			}
			if !test.wantCreate {
				var rpcErr *acpError
				if !errors.As(err, &rpcErr) || rpcErr.Code != -32016 {
					t.Fatal("exact Devin load rejection was not preserved through the bridge")
				}
			}
			if _, err := os.Stat(fixture.sessionFile); test.wantCreate && err != nil || !test.wantCreate && !os.IsNotExist(err) {
				t.Fatal("provider session creation did not match the allowed recovery boundary")
			}
		})
	}
}

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
	t.Parallel()
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
