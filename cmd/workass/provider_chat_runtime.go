package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// providerChatRuntime is the one command boundary shared by renderer, LAN,
// mobile, and agent-control starts. It owns one durable actor per immutable
// ChatID; tab ids remain disposable attachment metadata.
type providerChatRuntime struct {
	manager  *acp.Manager
	sessions *sessionStore
	stateDir string
	publish  func(string, any)

	mu      sync.Mutex
	actors  map[string]*providerChatActor
	known   map[string]struct{}
	bootErr error
	// actorsPaused is used only by the daemon's staged startup path. Durable
	// actor construction and migration may happen before the network listener,
	// but no coordinator may resume a provider effect until that listener is
	// accepting MCP connections.
	actorsPaused bool
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

type providerChatActor struct {
	mu          sync.Mutex
	engine      *chat.Engine
	coordinator *chat.Coordinator
}

func newProviderChatRuntime(manager *acp.Manager, sessions *sessionStore, stateDir string, publishers ...func(string, any)) *providerChatRuntime {
	return newProviderChatRuntimeWithStartupMode(manager, sessions, stateDir, false, publishers...)
}

func newProviderChatRuntimeBeforeProviderStartup(manager *acp.Manager, sessions *sessionStore, stateDir string, publishers ...func(string, any)) *providerChatRuntime {
	return newProviderChatRuntimeWithStartupMode(manager, sessions, stateDir, true, publishers...)
}

func newProviderChatRuntimeWithStartupMode(manager *acp.Manager, sessions *sessionStore, stateDir string, actorsPaused bool, publishers ...func(string, any)) *providerChatRuntime {
	stateDir = strings.TrimSpace(stateDir)
	runtime := &providerChatRuntime{
		manager: manager, sessions: sessions, stateDir: stateDir,
		actors: make(map[string]*providerChatActor), known: make(map[string]struct{}), actorsPaused: actorsPaused,
	}
	if len(publishers) > 0 {
		runtime.publish = publishers[0]
	}
	if manager == nil || sessions == nil || !sessions.enabled() || stateDir == "" {
		runtime.bootErr = errors.New("provider chat runtime requires manager, durable state, and state directory")
		return runtime
	}
	states, err := chat.DiscoverFileStates(filepath.Join(stateDir, "provider-chats"))
	if err != nil {
		runtime.bootErr = err
	} else {
		for _, state := range states {
			// FileStore may contain a schema-upgraded empty envelope from a
			// chat:create attempt that never committed InitializeChat. It has no
			// accepted chat identity and is not part of the immutable cutover
			// inventory, so discovery must not turn it into a runtime chat. A
			// referenced interrupted-cutover actor is repaired separately from
			// the receipt; any nonempty pre-initialization state still fails
			// closed through actor construction.
			if unacceptedEmptyActorState(state) {
				continue
			}
			runtime.known[state.ChatID] = struct{}{}
		}
	}
	manager.SetProviderAttachmentResolver(sessions.ResolveProviderAttachment)
	manager.SetProviderAttachmentPersister(sessions.PersistProviderAttachments)
	// Actor-managed Entorno snapshots must be committed by the chat actor before
	// the frozen chat:env event is published. The observer is installed before
	// any resumed lane can execute a provider effect.
	manager.SetChatEnvObserver(runtime.observeChatEnv)
	manager.SetChatEnvRestorer(runtime.restoreActorChatEnvReference)
	if runtime.bootErr == nil {
		runtime.bootErr = runtime.completeLegacyChatCutover()
	}
	if runtime.bootErr == nil {
		// Install semantic ingress before any resumed lane or manager recovery can
		// publish background executor evidence. A nil-observer window would make
		// the runtime record visible without a durable actor commit.
		if err := manager.InstallSpawnedWorkObserver(runtime.applySpawnedWorkSnapshot); err != nil {
			runtime.bootErr = err
		} else if !actorsPaused {
			runtime.bootErr = runtime.ResumeActors()
		}
	}
	if runtime.bootErr == nil {
		runtime.bootErr = runtime.syncSpawnedWorkSnapshots()
	}
	if runtime.bootErr == nil {
		runtime.bootErr = runtime.reconcileObligations(true)
	}
	if runtime.bootErr == nil {
		ctx, cancel := context.WithCancel(context.Background())
		runtime.cancel = cancel
		runtime.wg.Add(1)
		go runtime.runObligationReconciliation(ctx)
	}
	return runtime
}

func unacceptedEmptyActorState(state chat.State) bool {
	if state.Initialized || state.Deleted ||
		state.CreationOperationID != "" || strings.TrimSpace(state.CreationDigest) != "" || state.DeletionOperationID != "" ||
		state.ContextFloor != 0 || state.ActiveLaneID != "" || state.DesiredLaneID != "" ||
		state.Foreground != nil || state.PendingSteer != nil || state.PendingCancel != nil || state.Obligation != nil ||
		len(state.Ledger) != 0 || len(state.Lanes) != 0 || len(state.StagedQueue) != 0 || len(state.Queue) != 0 ||
		len(state.QueueMutationReceipts) != 0 || len(state.PresentationMutationReceipts) != 0 || len(state.RuntimeControlMutationReceipts) != 0 ||
		len(state.WorkspaceMutationReceipts) != 0 || len(state.LaneSelectionMutationReceipts) != 0 || len(state.CancelMutationReceipts) != 0 ||
		len(state.AgentWaitObservationReceipts) != 0 || len(state.Operations) != 0 || len(state.Outbox) != 0 || len(state.Tools) != 0 ||
		len(state.Plans) != 0 || len(state.Permissions) != 0 || len(state.Background) != 0 || len(state.Usage) != 0 ||
		len(state.Compactions) != 0 || len(state.Transport) != 0 || state.Migration != (chat.MigrationState{}) {
		return false
	}
	presentation := state.Presentation.Clone()
	if !emptyActorRawJSON(presentation.ModelControls) || !emptyActorRawJSON(presentation.ContextUsageByProvider) ||
		!emptyActorRawJSON(presentation.LegacyUsage) || len(presentation.PlanLatest) != 0 {
		return false
	}
	presentation.ModelControls = nil
	presentation.ContextUsageByProvider = nil
	presentation.LegacyUsage = nil
	presentation.PlanLatest = nil
	environment := state.Environment.Clone()
	if !emptyActorRawJSON(environment.Payload) || !emptyActorRawJSON(environment.Checkpoints) || !emptyActorRawJSON(environment.Reference) {
		return false
	}
	environment.Payload = nil
	environment.Checkpoints = nil
	environment.Reference = nil
	return reflect.DeepEqual(presentation, chat.PresentationState{}) && reflect.DeepEqual(environment, chat.EnvironmentState{})
}

func emptyActorRawJSON(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value == "" || value == "null"
}

func (r *providerChatRuntime) StartupError() error {
	if r == nil {
		return errors.New("provider chat runtime was not constructed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bootErr
}

func providerChatStatePath(stateDir, chatID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(chatID)))
	return filepath.Join(stateDir, "provider-chats", hex.EncodeToString(digest[:])+".json")
}

func (r *providerChatRuntime) actor(chatID string) (*providerChatActor, error) {
	return r.actorFromSource(chatID, nil, nil)
}

func (r *providerChatRuntime) actorFromLegacy(chatID string, legacy map[string]any) (*providerChatActor, error) {
	return r.actorFromSource(chatID, legacy, nil)
}

func (r *providerChatRuntime) actorForNewChat(chatID string, presentation chat.PresentationState) (*providerChatActor, error) {
	operationID := providercontract.OperationID("chat-create:" + strings.TrimSpace(chatID))
	return r.actorForNewChatOperation(chatID, presentation, operationID, false)
}

func (r *providerChatRuntime) actorForNewChatOperation(chatID string, presentation chat.PresentationState, operationID providercontract.OperationID, focus bool) (*providerChatActor, error) {
	digest, err := chatCreationDigest(presentation, focus)
	if err != nil {
		return nil, err
	}
	command := chat.InitializeChat{Presentation: presentation, OperationID: operationID, Digest: digest}
	return r.actorFromSource(chatID, nil, &command)
}

func (r *providerChatRuntime) actorForFork(chatID string, command chat.InitializeFork) (*providerChatActor, error) {
	if r == nil || r.manager == nil || r.sessions == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil, errors.New("fork requires an immutable child chat id")
	}
	r.mu.Lock()
	if r.bootErr != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("discover provider chat actors: %w", r.bootErr)
	}
	actor, loaded := r.actors[chatID]
	_, known := r.known[chatID]
	if loaded || known {
		r.mu.Unlock()
		if actor == nil {
			var err error
			actor, err = r.actor(chatID)
			if err != nil {
				return nil, err
			}
		}
		state := actor.engine.Snapshot()
		operationID := providercontract.NormalizeOperationID(string(command.OperationID))
		if !state.Initialized || state.Deleted || state.CreationOperationID != operationID || state.CreationDigest != strings.TrimSpace(command.Digest) {
			return nil, errors.New("fork child chat already exists with a different creation receipt")
		}
		return actor, nil
	}
	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(r.stateDir, chatID)})
	if err != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("open fork chat actor: %w", err)
	}
	if engine.Snapshot().Initialized {
		r.mu.Unlock()
		return nil, errors.New("fork child actor was already initialized")
	}
	if err := engine.Apply(command); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	coordinator, err := chat.NewCoordinator(engine, r.manager)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if err := r.configureCoordinator(coordinator); err != nil {
		r.mu.Unlock()
		_ = coordinator.Close(context.Background())
		return nil, err
	}
	actor = &providerChatActor{engine: engine, coordinator: coordinator}
	r.actors[chatID] = actor
	r.known[chatID] = struct{}{}
	r.mu.Unlock()
	r.wakeCoordinatorIfStarted(coordinator)
	return actor, nil
}

// existingForkActor is the durable child-creation readback boundary. Once the
// child actor's creation operation and digest are committed, the child is the
// only authority needed to retry the fork; the source actor is deliberately
// not consulted here. This keeps a lost fork reply from depending on a source
// that was later deleted or whose presentation/ledger has advanced.
func (r *providerChatRuntime) existingForkActor(chatID string, operationID providercontract.OperationID, digest string) (*providerChatActor, bool, error) {
	if r == nil {
		return nil, false, errors.New("provider chat runtime is unavailable")
	}
	chatID = strings.TrimSpace(chatID)
	operationID = providercontract.NormalizeOperationID(string(operationID))
	digest = strings.TrimSpace(digest)
	r.mu.Lock()
	if r.bootErr != nil {
		err := fmt.Errorf("discover provider chat actors: %w", r.bootErr)
		r.mu.Unlock()
		return nil, false, err
	}
	actor := r.actors[chatID]
	_, known := r.known[chatID]
	r.mu.Unlock()
	if actor == nil && !known {
		return nil, false, nil
	}
	if actor == nil {
		var err error
		actor, err = r.actor(chatID)
		if err != nil {
			return nil, true, err
		}
	}
	state := actor.engine.Snapshot()
	if !state.Initialized || state.Deleted || state.CreationOperationID != operationID || state.CreationDigest != digest {
		return nil, true, errors.New("fork child chat already exists with a different creation receipt")
	}
	return actor, true, nil
}

func (r *providerChatRuntime) actorFromSource(chatID string, legacy map[string]any, initialize *chat.InitializeChat) (*providerChatActor, error) {
	if r == nil || r.manager == nil || r.sessions == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil, errors.New("provider chat runtime requires an immutable chat id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bootErr != nil {
		return nil, fmt.Errorf("discover provider chat actors: %w", r.bootErr)
	}
	if actor := r.actors[chatID]; actor != nil {
		state := actor.engine.Snapshot()
		if state.Deleted && (legacy != nil || initialize != nil) {
			return nil, errors.New("chat was durably deleted")
		}
		if initialize != nil {
			if state.CreationOperationID != providercontract.NormalizeOperationID(string(initialize.OperationID)) || state.CreationDigest != strings.TrimSpace(initialize.Digest) {
				return nil, errors.New("chat creation operation conflicts with the existing actor")
			}
		}
		return actor, nil
	}
	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(r.stateDir, chatID)})
	if err != nil {
		return nil, fmt.Errorf("open provider chat actor: %w", err)
	}
	if legacy != nil {
		engine, err = r.reconcileUncommittedLegacyActor(chatID, engine, legacy)
		if err != nil {
			return nil, err
		}
	}
	state := engine.Snapshot()
	if state.Deleted && (legacy != nil || initialize != nil) {
		return nil, errors.New("chat was durably deleted")
	}
	switch {
	case legacy != nil:
		// Before the global cutover receipt, replaying the exact migration source
		// is an idempotency check. A changed source conflicts and fails closed.
		err = r.migrateLegacyChatFromSource(chatID, engine, legacy)
	case !state.Initialized && initialize != nil:
		command := *initialize
		command.Presentation = initialize.Presentation.Clone()
		err = engine.Apply(command)
	case state.Initialized && initialize != nil:
		if state.CreationOperationID != providercontract.NormalizeOperationID(string(initialize.OperationID)) || state.CreationDigest != strings.TrimSpace(initialize.Digest) {
			err = errors.New("chat creation operation conflicts with the durable actor")
		}
	case !state.Initialized:
		err = errActorChatNotFound
	}
	if err != nil {
		return nil, err
	}
	coordinator, err := chat.NewCoordinator(engine, r.manager)
	if err != nil {
		return nil, err
	}
	if err := r.configureCoordinator(coordinator); err != nil {
		_ = coordinator.Close(context.Background())
		return nil, err
	}
	actor := &providerChatActor{engine: engine, coordinator: coordinator}
	r.actors[chatID] = actor
	r.known[chatID] = struct{}{}
	if !r.actorsPaused {
		coordinator.Wake()
	}
	return actor, nil
}

