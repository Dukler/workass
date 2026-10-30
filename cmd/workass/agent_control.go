package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/artifacthost"
	"workass/internal/chat"
	"workass/internal/procchild"
	providercontract "workass/internal/provider"
)

type agentControlHandler struct {
	manager     *acp.Manager
	chats       *chatControlCoordinator
	artifacts   *artifacthost.Registry
	remoteChats agentChatRemoteRouter
	// headlessUpdates is set only on an install with no Electron shell. When it
	// is present it owns the update.* family; otherwise the renderer does.
	headlessUpdates *headlessUpdateService
	// selfMachineID lets an explicit fleet machine id that happens to name THIS
	// machine take the ordinary local path, so "machine" is a uniform argument
	// rather than a switch between two different tools.
	selfMachineID string
	forks         *acp.ForkService
	validateOwner func(ownerKey, chatID, tabID string) bool
	// fleetHome owns this machine's entity logs and the monitors it holds for
	// other machines' watchers; fleetWatch owns the monitors this machine holds
	// on other machines' entities. Both are nil on an install with no fleet
	// key, which is exactly the P2 behaviour: no mesh, no monitors, nothing
	// changed.
	fleetHome  *fleetHome
	fleetWatch *fleetWatcher
	// fleetMesh is how a home reaches the owner's daemon to offer a monitor it
	// never asked for, which is the one thing P4 needs that P3 did not: a
	// process child is registered on the machine that RUNS it, so its owner
	// made no call it could learn from.
	fleetMesh fleetMonitorInvoker
}

type agentControlRequest struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

func newAgentControlHandler(manager *acp.Manager, broadcast func(string, any), chats *chatControlCoordinator) (*agentControlHandler, error) {
	return newAgentControlHandlerWithStartupResume(manager, broadcast, chats, true)
}

// newAgentControlHandlerBeforeProviderStartup builds the final MCP control
// surface without waking durable chat queues. The daemon uses it while the
// HTTPS listener is being started; provider startup is released explicitly
// after the listener readiness probe succeeds.
func newAgentControlHandlerBeforeProviderStartup(manager *acp.Manager, broadcast func(string, any), chats *chatControlCoordinator) (*agentControlHandler, error) {
	return newAgentControlHandlerWithStartupResume(manager, broadcast, chats, false)
}

func newAgentControlHandlerWithStartupResume(manager *acp.Manager, broadcast func(string, any), chats *chatControlCoordinator, resumeQueues bool) (*agentControlHandler, error) {
	if manager == nil {
		return nil, errors.New("agent control requires an ACP manager")
	}
	if chats == nil || chats.providerChats == nil {
		return nil, errors.New("agent control requires the singleton durable chat actor runtime")
	}
	handler := &agentControlHandler{manager: manager, chats: chats}
	if resumeQueues {
		chats.resumeQueues()
	}
	return handler, nil
}

// fenceActor checks the durable exact pair before any Manager capability is
// consulted. Manager sessions are disposable execution attachments; a live
// one must not keep a deleted or stale actor addressable.
func (h *agentControlHandler) fenceActor(tabID, chatID string) error {
	if h == nil || h.chats == nil || h.chats.providerChats == nil {
		return errors.New("Workass agent control is unavailable")
	}
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return errors.New("exact tab and chat ids are required")
	}
	actor, err := h.chats.providerChats.actor(chatID)
	if err != nil {
		return err
	}
	identity := actor.engine.IdentitySnapshot()
	if identity.Deleted {
		return errors.New("chat was deleted")
	}
	if strings.TrimSpace(identity.TabID) != tabID {
		return errors.New("tab id does not own the requested chat")
	}
	return nil
}

func (h *agentControlHandler) authorizeActorOwner(ownerKey, tabID, chatID, message string) error {
	if err := h.fenceActor(tabID, chatID); err != nil {
		return err
	}
	if !h.ownerAuthorized(ownerKey, chatID, tabID) {
		return errors.New(message)
	}
	return nil
}

