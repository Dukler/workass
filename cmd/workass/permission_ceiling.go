package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"workass/internal/acp"
	providercontract "workass/internal/provider"
)

// The permission ceiling (PORT-SPEC §3, landmine 9).
//
// The chat tools change permissions, and they must: a parent agent has no other
// way to change what a child may do. What they must not do is hand out more than
// the caller holds. An `ask` chat could create a `bypass` chat, queue a message
// into it and set itself to `bypass`, so every narrower mode was one tool call
// away from full access.
//
// The caller's level is its ceiling. Up to it, nothing asks. Past it, the human
// answers an ordinary question card, because the daemon opens that card and only
// a person answers it: an agent cannot forge the receipt, where a refusal that
// said "ask the user, then retry" would have trusted the agent's word for it.
//
// A card needs a running turn in the calling chat to hang off. A command forwarded
// from another machine has none here, and a delegated agent cannot open a human
// question at all, so those two get a refusal that says what to do instead. The
// human's own actions never come through here: they reach the actor over the
// renderer's channels and are not capped by anything in this file.

const (
	permissionRaiseQuestionID = "permission_raise"
	permissionRaiseAllow      = "allow"
	permissionRaiseDeny       = "deny"
	// The leading word of every refusal, which an agent can branch on.
	permissionCeilingCode = "permission_ceiling"
	permissionDeniedCode  = "permission_denied"
)

type permissionCallerKind int

const (
	permissionCallerChat permissionCallerKind = iota
	permissionCallerForwarded
	permissionCallerSubagent
)

// permissionCaller is who is asking, and at what level.
type permissionCaller struct {
	kind       permissionCallerKind
	ownerKey   string
	tabID      string
	chatID     string
	providerID string
	modeID     string
	level      string
}

// rank is the caller's ceiling. A caller whose level cannot be read holds the
// lowest one rather than none, so unknown can never be the way past the gate.
func (c permissionCaller) rank() int {
	if rank := acp.PermissionLevelRank(c.level); rank > 0 {
		return rank
	}
	return acp.PermissionLevelRank(fleetLimitRead)
}

func (c permissionCaller) full() bool {
	return c.rank() == acp.PermissionLevelRank(fleetLimitFull)
}

// statedLevel is the level as a word, for an envelope or a sentence.
func (c permissionCaller) statedLevel() string {
	if c.level != "" {
		return c.level
	}
	return fleetLimitRead
}

// permissionChange is one thing that would run at a level.
type permissionChange struct {
	method      string
	operationID string
	// summary finishes "It wants to ...". The human reads it on the card.
	summary string
	// target names the chat or work in refusals an agent reads.
	target     string
	tabID      string
	chatID     string
	providerID string
	modeID     string
	// configure only: what the chat runs at now, because lowering is always
	// allowed even from below the level the chat is lowered to.
	reconfigures      bool
	currentProviderID string
	currentModeID     string
	// contentDigest binds an approval to what it was given for.
	contentDigest string
}

// permissionChangeRaises decides whether a change is above the caller's level.
func permissionChangeRaises(caller permissionCaller, change permissionChange) bool {
	if caller.full() {
		return false
	}
	// A provider with no modes (workass-agent over LM Studio or Ollama) names none,
	// so there is no level to raise.
	if strings.TrimSpace(change.modeID) == "" {
		return false
	}
	target := acp.PermissionLevelRank(acp.PermissionLevel(change.providerID, change.modeID))
	if change.reconfigures {
		if samePermissionMode(change.providerID, change.modeID, change.currentProviderID, change.currentModeID) {
			return false
		}
		if current := acp.PermissionLevelRank(acp.PermissionLevel(change.currentProviderID, change.currentModeID)); current > 0 && target > 0 && target <= current {
			return false
		}
	}
	if caller.kind != permissionCallerForwarded && samePermissionMode(change.providerID, change.modeID, caller.providerID, caller.modeID) {
		return false
	}
	// A mode no vocabulary knows could allow anything, so it is never below a
	// ceiling, and only full access reaches it without asking.
	return target == 0 || target > caller.rank()
}