func (r *providerChatRuntime) wakeCoordinatorIfStarted(coordinator *chat.Coordinator) {
	if r == nil || coordinator == nil {
		return
	}
	r.mu.Lock()
	paused := r.actorsPaused
	r.mu.Unlock()
	if !paused {
		coordinator.Wake()
	}
}

func chatCreationDigest(presentation chat.PresentationState, focus bool) (string, error) {
	payload := struct {
		Presentation chat.PresentationState `json:"presentation"`
		Focus        bool                   `json:"focus"`
	}{Presentation: presentation.Clone(), Focus: focus}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%x", digest[:]), nil
}

func (r *providerChatRuntime) configureCoordinator(coordinator *chat.Coordinator) error {
	if r == nil || r.manager == nil {
		return errors.New("provider chat effect executor is unavailable")
	}
	if err := coordinator.SetChatCleanup(func(ctx context.Context, tabID, chatID string, operationID providercontract.OperationID) error {
		if providercontract.NormalizeOperationID(string(operationID)) == "" {
			return errors.New("provider chat cleanup requires a stable operation identity")
		}
		// A false Manager result means that the idempotent cleanup had no live or
		// hibernated binding to change; it is still a successful actor effect.
		r.manager.ForgetChat(ctx, tabID, chatID, operationID)
		return nil
	}); err != nil {
		return err
	}
	if err := coordinator.SetBackgroundExecutor(r.executeBackgroundAction); err != nil {
		return err
	}
	if err := coordinator.SetLifecycleObserver(r.publishLifecycleReceipt); err != nil {
		return err
	}
	return coordinator.SetCheckpointRestoreExecutor(func(ctx context.Context, chatID string, turnSequence int, checkpoint json.RawMessage, checkpointDigest string, operationID providercontract.OperationID) (json.RawMessage, error) {
		result, err := r.manager.RestoreChatCheckpoint(ctx, chatID, turnSequence, checkpoint, checkpointDigest, operationID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	})
}

func (r *providerChatRuntime) publishLifecycleReceipt(receipt chat.LifecycleReceipt) {
	if r == nil || r.publish == nil {
		return
	}
	state, ok := r.Snapshot(receipt.ChatID)
	if !ok {
		return
	}
	switch receipt.Kind {
	case chat.LifecycleHostRecoveryResumed:
		r.publish("chat:engine-recovered", map[string]any{
			"chatId": receipt.ChatID, "tabId": nullableString(state.Presentation.TabID),
			"oldSessionId": receipt.Thread.HeadID, "sessionId": receipt.Thread.HeadID,
			"at": time.Now().UTC().Format(time.RFC3339Nano),
		})
	case chat.LifecycleTurnReconciled:
		if receipt.Terminal {
			if event, err := projectReconciledTerminalJob(state, receipt.OperationID, receipt.Turn); err == nil {
				r.publish("job:event", event)
			}
		}
		// The actor snapshot carries the complete authoritative result, including
		// rich timeline/media that an operation readback cannot reproduce in one
		// terminal receipt. Ask connected renderers to reconcile from that state.
		r.publish("agent:apply", map[string]any{"action": "session-refresh"})
	case chat.LifecycleCheckpointRestored:
		var payload map[string]any
		if err := json.Unmarshal(receipt.Result, &payload); err != nil {
			return
		}
		// Restore mutates the worktree through the manager's filesystem executor.
		// Refresh and durably commit that observation before either frozen event
		// is visible. If the actor rejects it, fail closed: a restored card with a
		// stale Entorno snapshot would claim a transaction that is not rebuildable.
		if env := r.manager.ChatEnvGet(receipt.ChatID, state.Presentation.TabID); env.ChatID != "" {
			if err := r.observeChatEnv(env); err != nil {
				return
			}
		}
		payload["chatId"] = receipt.ChatID
		payload["turnSeq"] = receipt.TurnSequence
		payload["operationId"] = string(receipt.OperationID)
		r.publish("chat:checkpoint-restored", payload)
		r.publish("agent:apply", map[string]any{"action": "session-refresh"})
	}
}

func (r *providerChatRuntime) ResumeActors() error {
	if r == nil {
		return errors.New("provider chat runtime is unavailable")
	}
	r.mu.Lock()
	r.actorsPaused = false
	r.mu.Unlock()
	ids, err := r.knownChatIDs()
	if err != nil {
		return err
	}
	for _, chatID := range ids {
		actor, err := r.actor(chatID)
		if err != nil {
			return err
		}
		state := actor.engine.Snapshot()
		if state.Deleted {
			// Tombstones still need their coordinator resumed so a pending
			// native-cleanup effect can finish, but deleted actors no longer
			// own a live environment to restore.
			actor.coordinator.Wake()
			continue
		}
		if state.Migration.BlockedError != "" {
			// A quarantined chat remains visible and deletable but owns no live
			// provider/environment effects until an explicit repair exists.
			continue
		}
		if err := r.restoreActorEnvironment(actor); err != nil {
			return err
		}
		actor.coordinator.Wake()
	}
	return nil
}

func (r *providerChatRuntime) restoreActorEnvironment(actor *providerChatActor) error {
	if r == nil || r.manager == nil || actor == nil {
		return errors.New("provider chat runtime environment restore is unavailable")
	}
	state := actor.engine.Snapshot()
	if len(state.Environment.Reference) == 0 || string(state.Environment.Reference) == "null" {
		return nil
	}
	return r.manager.RestoreChatEnvReference(state.Environment.Reference)
}

func (r *providerChatRuntime) restoreActorChatEnvReference(chatID, tabID string) error {
	if r == nil || r.manager == nil {
		return errors.New("provider chat runtime environment restore is unavailable")
	}
	_, state, err := r.actorForExactChatPair(tabID, chatID)
	if err != nil {
		return err
	}
	if len(state.Environment.Reference) == 0 || string(state.Environment.Reference) == "null" {
		return nil
	}
	if err := r.manager.RestoreChatEnvReference(state.Environment.Reference); err != nil {
		return err
	}
	return nil
}

func (r *providerChatRuntime) knownChatIDs() ([]string, error) {
	if r == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bootErr != nil {
		return nil, fmt.Errorf("discover provider chat actors: %w", r.bootErr)
	}
	ids := make([]string, 0, len(r.known))
	for chatID := range r.known {
		ids = append(ids, chatID)
	}
	sort.Strings(ids)
	return ids, nil
}

func newChatPresentation(arg map[string]any, selection acp.ProviderLaneSelection) chat.PresentationState {
	return chat.PresentationState{
		TabID:          strings.TrimSpace(fieldString(arg, "tabId")),
		Title:          fieldString(arg, "title"),
		TitleLocked:    boolFieldValue(arg, "titleLocked"),
		Group:          optionalStringPointer(arg, "group"),
		CWD:            stringPointerValueOrNil(selection.CWD),
		Draft:          stringValue(arg["draft"]),
		Unread:         boolFieldValue(arg, "unread"),
		Settled:        fieldString(arg, "settled"),
		Pane:           optionalStringPointer(arg, "pane"),
		ProviderID:     selection.Identity.Realm.ProviderID,
		CurrentModelID: selection.ModelID,
		CurrentModeID:  selection.ModeID,
	}
}

func stringPointerValueOrNil(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func (r *providerChatRuntime) Select(ctx context.Context, opts acp.SessionOptions) (acp.SessionInfo, error) {
	opts.TabID = strings.TrimSpace(opts.TabID)
	opts.ChatID = strings.TrimSpace(opts.ChatID)
	if opts.TabID == "" || opts.ChatID == "" {
		return acp.SessionInfo{}, errors.New("provider chat commands require exact tabId and chatId")
	}
	actor, err := r.actor(opts.ChatID)
	if err != nil {
		return acp.SessionInfo{}, err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	selection, err := r.resolveSelectionLocked(ctx, actor, opts)
	if err != nil {
		return acp.SessionInfo{}, err
	}
	selection, err = r.selectLocked(ctx, actor, selection, opts)
	if err != nil {
		return acp.SessionInfo{}, err
	}
	info, ok := r.manager.LiveProviderLaneInfo(selection)
	if !ok {
		return acp.SessionInfo{}, &providercontract.Error{
			Kind: providercontract.ErrorTransientTransport, Message: "the exact provider lane is not attached after selection",
		}
	}
	return info, nil
}

// SelectNewChat attaches a provider lane to an already-created actor. Chat
// creation is a separate stable-operation command and this method must never
// treat a missing actor as an empty chat.
func (r *providerChatRuntime) SelectNewChat(ctx context.Context, arg map[string]any) (acp.SessionInfo, error) {
	opts := parseSessionOptions(arg)
	if strings.TrimSpace(opts.TabID) == "" || strings.TrimSpace(opts.ChatID) == "" {
		return acp.SessionInfo{}, errors.New("new provider chat requires exact tabId and chatId")
	}
	if providercontract.NormalizeOperationID(string(opts.OperationID)) == "" {
		// Preserve the precise missing-chat error for stale callers while still
		// requiring an explicit operation on an existing frozen-wire chat.
		if _, err := r.actor(opts.ChatID); err != nil {
			return acp.SessionInfo{}, err
		}
		return acp.SessionInfo{}, errors.New("new provider chat requires stable operationId")
	}
	// Chat creation is a separate durable actor command. Provider attachment may
	// never manufacture a missing chat as a compatibility side effect.
	return r.Select(ctx, opts)
}

// ChatWorkspaceForExactPair is the workspace projection for chat surfaces.
// The immutable ChatID is not an ownership credential: callers must prove the
// current tab attachment before reading the actor's workspace. This method is
// deliberately actor-only and performs no manager environment restore or
// provider-lane selection.
func (r *providerChatRuntime) ChatWorkspaceForExactPair(tabID, chatID string) (string, uint64, bool, error) {
	_, state, err := r.actorForExactChatPair(tabID, chatID)
	if err != nil {
		if errors.Is(err, errActorChatNotFound) {
			return "", 0, false, nil
		}
		return "", 0, false, err
	}
	if state.Presentation.CWD == nil {
		return "", state.Presentation.WorkspaceRevision, false, nil
	}
	return strings.TrimSpace(*state.Presentation.CWD), state.Presentation.WorkspaceRevision, true, nil
}

// actorForExactChatPair is the ownership fence for chat-scoped diagnostic
// surfaces. A ChatID alone is not sufficient: a stale renderer tab must not
// read or mutate another attachment's Entorno/checkpoint state, and a deleted
// actor must never be resurrected through the manager cache.
func (r *providerChatRuntime) actorForExactChatPair(tabID, chatID string) (*providerChatActor, chat.State, error) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return nil, chat.State{}, errors.New("chat surface requires exact tabId and chatId")
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return nil, chat.State{}, err
	}
	state := actor.engine.Snapshot()
	if !state.Initialized || state.Deleted {
		return nil, chat.State{}, errors.New("chat surface requires an active actor")
	}
	if strings.TrimSpace(state.Presentation.TabID) != tabID {
		return nil, chat.State{}, errors.New("chat surface tab attachment is stale")
	}
	return actor, state, nil
}

// observeChatEnv is the only actor-managed Entorno ingress. Manager remains a
// Git/filesystem observer; this method commits its typed observation and the
// durable checkpoint metadata to the actor before publishing the frozen event.
// It intentionally does not take actor.mu: manager.NewSession can invoke this
// callback synchronously while the coordinator already holds that lock.
func (r *providerChatRuntime) observeChatEnv(payload acp.ChatEnvPayload) error {
	if r == nil || r.manager == nil {
		return errors.New("provider chat runtime is unavailable")
	}
	chatID, tabID := strings.TrimSpace(payload.ChatID), strings.TrimSpace(payload.TabID)
	if chatID == "" || tabID == "" {
		return errors.New("chat environment observation is missing immutable ownership")
	}
	actor, state, err := r.actorForExactChatPair(tabID, chatID)
	if err != nil {
		return err
	}
	checkpoints := r.manager.ChatCheckpoints(chatID, tabID)
	reference, err := r.manager.ChatEnvReference(chatID, tabID)
	if err != nil {
		return err
	}
	payload.ChatID, payload.TabID = chatID, tabID
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode chat environment observation: %w", err)
	}
	checkpointRaw, err := json.Marshal(checkpoints)
	if err != nil {
		return fmt.Errorf("encode chat checkpoints: %w", err)
	}
	if err := actor.engine.Apply(chat.UpdateEnvironment{
		ExpectedTabID: tabID, CWD: strings.TrimSpace(payload.CWD),
		Payload: payloadRaw, Checkpoints: checkpointRaw, Reference: reference,
	}); err != nil {
		return err
	}
	committed := actor.engine.Snapshot()
	if committed.Revision <= state.Revision || committed.Environment.TabID != tabID || len(committed.Environment.Payload) == 0 {
		return errors.New("chat environment actor commit failed durable readback")
	}
	if r.publish != nil {
		r.publish("chat:env", payload)
	}
	return nil
}

