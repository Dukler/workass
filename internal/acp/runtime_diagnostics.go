package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
)

const maxRuntimeDiagnosticEvents = 32

// Only numeric measurements, fixed enums and a fresh host-instance UUID cross
// this boundary. Free-form provider errors can contain prompts and credentials;
// redacting arbitrary text is not sufficient for this diagnostic surface.
type runtimeDiagnosticState struct {
	Observed      int64            `json:"observed"`
	Retries       int64            `json:"retries"`
	Errors        int64            `json:"errors"`
	HostErrors    int64            `json:"hostErrors"`
	Compactions   int64            `json:"compactions"`
	Fallbacks     int64            `json:"fallbacks"`
	DroppedEvents int64            `json:"droppedEvents"`
	Input         map[string]any   `json:"input,omitempty"`
	Usage         map[string]any   `json:"usage,omitempty"`
	Events        []map[string]any `json:"events"`
}

func cloneDiagnosticMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (d runtimeDiagnosticState) clone() runtimeDiagnosticState {
	d.Input, d.Usage = cloneDiagnosticMap(d.Input), cloneDiagnosticMap(d.Usage)
	events := make([]map[string]any, len(d.Events))
	for i, e := range d.Events {
		events[i] = cloneDiagnosticMap(e)
	}
	d.Events = events
	return d
}

func diagnosticNumber(value any) (int64, bool) {
	var n float64
	switch v := value.(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1<<53-1 || math.Trunc(n) != n {
		return 0, false
	}
	return int64(n), true
}

var diagnosticHostID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func sanitizeRuntimeDiagnostic(raw map[string]any) map[string]any {
	kind := asString(raw["kind"])
	var numbers, booleans string
	enums := map[string]string{}
	switch kind {
	case "input":
		numbers = "inputBytes textBytes imageCount imageDataBytes resumeReplyBytes resumeElapsedMs"
		booleans = "resumed"
		enums["historyMode"] = "paginated legacy unknown"
		enums["effort"] = "minimal none low medium high xhigh max ultra"
		enums["serviceTier"] = "default fast priority"
	case "usage":
		numbers = "used size input cachedInput output reasoningOutput"
		booleans = "prior"
	case "error":
		numbers = "httpStatus retryAttempt retryLimit"
		booleans = "willRetry closeDetailsAvailable"
		enums["category"] = "stream_disconnected connection_failed context_limit rate_limit authentication server_error retry_exhausted other"
		enums["reason"] = "peer_closed idle_timeout connection_failed retry_exhausted other"
	case "fallback":
		enums["transport"] = "https"
		enums["scope"] = "thread"
	case "host_transport":
		booleans = "rpcReply"
		enums["reason"] = "rpc_error closed cancelled deadline_exceeded other"
	case "compaction":
		enums["phase"] = "started completed"
	case "turn":
		enums["phase"] = "started completed failed interrupted"
	default:
		return nil
	}
	out := map[string]any{"kind": kind}
	for _, key := range strings.Fields(numbers) {
		if n, ok := diagnosticNumber(raw[key]); ok {
			if key == "httpStatus" && (n < 100 || n > 599) {
				continue
			}
			out[key] = n
		}
	}
	for _, key := range strings.Fields(booleans) {
		if v, ok := raw[key].(bool); ok {
			out[key] = v
		}
	}
	for key, values := range enums {
		value := asString(raw[key])
		for _, allowed := range strings.Fields(values) {
			if value == allowed {
				out[key] = value
				break
			}
		}
	}
	if kind == "input" && diagnosticHostID.MatchString(asString(raw["hostInstanceId"])) {
		out["hostInstanceId"] = raw["hostInstanceId"]
	}
	return out
}

func (s *turnStartupTiming) recordInput(input map[string]int64) {
	if s == nil {
		return
	}
	s.detailMu.Lock()
	defer s.detailMu.Unlock()
	s.input = input
}

// Returns true only for a failure/fallback checkpoint. Usage and deltas never
// trigger disk writes, and no provider activity is inferred from these events.
func (s *turnStartupTiming) observeRuntimeDiagnostic(raw map[string]any) bool {
	if s == nil {
		return false
	}
	event := sanitizeRuntimeDiagnostic(raw)
	if event == nil {
		return false
	}
	s.detailMu.Lock()
	defer s.detailMu.Unlock()
	if s.stages[startupFinished].Load() != 0 || s.restoredElapsed != nil {
		return false
	}
	d := &s.runtime
	d.Observed++
	event["sequence"] = d.Observed
	event["atMs"] = time.Since(s.started).Milliseconds()
	kind := event["kind"]
	switch kind {
	case "input":
		d.Input = cloneDiagnosticMap(event)
	case "usage":
		d.Usage = cloneDiagnosticMap(event)
		// Preserve useful retry/compaction chronology during long tool loops.
		// Usage has a latest-value snapshot instead of displacing event history.
		return false
	case "error":
		d.Errors++
		if event["willRetry"] == true {
			d.Retries++
		}
	case "fallback":
		d.Fallbacks++
	case "host_transport":
		d.HostErrors++
	case "compaction":
		if event["phase"] == "completed" {
			d.Compactions++
		}
	}
	if len(d.Events) == maxRuntimeDiagnosticEvents {
		copy(d.Events, d.Events[1:])
		d.Events = d.Events[:maxRuntimeDiagnosticEvents-1]
		d.DroppedEvents++
	}
	d.Events = append(d.Events, event)
	return kind == "error" || kind == "fallback" || kind == "host_transport"
}

func (s *turnStartupTiming) observeHostFailure(err error) {
	if s == nil || err == nil {
		return
	}
	event := map[string]any{"kind": "host_transport", "reason": "other", "rpcReply": false}
	var rpcErr *acpError
	switch {
	case errors.As(err, &rpcErr):
		event["reason"], event["rpcReply"] = "rpc_error", true
		s.mark(startupTerminalReply)
	case errors.Is(err, context.Canceled):
		event["reason"] = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		event["reason"] = "deadline_exceeded"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrClosedPipe):
		event["reason"] = "closed"
	}
	s.observeRuntimeDiagnostic(event)
}
