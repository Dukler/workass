package chat

import (
	"encoding/json"
	"errors"
	"strings"

	"workass/internal/provider"
)

// PresentationState is the actor-owned, chat-scoped part of the frozen
// renderer mirror. Global application preferences (theme, density, workspace
// lists and pane sizes) deliberately do not live in a chat actor.
//
// Structured renderer values whose schema is already owned by the TypeScript
// boundary remain opaque JSON here. They still have one owner and one write
// path; chat/provider logic never interprets or branches on their contents.
type PresentationState struct {
	TabID                  string
	Title                  string
	TitleLocked            bool
	Group                  *string
	CWD                    *string
	Draft                  string
	Unread                 bool
	Settled                string
	SettledAt              int64
	Pane                   *string
	PresentationRevision   uint64
	ProviderID             provider.ID
	CurrentModelID         string
	CurrentModeID          string
	WorkspaceRevision      uint64
	AgentQueueRevision     uint64
	RuntimeControlRevision uint64
	ModelControls          json.RawMessage
	ContextUsageByProvider json.RawMessage
	PlanLatest             []provider.PlanEntry
	PlanLatestMessageID    string
}

func (p PresentationState) Clone() PresentationState {
	out := p
	if p.Group != nil {
		value := *p.Group
		out.Group = &value
	}
	if p.CWD != nil {
		value := *p.CWD
		out.CWD = &value
	}
	if p.Pane != nil {
		value := *p.Pane
		out.Pane = &value
	}
	out.ModelControls = append(json.RawMessage(nil), p.ModelControls...)
	out.ContextUsageByProvider = append(json.RawMessage(nil), p.ContextUsageByProvider...)
	out.PlanLatest = append([]provider.PlanEntry(nil), p.PlanLatest...)
	return out
}

func (p PresentationState) Validate() error {
	if p.Settled != "" && p.Settled != "settled" && p.Settled != "active" {
		return errors.New("chat presentation has invalid settled state")
	}
	if p.SettledAt < 0 || (p.SettledAt != 0 && p.Settled != "settled") {
		return errors.New("chat presentation has invalid settled timestamp")
	}
	if p.Pane != nil {
		switch strings.TrimSpace(*p.Pane) {
		case "", "rail", "browser":
		default:
			return errors.New("chat presentation has invalid pane")
		}
	}
	return nil
}

// InitializeChat is the only actor-native creation transition. It is legal
// only while the actor contains no chat data.
type InitializeChat struct {
	Presentation PresentationState
	OperationID  provider.OperationID
	Digest       string
}

func (InitializeChat) chatCommand() {}

// InitializeFork creates a new actor from an explicit visible prefix. The
// reducer rewrites operation/event identity into the child chat and records a
// context floor, so the new provider lane is contextless by construction.
type InitializeFork struct {
	Presentation PresentationState
	SourceChatID string
	Messages     []LedgerEvent
	OperationID  provider.OperationID
	Digest       string
}

func (InitializeFork) chatCommand() {}

// UpdatePresentation changes only actor-owned chat presentation. Transcript,
// queue-runtime, lanes and provider activity are intentionally not fields of
// this command, so a renderer save cannot overwrite them accidentally.
type UpdatePresentation struct {
	Presentation PresentationState
}

func (UpdatePresentation) chatCommand() {}

// SavePresentation is the renderer/controller CAS boundary. Internal actor
// transitions use their own typed commands; a UI may only write the explicitly
// whitelisted presentation fields under a stable request id.
type SavePresentation struct {
	OperationID  provider.OperationID
	Digest       string
	Presentation PresentationState
}

func (SavePresentation) chatCommand() {}

// ChangeWorkspace commits the next immutable workspace epoch before any live
// provider attachment is closed. Old lanes remain historical and resumable
// only under their original epoch; the next selection resolves a new LaneID.
type ChangeWorkspace struct {
	OperationID      provider.OperationID
	Digest           string
	CWD              string
	ExpectedRevision uint64
	// DetachTargets are the complete, ordered set of old disposable
	// attachments proved by the caller. The reducer validates every target
	// before mutating the workspace, then journals all resulting effects in the
	// same actor/store commit as the workspace receipt and epoch change.
	DetachTargets []DetachTarget
}

