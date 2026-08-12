package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	providercontract "workass/internal/provider"
)

// initProviderRegistry builds the semantic registry from the exact normalized
// provider runtimes already owned by Manager. It is intentionally not another
// source of provider configuration: every definition closes over the same
// runtime record used by discovery, catalog, launch, and updates.
func (m *Manager) initProviderRegistry() {
	registry := providercontract.NewRegistry()
	for _, id := range m.providerOrder {
		runtime := m.providers[id]
		if runtime == nil {
			continue
		}
		fixture := false
		authentication := providercontract.AuthenticationStrategy(noProviderAuthenticationStrategy{})
		if registration, ok := providerRegistrationForID(id); ok {
			fixture = registration.FixtureOnly
			if registration.Authentication != nil {
				authentication = registration.Authentication
			}
		}
		definition := providercontract.Definition{
			Identity: providercontract.ProviderIdentity{
				ID: providercontract.ID(id), DisplayName: runtime.Config.Name, FixtureOnly: fixture,
			},
			Realm:          managerRealmResolver{manager: m, providerID: id},
			Runtime:        managerLaneFactory{manager: m, providerID: id},
			Metadata:       managerMetadataSource{manager: m, providerID: id},
			Update:         managerUpdateStrategy{manager: m, providerID: id},
			Authentication: authentication,
		}
		if err := registry.Register(definition); err != nil {
			m.providerRegistryErr = errors.Join(m.providerRegistryErr, err)
		}
	}
	m.providerRegistry = registry
	if m.providerRegistryErr != nil {
		m.opts.Logf("provider registry initialization failed", map[string]any{
			"error": redactSensitiveText(m.providerRegistryErr.Error()),
		})
	}
}

// ProviderDefinition returns the one registered semantic definition for an
// enabled or disabled configured provider. Callers never infer capabilities
// from branding; they consume the definition's typed facets.
func (m *Manager) ProviderDefinition(providerID string) (providercontract.Definition, error) {
	if m == nil || m.providerRegistry == nil {
		return providercontract.Definition{}, errors.New("provider registry is unavailable")
	}
	if m.providerRegistryErr != nil {
		return providercontract.Definition{}, fmt.Errorf("provider registry is invalid: %w", m.providerRegistryErr)
	}
	id := providercontract.NormalizeID(providerID)
	if id == "" {
		id = providercontract.ID(m.defaultProviderID)
	}
	definition, ok := m.providerRegistry.Resolve(id)
	if !ok {
		return providercontract.Definition{}, fmt.Errorf("unknown provider definition %q", id)
	}
	if err := definition.Validate(); err != nil {
		return providercontract.Definition{}, fmt.Errorf("invalid provider definition %q: %w", id, err)
	}
	return definition, nil
}

func (m *Manager) providerAuthenticationStrategy(providerID string) (providercontract.AuthenticationStrategy, error) {
	definition, err := m.ProviderDefinition(providerID)
	if err != nil {
		return nil, err
	}
	if definition.Authentication == nil {
		return nil, fmt.Errorf("provider definition %q has no authentication strategy", normalizeProviderID(providerID))
	}
	return definition.Authentication, nil
}

// Resolve lets Manager serve as the chat coordinator's immutable definition
// resolver without exposing Registry.Register to runtime consumers.
func (m *Manager) Resolve(id providercontract.ID) (providercontract.Definition, bool) {
	definition, err := m.ProviderDefinition(string(id))
	return definition, err == nil
}

// ProviderLaneSelection is a read-only resolution of one chat/provider choice.
// Established means Identity+Thread came from the daemon-owned provider lane
// ledger. Otherwise Identity is only a proposal for the one real create call;
// the provider may attest a stronger account/install realm in that reply.
type ProviderLaneSelection struct {
	Identity    providercontract.LaneIdentity
	Thread      providercontract.ThreadRef
	Owner       providercontract.AttachmentOwner
	CWD         string
	ModelID     string
	ModeID      string
	Context     providercontract.ContextCapabilities
	Delivery    providercontract.DeliveryCapabilities
	Established bool
}

// LegacyCoverageMessage is the exact old cursor input used only by the one-time
// renderer/session-to-actor migration. It is deliberately smaller than the
// actor ledger and must be deleted with the cutover reader after rollout.
type LegacyCoverageMessage struct {
	ID      string
	Role    string
	Content string
}

type LegacyProviderLaneMigration struct {
	Selection       ProviderLaneSelection
	CoveredMessages int
	BlockedError    providercontract.ErrorKind
}

// LegacyProviderChatInventoryItem is the minimal, non-secret identity needed
// to ensure the one-time cutover accounts for native bindings that disappeared
// from the renderer mirror. Such a chat is quarantined; it is never silently
// deleted or adopted into another visible chat.
type LegacyProviderChatInventoryItem struct {
	ChatID     string
	TabID      string
	ProviderID string
	CWD        string
}

func (m *Manager) LegacyProviderChatInventory() ([]LegacyProviderChatInventoryItem, error) {
	if m == nil || m.nativeSessions == nil {
		return nil, errors.New("legacy provider lane inventory is unavailable")
	}
	bindings := m.nativeSessions.allBindings()
	byChat := make(map[string]LegacyProviderChatInventoryItem)
	for _, binding := range bindings {
		chatID := strings.TrimSpace(binding.ChatID)
		if chatID == "" {
			continue
		}
		item, exists := byChat[chatID]
		if !exists {
			byChat[chatID] = LegacyProviderChatInventoryItem{
				ChatID: chatID, TabID: strings.TrimSpace(binding.TabID),
				ProviderID: normalizeProviderID(binding.ProviderID), CWD: strings.TrimSpace(binding.CWD),
			}
			continue
		}
		// Conflicting attachment metadata is itself ambiguity. Leave the fields
		// empty so the cutover creates a deterministic quarantine attachment.
		if item.TabID != strings.TrimSpace(binding.TabID) {
			item.TabID = ""
		}
		if item.ProviderID != normalizeProviderID(binding.ProviderID) {
			item.ProviderID = ""
		}
		if item.CWD != strings.TrimSpace(binding.CWD) {
			item.CWD = ""
		}
		byChat[chatID] = item
	}
	out := make([]LegacyProviderChatInventoryItem, 0, len(byChat))
	for _, item := range byChat {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChatID < out[j].ChatID })
	return out, nil
}

// LegacyProviderLaneMigrations enumerates every exact native binding for one
// pre-actor chat and verifies the old history cursor without starting a
// provider process. It never creates, resumes, probes, or repairs a thread.
func (m *Manager) LegacyProviderLaneMigrations(chatID string, messages []LegacyCoverageMessage) ([]LegacyProviderLaneMigration, error) {
	if m == nil || m.nativeSessions == nil {
		return nil, errors.New("legacy provider lane migration is unavailable")
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil, errors.New("legacy provider lane migration requires chat id")
	}
	bindings := m.nativeSessions.bindingsForChat(chatID)
	out := make([]LegacyProviderLaneMigration, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Quarantined {
			return nil, &providercontract.Error{
				Kind:    providercontract.ErrorNativeIdentityConflict,
				Message: firstNonEmpty(binding.QuarantineReason, "legacy provider binding is quarantined"),
			}
		}
		identity, thread := bindingLaneIdentity(binding), bindingThreadRef(binding)
		if err := identity.Validate(); err != nil {
			return nil, &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "legacy provider lane identity is invalid", Cause: err}
		}
		if err := thread.Validate(identity.Realm.ProviderID); err != nil {
			return nil, &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "legacy provider thread identity is invalid", Cause: err}
		}
		if _, err := m.ProviderDefinition(binding.ProviderID); err != nil {
			return nil, &providercontract.Error{Kind: providercontract.ErrorProviderUnavailable, Message: "legacy provider lane is not registered", Cause: err}
		}
		migration := LegacyProviderLaneMigration{Selection: ProviderLaneSelection{
			Identity: identity, Thread: thread,
			Owner: providercontract.AttachmentOwner{TabID: binding.TabID},
			CWD:   binding.CWD, ModelID: binding.ModelID, ModeID: binding.ModeID,
			Context: providerAdapterForID(binding.ProviderID).context.Capabilities(), Established: true,
		}}
		count := binding.SyncedMessages
		switch {
		case count < 0 || count > len(messages):
			migration.BlockedError = providercontract.ErrorNativeIdentityConflict
		case count == 0 && strings.TrimSpace(binding.HistoryHash) == "":
			// Old empty bindings may predate cursor hashing. They prove no visible
			// coverage, which is safe and intentionally does not guess.
		case binding.HistoryVersion != 1 && binding.HistoryVersion != 2:
			migration.BlockedError = providercontract.ErrorNativeIdentityConflict
		case !legacyCoverageDigestMatches(binding, messages[:count]):
			migration.BlockedError = providercontract.ErrorNativeIdentityConflict
		default:
			migration.CoveredMessages = count
		}
		if migration.BlockedError == "" && !binding.ResumeSafe {
			migration.BlockedError = providercontract.ErrorAcceptanceAmbiguous
		}
		out = append(out, migration)
	}
	return out, nil
}