// ChatEnvGet returns only the actor's durable Entorno projection. A missing
// observation is an honest empty state; it is never filled from the retired
// manager cache.
func (r *providerChatRuntime) ChatEnvGet(tabID, chatID string) (acp.ChatEnvPayload, error) {
	_, state, err := r.actorForExactChatPair(tabID, chatID)
	if err != nil {
		return acp.ChatEnvPayload{}, err
	}
	if len(state.Environment.Payload) == 0 {
		cwd := ""
		if state.Presentation.CWD != nil {
			cwd = strings.TrimSpace(*state.Presentation.CWD)
		}
		return acp.ChatEnvPayload{ChatID: state.ChatID, TabID: state.Presentation.TabID, CWD: cwd,
			Repos: []acp.ChatEnvRepo{}, Unchanged: []string{}, RepoLimit: 12, FileLimit: 200,
			Approximation: "changes are computed from this chat's git status/numstat baseline; edits to already-dirty files can only be approximated"}, nil
	}
	var payload acp.ChatEnvPayload
	if err := json.Unmarshal(state.Environment.Payload, &payload); err != nil {
		return acp.ChatEnvPayload{}, fmt.Errorf("decode actor chat environment: %w", err)
	}
	if payload.ChatID != state.ChatID || payload.TabID != state.Presentation.TabID {
		return acp.ChatEnvPayload{}, errors.New("actor chat environment ownership does not match chat")
	}
	return payload, nil
}

// ChatCheckpoints returns the actor-owned checkpoint metadata. Checkpoint refs
// remain executor data; the actor owns which refs belong to this chat.
func (r *providerChatRuntime) ChatCheckpoints(tabID, chatID string) ([]acp.ChatCheckpoint, error) {
	_, state, err := r.actorForExactChatPair(tabID, chatID)
	if err != nil {
		return nil, err
	}
	if len(state.Environment.Checkpoints) == 0 {
		return []acp.ChatCheckpoint{}, nil
	}
	var checkpoints []acp.ChatCheckpoint
	if err := json.Unmarshal(state.Environment.Checkpoints, &checkpoints); err != nil {
		return nil, fmt.Errorf("decode actor chat checkpoints: %w", err)
	}
	if checkpoints == nil {
		checkpoints = []acp.ChatCheckpoint{}
	}
	return checkpoints, nil
}

// ChatDiff validates actor ownership and then delegates only the Git diff
// calculation to the manager. The manager cannot select a chat or checkpoint;
// it receives the actor-selected immutable checkpoint metadata explicitly.
func (r *providerChatRuntime) ChatDiff(ctx context.Context, tabID, chatID, repo, path string) (map[string]any, error) {
	checkpoints, err := r.ChatCheckpoints(tabID, chatID)
	if err != nil {
		return nil, err
	}
	return r.manager.ChatDiffFromCheckpoints(ctx, chatID, checkpoints, repo, path)
}

func (r *providerChatRuntime) MoveWorkspace(ctx context.Context, arg map[string]any) (map[string]any, error) {
	if r == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	tabID, chatID, cwd := strings.TrimSpace(fieldString(arg, "tabId")), strings.TrimSpace(fieldString(arg, "chatId")), strings.TrimSpace(fieldString(arg, "cwd"))
	operationID := providercontract.NormalizeOperationID(fieldString(arg, "operationId"))
	expected, ok := intFieldPresent(arg, "expectedWorkspaceRevision")
	if tabID == "" || chatID == "" || cwd == "" || operationID == "" || !ok || expected < 0 {
		return nil, errors.New("workspace rebind requires exact tabId, chatId, cwd, stable operationId, and daemon revision authority")
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return nil, err
	}
	requestedSessionID := strings.TrimSpace(fieldString(arg, "replaceSessionId"))
	actor.mu.Lock()
	state := actor.engine.Snapshot()
	if state.Presentation.TabID != tabID {
		actor.mu.Unlock()
		return nil, errors.New("workspace rebind tab attachment is stale")
	}
	if receipt, exists := state.WorkspaceMutationReceipts[operationID]; exists {
		digest, digestErr := workspaceMoveRequestDigest(arg, tabID, chatID, cwd, uint64(expected), receipt.DetachOperationIDs)
		if digestErr != nil {
			actor.mu.Unlock()
			return nil, digestErr
		}
		if receipt.Digest != digest {
			actor.mu.Unlock()
			return nil, errors.New("workspace operation id was reused for different content")
		}
		actor.mu.Unlock()
		// The actor receipt is the complete retry authority. A completed group is
		// pure state readback; only a still-Pending member may be claimed through
		// the coordinator's exact actor effect boundary.
		return r.reconcileWorkspaceDetachGroup(ctx, actor, operationID, receipt), nil
	}
	if !state.Initialized || state.Deleted {
		actor.mu.Unlock()
		return nil, errors.New("workspace rebind requires an active chat actor")
	}
	targets, err := workspaceDetachTargets(state, tabID, cwd, requestedSessionID)
	if err != nil {
		actor.mu.Unlock()
		return nil, err
	}
	detachOperationIDs := make([]providercontract.OperationID, 0, len(targets))
	commandTargets := make([]chat.DetachTarget, 0, len(targets))
	for _, target := range targets {
		detachOperationIDs = append(detachOperationIDs, target.OperationID)
		commandTargets = append(commandTargets, chat.DetachTarget{
			OperationID: target.OperationID, LaneID: target.LaneID, Owner: target.Owner,
			ConnectionID: target.ConnectionID, ConnectionGeneration: target.Generation,
		})
	}
	digest, err := workspaceMoveRequestDigest(arg, tabID, chatID, cwd, uint64(expected), detachOperationIDs)
	if err != nil {
		actor.mu.Unlock()
		return nil, err
	}
	if err := actor.engine.Apply(chat.ChangeWorkspace{
		OperationID: operationID, Digest: digest, CWD: cwd, ExpectedRevision: uint64(expected),
		DetachTargets: commandTargets,
	}); err != nil {
		actor.mu.Unlock()
		return nil, err
	}
	committed := actor.engine.Snapshot()
	receipt, exists := committed.WorkspaceMutationReceipts[operationID]
	actor.mu.Unlock()
	if !exists || receipt.Digest != digest {
		return nil, errors.New("workspace actor commit failed durable readback")
	}
	return r.reconcileWorkspaceDetachGroup(ctx, actor, operationID, receipt), nil
}

func (r *providerChatRuntime) reconcileWorkspaceDetachGroup(ctx context.Context, actor *providerChatActor, operationID providercontract.OperationID, receipt chat.WorkspaceMutationReceipt) map[string]any {
	if actor == nil {
		return workspaceMoveUncertainResponse(operationID, receipt, "workspace detach coordinator is unavailable", true)
	}
	state := actor.engine.Snapshot()
	currentReceipt, exists := state.WorkspaceMutationReceipts[operationID]
	if !exists || currentReceipt.Digest != receipt.Digest || !sameWorkspaceOperationIDSequence(currentReceipt.DetachOperationIDs, receipt.DetachOperationIDs) {
		return workspaceMoveUncertainResponse(operationID, receipt, "workspace receipt changed before detach reconciliation", true)
	}
	reconcile := make([]providercontract.OperationID, 0, len(receipt.DetachOperationIDs))
	for _, detachOperationID := range receipt.DetachOperationIDs {
		entry, found := workspaceDetachOutboxEntry(state, detachOperationID)
		if !found {
			return workspaceMoveUncertainResponse(operationID, receipt, "workspace receipt is missing a durable detach effect", true)
		}
		switch entry.Status {
		case chat.OutboxCompleted:
		case chat.OutboxPending:
			reconcile = append(reconcile, detachOperationID)
		case chat.OutboxDispatched:
			// A same-process generic drain may have claimed this exact effect just
			// before the handler read the actor. Synchronize with the coordinator
			// below before deciding it is crash-ambiguous; ExecuteDetach never
			// resends a Dispatched effect.
			reconcile = append(reconcile, detachOperationID)
		case chat.OutboxAmbiguous:
			return workspaceMoveUncertainResponse(operationID, receipt, "workspace detach acceptance is ambiguous; the effect was not resent", true)
		case chat.OutboxFailed:
			return workspaceMoveUncertainResponse(operationID, receipt, "workspace detach failed before completion", false)
		default:
			return workspaceMoveUncertainResponse(operationID, receipt, fmt.Sprintf("workspace detach has invalid durable status %q", entry.Status), true)
		}
	}
	for _, detachOperationID := range reconcile {
		executed, executeErr := actor.coordinator.ExecuteDetach(ctx, detachOperationID)
		state = actor.engine.Snapshot()
		entry, found := workspaceDetachOutboxEntry(state, detachOperationID)
		if !found {
			return workspaceMoveUncertainResponse(operationID, receipt, "workspace detach effect disappeared during reconciliation", true)
		}
		if entry.Status != chat.OutboxCompleted {
			message := "workspace detach remained pending and was not claimed by the coordinator"
			if entry.Status == chat.OutboxDispatched {
				message = "workspace detach acceptance is unknown; the dispatched effect was not resent"
			} else if executeErr != nil {
				message = executeErr.Error()
			} else if executed {
				message = "workspace detach was claimed without a durable completion receipt"
			}
			return workspaceMoveUncertainResponse(operationID, receipt, message, entry.Status == chat.OutboxAmbiguous || entry.Status == chat.OutboxDispatched)
		}
	}
	state = actor.engine.Snapshot()
	for _, detachOperationID := range receipt.DetachOperationIDs {
		entry, found := workspaceDetachOutboxEntry(state, detachOperationID)
		if !found || entry.Status != chat.OutboxCompleted {
			return workspaceMoveUncertainResponse(operationID, receipt, "workspace detach group is not durably complete", true)
		}
	}
	return workspaceMoveReceiptResponse(operationID, receipt)
}

func workspaceDetachOutboxEntry(state chat.State, operationID providercontract.OperationID) (chat.OutboxEntry, bool) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectDetachLane && entry.OperationID == operationID {
			return entry, true
		}
	}
	return chat.OutboxEntry{}, false
}