func (h *agentControlHandler) ownerAuthorized(ownerKey, chatID, tabID string) bool {
	if h == nil {
		return false
	}
	if h.validateOwner != nil {
		return h.validateOwner(ownerKey, chatID, tabID)
	}
	return h.manager != nil && h.manager.ValidateAgentOwner(ownerKey, chatID, tabID)
}

func (h *agentControlHandler) call(r *http.Request, request agentControlRequest) (any, error) {
	params := request.Params
	chatID := fieldString(params, "parent_chat_id")
	tabID := fieldString(params, "parent_tab_id")
	ownerKey := fieldString(params, "owner_key")
	// A one-shot supervised-job credential is not a general one. It is minted by
	// `process.start` for exactly one job (fleet_process_start.go) and it
	// authorizes exactly one call: that job's own registration. The list is
	// allow-only, so a method added later is refused until somebody decides it
	// belongs here.
	if err := h.fenceRunCredential(request.Method, ownerKey, tabID, chatID, params); err != nil {
		return nil, err
	}
	// One authority decides whether this call addresses this machine or another
	// fleet machine, for every tool, before any local state is touched.
	if self := strings.TrimSpace(h.selfMachineID); self != "" && self == strings.TrimSpace(fieldString(params, "machine_id")) {
		params = copyAnyMap(params)
		delete(params, "machine_id")
	}
	// The same uniformity for ids: an id whose home is this machine resolves to
	// the in-process path, whether it was spelled as a global fleet id, as the
	// legacy machine tag, or as the bare local id it contains. Another home's
	// id continues onward in the tag its relay already speaks, so nothing
	// downstream of here sees a shape it did not see before.
	resolvedParams, fleetRouteErr := resolveFleetAddressedParams(h.fleetRouter(), params)
	if fleetRouteErr != nil {
		return nil, fleetRouteErr
	}
	params = resolvedParams
	if machineID, remote, routeErr := remoteAgentMethodTarget(request.Method, params); routeErr != nil {
		return nil, routeErr
	} else if remote {
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerErrorFor(request.Method)); err != nil {
			return nil, err
		}
		if request.Method == "chat.send" {
			if err := remoteAgentRejectAttachments(params); err != nil {
				return nil, err
			}
		}
		return h.forwardRemoteAgentCall(r.Context(), request.Method, machineID, params)
	}
	// The subagent family now accepts an explicit pair so it can name a chat on
	// another machine. Locally that pair may only be the calling chat.
	switch request.Method {
	case "chat.subsession", "agent.spawn", "agent.list", "agent.receipts", "agent.wait", "agent.wait_many",
		"agent.message", "agent.retry", "agent.cancel", "agent.decide_permission":
		if _, _, err := remoteAgentSubagentPair(params, tabID, chatID); err != nil {
			return nil, err
		}
	}
	switch request.Method {
	case "update.targets", "update.status", "update.apply":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
			return nil, err
		}
		// The updater is the one family that is addressed by machine_id all the
		// way down, so the self-addressing strip above has to be undone before
		// either half answers. Without this a packaged install refused its OWN
		// machine — `workass_get_update_status` with the exact machine_id
		// `workass_list_update_targets` had just printed came back "update
		// status requires an exact machine_id", while every remote machine
		// worked, because only the self case ever lost the field.
		if request.Method != "update.targets" && strings.TrimSpace(fieldString(params, "machine_id")) == "" {
			params = copyAnyMap(params)
			params["machine_id"] = h.selfMachineID
		}
		// A headless daemon answers for itself; a packaged install asks its
		// stamped renderer, which owns the transactional UpdateManager.
		if h.headlessUpdates.available() {
			if request.Method == "update.targets" {
				return h.headlessUpdates.Targets(r.Context())
			}
			return h.headlessUpdates.headlessUpdateCall(r.Context(), request.Method, params)
		}
		if h.remoteChats == nil {
			return nil, errors.New("Workass updater MCP router has no local renderer")
		}
		return h.remoteChats.Call(r.Context(), request.Method, copyRemoteAgentRouteParams(params))
	case "chat.list", "chat.read", "chat.diagnostics", "chat.create", "chat.subsession", "chat.rename", "chat.settle", "chat.configure", "chat.focus", "chat.delete", "chat.send", "chat.cancel":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, "Workass chat control caller is not an owned ACP session"); err != nil {
			return nil, err
		}
		if request.Method == "chat.list" {
			local, err := h.chats.list()
			if err != nil || h.remoteChats == nil {
				return local, err
			}
			remote, routeErr := h.remoteChats.Call(r.Context(), request.Method, map[string]any{})
			if routeErr != nil {
				// Local chat control must remain usable while the shell is closed or
				// every mounted machine is offline. Report the missing projection in
				// the same result instead of hiding it or failing the local catalog.
				local["remoteAvailable"] = false
				local["remoteError"] = redactedSessionString(routeErr.Error())
				return local, nil
			}
			local["remoteAvailable"] = true
			return mergeRemoteAgentChatList(local, remote), nil
		}
		switch request.Method {
		case "chat.diagnostics":
			return h.chats.providerChats.TurnDiagnostics(params)
		case "chat.read":
			return h.chats.read(params)
		case "chat.create":
			return h.chats.create(r.Context(), tabID, chatID, params)
		case "chat.subsession":
			return h.chats.subsession(r.Context(), tabID, chatID, params)
		case "chat.rename":
			return h.chats.rename(params)
		case "chat.settle":
			return h.chats.settle(params)
		case "chat.configure":
			return h.chats.configure(r.Context(), params)
		case "chat.focus":
			return h.chats.focus(params)
		case "chat.delete":
			return h.chats.delete(params)
		case "chat.send":
			return h.chats.send(params)
		case "chat.cancel":
			return h.chats.cancel(params)
		}
		return nil, errors.New("unknown chat control method")
	case "schedule.create", "schedule.list", "schedule.cancel":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, "Workass chat control caller is not an owned ACP session"); err != nil {
			return nil, err
		}
		return h.scheduledMessageCall(request.Method, params, tabID, chatID)
	case "agent.catalog":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
			return nil, err
		}
		return h.manager.AgentCatalog(r.Context(), ownerKey, chatID, tabID)
	case "agent.question":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
			return nil, err
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("question tool requires the durable chat actor")
		}
		question, err := parseWorkassQuestion(params)
		if err != nil {
			return nil, err
		}
		return h.chats.providerChats.executeAgentQuestion(r.Context(), h.manager, ownerKey, tabID, chatID, question)
	case "agent.spawn":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
			return nil, err
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("subagent spawn requires the durable chat actor")
		}
		spawnOrigin := fleetCommandOrigin{}
		if work, forwarded := fleetForwardedWorkFrom(r.Context()); forwarded && !work.Origin.empty() {
			spawnOrigin = work.Origin
		}
		return h.chats.providerChats.RunBackgroundAction(r.Context(), tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundSpawnAgent, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Spawn: &chat.SpawnAgentAction{
				Prompt: fieldString(params, "prompt"), Label: acp.SanitizeDisplayText(fieldString(params, "label"), 80),
				ProviderID: fieldString(params, "provider_id"), ModelID: fieldString(params, "model_id"),
				Effort: fieldString(params, "effort"), ModeID: fieldString(params, "mode_id"),
				CWD: fieldString(params, "cwd"), Profile: fieldString(params, "profile"),
				PermissionIntent: fieldString(params, "permission_intent"),
				Fork:             boolFieldValue(params, "fork"),
				// The origin stamp, when this spawn arrived over a fleet link.
				// A child used to report into the chat on the machine that HOSTS
				// it; carrying the asking chat into the durable action is what
				// lets a monitor send the outcome to the chat that asked.
				OriginMachine: spawnOrigin.Machine, OriginChatID: spawnOrigin.ChatID, OriginTabID: spawnOrigin.TabID,
			},
		})
	case "agent.list":
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("agent list requires the durable chat actor")
		}
		subagents, err := h.chats.providerChats.ListSubagents(ownerKey, tabID, chatID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"subagents": subagents}, nil
	case "agent.wait":
		id := fieldString(params, "id")
		if id == "" {
			return nil, errors.New("subagent id is required")
		}
		timeoutMS := intField(params, "timeout_ms")
		if timeoutMS != -1 && timeoutMS != 0 && (timeoutMS < 1000 || timeoutMS > 3600000) {
			return nil, errors.New("subagent wait timeout_ms must be -1 (event-only), 0 (default), or between 1000 and 3600000")
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("agent wait requires the durable chat actor")
		}
		timeout := time.Duration(timeoutMS) * time.Millisecond
		operationID := providercontract.NormalizeOperationID(fieldString(params, "operation_id"))
		return h.chats.providerChats.WaitSubagent(r.Context(), ownerKey, tabID, chatID, operationID, id, timeout)
	case "agent.wait_many":
		ids := stringSliceField(params["ids"])
		if len(ids) == 0 {
			return nil, errors.New("at least one subagent id is required")
		}
		timeoutMS := intField(params, "timeout_ms")
		if timeoutMS != -1 && timeoutMS != 0 && (timeoutMS < 1000 || timeoutMS > 3600000) {
			return nil, errors.New("subagent wait timeout_ms must be -1 (event-only), 0 (default), or between 1000 and 3600000")
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("agent wait requires the durable chat actor")
		}
		operationID := providercontract.NormalizeOperationID(fieldString(params, "operation_id"))
		return h.chats.providerChats.WaitSubagents(r.Context(), ownerKey, tabID, chatID, operationID, ids, fieldString(params, "return_when"), time.Duration(timeoutMS)*time.Millisecond)
	case "agent.message":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
			return nil, err
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundMessageAgent, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Message: &chat.MessageAgentAction{WorkID: fieldString(params, "id"), Message: fieldString(params, "message")},
		})
	case "agent.retry":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
			return nil, err
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundRetryAgent, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Retry: &chat.RetryAgentAction{WorkID: fieldString(params, "id"), Message: fieldString(params, "message")},
		})
	case "agent.receipts":
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("agent receipts require the durable chat actor")
		}
		receipts, err := h.chats.providerChats.ListSubagentReceipts(ownerKey, tabID, chatID, intField(params, "limit"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"receipts": receipts}, nil
	case "spawned_work.list":
		tailChars := intField(params, "tail_chars")
		if tailChars < 0 || tailChars > 12000 {
			return nil, errors.New("spawned work tail_chars must be between 0 and 12000")
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("spawned work list requires the durable chat actor")
		}
		items, err := h.chats.providerChats.ListSpawnedWorkForOwner(
			ownerKey, chatID, tabID, fieldString(params, "chat_id"), fieldString(params, "tab_id"), tailChars,
		)
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": items}, nil
	case "spawned_work.receipts":
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("spawned work receipts require the durable chat actor")
		}
		receipts, err := h.chats.providerChats.ListSpawnedWorkReceipts(
			ownerKey, chatID, tabID, fieldString(params, "chat_id"), fieldString(params, "tab_id"), intField(params, "limit"),
		)
		if err != nil {
			return nil, err
		}
		return map[string]any{"receipts": receipts}, nil
	case "external.register":
		var pid *int
		if value, ok := intFieldPresent(params, "pid"); ok {
			if value <= 1 {
				return nil, errors.New("external work pid must be greater than 1")
			}
			pid = &value
		}
		targetTab, targetChat := firstNonEmptyString(fieldString(params, "tab_id"), tabID), firstNonEmptyString(fieldString(params, "chat_id"), chatID)
		if targetTab != tabID || targetChat != chatID {
			return nil, errors.New("external work must remain in the exact owning Workass chat")
		}
		if err := h.authorizeActorOwner(ownerKey, targetTab, targetChat, "no running Workass turn owns this external work request"); err != nil {
			return nil, err
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundRegisterExternal, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Register: &chat.RegisterExternalAction{
				Label: fieldString(params, "label"), Role: fieldString(params, "role"), PID: pid,
				OutputFile: fieldString(params, "output_file"), DoneFile: fieldString(params, "done_file"),
			},
		})
	case "process.register":
		// `workass run` reporting the child it supervises: the registration
		// facts and the batch of events this daemon has not acknowledged. Not
		// an outbox mutation: a repeat with the same operation id must be
		// ACCEPTED and must apply whatever it carries, which is how the
		// supervisor's heartbeat, its backlog after an outage and its closing
		// outcome all arrive through one verb. Idempotency comes from the work
		// id being derived from the operation id (§6) and from the per-event
		// seq cursor, so a retry resolves to the row that already exists and
		// re-applies nothing.
		result, err := h.registerProcessChild(ownerKey, tabID, chatID, params)
		if err == nil && h.manager != nil {
			// The grant lasts as long as the job, because the job reports for
			// as long as it runs — this used to end with the FIRST report,
			// which was correct only while the spool was the real record. It
			// ends the moment the job is terminal: a token that leaks after
			// that authorizes nothing.
			if _, isRun := h.manager.RunTokenFor(ownerKey); isRun {
				if processReportIsTerminal(result) {
					h.manager.ConsumeRunToken(ownerKey)
				} else {
					// Still running, so the credential is still needed. The
					// touch is what keeps the idle ceiling from expiring a job
					// that has been reporting all day.
					h.manager.TouchRunToken(ownerKey)
				}
			}
		}
		return result, err
	case "process.start":
		// Addressed to this machine, where the caller already has a shell. The
		// daemon declines to be a job runner for a client standing next to it,
		// which is exactly the reason §9 gives for the non-goal.
		return nil, errors.New(processStartLocalReason)
	case "process.list":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, "no running Workass turn owns this process child request"); err != nil {
			return nil, err
		}
		return map[string]any{
			"ok": true, "children": h.manager.ListProcessChildren(),
			"resource_queues": h.manager.ResourceQueues(), "machine_id": h.selfMachineID,
		}, nil
	case "process.read":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, "no running Workass turn owns this process child request"); err != nil {
			return nil, err
		}
		tailChars := intField(params, "tail_chars")
		if tailChars < 0 || tailChars > 65536 {
			return nil, errors.New("process child tail_chars must be between 0 and 65536")
		}
		return h.manager.ReadProcessChild(fieldString(params, "work_id"), tailChars, uint64(intField(params, "after_seq")))
	case "process.cancel":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, "no running Workass turn owns this process child request"); err != nil {
			return nil, err
		}
		return h.manager.CancelProcessChild(fieldString(params, "work_id"), fieldString(params, "reason"))
	case "obligation.get":
		obligationTab := firstNonEmptyString(fieldString(params, "tab_id"), tabID)
		obligationChat := firstNonEmptyString(fieldString(params, "chat_id"), chatID)
		if obligationTab != tabID || obligationChat != chatID {
			return nil, errors.New("obligation must remain in the exact owning Workass chat")
		}
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, "no running Workass turn owns this obligation request"); err != nil {
			return nil, err
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("obligation read requires the durable chat actor")
		}
		obligation, err := h.chats.providerChats.Obligation(obligationTab, obligationChat)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "obligation": obligation}, nil
	case "external.settle":
		var exitCode *int
		if value, ok := intFieldPresent(params, "exit_code"); ok {
			exitCode = &value
		}
		targetTab, targetChat := firstNonEmptyString(fieldString(params, "tab_id"), tabID), firstNonEmptyString(fieldString(params, "chat_id"), chatID)
		if targetTab != tabID || targetChat != chatID {
			return nil, errors.New("external work must remain in the exact owning Workass chat")
		}
		if err := h.authorizeActorOwner(ownerKey, targetTab, targetChat, "no running Workass turn owns this external work request"); err != nil {
			return nil, err
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundSettleExternal, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Settle: &chat.SettleExternalAction{
				WorkID: fieldString(params, "work_id"), Status: fieldString(params, "status"),
				ExitCode: exitCode, Summary: fieldString(params, "summary"),
			},
		})
	case "artifact.host":
		return h.hostArtifact(r, ownerKey, tabID, chatID, params)
	case "html.render":
		return h.renderHTML(r, ownerKey, tabID, chatID, params)
	case "fork.create", "fork.list", "fork.run", "fork.wait", "fork.cancel", "fork.retry":
		return h.callFork(r, request.Method, params, ownerKey, tabID, chatID)
	case "agent.cancel":
		id := fieldString(params, "id")
		if id == "" {
			return nil, errors.New("subagent id is required")
		}
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
			return nil, err
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundCancelAgent, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Cancel: &chat.CancelAgentAction{WorkID: id},
		})
	case "agent.decide_permission":
		if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
			return nil, err
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundAgentPermission, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Permission: &chat.AgentPermissionAction{WorkID: fieldString(params, "subagent_id"), Decision: fieldString(params, "decision")},
		})
	default:
		return nil, errors.New("unknown agent control method")
	}
}

