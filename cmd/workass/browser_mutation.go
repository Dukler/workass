package main

import (
	"context"
	"errors"
	"strings"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// executeBrowserMutation is the actor boundary for provider-neutral browser
// effects. The raw browser request is supplied only by the dispatch callback;
// the durable actor stores the exact operation target and a safe digest.
//
// A committed operation is either completed by an explicit shell receipt,
// returned as a bounded terminal actor receipt, or left ambiguous. Only a
// Dispatched/Ambiguous retry may use shell readback; no retry path calls the
// browser mutation again.
func (r *providerChatRuntime) executeBrowserMutation(
	ctx context.Context,
	tabID, chatID string,
	operationID providercontract.OperationID,
	kind, method, digest string,
	dispatch func() (browserControlReply, error),
	readback func() (browserControlReply, error),
) (browserControlReply, error) {
	return r.executeBrowserMutationWithAdmission(
		ctx, tabID, chatID, operationID, kind, method, digest, nil, dispatch, readback,
	)
}

func (r *providerChatRuntime) executeBrowserMutationWithAdmission(
	ctx context.Context,
	tabID, chatID string,
	operationID providercontract.OperationID,
	kind, method, digest string,
	admit func() error,
	dispatch func() (browserControlReply, error),
	readback func() (browserControlReply, error),
) (browserControlReply, error) {
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return browserControlReply{}, err
	}
	validatedOperationID, validationErr := providercontract.ValidateOperationID(string(operationID))
	if validationErr != nil {
		return browserControlReply{}, errors.New("browser mutation requires a valid caller-stable operation id")
	}
	operationID = validatedOperationID

	// One dedicated executor lock preserves external-effect ordering and keeps a
	// retry from reading a receipt before the first dispatch has reached the
	// shell. The actor state lock is never held across shell HTTP.
	actor.externalMutationMu.Lock()
	defer actor.externalMutationMu.Unlock()

	existing, exists, err := inspectBrowserMutation(actor, tabID, chatID, operationID, kind, method, digest)
	if err != nil {
		return browserControlReply{}, err
	}
	// Admission is a pre-dispatch fact, not an authority over an already
	// journaled outcome. Terminal retries are answered from the actor and
	// Dispatched/Ambiguous retries are readback-only even when the shell or its
	// controller is currently absent. A recovered Pending entry still needs its
	// first claim, so it is admitted before that claim without creating a second
	// actor operation.
	if (!exists || existing.Status == chat.OutboxPending) && admit != nil {
		if err := admit(); err != nil {
			return browserControlReply{}, err
		}
	}

	entry, dispatchClaimed, err := prepareBrowserMutation(actor, tabID, chatID, operationID, kind, method, digest)
	if err != nil {
		return browserControlReply{}, err
	}

	if dispatchClaimed {
		if ctx != nil {
			select {
			case <-ctx.Done():
				_ = applyBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest)
				return browserControlReply{}, errors.New("browser mutation dispatch was cancelled after durable claim")
			default:
			}
		}
		if dispatch == nil {
			_ = applyBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest)
			return browserControlReply{}, errors.New("browser mutation has no dispatch executor")
		}
		reply, dispatchErr := dispatch()
		if dispatchErr != nil {
			if err := applyBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest); err != nil {
				return browserControlReply{}, err
			}
			return browserControlReply{}, errors.New("browser mutation dispatch is ambiguous")
		}
		if err := validateBrowserMutationReceipt(reply, operationID, digest); err != nil {
			if receiptErr := applyBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest); receiptErr != nil {
				return browserControlReply{}, receiptErr
			}
			return browserControlReply{}, err
		}
		if err := applyBrowserMutationReply(actor, operationID, kind, method, tabID, digest, reply); err != nil {
			return browserControlReply{}, err
		}
		return reply, nil
	}
	if entry.Status == chat.OutboxCompleted {
		// The actor's terminal commit is authoritative even if the shell that
		// produced the original browser result has since restarted. Do not probe
		// the shell or expose an unbounded browser result from memory; the caller
		// can use this bounded receipt and snapshot current actor state if needed.
		reply := browserControlReply{
			OperationID: string(operationID), RequestDigest: digest, Receipt: true,
			Result: map[string]any{"operationId": string(operationID), "status": "completed", "receipt": true},
		}
		return reply, nil
	}
	if entry.Status == chat.OutboxFailed {
		// Failed is also a terminal actor fact. Keep the retry pure and bounded;
		// LastError is an internal enum and is deliberately not copied into the
		// browser-facing payload.
		reply := browserControlReply{
			OperationID: string(operationID), RequestDigest: digest, Receipt: true,
			Error: "browser mutation was rejected",
		}
		return reply, nil
	}

	// Only a dispatched or ambiguous operation is readback-only. Completed and
	// failed operations returned bounded actor receipts above. The
	// browser receipt endpoint never receives the original selector, text, URL,
	// or batch actions, so a lost response cannot trigger a duplicate effect.
	if readback == nil {
		return browserControlReply{}, errors.New("browser mutation has no receipt readback")
	}
	reply, readbackErr := readback()
	if readbackErr != nil {
		if entry.Status == chat.OutboxDispatched || entry.Status == chat.OutboxAmbiguous {
			_ = applyBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest)
		}
		return browserControlReply{}, errors.New("browser mutation receipt readback is unavailable")
	}
	if err := validateBrowserMutationReceipt(reply, operationID, digest); err != nil {
		if entry.Status == chat.OutboxDispatched || entry.Status == chat.OutboxAmbiguous {
			_ = applyBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest)
		}
		return browserControlReply{}, err
	}
	if err := applyBrowserMutationReply(actor, operationID, kind, method, tabID, digest, reply); err != nil {
		return browserControlReply{}, err
	}
	return reply, nil
}

