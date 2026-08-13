package provider

import (
	"errors"
	"strings"
)

type EventKind string

const (
	EventLaneAttached         EventKind = "lane_attached"
	EventLaneDetached         EventKind = "lane_detached"
	EventLaneCapabilities     EventKind = "lane_capabilities"
	EventLineageAdvanced      EventKind = "lineage_advanced"
	EventTurnAdmitted         EventKind = "turn_admitted"
	EventInputConsumed        EventKind = "input_consumed"
	EventAssistantChunk       EventKind = "assistant_chunk"
	EventAssistantMedia       EventKind = "assistant_media"
	EventThinkingUpdate       EventKind = "thinking_update"
	EventToolUpdate           EventKind = "tool_update"
	EventPlanUpdate           EventKind = "plan_update"
	EventPermissionRequested  EventKind = "permission_requested"
	EventPermissionResolved   EventKind = "permission_resolved"
	EventUsageUpdated         EventKind = "usage_updated"
	EventCompactionStarted    EventKind = "compaction_started"
	EventCompactionCheckpoint EventKind = "compaction_checkpoint"
	EventCheckpointRestored   EventKind = "checkpoint_restored"
	EventBackgroundWork       EventKind = "background_work"
	EventTurnTerminal         EventKind = "turn_terminal"
	EventTransportHealth      EventKind = "transport_health"
)

type EventIdentity struct {
	ChatID      string
	LaneID      LaneID
	OperationID OperationID
	TurnID      string
	Sequence    uint64
	// ObservedAtUnixMS is stamped once at the adapter boundary. Renderer-visible
	// lifecycle timing must survive daemon restarts and therefore cannot be
	// recreated later by the projector or browser.
	ObservedAtUnixMS int64
}

func (i EventIdentity) Validate() error {
	if strings.TrimSpace(i.ChatID) == "" || i.LaneID == "" {
		return errors.New("provider event requires chat and lane identity")
	}
	if i.Sequence == 0 {
		return errors.New("provider event requires a positive adapter sequence")
	}
	return nil
}

type AssistantPhase string

const (
	AssistantPhaseContent    AssistantPhase = "content"
	AssistantPhaseCommentary AssistantPhase = "commentary"
	AssistantPhaseFinal      AssistantPhase = "final_answer"
)

type AssistantEvent struct {
	Phase AssistantPhase
	Text  string
	Final bool
	// TypedPhase distinguishes a provider-owned commentary/final boundary from
	// the compatibility stream used by phase-less ACP agents. A terminal result
	// may replace the latter as the provider's canonical whole-turn result, but
	// must never flatten an already typed commentary/final transcript.
	TypedPhase bool
}

type AssistantMediaEvent struct {
	Attachments []Attachment
}

type ThinkingEvent struct {
	Text string
}

type InputEvent struct {
	OperationID  OperationID
	NativeTurnID string
	// Thread is present only when consuming this input is also the provider's
	// durable creation receipt.
	Thread *ThreadRef
}

type ToolEvent struct {
	ToolCallID       string
	ToolKind         string
	Title            string
	Status           string
	Command          string
	TerminalID       string
	Input            string
	Output           string
	Location         string
	Attachments      []Attachment
	SubagentID       string
	SubagentLabel    string
	SubagentProvider string
	SubagentModel    string
	SubagentHeader   bool
	StartedAtUnixMS  int64
	EndedAtUnixMS    int64
}

type PlanEntry struct {
	ID     string
	Text   string
	Status string
}

type PlanEvent struct {
	Entries []PlanEntry
}

type PermissionEvent struct {
	RequestID        string
	Title            string
	Kind             string
	Status           string
	Options          []string
	OptionDetails    []PermissionOption
	Question         *PermissionQuestion
	ResolvedOptionID string
}

type PermissionOption struct {
	ID   string
	Name string
	Kind string
}

type PermissionQuestion struct {
	Question    string
	Header      string
	Options     []PermissionQuestionOption
	MultiSelect bool
}

type PermissionQuestionOption struct {
	Label       string
	Description string
}

type UsageEvent struct {
	Used         int
	Size         int
	InputTokens  int
	OutputTokens int
	// ObservedAtUnixMS is copied from the canonical adapter event identity when
	// usage becomes actor state. It makes renderer projection deterministic
	// across restarts and across several lanes for the same provider.
	ObservedAtUnixMS int64
}

type CompactionEvent struct {
	CheckpointID string
	Coverage     uint64
	Digest       string
}

type CheckpointRestoredEvent struct{ TurnSequence int }

type BackgroundEvent struct {
	WorkID        string
	TaskID        string
	ToolCallID    string
	Title         string
	Kind          string
	Role          string
	Status        string
	StartedAt     string
	UpdatedAt     string
	FinishedAt    string
	ExitCode      *int
	Summary       string
	OutputFile    string
	PID           *int
	LastToolName  string
	ModelLabel    string
	ResultExcerpt string
}

type TransportHealthEvent struct {
	State string
	Error ErrorKind
}

type TerminalEvent struct {
	Status            string
	StopReason        string
	Result            string
	Error             string
	FinishedAt        string
	Code              *int
	Interrupted       bool
	CrashInterrupted  bool
	DispositionState  string
	DispositionSource string
	DispositionNote   string
	ConsumedSteerIDs  []OperationID
	Attachments       []Attachment
}