func legacyCoverageDigestMatches(binding nativeSessionBinding, messages []LegacyCoverageMessage) bool {
	hash := sha256.New()
	for _, message := range messages {
		if binding.HistoryVersion > 1 {
			hash.Write([]byte(strings.TrimSpace(message.ID)))
			hash.Write([]byte{0xfe})
		}
		hash.Write([]byte(strings.TrimSpace(message.Role)))
		hash.Write([]byte{0})
		hash.Write([]byte(strings.TrimSpace(message.Content)))
		hash.Write([]byte{0xff})
	}
	return strings.TrimSpace(binding.HistoryHash) == hex.EncodeToString(hash.Sum(nil))
}

// ResolveProviderLaneSelection performs no provider RPC. It is the single
// identity resolver used by the durable chat actor before SelectLane. Existing
// bindings are returned exactly; new selections receive a conservative realm
// proposal that can be canonicalized only by the eventual create receipt.
func (m *Manager) ResolveProviderLaneSelection(ctx context.Context, opts SessionOptions) (ProviderLaneSelection, error) {
	if m == nil || m.nativeSessions == nil || m.nativeSessions.path == "" {
		return ProviderLaneSelection{}, errors.New("durable provider lane storage is unavailable")
	}
	opts.TabID = strings.TrimSpace(opts.TabID)
	opts.ChatID = strings.TrimSpace(opts.ChatID)
	if opts.TabID == "" || opts.ChatID == "" {
		return ProviderLaneSelection{}, errors.New("provider lane selection requires tab and chat ids")
	}
	if strings.TrimSpace(opts.CWD) == "" {
		opts.CWD = m.opts.RootDir
	}
	opts.ProviderLaneManaged = true
	m.mu.Lock()
	providerID, err := m.resolveSessionProviderLocked(opts)
	m.mu.Unlock()
	if err != nil {
		return ProviderLaneSelection{}, err
	}
	opts.ProviderID = providerID
	opts = m.withNativeSessionControls(opts)
	contextCapabilities := providerAdapterForID(providerID).context.Capabilities()
	selection := ProviderLaneSelection{
		Owner: providercontract.AttachmentOwner{TabID: opts.TabID, AgentOwnerKey: strings.TrimSpace(opts.AgentOwnerKey)},
		CWD:   strings.TrimSpace(opts.CWD), ModelID: strings.TrimSpace(opts.ModelID), ModeID: strings.TrimSpace(opts.ModeID),
		Context: contextCapabilities,
	}
	if binding, exists := m.nativeSessions.getForWorkspace(opts.TabID, opts.ChatID, providerID, opts.CWD); exists {
		if binding.Quarantined {
			return ProviderLaneSelection{}, &providercontract.Error{
				Kind:    providercontract.ErrorNativeIdentityConflict,
				Message: firstNonEmpty(binding.QuarantineReason, "provider lane identity is quarantined"),
			}
		}
		identity, thread := bindingLaneIdentity(binding), bindingThreadRef(binding)
		if err := identity.Validate(); err != nil {
			return ProviderLaneSelection{}, &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Message: "stored provider lane identity is invalid", Cause: err}
		}
		if err := thread.Validate(identity.Realm.ProviderID); err != nil {
			return ProviderLaneSelection{}, &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Message: "stored provider thread is invalid", Cause: err}
		}
		if requested := strings.TrimSpace(opts.CWD); requested != "" && strings.TrimSpace(binding.CWD) != "" && !sameFilesystemPath(requested, binding.CWD) {
			return ProviderLaneSelection{}, &providercontract.Error{
				Kind:    providercontract.ErrorNativeIdentityConflict,
				Message: "the requested workspace does not match the provider lane's workspace epoch",
			}
		}
		selection.Identity = identity
		selection.Thread = thread
		selection.CWD = firstNonEmpty(strings.TrimSpace(binding.CWD), selection.CWD)
		selection.ModelID = firstNonEmpty(strings.TrimSpace(opts.ModelID), binding.ModelID)
		selection.ModeID = firstNonEmpty(strings.TrimSpace(opts.ModeID), binding.ModeID)
		selection.Established = true
		return selection, nil
	}
	definition, err := m.ProviderDefinition(providerID)
	if err != nil {
		return ProviderLaneSelection{}, err
	}
	realm, err := definition.Realm.ResolveRealm(ctx, providercontract.RealmRequest{
		ProviderID: providercontract.ID(providerID), MachineID: m.nativeSessions.machineID,
		TabID: opts.TabID, ChatID: opts.ChatID,
	})
	if err != nil {
		return ProviderLaneSelection{}, err
	}
	selection.Identity = providercontract.LaneIdentity{
		ChatID: opts.ChatID, Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(opts.CWD),
	}.Normalize()
	if err := selection.Identity.Validate(); err != nil {
		return ProviderLaneSelection{}, err
	}
	return selection, nil
}

// LiveProviderLaneInfo returns the transient wire-facing session information
// only when the exact resolved native head is attached in this process.
func (m *Manager) LiveProviderLaneInfo(selection ProviderLaneSelection) (SessionInfo, bool) {
	thread := selection.Thread.Normalize()
	if thread.HeadID == "" {
		return SessionInfo{}, false
	}
	live, ok := m.LiveSession(thread.HeadID)
	if !ok || live.ChatID != selection.Identity.ChatID || live.Info.ProviderID != string(selection.Identity.Realm.ProviderID) {
		return SessionInfo{}, false
	}
	return live.Info, true
}

// SetProviderAttachmentResolver installs the daemon's one content-addressed
// attachment reader. Provider adapters receive ordinary payloads only after
// the shared host boundary has verified the immutable reference.
func (m *Manager) SetProviderAttachmentResolver(resolver func(context.Context, providercontract.Attachment) (any, error)) {
	if m == nil {
		return
	}
	m.providerAttachmentMu.Lock()
	m.providerAttachmentResolver = resolver
	m.providerAttachmentMu.Unlock()
}

// SetProviderAttachmentPersister installs the provider-neutral media ingress
// used by assistant/tool events. The ACP adapter never persists base64 in lane
// events; it converts renderer-safe image blocks to immutable daemon refs first.
func (m *Manager) SetProviderAttachmentPersister(persister func([]any) ([]providercontract.Attachment, error)) {
	if m == nil {
		return
	}
	m.providerAttachmentMu.Lock()
	m.providerAttachmentPersister = persister
	m.providerAttachmentMu.Unlock()
}

func (m *Manager) persistProviderEventAttachments(images []any) ([]providercontract.Attachment, error) {
	if len(images) == 0 {
		return nil, nil
	}
	m.providerAttachmentMu.RLock()
	persister := m.providerAttachmentPersister
	m.providerAttachmentMu.RUnlock()
	if persister == nil {
		return nil, errors.New("provider event attachment storage is unavailable")
	}
	return persister(images)
}

func (m *Manager) resolveProviderAttachment(ctx context.Context, attachment providercontract.Attachment) (any, error) {
	m.providerAttachmentMu.RLock()
	resolver := m.providerAttachmentResolver
	m.providerAttachmentMu.RUnlock()
	if resolver == nil {
		return attachment.Ref, nil
	}
	return resolver(ctx, attachment)
}

