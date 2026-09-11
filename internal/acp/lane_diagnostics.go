package acp

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	providercontract "workass/internal/provider"
)

const maxLaneDiagnostics = 64

// Attachment happens before Manager.StartJob. Keep its outcome separately so a
// failed exact resume cannot disappear merely because no prompt was admitted.
// This is bounded diagnostic evidence, never a retry or lifecycle authority.
type laneDiagnostic struct {
	tabID, chatID, providerID, operation string
	started                              time.Time
	elapsedMS                            int64
	kind                                 providercontract.ErrorKind
	message, transport                   string
	rpcCode                              int
	hasRPCCode                           bool
}

func (m *Manager) recordLaneDiagnostic(tabID, chatID, providerID, operation string, started time.Time, err error, nativeIDs ...string) {
	if m == nil || tabID == "" || chatID == "" {
		return
	}
	d := laneDiagnostic{tabID: tabID, chatID: chatID, providerID: providerID, operation: operation,
		started: started, elapsedMS: time.Since(started).Milliseconds()}
	if err != nil {
		d.kind = providercontract.ErrorTransientTransport
		var typed *providercontract.Error
		if errors.As(err, &typed) {
			if typed.Kind != "" {
				d.kind = typed.Kind
			}
		}
		message := err.Error()
		for _, id := range nativeIDs {
			if id != "" {
				message = strings.ReplaceAll(message, id, "[native-thread]")
			}
		}
		d.message = redactSensitiveText(message)
		if len(d.message) > 2048 {
			d.message = strings.ToValidUTF8(d.message[:2048], "")
		}
		var rpcErr *acpError
		if errors.As(err, &rpcErr) {
			d.rpcCode, d.hasRPCCode = rpcErr.Code, true
		}
		switch {
		case errors.Is(err, context.Canceled):
			d.transport = "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			d.transport = "deadline_exceeded"
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrClosedPipe):
			d.transport = "closed"
		}
	}
	m.diagnosticsMu.Lock()
	if len(m.laneDiagnostics) == maxLaneDiagnostics {
		copy(m.laneDiagnostics, m.laneDiagnostics[1:])
		m.laneDiagnostics = m.laneDiagnostics[:maxLaneDiagnostics-1]
	}
	m.laneDiagnostics = append(m.laneDiagnostics, d)
	m.diagnosticsMu.Unlock()
	if err != nil {
		fields := d.fields()
		fields["tabId"], fields["chatId"] = redactSensitiveText(tabID), redactSensitiveText(chatID)
		m.opts.Logf("provider lane attachment failed", fields)
	}
}

func (d laneDiagnostic) fields() map[string]any {
	fields := map[string]any{
		"providerId": redactSensitiveText(d.providerID), "operation": d.operation,
		"startedAt": d.started.UTC().Format(time.RFC3339Nano), "elapsedMs": d.elapsedMS,
		"outcome": "attached",
	}
	if d.kind != "" {
		fields["outcome"], fields["errorKind"], fields["error"] = "failed", d.kind, d.message
	}
	if d.transport != "" {
		fields["transport"] = d.transport
	}
	if d.hasRPCCode {
		fields["rpcCode"] = d.rpcCode
	}
	return fields
}

func (m *Manager) recentLaneDiagnostics(tabID, chatID string, limit int) []any {
	m.diagnosticsMu.Lock()
	defer m.diagnosticsMu.Unlock()
	result := make([]any, 0, limit)
	for i := len(m.laneDiagnostics) - 1; i >= 0 && len(result) < limit; i-- {
		d := m.laneDiagnostics[i]
		if d.tabID == tabID && d.chatID == chatID {
			result = append(result, d.fields())
		}
	}
	return result
}
