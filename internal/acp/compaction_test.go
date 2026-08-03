package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderOwnsContextCompaction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		providerID string
		want       bool
	}{
		{providerID: "codex", want: true},
		{providerID: " Codex ", want: true},
		{providerID: "claude", want: true},
		{providerID: "CLAUDE", want: true},
		{providerID: "mock", want: false},
		{providerID: "custom", want: false},
		{providerID: "qwen", want: false},
		{providerID: "local-lmstudio", want: false},
		{providerID: "", want: false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.providerID, func(t *testing.T) {
			t.Parallel()
			if got := providerOwnsContextCompaction(tc.providerID); got != tc.want {
				t.Fatalf("providerOwnsContextCompaction(%q) = %v; want %v", tc.providerID, got, tc.want)
			}
		})
	}
}

func TestProviderNativeCompactionBypassesWorkassFallback(t *testing.T) {
	for _, providerID := range []string{"codex", "claude"} {
		providerID := providerID
		t.Run(providerID, func(t *testing.T) {
			root := repoRoot(t)
			traceFile := filepath.Join(t.TempDir(), "mock-prompts.jsonl")
			events := newEventCollector()
			manager := NewManager(Options{
				RootDir: root,
				Provider: ProviderConfig{
					ID:      providerID,
					Command: "node",
					Args:    []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
					CWD:     root,
					Env:     map[string]string{"WORKASS_MOCK_ACP_TRACE_FILE": traceFile},
					Enabled: true,
					Label:   "Workass Mock ACP",
				},
				DefaultProviderID:       providerID,
				Broadcast:               events.Broadcast,
				StdoutFlushInterval:     5 * time.Millisecond,
				ThoughtFlushInterval:    5 * time.Millisecond,
				RSSSampleInterval:       time.Hour,
				CompactionEnabled:       true,
				CompactionThresholdPct:  80,
				CompactionKeepLastTurns: 1,
			})
			t.Cleanup(func() { manager.Reset() })

			session := newMockSession(t, manager, "native-compact-"+providerID)
			job := startAppChatJob(t, manager, session.SessionID, "native-compact-"+providerID, "[mock:bigusage] native provider owns compaction")
			assertJobStatus(t, events.waitJobEnd(t, jobID(job), 5*time.Second), "done", 0, "end_turn")

			usage, ok := manager.usageForSession(session.SessionID)
			if !ok || usage.Used != 85 || usage.Size != 100 || usage.UsedPct != 85 {
				t.Fatalf("native provider usage was replaced: %#v ok=%v", usage, ok)
			}
			bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID, ProviderID: providerID})
			if bridge == nil || !bridge.hasLiveSession(session.SessionID) {
				t.Fatalf("native provider session was replaced: bridge=%v session=%s", bridge != nil, session.SessionID)
			}
			trace, err := os.ReadFile(traceFile)
			if err != nil {
				t.Fatalf("read mock trace: %v", err)
			}
			if strings.Contains(string(trace), compactionPromptPreamble) {
				t.Fatalf("Workass fallback compaction prompt reached native provider %q", providerID)
			}
		})
	}
}