const providerAdmissionReceiptLimit = 2048

func providerAdmissionKey(tabID, chatID string, operationID providercontract.OperationID) string {
	_ = strings.TrimSpace(tabID) // compatibility parameter; tab is not ownership.
	return strings.TrimSpace(chatID) + "\x00" + string(providercontract.NormalizeOperationID(string(operationID)))
}

// recordProviderLaneAdmission bridges the provider-neutral actor back to the
// frozen job:start reply without making the renderer query transient manager
// state. Fast providers may finish before StartTurn returns, so RunningJobForChat
// is not an admission receipt.
func (m *Manager) recordProviderLaneAdmission(tabID, chatID string, operationID providercontract.OperationID, job map[string]any) {
	if m == nil || operationID == "" || strings.TrimSpace(asString(job["id"])) == "" {
		return
	}
	key := providerAdmissionKey(tabID, chatID, operationID)
	copyJob := cloneMap(job)
	m.providerAdmissionMu.Lock()
	if _, exists := m.providerAdmissions[key]; !exists {
		m.providerAdmissionOrder = append(m.providerAdmissionOrder, key)
	}
	m.providerAdmissions[key] = copyJob
	for len(m.providerAdmissionOrder) > providerAdmissionReceiptLimit {
		oldest := m.providerAdmissionOrder[0]
		m.providerAdmissionOrder = m.providerAdmissionOrder[1:]
		delete(m.providerAdmissions, oldest)
	}
	m.providerAdmissionMu.Unlock()
}

// ProviderLaneAdmission reads the exact admission receipt for one durable chat
// operation. Receipts remain in the bounded cache so an invoke retry can return
// the same public job rather than submitting the operation again.
func (m *Manager) ProviderLaneAdmission(tabID, chatID string, operationID providercontract.OperationID) (map[string]any, bool) {
	if m == nil || operationID == "" {
		return nil, false
	}
	key := providerAdmissionKey(tabID, chatID, operationID)
	m.providerAdmissionMu.Lock()
	job, ok := m.providerAdmissions[key]
	m.providerAdmissionMu.Unlock()
	return cloneMap(job), ok
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

type managerRealmResolver struct {
	manager    *Manager
	providerID string
}

func (r managerRealmResolver) ResolveRealm(_ context.Context, request providercontract.RealmRequest) (providercontract.Realm, error) {
	if r.manager == nil {
		return providercontract.Realm{}, errors.New("provider realm resolver has no manager")
	}
	providerID := normalizeProviderID(string(request.ProviderID))
	if providerID == "" {
		providerID = r.providerID
	}
	if providerID != r.providerID {
		return providercontract.Realm{}, fmt.Errorf("realm request for %q reached provider %q", providerID, r.providerID)
	}
	machineID := strings.TrimSpace(request.MachineID)
	if machineID == "" && r.manager.nativeSessions != nil {
		machineID = r.manager.nativeSessions.machineID
	}
	accountScope := strings.TrimSpace(request.AccountScope)
	installScope := strings.TrimSpace(request.InstallScope)
	verified := request.Verified
	if r.manager.nativeSessions != nil && strings.TrimSpace(request.TabID) != "" && strings.TrimSpace(request.ChatID) != "" {
		if binding, ok := r.manager.nativeSessions.get(request.TabID, request.ChatID, providerID); ok && !binding.Quarantined {
			machineID = firstNonEmpty(binding.MachineID, machineID)
			accountScope = firstNonEmpty(binding.AccountScope, accountScope)
			installScope = firstNonEmpty(binding.InstallScope, installScope)
			verified = binding.RealmVerified
		}
	}
	realm := providercontract.Realm{
		ProviderID: providercontract.ID(providerID), MachineID: machineID,
		AccountScope: firstNonEmpty(accountScope, "unverified-account"),
		InstallScope: firstNonEmpty(installScope, managerProviderInstallScope(r.manager, providerID)),
		Verified:     verified,
	}.Normalize()
	if err := realm.Validate(); err != nil {
		return providercontract.Realm{}, err
	}
	return realm, nil
}

// managerProviderInstallScope is the conservative proposal used before a new
// native thread can attest its own account/install realm. Values are hashed and
// never logged. Provider replies may canonicalize this proposal exactly once,
// while the lane still has no thread; established lanes always resolve from
// their daemon-owned binding above.
func managerProviderInstallScope(manager *Manager, providerID string) string {
	parts := []string{normalizeProviderID(providerID)}
	if manager != nil {
		parts = append(parts, strings.TrimSpace(manager.opts.StateDir))
		manager.mu.Lock()
		if runtime := manager.providers[normalizeProviderID(providerID)]; runtime != nil {
			parts = append(parts,
				strings.TrimSpace(runtime.Config.Command),
				strings.TrimSpace(runtime.Config.ResolvedCommand),
				strings.TrimSpace(runtime.Config.CWD),
				strings.Join(runtime.Config.Args, "\x00"),
			)
			keys := make([]string, 0, len(runtime.Config.Env)+len(runtime.Config.AutoEnv))
			for key := range runtime.Config.Env {
				keys = append(keys, "env:"+key)
			}
			for key := range runtime.Config.AutoEnv {
				keys = append(keys, "auto-env:"+key)
			}
			sort.Strings(keys)
			parts = append(parts, keys...)
		}
		manager.mu.Unlock()
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "install-" + hex.EncodeToString(digest[:16])
}

type managerLaneFactory struct {
	manager    *Manager
	providerID string
}

func (f managerLaneFactory) Create(ctx context.Context, request providercontract.CreateLaneRequest) (providercontract.Lane, providercontract.ThreadRef, error) {
	identity, opts, err := f.validateCreate(request)
	if err != nil {
		return nil, providercontract.ThreadRef{}, err
	}
	if binding, exists := f.manager.nativeSessions.getForWorkspace(opts.TabID, opts.ChatID, opts.ProviderID, opts.CWD); exists {
		if request.Reconcile && !binding.Quarantined {
			canonical := bindingLaneIdentity(binding)
			if err := validateCanonicalCreatedLane(identity, canonical); err != nil {
				return nil, providercontract.ThreadRef{}, err
			}
			thread := bindingThreadRef(binding)
			lane, err := f.Resume(ctx, providercontract.ResumeLaneRequest{
				Identity: canonical, Thread: thread, Owner: request.Owner, CWD: request.CWD,
				ModelID: request.ModelID, ModeID: request.ModeID,
			})
			return lane, thread, err
		}
		kind := providercontract.ErrorNativeIdentityConflict
		message := "an established provider lane cannot enter create"
		if binding.Quarantined {
			message = firstNonEmpty(binding.QuarantineReason, message)
		}
		return nil, providercontract.ThreadRef{}, &providercontract.Error{Kind: kind, Message: message}
	}
	if request.Reconcile {
		return nil, providercontract.ThreadRef{}, &providercontract.Error{
			Kind:    providercontract.ErrorAcceptanceAmbiguous,
			Message: "provider create has no durable native binding to reconcile; Workass will not issue another create",
		}
	}
	// Exact resume is mandatory for a durable lane. Negotiate it before the one
	// legal session/new call so an ACP provider that cannot resume never leaves
	// behind a native thread Workass is structurally unable to own.
	bridge := f.manager.getBridge(opts)
	if bridge == nil {
		if _, configErr := f.manager.providerConfig(f.providerID); configErr != nil {
			return nil, providercontract.ThreadRef{}, configErr
		}
		return nil, providercontract.ThreadRef{}, &providercontract.Error{
			Kind: providercontract.ErrorProviderUnavailable, Message: "provider bridge is unavailable",
		}
	}
	if _, err := bridge.Initialize(ctx); err != nil {
		if hint, policyErr := f.manager.markProviderNeedsLogin(ctx, f.providerID, err); policyErr != nil {
			return nil, providercontract.ThreadRef{}, policyErr
		} else if hint != "" {
			return nil, providercontract.ThreadRef{}, providerAuthenticationFailureError(f.providerID, err, hint)
		}
		return nil, providercontract.ThreadRef{}, classifyLaneRuntimeError("initialize lane", err)
	}
	if !bridge.supportsSessionResume() {
		return nil, providercontract.ThreadRef{}, providercontract.Unsupported("create-lane", "provider does not expose exact session/resume")
	}
	info, err := f.manager.NewSession(ctx, opts)
	if err != nil {
		return nil, providercontract.ThreadRef{}, classifyLaneRuntimeError("create lane", err)
	}
	binding, ok := f.manager.nativeSessions.getForWorkspace(opts.TabID, opts.ChatID, opts.ProviderID, opts.CWD)
	if !ok {
		f.manager.CloseSession(context.Background(), info.SessionID)
		return nil, providercontract.ThreadRef{}, &providercontract.Error{
			Kind: providercontract.ErrorProtocolViolation, Message: "provider lane creation did not produce a durable binding",
		}
	}
	canonical := bindingLaneIdentity(binding)
	if err := validateCanonicalCreatedLane(identity, canonical); err != nil {
		f.manager.CloseSession(context.Background(), info.SessionID)
		return nil, providercontract.ThreadRef{}, err
	}
	thread := bindingThreadRef(binding)
	if err := thread.Validate(identity.Realm.ProviderID); err != nil {
		f.manager.CloseSession(context.Background(), info.SessionID)
		return nil, providercontract.ThreadRef{}, &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Message: "created provider thread is invalid", Cause: err}
	}
	lane := newManagerLane(f.manager, canonical, request.Owner, info, thread)
	return lane, thread, nil
}

func validateCanonicalCreatedLane(proposed, canonical providercontract.LaneIdentity) error {
	proposed, canonical = proposed.Normalize(), canonical.Normalize()
	if err := proposed.Validate(); err != nil {
		return err
	}
	if err := canonical.Validate(); err != nil {
		return err
	}
	if proposed.ChatID != canonical.ChatID || proposed.Realm.ProviderID != canonical.Realm.ProviderID ||
		proposed.Realm.MachineID != canonical.Realm.MachineID || proposed.WorkspaceEpoch != canonical.WorkspaceEpoch {
		return &providercontract.Error{
			Kind:    providercontract.ErrorNativeIdentityConflict,
			Message: "provider creation changed chat, provider, machine, or workspace identity",
		}
	}
	return nil
}

func (f managerLaneFactory) Resume(ctx context.Context, request providercontract.ResumeLaneRequest) (providercontract.Lane, error) {
	identity, opts, err := f.validateResume(request)
	if err != nil {
		return nil, err
	}
	binding, ok := f.manager.nativeSessions.getForLane(identity)
	if !ok {
		return nil, &providercontract.Error{Kind: providercontract.ErrorNativeThreadMissing, Message: "the exact provider lane binding is missing"}
	}
	if binding.Quarantined {
		return nil, &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: firstNonEmpty(binding.QuarantineReason, "provider lane is quarantined")}
	}
	if bindingLaneIdentity(binding) != identity {
		return nil, &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "resume request does not match the durable provider lane"}
	}
	if current := bindingThreadRef(binding); !current.Equal(request.Thread) {
		// The actor commit is authoritative. A crash can occur after it accepted
		// one verified lineage edge but before the adapter lookup was materialized;
		// repair exactly that one edge and no broader mismatch.
		if request.Thread.Lineage != current.Lineage+1 || !current.CanAdvanceTo(request.Thread) {
			return nil, &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "resume request does not match the durable provider lane"}
		}
		if err := f.manager.nativeSessions.materializeActorLineage(identity, current, request.Thread); err != nil {
			return nil, classifyLaneRuntimeError("repair actor lineage materialization", err)
		}
		binding, ok = f.manager.nativeSessions.getForLane(identity)
		if !ok || !bindingThreadRef(binding).Equal(request.Thread) {
			return nil, &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "actor lineage materialization failed readback"}
		}
	}
	info, err := f.manager.NewSession(ctx, opts)
	if err != nil {
		return nil, classifyLaneRuntimeError("resume lane", err)
	}
	if info.SessionID != request.Thread.HeadID {
		f.manager.CloseSession(context.Background(), info.SessionID)
		return nil, &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "exact resume returned a replacement native thread"}
	}
	return newManagerLane(f.manager, identity, request.Owner, info, request.Thread.Normalize()), nil
}

