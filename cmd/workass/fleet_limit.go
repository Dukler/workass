package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Narrowing, origin, and the rule that replaced max_depth.
//
// A forwarded command's effective permission is `min(link grant, caller limit)`
// and nothing else: it can never obtain a permission the caller did not hold,
// and never one the link was not given (docs/specs/fleet-runtime.md §6, §11).
// The vocabulary is the read/edit/full intent the provider lanes already use
// (`PermissionIntentForMode`), so this is a narrowing of something that already
// exists rather than a second permission model beside it.
//
// `sandbox.max_depth` is REMOVED by decision (§15). What bounds escalation is
// the no-chaining rule below: work that arrives through a link below full access
// may not use the receiving daemon's own links, so a narrowed grant cannot be
// laundered into a wider one by passing through a third machine. A permission
// rule is simpler than a counter, needs no bookkeeping, and bounds the thing
// that actually matters.

// fleetEnvelopeVersion is the envelope's version field. It exists so a future
// shape can be refused by name rather than misread.
const fleetEnvelopeVersion = 1

// The reserved params the local edge uses to hand its own ceiling and
// provenance to the router. They never reach a provider: the router lifts them
// into the envelope and deletes them from the forwarded params.
const (
	fleetLimitParam  = "_fleet_limit"
	fleetOriginParam = "_fleet_origin"
)

// The permission ceiling vocabulary, in increasing order.
const (
	fleetLimitRead = "read"
	fleetLimitEdit = "edit"
	fleetLimitFull = "full"
)

// fleetLimitRank orders the ceilings. An unstated ceiling is rank 0, which is
// NOT "the lowest": it means the caller did not say, which is what every peer
// and every relay did before this envelope existed. Treating silence as a
// refusal would break a mixed fleet; treating it as a claim would be a lie. So
// it is carried as unstated and resolved once, in fleetEffectiveLimit.
func fleetLimitRank(limit string) int {
	switch strings.ToLower(strings.TrimSpace(limit)) {
	case fleetLimitRead:
		return 1
	case fleetLimitEdit:
		return 2
	case fleetLimitFull:
		return 3
	default:
		return 0
	}
}

// fleetEffectiveLimit is the lesser of the link's grant and the caller's limit.
//
// Where neither is stated the answer is full, which is exactly what a forwarded
// command has always been able to do: fleet membership means "my machines", and
// a peer reached over the legacy relay states nothing at all. Where one is
// stated it binds. Where both are, the lower wins — a `limit` wider than the
// link's grant is narrowed to the grant, never the other way round.
func fleetEffectiveLimit(grant, limit string) string {
	grantRank, limitRank := fleetLimitRank(grant), fleetLimitRank(limit)
	switch {
	case grantRank == 0 && limitRank == 0:
		return fleetLimitFull
	case grantRank == 0:
		return canonicalFleetLimit(limit)
	case limitRank == 0:
		return canonicalFleetLimit(grant)
	case grantRank <= limitRank:
		return canonicalFleetLimit(grant)
	default:
		return canonicalFleetLimit(limit)
	}
}

func canonicalFleetLimit(limit string) string {
	switch fleetLimitRank(limit) {
	case 1:
		return fleetLimitRead
	case 2:
		return fleetLimitEdit
	case 3:
		return fleetLimitFull
	default:
		return fleetLimitFull
	}
}

