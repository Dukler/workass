package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// TestActorRendererSessionSnapshotParityAcrossRestartAndMirrorForgery is the
// renderer/session acceptance gate for the actor cutover. It deliberately
// builds the rich state through the actor reducer, then checks every read path
// from that state after a durable restart. The legacy mirror is only an input
// to the final cutover receipt and is forged after that receipt exists.
func TestActorRendererSessionSnapshotParityAcrossRestartAndMirrorForgery(t *testing.T) {
	const (
		tabID          = "snapshot-parity-tab"
		chatID         = "snapshot-parity-chat"
		ownerKey       = "snapshot-parity-owner"
		providerID     = providercontract.ID("mock")
		modelID        = "mock-model"
		modeID         = "plan"
		turnOperation  = providercontract.OperationID("snapshot-turn")
		nativeTurnID   = "snapshot-native-turn"
		steerOperation = providercontract.OperationID("snapshot-steer")
	)

	workspace := t.TempDir()
	baselineCommit := snapshotParityGitFixture(t, workspace)
	stateDir := t.TempDir()

	manager := acp.NewManager(acp.Options{
		RootDir: workspace, StateDir: stateDir, RuntimeProfile: "dev",
	})
	store := sharedSessionStore(stateDir)
	runtime := newProviderChatRuntimeBeforeProviderStartup(manager, store, stateDir)
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("start paused actor runtime: %v", err)
	}

	actor, err := runtime.actorForNewChat(chatID, snapshotParityPresentation(workspace, providerID, modelID, modeID))
	if err != nil {
		t.Fatalf("create file-backed actor chat: %v", err)
	}

	image := snapshotParityAttachment(t, stateDir, "snapshot-image", "fixture-image.txt")
	queueImage := image
	queueImage.ID = "snapshot-queue-image"
	toolImage := image
	toolImage.ID = "snapshot-tool-image"
	assistantImage := image
	assistantImage.ID = "snapshot-assistant-image"
	steerImage := image
	steerImage.ID = "snapshot-steer-image"

	presentation := actor.engine.Snapshot().Presentation.Clone()
	presentation.Draft = "draft persisted by actor"
	applySnapshotParityCommand(t, actor.engine, chat.SavePresentation{
		OperationID: "snapshot-presentation-save", Digest: "snapshot-presentation-digest", Presentation: presentation,
	})

	identity := providercontract.LaneIdentity{
		ChatID: chatID,
		Realm: providercontract.Realm{
			ProviderID: providerID, MachineID: "snapshot-machine", AccountScope: "snapshot-account",
			InstallScope: "snapshot-install", Verified: true,
		},
		WorkspaceEpoch: "snapshot-workspace-epoch",
	}.Normalize()
	owner := providercontract.AttachmentOwner{TabID: tabID, AgentOwnerKey: ownerKey}
	applySnapshotParityCommand(t, actor.engine, chat.SelectLane{
		Identity: identity, Owner: owner, CWD: workspace, ModelID: modelID, ModeID: modeID,
	})

	commandCatalog := &providercontract.RuntimeCommandCatalog{
		Commands: []providercontract.RuntimeCommand{{
			Name: "/review", Description: "Review the current workspace", ArgumentHint: "[path]", Aliases: []string{"r"},
		}},
		Agents:      []providercontract.RuntimeAgent{{Name: "reviewer", Description: "Reviews a change", Model: modelID}},
		OutputStyle: "concise", AvailableOutputStyles: []string{"concise", "detailed"},
		CommandsTruncated: 1, AgentsTruncated: 2, StylesTruncated: 3, AsOf: 1786440010000,
	}
	thread := providercontract.ThreadRef{
		ProviderID: providerID, RootID: "snapshot-native-root", HeadID: "snapshot-native-head", Lineage: 1,
		Proof: "snapshot-lineage-proof",
	}
	applySnapshotParityCommand(t, actor.engine, chat.LaneOpened{
		LaneID: identity.ID, Identity: identity, Thread: thread, ConnectionGeneration: 1,
		Context: providercontract.ContextCapabilities{
			ExactResume: true, ImportMode: providercontract.ContextImportUnsupported,
			NativeCompaction: true, VerifiedLineage: true,
		},
		Delivery: providercontract.DeliveryCapabilities{
			StableInputIdentity: true, LiveSteer: true, ConsumptionReceipt: true, TurnReadback: true,
		},
		Attachment: &providercontract.LaneAttachmentSnapshot{
			ConnectionID: "snapshot-connection", CWD: workspace, Agent: "Mock ACP",
			ProviderID: providerID, ProviderName: "Mock ACP", Models: []providercontract.RuntimeModel{{
				ID: modelID, Name: "Mock deterministic", Efforts: []string{"low", "high"},
			}}, CurrentModelID: modelID,
			Modes: []providercontract.RuntimeMode{{ID: modeID, Name: "Plan"}}, CurrentModeID: modeID,
			ImageSupport: true, CommandCatalogSupported: true, CommandCatalog: commandCatalog,
		},
	})

	applySnapshotParityCommand(t, actor.engine, chat.UpdateRuntimeControls{
		ProviderID: providerID, ModelID: modelID, ModeID: modeID,
		ReplaceModelID: true, ReplaceModeID: true,
		ModelControls: actor.engine.Snapshot().Presentation.ModelControls, ReplaceModelControls: true,
		ExpectedRevision: 0, RequireRevision: true,
	})
	applySnapshotParityCommand(t, actor.engine, chat.ReplaceStagedQueue{
		OperationID: "snapshot-queue-replace", Digest: "snapshot-queue-digest", ExpectedRevision: 0,
		Entries: []chat.StagedQueueEntry{{
			ID: "snapshot-staged-follow-up", Text: "queued follow-up", Source: "agent", Delivery: "queue",
			QueuedAt: "2026-08-11T12:00:02Z", Attachments: []providercontract.Attachment{queueImage},
			AttachmentNames: []string{"fixture-image.txt"}, AttachmentState: "ready",
			TargetProviderID: providerID, ModelID: modelID, ModeID: modeID, Permission: "read-only",
		}},
	})

	applySnapshotParityCommand(t, actor.engine, chat.Submit{
		OperationID: turnOperation, LaneID: identity.ID, Text: "user prompt 😀",
		Attachments: []providercontract.Attachment{image}, ModelID: modelID, ModeID: modeID, Permission: "ask",
		Presentation: providercontract.TurnPresentation{
			UserMessageID: "snapshot-user", AssistantMessageID: "snapshot-assistant",
			QueueID: "snapshot-turn-queue", PromptText: "user prompt 😀", Title: "Snapshot turn",
			Origin: "human", StartedAt: "2026-08-11T12:00:00Z",
		},
	})

	var sequence uint64
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind: providercontract.EventTurnAdmitted,
		Admission: &providercontract.TurnAdmission{
			Turn: providercontract.TurnRef{OperationID: turnOperation, NativeID: nativeTurnID}, Accepted: true,
		},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:  providercontract.EventInputConsumed,
		Input: &providercontract.InputEvent{OperationID: turnOperation, NativeTurnID: nativeTurnID},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:      providercontract.EventAssistantChunk,
		Assistant: &providercontract.AssistantEvent{Phase: providercontract.AssistantPhaseCommentary, Text: "commentary before steer 😀", TypedPhase: true},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:      providercontract.EventAssistantChunk,
		Assistant: &providercontract.AssistantEvent{Phase: providercontract.AssistantPhaseFinal, Text: "final before steer", TypedPhase: true},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:     providercontract.EventThinkingUpdate,
		Thinking: &providercontract.ThinkingEvent{Text: "thinking window"},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind: providercontract.EventToolUpdate,
		Tool: &providercontract.ToolEvent{
			ToolCallID: "snapshot-tool", ToolKind: "terminal", Title: "Run verification", Status: "running",
			Command: "go test ./cmd/workass", Input: "fixture input", Location: "cmd/workass",
			Attachments: []providercontract.Attachment{toolImage}, SubagentID: "snapshot-child",
			SubagentLabel: "Review child", SubagentProvider: "codex", SubagentModel: "Mock-high",
			SubagentHeader: true, StartedAtUnixMS: 1786440010100,
		},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind: providercontract.EventToolUpdate,
		Tool: &providercontract.ToolEvent{
			ToolCallID: "snapshot-tool", Status: "completed", Output: "verification output", TerminalID: "snapshot-terminal",
			EndedAtUnixMS: 1786440010200,
		},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind: providercontract.EventPlanUpdate,
		Plan: &providercontract.PlanEvent{Entries: []providercontract.PlanEntry{
			{ID: "plan-1", Text: "Inspect actor state", Status: "completed"},
			{ID: "plan-2", Text: "Verify replay", Status: "in_progress"},
		}},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:       providercontract.EventCompactionStarted,
		Compaction: &providercontract.CompactionEvent{CheckpointID: "snapshot-compaction", Coverage: 1, Digest: "snapshot-compaction-start"},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:       providercontract.EventCompactionCheckpoint,
		Compaction: &providercontract.CompactionEvent{CheckpointID: "snapshot-compaction", Coverage: 1, Digest: "snapshot-compaction-digest"},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:  providercontract.EventAssistantMedia,
		Media: &providercontract.AssistantMediaEvent{Attachments: []providercontract.Attachment{assistantImage}},
	})

	question := &providercontract.PermissionQuestion{
		Question: "Allow the verification?", Header: "Verification", MultiSelect: false,
		Options: []providercontract.PermissionQuestionOption{{Label: "Allow", Description: "Continue verification"}},
	}
	permissionOptions := []providercontract.PermissionOption{{ID: "allow", Name: "Allow", Kind: "allow"}, {ID: "deny", Name: "Deny", Kind: "deny"}}
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind: providercontract.EventPermissionRequested,
		Permission: &providercontract.PermissionEvent{
			RequestID: "snapshot-permission-resolved", Title: "Verification permission", Kind: "permission", Status: "pending",
			Options: []string{"allow", "deny"}, OptionDetails: permissionOptions, Question: question,
		},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind: providercontract.EventPermissionResolved,
		Permission: &providercontract.PermissionEvent{
			RequestID: "snapshot-permission-resolved", Title: "Verification permission", Kind: "permission", Status: "resolved",
			Options: []string{"allow", "deny"}, OptionDetails: permissionOptions, Question: question, ResolvedOptionID: "allow",
		},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind: providercontract.EventBackgroundWork,
		Background: &providercontract.BackgroundEvent{
			WorkID: "snapshot-child", TaskID: "snapshot-child-task", ToolCallID: "snapshot-tool", Title: "Review child",
			Kind: "subagent", Role: "agent", Status: "exited", StartedAt: "2026-08-11T12:00:05Z",
			UpdatedAt: "2026-08-11T12:00:15Z", FinishedAt: "2026-08-11T12:00:15Z", ExitCode: snapshotParityIntPointer(0),
			Summary: "child summary", OutputFile: "snapshot-child-output.txt", PID: snapshotParityIntPointer(4242),
			LastToolName: "Run verification", ModelLabel: "Mock-high", ResultExcerpt: "child result",
		},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:  providercontract.EventUsageUpdated,
		Usage: &providercontract.UsageEvent{Used: 321, Size: 1000, InputTokens: 123, OutputTokens: 198},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:   providercontract.EventTransportHealth,
		Health: &providercontract.TransportHealthEvent{State: "connected"},
	})

	applySnapshotParityCommand(t, actor.engine, chat.Steer{
		OperationID: steerOperation, Text: "continue with the changed direction",
		Attachments: []providercontract.Attachment{steerImage},
		Presentation: providercontract.TurnPresentation{
			UserMessageID: "snapshot-steer-user", AssistantMessageID: "snapshot-steer-assistant",
			QueueID: "snapshot-steer-queue", PromptText: "continue with the changed direction", Title: "Steer",
			Origin: "human", StartedAt: "2026-08-11T12:00:12Z",
		},
	})
	applySnapshotParityCommand(t, actor.engine, chat.SteerAdmitted{
		OperationID: steerOperation, Accepted: true, AwaitConsumption: true,
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, steerOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:  providercontract.EventInputConsumed,
		Input: &providercontract.InputEvent{OperationID: steerOperation, NativeTurnID: nativeTurnID},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:      providercontract.EventAssistantChunk,
		Assistant: &providercontract.AssistantEvent{Phase: providercontract.AssistantPhaseCommentary, Text: "continued commentary", TypedPhase: true},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:  providercontract.EventAssistantMedia,
		Media: &providercontract.AssistantMediaEvent{Attachments: []providercontract.Attachment{assistantImage}},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind:      providercontract.EventAssistantChunk,
		Assistant: &providercontract.AssistantEvent{Phase: providercontract.AssistantPhaseFinal, Text: "continued final", TypedPhase: true},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind: providercontract.EventTurnTerminal,
		Terminal: &providercontract.TerminalEvent{
			Status: "completed", StopReason: "finished", Result: "whole-turn fallback must not flatten typed phases",
			FinishedAt: "2026-08-11T12:00:20Z", DispositionState: "needs_input", DispositionSource: "provider",
			DispositionNote: "a later question remains", ConsumedSteerIDs: []providercontract.OperationID{steerOperation},
		},
	})
	applySnapshotParityEvent(t, actor.engine, chatID, identity.ID, turnOperation, nativeTurnID, &sequence, providercontract.Event{
		Kind: providercontract.EventPermissionRequested,
		Permission: &providercontract.PermissionEvent{
			RequestID: "snapshot-permission-pending", Title: "Follow-up question", Kind: "question", Status: "pending",
			Options: []string{"allow", "deny"}, OptionDetails: permissionOptions,
			Question: &providercontract.PermissionQuestion{
				Question: "Choose the follow-up action", Header: "Follow-up", MultiSelect: false,
				Options: []providercontract.PermissionQuestionOption{{Label: "Allow", Description: "Allow the follow-up"}, {Label: "Deny", Description: "Stop the follow-up"}},
			},
		},
	})

	checkpoints := []acp.ChatCheckpoint{{
		TurnSeq: 2, JobID: string(nativeTurnID), Ts: "2026-08-11T12:00:20Z",
		Repos: []acp.ChatCheckpointRepo{{
			Name: "fixture", Path: workspace, Branch: "main", Ref: baselineCommit, Commit: baselineCommit,
			ObservedTree: baselineCommit, ChangedFiles: 1,
		}},
	}}
	environmentPayload := acp.ChatEnvPayload{
		ChatID: chatID, TabID: tabID, CWD: workspace,
		Repos: []acp.ChatEnvRepo{{
			Name: "fixture", Branch: "main", Adds: 1, Dels: 1,
			Files: []acp.ChatEnvFile{{Path: "work.txt", Adds: 1, Dels: 1}},
		}},
		Unchanged: []string{"README.md"}, RepoLimit: 12, FileLimit: 200,
		Approximation: "actor fixture diff metadata",
	}
	environmentRaw, err := json.Marshal(environmentPayload)
	if err != nil {
		t.Fatalf("marshal actor environment payload: %v", err)
	}
	checkpointsRaw, err := json.Marshal(checkpoints)
	if err != nil {
		t.Fatalf("marshal actor checkpoints: %v", err)
	}
	referenceRaw, err := json.Marshal(map[string]any{
		"version": 1, "chatId": chatID, "tabId": tabID, "cwd": workspace,
		"payload": environmentPayload, "reposTruncated": false, "turnSeq": 2, "repos": []any{},
	})
	if err != nil {
		t.Fatalf("marshal actor environment reference: %v", err)
	}
	applySnapshotParityCommand(t, actor.engine, chat.UpdateEnvironment{
		ExpectedTabID: tabID, CWD: workspace, Payload: environmentRaw, Checkpoints: checkpointsRaw, Reference: referenceRaw,
	})

	state := actor.engine.Snapshot()
	if err := state.Validate(); err != nil {
		t.Fatalf("rich file-backed actor state is invalid: %v", err)
	}
	if state.Lanes[identity.ID].LastEventSequence != sequence {
		t.Fatalf("actor lost provider sequence: got %d want %d", state.Lanes[identity.ID].LastEventSequence, sequence)
	}

	firstRoot, firstChat, firstArchive := snapshotParityProjection(t, runtime, tabID)
	snapshotParityAssertRichRendererContract(t, runtime, firstRoot, firstChat, firstArchive, tabID, chatID, identity, thread, sequence, workspace, baselineCommit, ownerKey)
	firstSemanticBytes := snapshotParitySemanticBytes(t, firstRoot)
	firstMessageBytes := snapshotParityJSONBytes(t, anySlice(firstChat["messages"]))

	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close first runtime: %v", err)
	}
	if !manager.Reset() {
		t.Fatal("reset first manager")
	}

	for _, scenario := range []struct {
		name  string
		chats []any
	}{
		{
			name: "forged legacy chat rows",
			chats: []any{
				map[string]any{
					"id": tabID, "chatId": chatID, "title": "FORGED TITLE", "messages": []any{
						map[string]any{"id": "forged-message", "role": "assistant", "content": "forged content"},
					},
				},
				map[string]any{"id": "ghost-tab", "chatId": "ghost-chat", "title": "ghost"},
			},
		},
		{name: "empty legacy chat rows", chats: []any{}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			legacy := mapFromAnyMain(cloneJSON(firstRoot))
			legacy["chats"] = scenario.chats
			writeLegacySessionSnapshot(t, stateDir, legacy)

			restartedManager := acp.NewManager(acp.Options{
				RootDir: workspace, StateDir: stateDir, RuntimeProfile: "dev",
			})
			restartedStore := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
			restarted := newProviderChatRuntimeBeforeProviderStartup(restartedManager, restartedStore, stateDir)
			if err := restarted.StartupError(); err != nil {
				t.Fatalf("restart runtime with %s: %v", scenario.name, err)
			}

			root, projectedChat, archive := snapshotParityProjection(t, restarted, tabID)
			if got := len(anySlice(root["chats"])); got != 1 {
				t.Fatalf("legacy %s changed actor chat inventory: %d chats", scenario.name, got)
			}
			if !reflect.DeepEqual(anySlice(projectedChat["messages"]), archive) {
				t.Fatalf("legacy %s made archive and session message projections diverge", scenario.name)
			}
			if !bytes.Equal(snapshotParitySemanticBytes(t, root), firstSemanticBytes) {
				t.Fatalf("legacy %s changed the actor-derived session semantic bytes", scenario.name)
			}
			if !bytes.Equal(snapshotParityJSONBytes(t, anySlice(projectedChat["messages"])), firstMessageBytes) {
				t.Fatalf("legacy %s changed actor message bytes", scenario.name)
			}
			snapshotParityAssertRichRendererContract(t, restarted, root, projectedChat, archive, tabID, chatID, identity, thread, sequence, workspace, baselineCommit, ownerKey)

			if err := restarted.Close(context.Background()); err != nil {
				t.Fatalf("close restarted runtime: %v", err)
			}
			if !restartedManager.Reset() {
				t.Fatal("reset restarted manager")
			}
		})
	}
}