func (ChangeWorkspace) chatCommand() {}

// UpdateEnvironment commits one manager-observed Entorno snapshot to the
// actor. The payload and checkpoint list are provider/filesystem-specific
// wire data; the reducer stores them atomically with the exact tab fence.
// Manager callbacks must use this command before publishing chat:env.
type UpdateEnvironment struct {
	ExpectedTabID string
	CWD           string
	Payload       json.RawMessage
	Checkpoints   json.RawMessage
	Reference     json.RawMessage
}

func (UpdateEnvironment) chatCommand() {}

// ReplaceStagedQueue is the actor-native FIFO mutation boundary. The renderer
// submits one ordered snapshot under an immutable request id and the current
// daemon-issued revision; it cannot mutate provider operations already promoted
// into Queue/Foreground/Outbox.
type ReplaceStagedQueue struct {
	OperationID      provider.OperationID
	Digest           string
	ExpectedRevision uint64
	Entries          []StagedQueueEntry
}

func (ReplaceStagedQueue) chatCommand() {}

// UpdateRuntimeControls is the sole typed ingress for chat-scoped
// provider/model/mode state. A renderer picker may request this command only
// through the daemon's revision-fenced session-save translator;
// UpdatePresentation itself deliberately ignores these fields.
type UpdateRuntimeControls struct {
	ProviderID           provider.ID
	ModelID              string
	ModeID               string
	ReplaceModelID       bool
	ReplaceModeID        bool
	ModelControls        json.RawMessage
	ReplaceModelControls bool
	ExpectedRevision     uint64
	RequireRevision      bool
}

func (UpdateRuntimeControls) chatCommand() {}

// SaveRuntimeControls is the controller/agent CAS boundary for selecting a
// provider/model/mode. Adapter-originated corrections use the typed internal
// UpdateRuntimeControls command; user-authored requests require an immutable
// operation receipt so a lost reply cannot apply the change twice.
type SaveRuntimeControls struct {
	OperationID provider.OperationID
	Digest      string
	Update      UpdateRuntimeControls
}

func (SaveRuntimeControls) chatCommand() {}

// CommitLaneSelection is the atomic provider-lane selection transaction.  The
// resolver must produce the read-only selection before this command is
// applied; the reducer then commits the desired controls and lane under one
// actor revision and one idempotency receipt.
type CommitLaneSelection struct {
	OperationID provider.OperationID
	Digest      string
	Identity    provider.LaneIdentity
	Thread      provider.ThreadRef
	Owner       provider.AttachmentOwner
	CWD         string
	ModelID     string
	ModeID      string
	Context     provider.ContextCapabilities
	Creation    provider.CreationCapabilities
	Established bool
	Update      UpdateRuntimeControls
}

func (CommitLaneSelection) chatCommand() {}

// PromoteStagedQueue atomically transfers one renderer-owned FIFO row into a
// provider operation. The same queue id can therefore never remain both a
// staged row and a dispatched turn across a renderer crash/reload.
type PromoteStagedQueue struct {
	QueueID      string
	OperationID  provider.OperationID
	LaneID       provider.LaneID
	ModelID      string
	ModeID       string
	Permission   string
	Presentation provider.TurnPresentation
}

func (PromoteStagedQueue) chatCommand() {}

// DeleteChat writes a durable tombstone before native attachments and cached
// renderer rows are removed. Force is an explicit user/agent delete intent;
// it is never inferred from a missing mirror row.
type DeleteChat struct {
	OperationID provider.OperationID
	Force       bool
}

func (DeleteChat) chatCommand() {}

// AttachTab updates only the disposable renderer attachment for an idle chat.
// It never changes ChatID, lane identity, thread identity, or workspace epoch.
type AttachTab struct{ TabID string }

func (AttachTab) chatCommand() {}
