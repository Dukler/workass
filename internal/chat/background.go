package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"workass/internal/provider"
)

// BackgroundActionKind is the provider-neutral command surface for every
// mutation of chat-owned background work. Provider managers execute these
// effects; they never own ordering or semantic state.
type BackgroundActionKind string

const (
	BackgroundSpawnAgent       BackgroundActionKind = "spawn_agent"
	BackgroundMessageAgent     BackgroundActionKind = "message_agent"
	BackgroundRetryAgent       BackgroundActionKind = "retry_agent"
	BackgroundCancelAgent      BackgroundActionKind = "cancel_agent"
	BackgroundAgentPermission  BackgroundActionKind = "agent_permission"
	BackgroundRegisterExternal BackgroundActionKind = "register_external"
	BackgroundSettleExternal   BackgroundActionKind = "settle_external"
	BackgroundStopWork         BackgroundActionKind = "stop_work"
)

// BackgroundAction is a typed union with one shared owner and immutable
// operation identity. Fields are grouped by operation instead of leaking
// provider-specific branches into the chat actor.
type BackgroundAction struct {
	Kind        BackgroundActionKind
	OperationID provider.OperationID
	Owner       ProviderActivityOwner
	TabID       string
	ChatID      string

	Spawn      *SpawnAgentAction
	Message    *MessageAgentAction
	Retry      *RetryAgentAction
	Cancel     *CancelAgentAction
	Permission *AgentPermissionAction
	Register   *RegisterExternalAction
	Settle     *SettleExternalAction
	Stop       *StopWorkAction
}

type SpawnAgentAction struct {
	Prompt           string
	Label            string
	ProviderID       string
	ModelID          string
	Effort           string
	ModeID           string
	CWD              string
	Profile          string
	PermissionIntent string
}

type MessageAgentAction struct {
	WorkID  string
	Message string
}

type RetryAgentAction struct {
	WorkID  string
	Message string
}

type CancelAgentAction struct{ WorkID string }

type AgentPermissionAction struct {
	WorkID   string
	Decision string
}

type RegisterExternalAction struct {
	Label      string
	Role       string
	PID        *int
	OutputFile string
	DoneFile   string
}

type SettleExternalAction struct {
	WorkID   string
	Status   string
	ExitCode *int
	Summary  string
}

type StopWorkAction struct{ WorkID string }

func (a BackgroundAction) Clone() BackgroundAction {
	out := a
	if a.Spawn != nil {
		value := *a.Spawn
		out.Spawn = &value
	}
	if a.Message != nil {
		value := *a.Message
		out.Message = &value
	}
	if a.Retry != nil {
		value := *a.Retry
		out.Retry = &value
	}
	if a.Cancel != nil {
		value := *a.Cancel
		out.Cancel = &value
	}
	if a.Permission != nil {
		value := *a.Permission
		out.Permission = &value
	}
	if a.Register != nil {
		value := *a.Register
		if a.Register.PID != nil {
			pid := *a.Register.PID
			value.PID = &pid
		}
		out.Register = &value
	}
	if a.Settle != nil {
		value := *a.Settle
		if a.Settle.ExitCode != nil {
			code := *a.Settle.ExitCode
			value.ExitCode = &code
		}
		out.Settle = &value
	}
	if a.Stop != nil {
		value := *a.Stop
		out.Stop = &value
	}
	return out
}

// SameRequest compares the immutable caller intent for an exact-once
// background mutation. Provider ownership is deliberately excluded: a lost
// reply may be retried after the foreground turn has advanced, but the durable
// outbox retains the original owner. Chat/tab identity, operation identity,
// kind, and the complete typed payload must remain byte-for-byte equivalent.
func (a BackgroundAction) SameRequest(other BackgroundAction) bool {
	left, right := a.Clone(), other.Clone()
	left.Owner = ProviderActivityOwner{}
	right.Owner = ProviderActivityOwner{}
	left.OperationID = provider.NormalizeOperationID(string(left.OperationID))
	right.OperationID = provider.NormalizeOperationID(string(right.OperationID))
	left.TabID, right.TabID = strings.TrimSpace(left.TabID), strings.TrimSpace(right.TabID)
	left.ChatID, right.ChatID = strings.TrimSpace(left.ChatID), strings.TrimSpace(right.ChatID)
	return reflect.DeepEqual(left, right)
}

