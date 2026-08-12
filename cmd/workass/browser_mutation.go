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
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return browserControlReply{}, err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()

	state := actor.engine.Snapshot()
	if !state.Initialized || state.Deleted || strings.TrimSpace(state.ChatID) != strings.TrimSpace(chatID) ||
		strings.TrimSpace(state.Presentation.TabID) != strings.TrimSpace(tabID) {
		return browserControlReply{}, errors.New("browser mutation actor fence is stale")
	}
	validatedOperationID, validationErr := providercontract.ValidateOperationID(string(operationID))
	if validationErr != nil {
		return browserControlReply{}, errors.New("browser mutation requires a valid caller-stable operation id")
	}
	operationID = validatedOperationID
	if err := actor.engine.Apply(chat.RecordExternalMutation{
		OperationID: operationID, Kind: kind, Method: method, TabID: tabID, Digest: digest,
	}); err != nil {
		return browserControlReply{}, err
	}

	entry, ok := externalBrowserMutationEntry(actor.engine.Snapshot(), operationID)
	if !ok {
		return browserControlReply{}, errors.New("browser mutation journal entry was not committed")
	}
	if entry.ChatID != strings.TrimSpace(chatID) || entry.TabID != strings.TrimSpace(tabID) ||
		entry.MutationKind != strings.TrimSpace(kind) || entry.MutationMethod != strings.TrimSpace(method) ||
		entry.MutationDigest != strings.ToLower(strings.TrimSpace(digest)) {
		return browserControlReply{}, errors.New("browser mutation journal target changed")
	}

	if entry.Status == chat.OutboxPending {
		effect, claimed, err := actor.engine.ClaimEffect(entry.ID)
		if err != nil {
			return browserControlReply{}, err
		}
		if !claimed {
			return browserControlReply{}, errors.New("browser mutation was not durably claimed")
		}
		if _, ok := effect.(chat.ExternalMutationEffect); !ok {
			return browserControlReply{}, errors.New("browser mutation claim returned the wrong effect")
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				_ = recordBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest)
				return browserControlReply{}, errors.New("browser mutation dispatch was cancelled after durable claim")
			default:
			}
		}
		if dispatch == nil {
			_ = recordBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest)
			return browserControlReply{}, errors.New("browser mutation has no dispatch executor")
		}
		reply, dispatchErr := dispatch()
		if dispatchErr != nil {
			if err := recordBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest); err != nil {
				return browserControlReply{}, err
			}
			return browserControlReply{}, errors.New("browser mutation dispatch is ambiguous")
		}
		if err := validateBrowserMutationReceipt(reply, operationID, digest); err != nil {
			if receiptErr := recordBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest); receiptErr != nil {
				return browserControlReply{}, receiptErr
			}
			return browserControlReply{}, err
		}
		if err := recordBrowserMutationReply(actor, operationID, kind, method, tabID, digest, reply); err != nil {
			return browserControlReply{}, err
		}
		return reply, nil
	}
	if entry.Status == chat.OutboxCompleted {
		// The actor's terminal commit is authoritative even if the shell that
		// produced the original browser result has since restarted. Do not probe
		// the shell or expose an unbounded browser result from memory; the caller
		// can use this bounded receipt and snapshot current actor state if needed.
		return browserControlReply{
			OperationID: string(operationID), RequestDigest: digest, Receipt: true,
			Result: map[string]any{"operationId": string(operationID), "status": "completed", "receipt": true},
		}, nil
	}
	if entry.Status == chat.OutboxFailed {
		// Failed is also a terminal actor fact. Keep the retry pure and bounded;
		// LastError is an internal enum and is deliberately not copied into the
		// browser-facing payload.
		return browserControlReply{
			OperationID: string(operationID), RequestDigest: digest, Receipt: true,
			Error: "browser mutation was rejected",
		}, nil
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
			_ = recordBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest)
		}
		return browserControlReply{}, errors.New("browser mutation receipt readback is unavailable")
	}
	if err := validateBrowserMutationReceipt(reply, operationID, digest); err != nil {
		if entry.Status == chat.OutboxDispatched || entry.Status == chat.OutboxAmbiguous {
			_ = recordBrowserMutationAmbiguous(actor, operationID, kind, method, tabID, digest)
		}
		return browserControlReply{}, err
	}
	if err := recordBrowserMutationReply(actor, operationID, kind, method, tabID, digest, reply); err != nil {
		return browserControlReply{}, err
	}
	return reply, nil
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

func recordBrowserMutationReply(actor *providerChatActor, operationID providercontract.OperationID, kind, method, tabID, digest string, reply browserControlReply) error {
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

func recordBrowserMutationAmbiguous(actor *providerChatActor, operationID providercontract.OperationID, kind, method, tabID, digest string) error {
	return actor.engine.Apply(chat.ExternalMutationReceipt{
		OperationID: operationID, Kind: kind, Method: method, TabID: tabID, Digest: digest,
		Ambiguous: true, ErrorKind: providercontract.ErrorAcceptanceAmbiguous,
	})
}