func sameWorkspaceOperationIDSequence(left, right []providercontract.OperationID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type workspaceDetachTarget struct {
	OperationID  providercontract.OperationID
	LaneID       providercontract.LaneID
	Owner        providercontract.AttachmentOwner
	ConnectionID string
	Generation   uint64
}

// workspaceDetachTargets is a pure actor proof. The manager's session table is
// intentionally absent: a workspace move first proves the old disposable
// attachment from the durable lane, then journals ChangeWorkspace and the
// exact typed detach effect before the coordinator may close anything.
func workspaceDetachTargets(state chat.State, tabID, targetCWD, requestedSessionID string) ([]workspaceDetachTarget, error) {
	tabID, targetCWD, requestedSessionID = strings.TrimSpace(tabID), strings.TrimSpace(targetCWD), strings.TrimSpace(requestedSessionID)
	currentCWD := ""
	if state.Presentation.CWD != nil {
		currentCWD = strings.TrimSpace(*state.Presentation.CWD)
	}
	workspaceChanged := currentCWD == "" || !sameLegacyFilesystemPath(currentCWD, targetCWD)
	targets := make([]workspaceDetachTarget, 0)
	seenConnections := make(map[string]struct{})
	requestedFound := false
	laneIDs := make([]string, 0, len(state.Lanes))
	for laneID := range state.Lanes {
		laneIDs = append(laneIDs, string(laneID))
	}
	sort.Strings(laneIDs)
	for _, rawLaneID := range laneIDs {
		laneID := providercontract.LaneID(rawLaneID)
		lane := state.Lanes[laneID]
		if lane.Attachment == nil {
			continue
		}
		connectionID := strings.TrimSpace(lane.Attachment.ConnectionID)
		if connectionID == "" || lane.ConnectionGeneration == 0 || lane.Identity.ChatID != state.ChatID ||
			strings.TrimSpace(lane.Owner.TabID) != tabID {
			return nil, errors.New("workspace rebind found an invalid durable old attachment")
		}
		matchesRequested := requestedSessionID != "" && connectionID == requestedSessionID
		if matchesRequested {
			if requestedFound {
				return nil, errors.New("workspace rebind attachment is ambiguous")
			}
			requestedFound = true
		}
		if !workspaceChanged && !matchesRequested {
			continue
		}
		if _, duplicate := seenConnections[connectionID]; duplicate {
			return nil, errors.New("workspace rebind attachment connection is ambiguous")
		}
		seenConnections[connectionID] = struct{}{}
		targets = append(targets, workspaceDetachTarget{
			OperationID: chat.DetachOperationID(state.ChatID, laneID, connectionID, lane.ConnectionGeneration),
			LaneID:      laneID, Owner: lane.Owner, ConnectionID: connectionID, Generation: lane.ConnectionGeneration,
		})
	}
	if requestedSessionID != "" && !requestedFound {
		return nil, errors.New("workspace replacement session is not actor-owned")
	}
	return targets, nil
}

// workspaceMoveRequestDigest covers the immutable workspace request and the
// exact ordered detach group carried by the actor command. OperationID is
// deliberately excluded: it names the receipt, while the digest proves the
// immutable request bytes associated with that receipt. The replacement
// session id is included because changing it changes the exact detach intent;
// a retry must not silently target a different attachment.
func workspaceMoveRequestDigest(arg map[string]any, tabID, chatID, cwd string, expected uint64, targetSequences ...[]providercontract.OperationID) (string, error) {
	opts := parseSessionOptions(arg)
	workspaceRebind, _ := boolField(arg, "workspaceRebind")
	var detachOperationIDs []string
	if len(targetSequences) > 1 {
		return "", errors.New("workspace move digest received multiple detach groups")
	}
	if len(targetSequences) == 1 {
		detachOperationIDs = make([]string, 0, len(targetSequences[0]))
		for _, operationID := range targetSequences[0] {
			detachOperationIDs = append(detachOperationIDs, string(providercontract.NormalizeOperationID(string(operationID))))
		}
	}
	payload := struct {
		TabID              string
		ChatID             string
		CWD                string
		ExpectedRevision   uint64
		ProviderID         string
		ModelID            string
		ModeID             string
		WorkspaceRebind    bool
		ReplaceSessionID   string
		DetachOperationIDs []string
	}{
		TabID: tabID, ChatID: chatID, CWD: cwd, ExpectedRevision: expected,
		ProviderID: strings.TrimSpace(opts.ProviderID), ModelID: strings.TrimSpace(opts.ModelID),
		ModeID: strings.TrimSpace(opts.ModeID), WorkspaceRebind: workspaceRebind,
		ReplaceSessionID: strings.TrimSpace(fieldString(arg, "replaceSessionId")), DetachOperationIDs: detachOperationIDs,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode workspace move request: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func workspaceMoveReceiptResponse(operationID providercontract.OperationID, receipt chat.WorkspaceMutationReceipt) map[string]any {
	return map[string]any{
		"operationId": string(operationID), "sessionId": "", "cwd": strings.TrimSpace(receipt.CWD),
		"models": []any{}, "modes": []any{}, "workspaceCommitted": true,
		"workspaceRebound": true, "workspaceRevision": receipt.Revision,
	}
}

func workspaceMoveUncertainResponse(operationID providercontract.OperationID, receipt chat.WorkspaceMutationReceipt, message string, uncertain bool) map[string]any {
	response := map[string]any{
		"operationId": string(operationID), "sessionId": "", "cwd": strings.TrimSpace(receipt.CWD),
		"models": []any{}, "modes": []any{}, "workspaceCommitted": true,
		"workspaceRebound": false, "workspaceRevision": receipt.Revision,
		"error": strings.TrimSpace(message),
	}
	if uncertain {
		response["workspaceUncertain"] = true
	}
	return response
}

func (r *providerChatRuntime) Fork(ctx context.Context, arg map[string]any) (map[string]any, error) {
	if r == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	sourceTabID := fieldString(arg, "tabId")
	sourceChatID := fieldString(arg, "chatId")
	newTabID := fieldString(arg, "newTabId")
	newChatID := firstNonEmptyString(fieldString(arg, "newChatId"), fieldString(arg, "chatIdNew"))
	if sourceTabID == "" || sourceChatID == "" || newTabID == "" || newChatID == "" {
		return nil, errors.New("app-chat:fork requires exact source and child tab/chat ids")
	}
	if sourceTabID == newTabID || sourceChatID == newChatID {
		return nil, errors.New("app-chat:fork requires distinct child tab and chat ids")
	}
	operationID := providercontract.NormalizeOperationID(firstNonEmptyString(fieldString(arg, "operationId"), "fork:"+newChatID))
	digest, err := forkInitializationDigest(arg, sourceTabID, sourceChatID, newTabID, newChatID)
	if err != nil {
		return nil, err
	}
	if actor, found, err := r.existingForkActor(newChatID, operationID, digest); found {
		if err != nil {
			return nil, err
		}
		return r.attachForkChild(ctx, actor, operationID, sourceTabID)
	}
	source, err := r.actor(sourceChatID)
	if err != nil {
		return nil, err
	}
	source.mu.Lock()
	sourceState := source.engine.Snapshot()
	if sourceState.Presentation.TabID != sourceTabID {
		source.mu.Unlock()
		return nil, errors.New("fork source tab attachment is stale")
	}
	if sourceState.Foreground != nil || sourceState.PendingSteer != nil || sourceState.PendingCancel != nil {
		source.mu.Unlock()
		return nil, errors.New("fork requires an idle semantic turn boundary")
	}
	atTurn, hasAtTurn := intFieldPresent(arg, "atTurn")
	prefix, _ := actorForkPrefix(sourceState.Ledger, atTurn, hasAtTurn)
	presentation := sourceState.Presentation.Clone()
	source.mu.Unlock()
	presentation.TabID = newTabID
	presentation.TitleLocked = false
	presentation.Draft = ""
	presentation.Unread = false
	presentation.Settled = ""
	presentation.WorkspaceRevision = 0
	presentation.AgentQueueRevision = 0
	presentation.RuntimeControlRevision = 0
	presentation.ContextUsageByProvider = nil
	presentation.LegacyUsage = nil
	presentation.PlanLatest = nil
	presentation.PlanLatestMessageID = ""
	if cwd := strings.TrimSpace(fieldString(arg, "cwd")); cwd != "" {
		presentation.CWD = &cwd
	}

	if strings.TrimSpace(string(presentation.ProviderID)) == "" {
		if lane, ok := sourceState.Lanes[sourceState.ActiveLaneID]; ok {
			presentation.ProviderID = lane.Identity.Realm.ProviderID
		}
	}
	command := chat.InitializeFork{
		Presentation: presentation, SourceChatID: sourceChatID, Messages: prefix, OperationID: operationID,
	}
	command.Digest = digest
	actor, err := r.actorForFork(newChatID, command)
	if err != nil {
		return nil, err
	}
	return r.attachForkChild(ctx, actor, operationID, sourceTabID)
}

func (r *providerChatRuntime) attachForkChild(ctx context.Context, actor *providerChatActor, operationID providercontract.OperationID, sourceTabID string) (map[string]any, error) {
	if actor == nil {
		return nil, errors.New("fork child actor is unavailable")
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	childState := actor.engine.Snapshot()
	if !childState.Initialized || childState.Deleted || childState.CreationOperationID != operationID {
		return nil, errors.New("fork child creation receipt is not readable")
	}
	opts := acp.SessionOptions{
		TabID: childState.Presentation.TabID, ChatID: childState.ChatID,
		ProviderID: string(childState.Presentation.ProviderID),
		ModelID:    childState.Presentation.CurrentModelID, ModeID: childState.Presentation.CurrentModeID,
		OperationID: operationID,
	}
	if childState.Presentation.CWD != nil {
		opts.CWD = strings.TrimSpace(*childState.Presentation.CWD)
	}
	if opts.ProviderID == "" {
		if lane, ok := childState.Lanes[childState.ActiveLaneID]; ok {
			opts.ProviderID = string(lane.Identity.Realm.ProviderID)
		}
	}
	var selection acp.ProviderLaneSelection
	var err error
	if receipt, replay := childState.LaneSelectionMutationReceipts[operationID]; replay {
		lane, ok := childState.Lanes[receipt.LaneID]
		if !ok {
			err = errors.New("fork lane-selection receipt references a missing durable lane")
		} else {
			selection = providerLaneSelectionFromActorLane(lane)
		}
	} else {
		selection, err = r.resolveSelectionLocked(ctx, actor, opts)
	}
	if err == nil {
		selection, err = r.selectLocked(ctx, actor, selection, opts)
	}
	if err != nil {
		return nil, err
	}
	info, ok := r.manager.LiveProviderLaneInfo(selection)
	if !ok {
		return nil, errors.New("fork child provider lane is not attached")
	}
	effectiveTurn := forkChildTurn(childState.Ledger)
	return sessionInfoWithFork(info, sourceTabID, effectiveTurn), nil
}

func forkChildTurn(ledger []chat.LedgerEvent) int {
	turns := 0
	for _, event := range ledger {
		if event.Role == "user" && event.SteerState == "" {
			turns++
		}
	}
	return turns
}

func forkInitializationDigest(arg map[string]any, sourceTabID, sourceChatID, newTabID, newChatID string) (string, error) {
	atTurn, hasAtTurn := intFieldPresent(arg, "atTurn")
	payload := struct {
		SourceTabID  string
		SourceChatID string
		NewTabID     string
		NewChatID    string
		CWD          string
		AtTurn       int
		HasAtTurn    bool
	}{
		SourceTabID: strings.TrimSpace(sourceTabID), SourceChatID: strings.TrimSpace(sourceChatID),
		NewTabID: strings.TrimSpace(newTabID), NewChatID: strings.TrimSpace(newChatID),
		CWD: strings.TrimSpace(fieldString(arg, "cwd")), AtTurn: atTurn, HasAtTurn: hasAtTurn,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode fork initialization: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func actorForkPrefix(ledger []chat.LedgerEvent, atTurn int, hasAtTurn bool) ([]chat.LedgerEvent, int) {
	totalTurns := 0
	for _, event := range ledger {
		if event.Role == "user" && event.SteerState == "" {
			totalTurns++
		}
	}
	if !hasAtTurn || atTurn > totalTurns {
		atTurn = totalTurns
	}
	if atTurn <= 0 {
		return nil, 0
	}
	seenTurns := 0
	prefix := make([]chat.LedgerEvent, 0, len(ledger))
	for _, event := range ledger {
		if event.Role == "user" && event.SteerState == "" {
			seenTurns++
			if seenTurns > atTurn {
				break
			}
		}
		prefix = append(prefix, event)
	}
	return prefix, atTurn
}

func (r *providerChatRuntime) resolveSelectionLocked(ctx context.Context, actor *providerChatActor, opts acp.SessionOptions) (acp.ProviderLaneSelection, error) {
	if actor == nil {
		return acp.ProviderLaneSelection{}, errors.New("provider chat actor is unavailable")
	}
	state := actor.engine.Snapshot()
	if !state.Initialized || state.Deleted {
		return acp.ProviderLaneSelection{}, errors.New("provider chat is not an active initialized actor")
	}
	if tabID := strings.TrimSpace(opts.TabID); tabID == "" {
		return acp.ProviderLaneSelection{}, errors.New("provider lane selection is missing tab attachment")
	} else if tabID != strings.TrimSpace(state.Presentation.TabID) {
		// A tab id is disposable attachment metadata, but it is still an
		// ownership fence for ordinary provider operations.  Selection is
		// reached by session/new, job:start, and queue admission; none of those
		// commands is authorized to migrate an established chat to an arbitrary
		// caller merely because it knows the immutable ChatID.  Tab migration is
		// an explicit actor AttachTab command (for example, the focus/rekey
		// path), after which ordinary selection can proceed with the exact tab.
		return acp.ProviderLaneSelection{}, errors.New("provider lane selection tab attachment is stale; use explicit chat tab migration")
	}
	// Exact actor ownership must be established before restoring any manager
	// environment state or asking the manager to select a provider lane. A
	// stale tab may know the immutable ChatID, but it cannot cause manager work.
	if err := r.restoreActorEnvironment(actor); err != nil {
		return acp.ProviderLaneSelection{}, err
	}
	opts = effectiveSelectionOptions(state, opts)
	// A committed selection receipt is authoritative on retry. Do not resolve
	// the provider, probe a CLI, or create a second native attachment merely to
	// reconstruct a reply after the original wire response was lost.
	operationID := providercontract.NormalizeOperationID(string(opts.OperationID))
	if operationID != "" {
		digest, err := selectionRequestDigest(opts)
		if err != nil {
			return acp.ProviderLaneSelection{}, err
		}
		if receipt, exists := state.LaneSelectionMutationReceipts[operationID]; exists {
			if receipt.Digest != digest {
				return acp.ProviderLaneSelection{}, errors.New("lane-selection operation id was reused for different content")
			}
			lane, ok := state.Lanes[receipt.LaneID]
			if !ok {
				return acp.ProviderLaneSelection{}, errors.New("lane-selection receipt references a missing durable lane")
			}
			return providerLaneSelectionFromActorLane(lane), nil
		}
	}
	selection, err := r.manager.ResolveProviderLaneSelection(ctx, opts)
	if err != nil {
		return acp.ProviderLaneSelection{}, err
	}
	return selection, nil
}

func effectiveSelectionOptions(state chat.State, opts acp.SessionOptions) acp.SessionOptions {
	if strings.TrimSpace(opts.ProviderID) == "" {
		opts.ProviderID = string(state.Presentation.ProviderID)
	}
	if strings.TrimSpace(opts.ModelID) == "" {
		opts.ModelID = state.Presentation.CurrentModelID
	}
	if strings.TrimSpace(opts.ModeID) == "" {
		opts.ModeID = state.Presentation.CurrentModeID
	}
	if state.Presentation.CWD != nil {
		opts.CWD = strings.TrimSpace(*state.Presentation.CWD)
	}
	opts.ProviderID = string(providercontract.NormalizeID(opts.ProviderID))
	opts.ModelID = hydratableStoredModelID(opts.ModelID)
	opts.ModeID = strings.TrimSpace(opts.ModeID)
	opts.ProviderLaneManaged = true
	return opts
}

func selectionRequestDigest(opts acp.SessionOptions) (string, error) {
	payload := struct {
		TabID      string
		ChatID     string
		ProviderID string
		ModelID    string
		ModeID     string
		CWD        string
		OwnerKey   string
	}{
		TabID: strings.TrimSpace(opts.TabID), ChatID: strings.TrimSpace(opts.ChatID),
		ProviderID: strings.TrimSpace(opts.ProviderID), ModelID: strings.TrimSpace(opts.ModelID),
		ModeID: strings.TrimSpace(opts.ModeID), CWD: strings.TrimSpace(opts.CWD),
		OwnerKey: strings.TrimSpace(opts.AgentOwnerKey),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func providerLaneSelectionFromActorLane(lane chat.LaneState) acp.ProviderLaneSelection {
	return acp.ProviderLaneSelection{
		Identity: lane.Identity, Thread: lane.Thread, Owner: lane.Owner,
		CWD: lane.CWD, ModelID: lane.ModelID, ModeID: lane.ModeID,
		Context: lane.Context, Delivery: lane.Delivery, Established: !lane.Thread.IsZero(),
	}
}

func (r *providerChatRuntime) selectLocked(ctx context.Context, actor *providerChatActor, selection acp.ProviderLaneSelection, opts acp.SessionOptions) (acp.ProviderLaneSelection, error) {
	before := actor.engine.Snapshot()
	operationID := providercontract.NormalizeOperationID(string(opts.OperationID))
	if receipt, replay := before.LaneSelectionMutationReceipts[operationID]; replay {
		// A committed operation is a readback, not a new selection attempt. In
		// particular, do not drain another lane's outbox or reattach a hibernated
		// host when an old wire reply is retried after a newer user selection.
		lane, ok := before.Lanes[receipt.LaneID]
		if !ok {
			return selection, errors.New("lane-selection receipt references a missing durable lane")
		}
		selection = providerLaneSelectionFromActorLane(lane)
		switch lane.Phase {
		case chat.LaneReady, chat.LaneRunning, chat.LaneReconciling:
			return selection, nil
		case chat.LaneBlocked:
			return selection, &providercontract.Error{Kind: lane.LastError, Message: "provider lane is blocked at a safe boundary"}
		case chat.LaneBroken:
			return selection, &providercontract.Error{Kind: lane.LastError, Message: "provider lane failed closed"}
		default:
			return selection, &providercontract.Error{Kind: providercontract.ErrorTransientTransport, Message: "committed provider lane selection is not currently attached"}
		}
	}
	if err := r.commitLaneSelectionLocked(actor, selection, opts); err != nil {
		return selection, err
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return selection, err
	}
	state := actor.engine.Snapshot()
	// A retry may be replaying an older committed selection receipt after the
	// chat has since selected another desired lane. The immutable receipt owns
	// this reply, so finish against its exact lane instead of whatever happens
	// to be desired now. The receipt is also rekeyed when a provisional realm is
	// canonicalized by session/new, so it is the authority for first creation.
	laneID := state.DesiredLaneID
	if operationID != "" {
		if receipt, exists := state.LaneSelectionMutationReceipts[operationID]; exists {
			laneID = receipt.LaneID
		}
	}
	lane, ok := state.Lanes[laneID]
	if !ok {
		return selection, errors.New("provider lane selection did not persist a desired lane")
	}
	selection.Identity = lane.Identity
	selection.Thread = lane.Thread
	selection.CWD = lane.CWD
	selection.ModelID = lane.ModelID
	selection.ModeID = lane.ModeID
	selection.Context = lane.Context
	selection.Delivery = lane.Delivery
	selection.Established = !lane.Thread.IsZero()
	switch lane.Phase {
	case chat.LaneReady, chat.LaneRunning, chat.LaneReconciling:
		// Tab ids are disposable attachment metadata. At an idle boundary, move
		// the already-attached exact lane to the caller's current tab without
		// creating/resuming/replaying any provider state.
		if state.Foreground == nil && selection.Established {
			if err := r.manager.ReattachProviderLane(selection); err != nil {
				return selection, err
			}
		}
		return selection, nil
	case chat.LaneBlocked:
		return selection, &providercontract.Error{Kind: lane.LastError, Message: "provider lane is blocked at a safe boundary"}
	case chat.LaneBroken:
		return selection, &providercontract.Error{Kind: lane.LastError, Message: "provider lane failed closed"}
	case chat.LaneAbsent, chat.LaneCreating, chat.LaneDetached, chat.LaneResuming, chat.LaneImporting:
		if state.Foreground != nil && state.Foreground.LaneID != laneID {
			// A desired provider selected during another lane's running turn is a
			// valid immutable queue target. Attachment/import waits for the safe
			// terminal boundary; the current turn is never retargeted.
			return selection, nil
		}
		return selection, &providercontract.Error{Kind: providercontract.ErrorTransientTransport, Message: "provider lane selection is not ready"}
	default:
		return selection, fmt.Errorf("provider lane has unknown phase %q", lane.Phase)
	}
}

func (r *providerChatRuntime) commitLaneSelectionLocked(actor *providerChatActor, selection acp.ProviderLaneSelection, opts acp.SessionOptions) error {
	if actor == nil {
		return errors.New("provider chat actor is unavailable")
	}
	state := actor.engine.Snapshot()
	opts = effectiveSelectionOptions(state, opts)
	operationID := providercontract.NormalizeOperationID(string(opts.OperationID))
	if operationID == "" {
		// Direct internal callers predate the frozen wire operation field. Keep
		// those calls deterministic without weakening app-chat:new-session,
		// which rejects a missing explicit operation id before reaching here.
		operationID = providercontract.OperationID(fmt.Sprintf("lane-select:%s:%s:%d", state.ChatID, selection.Identity.ID, state.Revision))
		opts.OperationID = operationID
	}
	digest, err := selectionRequestDigest(opts)
	if err != nil {
		return err
	}
	update := chat.UpdateRuntimeControls{
		ProviderID: selection.Identity.Realm.ProviderID, ModelID: selection.ModelID, ModeID: selection.ModeID,
		ReplaceModelID: true, ReplaceModeID: true,
		ExpectedRevision: state.Presentation.RuntimeControlRevision, RequireRevision: true,
	}
	return actor.engine.Apply(chat.CommitLaneSelection{
		OperationID: operationID, Digest: digest,
		Identity: selection.Identity, Thread: selection.Thread, Owner: selection.Owner,
		CWD: selection.CWD, ModelID: selection.ModelID, ModeID: selection.ModeID,
		Context: selection.Context, Established: selection.Established, Update: update,
	})
}

func (r *providerChatRuntime) Start(ctx context.Context, arg map[string]any, origin string) (map[string]any, error) {
	if r == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	tabID, chatID := fieldString(arg, "tabId"), fieldString(arg, "chatId")
	if tabID == "" || chatID == "" {
		return nil, errors.New("job:start requires exact tabId and chatId")
	}
	fields, err := actorTurnPublicFields(arg)
	if err != nil {
		return nil, err
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return nil, err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if result, handled, err := r.existingTurnReceiptLocked(actor, arg, fields, origin); handled {
		return result, err
	}
	if actor.engine.Snapshot().Foreground != nil {
		if fieldString(arg, "busyMode") == "queue-v1" {
			opts := parseSessionOptions(arg)
			applyStagedTargetOptions(actor.engine.Snapshot(), fieldString(arg, "queueId"), &opts)
			selection, err := r.resolveSelectionLocked(ctx, actor, opts)
			if err != nil {
				return nil, err
			}
			selection, err = r.selectLocked(ctx, actor, selection, opts)
			if err != nil {
				return nil, err
			}
			return r.queueBusyTurnLocked(ctx, actor, selection, arg, fields, origin)
		}
		return nil, acp.ErrChatBusy
	}
	opts := parseSessionOptions(arg)
	applyStagedTargetOptions(actor.engine.Snapshot(), fieldString(arg, "queueId"), &opts)
	selection, err := r.resolveSelectionLocked(ctx, actor, opts)
	if err != nil {
		return nil, err
	}
	selection, err = r.selectLocked(ctx, actor, selection, opts)
	if err != nil {
		return nil, err
	}
	if queueID := strings.TrimSpace(fieldString(arg, "queueId")); queueID != "" && actorHasStagedQueue(actor.engine.Snapshot(), queueID) {
		return r.admitStagedLocked(ctx, actor, selection, arg, fields, origin)
	}
	attachmentPlan, err := r.sessions.PlanProviderAttachments(sliceArg(arg["images"]))
	if err != nil {
		return nil, err
	}
	return r.admitPreparedLocked(ctx, actor, selection, arg, fields, attachmentPlan, origin)
}

// existingTurnReceiptLocked fences lost-reply retries before provider
// resolution. The actor's immutable input is the authority; an old retry may
// read its receipt but cannot select/attach a lane, probe a provider, or mutate
// today's controls. Reusing the operation id with different input fails closed.
func (r *providerChatRuntime) existingTurnReceiptLocked(
	actor *providerChatActor,
	arg map[string]any,
	fields map[string]string,
	origin string,
) (map[string]any, bool, error) {
	operationID := providercontract.NormalizeOperationID(fields["operationId"])
	state := actor.engine.Snapshot()
	if _, exists := state.Operations[operationID]; !exists {
		return nil, false, nil
	}
	var durable *chat.QueueEntry
	for index := range state.Queue {
		if state.Queue[index].OperationID == operationID {
			entry := state.Queue[index]
			durable = &entry
			break
		}
	}
	if durable == nil && state.Foreground != nil && state.Foreground.OperationID == operationID {
		entry := state.Foreground.Input
		durable = &entry
	}
	if durable == nil {
		for _, entry := range state.Outbox {
			if entry.Kind == chat.EffectStartTurn && entry.OperationID == operationID && entry.Input != nil {
				input := *entry.Input
				durable = &input
				break
			}
		}
	}
	if durable == nil {
		return nil, true, errors.New("operation id already belongs to a different actor command")
	}
	attachments, err := steerAttachmentInputIdentity(sliceArg(arg["images"]), r.sessions)
	if err != nil {
		return nil, true, err
	}
	incomingText := strings.TrimSpace(firstNonEmptyString(fieldString(arg, "prompt"), fieldString(arg, "message")))
	incomingOrigin := strings.ToLower(strings.TrimSpace(origin))
	if incomingOrigin == "" {
		incomingOrigin = "human"
	}
	if durable.Text != incomingText || !sameSteerAttachments(durable.Attachments, attachments) ||
		durable.Presentation.UserMessageID != fields["userMessageId"] ||
		durable.Presentation.AssistantMessageID != fields["assistantMessageId"] ||
		durable.Presentation.Origin != incomingOrigin ||
		(strings.TrimSpace(fieldString(arg, "queueId")) != "" && durable.Presentation.QueueID != strings.TrimSpace(fieldString(arg, "queueId"))) {
		return nil, true, errors.New("turn operation id was reused for different content")
	}
	for _, entry := range state.Queue {
		if entry.OperationID == operationID {
			return queuedActorReceipt(state, operationID, entry.Presentation.QueueID), true, nil
		}
	}
	result, err := r.admissionOutcomeLocked(actor, fieldString(arg, "tabId"), fieldString(arg, "chatId"), operationID)
	return result, true, err
}

func (r *providerChatRuntime) queueBusyTurnLocked(
	ctx context.Context,
	actor *providerChatActor,
	selection acp.ProviderLaneSelection,
	arg map[string]any,
	fields map[string]string,
	origin string,
) (map[string]any, error) {
	operationID := providercontract.NormalizeOperationID(fields["operationId"])
	queueID := firstNonEmptyString(fieldString(arg, "queueId"), fieldString(arg, agentQueueMessageField))
	if queueID == "" {
		queueID = nextSessionID("q")
	}
	if actorHasStagedQueue(actor.engine.Snapshot(), queueID) {
		return r.promoteStagedLocked(ctx, actor, selection, arg, fields, origin, false)
	}
	state := actor.engine.Snapshot()
	if _, exists := state.Operations[operationID]; exists {
		return queuedActorReceipt(state, operationID, queueID), nil
	}
	attachmentPlan, err := r.sessions.PlanProviderAttachments(sliceArg(arg["images"]))
	if err != nil {
		return nil, err
	}
	attachments := attachmentPlan.Attachments
	presentation := turnPresentation(arg, fields, origin)
	presentation.QueueID = queueID
	if err := actor.engine.ApplyPrepared(chat.Submit{
		OperationID: operationID, LaneID: selection.Identity.ID,
		Text:        firstNonEmptyString(fieldString(arg, "prompt"), fieldString(arg, "message")),
		Attachments: attachments, ModelID: selection.ModelID, ModeID: selection.ModeID,
		Permission: fieldString(arg, "permissionMode"), Presentation: presentation,
	}, attachmentPlan.Materialize); err != nil {
		return nil, err
	}
	// The foreground may have crossed its terminal boundary between the busy
	// check and this commit. Drain executes only if the queued operation became
	// immediately eligible; otherwise the coordinator's event wake owns it.
	if err := actor.coordinator.Drain(ctx); err != nil {
		return nil, err
	}
	return queuedActorReceipt(actor.engine.Snapshot(), operationID, queueID), nil
}

func actorHasStagedQueue(state chat.State, queueID string) bool {
	queueID = strings.TrimSpace(queueID)
	for _, entry := range state.StagedQueue {
		if entry.ID == queueID {
			return true
		}
	}
	return false
}

func applyStagedTargetOptions(state chat.State, queueID string, opts *acp.SessionOptions) {
	if opts == nil {
		return
	}
	queueID = strings.TrimSpace(queueID)
	for _, entry := range state.StagedQueue {
		if entry.ID != queueID {
			continue
		}
		if entry.TargetProviderID != "" {
			opts.ProviderID = string(entry.TargetProviderID)
		}
		if entry.ModelID != "" {
			opts.ModelID = entry.ModelID
		}
		if entry.ModeID != "" {
			opts.ModeID = entry.ModeID
		}
		return
	}
}

func (r *providerChatRuntime) admitStagedLocked(
	ctx context.Context,
	actor *providerChatActor,
	selection acp.ProviderLaneSelection,
	arg map[string]any,
	fields map[string]string,
	origin string,
) (map[string]any, error) {
	return r.promoteStagedLocked(ctx, actor, selection, arg, fields, origin, true)
}

func (r *providerChatRuntime) promoteStagedLocked(
	ctx context.Context,
	actor *providerChatActor,
	selection acp.ProviderLaneSelection,
	arg map[string]any,
	fields map[string]string,
	origin string,
	wantAdmission bool,
) (map[string]any, error) {
	queueID := strings.TrimSpace(fieldString(arg, "queueId"))
	operationID := providercontract.NormalizeOperationID(fields["operationId"])
	state := actor.engine.Snapshot()
	if _, exists := state.Operations[operationID]; exists {
		if wantAdmission {
			return r.admissionOutcomeLocked(actor, fieldString(arg, "tabId"), fieldString(arg, "chatId"), operationID)
		}
		return queuedActorReceipt(state, operationID, queueID), nil
	}
	presentation := turnPresentation(arg, fields, origin)
	presentation.QueueID = queueID
	if err := actor.engine.Apply(chat.PromoteStagedQueue{
		QueueID: queueID, OperationID: operationID, LaneID: selection.Identity.ID,
		ModelID: selection.ModelID, ModeID: selection.ModeID, Permission: fieldString(arg, "permissionMode"),
		Presentation: presentation,
	}); err != nil {
		return nil, err
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return nil, err
	}
	if wantAdmission {
		return r.admissionOutcomeLocked(actor, fieldString(arg, "tabId"), fieldString(arg, "chatId"), operationID)
	}
	return queuedActorReceipt(actor.engine.Snapshot(), operationID, queueID), nil
}

func queuedActorReceipt(state chat.State, operationID providercontract.OperationID, queueID string) map[string]any {
	position := 0
	for index, entry := range state.Queue {
		if entry.OperationID == operationID {
			position = index + 1
			queueID = firstNonEmptyString(entry.Presentation.QueueID, queueID)
			break
		}
	}
	if state.Foreground != nil && state.Foreground.OperationID == operationID {
		queueID = firstNonEmptyString(state.Foreground.Input.Presentation.QueueID, queueID)
	}
	return map[string]any{
		"queued": true, "queueId": queueID, "position": position, "delivery": "queue",
		"operationId":        string(operationID),
		"agentQueueRevision": state.Presentation.AgentQueueRevision,
	}
}

func actorTurnPublicFields(arg map[string]any) (map[string]string, error) {
	userID := firstNonEmptyString(fieldString(arg, "userMessageId"), fieldString(arg, "clientUserMessageId"))
	assistantID := firstNonEmptyString(fieldString(arg, "assistantMessageId"), fieldString(arg, "continuationAssistantMessageId"))
	// userMessageId was the frozen wire's original idempotency key. Modern
	// clients send the explicit alias; old exact clients remain safe because the
	// stable public user id is itself the immutable operation identity.
	operationID := providercontract.NormalizeOperationID(firstNonEmptyString(fieldString(arg, "operationId"), userID))
	if operationID == "" || userID == "" || assistantID == "" {
		return nil, errors.New("job:start requires stable operationId, userMessageId, and assistantMessageId")
	}
	startedAt := fieldString(arg, "startedAt")
	if startedAt == "" {
		startedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return map[string]string{
		"operationId":   string(operationID),
		"userMessageId": userID, "assistantMessageId": assistantID,
		"promptText": redactedSessionString(firstNonEmptyString(fieldString(arg, "prompt"), fieldString(arg, "message"))),
		"startedAt":  startedAt,
	}, nil
}

func turnPresentation(arg map[string]any, fields map[string]string, origin string) providercontract.TurnPresentation {
	presentation := providercontract.TurnPresentation{
		UserMessageID: fields["userMessageId"], AssistantMessageID: fields["assistantMessageId"],
		QueueID: fieldString(arg, "queueId"), PromptText: fields["promptText"],
		Title: fieldString(arg, "title"), Origin: strings.ToLower(strings.TrimSpace(origin)),
		StartedAt: firstNonEmptyString(fields["startedAt"], fieldString(arg, "startedAt")),
	}
	if presentation.StartedAt == "" {
		presentation.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if presentation.Origin == "" {
		presentation.Origin = "human"
	}
	return presentation
}

func (r *providerChatRuntime) admitPreparedLocked(
	ctx context.Context,
	actor *providerChatActor,
	selection acp.ProviderLaneSelection,
	arg map[string]any,
	fields map[string]string,
	attachmentPlan providerAttachmentPlan,
	origin string,
) (map[string]any, error) {
	tabID, chatID := fieldString(arg, "tabId"), fieldString(arg, "chatId")
	operationID := providercontract.NormalizeOperationID(fields["operationId"])
	if operationID == "" {
		return nil, errors.New("canonical turn is missing its stable operation id")
	}
	if _, exists := actor.engine.Snapshot().Operations[operationID]; exists {
		return r.admissionOutcomeLocked(actor, tabID, chatID, operationID)
	}
	prompt := firstNonEmptyString(fieldString(arg, "prompt"), fieldString(arg, "message"))
	presentation := turnPresentation(arg, fields, origin)
	if err := actor.engine.ApplyPrepared(chat.Submit{
		OperationID: operationID, LaneID: selection.Identity.ID, Text: prompt, Attachments: attachmentPlan.Attachments,
		ModelID: selection.ModelID, ModeID: selection.ModeID, Permission: fieldString(arg, "permissionMode"),
		Presentation: presentation,
	}, attachmentPlan.Materialize); err != nil {
		return nil, err
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return nil, err
	}
	return r.admissionOutcomeLocked(actor, tabID, chatID, operationID)
}

func (r *providerChatRuntime) admissionOutcomeLocked(actor *providerChatActor, tabID, chatID string, operationID providercontract.OperationID) (map[string]any, error) {
	state := actor.engine.Snapshot()
	if _, exists := state.Operations[operationID]; !exists {
		return nil, &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Operation: operationID, Message: "provider admission receipt has no durable actor operation"}
	}
	for _, entry := range state.Outbox {
		if entry.Kind != chat.EffectStartTurn || entry.OperationID != operationID {
			continue
		}
		switch entry.Status {
		case chat.OutboxAccepted, chat.OutboxConsumed, chat.OutboxCompleted:
			if admitted, ok := r.manager.ProviderLaneAdmission(tabID, chatID, operationID); ok {
				return admitted, nil
			}
			return nil, &providercontract.Error{
				Kind: providercontract.ErrorAcceptanceAmbiguous, Operation: operationID,
				Message: "the actor proves provider admission but its frozen wire receipt is unavailable; input was not resent",
			}
		case chat.OutboxAmbiguous:
			return nil, &providercontract.Error{Kind: providercontract.ErrorAcceptanceAmbiguous, Operation: operationID, Message: "provider turn acceptance is uncertain; input was not resent"}
		case chat.OutboxFailed:
			kind := entry.LastError
			if kind == "" {
				kind = providercontract.ErrorAdmissionRejected
			}
			return nil, &providercontract.Error{Kind: kind, Operation: operationID, Message: "provider turn admission failed"}
		}
	}
	return nil, &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Operation: operationID, Message: "provider turn admission omitted its frozen wire receipt"}
}

type durableSteerInput struct {
	Text         string
	Attachments  []providercontract.Attachment
	Presentation providercontract.TurnPresentation
	Turn         providercontract.TurnRef
}

// durableSteerInputForOperation is a pure actor read. It deliberately reads
// the durable pending/outbox/queue/ledger records instead of asking the
// manager to reconstruct a lost reply. A steer operation has one immutable
// public user-message id; every durable representation must describe that
// same operation.
func durableSteerInputForOperation(state chat.State, operationID providercontract.OperationID) (durableSteerInput, bool) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return durableSteerInput{}, false
	}
	var input durableSteerInput
	found := false
	for _, event := range state.Ledger {
		if event.OperationID != operationID || event.Role != "user" || (strings.TrimSpace(event.SteerState) == "" && strings.TrimSpace(event.QueueID) == "") {
			continue
		}
		input.Text = strings.TrimSpace(event.Text)
		input.Attachments = append([]providercontract.Attachment(nil), event.Attachments...)
		input.Presentation.UserMessageID = strings.TrimSpace(event.MessageID)
		input.Presentation.AssistantMessageID = strings.TrimSpace(event.SteerContinuationID)
		input.Presentation.PromptText = strings.TrimSpace(event.Text)
		input.Turn = providercontract.TurnRef{OperationID: operationID, NativeID: strings.TrimSpace(event.NativeTurnID)}
		found = true
		break
	}
	if state.Foreground != nil && state.Foreground.OperationID == operationID && strings.TrimSpace(state.Foreground.Input.Presentation.QueueID) != "" {
		input.Text = strings.TrimSpace(state.Foreground.Input.Text)
		input.Attachments = append([]providercontract.Attachment(nil), state.Foreground.Input.Attachments...)
		input.Presentation = state.Foreground.Input.Presentation
		input.Turn = state.Foreground.Turn
		found = true
	}
	for _, entry := range state.Outbox {
		if entry.OperationID != operationID {
			continue
		}
		if entry.Kind == chat.EffectStartTurn && entry.Input != nil && strings.TrimSpace(entry.Input.Presentation.QueueID) != "" {
			input.Text = strings.TrimSpace(entry.Input.Text)
			input.Attachments = append([]providercontract.Attachment(nil), entry.Input.Attachments...)
			input.Presentation = entry.Input.Presentation
			input.Turn = entry.Turn
			found = true
			continue
		}
		if entry.Kind != chat.EffectSteerTurn {
			continue
		}
		if entry.Input != nil {
			input.Text = strings.TrimSpace(entry.Input.Text)
			input.Attachments = append([]providercontract.Attachment(nil), entry.Input.Attachments...)
		}
		if entry.Turn.NativeID != "" {
			input.Turn = entry.Turn
		}
		found = true
		break
	}
	for _, entry := range state.Queue {
		if entry.OperationID != operationID {
			continue
		}
		input.Text = strings.TrimSpace(entry.Text)
		input.Attachments = append([]providercontract.Attachment(nil), entry.Attachments...)
		input.Presentation = entry.Presentation
		input.Turn = providercontract.TurnRef{OperationID: operationID}
		found = true
		break
	}
	if state.PendingSteer != nil && state.PendingSteer.OperationID == operationID {
		pending := state.PendingSteer
		input.Text = strings.TrimSpace(pending.Text)
		input.Attachments = append([]providercontract.Attachment(nil), pending.Attachments...)
		input.Presentation = pending.Presentation
		input.Turn = pending.Turn
		found = true
	}
	return input, found
}

// steerAttachmentInputIdentity computes the same immutable attachment
// identity that PersistProviderAttachments will materialize, without writing
// image sidecars. This is the duplicate/changed-request read path: retries
// must be able to compare bytes and metadata before any persistence occurs.
func steerAttachmentInputIdentity(images []any, stores ...*sessionStore) ([]providercontract.Attachment, error) {
	if len(images) == 0 {
		return nil, nil
	}
	var store *sessionStore
	if len(stores) > 1 {
		return nil, errors.New("provider image identity received multiple session stores")
	}
	if len(stores) == 1 {
		store = stores[0]
	}
	attachments := make([]providercontract.Attachment, 0, len(images))
	for index, raw := range images {
		image := mapFromAnyMain(raw)
		mimeType := strings.ToLower(strings.TrimSpace(fieldString(image, "mimeType")))
		if !strings.HasPrefix(mimeType, "image/") {
			return nil, fmt.Errorf("provider image %d is missing MIME identity", index)
		}
		name := firstNonEmptyString(fieldString(image, "name"), fmt.Sprintf("image-%d", index+1))
		data := fieldString(image, "data")
		ref := strings.TrimSpace(fieldString(image, sessionImageDataRefField))
		digest := strings.TrimSpace(fieldString(image, "digest"))
		var size int64
		if data != "" {
			digest = sessionImageName(data)
			ref = sessionImageDirname + "/" + digest
			size = int64(len(data))
		} else {
			cleanRef := filepath.ToSlash(filepath.Clean(ref))
			if cleanRef == "." || cleanRef == "" || !strings.HasPrefix(cleanRef, sessionImageDirname+"/") || strings.Contains(cleanRef, "../") {
				return nil, fmt.Errorf("provider image %d is missing durable content reference", index)
			}
			ref = cleanRef
			if store != nil && store.enabled() {
				name, _, info, refErr := externalSessionImageInfo(ref, filepath.Dir(store.path))
				if refErr != nil {
					return nil, fmt.Errorf("provider image %d has an unreadable durable content reference: %w", index, refErr)
				}
				digest, size = name, info.Size()
			} else {
				if digest == "" {
					digest = strings.TrimPrefix(ref, sessionImageDirname+"/")
				}
				if rawSize, ok := image["size"]; ok {
					size = int64(intValue(rawSize))
				}
			}
		}
		if digest == "" || len(digest) < 16 || ref == "" {
			return nil, fmt.Errorf("provider image %d is missing immutable content identity", index)
		}
		attachments = append(attachments, providercontract.Attachment{
			ID: "image-" + digest[:16], Name: name, MIMEType: mimeType,
			Digest: digest, Size: size, Ref: providerSessionImageRefPrefix + ref,
		})
	}
	return attachments, nil
}

func sameSteerAttachmentIdentity(left, right providercontract.Attachment) bool {
	if left.ID != right.ID || left.Name != right.Name || left.MIMEType != right.MIMEType ||
		left.Digest != right.Digest || left.Ref != right.Ref {
		return false
	}
	// A ref-native retry may not carry the optional size field. Digest, ref,
	// MIME, and name remain the immutable identity in that case.
	return left.Size == 0 || right.Size == 0 || left.Size == right.Size
}

func sameSteerAttachments(left, right []providercontract.Attachment) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !sameSteerAttachmentIdentity(left[index], right[index]) {
			return false
		}
	}
	return true
}

func sameSteerImmutableInput(input durableSteerInput, operationID providercontract.OperationID, prompt string, attachments []providercontract.Attachment, continuationID string) bool {
	if strings.TrimSpace(input.Text) != strings.TrimSpace(prompt) || !sameSteerAttachments(input.Attachments, attachments) {
		return false
	}
	if stored := strings.TrimSpace(input.Presentation.UserMessageID); stored != "" && stored != string(operationID) {
		return false
	}
	if stored := strings.TrimSpace(input.Presentation.AssistantMessageID); stored != "" && stored != strings.TrimSpace(continuationID) {
		return false
	}
	return true
}

// steerAttachmentTarget is the actor-owned attachment/generation fence. The
// manager's transient session map is intentionally not consulted here. A
// session id authorizes steering only when the durable actor still associates
// it with exactly one current lane attachment and connection generation.
func steerAttachmentTarget(state chat.State, sessionID, requestedTabID, requestedChatID string) (providercontract.LaneID, uint64, error) {
	sessionID = strings.TrimSpace(sessionID)
	requestedTabID, requestedChatID = strings.TrimSpace(requestedTabID), strings.TrimSpace(requestedChatID)
	if sessionID == "" {
		return "", 0, errors.New("live steer requires a provider session id")
	}
	if requestedTabID != "" || requestedChatID != "" {
		if requestedTabID == "" || requestedChatID == "" {
			return "", 0, errors.New("live steer requires the exact tabId and chatId pair")
		}
		if requestedChatID != strings.TrimSpace(state.ChatID) {
			return "", 0, errors.New("steer chat id does not own the actor")
		}
		if requestedTabID != strings.TrimSpace(state.Presentation.TabID) {
			return "", 0, errors.New("steer tab attachment is stale")
		}
	}
	if state.Deleted || !state.Initialized || strings.TrimSpace(state.Presentation.TabID) == "" {
		return "", 0, errors.New("steer requires an active actor attachment")
	}
	var found providercontract.LaneID
	var generation uint64
	for laneID, lane := range state.Lanes {
		if lane.Attachment == nil || strings.TrimSpace(lane.Attachment.ConnectionID) != sessionID {
			continue
		}
		if lane.Identity.ChatID != state.ChatID || strings.TrimSpace(lane.Owner.TabID) != strings.TrimSpace(state.Presentation.TabID) || lane.ConnectionGeneration == 0 {
			return "", 0, errors.New("steer session attachment is stale")
		}
		if providerID := providercontract.NormalizeID(string(lane.Attachment.ProviderID)); providerID != "" && providerID != providercontract.NormalizeID(string(lane.Identity.Realm.ProviderID)) {
			return "", 0, errors.New("steer session provider attachment is stale")
		}
		if found != "" {
			return "", 0, errors.New("steer session attachment is ambiguous")
		}
		found, generation = laneID, lane.ConnectionGeneration
	}
	if found == "" {
		return "", 0, errors.New("steer session attachment is stale or not actor-owned")
	}
	return found, generation, nil
}

func (r *providerChatRuntime) actorForSteerSession(sessionID, requestedTabID, requestedChatID string) (*providerChatActor, chat.State, providercontract.LaneID, uint64, error) {
	if r == nil {
		return nil, chat.State{}, "", 0, errors.New("provider chat runtime is unavailable")
	}
	sessionID = strings.TrimSpace(sessionID)
	requestedTabID, requestedChatID = strings.TrimSpace(requestedTabID), strings.TrimSpace(requestedChatID)
	if sessionID == "" {
		return nil, chat.State{}, "", 0, errors.New("live steer requires a provider session id")
	}
	if requestedTabID != "" || requestedChatID != "" {
		if requestedTabID == "" || requestedChatID == "" {
			return nil, chat.State{}, "", 0, errors.New("live steer requires the exact tabId and chatId pair")
		}
		actor, state, err := r.exactActor(requestedTabID, requestedChatID)
		if err != nil {
			return nil, chat.State{}, "", 0, fmt.Errorf("steer session attachment is stale: %w", err)
		}
		laneID, generation, err := steerAttachmentTarget(state, sessionID, requestedTabID, requestedChatID)
		if err != nil {
			return nil, chat.State{}, "", 0, err
		}
		return actor, state, laneID, generation, nil
	}

	ids, err := r.knownChatIDs()
	if err != nil {
		return nil, chat.State{}, "", 0, err
	}
	var match *providerChatActor
	var matchState chat.State
	var matchLane providercontract.LaneID
	var matchGeneration uint64
	for _, chatID := range ids {
		actor, err := r.actor(chatID)
		if err != nil {
			return nil, chat.State{}, "", 0, err
		}
		state := actor.engine.Snapshot()
		laneID, generation, targetErr := steerAttachmentTarget(state, sessionID, "", "")
		if targetErr != nil {
			continue
		}
		if match != nil {
			return nil, chat.State{}, "", 0, errors.New("steer session attachment is ambiguous")
		}
		match, matchState, matchLane, matchGeneration = actor, state, laneID, generation
	}
	if match == nil {
		return nil, chat.State{}, "", 0, errors.New("steer session attachment is stale or not actor-owned")
	}
	return match, matchState, matchLane, matchGeneration, nil
}

func steerForegroundTarget(state chat.State, laneID providercontract.LaneID, generation uint64, sessionID string) error {
	if state.Foreground == nil || state.Foreground.Status != chat.ForegroundRunning || state.Foreground.Turn.NativeID == "" {
		return errors.New("steer requires a running foreground turn")
	}
	if state.Foreground.LaneID != laneID {
		return errors.New("steer session does not own the foreground turn")
	}
	lane, ok := state.Lanes[laneID]
	if !ok || lane.Phase != chat.LaneRunning || lane.ConnectionGeneration != generation || lane.Attachment == nil || strings.TrimSpace(lane.Attachment.ConnectionID) != strings.TrimSpace(sessionID) {
		return errors.New("steer session attachment generation is stale")
	}
	if state.PendingSteer != nil {
		return errors.New("another steer is awaiting a provider receipt")
	}
	return nil
}

func steerForegroundEnded(state chat.State) bool {
	return state.Foreground == nil
}

func (r *providerChatRuntime) durableSteerReply(state chat.State, operationID providercontract.OperationID) (map[string]any, error) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	for _, entry := range state.Queue {
		if entry.OperationID == operationID {
			return queuedSteerReceipt(state, operationID, steerQueueErrorKind(state, operationID, providercontract.ErrorAdmissionRejected)), nil
		}
	}
	if state.Foreground != nil && state.Foreground.OperationID == operationID && strings.TrimSpace(state.Foreground.Input.Presentation.QueueID) != "" {
		return queuedSteerReceipt(state, operationID, steerQueueErrorKind(state, operationID, providercontract.ErrorAdmissionRejected)), nil
	}
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectStartTurn && entry.OperationID == operationID && entry.Input != nil && strings.TrimSpace(entry.Input.Presentation.QueueID) != "" {
			return queuedSteerReceiptWithQueueID(state, operationID, entry.Input.Presentation.QueueID, providercontract.ErrorAdmissionRejected), nil
		}
		if entry.Kind != chat.EffectSteerTurn || entry.OperationID != operationID {
			continue
		}
		turnID := strings.TrimSpace(entry.Turn.NativeID)
		if turnID == "" && state.Foreground != nil && state.Foreground.OperationID == operationID {
			turnID = strings.TrimSpace(state.Foreground.Turn.NativeID)
		}
		switch entry.Status {
		case chat.OutboxAmbiguous:
			result := map[string]any{"ok": false, "live": false, "queued": false, "strategy": "uncertain", "error": "the provider did not confirm steering acceptance; input was not resent"}
			if state.PendingSteer != nil && state.PendingSteer.OperationID == operationID && state.PendingSteer.AwaitConsumption {
				result["receipt"] = true
			}
			return result, nil
		case chat.OutboxFailed:
			return nil, &providercontract.Error{Kind: entry.LastError, Operation: operationID, Message: "provider rejected live steering without a safe queue transfer"}
		case chat.OutboxAccepted, chat.OutboxConsumed, chat.OutboxCompleted, chat.OutboxPending, chat.OutboxDispatched:
			strategy := "generic-live"
			result := map[string]any{"ok": true, "live": true, "queued": false, "strategy": strategy}
			if turnID != "" {
				result["turnId"] = turnID
			}
			if state.PendingSteer != nil && state.PendingSteer.OperationID == operationID {
				if state.PendingSteer.AwaitConsumption {
					result["strategy"] = "receipt-live"
					result["receipt"] = true
				}
				if state.PendingSteer.Interrupted {
					result["interrupted"] = true
				}
			}
			return result, nil
		}
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Operation: operationID, Message: "steer operation has no durable provider receipt"}
	}
	return nil, errors.New("steer operation is not owned by this chat actor")
}

// Steer routes a live user correction through the durable actor mailbox. The
// manager is reached only by the coordinator after the exact operation,
// immutable input, and attachment generation have been committed.
func (r *providerChatRuntime) Steer(ctx context.Context, arg map[string]any) (map[string]any, bool, error) {
	if r == nil {
		return nil, false, nil
	}
	sessionID := strings.TrimSpace(fieldString(arg, "sessionId"))
	requestedTabID, requestedChatID := fieldString(arg, "tabId"), fieldString(arg, "chatId")
	actor, _, laneID, generation, err := r.actorForSteerSession(sessionID, requestedTabID, requestedChatID)
	if err != nil {
		return nil, true, err
	}
	prompt := strings.TrimSpace(fieldString(arg, "prompt"))
	continuationID := strings.TrimSpace(fieldString(arg, "continuationAssistantMessageId"))
	operationID := providercontract.NormalizeOperationID(fieldString(arg, "clientUserMessageId"))
	if operationID == "" {
		return nil, true, errors.New("live steer requires a stable client user message id")
	}
	rawImages := sliceArg(arg["images"])
	attachmentPlan, err := r.sessions.PlanProviderAttachments(rawImages)
	if err != nil {
		return nil, true, err
	}
	inputAttachments := attachmentPlan.Attachments

	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.Snapshot()
	if currentLaneID, currentGeneration, targetErr := steerAttachmentTarget(state, sessionID, requestedTabID, requestedChatID); targetErr != nil || currentLaneID != laneID || currentGeneration != generation {
		if targetErr != nil {
			return nil, true, targetErr
		}
		return nil, true, errors.New("steer session attachment generation is stale")
	}
	if durable, found := durableSteerInputForOperation(state, operationID); found {
		if !sameSteerImmutableInput(durable, operationID, prompt, inputAttachments, continuationID) {
			return nil, true, errors.New("steer operation id was reused for different content")
		}
		result, readErr := r.durableSteerReply(state, operationID)
		return result, true, readErr
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, true, errors.New("steer operation id already belongs to a different actor command")
	}
	if err := steerForegroundTarget(state, laneID, generation, sessionID); err != nil {
		// A steer arriving after the foreground has already ended is the
		// provider-neutral FIFO fallback, not a rejected operation. Materialize
		// its attachments only inside the accepted actor command, then let the
		// existing queue reducer own the one immutable operation.
		if steerForegroundEnded(state) {
			presentation := providercontract.TurnPresentation{
				UserMessageID: string(operationID), AssistantMessageID: continuationID,
				PromptText: prompt, Origin: "human", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			}
			return r.queueActorFallbackLocked(ctx, actor, operationID, prompt, attachmentPlan, presentation, "the foreground turn ended before steering admission")
		}
		return nil, true, err
	}
	if prompt == "" && len(inputAttachments) == 0 {
		return nil, true, errors.New("steer requires text or attachments")
	}
	presentation := providercontract.TurnPresentation{
		UserMessageID: string(operationID), AssistantMessageID: continuationID,
		PromptText: prompt, Origin: "human", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	// Revalidate immediately before the actor command so a terminal event that
	// won the race transfers the same immutable operation into FIFO exactly once.
	state = actor.engine.Snapshot()
	if err := steerForegroundTarget(state, laneID, generation, sessionID); err != nil {
		if steerForegroundEnded(state) {
			return r.queueActorFallbackLocked(ctx, actor, operationID, prompt, attachmentPlan, presentation, "the foreground turn ended before steering admission")
		}
		return nil, true, err
	}
	if err := actor.engine.ApplyPrepared(chat.Steer{
		OperationID: operationID, Text: prompt, Attachments: inputAttachments, Presentation: presentation,
	}, attachmentPlan.Materialize); err != nil {
		state = actor.engine.Snapshot()
		if _, exists := state.Operations[operationID]; exists {
			result, readErr := r.durableSteerReply(state, operationID)
			return result, true, readErr
		}
		if steerForegroundEnded(state) {
			return r.queueActorFallbackLocked(ctx, actor, operationID, prompt, attachmentPlan, presentation, "the foreground turn ended before steering admission")
		}
		return nil, true, err
	}

	// The manager is now only an executor authority. A stale/mismatched
	// transient session cannot receive the durable steer; turn it into the
	// provider-neutral FIFO disposition through the actor instead.
	state = actor.engine.Snapshot()
	live, liveOK := r.manager.LiveSessionByID(sessionID)
	if !liveOK || strings.TrimSpace(live.ChatID) != strings.TrimSpace(state.ChatID) || strings.TrimSpace(live.TabID) != strings.TrimSpace(state.Presentation.TabID) || strings.TrimSpace(live.Info.SessionID) != sessionID {
		if applyErr := actor.engine.Apply(chat.SteerFailed{OperationID: operationID, Kind: providercontract.ErrorAdmissionRejected}); applyErr != nil {
			return nil, true, applyErr
		}
		result, readErr := r.durableSteerReply(actor.engine.Snapshot(), operationID)
		return result, true, readErr
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return nil, true, err
	}
	result, readErr := r.durableSteerReply(actor.engine.Snapshot(), operationID)
	return result, true, readErr
}

func (r *providerChatRuntime) queueActorFallbackLocked(
	ctx context.Context,
	actor *providerChatActor,
	operationID providercontract.OperationID,
	prompt string,
	attachmentPlan providerAttachmentPlan,
	presentation providercontract.TurnPresentation,
	reason string,
) (map[string]any, bool, error) {
	state := actor.engine.Snapshot()
	target := state.DesiredLaneID
	if target == "" {
		target = state.ActiveLaneID
	}
	if target == "" {
		return nil, true, errors.New("steer fallback has no selected provider lane")
	}
	presentation.QueueID = firstNonEmptyString(presentation.QueueID, nextSessionID("q"))
	if err := actor.engine.ApplyPrepared(chat.Submit{
		OperationID: operationID, LaneID: target, Text: prompt, Attachments: attachmentPlan.Attachments,
		Presentation: presentation,
	}, attachmentPlan.Materialize); err != nil {
		return nil, true, err
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return nil, true, err
	}
	receipt := queuedActorReceipt(actor.engine.Snapshot(), operationID, presentation.QueueID)
	receipt["ok"] = false
	receipt["strategy"] = "queue"
	receipt["daemonQueued"] = true
	receipt["error"] = strings.TrimSpace(reason)
	return receipt, true, nil
}

func queuedSteerReceipt(state chat.State, operationID providercontract.OperationID, kind providercontract.ErrorKind) map[string]any {
	return queuedSteerReceiptWithQueueID(state, operationID, "", kind)
}

func steerQueueErrorKind(state chat.State, operationID providercontract.OperationID, fallback providercontract.ErrorKind) providercontract.ErrorKind {
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectSteerTurn && entry.OperationID == operationID && entry.Status == chat.OutboxCompleted && entry.LastError != "" {
			return entry.LastError
		}
	}
	return fallback
}

func queuedSteerReceiptWithQueueID(state chat.State, operationID providercontract.OperationID, queueID string, kind providercontract.ErrorKind) map[string]any {
	queueID = strings.TrimSpace(queueID)
	for _, entry := range state.Queue {
		if entry.OperationID == operationID {
			queueID = firstNonEmptyString(entry.Presentation.QueueID, queueID)
			break
		}
	}
	receipt := queuedActorReceipt(state, operationID, queueID)
	receipt["ok"] = false
	receipt["strategy"] = "queue"
	receipt["daemonQueued"] = true
	receipt["error"] = string(kind)
	return receipt
}

// SteerQueued is the agent-control form: the durable session FIFO row already
// owns presentation, so the actor only decides live admission versus keeping
// that same row queued for the next turn.
func (r *providerChatRuntime) SteerQueued(ctx context.Context, tabID, chatID, queueID, message string) (map[string]any, bool, error) {
	if r == nil {
		return nil, false, nil
	}
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, true, err
	}
	actor.mu.Lock()
	needsDrain, result, err := r.steerQueuedLocked(actor, tabID, chatID, queueID, message)
	actor.mu.Unlock()
	if err != nil || !needsDrain {
		return result, true, err
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return nil, true, err
	}
	actor.mu.Lock()
	result, err = r.agentSteerQueuedResultLocked(actor.engine.Snapshot(), providercontract.NormalizeOperationID(queueID))
	actor.mu.Unlock()
	return result, true, err
}

func (r *providerChatRuntime) actorSnapshot() []*providerChatActor {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	actors := make([]*providerChatActor, 0, len(r.actors))
	for _, actor := range r.actors {
		actors = append(actors, actor)
	}
	r.mu.Unlock()
	return actors
}

// Cancel targets the actor lane that owns the native turn. Terminal and unknown
// ids are also answered from actor state so the frozen wire behavior never
// falls back to the manager's transient job cache.
func (r *providerChatRuntime) Cancel(ctx context.Context, jobID string) (acp.JobCancelResult, bool, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return acp.JobCancelResult{Cancelled: false, Reason: "unknown"}, true, nil
	}
	operationID := providercontract.OperationID("cancel:" + jobID)
	for _, actor := range r.actorSnapshot() {
		actor.mu.Lock()
		state := actor.engine.Snapshot()
		if state.Foreground == nil || state.Foreground.Turn.NativeID != jobID {
			actor.mu.Unlock()
			continue
		}
		if _, exists := state.Operations[operationID]; !exists {
			if err := actor.engine.Apply(chat.CancelTurn{OperationID: operationID}); err != nil {
				actor.mu.Unlock()
				return acp.JobCancelResult{}, true, err
			}
		}
		err := actor.coordinator.Drain(ctx)
		state = actor.engine.Snapshot()
		actor.mu.Unlock()
		if err != nil {
			return acp.JobCancelResult{}, true, err
		}
		for _, entry := range state.Outbox {
			if entry.Kind != chat.EffectCancelTurn || entry.OperationID != operationID {
				continue
			}
			switch entry.Status {
			case chat.OutboxAccepted, chat.OutboxCompleted:
				return acp.JobCancelResult{Cancelled: true, Reason: "cancelled"}, true, nil
			case chat.OutboxAmbiguous:
				return acp.JobCancelResult{Cancelled: false, Reason: "uncertain"}, true, nil
			case chat.OutboxFailed:
				return acp.JobCancelResult{Cancelled: false, Reason: "not-owned"}, true, nil
			}
		}
		return acp.JobCancelResult{Cancelled: false, Reason: "pending"}, true, nil
	}
	for _, actor := range r.actorSnapshot() {
		state := actor.engine.Snapshot()
		for _, event := range state.Ledger {
			if strings.TrimSpace(event.NativeTurnID) == jobID {
				return acp.JobCancelResult{Cancelled: false, Reason: "idle"}, true, nil
			}
		}
	}
	return acp.JobCancelResult{Cancelled: false, Reason: "unknown"}, true, nil
}

