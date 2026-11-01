package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// The owner may program an update ("after you are done with this update") and walk
// away, so no click can be required. What cannot be allowed is what the
// expected-versions sentence let through: every word of it is public, so an
// injected agent could write it. Authorization is therefore one real message of
// the owner's, in the calling chat, whose origin the daemon stamped when it was
// admitted (AGENTS.md, "Applying a Workass update needs the owner's permission").
//
// The check runs here, on the machine that holds the chat, for this machine and
// for any other: another machine's daemon has no way to read this chat, and its
// wire channel stays the authenticated controller surface it always was.

// updatePermissionMaxAge bounds how old the owner's message may be when the update
// is applied. The scheduler can hold a message for at most 7 days, and one more day
// keeps the longest programmed update working when it is scheduled the same day.
const updatePermissionMaxAge = 8 * 24 * time.Hour

// updatePermissionFutureSkew tolerates a controller whose clock runs ahead.
const updatePermissionFutureSkew = 24 * time.Hour

const updateAuthorizationsFile = "update-authorizations.json"

const updatePermissionHint = "Pass authorizing_message_id: the id of the owner's own message in this chat allowing this update. workass_list_chats marks this chat callingChat, and workass_read_chat on it shows the owner's messages with origin \"human\". If there is none, ask the owner."

// markCallingChat says which listed chat is the caller's, which an agent otherwise
// cannot tell without guessing from a title.
func markCallingChat(list map[string]any, tabID, chatID string) {
	for _, raw := range anySlice(list["chats"]) {
		if item, ok := raw.(map[string]any); ok && fieldString(item, "tabId") == tabID && fieldString(item, "chatId") == chatID {
			item["callingChat"] = true
		}
	}
}