// Event is a typed discriminated union. Exactly the payload matching Kind may
// be set; adapters reject malformed vendor events before emitting it.
type Event struct {
	Kind       EventKind
	Identity   EventIdentity
	Thread     *ThreadRef
	Admission  *TurnAdmission
	Input      *InputEvent
	Assistant  *AssistantEvent
	Media      *AssistantMediaEvent
	Thinking   *ThinkingEvent
	Tool       *ToolEvent
	Plan       *PlanEvent
	Permission *PermissionEvent
	Usage      *UsageEvent
	Compaction *CompactionEvent
	Restored   *CheckpointRestoredEvent
	Background *BackgroundEvent
	Terminal   *TerminalEvent
	Health     *TransportHealthEvent
	Attachment *LaneAttachmentSnapshot
}

func (e Event) Validate() error {
	if err := e.Identity.Validate(); err != nil {
		return err
	}
	payloads := 0
	for _, present := range []bool{
		e.Thread != nil,
		e.Admission != nil,
		e.Input != nil,
		e.Assistant != nil,
		e.Media != nil,
		e.Thinking != nil,
		e.Tool != nil,
		e.Plan != nil,
		e.Permission != nil,
		e.Usage != nil,
		e.Compaction != nil,
		e.Restored != nil,
		e.Background != nil,
		e.Terminal != nil,
		e.Health != nil,
		e.Attachment != nil,
	} {
		if present {
			payloads++
		}
	}
	requiresPayload := e.Kind != EventLaneDetached
	if requiresPayload && payloads != 1 {
		return errors.New("provider event requires exactly one typed payload")
	}
	if !requiresPayload && payloads > 1 {
		return errors.New("provider event contains conflicting payloads")
	}
	correctPayload := false
	switch e.Kind {
	case EventLaneAttached, EventLineageAdvanced:
		correctPayload = e.Thread != nil
	case EventLaneDetached:
		correctPayload = payloads == 0
	case EventLaneCapabilities:
		correctPayload = e.Attachment != nil
	case EventTurnAdmitted:
		correctPayload = e.Admission != nil
	case EventInputConsumed:
		correctPayload = e.Input != nil
	case EventAssistantChunk:
		correctPayload = e.Assistant != nil
	case EventAssistantMedia:
		correctPayload = e.Media != nil
	case EventThinkingUpdate:
		correctPayload = e.Thinking != nil
	case EventToolUpdate:
		correctPayload = e.Tool != nil
	case EventPlanUpdate:
		correctPayload = e.Plan != nil
	case EventPermissionRequested, EventPermissionResolved:
		correctPayload = e.Permission != nil
	case EventUsageUpdated:
		correctPayload = e.Usage != nil
	case EventCompactionStarted, EventCompactionCheckpoint:
		correctPayload = e.Compaction != nil
	case EventCheckpointRestored:
		correctPayload = e.Restored != nil
	case EventBackgroundWork:
		correctPayload = e.Background != nil
	case EventTurnTerminal:
		correctPayload = e.Terminal != nil
	case EventTransportHealth:
		correctPayload = e.Health != nil
	default:
		return errors.New("provider event has unknown kind")
	}
	if !correctPayload {
		return errors.New("provider event payload does not match its kind")
	}
	switch e.Kind {
	case EventTurnAdmitted:
		if e.Admission.Turn.OperationID == "" || (e.Admission.Accepted && strings.TrimSpace(e.Admission.Turn.NativeID) == "") {
			return errors.New("turn admission event is missing stable operation or native turn identity")
		}
	case EventInputConsumed:
		if e.Input.OperationID == "" {
			return errors.New("input consumption event is missing operation identity")
		}
		if e.Input.Thread != nil {
			thread := e.Input.Thread.Normalize()
			if err := thread.Validate(""); err != nil {
				return errors.New("input consumption event has an invalid durable thread receipt")
			}
		}
	case EventAssistantChunk:
		if e.Identity.OperationID == "" || e.Assistant.Text == "" {
			return errors.New("assistant event is missing operation identity or text")
		}
	case EventAssistantMedia:
		if e.Identity.OperationID == "" || len(e.Media.Attachments) == 0 {
			return errors.New("assistant media event is missing operation identity or attachments")
		}
	case EventThinkingUpdate:
		if e.Identity.OperationID == "" {
			return errors.New("thinking event is missing operation identity")
		}
	case EventToolUpdate:
		if strings.TrimSpace(e.Tool.ToolCallID) == "" {
			return errors.New("tool event is missing tool-call identity")
		}
	case EventPlanUpdate:
		if e.Identity.OperationID == "" {
			return errors.New("plan event is missing operation identity")
		}
	case EventPermissionRequested, EventPermissionResolved:
		if strings.TrimSpace(e.Permission.RequestID) == "" || strings.TrimSpace(e.Permission.Status) == "" {
			return errors.New("permission event is missing request identity or status")
		}
	case EventCompactionCheckpoint:
		if strings.TrimSpace(e.Compaction.CheckpointID) == "" || strings.TrimSpace(e.Compaction.Digest) == "" {
			return errors.New("compaction checkpoint is missing provider identity or digest")
		}
	case EventCheckpointRestored:
		if e.Restored.TurnSequence <= 0 {
			return errors.New("checkpoint restore is missing a positive turn sequence")
		}
	case EventBackgroundWork:
		if strings.TrimSpace(e.Background.WorkID) == "" || strings.TrimSpace(e.Background.Status) == "" {
			return errors.New("background event is missing work identity or status")
		}
	case EventTurnTerminal:
		switch strings.ToLower(strings.TrimSpace(e.Terminal.Status)) {
		case "completed", "done", "cancelled", "failed":
		default:
			return errors.New("terminal event has unknown status")
		}
	case EventTransportHealth:
		if strings.TrimSpace(e.Health.State) == "" {
			return errors.New("transport health event is missing state")
		}
	}
	return nil
}