func samePermissionMode(providerA, modeA, providerB, modeB string) bool {
	return strings.TrimSpace(modeA) == strings.TrimSpace(modeB) && samePermissionProvider(providerA, providerB)
}

func samePermissionProvider(providerA, providerB string) bool {
	return providercontract.NormalizeID(providerA) == providercontract.NormalizeID(providerB)
}

// permissionLevelWords says a level the way a person would.
func permissionLevelWords(level string) string {
	switch level {
	case fleetLimitRead:
		return "read only, asking before anything else"
	case fleetLimitEdit:
		return "can edit"
	case fleetLimitFull:
		return "full access"
	default:
		return "an unrecognized permission level"
	}
}

func permissionModeWords(providerID, modeID string) string {
	mode := strings.TrimSpace(modeID)
	if mode == "" {
		mode = "the provider's default mode"
	}
	return mode + " (" + permissionLevelWords(acp.PermissionLevel(providerID, modeID)) + ")"
}

type permissionChildKey struct{}

// withPermissionSubagent marks a request as a delegated agent's. Its tools run
// under its parent's chat, so the chat's identity says nothing about who asked.
func withPermissionSubagent(ctx context.Context, subagentID string) context.Context {
	return context.WithValue(ctx, permissionChildKey{}, subagentID)
}

// permissionCallerFor reads the ceiling from where the request says it came from.
func (h *agentControlHandler) permissionCallerFor(ctx context.Context, ownerKey, tabID, chatID string) permissionCaller {
	caller := permissionCaller{ownerKey: ownerKey, tabID: tabID, chatID: chatID}
	if subagentID, ok := ctx.Value(permissionChildKey{}).(string); ok && subagentID != "" {
		caller.kind = permissionCallerSubagent
		if h != nil && h.manager != nil {
			caller.providerID, caller.modeID, _ = h.manager.SubagentPermissionMode(subagentID)
		}
		caller.level = acp.PermissionLevel(caller.providerID, caller.modeID)
		return caller
	}
	if work, forwarded := fleetForwardedWorkFrom(ctx); forwarded {
		caller.kind = permissionCallerForwarded
		caller.level = canonicalFleetLimit(work.Limit)
		return caller
	}
	if h != nil && h.chats != nil {
		caller.providerID, caller.modeID = h.chats.providerChats.chatPermissionMode(tabID, chatID)
	}
	caller.level = acp.PermissionLevel(caller.providerID, caller.modeID)
	return caller
}

// chatPermissionMode is what a chat runs at now: what the owner sees, and what
// only the owner or an approved change can set. Live state answers only for a
// chat with nothing durable, because a mode the owner just changed reaches a
// running session later than it reaches the chat.
func (r *providerChatRuntime) chatPermissionMode(tabID, chatID string) (string, string) {
	if r == nil {
		return "", ""
	}
	state, ok := r.Snapshot(strings.TrimSpace(chatID))
	if !ok {
		return "", ""
	}
	providerID, modeID := string(state.Presentation.ProviderID), strings.TrimSpace(state.Presentation.CurrentModeID)
	if modeID == "" && r.manager != nil {
		liveProvider, liveMode := r.manager.ChatPermissionMode(chatID, tabID)
		modeID = liveMode
		providerID = firstNonEmptyString(providerID, liveProvider)
	}
	return providerID, modeID
}