func (f managerLaneFactory) validateCreate(request providercontract.CreateLaneRequest) (providercontract.LaneIdentity, SessionOptions, error) {
	identity := request.Identity.Normalize()
	if err := identity.Validate(); err != nil {
		return providercontract.LaneIdentity{}, SessionOptions{}, err
	}
	if normalizeProviderID(string(identity.Realm.ProviderID)) != f.providerID {
		return providercontract.LaneIdentity{}, SessionOptions{}, errors.New("lane factory received another provider's identity")
	}
	if strings.TrimSpace(request.Owner.TabID) == "" {
		return providercontract.LaneIdentity{}, SessionOptions{}, errors.New("lane attachment requires a tab id")
	}
	if f.manager == nil || f.manager.nativeSessions == nil || f.manager.nativeSessions.path == "" {
		return providercontract.LaneIdentity{}, SessionOptions{}, errors.New("durable provider lane storage is unavailable")
	}
	opts := SessionOptions{
		TabID: request.Owner.TabID, ChatID: identity.ChatID, CWD: request.CWD,
		ProviderID: f.providerID, ModelID: request.ModelID, ModeID: request.ModeID,
		AgentOwnerKey: request.Owner.AgentOwnerKey, ProviderLaneManaged: true, ProviderLaneCreate: true,
	}
	return identity, opts, nil
}

func (f managerLaneFactory) validateResume(request providercontract.ResumeLaneRequest) (providercontract.LaneIdentity, SessionOptions, error) {
	identity, opts, err := f.validateCreate(providercontract.CreateLaneRequest{
		Identity: request.Identity, Owner: request.Owner, CWD: request.CWD, ModelID: request.ModelID, ModeID: request.ModeID,
	})
	if err != nil {
		return providercontract.LaneIdentity{}, SessionOptions{}, err
	}
	opts.ProviderLaneCreate = false
	thread := request.Thread.Normalize()
	if err := thread.Validate(identity.Realm.ProviderID); err != nil {
		return providercontract.LaneIdentity{}, SessionOptions{}, err
	}
	return identity, opts, nil
}

func bindingThreadRef(binding nativeSessionBinding) providercontract.ThreadRef {
	return providercontract.ThreadRef{
		ProviderID: providercontract.ID(binding.ProviderID), RootID: binding.SessionID,
		HeadID: bindingCurrentThreadID(binding), Lineage: binding.ThreadLineage, Proof: binding.LineageProof,
	}.Normalize()
}

func classifyLaneRuntimeError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var providerErr *providercontract.Error
	if errors.As(err, &providerErr) {
		return err
	}
	return &providercontract.Error{Kind: providercontract.ErrorTransientTransport, Message: operation + " failed", Cause: err}
}

type managerLane struct {
	manager  *Manager
	identity providercontract.LaneIdentity
	owner    providercontract.AttachmentOwner
	info     SessionInfo
	thread   providercontract.ThreadRef

	emitMu     sync.Mutex
	mu         sync.Mutex
	sequence   uint64
	events     chan providercontract.Event
	detached   bool
	detachDone chan struct{}
	closed     bool
	jobs       map[string]providercontract.OperationID

	// durableCommits is armed before the lane is registered so provider
	// callbacks emitted in the Create/Resume -> LaneOpened window cannot reach
	// Manager.emit without waiting for the actor's acknowledgement. Lifecycle
	// cleanup commits are enabled when the coordinator reaches its normal
	// post-LaneOpened start boundary.
	durableCommits          bool
	durableLifecycleCommits bool
	commitWait              map[uint64]chan error
	protocolFailed          bool
}

