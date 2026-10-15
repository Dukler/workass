package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// providerForkStrategy is a provider's native session fork. A provider whose
// adapter has none cannot fork, and the catalog says so instead of a replay.
type providerForkStrategy interface {
	// Capability is the catalog's per-provider description, including the
	// measured evidence that children actually reuse the prompt cache.
	Capability() map[string]any
	// RunTurn resumes spec.ResumeSessionID as a NEW forked native session,
	// delivers spec.Content as one user message and runs until the turn ends.
	RunTurn(ctx context.Context, spec forkTurnSpec) (forkTurnResult, error)
	// NativeStore is where the provider keeps its own sessions for this
	// launch environment.
	NativeStore(extraEnv map[string]string) string
	// DiscardSession deletes a throwaway child's native session from store.
	DiscardSession(store, sessionID string)
}

type forkTurnSpec struct {
	Launch          ForkLaunch
	CWD             string
	ResumeSessionID string
	ModelID         string
	Effort          string
	ModeID          string
	AddDirs         []string
	// Persist keeps the forked native session on disk so it can be resumed.
	// Seals must persist (children resume them); throwaway children must not.
	Persist         bool
	Content         []any
	Timeout         time.Duration
	TranscriptPath  string
	UserText        string
	UserAttachments []ImageAttachment
	OnActivity      func(string)
	OnSession       func(string)
}

type forkTurnResult struct {
	SessionID       string
	Result          string
	IsError         bool
	Subtype         string
	Turns           int
	Usage           ForkUsage
	LastCallContext int64
}

const (
	forkStderrTailBytes    = 8 * 1024
	forkTranscriptToolText = 4000
	forkTranscriptMaxBytes = 4 << 20
)

func forkStrategy(providerID string) providerForkStrategy {
	return providerAdapterForID(normalizeProviderID(providerID)).fork
}

// ForkCapability reports whether a provider can fork natively.
func ForkCapability(providerID string) map[string]any {
	if strategy := forkStrategy(providerID); strategy != nil {
		return strategy.Capability()
	}
	return map[string]any{
		"supported": false,
		"reason":    "this provider exposes no native session fork; Workass never substitutes a transcript replay",
	}
}

func forkSupported(providerID string) bool {
	return forkStrategy(providerID) != nil
}

// runProviderForkTurn dispatches to the provider's registered fork strategy.
func runProviderForkTurn(ctx context.Context, spec forkTurnSpec) (forkTurnResult, error) {
	strategy := forkStrategy(spec.Launch.ProviderID)
	if strategy == nil {
		return forkTurnResult{}, fmt.Errorf("provider %q has no native session fork", spec.Launch.ProviderID)
	}
	return strategy.RunTurn(ctx, spec)
}

func discardForkSession(launch ForkLaunch, sessionID string) {
	if strategy := forkStrategy(launch.ProviderID); strategy != nil && strings.TrimSpace(sessionID) != "" {
		strategy.DiscardSession(launch.NativeStore, sessionID)
	}
}

// ForkLaunch resolves the provider executable and environment the chat's own
// provider process would use. Only native-fork providers are accepted.
func (m *Manager) ForkLaunch(providerID string) (ForkLaunch, error) {
	if m == nil {
		return ForkLaunch{}, errors.New("Workass provider manager is unavailable")
	}
	id := normalizeProviderID(providerID)
	strategy := forkStrategy(id)
	if strategy == nil {
		return ForkLaunch{}, fmt.Errorf("provider %q has no native session fork", id)
	}
	cfg, err := m.providerConfig(id)
	if err != nil {
		return ForkLaunch{}, err
	}
	command := launchCommand(cfg)
	if command == "" {
		return ForkLaunch{}, fmt.Errorf("provider %q has no executable", id)
	}
	extra := effectiveProviderEnv(cfg)
	env := mergedEnv(os.Environ(), extra, providerAdapterForID(id).launch.EnvironmentPolicy())
	return ForkLaunch{ProviderID: id, Command: command, Env: env, NativeStore: strategy.NativeStore(extra)}, nil
}

func forkToolTarget(input map[string]any) string {
	for _, key := range []string{"file_path", "path", "command", "pattern", "url", "description"} {
		if value := strings.TrimSpace(asString(input[key])); value != "" {
			return compactText(RedactSensitiveText(value), 160)
		}
	}
	return ""
}

func boundedToolInput(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case string:
			out[key] = compactText(RedactSensitiveText(typed), forkTranscriptToolText)
		case float64, bool, nil:
			out[key] = typed
		default:
			encoded, _ := json.Marshal(typed)
			out[key] = compactText(RedactSensitiveText(string(encoded)), forkTranscriptToolText)
		}
	}
	return out
}

func transcriptAttachments(attachments []ImageAttachment) []any {
	out := make([]any, 0, len(attachments))
	for _, attachment := range attachments {
		out = append(out, map[string]any{"name": attachment.Name, "path": attachment.Path, "width": attachment.Width, "height": attachment.Height, "bytes": attachment.Bytes})
	}
	return out
}

func int64Field(m map[string]any, key string) int64 {
	switch value := m[key].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	case json.Number:
		n, _ := value.Int64()
		return n
	}
	return 0
}

func anyList(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}

// boundedTail keeps only the last limit bytes written (the ACP stderr law:
// diagnostics are bounded, never accumulated without limit).
type boundedTail struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (t *boundedTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > t.limit {
		t.data = append([]byte(nil), t.data[len(t.data)-t.limit:]...)
	}
	return len(p), nil
}

func (t *boundedTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}

// forkTranscript is the compact, image-free record Workass keeps of a child.
type forkTranscript struct {
	file    *os.File
	written int
}

func newForkTranscript(path string) *forkTranscript {
	if strings.TrimSpace(path) == "" {
		return &forkTranscript{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return &forkTranscript{}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return &forkTranscript{}
	}
	return &forkTranscript{file: file}
}

func (t *forkTranscript) write(entry map[string]any) {
	if t == nil || t.file == nil || t.written > forkTranscriptMaxBytes {
		return
	}
	entry["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	n, _ := t.file.Write(append(data, '\n'))
	t.written += n
}

func (t *forkTranscript) close() {
	if t != nil && t.file != nil {
		_ = t.file.Close()
	}
}