// admitPermission lets a change through, asks the human, or says why it cannot.
func (h *agentControlHandler) admitPermission(ctx context.Context, caller permissionCaller, change permissionChange) error {
	if !permissionChangeRaises(caller, change) {
		return nil
	}
	switch caller.kind {
	case permissionCallerForwarded:
		return fmt.Errorf("%s: this command came from another machine with a %s limit, and %s would run at %s, which is above it. This machine cannot show the user a card for it. Raise the calling chat first: call workass_configure_chat on your own chat with permission_intent set to what you need, which asks the user on your machine. Then retry this call",
			permissionCeilingCode, caller.statedLevel(), change.target, permissionModeWords(change.providerID, change.modeID))
	case permissionCallerSubagent:
		return fmt.Errorf("%s: this subagent runs at %s, and %s would run at %s, which is above it. A subagent cannot ask the user. Its parent agent can do this itself if its own level allows, or ask the user",
			permissionCeilingCode, caller.statedLevel(), change.target, permissionModeWords(change.providerID, change.modeID))
	}
	allowed, status, err := h.askPermissionRaise(ctx, caller, change)
	if err != nil {
		if acp.IsNoRunningQuestionCaller(err) {
			return fmt.Errorf("%s: this chat is at %s, and %s would run at %s, which is above it. No turn is running here to show the user a card in. Ask the user: \"Please allow %s to run at %s, or set it yourself in the chat's permission menu.\"",
				permissionCeilingCode, caller.statedLevel(), change.target, permissionModeWords(change.providerID, change.modeID), change.target, permissionModeWords(change.providerID, change.modeID))
		}
		return err
	}
	if allowed {
		return nil
	}
	return fmt.Errorf("%s: the user was asked to let this chat run %s at %s and the card ended as %s. Nothing changed. A retry with this operation_id reads the same answer; a new operation_id asks again",
		permissionDeniedCode, change.target, permissionModeWords(change.providerID, change.modeID), status)
}

// askPermissionRaise opens the existing question card and waits, exactly as
// workass_ask_user_question does: no deadline of ours, settled by a Stop, and
// durable by operation id, so a retry reads the answer and never opens a second
// card. Its digest is its own domain, so nothing an agent can ask through the
// question tool could ever stand in for this receipt.
func (h *agentControlHandler) askPermissionRaise(ctx context.Context, caller permissionCaller, change permissionChange) (bool, string, error) {
	operationID, err := providercontract.ValidateOperationID(change.operationID)
	if err != nil {
		return false, "", err
	}
	text := fmt.Sprintf("This chat is at %s. It wants to %s, which would run at %s. Allow it?",
		permissionLevelWords(caller.statedLevel()), change.summary, permissionModeWords(change.providerID, change.modeID))
	if runes := []rune(text); len(runes) > 400 {
		text = string(runes[:399]) + "…"
	}
	call, err := parseWorkassQuestion(map[string]any{
		"operation_id": string(derivedOperationID("permission-raise:", operationID, "")),
		"question_id":  permissionRaiseQuestionID,
		"header":       "Permission",
		"question":     text,
		"options": []any{
			map[string]any{"id": permissionRaiseAllow, "label": "Allow"},
			map[string]any{"id": permissionRaiseDeny, "label": "Deny"},
		},
	})
	if err != nil {
		return false, "", err
	}
	if call.Digest, err = permissionRaiseDigest(change); err != nil {
		return false, "", err
	}
	value, err := h.chats.providerChats.executeAgentQuestion(ctx, h.manager, caller.ownerKey, caller.tabID, caller.chatID, call)
	if err != nil {
		return false, "", err
	}
	result, ok := value.(workassQuestionResult)
	if !ok {
		return false, "", errors.New("the permission card returned no answer")
	}
	if result.Status != "answered" {
		return false, result.Status, nil
	}
	for _, selected := range result.SelectedOptions {
		if selected.ID == permissionRaiseAllow {
			return true, result.Status, nil
		}
	}
	return false, "denied", nil
}

func permissionRaiseDigest(change permissionChange) (string, error) {
	raw, err := json.Marshal(map[string]string{
		"method": change.method, "tabId": change.tabID, "chatId": change.chatID,
		"providerId": change.providerID, "modeId": change.modeID, "content": change.contentDigest,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("workass-permission-raise-v1\x00"), raw...))
	return fmt.Sprintf("%x", sum), nil
}