func newManagerLane(manager *Manager, identity providercontract.LaneIdentity, owner providercontract.AttachmentOwner, info SessionInfo, thread providercontract.ThreadRef) *managerLane {
	lane := &managerLane{
		manager: manager, identity: identity.Normalize(), owner: owner, info: info,
		thread: thread.Normalize(), events: make(chan providercontract.Event, 128),
		detachDone: make(chan struct{}), jobs: make(map[string]providercontract.OperationID),
		commitWait: make(map[uint64]chan error), durableCommits: true,
	}
	// Registration exposes the lane to provider callbacks. Hold emitMu across
	// registration and the initial queueing so a callback cannot overtake the
	// LaneAttached sequence, while the initial lifecycle event itself does not
	// wait for the coordinator that is only started after LaneOpened.
	lane.emitMu.Lock()
	manager.registerProviderLane(lane)
	_ = lane.emitLocked(providercontract.Event{Kind: providercontract.EventLaneAttached, Thread: &lane.thread}, false)
	lane.emitMu.Unlock()
	return lane
}

func (l *managerLane) Identity() providercontract.LaneIdentity { return l.identity }
func (l *managerLane) Thread() providercontract.ThreadRef      { return l.thread }
func (l *managerLane) Delivery() providercontract.DeliveryStrategy {
	return managerLaneDelivery{lane: l}
}
func (l *managerLane) Context() providercontract.ContextStrategy {
	return managerLaneContext{lane: l, capabilities: providerAdapterForID(string(l.identity.Realm.ProviderID)).context.Capabilities()}
}
func (l *managerLane) Events() <-chan providercontract.Event { return l.events }

func (l *managerLane) AttachmentSnapshot() providercontract.LaneAttachmentSnapshot {
	if l == nil {
		return providercontract.LaneAttachmentSnapshot{}
	}
	l.mu.Lock()
	info := l.info
	l.mu.Unlock()
	snapshot := providercontract.LaneAttachmentSnapshot{
		ConnectionID:            info.SessionID,
		CWD:                     info.CWD,
		Agent:                   info.Agent,
		ProviderID:              providercontract.ID(info.ProviderID),
		ProviderName:            info.ProviderName,
		CurrentModelID:          stringValue(info.CurrentModelID),
		CurrentModeID:           stringValue(info.CurrentModeID),
		ImageSupport:            info.ImageSupport,
		CommandCatalogSupported: info.CommandCatalogSupported,
		Models:                  make([]providercontract.RuntimeModel, len(info.Models)),
		Modes:                   make([]providercontract.RuntimeMode, len(info.Modes)),
	}
	for i, model := range info.Models {
		snapshot.Models[i] = providercontract.RuntimeModel{
			ID: model.ModelID, Name: model.Name, Efforts: append([]string(nil), model.Efforts...),
		}
	}
	for i, mode := range info.Modes {
		snapshot.Modes[i] = providercontract.RuntimeMode{ID: mode.ID, Name: mode.Name}
	}
	if info.CommandCatalog != nil {
		catalog := &providercontract.RuntimeCommandCatalog{
			OutputStyle:           info.CommandCatalog.OutputStyle,
			AvailableOutputStyles: append([]string(nil), info.CommandCatalog.AvailableOutputStyles...),
			CommandsTruncated:     info.CommandCatalog.CommandsTruncated,
			AgentsTruncated:       info.CommandCatalog.AgentsTruncated,
			StylesTruncated:       info.CommandCatalog.StylesTruncated,
			AsOf:                  info.CommandCatalog.AsOf,
			Commands:              make([]providercontract.RuntimeCommand, len(info.CommandCatalog.Commands)),
			Agents:                make([]providercontract.RuntimeAgent, len(info.CommandCatalog.Agents)),
		}
		for i, command := range info.CommandCatalog.Commands {
			catalog.Commands[i] = providercontract.RuntimeCommand{
				Name: command.Name, Description: command.Description, ArgumentHint: command.ArgumentHint,
				Aliases: append([]string(nil), command.Aliases...),
			}
		}
		for i, agent := range info.CommandCatalog.Agents {
			catalog.Agents[i] = providercontract.RuntimeAgent{
				Name: agent.Name, Description: agent.Description, Model: agent.Model,
			}
		}
		snapshot.CommandCatalog = catalog
	}
	return snapshot
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (l *managerLane) RequireDurableEventCommits() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.durableCommits = true
	l.durableLifecycleCommits = true
	l.mu.Unlock()
}

func (l *managerLane) AcknowledgeDurableEvent(sequence uint64, commitErr error) {
	if l == nil || sequence == 0 {
		return
	}
	l.mu.Lock()
	wait := l.commitWait[sequence]
	if commitErr != nil {
		l.protocolFailed = true
	}
	l.mu.Unlock()
	if wait != nil {
		wait <- commitErr
	}
}

func (l *managerLane) Detach(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	alreadyDetached := l.detached
	detachDone := l.detachDone
	l.mu.Unlock()
	if alreadyDetached {
		// Detach is a lifecycle cleanup operation and must be idempotent. More
		// importantly, an old attachment may share its immutable native session
		// id with a later exact resume; asking the manager to close that id again
		// could terminate the newer host.
		select {
		case <-detachDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	closedAttachment := l.manager.CloseSession(ctx, l.info.SessionID)
	l.attachmentClosed()
	if !closedAttachment {
		return &providercontract.Error{Kind: providercontract.ErrorTransientTransport, Message: "provider lane attachment was already unavailable"}
	}
	return nil
}

// attachmentClosed is the one adapter-detach boundary, regardless of whether
// shutdown originated in the chat actor, workspace movement, lifecycle reap,
// or manager cleanup. Closing a bridge behind the actor without emitting this
// event leaves durable state falsely Ready and makes later exact resume fail.
func (l *managerLane) attachmentClosed() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.detached {
		detachDone := l.detachDone
		l.mu.Unlock()
		<-detachDone
		return
	}
	l.detached = true
	detachDone := l.detachDone
	l.mu.Unlock()
	defer close(detachDone)
	if err := l.emit(providercontract.Event{Kind: providercontract.EventLaneDetached}); err != nil && l.manager != nil {
		l.manager.opts.Logf("provider lane detach was rejected by chat actor", map[string]any{
			"chatId": l.identity.ChatID, "laneId": l.identity.ID, "error": redactSensitiveText(err.Error()),
		})
	}
	l.emitMu.Lock()
	l.mu.Lock()
	if !l.closed {
		close(l.events)
		l.closed = true
	}
	l.mu.Unlock()
	l.emitMu.Unlock()
	l.manager.unregisterProviderLane(l)
}

// rejectFrozenProtocol commits the dropped semantic event as an actor-owned
// protocol failure before asynchronously destroying the bridge. Continuing
// after normalization failed would make live state differ from durable state.
func (l *managerLane) rejectFrozenProtocol(_ error) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.closed || l.detached || l.protocolFailed {
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	if err := l.emit(providercontract.Event{
		Kind: providercontract.EventTransportHealth,
		Health: &providercontract.TransportHealthEvent{
			State: "protocol_failed", Error: providercontract.ErrorProtocolViolation,
		},
	}); err != nil {
		l.manager.opts.Logf("provider protocol failure could not reach chat actor", map[string]any{
			"chatId": l.identity.ChatID, "error": redactSensitiveText(err.Error()),
		})
	}
	l.mu.Lock()
	l.protocolFailed = true
	l.mu.Unlock()
	go func() {
		_ = l.manager.CloseSession(context.Background(), l.info.SessionID)
		l.attachmentClosed()
	}()
}

func (l *managerLane) advanceLineage(previousHead, nextHead string, generation uint64, proof string) error {
	if l == nil {
		return errors.New("provider lineage event has no lane")
	}
	previousHead, nextHead, proof = strings.TrimSpace(previousHead), strings.TrimSpace(nextHead), strings.TrimSpace(proof)
	l.mu.Lock()
	from := l.thread.Normalize()
	l.mu.Unlock()
	if previousHead == "" || previousHead != from.HeadID || nextHead == "" || generation != from.Lineage+1 || proof == "" {
		return &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "provider lineage announcement does not advance the exact actor-owned head"}
	}
	to := from
	to.HeadID, to.Lineage, to.Proof = nextHead, generation, proof
	if !from.CanAdvanceTo(to) {
		return &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "provider lineage announcement is not a verified monotonic advance"}
	}
	// Durable actor commit comes first. The native-session ledger below is only
	// a materialized adapter lookup and can be reconstructed from this receipt.
	if err := l.emit(providercontract.Event{Kind: providercontract.EventLineageAdvanced, Thread: &to}); err != nil {
		return err
	}
	l.mu.Lock()
	if !l.thread.Equal(from) {
		l.mu.Unlock()
		return &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "provider lineage changed concurrently"}
	}
	l.thread = to
	l.mu.Unlock()
	if l.manager == nil || l.manager.nativeSessions == nil {
		return errors.New("provider lineage materialization is unavailable")
	}
	return l.manager.nativeSessions.materializeActorLineage(l.identity, from, to)
}

