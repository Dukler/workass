package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// providerLaneBindingV6 exists only at the disk upgrade boundary. The live
// lane record intentionally has no transcript cursor or recoverable-invalid
// state: native identity is validated before the runtime can observe it.
type providerLaneBindingV6 struct {
	nativeSessionBinding
	SyncedMessages   int    `json:"syncedMessages,omitempty"`
	HistoryHash      string `json:"historyHash,omitempty"`
	HistoryVersion   int    `json:"historyVersion,omitempty"`
	ResumeSafe       bool   `json:"resumeSafe,omitempty"`
	Quarantined      bool   `json:"quarantined,omitempty"`
	QuarantineReason string `json:"quarantineReason,omitempty"`
}

type providerLaneStoreV6 struct {
	Version  int                     `json:"v"`
	Bindings []providerLaneBindingV6 `json:"bindings"`
}

// upgradeProviderLaneStoreV7 performs the one supported provider-lane storage
// evolution before the ledger is loaded. It drops transcript-derived resume
// evidence and accepts only an unambiguous immutable lane/thread ownership
// graph. It never contacts a provider and never chooses between identities.
func upgradeProviderLaneStoreV7(path, machineID string) error {
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
	switch header.Version {
	case currentNativeLaneStoreVersion:
		return nil
	case 6:
	default:
		return fmt.Errorf("provider lane store requires unsupported bridge from schema v%d", header.Version)
	}

	var source providerLaneStoreV6
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&source); err != nil {
		return fmt.Errorf("decode provider lane store schema v6: %w", err)
	}
	validator := &nativeSessionLedger{machineID: strings.TrimSpace(machineID)}
	bindings := make(map[string]nativeSessionBinding, len(source.Bindings))
	ownerByThread := make(map[string]string, len(source.Bindings))
	for index, sourceBinding := range source.Bindings {
		if sourceBinding.Quarantined {
			return fmt.Errorf("provider lane store binding %d has no valid immutable owner", index)
		}
		binding, err := validator.normalizeBinding(sourceBinding.nativeSessionBinding)
		if err != nil {
			return fmt.Errorf("provider lane store binding %d is invalid: %w", index, err)
		}
		key := nativeLaneStorageKey(binding.LaneID)
		if _, exists := bindings[key]; exists {
			return fmt.Errorf("provider lane store has duplicate immutable lane %q", binding.LaneID)
		}
		for _, threadID := range bindingThreadIDs(binding) {
			threadKey := binding.ProviderID + "\x00" + threadID
			if owner, exists := ownerByThread[threadKey]; exists && owner != key {
				return fmt.Errorf("provider-native thread %q has multiple immutable lane owners", threadID)
			}
			ownerByThread[threadKey] = key
		}
		bindings[key] = binding
	}

	ledger := &nativeSessionLedger{path: path, bindings: bindings, machineID: strings.TrimSpace(machineID)}
	if err := ledger.writeLocked(); err != nil {
		return fmt.Errorf("commit provider lane store schema v7: %w", err)
	}
	if err := verifyProviderLaneStoreV7(path, bindings, machineID); err != nil {
		return fmt.Errorf("verify provider lane store schema v7: %w", err)
	}
	return nil
}

func verifyProviderLaneStoreV7(path string, expected map[string]nativeSessionBinding, machineID string) error {
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
	seen := make([]string, 0, len(disk.Bindings))
	for index, binding := range disk.Bindings {
		binding, err = validator.normalizeBinding(binding)
		if err != nil {
			return fmt.Errorf("binding %d: %w", index, err)
		}
		key := nativeLaneStorageKey(binding.LaneID)
		want, exists := expected[key]
		if !exists || bindingLaneIdentity(binding) != bindingLaneIdentity(want) || !bindingThreadRef(binding).Equal(bindingThreadRef(want)) {
			return fmt.Errorf("lane %q failed immutable ownership readback", binding.LaneID)
		}
		seen = append(seen, key)
	}
	sort.Strings(seen)
	for index := 1; index < len(seen); index++ {
		if seen[index] == seen[index-1] {
			return fmt.Errorf("lane %q appears more than once after readback", seen[index])
		}
	}
	return nil
}