// forwardRemoteAgentCall hands one addressed call to the home that owns it —
// over this daemon's own link when there is one, and over the renderer relay
// when the peer has no mesh yet. The caller's bearer capability and parent ids
// never travel; the destination daemon authorizes the link and re-applies the
// exact actor fence, so remote validation is the same code as local validation.
//
// What is added here is the §6 envelope: the caller's own permission ceiling,
// which the home can only narrow, and an immutable origin stamp so provenance
// survives restarts and re-homing.
func (h *agentControlHandler) forwardRemoteAgentCall(ctx context.Context, method, machineID string, params map[string]any) (any, error) {
	if h == nil || h.remoteChats == nil {
		return nil, errors.New("Workass remote-chat MCP router is unavailable")
	}
	// No chaining: work that arrived here through a link below full access may
	// not use THIS daemon's links, so a narrowed grant cannot be laundered into
	// a wider one by passing through a third machine (§11).
	if err := fleetRefuseSecondHop(ctx, method); err != nil {
		return nil, err
	}
	forwarded := copyRemoteAgentRouteParams(params)
	forwarded["machine_id"] = machineID
	if err := stripDestinationMachineTags(forwarded, machineID); err != nil {
		return nil, err
	}
	if limit := h.callerFleetLimit(ctx, params); limit != "" {
		forwarded[fleetLimitParam] = limit
	}
	if origin := h.callerFleetOrigin(ctx, params); !origin.empty() {
		forwarded[fleetOriginParam] = origin.payload()
	}
	// A host-targeted spawn names no chat on the home: the owner is the calling
	// chat, so the pair is deliberately absent from the forwarded params.
	hostedSpawn := hostedSpawnIsForThisMachine(method, forwarded)
	result, err := h.remoteChats.Call(ctx, method, forwarded)
	if err != nil || method != "agent.spawn" {
		return result, err
	}
	// The watcher's half of the monitor (§7: durable at BOTH ends). The home
	// registered its side at spawn and returned the reference; recording it here
	// is what makes the outcome survive THIS daemon restarting, and applying the
	// slice it came with is what makes a child that finished while the reply
	// travelled arrive anyway.
	if h.chats != nil && h.chats.providerChats != nil {
		parentTabID, parentChatID := fieldString(params, "parent_tab_id"), fieldString(params, "parent_chat_id")
		laneID, parentOperationID := h.chats.providerChats.watcherOrigin(parentTabID, parentChatID)
		if h.fleetWatch.ready() {
			h.fleetWatch.WatchRemoteSubagent(ctx, machineID, result, parentTabID, parentChatID, laneID, parentOperationID)
		}
		// The row, in the chat that asked, for as long as the child runs. The
		// monitor above is what makes its OUTCOME survive a restart here; this
		// is what makes the work visible before it is over, which is the half
		// §6's separation of host from owner was missing.
		if hostedSpawn {
			h.recordHostedSubagent(machineID, parentTabID, parentChatID, laneID, parentOperationID, result)
		}
	}
	return result, nil
}