// updateMention is deliberately loose about which update: it is the check that the
// owner was talking about updating at all, in the two languages they use.
var updateMention = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(?:updat\p{L}*|upgrad\p{L}*|actuali[zc]\p{L}*)`)

func mentionsAnUpdate(text string) bool { return updateMention.MatchString(text) }

// verifyOwnerUpdateMessage is the whole test of one message. It reads only what the
// chat recorded, and every refusal says what to do next.
func verifyOwnerUpdateMessage(state chat.State, messageID string, now time.Time) (chat.UserMessageRecord, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return chat.UserMessageRecord{}, errors.New("this update has no owner permission. " + updatePermissionHint)
	}
	record, found := state.UserMessageRecord(messageID)
	if !found {
		return record, fmt.Errorf("message %q is not in this chat. %s", messageID, updatePermissionHint)
	}
	switch {
	case record.Copied:
		return record, fmt.Errorf("message %q was copied into this chat by a fork, so it cannot authorize an update here. %s", messageID, updatePermissionHint)
	case record.Notice:
		return record, fmt.Errorf("message %q is a Workass notice, not the owner's words. %s", messageID, updatePermissionHint)
	case strings.TrimSpace(record.Origin) == "":
		return record, fmt.Errorf("Workass has no record of who wrote message %q (it predates origins being recorded, or this provider does not report them), so it cannot authorize an update. Ask the owner to send a new message in this chat allowing the update, and pass that message's id", messageID)
	case record.Origin != "human":
		return record, fmt.Errorf("message %q was written by %s, not by the owner. %s", messageID, record.Origin, updatePermissionHint)
	case record.Withdrawn:
		return record, fmt.Errorf("the owner stopped message %q before it was delivered. %s", messageID, updatePermissionHint)
	case !mentionsAnUpdate(record.Text):
		return record, fmt.Errorf("the owner's message %q does not mention an update. %s", messageID, updatePermissionHint)
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.At))
	if err != nil {
		return record, fmt.Errorf("message %q has no usable time, so its age cannot be checked. Ask the owner to send a new message in this chat allowing the update, and pass that message's id", messageID)
	}
	if age := now.Sub(at); age > updatePermissionMaxAge || age < -updatePermissionFutureSkew {
		return record, fmt.Errorf("the owner's message %q is too old to authorize an update: the limit is %d days. Ask the owner to allow it again in this chat, and pass that message's id", messageID, int(updatePermissionMaxAge/(24*time.Hour)))
	}
	return record, nil
}

// updateAuthorizationClaim records that one message authorized one activation. It
// is written before the updater is asked, so a lost reply cannot start a second.
type updateAuthorizationClaim struct {
	MessageID      string `json:"messageId"`
	ChatID         string `json:"chatId"`
	MachineID      string `json:"machineId"`
	CurrentVersion string `json:"currentVersion"`
	TargetVersion  string `json:"targetVersion"`
	OperationID    string `json:"operationId"`
	AcceptedAt     string `json:"acceptedAt"`
	// Started: the updater accepted this operation, so from here on its id only
	// ever reads a receipt and the message's age no longer matters.
	Started bool `json:"started,omitempty"`
}

type updateAuthorizationFile struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Claims        []updateAuthorizationClaim `json:"claims"`
}

// updateAuthorizationLock serializes the one file below. Update calls are rare.
var updateAuthorizationLock sync.Mutex

func (h *agentControlHandler) updateAuthorizationPath() (string, error) {
	dir := strings.TrimSpace(h.stateDir)
	if dir == "" && h.manager != nil {
		dir = strings.TrimSpace(h.manager.StateDir())
	}
	if dir == "" {
		return "", errors.New("this Workass daemon has no state directory, so it cannot record an update permission")
	}
	return filepath.Join(dir, updateAuthorizationsFile), nil
}

// A store that cannot be read refuses rather than reads as empty: empty would let a
// message that has already been used authorize a second activation.
func readUpdateAuthorizations(path string) ([]updateAuthorizationClaim, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the update permission record cannot be read: %v", err)
	}
	var stored updateAuthorizationFile
	if err := json.Unmarshal(raw, &stored); err != nil || stored.SchemaVersion != 1 {
		return nil, fmt.Errorf("the update permission record at %s is damaged, so no update can be authorized until it is removed", path)
	}
	return stored.Claims, nil
}

func writeUpdateAuthorizations(path string, claims []updateAuthorizationClaim) error {
	encoded, err := json.MarshalIndent(updateAuthorizationFile{SchemaVersion: 1, Claims: claims}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	_, writeErr := temp.Write(append(encoded, '\n'))
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// pruneUpdateAuthorizations keeps a claim for as long as its message could still
// pass the age check, and then some: past that the message refuses by itself.
func pruneUpdateAuthorizations(claims []updateAuthorizationClaim, now time.Time) []updateAuthorizationClaim {
	kept := claims[:0:0]
	for _, claim := range claims {
		at, err := time.Parse(time.RFC3339Nano, claim.AcceptedAt)
		if err != nil || now.Sub(at) <= 4*updatePermissionMaxAge {
			kept = append(kept, claim)
		}
	}
	return kept
}

func (h *agentControlHandler) updatePermissionTime() time.Time {
	if h.updatePermissionNow != nil {
		return h.updatePermissionNow()
	}
	return time.Now()
}

func (h *agentControlHandler) readChatForUpdatePermission(tabID, chatID string) (chat.State, error) {
	if h.chats == nil || h.chats.providerChats == nil {
		return chat.State{}, errors.New("this Workass daemon cannot read its chats, so it cannot check an update permission")
	}
	actor, _, err := h.chats.providerChats.exactActor(tabID, chatID)
	if err != nil {
		return chat.State{}, err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	return actor.engine.ReadSnapshot(), nil
}

// claimUpdatePermission decides whether this call may reach the updater. A replay
// of an operation that already started only reads its receipt, whatever has
// happened to its message since. Anything else needs a message that passes today,
// and that has not authorized a different operation for this machine and version.
func (h *agentControlHandler) claimUpdatePermission(tabID, chatID string, claim updateAuthorizationClaim) (replay bool, err error) {
	path, err := h.updateAuthorizationPath()
	if err != nil {
		return false, err
	}
	updateAuthorizationLock.Lock()
	defer updateAuthorizationLock.Unlock()
	claims, err := readUpdateAuthorizations(path)
	if err != nil {
		return false, err
	}
	known := false
	for _, existing := range claims {
		if existing.OperationID == claim.OperationID {
			if existing.MessageID != claim.MessageID || existing.ChatID != claim.ChatID || existing.MachineID != claim.MachineID ||
				existing.CurrentVersion != claim.CurrentVersion || existing.TargetVersion != claim.TargetVersion {
				return false, errors.New("update operation_id was reused for a different message, chat, machine or version")
			}
			if existing.Started {
				return true, nil
			}
			known = true
			continue
		}
		if existing.MessageID == claim.MessageID && existing.ChatID == claim.ChatID &&
			existing.MachineID == claim.MachineID && existing.TargetVersion == claim.TargetVersion {
			return false, fmt.Errorf("message %q already authorized operation %q for this machine and version; replay that operation_id to read its receipt. Another activation needs a new message from the owner", claim.MessageID, existing.OperationID)
		}
	}
	now := h.updatePermissionTime()
	state, err := h.readChatForUpdatePermission(tabID, chatID)
	if err != nil {
		return false, err
	}
	if _, err := verifyOwnerUpdateMessage(state, claim.MessageID, now); err != nil {
		return false, err
	}
	if known {
		return false, nil
	}
	claim.AcceptedAt = now.UTC().Format(time.RFC3339Nano)
	return false, writeUpdateAuthorizations(path, append(pruneUpdateAuthorizations(claims, now), claim))
}

func (h *agentControlHandler) markUpdatePermissionStarted(operationID string) {
	path, err := h.updateAuthorizationPath()
	if err != nil {
		return
	}
	updateAuthorizationLock.Lock()
	defer updateAuthorizationLock.Unlock()
	claims, err := readUpdateAuthorizations(path)
	if err != nil {
		return
	}
	for index := range claims {
		if claims[index].OperationID == operationID && !claims[index].Started {
			claims[index].Started = true
			// Best effort: without it a replay past the age limit is refused, and
			// the receipt is still there for workass_get_update_status to read.
			_ = writeUpdateAuthorizations(path, claims)
			return
		}
	}
}

// applyAuthorizedUpdate is update.apply from an agent, for this machine or another.
// The updater below keeps the contract it has always had, so a machine or renderer
// that predates this still answers: the version sentence is filled in here rather
// than asked for, and the message id stops at this machine.
func (h *agentControlHandler) applyAuthorizedUpdate(ctx context.Context, tabID, chatID string, params map[string]any) (any, error) {
	operationID, err := providercontract.ValidateOperationID(fieldString(params, "operation_id"))
	if err != nil {
		return nil, fmt.Errorf("update %w", err)
	}
	machineID := firstNonEmptyString(strings.TrimSpace(fieldString(params, "machine_id")), strings.TrimSpace(h.selfMachineID))
	currentVersion := strings.TrimSpace(fieldString(params, "expected_current_version"))
	targetVersion := strings.TrimSpace(fieldString(params, "expected_target_version"))
	if machineID == "" {
		return nil, errors.New("update request requires an exact machine_id")
	}
	if !updateVersionPattern.MatchString(currentVersion) || !updateVersionPattern.MatchString(targetVersion) {
		return nil, errors.New("update request requires exact X.Y.Z current and target versions")
	}
	sentence := fmt.Sprintf("update %s from %s to %s", machineID, currentVersion, targetVersion)
	if text := fieldString(params, "authorization"); text != "" && text != sentence {
		return nil, errors.New("update authorization does not exactly match the machine and versions")
	}
	replay, err := h.claimUpdatePermission(tabID, chatID, updateAuthorizationClaim{
		MessageID: strings.TrimSpace(fieldString(params, "authorizing_message_id")), ChatID: chatID, MachineID: machineID,
		CurrentVersion: currentVersion, TargetVersion: targetVersion, OperationID: string(operationID),
	})
	if err != nil {
		return nil, err
	}
	forwarded := copyAnyMap(params)
	delete(forwarded, "authorizing_message_id")
	forwarded["authorization"] = sentence
	result, err := h.answerUpdateLocally(ctx, "update.apply", forwarded)
	if err == nil && !replay {
		h.markUpdatePermissionStarted(string(operationID))
	}
	return result, err
}

// checkScheduledUpdatePermission is the same test at the moment a programmed update
// is scheduled, so a bad id or an impossible date fails while the owner is still
// reachable instead of days later. The id only travels: the delivered message is
// agent text, and it is the owner's original message that is checked again on apply.
func (h *agentControlHandler) checkScheduledUpdatePermission(tabID, chatID, messageID string, now, due time.Time) error {
	state, err := h.readChatForUpdatePermission(tabID, chatID)
	if err != nil {
		return err
	}
	record, err := verifyOwnerUpdateMessage(state, messageID, now)
	if err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.At))
	if err != nil {
		return err
	}
	if due.After(at.Add(updatePermissionMaxAge)) {
		return fmt.Errorf("this message could come due after the owner's message %q is too old to authorize an update (limit: %d days from it). Shorten the schedule, or ask the owner to allow it closer to the time", messageID, int(updatePermissionMaxAge/(24*time.Hour)))
	}
	return nil
}
