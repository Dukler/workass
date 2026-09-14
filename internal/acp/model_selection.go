package acp

import (
	"errors"
	"strings"

	providercontract "workass/internal/provider"
)

// Only a fully understood select is evidence of absence. An omitted or malformed
// catalog must not prevent custom/dynamic models from reaching their provider.
func completeModelSelectValues(raw any) map[string]struct{} {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	result := make(map[string]struct{}, len(values))
	for _, rawValue := range values {
		value, ok := mapFromAny(rawValue)["value"].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil
		}
		result[value] = struct{}{}
	}
	return result
}

func (b *Bridge) validateModelSelection(modelID string) error {
	if strings.TrimSpace(modelID) == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.validateModelSelectionLocked(b.resolveModelWriteLocked(modelID))
}

func (b *Bridge) validateModelSelectionLocked(selection modelWriteResolution) error {
	if !providerAdapterForID(b.providerID).model.ClosedCatalog || b.modelSelectValues == nil {
		return nil
	}
	if _, ok := b.modelSelectValues[selection.modelValue]; ok {
		return nil
	}
	return &providercontract.Error{Kind: providercontract.ErrorModelUnavailable, Message: "selected model is not in the provider's current model select"}
}

// A model may disappear after the catalog read. Recognize the exact resource
// error at the model-write boundary; unrelated resources/operations stay intact.
func modelNotFoundRPC(err error) bool {
	var rpcErr *acpError
	return errors.As(err, &rpcErr) && rpcErr.Code == -32002 && rpcErr.Msg == "Resource not found" &&
		strings.HasPrefix(asString(mapFromAny(rpcErr.Data)["uri"]), "Model not found: ")
}

func classifyModelSelectionError(err error) error {
	if modelNotFoundRPC(err) {
		return &providercontract.Error{Kind: providercontract.ErrorModelUnavailable, Message: "provider rejected the selected model", Cause: err}
	}
	return err
}