// fleetDestinationTaggedParams are the params that may arrive machine-tagged
// for the DESTINATION and must reach it as its own bare ids.
//
// They are the ids an agent is handed by a listing and then passes to a tool
// that acts on the machine they came from. The router has always stripped its
// own tags on the way out for the addressing pair; these are the same thing
// for ids that are not the address. `host_tab_id` / `host_chat_id` are the case
// that made this visible: `workass_list_chats` hands out tagged ids, so the
// tagged pair was the only pair an agent had, and `process.start` refused it
// with "a forwarded Workass tool call must carry this machine's own ids".
var fleetDestinationTaggedParams = []string{"host_tab_id", "host_chat_id", "work_id"}

// stripDestinationMachineTags reduces those ids to the destination's own local
// ids, and refuses one that names a third machine.
func stripDestinationMachineTags(params map[string]any, machineID string) error {
	machineID = strings.TrimSpace(machineID)
	for _, field := range fleetDestinationTaggedParams {
		value := strings.TrimSpace(fieldString(params, field))
		if value == "" {
			continue
		}
		home, local, tagged, err := splitMachineTaggedID(value)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		if !tagged {
			continue
		}
		if machineID != "" && home != machineID {
			// Not a tag to strip but a different machine named, which no
			// stripping could make true.
			return fmt.Errorf("%s names a chat on %s, but this call is addressed to %s; the chat that holds the local row must be on the machine that runs the work", field, home, machineID)
		}
		params[field] = local
	}
	return nil
}

