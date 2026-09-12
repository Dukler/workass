package acp

import (
	"encoding/json"
	"testing"

	providercontract "workass/internal/provider"
)

func bridgeWithDeliveryCapabilities(names ...string) *Bridge {
	meta := make(map[string]any, len(names))
	for _, name := range names {
		meta[name] = true
	}
	return &Bridge{agentMeta: meta}
}

func TestDeliveryStrategyProjectsNegotiatedSteerSemantics(t *testing.T) {
	tests := []struct {
		name         string
		strategy     providerDeliveryStrategy
		bridge       *Bridge
		live         bool
		steerReceipt bool
	}{
		{
			name: "standard live admission has no later steer receipt", strategy: genericACPDeliveryStrategy{},
			bridge: bridgeWithDeliveryCapabilities("sessionSteer"), live: true,
		},
		{
			name: "versioned receipt strategy exposes its later boundary", strategy: codexDeliveryStrategy{},
			bridge: bridgeWithDeliveryCapabilities("workassCodexSteerRequest", "workassCodexSteerReceipt"),
			live:   true, steerReceipt: true,
		},
		{
			name: "request without receipt stays admission-bound", strategy: claudeDeliveryStrategy{},
			bridge: bridgeWithDeliveryCapabilities("workassClaudeSteerRequest"), live: true,
		},
		{
			name: "selected native strategy keeps its versioned receipt semantics", strategy: codexDeliveryStrategy{},
			bridge: bridgeWithDeliveryCapabilities("sessionSteer", "workassCodexSteerRequest", "workassCodexSteerReceipt"),
			live:   true, steerReceipt: true,
		},
		{
			name: "selected generic strategy keeps standard ACP semantics on the same handshake", strategy: genericACPDeliveryStrategy{},
			bridge: bridgeWithDeliveryCapabilities("sessionSteer", "workassCodexSteerRequest", "workassCodexSteerReceipt"),
			live:   true,
		},
		{
			name: "selected native strategy never inherits generic ACP steering", strategy: codexDeliveryStrategy{},
			bridge: bridgeWithDeliveryCapabilities("sessionSteer"),
		},
		{name: "missing handshake is unsupported", strategy: genericACPDeliveryStrategy{}, bridge: bridgeWithDeliveryCapabilities()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capabilities := test.strategy.Capabilities(test.bridge)
			if capabilities.LiveSteer != test.live || capabilities.SteerConsumptionReceipt != test.steerReceipt {
				t.Fatalf("capabilities = %#v, want live=%v steerReceipt=%v", capabilities, test.live, test.steerReceipt)
			}
		})
	}
}

func TestSessionDeliveryCapabilitiesUseTypedCamelCaseWireShape(t *testing.T) {
	capabilities := providercontract.DeliveryCapabilities{
		StableInputIdentity:     true,
		LiveSteer:               true,
		SteerConsumptionReceipt: true,
		ConsumptionReceipt:      true,
	}
	raw, err := json.Marshal(SessionInfo{
		PlanUsageSupported: true, PlanUsageResetSupported: true,
		DeliveryCapabilities: DeliveryCapabilitiesForWire(capabilities),
	})
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	delivery := mapFromAny(projected["deliveryCapabilities"])
	if projected["planUsageSupported"] != true || projected["planUsageResetSupported"] != true {
		t.Fatalf("typed plan usage capabilities = %#v", projected)
	}
	for _, field := range []string{"stableInputIdentity", "liveSteer", "steerConsumptionReceipt", "consumptionReceipt"} {
		if delivery[field] != true {
			t.Fatalf("deliveryCapabilities.%s = %#v, full=%#v", field, delivery[field], delivery)
		}
	}
	if delivery["turnReadback"] != false {
		t.Fatalf("retired deliveryCapabilities.turnReadback = %#v, full=%#v", delivery["turnReadback"], delivery)
	}
	if _, leaked := delivery["LiveSteer"]; leaked {
		t.Fatalf("wire projection leaked actor-storage field names: %#v", delivery)
	}
}

// Structural capability evidence from the official 3000.10.21 bundle,
// initialized without credentials in an isolated XDG profile on 2026-09-11.
// The vendor's other ACP extensions must not be mistaken for live steering.
func TestDevinObservedHandshakeDoesNotAdvertiseLiveSteering(t *testing.T) {
	bridge := &Bridge{providerID: "devin", agentCaps: map[string]any{
		"loadSession": true,
		"_meta":       map[string]any{"cognition.ai/userEdits": true, "cognition.ai/userShellCommand": true, "cognition.ai/chains": true},
	}, agentMeta: map[string]any{"mcpConfigPath": "/fixture/mcp_config.json"}}
	strategy := providerAdapterForID("devin").delivery
	if strategy.Capabilities(bridge).LiveSteer {
		t.Fatal("unadvertised Devin live steering was enabled")
	}
	outcome := strategy.Steer(bridge, providerSteerRequest{sessionID: "fixture", clientUserMessageID: "direction"})
	if outcome.ok || outcome.live || outcome.queued || !outcome.unsupported {
		t.Fatalf("unsupported direction was misreported or queued: %#v", outcome)
	}
}