// ResolvePermission preserves the origin lane recorded by the normalized
// provider event even when the user has selected another provider meanwhile.
func (r *providerChatRuntime) ResolvePermission(ctx context.Context, requestID, optionID string) (bool, bool, error) {
	requestID, optionID = strings.TrimSpace(requestID), strings.TrimSpace(optionID)
	if requestID == "" || optionID == "" {
		return false, true, nil
	}
	operationID := providercontract.OperationID("permission:" + requestID + ":" + optionID)
	for _, actor := range r.actorSnapshot() {
		actor.mu.Lock()
		state := actor.engine.Snapshot()
		permission, exists := state.Permissions[requestID]
		if !exists {
			actor.mu.Unlock()
			continue
		}
		if _, exists := state.Operations[operationID]; !exists {
			if err := actor.engine.Apply(chat.ResolvePermission{
				OperationID: operationID, RequestID: requestID, OptionID: optionID,
			}); err != nil {
				actor.mu.Unlock()
				return false, true, err
			}
		}
		err := actor.coordinator.Drain(ctx)
		state = actor.engine.Snapshot()
		actor.mu.Unlock()
		if err != nil {
			return false, true, err
		}
		permission = state.Permissions[requestID]
		switch permission.Event.Status {
		case "resolved":
			return true, true, nil
		case "uncertain":
			return false, true, &providercontract.Error{
				Kind: providercontract.ErrorAcceptanceAmbiguous, Operation: operationID,
				Message: "permission decision acceptance is uncertain",
			}
		case "failed":
			return false, true, nil
		default:
			_ = permission
			return false, true, nil
		}
	}
	if strings.TrimSpace(r.manager.PermissionChatID(requestID)) != "" {
		return false, true, errors.New("actor-owned permission request is missing from durable chat state")
	}
	return false, false, nil
}

