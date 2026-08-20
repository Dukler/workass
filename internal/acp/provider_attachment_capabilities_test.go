package acp

import "testing"

func TestLiveSessionProjectsNegotiatedPlanUsageCapabilities(t *testing.T) {
	bridge := &Bridge{
		providerID: "codex",
		agentMeta: map[string]any{
			"workassCodexRateLimitsRequest":     true,
			"workassCodexRateLimitResetRequest": true,
		},
		sessions: map[string]struct{}{"session-plan-capabilities": {}},
	}
	live, ok := bridge.liveSession("session-plan-capabilities")
	if !ok {
		t.Fatal("expected the exact live session")
	}
	if !live.Info.PlanUsageSupported || !live.Info.PlanUsageResetSupported {
		t.Fatalf("plan usage capabilities = %#v", live.Info)
	}

	bridge.mu.Lock()
	delete(bridge.agentMeta, "workassCodexRateLimitResetRequest")
	bridge.mu.Unlock()
	live, ok = bridge.liveSession("session-plan-capabilities")
	if !ok || !live.Info.PlanUsageSupported || live.Info.PlanUsageResetSupported {
		t.Fatalf("reset capability did not follow the selected adapter handshake: %#v", live.Info)
	}
}