// fleetCommandOrigin is the immutable provenance stamp. It travels with
// everything derived from a forwarded command, so "who asked for this, through
// which link, under what limit" is answerable per entity after the fact.
type fleetCommandOrigin struct {
	Machine     string `json:"machine,omitempty"`
	ChatID      string `json:"chat_id,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	TurnID      string `json:"turn_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

func (o fleetCommandOrigin) empty() bool {
	return o.Machine == "" && o.ChatID == "" && o.TabID == "" && o.TurnID == "" && o.OperationID == ""
}

func (o fleetCommandOrigin) payload() map[string]any {
	out := map[string]any{}
	if o.Machine != "" {
		out["machine"] = o.Machine
	}
	if o.ChatID != "" {
		out["chat_id"] = o.ChatID
	}
	if o.TabID != "" {
		out["tab_id"] = o.TabID
	}
	if o.TurnID != "" {
		out["turn_id"] = o.TurnID
	}
	if o.OperationID != "" {
		out["operation_id"] = o.OperationID
	}
	return out
}

func fleetOriginFrom(raw any) fleetCommandOrigin {
	fields := mapFromAnyMain(raw)
	return fleetCommandOrigin{
		Machine:     fieldString(fields, "machine"),
		ChatID:      fieldString(fields, "chat_id"),
		TabID:       fieldString(fields, "tab_id"),
		TurnID:      fieldString(fields, "turn_id"),
		OperationID: fieldString(fields, "operation_id"),
	}
}

// fleetForwardedWork is what the receiving daemon knows about a command that
// arrived over a link. It rides the request context, because the local handler
// it reaches is the same code a local tool call reaches: there is one command
// surface, so the only honest way to tell the two apart is beside the call
// rather than inside it.
type fleetForwardedWork struct {
	Peer        string
	Limit       string
	Origin      fleetCommandOrigin
	OperationID string
}

type fleetForwardedKey struct{}

// withFleetForwardedWork marks a context as serving one forwarded command.
func withFleetForwardedWork(ctx context.Context, work fleetForwardedWork) context.Context {
	return context.WithValue(ctx, fleetForwardedKey{}, work)
}

// fleetForwardedWorkFrom reads the mark, if there is one.
func fleetForwardedWorkFrom(ctx context.Context) (fleetForwardedWork, bool) {
	if ctx == nil {
		return fleetForwardedWork{}, false
	}
	work, ok := ctx.Value(fleetForwardedKey{}).(fleetForwardedWork)
	return work, ok
}

// fleetNarrowedCapabilityError says what is missing, which is the whole point:
// a home that will not accept a narrowed command answers immediately with the
// capability it lacks — never a silent queue and never a generic denial (§16.4).
func fleetNarrowedCapabilityError(capability, method, limit string) error {
	return fmt.Errorf("this command arrived over a fleet link with %s access, which does not carry %s; %s needs full access on the link that forwards it",
		limit, capability, method)
}

// fleetRefuseNarrowedSurface applies §11's refusals to one forwarded command.
// Work below full access may not change this machine's settings, projects,
// schedules or updates, and may not reconfigure a chat's workspace.
func fleetRefuseNarrowedSurface(method, limit string, params map[string]any) error {
	if canonicalFleetLimit(limit) == fleetLimitFull {
		return nil
	}
	switch {
	case strings.HasPrefix(method, "schedule."):
		return fleetNarrowedCapabilityError("schedule.*", method, limit)
	case strings.HasPrefix(method, "update."):
		return fleetNarrowedCapabilityError("update.*", method, limit)
	case method == "chat.configure":
		// Its own entity is allowed; the workspace it runs in is not. A cwd is
		// project mutation by another name — it decides which files on this
		// machine the chat can touch at all.
		if cwd := strings.TrimSpace(fieldString(params, "cwd")); cwd != "" && cwd != "inherit" {
			return fleetNarrowedCapabilityError("workspace mutation (chat.configure cwd)", method, limit)
		}
	case method == "chat.delete":
		return fleetNarrowedCapabilityError("chat.delete", method, limit)
	case method == "process.cancel", method == "agent.cancel_hosted":
		// Reading another machine's jobs is a read; stopping one is not. A
		// narrowed link may watch the fleet's work and may not end it.
		return fleetNarrowedCapabilityError("process.cancel", method, limit)
	}
	return nil
}

// fleetRefuseSecondHop is the no-chaining rule. Work that arrived through a
// link below full access may not use the receiving daemon's OWN links, so a
// narrowed grant cannot be widened by passing through a third machine.
//
// Full-access work may chain: that is the operator's own fleet acting on their
// behalf, and its storm guards are the per-peer rate limits and the notice window,
// not an arbitrary depth counter.
func fleetRefuseSecondHop(ctx context.Context, method string) error {
	work, forwarded := fleetForwardedWorkFrom(ctx)
	if !forwarded || canonicalFleetLimit(work.Limit) == fleetLimitFull {
		return nil
	}
	return errors.New("this command arrived over a fleet link with " + canonicalFleetLimit(work.Limit) +
		" access, so it may not forward " + method + " on to a third machine; a narrowed grant cannot be widened by another hop")
}
