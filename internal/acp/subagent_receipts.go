package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxSubagentReceiptsPerChat  = 256
	maxSubagentReceiptFileBytes = 4 * 1024 * 1024
)

// SubagentReceipt is the durable, provider-neutral result visible to later
// turns in the same Workass chat. Provider-native/session ids are deliberately
// excluded: receipts describe work, not transient transport ownership.
type SubagentReceipt struct {
	ReceiptID       string `json:"receiptId"`
	SubagentID      string `json:"subagentId"`
	Label           string `json:"label"`
	Status          string `json:"status"`
	ProviderID      string `json:"providerId"`
	ModelID         string `json:"modelId,omitempty"`
	Effort          string `json:"effort,omitempty"`
	ModelLabel      string `json:"modelLabel,omitempty"`
	ModeID          string `json:"modeId,omitempty"`
	Profile         string `json:"profile,omitempty"`
	RetryOf         string `json:"retryOf,omitempty"`
	StartedAt       string `json:"startedAt"`
	FinishedAt      string `json:"finishedAt"`
	ElapsedMs       int64  `json:"elapsedMs"`
	StopReason      string `json:"stopReason,omitempty"`
	Result          string `json:"result,omitempty"`
	Error           string `json:"error,omitempty"`
	ResultTruncated bool   `json:"resultTruncated,omitempty"`
	ErrorTruncated  bool   `json:"errorTruncated,omitempty"`
	ParentChatID    string `json:"parentChatId,omitempty"`
	ParentTabID     string `json:"parentTabId,omitempty"`
	OriginLaneID    string `json:"originLaneId,omitempty"`
	DeliveryPending bool   `json:"deliveryPending,omitempty"`
	DeliveryError   string `json:"deliveryError,omitempty"`
}

func receiptFromRun(run SubagentRun) SubagentReceipt {
	return SubagentReceipt{
		ReceiptID: run.ReceiptID, SubagentID: run.ID, Label: run.Label, Status: run.Status,
		ProviderID: run.ProviderID, ModelID: run.ModelID, Effort: run.Effort,
		ModelLabel: run.ModelLabel, ModeID: run.ModeID, Profile: run.Profile, RetryOf: run.RetryOf,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, ElapsedMs: run.ElapsedMs,
		StopReason: run.StopReason, Result: run.Result, Error: run.Error,
		ResultTruncated: run.ResultTruncated, ErrorTruncated: run.ErrorTruncated,
		OriginLaneID: run.originLaneID,
	}
}

func (m *Manager) subagentReceiptPath(chatID, tabID string) string {
	stateDir := strings.TrimSpace(m.opts.StateDir)
	if stateDir == "" {
		return ""
	}
	key := firstNonEmpty(strings.TrimSpace(tabID), strings.TrimSpace(chatID))
	if key == "" {
		return ""
	}
	return filepath.Join(stateDir, "subagent-receipts", safeArchiveName(key)+".jsonl")
}

func (m *Manager) persistSubagentReceipt(chatID, tabID string, run SubagentRun, deliveryPending bool) bool {
	path := m.subagentReceiptPath(chatID, tabID)
	if path == "" || run.FinishedAt == "" {
		return false
	}
	receipt := receiptFromRun(run)
	receipt.ParentChatID, receipt.ParentTabID = strings.TrimSpace(chatID), strings.TrimSpace(tabID)
	receipt.DeliveryPending = deliveryPending
	return m.writeSubagentReceipt(tabID, chatID, receipt) == nil
}

// InstallSubagentCompletionObserver installs the actor-owned delivery boundary
// once, then retries only durable receipts whose pending bit was committed.
func (m *Manager) InstallSubagentCompletionObserver(observer func(tabID, chatID string, receipt SubagentReceipt) error) error {
	if m == nil || observer == nil {
		return errors.New("tracked subagent completion observer is required")
	}
	m.subagentCompletionObserverMu.Lock()
	if m.subagentCompletionObserver != nil {
		m.subagentCompletionObserverMu.Unlock()
		return errors.New("tracked subagent completion observer is already installed")
	}
	m.subagentCompletionObserver = observer
	m.subagentCompletionObserverMu.Unlock()
	for _, receipt := range m.pendingSubagentCompletions() {
		m.deliverSubagentCompletion(receipt.ParentTabID, receipt.ParentChatID, receipt)
	}
	return nil
}

func (m *Manager) deliverSubagentCompletion(tabID, chatID string, receipt SubagentReceipt) {
	m.subagentCompletionObserverMu.RLock()
	observer := m.subagentCompletionObserver
	m.subagentCompletionObserverMu.RUnlock()
	if observer == nil || !receipt.DeliveryPending {
		return
	}
	if err := observer(tabID, chatID, receipt); err != nil {
		receipt.DeliveryError = compactText(redactSensitiveText(err.Error()), 300)
		_ = m.writeSubagentReceipt(tabID, chatID, receipt)
		return // pending receipt remains durable for restart recovery
	}
	receipt.DeliveryPending = false
	receipt.DeliveryError = ""
	_ = m.writeSubagentReceipt(tabID, chatID, receipt)
}