func (m *Manager) providerLaneForSessionID(sessionID string) *managerLane {
	if m == nil {
		return nil
	}
	m.providerLaneMu.Lock()
	lane := m.providerLanesBySession[strings.TrimSpace(sessionID)]
	m.providerLaneMu.Unlock()
	return lane
}

func (l *managerLane) emit(event providercontract.Event) error {
	l.emitMu.Lock()
	defer l.emitMu.Unlock()
	return l.emitLocked(event, true)
}

// emitLocked normalizes and queues one provider event while emitMu is held.
// waitForCommit is false only for the constructor's LaneAttached event: that
// event must be queued before the coordinator can start its forwarder, while
// every provider callback remains durably backpressured from registration.
func (l *managerLane) emitLocked(event providercontract.Event, waitForCommit bool) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return errors.New("provider event lane is closed")
	}
	if l.detached && event.Kind != providercontract.EventLaneDetached {
		l.mu.Unlock()
		return errors.New("provider event lane is detached")
	}
	if l.protocolFailed && event.Kind == providercontract.EventLaneDetached {
		l.mu.Unlock()
		return nil
	}
	if event.Kind != providercontract.EventTurnTerminal && event.Identity.TurnID != "" &&
		l.manager != nil && l.manager.providerLaneClosedJob(event.Identity.TurnID) {
		l.mu.Unlock()
		return fmt.Errorf("provider event arrived after terminal job cleanup: %s", event.Identity.TurnID)
	}
	l.sequence++
	event.Identity = providercontract.EventIdentity{
		ChatID: l.identity.ChatID, LaneID: l.identity.ID, OperationID: event.Identity.OperationID,
		TurnID: event.Identity.TurnID, Sequence: l.sequence, ObservedAtUnixMS: event.Identity.ObservedAtUnixMS,
	}
	if event.Identity.ObservedAtUnixMS <= 0 {
		event.Identity.ObservedAtUnixMS = time.Now().UnixMilli()
	}
	if err := event.Validate(); err != nil {
		l.mu.Unlock()
		l.manager.opts.Logf("provider adapter rejected malformed normalized event", map[string]any{
			"providerId": l.info.ProviderID, "kind": string(event.Kind), "error": err.Error(),
		})
		return err
	}
	var wait chan error
	shouldWait := waitForCommit && l.durableCommits
	if event.Kind == providercontract.EventLaneDetached && !l.durableLifecycleCommits {
		// Direct manager-lane users can tear down a lane before a coordinator has
		// reached LaneOpened. Production coordinators arm lifecycle commits at that
		// boundary; provider semantic callbacks are still durable from registration.
		shouldWait = false
	}
	if shouldWait {
		wait = make(chan error, 1)
		l.commitWait[event.Identity.Sequence] = wait
	}
	l.mu.Unlock()
	// The actor is the durable owner of normalized provider events. Backpressure
	// the adapter when its bounded handoff is full; dropping here would let the
	// frozen renderer observe output that the authoritative actor can never
	// reconstruct after a restart.
	l.events <- event
	if wait == nil {
		return nil
	}
	err := <-wait
	l.mu.Lock()
	delete(l.commitWait, event.Identity.Sequence)
	l.mu.Unlock()
	return err
}

type managerLaneDelivery struct{ lane *managerLane }

func (d managerLaneDelivery) Capabilities() providercontract.DeliveryCapabilities {
	bridge := d.lane.manager.bridgeForSession(d.lane.info.SessionID, SessionOptions{
		TabID: d.lane.owner.TabID, ChatID: d.lane.identity.ChatID, SessionID: d.lane.info.SessionID,
		ProviderID: d.lane.info.ProviderID,
	})
	if bridge == nil {
		return providercontract.DeliveryCapabilities{}
	}
	return providercontract.DeliveryCapabilities{
		StableInputIdentity: bridge.hasProviderCapability("workassStableTurnInputV1"),
		LiveSteer:           bridge.hasProviderCapability("steerNotification", "sessionSteer", "workassCodexSteerRequest", "workassClaudeSteerRequest"),
		ConsumptionReceipt:  bridge.hasProviderCapability("workassStableTurnInputV1"),
		TurnReadback:        bridge.hasProviderCapability("workassOperationReadbackV1", "workassTurnReconcileRequest"),
	}
}

func (d managerLaneDelivery) StartTurn(ctx context.Context, input providercontract.TurnInput) (providercontract.TurnAdmission, error) {
	operationID := providercontract.NormalizeOperationID(string(input.OperationID))
	if operationID == "" {
		return providercontract.TurnAdmission{}, errors.New("turn admission requires an operation id")
	}
	d.lane.mu.Lock()
	info := d.lane.info
	owner := d.lane.owner
	identity := d.lane.identity
	d.lane.mu.Unlock()
	if modelID := strings.TrimSpace(input.ModelID); modelID != "" && modelID != stringPointer(info.CurrentModelID) {
		result, err := d.lane.manager.SetModel(ctx, info.SessionID, modelID)
		if err != nil {
			return providercontract.TurnAdmission{}, classifyLaneRuntimeError("apply lane model", err)
		}
		applied := firstNonEmpty(strings.TrimSpace(asString(result["currentModelId"])), modelID)
		d.lane.mu.Lock()
		d.lane.info.CurrentModelID = &applied
		d.lane.mu.Unlock()
		info.CurrentModelID = &applied
	}
	if modeID := strings.TrimSpace(input.ModeID); modeID != "" && modeID != stringPointer(info.CurrentModeID) {
		result, err := d.lane.manager.SetMode(ctx, info.SessionID, modeID)
		if err != nil {
			return providercontract.TurnAdmission{}, classifyLaneRuntimeError("apply lane mode", err)
		}
		applied := firstNonEmpty(strings.TrimSpace(asString(result["currentModeId"])), modeID)
		d.lane.mu.Lock()
		d.lane.info.CurrentModeID = &applied
		d.lane.mu.Unlock()
		info.CurrentModeID = &applied
	}
	images := make([]any, 0, len(input.Attachments))
	for _, attachment := range input.Attachments {
		if strings.TrimSpace(attachment.Ref) == "" {
			return providercontract.TurnAdmission{}, errors.New("turn attachment is missing its immutable content reference")
		}
		payload, err := d.lane.manager.resolveProviderAttachment(ctx, attachment)
		if err != nil {
			return providercontract.TurnAdmission{}, classifyLaneRuntimeError("resolve turn attachment", err)
		}
		images = append(images, payload)
	}
	presentation := input.Presentation
	humanAuthored := strings.TrimSpace(presentation.Origin) == "" || strings.EqualFold(strings.TrimSpace(presentation.Origin), "human")
	var admitted providercontract.TurnAdmission
	job, err := d.lane.manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", Title: strings.TrimSpace(presentation.Title), PermissionMode: strings.TrimSpace(input.Permission),
		ChatID: identity.ChatID, TabID: owner.TabID,
		SessionID: info.SessionID, CWD: info.CWD, ProviderID: info.ProviderID,
		Prompt: input.Text, Images: images, ModelID: strings.TrimSpace(input.ModelID), ModeID: strings.TrimSpace(input.ModeID),
		HumanAuthored:       humanAuthored,
		ProviderLaneManaged: true,
		OperationID:         string(operationID),
		UserMessageID:       firstNonEmpty(strings.TrimSpace(presentation.UserMessageID), string(operationID)),
		AssistantMessageID:  strings.TrimSpace(presentation.AssistantMessageID), QueueID: strings.TrimSpace(presentation.QueueID),
		PromptText: firstNonEmpty(strings.TrimSpace(presentation.PromptText), input.Text),
		CommitAdmission: func(job map[string]any) error {
			turn := providercontract.TurnRef{OperationID: operationID, NativeID: strings.TrimSpace(asString(job["id"]))}
			admitted = providercontract.TurnAdmission{Turn: turn, Accepted: turn.NativeID != ""}
			if !admitted.Accepted {
				return &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Operation: operationID, Message: "provider turn admission omitted the job identity"}
			}
			if input.CommitAdmission != nil {
				if err := input.CommitAdmission(admitted); err != nil {
					return err
				}
			}
			d.lane.manager.recordProviderLaneAdmission(owner.TabID, identity.ChatID, operationID, job)
			return nil
		},
	})
	if err != nil {
		return providercontract.TurnAdmission{}, classifyLaneRuntimeError("start turn", err)
	}
	turn := admitted.Turn
	if turn.NativeID == "" {
		turn = providercontract.TurnRef{OperationID: operationID, NativeID: strings.TrimSpace(asString(job["id"]))}
	}
	if turn.NativeID == "" {
		return providercontract.TurnAdmission{}, &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Operation: operationID, Message: "provider turn admission omitted the job identity"}
	}
	admission := providercontract.TurnAdmission{Turn: turn, Accepted: true}
	return admission, nil
}