// callerFleetLimit is the caller's own permission ceiling, in the read/edit/full
// vocabulary. A chat whose effective mode cannot be resolved states nothing
// rather than claiming full: unstated is how every peer behaved before the
// envelope existed, and the home resolves it the same way for both.
//
// Work that itself arrived over a link carries that link's effective limit
// onward unchanged, which is what makes narrowing transitive without a counter.
func (h *agentControlHandler) callerFleetLimit(ctx context.Context, params map[string]any) string {
	if work, forwarded := fleetForwardedWorkFrom(ctx); forwarded {
		return canonicalFleetLimit(work.Limit)
	}
	if h == nil || h.manager == nil {
		return ""
	}
	return h.manager.CallerPermissionIntent(
		fieldString(params, "owner_key"),
		fieldString(params, "parent_chat_id"),
		fieldString(params, "parent_tab_id"),
	)
}

// callerFleetOrigin stamps who asked. An origin that arrived with the command
// is immutable and travels on unchanged; otherwise this machine and this chat
// are the origin.
func (h *agentControlHandler) callerFleetOrigin(ctx context.Context, params map[string]any) fleetCommandOrigin {
	if work, forwarded := fleetForwardedWorkFrom(ctx); forwarded && !work.Origin.empty() {
		return work.Origin
	}
	return fleetCommandOrigin{
		Machine:     strings.TrimSpace(h.selfMachineID),
		ChatID:      fieldString(params, "parent_chat_id"),
		TabID:       fieldString(params, "parent_tab_id"),
		OperationID: fieldString(params, "operation_id"),
	}
}

