//go:build windows

package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

// This opt-in native protocol canary owns a temporary Workass state directory
// and its own Devin process. The deliberately held actor is a fixture; the
// prompt, tool notification, cancellation and terminal response are real ACP.
func TestRealDevinCancelWhileAnotherChatCommitWaits(t *testing.T) {
	if os.Getenv("WORKASS_REAL_DEVIN") != "1" {
		t.Skip("set WORKASS_REAL_DEVIN=1 on a Windows Devin installation")
	}
	devin := strings.TrimSpace(os.Getenv("WORKASS_REAL_DEVIN_BIN"))
	if info, err := os.Stat(devin); err != nil || info.IsDir() {
		t.Fatal("WORKASS_REAL_DEVIN_BIN must name the installed Devin executable")
	}
	root := t.TempDir()
	toolStarted := make(chan struct{}, 1)
	ended := make(chan map[string]any, 1)
	manager := NewManager(Options{
		RootDir: root, StateDir: filepath.Join(root, "state"),
		Providers:         []ProviderConfig{{ID: "devin", Name: "Devin ACP", Command: devin, Args: []string{"acp"}, Enabled: true}},
		DefaultProviderID: "devin", InitTimeout: 120 * time.Second, RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, raw any) {
			if channel != "job:event" {
				return
			}
			payload := mapFromAny(raw)
			if asString(payload["type"]) == "acp" && asString(mapFromAny(payload["event"])["kind"]) == "tool" {
				select {
				case toolStarted <- struct{}{}:
				default:
				}
			}
			if asString(payload["type"]) == "end" {
				select {
				case ended <- mapFromAny(payload["job"]):
				default:
				}
			}
		},
	})
	defer manager.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "canary-tab", ChatID: "canary-chat", ProviderID: "devin"})
	if err != nil {
		t.Fatal("native canary session creation failed")
	}
	identity := providercontract.LaneIdentity{
		ChatID: "canary-chat", WorkspaceEpoch: "canary-workspace",
		Realm: providercontract.Realm{ProviderID: "devin", MachineID: "canary-machine", AccountScope: "canary", InstallScope: "canary"},
	}.Normalize()
	native := newManagerLane(manager, identity, providercontract.AttachmentOwner{TabID: "canary-tab"}, session,
		providercontract.ThreadRef{ProviderID: "devin", RootID: session.SessionID, HeadID: session.SessionID, Lineage: 1})
	slow := newUnopenedManagerLaneForTest(t, manager, "blocked-canary-chat", "blocked-canary-session")
	manager.bindProviderLaneJob(slow, "blocked-canary-job", "blocked-canary-operation")
	blocked, release, slowDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var forwarders sync.WaitGroup
	for _, lane := range []*managerLane{native, slow} {
		<-lane.Events()
		forwarders.Add(1)
		go func(lane *managerLane) {
			defer forwarders.Done()
			for event := range lane.Events() {
				if lane == slow && event.Kind == providercontract.EventAssistantChunk {
					close(blocked)
					<-release
				}
				lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
			}
		}(lane)
	}
	blockedStarted := false
	defer func() {
		close(release)
		if blockedStarted {
			<-slowDone
		}
		native.attachmentClosed()
		slow.attachmentClosed()
		forwarders.Wait()
	}()
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", TabID: "canary-tab", ChatID: "canary-chat", SessionID: session.SessionID, ProviderID: "devin",
		ModelID: "claude-opus-4-8-high", ModeID: "bypass", PermissionMode: "bypass",
		Prompt: "Cancellation diagnostic only. Immediately run PowerShell: Start-Sleep -Seconds 60. Do not read files, change files, or do other work. The client will cancel this turn.",
	})
	if err != nil {
		t.Fatal("native canary turn admission failed")
	}
	select {
	case <-toolStarted:
	case <-ended:
		t.Fatal("native turn ended before its tool notification")
	case <-ctx.Done():
		t.Fatal("native tool notification deadline exceeded")
	}
	blockedStarted = true
	go func() {
		defer close(slowDone)
		manager.emit("job:event", map[string]any{"type": "data", "id": "blocked-canary-job", "stream": "stdout", "chunk": "fixture"})
	}()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("fixture did not reach blocked actor commit")
	}
	start := time.Now()
	result := manager.CancelJobResult(jobID(job))
	dispatched := time.Since(start)
	if !result.Cancelled {
		t.Fatalf("cancel rejected: %s", result.Reason)
	}
	select {
	case end := <-ended:
		if asString(end["stopReason"]) != "cancelled" {
			t.Fatal("native terminal result was not cancelled")
		}
		t.Logf("real Devin cancellation: dispatch_ms=%d terminal_ms=%d unrelated_actor_still_blocked=true", dispatched.Milliseconds(), time.Since(start).Milliseconds())
	case <-time.After(20 * time.Second):
		t.Fatal("native cancelled turn did not publish while unrelated actor remained blocked")
	}
}