func (d managerLaneDelivery) Steer(ctx context.Context, input providercontract.SteerInput) (providercontract.SteerReceipt, error) {
	operationID := providercontract.NormalizeOperationID(string(input.OperationID))
	if operationID == "" || strings.TrimSpace(input.Turn.NativeID) == "" {
		return providercontract.SteerReceipt{}, errors.New("steer requires operation and turn identity")
	}
	images := make([]any, 0, len(input.Attachments))
	for _, attachment := range input.Attachments {
		if strings.TrimSpace(attachment.Ref) == "" {
			return providercontract.SteerReceipt{}, errors.New("steer attachment is missing its immutable content reference")
		}
		payload, err := d.lane.manager.resolveProviderAttachment(ctx, attachment)
		if err != nil {
			return providercontract.SteerReceipt{}, classifyLaneRuntimeError("resolve steer attachment", err)
		}
		images = append(images, payload)
	}
	d.lane.mu.Lock()
	sessionID := d.lane.info.SessionID
	d.lane.mu.Unlock()
	outcome := d.lane.manager.Steer(sessionID, input.Text, images, string(operationID))
	if ok, _ := outcome["ok"].(bool); !ok {
		if strategy := strings.TrimSpace(asString(outcome["strategy"])); strategy == "uncertain" {
			return providercontract.SteerReceipt{Turn: input.Turn, Ambiguous: true}, &providercontract.Error{
				Kind: providercontract.ErrorAcceptanceAmbiguous, Operation: operationID, Message: firstNonEmpty(asString(outcome["error"]), "steer acceptance is uncertain"),
			}
		}
		if unsupported, _ := outcome["unsupported"].(bool); unsupported {
			return providercontract.SteerReceipt{Turn: input.Turn}, providercontract.Unsupported(operationID, firstNonEmpty(asString(outcome["error"]), "live steering is unsupported"))
		}
		return providercontract.SteerReceipt{Turn: input.Turn}, &providercontract.Error{Kind: providercontract.ErrorAdmissionRejected, Operation: operationID, Message: firstNonEmpty(asString(outcome["error"]), "steer was rejected")}
	}
	awaitConsumption := boolValue(outcome["receipt"])
	return providercontract.SteerReceipt{
		Turn: input.Turn, Accepted: true, Consumed: !awaitConsumption,
		AwaitConsumption: awaitConsumption, Interrupted: boolValue(outcome["interrupted"]),
	}, nil
}

func (d managerLaneDelivery) Cancel(_ context.Context, turn providercontract.TurnRef) error {
	if strings.TrimSpace(turn.NativeID) == "" {
		return errors.New("cancel requires a native turn id")
	}
	result := d.lane.manager.CancelJobResult(turn.NativeID)
	if result.Cancelled || result.Reason == "idle" {
		return nil
	}
	return &providercontract.Error{Kind: providercontract.ErrorAdmissionRejected, Operation: turn.OperationID, Message: "provider turn is not owned by this lane"}
}

func (d managerLaneDelivery) Reconcile(ctx context.Context, request providercontract.ReconcileRequest) (providercontract.ReconcileResult, error) {
	operationID := providercontract.NormalizeOperationID(string(request.OperationID))
	if operationID == "" {
		return providercontract.ReconcileResult{}, errors.New("turn reconciliation requires an operation id")
	}
	bridge := d.lane.manager.bridgeForSession(d.lane.info.SessionID, SessionOptions{
		TabID: d.lane.owner.TabID, ChatID: d.lane.identity.ChatID, SessionID: d.lane.info.SessionID,
		ProviderID: d.lane.info.ProviderID,
	})
	if bridge == nil {
		return providercontract.ReconcileResult{}, &providercontract.Error{Kind: providercontract.ErrorTransientTransport, Operation: operationID, Message: "provider attachment is unavailable"}
	}
	readback, err := bridge.readbackOperation(ctx, d.lane.thread.HeadID, string(operationID))
	if err != nil {
		return providercontract.ReconcileResult{}, classifyLaneRuntimeError("reconcile turn", err)
	}
	// TurnRef.NativeID is the immutable Workass/provider-lane delivery identity
	// used by cancellation and the frozen job event contract. The provider's
	// operation readback may also expose its own vendor-native turn id; finding
	// that id proves the operation mapping, but it must not replace the admitted
	// lane turn owner after a host restart.
	return providercontract.ReconcileResult{
		Turn:  providercontract.TurnRef{OperationID: operationID, NativeID: request.Turn.NativeID},
		Found: readback.Found, State: readback.Status, Terminal: readback.Terminal, Consumed: readback.Consumed,
	}, nil
}

func (d managerLaneDelivery) ResolvePermission(_ context.Context, decision providercontract.PermissionDecision) (providercontract.PermissionReceipt, error) {
	decision.OperationID = providercontract.NormalizeOperationID(string(decision.OperationID))
	decision.RequestID = strings.TrimSpace(decision.RequestID)
	decision.OptionID = strings.TrimSpace(decision.OptionID)
	if decision.OperationID == "" || decision.RequestID == "" || decision.OptionID == "" {
		return providercontract.PermissionReceipt{}, errors.New("permission decision requires operation, request, and option identity")
	}
	if !d.lane.manager.PermissionDecide(decision.RequestID, decision.OptionID) {
		return providercontract.PermissionReceipt{
				OperationID: decision.OperationID, RequestID: decision.RequestID, OptionID: decision.OptionID,
			}, &providercontract.Error{
				Kind: providercontract.ErrorAdmissionRejected, Operation: decision.OperationID,
				Message: "permission request is no longer pending on this provider lane",
			}
	}
	return providercontract.PermissionReceipt{
		OperationID: decision.OperationID, RequestID: decision.RequestID, OptionID: decision.OptionID, Accepted: true,
	}, nil
}

type managerLaneContext struct {
	lane         *managerLane
	capabilities providercontract.ContextCapabilities
}

func (c managerLaneContext) Capabilities() providercontract.ContextCapabilities {
	capabilities := c.capabilities
	if c.lane == nil || c.lane.manager == nil {
		capabilities.ExactResume = false
		return capabilities
	}
	c.lane.mu.Lock()
	info, owner := c.lane.info, c.lane.owner
	c.lane.mu.Unlock()
	bridge := c.lane.manager.bridgeForSession(info.SessionID, SessionOptions{
		TabID: owner.TabID, ChatID: c.lane.identity.ChatID, SessionID: info.SessionID, ProviderID: info.ProviderID,
	})
	capabilities.ExactResume = capabilities.ExactResume && bridge != nil && bridge.supportsSessionResume()
	if negotiated, ok := bridge.contextImportCapabilities(); ok {
		capabilities.ImportMode = providercontract.ContextImportNonSampling
		capabilities.ImportReadback = true
		capabilities.IdempotentImport = true
		capabilities.MaxImportEvents = negotiated.MaxImportEvents
		capabilities.MaxImportBytes = negotiated.MaxImportBytes
	}
	return capabilities
}

type contextImportProtocol struct {
	MaxImportEvents int
	MaxImportBytes  int
}