func (a BackgroundAction) Validate(state State) error {
	a.OperationID = provider.NormalizeOperationID(string(a.OperationID))
	if a.OperationID == "" || strings.TrimSpace(a.ChatID) == "" || strings.TrimSpace(a.TabID) == "" {
		return errors.New("background action requires operation, chat, and tab identity")
	}
	if strings.TrimSpace(a.ChatID) != state.ChatID || strings.TrimSpace(a.TabID) != strings.TrimSpace(state.Presentation.TabID) {
		return errors.New("background action does not match the actor attachment")
	}
	if err := validateProviderActivityOwner(state, a.Owner); err != nil {
		return fmt.Errorf("background action owner: %w", err)
	}
	if a.Owner.OperationID == "" {
		return errors.New("background action owner is missing operation identity")
	}
	payloads := 0
	for _, present := range []bool{a.Spawn != nil, a.Message != nil, a.Retry != nil, a.Cancel != nil, a.Permission != nil, a.Register != nil, a.Settle != nil, a.Stop != nil} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return errors.New("background action requires exactly one typed payload")
	}
	correct := map[BackgroundActionKind]bool{
		BackgroundSpawnAgent:       a.Spawn != nil,
		BackgroundMessageAgent:     a.Message != nil,
		BackgroundRetryAgent:       a.Retry != nil,
		BackgroundCancelAgent:      a.Cancel != nil,
		BackgroundAgentPermission:  a.Permission != nil,
		BackgroundRegisterExternal: a.Register != nil,
		BackgroundSettleExternal:   a.Settle != nil,
		BackgroundStopWork:         a.Stop != nil,
	}[a.Kind]
	if !correct {
		return fmt.Errorf("background action %q has the wrong typed payload", a.Kind)
	}
	if a.Kind == BackgroundSpawnAgent || a.Kind == BackgroundRegisterExternal {
		if !backgroundOwnerExists(state, a.Owner) {
			return errors.New("new background work must be tied to an exact current or historical actor turn")
		}
	}
	target := a.TargetWorkID()
	if target != "" {
		existing, ok := state.Background[target]
		if !ok {
			return fmt.Errorf("background work %q is not actor-owned", target)
		}
		if existing.Owner != a.Owner {
			return errors.New("background action changed immutable work ownership")
		}
	}
	return nil
}

func backgroundOwnerExists(state State, owner ProviderActivityOwner) bool {
	return validateProviderActivityOwner(state, owner) == nil
}

func (a BackgroundAction) TargetWorkID() string {
	switch {
	case a.Message != nil:
		return strings.TrimSpace(a.Message.WorkID)
	case a.Retry != nil:
		return strings.TrimSpace(a.Retry.WorkID)
	case a.Cancel != nil:
		return strings.TrimSpace(a.Cancel.WorkID)
	case a.Permission != nil:
		return strings.TrimSpace(a.Permission.WorkID)
	case a.Settle != nil:
		return strings.TrimSpace(a.Settle.WorkID)
	case a.Stop != nil:
		return strings.TrimSpace(a.Stop.WorkID)
	default:
		return ""
	}
}

type RequestBackgroundAction struct{ Action BackgroundAction }

func (RequestBackgroundAction) chatCommand() {}

type BackgroundActionCompleted struct {
	OperationID provider.OperationID
	Result      json.RawMessage
}

func (BackgroundActionCompleted) chatCommand() {}

type BackgroundActionFailed struct {
	OperationID provider.OperationID
	Kind        provider.ErrorKind
	Ambiguous   bool
}

func (BackgroundActionFailed) chatCommand() {}

// ReconcileBackgroundSnapshot is the actor ingress for executor/liveness
// evidence. Runtime inventory is not semantic ownership: incoming rows may
// update exact actor-owned work, but omission never deletes terminal history.
// When the executor proves its inventory is complete, an omitted live row is
// made visibly orphaned instead of being silently erased or left running.
type ReconcileBackgroundSnapshot struct {
	Items                []BackgroundState
	AuthoritativeAbsence bool
	ObservedAt           string
}