func permissionContentDigest(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}

func permissionMessageWords(message string) string {
	if excerpt := acp.SanitizeDisplayText(acp.RedactSensitiveText(message), 120); excerpt != "" {
		return ": “" + excerpt + "”"
	}
	return ""
}

func permissionChatWords(title, fallback string) string {
	if name := acp.SanitizeDisplayText(title, 80); name != "" {
		return "“" + name + "”"
	}
	return fallback
}

// A gate rides the context into the chat coordinator, because only after catalog
// resolution does anyone know what a create or a configure would run at.
type permissionGate struct {
	handler *agentControlHandler
	caller  permissionCaller
}

type permissionGateKey struct{}

func (h *agentControlHandler) withPermissionGate(ctx context.Context, ownerKey, tabID, chatID string) context.Context {
	return context.WithValue(ctx, permissionGateKey{}, &permissionGate{
		handler: h, caller: h.permissionCallerFor(ctx, ownerKey, tabID, chatID),
	})
}

func permissionGateFrom(ctx context.Context) *permissionGate {
	gate, _ := ctx.Value(permissionGateKey{}).(*permissionGate)
	return gate
}

// admit is a no-op with no gate, which is the human's path and every in-process
// caller that is not an agent tool.
func (g *permissionGate) admit(ctx context.Context, change permissionChange) error {
	if g == nil {
		return nil
	}
	return g.handler.admitPermission(ctx, g.caller, change)
}

// resolveControlsWithinCeiling resolves a selection and, when the caller named no
// mode and the inherited or default one is above its ceiling, starts at the
// ceiling instead. Asking the human about a level nobody asked for is friction,
// and refusing it would be a dead end. A selection the caller did name is left
// exactly as named, so the gate can answer for it.
func (c *chatControlCoordinator) resolveControlsWithinCeiling(ctx context.Context, current, params map[string]any) (resolvedChatControls, error) {
	controls, err := c.resolveControls(ctx, current, params)
	gate := permissionGateFrom(ctx)
	if err != nil || gate == nil || gate.caller.full() || fieldString(params, "mode_id") != "" || fieldString(params, "permission_intent") != "" {
		return controls, err
	}
	if !permissionChangeRaises(gate.caller, permissionChange{providerID: controls.ProviderID, modeID: controls.ModeID}) {
		return controls, nil
	}
	// The caller's own mode where it means anything here, then the same level in
	// this provider's words: not every provider has a mode for every intent.
	attempts := []map[string]any{{"permission_intent": gate.caller.statedLevel()}}
	if gate.caller.modeID != "" && samePermissionProvider(gate.caller.providerID, controls.ProviderID) {
		attempts = append([]map[string]any{{"mode_id": gate.caller.modeID}}, attempts...)
	}
	for _, attempt := range attempts {
		narrowed := copyAnyMap(params)
		for key, value := range attempt {
			narrowed[key] = value
		}
		within, narrowErr := c.resolveControls(ctx, current, narrowed)
		if narrowErr == nil && !permissionChangeRaises(gate.caller, permissionChange{providerID: within.ProviderID, modeID: within.ModeID}) {
			return within, nil
		}
	}
	return controls, nil
}

