package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
}

func receiptFromRun(run SubagentRun) SubagentReceipt {
	return SubagentReceipt{
		ReceiptID: run.ReceiptID, SubagentID: run.ID, Label: run.Label, Status: run.Status,
		ProviderID: run.ProviderID, ModelID: run.ModelID, Effort: run.Effort,
		ModelLabel: run.ModelLabel, ModeID: run.ModeID, Profile: run.Profile, RetryOf: run.RetryOf,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, ElapsedMs: run.ElapsedMs,
		StopReason: run.StopReason, Result: run.Result, Error: run.Error,
		ResultTruncated: run.ResultTruncated, ErrorTruncated: run.ErrorTruncated,
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

func (m *Manager) persistSubagentReceipt(chatID, tabID string, run SubagentRun) {
	path := m.subagentReceiptPath(chatID, tabID)
	if path == "" || run.FinishedAt == "" {
		return
	}
	receipt := receiptFromRun(run)
	data, err := json.Marshal(receipt)
	if err != nil {
		return
	}
	m.receiptMu.Lock()
	defer m.receiptMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	existing, _ := os.ReadFile(path)
	lines := boundedReceiptLines(existing)
	lines = append(lines, data)
	if len(lines) > maxSubagentReceiptsPerChat {
		lines = lines[len(lines)-maxSubagentReceiptsPerChat:]
	}
	payload := append(bytes.Join(lines, []byte("\n")), '\n')
	for len(payload) > maxSubagentReceiptFileBytes && len(lines) > 1 {
		lines = lines[1:]
		payload = append(bytes.Join(lines, []byte("\n")), '\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
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
	if len(lines) > maxSubagentReceiptsPerChat-1 {
		lines = lines[len(lines)-(maxSubagentReceiptsPerChat-1):]
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
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	out := make([]SubagentReceipt, 0, len(lines))
	for _, line := range lines {
		var receipt SubagentReceipt
		if json.Unmarshal(line, &receipt) == nil && receipt.ReceiptID != "" {
			out = append(out, receipt)
		}
	}
	return out
}