func (m *Manager) pendingSubagentCompletions() []SubagentReceipt {
	stateDir := strings.TrimSpace(m.opts.StateDir)
	if stateDir == "" {
		return nil
	}
	dir := filepath.Join(stateDir, "subagent-receipts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	m.receiptMu.Lock()
	latest := readLatestSubagentReceipts(dir, entries)
	m.receiptMu.Unlock()
	out := make([]SubagentReceipt, 0)
	for _, receipt := range latest {
		if receipt.DeliveryPending && receipt.ParentTabID != "" && receipt.ParentChatID != "" {
			out = append(out, receipt)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FinishedAt < out[j].FinishedAt })
	return out
}

func readLatestSubagentReceipts(dir string, entries []os.DirEntry) map[string]SubagentReceipt {
	latest := make(map[string]SubagentReceipt)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		for _, line := range boundedReceiptLines(data) {
			var receipt SubagentReceipt
			if json.Unmarshal(line, &receipt) == nil && receipt.ReceiptID != "" {
				latest[receipt.ReceiptID] = receipt
			}
		}
	}
	return latest
}

func (m *Manager) writeSubagentReceipt(tabID, chatID string, receipt SubagentReceipt) error {
	path := m.subagentReceiptPath(chatID, tabID)
	if path == "" {
		return errors.New("tracked subagent receipt path is unavailable")
	}
	m.receiptMu.Lock()
	defer m.receiptMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	existing, _ := os.ReadFile(path)
	latest := make(map[string]SubagentReceipt)
	for _, line := range boundedReceiptLines(existing) {
		var prior SubagentReceipt
		if json.Unmarshal(line, &prior) == nil && prior.ReceiptID != "" {
			latest[prior.ReceiptID] = prior
		}
	}
	latest[receipt.ReceiptID] = receipt
	ordered := make([]SubagentReceipt, 0, len(latest))
	for _, prior := range latest {
		ordered = append(ordered, prior)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].FinishedAt < ordered[j].FinishedAt })
	pending := make([]SubagentReceipt, 0)
	ordinary := make([]SubagentReceipt, 0)
	for _, prior := range ordered {
		if prior.DeliveryPending {
			pending = append(pending, prior)
		} else {
			ordinary = append(ordinary, prior)
		}
	}
	if len(pending) > maxSubagentReceiptsPerChat {
		return errors.New("tracked subagent pending completion limit reached")
	}
	keep := maxSubagentReceiptsPerChat - len(pending)
	if len(ordinary) > keep {
		ordinary = ordinary[len(ordinary)-keep:]
	}
	ordered = append(pending, ordinary...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].FinishedAt < ordered[j].FinishedAt })
	lines := make([][]byte, 0, len(ordered))
	for _, prior := range ordered {
		line, marshalErr := json.Marshal(prior)
		if marshalErr != nil {
			return marshalErr
		}
		lines = append(lines, line)
	}
	payload := append(bytes.Join(lines, []byte("\n")), '\n')
	for len(payload) > maxSubagentReceiptFileBytes && len(ordinary) > 0 {
		ordinary = ordinary[1:]
		ordered = append(append([]SubagentReceipt(nil), pending...), ordinary...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].FinishedAt < ordered[j].FinishedAt })
		lines = lines[:0]
		for _, prior := range ordered {
			line, marshalErr := json.Marshal(prior)
			if marshalErr != nil {
				return marshalErr
			}
			lines = append(lines, line)
		}
		payload = append(bytes.Join(lines, []byte("\n")), '\n')
	}
	if len(payload) > maxSubagentReceiptFileBytes {
		return errors.New("tracked subagent pending receipts exceed storage bound")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func boundedReceiptLines(data []byte) [][]byte {
	if len(data) > maxSubagentReceiptFileBytes {
		data = data[len(data)-maxSubagentReceiptFileBytes:]
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			data = data[idx+1:]
		}
	}
	lines := make([][]byte, 0, maxSubagentReceiptsPerChat)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 16*1024), 64*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		lines = append(lines, append([]byte(nil), line...))
	}
	if len(lines) > maxSubagentReceiptsPerChat {
		lines = lines[len(lines)-maxSubagentReceiptsPerChat:]
	}
	return lines
}

func (m *Manager) ListSubagentReceipts(ownerKey, parentChatID, parentTabID string, limit int) []SubagentReceipt {
	chatID, tabID, ok := m.subagentOwnerIdentity(ownerKey, parentChatID, parentTabID)
	if !ok {
		return []SubagentReceipt{}
	}
	path := m.subagentReceiptPath(chatID, tabID)
	if path == "" {
		return []SubagentReceipt{}
	}
	m.receiptMu.Lock()
	data, err := os.ReadFile(path)
	m.receiptMu.Unlock()
	if err != nil {
		return []SubagentReceipt{}
	}
	if limit <= 0 || limit > maxSubagentReceiptsPerChat {
		limit = 32
	}
	lines := boundedReceiptLines(data)
	latest := make(map[string]SubagentReceipt, len(lines))
	for _, line := range lines {
		var receipt SubagentReceipt
		if json.Unmarshal(line, &receipt) == nil && receipt.ReceiptID != "" {
			latest[receipt.ReceiptID] = receipt
		}
	}
	ordered := make([]SubagentReceipt, 0, len(latest))
	for _, receipt := range latest {
		receipt.ParentChatID, receipt.ParentTabID, receipt.OriginLaneID, receipt.DeliveryPending = "", "", "", false
		ordered = append(ordered, receipt)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].FinishedAt < ordered[j].FinishedAt })
	if len(ordered) > limit {
		ordered = ordered[len(ordered)-limit:]
	}
	return ordered
}