func snapshotParityPresentation(workspace string, providerID providercontract.ID, modelID, modeID string) chat.PresentationState {
	group, cwd, pane := "snapshot-group", workspace, "rail"
	return chat.PresentationState{
		TabID: "snapshot-parity-tab", Title: "Actor snapshot parity", TitleLocked: true,
		Group: &group, CWD: &cwd, Draft: "draft before actor save", Unread: true, Settled: "active", Pane: &pane,
		ProviderID: providerID, CurrentModelID: modelID, CurrentModeID: modeID, WorkspaceRevision: 4,
		ModelControls:          json.RawMessage(`{"mock":{"mock-model":{"effort":"high","modeId":"plan"}}}`),
		ContextUsageByProvider: json.RawMessage(`{"legacy":{"used":99,"size":100}}`),
		LegacyUsage:            json.RawMessage(`{"used":1,"size":2}`),
	}
}

func snapshotParityAttachment(t *testing.T, stateDir, id, name string) providercontract.Attachment {
	t.Helper()
	data := []byte("actor-owned image bytes")
	digest := sha256.Sum256(data)
	hexDigest := hex.EncodeToString(digest[:])
	imageDir := filepath.Join(stateDir, sessionImageDirname)
	if err := os.MkdirAll(imageDir, 0o700); err != nil {
		t.Fatalf("create actor image directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, hexDigest), data, 0o600); err != nil {
		t.Fatalf("write actor image sidecar: %v", err)
	}
	return providercontract.Attachment{
		ID: id, Name: name, MIMEType: "image/png", Digest: hexDigest, Size: int64(len(data)),
		Ref: providerSessionImageRefPrefix + sessionImageDirname + "/" + hexDigest,
	}
}

func applySnapshotParityCommand(t *testing.T, engine *chat.Engine, command chat.Command) {
	t.Helper()
	if err := engine.Apply(command); err != nil {
		t.Fatalf("apply actor command %T: %v", command, err)
	}
}

func applySnapshotParityEvent(
	t *testing.T, engine *chat.Engine, chatID string, laneID providercontract.LaneID,
	operationID providercontract.OperationID, turnID string, sequence *uint64, event providercontract.Event,
) {
	t.Helper()
	*sequence++
	event.Identity = providercontract.EventIdentity{
		ChatID: chatID, LaneID: laneID, OperationID: operationID, TurnID: turnID,
		Sequence: *sequence, ObservedAtUnixMS: 1786440010000 + int64(*sequence)*1000,
	}
	if err := engine.Apply(chat.ProviderEventReceived{ConnectionGeneration: 1, Event: event}); err != nil {
		t.Fatalf("apply provider event %d (%s): %v", *sequence, event.Kind, err)
	}
}

func snapshotParityIntPointer(value int) *int { return &value }

func snapshotParityProjection(t *testing.T, runtime *providerChatRuntime, tabID string) (map[string]any, map[string]any, []any) {
	t.Helper()
	root, err := runtime.ProjectSession()
	if err != nil {
		t.Fatalf("project actor session: %v", err)
	}
	projectedChat := chatFromSnapshot(root, tabID)
	if projectedChat == nil {
		t.Fatalf("projected actor chat %q missing: %#v", tabID, root)
	}
	archive, found, err := runtime.ProjectArchiveByTab(tabID)
	if err != nil || !found {
		t.Fatalf("project actor archive: found=%v err=%v", found, err)
	}
	return root, projectedChat, archive
}

func snapshotParityAssertRichRendererContract(
	t *testing.T, runtime *providerChatRuntime, root map[string]any, projectedChat map[string]any, archive []any,
	tabID, chatID string, identity providercontract.LaneIdentity, thread providercontract.ThreadRef,
	sequence uint64, workspace, baselineCommit, ownerKey string,
) {
	t.Helper()
	messages := anySlice(projectedChat["messages"])
	if len(messages) != 4 {
		t.Fatalf("chronological actor messages = %#v", messages)
	}
	roles := make([]string, 0, len(messages))
	for _, raw := range messages {
		roles = append(roles, fieldString(mapFromAnyMain(raw), "role"))
	}
	if !reflect.DeepEqual(roles, []string{"user", "assistant", "user", "assistant"}) {
		t.Fatalf("user/commentary/final chronology roles = %#v", roles)
	}
	user := mapFromAnyMain(messages[0])
	rootAssistant := mapFromAnyMain(messages[1])
	steerUser := mapFromAnyMain(messages[2])
	continuedAssistant := mapFromAnyMain(messages[3])
	if fieldString(user, "id") != "snapshot-user" || fieldString(user, "content") != "user prompt 😀" {
		t.Fatalf("user semantic row = %#v", user)
	}
	if fieldString(rootAssistant, "content") != "commentary before steer 😀" || fieldString(rootAssistant, "result") != "final before steer" ||
		fieldString(rootAssistant, "turnRootId") != "snapshot-assistant" || rootAssistant["turnTerminal"] != false {
		t.Fatalf("typed commentary/final root row = %#v", rootAssistant)
	}
	if fieldString(steerUser, "id") != "snapshot-steer-user" || fieldString(steerUser, "steerState") != "applied" ||
		fieldString(steerUser, "turnRootId") != "snapshot-assistant" || fieldString(steerUser, "content") != "continue with the changed direction" {
		t.Fatalf("steering chronology row = %#v", steerUser)
	}
	if fieldString(continuedAssistant, "id") != "snapshot-steer-assistant" ||
		fieldString(continuedAssistant, "content") != "continued commentary" || fieldString(continuedAssistant, "result") != "continued final" ||
		fieldString(continuedAssistant, "turnRootId") != "snapshot-assistant" || continuedAssistant["turnTerminal"] != true {
		t.Fatalf("typed continuation row = %#v", continuedAssistant)
	}
	if len(anySlice(user["images"])) != 1 || fieldString(mapFromAnyMain(anySlice(user["images"])[0]), "data") != "actor-owned image bytes" {
		t.Fatalf("user image content reference was not materialized: %#v", user["images"])
	}
	if len(anySlice(rootAssistant["images"])) != 1 || fieldString(mapFromAnyMain(anySlice(rootAssistant["images"])[0]), "data") != "actor-owned image bytes" {
		t.Fatalf("assistant image content reference was not materialized: %#v", rootAssistant["images"])
	}
	if len(anySlice(steerUser["images"])) != 1 || fieldString(mapFromAnyMain(anySlice(steerUser["images"])[0]), "data") != "actor-owned image bytes" {
		t.Fatalf("steer image content reference was not materialized: %#v", steerUser["images"])
	}

	events := anySlice(rootAssistant["events"])
	if len(events) != 4 {
		t.Fatalf("rich assistant timeline = %#v", events)
	}
	if fieldString(mapFromAnyMain(events[0]), "kind") != "thinking" || fieldString(mapFromAnyMain(events[0]), "text") != "thinking window" {
		t.Fatalf("thinking timeline = %#v", events[0])
	}
	tool := mapFromAnyMain(events[1])
	if fieldString(tool, "kind") != "tool" || fieldString(tool, "status") != "completed" ||
		fieldString(tool, "output") != "verification output" || intValue(tool["startedAt"]) != 1786440010100 ||
		intValue(tool["endedAt"]) != 1786440010200 || tool["subagentHeader"] != true ||
		fieldString(tool, "subagentId") != "snapshot-child" || len(anySlice(tool["images"])) != 1 ||
		fieldString(mapFromAnyMain(anySlice(tool["images"])[0]), "data") != "actor-owned image bytes" {
		t.Fatalf("tool timing/image/subagent projection = %#v", tool)
	}
	plan := mapFromAnyMain(events[2])
	if fieldString(plan, "kind") != "plan" || len(anySlice(plan["entries"])) != 2 {
		t.Fatalf("plan timeline = %#v", plan)
	}
	if fieldString(mapFromAnyMain(events[3]), "kind") != "compaction" {
		t.Fatalf("compaction timeline = %#v", events[3])
	}

	queue := anySlice(projectedChat["queue"])
	if len(queue) != 1 {
		t.Fatalf("actor queue projection = %#v", queue)
	}
	queueRow := mapFromAnyMain(queue[0])
	if fieldString(queueRow, "id") != "snapshot-staged-follow-up" || fieldString(queueRow, "source") != "agent" ||
		fieldString(queueRow, "delivery") != "queue" || fieldString(queueRow, "attachmentState") != "ready" ||
		fieldString(mapFromAnyMain(anySlice(queueRow["images"])[0]), "data") != "actor-owned image bytes" {
		t.Fatalf("queue semantic contract = %#v", queueRow)
	}

	if fieldString(projectedChat, "title") != "Actor snapshot parity" || projectedChat["titleLocked"] != true ||
		fieldString(projectedChat, "group") != "snapshot-group" || fieldString(projectedChat, "cwd") != workspace ||
		fieldString(projectedChat, "draft") != "draft persisted by actor" || projectedChat["unread"] != true ||
		fieldString(projectedChat, "settled") != "active" || fieldString(projectedChat, "pane") != "rail" ||
		intValue(projectedChat[workspaceRevisionField]) != 4 || intValue(projectedChat[presentationRevisionField]) != 1 ||
		intValue(projectedChat[agentQueueRevisionField]) != 2 {
		state, _ := runtime.Snapshot(chatID)
		t.Fatalf("presentation/control projection = %#v actor staged=%#v queue=%#v", projectedChat, state.StagedQueue, state.Queue)
	}
	if _, present := projectedChat[runtimeControlRevisionField]; present {
		t.Fatalf("zero runtime-control revision should be omitted from the canonical projection: %#v", projectedChat)
	}
	modelControls := mapFromAnyMain(projectedChat["modelControls"])
	mockControls := mapFromAnyMain(modelControls["mock"])
	if fieldString(mapFromAnyMain(mockControls["mock-model"]), "effort") != "high" {
		t.Fatalf("model controls projection = %#v", projectedChat["modelControls"])
	}
	usage := mapFromAnyMain(projectedChat["usage"])
	if intValue(usage["used"]) != 321 || intValue(usage["size"]) != 1000 || len(mapFromAnyMain(projectedChat["contextUsageByProvider"])) != 2 {
		t.Fatalf("usage projection = %#v", projectedChat)
	}
	planLatest := anySlice(projectedChat["planLatest"])
	if len(planLatest) != 2 || fieldString(projectedChat, "planLatestMessageId") != "snapshot-assistant" {
		t.Fatalf("plan latest projection = %#v", projectedChat)
	}
	if projectedChat["pending"] != false || !reflect.DeepEqual(messages, archive) {
		t.Fatalf("session/archive actor projection mismatch: pending=%v archive=%#v messages=%#v", projectedChat["pending"], archive, messages)
	}

	liveSession := mapFromAnyMain(projectedChat["liveSession"])
	if fieldString(projectedChat, "sessionId") != thread.HeadID || fieldString(projectedChat, "sessionProviderId") != string(identity.Realm.ProviderID) ||
		fieldString(liveSession, "sessionId") != "snapshot-connection" || fieldString(liveSession, "providerName") != "Mock ACP" ||
		liveSession["imageSupport"] != true || liveSession["commandCatalogSupported"] != true {
		t.Fatalf("lane attachment projection = %#v", projectedChat)
	}
	catalog := mapFromAnyMain(liveSession["commandCatalog"])
	commands := anySlice(catalog["commands"])
	if len(commands) != 1 || fieldString(mapFromAnyMain(commands[0]), "name") != "/review" ||
		fieldString(mapFromAnyMain(commands[0]), "argumentHint") != "[path]" || intValue(catalog["commandsTruncated"]) != 1 {
		t.Fatalf("command catalog projection = %#v", catalog)
	}

	permissions, err := runtime.PendingPermissions()
	if err != nil || len(permissions) != 1 {
		t.Fatalf("pending permission replay state = %#v err=%v", permissions, err)
	}
	pendingPermission := mapFromAnyMain(permissions[0])
	if fieldString(pendingPermission, "id") != "snapshot-permission-pending" ||
		fieldString(mapFromAnyMain(pendingPermission["question"]), "question") != "Choose the follow-up action" ||
		fieldString(pendingPermission, "chatId") != chatID || fieldString(pendingPermission, "tabId") != tabID {
		t.Fatalf("pending question projection = %#v", pendingPermission)
	}
	var replayed []any
	if err := runtime.ReplayPendingPermissions(func(channel string, payload any) error {
		if channel != "chat:permission-request" {
			return fmt.Errorf("unexpected replay channel %q", channel)
		}
		replayed = append(replayed, payload)
		return nil
	}); err != nil || len(replayed) != 1 {
		t.Fatalf("pending permission reconnect replay = %#v err=%v", replayed, err)
	}

	background, err := runtime.ListBackground(tabID, chatID)
	if err != nil || len(background) != 1 {
		t.Fatalf("background actor projection = %#v err=%v", background, err)
	}
	backgroundRow := background[0]
	if fieldString(backgroundRow, "id") != "snapshot-child" || fieldString(backgroundRow, "kind") != "subagent" ||
		fieldString(backgroundRow, "status") != "exited" || fieldString(backgroundRow, "modelLabel") != "Mock-high" ||
		fieldString(backgroundRow, "resultExcerpt") != "child result" || fieldString(backgroundRow, "providerId") != string(identity.Realm.ProviderID) {
		t.Fatalf("background/subagent metadata = %#v", backgroundRow)
	}
	obligation, err := runtime.Obligation(tabID, chatID)
	if err != nil || obligation == nil || obligation.State != "needs_input" || obligation.PromptID != "snapshot-user" {
		t.Fatalf("actor obligation projection = %#v err=%v", obligation, err)
	}

	env, err := runtime.ChatEnvGet(tabID, chatID)
	if err != nil || env.ChatID != chatID || env.TabID != tabID || len(env.Repos) != 1 || len(env.Repos[0].Files) != 1 || env.Repos[0].Files[0].Adds != 1 {
		t.Fatalf("actor environment projection = %#v err=%v", env, err)
	}
	projectedCheckpoints, err := runtime.ChatCheckpoints(tabID, chatID)
	if err != nil || len(projectedCheckpoints) != 1 || projectedCheckpoints[0].Repos[0].Ref != baselineCommit {
		t.Fatalf("actor checkpoint projection = %#v err=%v", projectedCheckpoints, err)
	}
	diff, err := runtime.ChatDiff(context.Background(), tabID, chatID, "fixture", "work.txt")
	if err != nil || fieldString(diff, "chatId") != chatID || intValue(diff["turnSeq"]) != 2 ||
		fieldString(diff, "repo") != "fixture" || fieldString(diff, "path") != "work.txt" || !strings.Contains(fieldString(diff, "text"), "+after") {
		t.Fatalf("actor diff projection = %#v err=%v", diff, err)
	}

	state, ok := runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("actor snapshot disappeared after reconnect")
	}
	lane := state.Lanes[identity.ID]
	if lane.Identity != identity || !lane.Thread.Equal(thread) || lane.ConnectionGeneration != 1 || lane.CoveredThrough != uint64(len(state.Ledger)) ||
		lane.LastEventSequence != sequence || len(lane.Coverage) != len(state.Ledger) || lane.Coverage[1].Status != chat.CoverageNativeSeen {
		t.Fatalf("lane identity/coverage projection = %#v", lane)
	}
	// Tools is the provider-ownership index; the merged renderer-visible
	// lifecycle is asserted above from the assistant timeline.
	toolState, toolOK := state.Tools["snapshot-tool"]
	if !toolOK || toolState.Owner.OperationID != providercontract.OperationID("snapshot-turn") || toolState.Event.Status != "completed" ||
		toolState.Event.Output != "verification output" || toolState.Event.EndedAtUnixMS != 1786440010200 || len(state.Plans) != 1 ||
		state.Compactions[identity.ID].Event.Digest != "snapshot-compaction-digest" || state.Transport[identity.ID].State != "connected" {
		t.Fatalf("actor rich side indexes = tools:%#v plans:%#v compactions:%#v transport:%#v", state.Tools, state.Plans, state.Compactions, state.Transport)
	}
	resolved, ok := state.Permissions["snapshot-permission-resolved"]
	if !ok || resolved.Event.Status != "resolved" || resolved.Event.ResolvedOptionID != "allow" {
		t.Fatalf("resolved permission actor state = %#v", state.Permissions)
	}
	if sequence != 22 {
		t.Fatalf("fixture event count changed unexpectedly: got %d want 22", sequence)
	}
	if fieldString(root, "activeId") != tabID || fieldString(projectedChat, "chatId") != chatID {
		t.Fatalf("root/chat identity projection = root:%#v chat:%#v", root, projectedChat)
	}
	if fieldString(projectedChat, "sessionError") != "" {
		t.Fatalf("healthy actor lane projected an error: %#v", projectedChat)
	}
	_ = ownerKey // The lane owner is checked below from actor state, not a mirror.
	if lane.Owner.AgentOwnerKey != ownerKey || lane.Owner.TabID != tabID {
		t.Fatalf("lane attachment owner = %#v", lane.Owner)
	}
}