// agentOwnerErrorFor keeps each method family's existing authorization wording
// when the call is about to be forwarded instead of executed here.
func agentOwnerErrorFor(method string) string {
	switch {
	case strings.HasPrefix(method, "chat."), strings.HasPrefix(method, "schedule."):
		return "Workass chat control caller is not an owned ACP session"
	case strings.HasPrefix(method, "fork."):
		return "Workass fork caller is not an owned ACP session"
	default:
		return agentOwnerReadError
	}
}

func (h *agentControlHandler) runBackgroundAction(r *http.Request, tabID, chatID string, action chat.BackgroundAction) (any, error) {
	if h == nil || h.chats == nil || h.chats.providerChats == nil {
		return nil, errors.New("background mutation requires the durable chat actor")
	}
	return h.chats.providerChats.RunBackgroundAction(r.Context(), tabID, chatID, action)
}

func stringSliceField(raw any) []string {
	values, _ := raw.([]any)
	if values == nil {
		if stringsList, ok := raw.([]string); ok {
			return append([]string(nil), stringsList...)
		}
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text := strings.TrimSpace(toString(value)); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func localRemoteAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

// registerProcessChild records one supervised child and gives its owner a
// monitor on it.
//
// The monitor is registered HERE, at registration time, for the same reason a
// remote subagent's is: a job that finishes in the same second its reply is
// still travelling has somewhere to report, because the monitor already exists
// and the log already holds the event. An owner on another machine is offered
// the monitor over the mesh; an owner on this machine gets a self-homed one,
// which the home delivers in process — the same projection either way.
// reportedProcessEvents reads one report's batch off the wire.
//
// Validation of the SEQUENCE belongs to the manager, which owns the cursor;
// this only turns the JSON into events and drops nothing silently — an entry
// with no seq survives as a zero seq, which the manager refuses by name.
func reportedProcessEvents(value any) []procchild.Event {
	raw := anySlice(value)
	if len(raw) == 0 {
		return nil
	}
	events := make([]procchild.Event, 0, len(raw))
	for _, entry := range raw {
		fields := mapFromAnyMain(entry)
		if fields == nil {
			continue
		}
		event := procchild.Event{
			Seq:     uint64(intField(fields, "seq")),
			Kind:    strings.TrimSpace(fieldString(fields, "kind")),
			Payload: mapFromAnyMain(fields["payload"]),
		}
		if stamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(fieldString(fields, "at"))); err == nil {
			event.At = stamp.UTC()
		} else {
			event.At = time.Now().UTC()
		}
		events = append(events, event)
	}
	return events
}

// processReportIsTerminal reads the row's own status out of the report's answer.
func processReportIsTerminal(result any) bool {
	status := strings.TrimSpace(fieldString(mapFromAnyMain(result), "status"))
	return status != "" && status != "running"
}

func (h *agentControlHandler) registerProcessChild(ownerKey, tabID, chatID string, params map[string]any) (any, error) {
	if h == nil || h.manager == nil || h.chats == nil || h.chats.providerChats == nil {
		return nil, errors.New("process children require the durable chat actor")
	}
	targetTab := firstNonEmptyString(fieldString(params, "tab_id"), tabID)
	targetChat := firstNonEmptyString(fieldString(params, "chat_id"), chatID)
	if targetTab != tabID || targetChat != chatID {
		return nil, errors.New("a process child is registered by the supervisor on this machine, in the exact calling chat")
	}
	if err := h.authorizeActorOwner(ownerKey, tabID, chatID, "no running Workass turn owns this process child registration"); err != nil {
		return nil, err
	}
	laneID, operationID, turnID, err := h.chats.providerChats.backgroundOriginStamp(tabID, chatID)
	if err != nil {
		return nil, err
	}
	var supervisor *int
	if value, ok := intFieldPresent(params, "pid"); ok {
		supervisor = &value
	}
	owner, err := resolveProcessOwner(
		fieldString(params, "owner"), h.selfMachineID, chatID, tabID, laneID, operationID,
	)
	if err != nil {
		return nil, err
	}
	result, err := h.manager.RegisterProcessChild(acp.ProcessChildRegistrationOptions{
		OwnerKey: ownerKey, ParentChatID: chatID, ParentTabID: tabID, TabID: tabID, ChatID: chatID,
		WorkID: fieldString(params, "work_id"), Label: fieldString(params, "label"),
		Role: fieldString(params, "role"), Command: fieldString(params, "command"),
		Owner: fieldString(params, "owner"), Resource: fieldString(params, "resource"),
		OutputFile: fieldString(params, "output_file"), DoneFile: fieldString(params, "done_file"),
		Isolation: fieldString(params, "isolation"), ProcessGroup: intField(params, "process_group"),
		SupervisorPID: supervisor,
		MaxRuntimeMs:  int64(intField(params, "max_runtime_ms")),
		StaleAfterMs:  int64(intField(params, "stale_after_ms")),
		MaxHoldMs:     int64(intField(params, "max_hold_ms")),
		HeartbeatMs:   int64(intField(params, "heartbeat_ms")),
		ReportGapMs:   int64(intField(params, "report_gap_ms")),
		Events:        reportedProcessEvents(params["events"]),
		OriginLaneID:  laneID, OriginOperationID: operationID, OriginTurnID: turnID,
	})
	if err != nil {
		return nil, err
	}
	workID := fieldString(result, "workId")
	if h.fleetHome != nil && workID != "" {
		if row, ok := h.fleetHome.WatchProcess(
			context.Background(), h.fleetMesh, owner, workID, fieldString(params, "label"),
		); ok {
			result["monitor"] = map[string]any{"ref": row.Ref, "targetId": row.TargetID}
		}
	}
	return result, nil
}
