package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const providerLaneStoreV7Version = 7

// upgradeProviderLaneStoreV8 adds an explicit provider-durability receipt.
// A pre-v8 record from a deferred-creation provider is committed only when the
// old store itself contains native-consumption or lineage evidence. Otherwise
// it remains the exact candidate that must be verified by provider resume; the
// old schema's assumption that session/new was durable is not evidence.
func upgradeProviderLaneStoreV8(path, machineID string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var header struct {
		Version int `json:"v"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return fmt.Errorf("decode provider lane store header: %w", err)
	}
	if header.Version == currentNativeLaneStoreVersion {
		return nil
	}
	if header.Version != providerLaneStoreV7Version {
		return fmt.Errorf("provider lane store schema v%d is unsupported", header.Version)
	}

	var source nativeSessionLedgerFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&source); err != nil {
		return fmt.Errorf("decode provider lane store schema v7: %w", err)
	}
	validator := &nativeSessionLedger{machineID: strings.TrimSpace(machineID)}
	bindings := make(map[string]nativeSessionBinding, len(source.Bindings))
	for index, sourceBinding := range source.Bindings {
		sourceBinding.ThreadCommitted = preV8HasDurableThreadEvidence(sourceBinding)
		binding, normalizeErr := validator.normalizeBinding(sourceBinding)
		if normalizeErr != nil {
			return fmt.Errorf("provider lane store binding %d is invalid: %w", index, normalizeErr)
		}
		key := nativeLaneStorageKey(binding.LaneID)
		if _, exists := bindings[key]; exists {
			return fmt.Errorf("provider lane store has duplicate immutable lane %q", binding.LaneID)
		}
		bindings[key] = binding
	}
	ledger := &nativeSessionLedger{path: path, bindings: bindings, machineID: strings.TrimSpace(machineID)}
	if err := ledger.writeLocked(); err != nil {
		return fmt.Errorf("commit provider lane store schema v8: %w", err)
	}
	return verifyProviderLaneStoreV8(path, bindings, machineID)
}

func preV8HasDurableThreadEvidence(binding nativeSessionBinding) bool {
	if !providerAdapterForID(binding.ProviderID).creation.DeferredUntilInput {
		return true
	}
	if strings.TrimSpace(binding.ProviderSessionID) != "" || binding.ThreadLineage > 1 || strings.TrimSpace(binding.LineageProof) != "" {
		return true
	}
	if binding.PendingOperation != nil && binding.PendingOperation.State == nativeOperationConsumed {
		return true
	}
	if binding.LastOperation != nil {
		switch binding.LastOperation.State {
		case nativeOperationTerminal, nativeOperationAbsent:
			return true
		}
	}
	return false
}

func verifyProviderLaneStoreV8(path string, expected map[string]nativeSessionBinding, machineID string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var disk nativeSessionLedgerFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&disk); err != nil {
		return err
	}
	if disk.Version != currentNativeLaneStoreVersion || len(disk.Bindings) != len(expected) {
		return fmt.Errorf("receipt mismatch: version=%d bindings=%d want=%d", disk.Version, len(disk.Bindings), len(expected))
	}
	validator := &nativeSessionLedger{machineID: strings.TrimSpace(machineID)}
	for index, binding := range disk.Bindings {
		binding, err = validator.normalizeBinding(binding)
		if err != nil {
			return fmt.Errorf("binding %d: %w", index, err)
		}
		key := nativeLaneStorageKey(binding.LaneID)
		want, exists := expected[key]
		if !exists || binding.ThreadCommitted != want.ThreadCommitted || bindingLaneIdentity(binding) != bindingLaneIdentity(want) ||
			!bindingThreadRef(binding).Equal(bindingThreadRef(want)) {
			return fmt.Errorf("lane %q failed durable ownership readback", binding.LaneID)
		}
	}
	return nil
}