func snapshotParitySemanticBytes(t *testing.T, root map[string]any) []byte {
	t.Helper()
	clone := mapFromAnyMain(cloneJSON(root))
	for _, raw := range anySlice(clone["chats"]) {
		delete(mapFromAnyMain(raw), "actorRevision")
	}
	return snapshotParityJSONBytes(t, clone)
}

func snapshotParityJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal parity value: %v", err)
	}
	return raw
}

func snapshotParityGitFixture(t *testing.T, workspace string) string {
	t.Helper()
	snapshotParityGit(t, workspace, "init", "--quiet")
	snapshotParityGit(t, workspace, "config", "user.email", "snapshot@example.invalid")
	snapshotParityGit(t, workspace, "config", "user.name", "Snapshot Fixture")
	if err := os.WriteFile(filepath.Join(workspace, "work.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatalf("write git fixture: %v", err)
	}
	snapshotParityGit(t, workspace, "add", "work.txt")
	snapshotParityGit(t, workspace, "commit", "--quiet", "-m", "baseline")
	commit := snapshotParityGit(t, workspace, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workspace, "work.txt"), []byte("after\n"), 0o600); err != nil {
		t.Fatalf("modify git fixture: %v", err)
	}
	return commit
}

func snapshotParityGit(t *testing.T, workspace string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", workspace}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}