func (b *Bridge) contextImportCapabilities() (contextImportProtocol, bool) {
	if b == nil {
		return contextImportProtocol{}, false
	}
	b.mu.Lock()
	spec := mapFromAny(b.agentMeta["workassContextImportV1"])
	if len(spec) == 0 {
		spec = mapFromAny(b.agentCaps["workassContextImportV1"])
	}
	b.mu.Unlock()
	protocol := contextImportProtocol{
		MaxImportEvents: numberOrZero(spec["maxEvents"]),
		MaxImportBytes:  numberOrZero(spec["maxBytes"]),
	}
	if strings.TrimSpace(asString(spec["mode"])) != "non_sampling" ||
		strings.TrimSpace(asString(spec["receipt"])) != "operation_readback_v1" ||
		!boolValue(spec["idempotent"]) || protocol.MaxImportEvents <= 0 || protocol.MaxImportEvents > 1024 ||
		protocol.MaxImportBytes <= 0 || protocol.MaxImportBytes > 16<<20 {
		return contextImportProtocol{}, false
	}
	return protocol, true
}

func (c managerLaneContext) bridge() (*Bridge, SessionInfo, error) {
	if c.lane == nil || c.lane.manager == nil {
		return nil, SessionInfo{}, errors.New("provider lane context is unavailable")
	}
	c.lane.mu.Lock()
	info, owner := c.lane.info, c.lane.owner
	c.lane.mu.Unlock()
	bridge := c.lane.manager.bridgeForSession(info.SessionID, SessionOptions{
		TabID: owner.TabID, ChatID: c.lane.identity.ChatID, SessionID: info.SessionID, ProviderID: info.ProviderID,
	})
	if _, ok := bridge.contextImportCapabilities(); !ok {
		return nil, info, providercontract.Unsupported("", "provider does not expose verified non-sampling context import with operation readback")
	}
	return bridge, info, nil
}

func contextImportParams(sessionID string, request providercontract.ContextImportRequest, includeMessages bool) map[string]any {
	params := map[string]any{
		"sessionId": sessionID, "operationId": string(request.OperationID),
		"from": request.From, "to": request.To, "digest": request.Digest,
	}
	if includeMessages {
		params["messages"] = append([]providercontract.ContextMessage(nil), request.Messages...)
	}
	return params
}

func parseContextImportReceipt(request providercontract.ContextImportRequest, result map[string]any) (providercontract.ContextImportReceipt, error) {
	receipt := providercontract.ContextImportReceipt{
		OperationID: providercontract.NormalizeOperationID(asString(result["operationId"])),
		From:        uint64(numberOrZero(result["from"])), To: uint64(numberOrZero(result["to"])),
		Digest: strings.TrimSpace(asString(result["digest"])),
		Found:  boolValue(result["found"]), Confirmed: boolValue(result["confirmed"]), Ambiguous: boolValue(result["ambiguous"]),
	}
	if receipt.OperationID != request.OperationID || receipt.From != request.From || receipt.To != request.To || receipt.Digest != request.Digest {
		return providercontract.ContextImportReceipt{}, &providercontract.Error{
			Kind: providercontract.ErrorProtocolViolation, Operation: request.OperationID,
			Message: "provider context-import receipt changed immutable operation identity",
		}
	}
	if receipt.Confirmed && (!receipt.Found || receipt.Ambiguous) || receipt.Ambiguous && receipt.Found {
		return providercontract.ContextImportReceipt{}, &providercontract.Error{
			Kind: providercontract.ErrorProtocolViolation, Operation: request.OperationID,
			Message: "provider context-import receipt is contradictory",
		}
	}
	return receipt, nil
}

func (c managerLaneContext) Import(ctx context.Context, request providercontract.ContextImportRequest) (providercontract.ContextImportReceipt, error) {
	bridge, info, err := c.bridge()
	if err != nil {
		return providercontract.ContextImportReceipt{}, err
	}
	result, err := bridge.request(ctx, "_workass/context/import", contextImportParams(info.SessionID, request, true), bridge.opts.InitTimeout)
	if err != nil {
		var rpcErr *acpError
		if errors.As(err, &rpcErr) {
			return providercontract.ContextImportReceipt{}, &providercontract.Error{Kind: providercontract.ErrorAdmissionRejected, Operation: request.OperationID, Message: "provider rejected context import", Cause: err}
		}
		return providercontract.ContextImportReceipt{}, &providercontract.Error{Kind: providercontract.ErrorAcceptanceAmbiguous, Operation: request.OperationID, Message: "context-import acceptance is uncertain", Cause: err}
	}
	receipt, err := parseContextImportReceipt(request, result)
	if err != nil {
		return providercontract.ContextImportReceipt{}, err
	}
	if !receipt.Found || !receipt.Confirmed || receipt.Ambiguous {
		return receipt, &providercontract.Error{Kind: providercontract.ErrorAdmissionRejected, Operation: request.OperationID, Message: "provider did not confirm context import"}
	}
	return receipt, nil
}

func (c managerLaneContext) ReconcileImport(ctx context.Context, request providercontract.ContextImportRequest) (providercontract.ContextImportReceipt, error) {
	bridge, info, err := c.bridge()
	if err != nil {
		return providercontract.ContextImportReceipt{}, err
	}
	result, err := bridge.request(ctx, "_workass/context/import/readback", contextImportParams(info.SessionID, request, false), bridge.opts.PromptReconcileTimeout)
	if err != nil {
		return providercontract.ContextImportReceipt{}, &providercontract.Error{Kind: providercontract.ErrorTransientTransport, Operation: request.OperationID, Message: "context-import readback failed", Cause: err}
	}
	return parseContextImportReceipt(request, result)
}

func (c managerLaneContext) Checkpoint(_ context.Context) (providercontract.ContextCheckpoint, error) {
	return providercontract.ContextCheckpoint{}, providercontract.Unsupported("", "provider does not expose an authoritative context checkpoint readback")
}

type managerMetadataSource struct {
	manager    *Manager
	providerID string
}

type managerUpdateStrategy struct {
	manager    *Manager
	providerID string
}

func (s managerUpdateStrategy) CheckUpdate(ctx context.Context) (providercontract.UpdateInfo, error) {
	if s.manager == nil {
		return providercontract.UpdateInfo{}, errors.New("provider update strategy has no manager")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if version := s.manager.detectInstalledCLIVersion(ctx, s.providerID); version != nil {
		s.manager.setProviderCLIVersion(s.providerID, version)
	}
	candidate, ok := s.manager.providerUpdateCandidate(s.providerID)
	if !ok {
		return providercontract.UpdateInfo{}, nil
	}
	latest, err := s.manager.latestCLIVersion(ctx, candidate.spec)
	if err != nil {
		return providercontract.UpdateInfo{}, err
	}
	comparison, comparable := compareLenientSemver(candidate.installed, latest)
	if !comparable {
		return providercontract.UpdateInfo{}, fmt.Errorf("provider versions are not comparable: installed=%q latest=%q", candidate.installed, latest)
	}
	return providercontract.UpdateInfo{
		Available: comparison > 0, Version: latest, Source: candidate.spec.Source,
	}, nil
}

func (s managerMetadataSource) ReadMetadata(_ context.Context) (providercontract.MetadataSnapshot, error) {
	if s.manager == nil {
		return providercontract.MetadataSnapshot{}, errors.New("provider metadata source has no manager")
	}
	s.manager.mu.Lock()
	runtime := s.manager.providers[s.providerID]
	if runtime == nil {
		s.manager.mu.Unlock()
		return providercontract.MetadataSnapshot{}, errors.New("provider runtime is unavailable")
	}
	models := append([]Model(nil), runtime.Models...)
	modes := append([]Mode(nil), runtime.Modes...)
	s.manager.mu.Unlock()
	out := providercontract.MetadataSnapshot{Models: make([]providercontract.CatalogModel, 0, len(models)), Modes: make([]string, 0, len(modes))}
	for _, model := range models {
		out.Models = append(out.Models, providercontract.CatalogModel{ID: model.ModelID, Name: model.Name})
	}
	for _, mode := range modes {
		out.Modes = append(out.Modes, mode.ID)
	}
	return out, nil
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}