// admitSend gates chat.send, the laundering path: a message queued into a wider
// chat is that chat's permissions used by a caller who does not hold them.
func (h *agentControlHandler) admitSend(ctx context.Context, ownerKey, callerTab, callerChat string, params map[string]any) error {
	caller := h.permissionCallerFor(ctx, ownerKey, callerTab, callerChat)
	if caller.full() {
		return nil
	}
	tabID, chatID := fieldString(params, "tab_id"), fieldString(params, "chat_id")
	// Snapshot sees only open actors, and an idle chat's actor closes while the
	// send below reopens it, so the target is opened here first.
	if _, err := h.chats.providerChats.exactActorFence(tabID, chatID); err != nil {
		// The send itself refuses a chat that does not exist, in its own words.
		return nil
	}
	state, ok := h.chats.providerChats.Snapshot(chatID)
	if !ok || strings.TrimSpace(state.Presentation.TabID) != tabID {
		return nil
	}
	providerID, modeID := h.chats.providerChats.chatPermissionMode(tabID, chatID)
	message := fieldString(params, "message")
	return h.admitPermission(ctx, caller, permissionChange{
		method: "chat.send", operationID: fieldString(params, "operation_id"),
		summary: "send a message to " + permissionChatWords(state.Presentation.Title, "another chat") + permissionMessageWords(message),
		target:  "a message to that chat", tabID: tabID, chatID: chatID,
		providerID: providerID, modeID: modeID, contentDigest: permissionContentDigest(message),
	})
}

// admitRemoteSend is the same gate at the sender. A remote send has a route with
// no envelope (the renderer's own), so its home may never hear the limit; the
// chat list, which every route answers, is what tells the sender how wide the
// target is. It returns the level the human approved, when one did. Anything it
// cannot read goes on to the home, which holds the same law wherever it can.
func (h *agentControlHandler) admitRemoteSend(ctx context.Context, params map[string]any) (string, error) {
	caller := h.permissionCallerFor(ctx, fieldString(params, "owner_key"), fieldString(params, "parent_tab_id"), fieldString(params, "parent_chat_id"))
	if caller.full() || caller.kind == permissionCallerForwarded {
		return "", nil
	}
	listed, err := h.remoteChats.Call(ctx, "chat.list", map[string]any{})
	if err != nil {
		return "", nil
	}
	row := matchingRemoteChatRow(listed, fieldString(params, "tab_id"), fieldString(params, "chat_id"))
	if row == nil || fieldString(row, "modeId") == "" {
		return "", nil
	}
	providerID, modeID, message := fieldString(row, "providerId"), fieldString(row, "modeId"), fieldString(params, "message")
	change := permissionChange{
		method: "chat.send", operationID: fieldString(params, "operation_id"),
		summary: "send a message to " + permissionChatWords(fieldString(row, "title"), "a chat on another machine") + permissionMessageWords(message),
		target:  "a message to that chat", tabID: fieldString(params, "tab_id"), chatID: fieldString(params, "chat_id"),
		providerID: providerID, modeID: modeID, contentDigest: permissionContentDigest(message),
	}
	if !permissionChangeRaises(caller, change) {
		return "", nil
	}
	if err := h.admitPermission(ctx, caller, change); err != nil {
		return "", err
	}
	if level := acp.PermissionLevel(providerID, modeID); level != "" {
		return level, nil
	}
	return fleetLimitFull, nil
}

func matchingRemoteChatRow(listed any, tabID, chatID string) map[string]any {
	tabMachine, tabLocal, _, tabErr := splitMachineTaggedID(tabID)
	chatMachine, chatLocal, _, chatErr := splitMachineTaggedID(chatID)
	if tabErr != nil || chatErr != nil {
		return nil
	}
	for _, raw := range anySlice(mapFromAnyMain(listed)["chats"]) {
		row := mapFromAnyMain(raw)
		rowTabMachine, rowTab, _, rowTabErr := splitMachineTaggedID(fieldString(row, "tabId"))
		rowChatMachine, rowChat, _, rowChatErr := splitMachineTaggedID(fieldString(row, "chatId"))
		if rowTabErr == nil && rowChatErr == nil && rowTabMachine == tabMachine && rowTab == tabLocal &&
			rowChatMachine == chatMachine && rowChat == chatLocal {
			return row
		}
	}
	return nil
}