func (ReconcileBackgroundSnapshot) chatCommand() {}

type BackgroundActionEffect struct{ Action BackgroundAction }

func (BackgroundActionEffect) chatEffect() {}

func reduceRequestBackgroundAction(state *State, command RequestBackgroundAction) ([]Effect, error) {
	action := command.Action.Clone()
	if err := action.Validate(*state); err != nil {
		return nil, err
	}
	if _, exists := state.Operations[action.OperationID]; exists {
		for _, entry := range state.Outbox {
			if entry.Kind != EffectBackground || entry.OperationID != action.OperationID {
				continue
			}
			if entry.Background == nil || !entry.Background.SameRequest(action) {
				return nil, errors.New("background operation id was reused for different content")
			}
			return nil, nil
		}
		return nil, errors.New("background operation id is already owned by another actor command")
	}
	state.Operations[action.OperationID] = struct{}{}
	return []Effect{BackgroundActionEffect{Action: action}}, nil
}

func reduceBackgroundActionCompleted(state *State, command BackgroundActionCompleted) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	for i := range state.Outbox {
		entry := &state.Outbox[i]
		if entry.Kind != EffectBackground || entry.OperationID != operationID {
			continue
		}
		if entry.Status == OutboxCompleted {
			return nil
		}
		if entry.Status != OutboxDispatched {
			return fmt.Errorf("background action cannot complete from %q", entry.Status)
		}
		entry.Status = OutboxCompleted
		entry.LastError = ""
		entry.Result = append(json.RawMessage(nil), command.Result...)
		return nil
	}
	return errors.New("background action receipt has no durable operation")
}

func reduceBackgroundActionFailed(state *State, command BackgroundActionFailed) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	kind := command.Kind
	if kind == "" {
		kind = provider.ErrorAdmissionRejected
	}
	for i := range state.Outbox {
		entry := &state.Outbox[i]
		if entry.Kind != EffectBackground || entry.OperationID != operationID {
			continue
		}
		entry.LastError = kind
		if command.Ambiguous {
			entry.Status = OutboxAmbiguous
		} else {
			entry.Status = OutboxFailed
		}
		return nil
	}
	return errors.New("background action failure has no durable operation")
}

func reduceReconcileBackgroundSnapshot(state *State, command ReconcileBackgroundSnapshot) error {
	incoming, err := canonicalBackgroundSnapshot(*state, command.Items)
	if err != nil {
		return err
	}
	next := make(map[string]BackgroundState, len(state.Background)+len(incoming))
	for id, item := range state.Background {
		next[id] = item
	}
	for id, item := range incoming {
		next[id] = item
	}
	if command.AuthoritativeAbsence {
		observedAt := strings.TrimSpace(command.ObservedAt)
		if observedAt == "" {
			return errors.New("authoritative background reconciliation requires an observed timestamp")
		}
		for id, item := range next {
			if _, present := incoming[id]; present || !strings.EqualFold(strings.TrimSpace(item.Event.Status), "running") {
				continue
			}
			item.Event.Status = "orphaned"
			item.Event.UpdatedAt = observedAt
			if strings.TrimSpace(item.Event.FinishedAt) == "" {
				item.Event.FinishedAt = observedAt
			}
			next[id] = item
		}
	}
	state.Background = next
	return nil
}

func canonicalBackgroundSnapshot(state State, items []BackgroundState) (map[string]BackgroundState, error) {
	next := make(map[string]BackgroundState, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.Event.WorkID)
		if id == "" {
			return nil, errors.New("background snapshot contains an item without identity")
		}
		if _, duplicate := next[id]; duplicate {
			return nil, fmt.Errorf("background snapshot duplicates work %q", id)
		}
		if err := validateProviderActivityOwner(state, item.Owner); err != nil {
			return nil, fmt.Errorf("background work %q: %w", id, err)
		}
		if item.Owner.OperationID == "" {
			return nil, fmt.Errorf("background work %q is missing immutable operation ownership", id)
		}
		next[id] = item
	}
	return next, nil
}

func sameBackgroundSnapshot(left, right map[string]BackgroundState) bool {
	return reflect.DeepEqual(left, right)
}