func inspectBrowserMutation(
	actor *providerChatActor,
	tabID, chatID string,
	operationID providercontract.OperationID,
	kind, method, digest string,
) (chat.OutboxEntry, bool, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()

	state := actor.engine.Snapshot()
	if !state.Initialized || state.Deleted || strings.TrimSpace(state.ChatID) != strings.TrimSpace(chatID) ||
		strings.TrimSpace(state.Presentation.TabID) != strings.TrimSpace(tabID) {
		return chat.OutboxEntry{}, false, errors.New("browser mutation actor fence is stale")
	}
	entry, exists := externalBrowserMutationEntry(state, operationID)
	if !exists {
		return chat.OutboxEntry{}, false, nil
	}
	if entry.ChatID != strings.TrimSpace(chatID) || entry.TabID != strings.TrimSpace(tabID) ||
		entry.MutationKind != strings.TrimSpace(kind) || entry.MutationMethod != strings.TrimSpace(method) ||
		entry.MutationDigest != strings.ToLower(strings.TrimSpace(digest)) {
		// Preserve the reducer's public conflict contract even though admission
		// must inspect an existing operation before deciding whether to contact
		// the browser controller. This boundary is shared by every external
		// mutation (browser, artifact hosting, and visualizations).
		return chat.OutboxEntry{}, false, errors.New("external mutation operation was reused with a different request")
	}
	return entry, true, nil
}

func prepareBrowserMutation(
	actor *providerChatActor,
	tabID, chatID string,
	operationID providercontract.OperationID,
	kind, method, digest string,
) (chat.OutboxEntry, bool, error) {
	actor.mu.Lock()
	defer actor.mu.Unlock()

	state := actor.engine.Snapshot()
	if !state.Initialized || state.Deleted || strings.TrimSpace(state.ChatID) != strings.TrimSpace(chatID) ||
		strings.TrimSpace(state.Presentation.TabID) != strings.TrimSpace(tabID) {
		return chat.OutboxEntry{}, false, errors.New("browser mutation actor fence is stale")
	}
	if err := actor.engine.Apply(chat.RecordExternalMutation{
		OperationID: operationID, Kind: kind, Method: method, TabID: tabID, Digest: digest,
	}); err != nil {
		return chat.OutboxEntry{}, false, err
	}

	entry, ok := externalBrowserMutationEntry(actor.engine.Snapshot(), operationID)
	if !ok {
		return chat.OutboxEntry{}, false, errors.New("browser mutation journal entry was not committed")
	}
	if entry.ChatID != strings.TrimSpace(chatID) || entry.TabID != strings.TrimSpace(tabID) ||
		entry.MutationKind != strings.TrimSpace(kind) || entry.MutationMethod != strings.TrimSpace(method) ||
		entry.MutationDigest != strings.ToLower(strings.TrimSpace(digest)) {
		return chat.OutboxEntry{}, false, errors.New("external mutation operation was reused with a different request")
	}
	if entry.Status != chat.OutboxPending {
		return entry, false, nil
	}

	effect, claimed, err := actor.engine.ClaimEffect(entry.ID)
	if err != nil {
		return chat.OutboxEntry{}, false, err
	}
	if !claimed {
		return chat.OutboxEntry{}, false, errors.New("browser mutation was not durably claimed")
	}
	if _, ok := effect.(chat.ExternalMutationEffect); !ok {
		return chat.OutboxEntry{}, false, errors.New("browser mutation claim returned the wrong effect")
	}
	return entry, true, nil
}

func externalBrowserMutationEntry(state chat.State, operationID providercontract.OperationID) (chat.OutboxEntry, bool) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectExternalMutation && entry.OperationID == operationID {
			return entry, true
		}
	}
	return chat.OutboxEntry{}, false
}

func validateBrowserMutationReceipt(reply browserControlReply, operationID providercontract.OperationID, digest string) error {
	if !reply.Receipt {
		return errors.New("browser mutation returned no durable receipt")
	}
	if strings.TrimSpace(reply.OperationID) != string(providercontract.NormalizeOperationID(string(operationID))) ||
		strings.ToLower(strings.TrimSpace(reply.RequestDigest)) != strings.ToLower(strings.TrimSpace(digest)) {
		return errors.New("browser mutation receipt changed its immutable identity")
	}
	return nil
}

func applyBrowserMutationReply(actor *providerChatActor, operationID providercontract.OperationID, kind, method, tabID, digest string, reply browserControlReply) error {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	failed := strings.TrimSpace(reply.Error) != ""
	errorKind := providercontract.ErrorKind("")
	if failed {
		errorKind = providercontract.ErrorAdmissionRejected
	}
	return actor.engine.Apply(chat.ExternalMutationReceipt{
		OperationID: operationID, Kind: kind, Method: method, TabID: tabID, Digest: digest,
		Failed: failed, ErrorKind: errorKind,
	})
}

func applyBrowserMutationAmbiguous(actor *providerChatActor, operationID providercontract.OperationID, kind, method, tabID, digest string) error {
	actor.mu.Lock()
	defer actor.mu.Unlock()
	return actor.engine.Apply(chat.ExternalMutationReceipt{
		OperationID: operationID, Kind: kind, Method: method, TabID: tabID, Digest: digest,
		Ambiguous: true, ErrorKind: providercontract.ErrorAcceptanceAmbiguous,
	})
}