// admitSpawn gates agent.spawn. The selection is resolved once here, read-only,
// to learn the level; a selection that does not resolve is left for the spawn to
// report in its own words. It returns the params the spawn should run with.
func (h *agentControlHandler) admitSpawn(ctx context.Context, ownerKey, tabID, chatID string, params map[string]any) (map[string]any, error) {
	caller := h.permissionCallerFor(ctx, ownerKey, tabID, chatID)
	if caller.full() {
		return params, nil
	}
	resolve := func(p map[string]any) (string, string, error) {
		return h.manager.ResolveSubagentPermission(ctx, acp.SubagentSpawnOptions{
			OwnerKey: ownerKey, ParentChatID: chatID, ParentTabID: tabID,
			ProviderID: fieldString(p, "provider_id"), ModelID: fieldString(p, "model_id"),
			Effort: fieldString(p, "effort"), ModeID: fieldString(p, "mode_id"),
			PermissionIntent: fieldString(p, "permission_intent"),
		})
	}
	providerID, modeID, err := resolve(params)
	if err != nil {
		return params, nil
	}
	change := permissionChange{
		method: "agent.spawn", operationID: fieldString(params, "operation_id"),
		summary: "start a subagent" + permissionMessageWords(fieldString(params, "prompt")),
		target:  "that subagent", tabID: tabID, chatID: chatID, providerID: providerID, modeID: modeID,
		contentDigest: permissionContentDigest(fieldString(params, "prompt")),
	}
	if !permissionChangeRaises(caller, change) {
		return params, nil
	}
	// Named by nobody, so it starts at the ceiling, as a new chat does.
	intent := strings.ToLower(fieldString(params, "permission_intent"))
	if fieldString(params, "mode_id") == "" && (intent == "" || intent == "inherit") {
		narrowed := copyAnyMap(params)
		narrowed["permission_intent"] = caller.statedLevel()
		if narrowedProvider, narrowedMode, err := resolve(narrowed); err == nil {
			within := change
			within.providerID, within.modeID = narrowedProvider, narrowedMode
			if !permissionChangeRaises(caller, within) {
				return narrowed, nil
			}
		}
	}
	return params, h.admitPermission(ctx, caller, change)
}

// admitSubagentWork gates more work for an existing child. A parent can be below
// its child: the owner may have lowered the parent after the child started.
func (h *agentControlHandler) admitSubagentWork(ctx context.Context, method, ownerKey, tabID, chatID string, params map[string]any) error {
	caller := h.permissionCallerFor(ctx, ownerKey, tabID, chatID)
	if caller.full() {
		return nil
	}
	id := fieldString(params, "id")
	parentChat, parentTab, owned := h.manager.SubagentParent(id)
	if !owned || parentChat != chatID || parentTab != tabID {
		return nil
	}
	providerID, modeID, known := h.manager.SubagentPermissionMode(id)
	if !known {
		return nil
	}
	message := fieldString(params, "message")
	return h.admitPermission(ctx, caller, permissionChange{
		method: method, operationID: fieldString(params, "operation_id"),
		summary: "give its subagent more work" + permissionMessageWords(message),
		target:  "that subagent", tabID: tabID, chatID: chatID, providerID: providerID, modeID: modeID,
		contentDigest: permissionContentDigest(id + "\x00" + message),
	})
}

// refuseGrantBelowFull holds workass_decide_subagent_permission to its rule for
// the two callers the manager's own check cannot see: a forwarded command, which
// acts through a chat whose level is not the caller's, and a chat the owner has
// lowered while its session still runs at the old mode. Denying is always allowed.
func (h *agentControlHandler) refuseGrantBelowFull(ctx context.Context, ownerKey, tabID, chatID, decision string) error {
	if !strings.EqualFold(strings.TrimSpace(decision), "allow") {
		return nil
	}
	caller := h.permissionCallerFor(ctx, ownerKey, tabID, chatID)
	if caller.full() {
		return nil
	}
	return fmt.Errorf("%s: only a caller at full access can allow a subagent's request, and this one is at %s; the card is the user's to answer. Denying it is still allowed",
		permissionCeilingCode, caller.statedLevel())
}