func (r *providerChatRuntime) PendingPermissions() ([]any, error) {
	ids, err := r.knownChatIDs()
	if err != nil {
		return nil, err
	}
	out := make([]any, 0)
	for _, chatID := range ids {
		actor, err := r.actor(chatID)
		if err != nil {
			return nil, err
		}
		state := actor.engine.Snapshot()
		if state.Deleted {
			continue
		}
		requestIDs := make([]string, 0, len(state.Permissions))
		for requestID := range state.Permissions {
			requestIDs = append(requestIDs, requestID)
		}
		sort.Strings(requestIDs)
		for _, requestID := range requestIDs {
			permission := state.Permissions[requestID]
			projected := projectPermission(&permission.Event)
			if projected == nil {
				continue
			}
			projected["jobId"] = nullableString(permission.Owner.TurnID)
			projected["tabId"] = nullableString(state.Presentation.TabID)
			projected["chatId"] = nullableString(state.ChatID)
			if lane, ok := state.Lanes[permission.Owner.LaneID]; ok {
				projected["sessionId"] = nullableString(lane.Thread.HeadID)
			} else {
				projected["sessionId"] = nil
			}
			out = append(out, projected)
		}
	}
	return out, nil
}

func (r *providerChatRuntime) ReplayPendingPermissions(send func(string, any) error) error {
	if send == nil {
		return nil
	}
	permissions, err := r.PendingPermissions()
	if err != nil {
		return err
	}
	for _, payload := range permissions {
		if err := send("chat:permission-request", payload); err != nil {
			return err
		}
	}
	return nil
}

func (r *providerChatRuntime) Snapshot(chatID string) (chat.State, bool) {
	if r == nil {
		return chat.State{}, false
	}
	r.mu.Lock()
	actor := r.actors[strings.TrimSpace(chatID)]
	r.mu.Unlock()
	if actor == nil {
		return chat.State{}, false
	}
	return actor.engine.Snapshot(), true
}

func (r *providerChatRuntime) runObligationReconciliation(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = r.reconcileObligations(false)
		}
	}
}

func (r *providerChatRuntime) reconcileObligations(daemonBoot bool) error {
	ids, err := r.knownChatIDs()
	if err != nil {
		return err
	}
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for _, chatID := range ids {
		actor, err := r.actor(chatID)
		if err != nil {
			return err
		}
		state := actor.engine.Snapshot()
		if state.Deleted || state.Migration.BlockedError != "" || state.Obligation == nil {
			continue
		}
		evidence := r.manager.ChatObligationEvidence(state.Presentation.TabID, state.ChatID)
		actor.mu.Lock()
		err = actor.engine.Apply(chat.ReconcileObligation{
			ObservedAt: observedAt, LiveEvidence: evidence.Live,
			HarnessQuiet: evidence.HarnessQuiet, DaemonBoot: daemonBoot,
		})
		actor.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *providerChatRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if r.cancel != nil {
		r.cancel()
		r.wg.Wait()
	}
	if r.manager != nil {
		r.manager.SetChatEnvObserver(nil)
		r.manager.SetChatEnvRestorer(nil)
	}
	r.mu.Lock()
	actors := make([]*providerChatActor, 0, len(r.actors))
	for _, actor := range r.actors {
		actors = append(actors, actor)
	}
	r.actors = make(map[string]*providerChatActor)
	r.mu.Unlock()
	var closeErr error
	for _, actor := range actors {
		actor.mu.Lock()
		err := actor.coordinator.Close(ctx)
		actor.mu.Unlock()
		closeErr = errors.Join(closeErr, err)
	}
	return closeErr
}
