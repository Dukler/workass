package acp

import "testing"

func TestExactSessionAttachmentCapabilityMatrix(t *testing.T) {
	tests := []struct {
		name string
		caps map[string]any
		want exactSessionAttachmentMethod
		ok   bool
	}{
		{
			name: "resume",
			caps: map[string]any{
				"sessionCapabilities": map[string]any{"resume": map[string]any{}},
			},
			want: exactSessionResume,
			ok:   true,
		},
		{name: "load", caps: map[string]any{"loadSession": true}, want: exactSessionLoad, ok: true},
		{
			name: "both prefer resume",
			caps: map[string]any{
				"sessionCapabilities": map[string]any{"resume": map[string]any{}},
				"loadSession":         true,
			},
			want: exactSessionResume,
			ok:   true,
		},
		{
			name: "neither",
			caps: map[string]any{
				"sessionCapabilities": map[string]any{"close": map[string]any{}},
				"loadSession":         false,
			},
		},
		{
			name: "malformed resume",
			caps: map[string]any{
				"sessionCapabilities": map[string]any{"resume": true},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge := &Bridge{agentCaps: test.caps}
			got, ok := bridge.exactSessionAttachment()
			if ok != test.ok || got.method != test.want {
				t.Fatalf("exact attachment = method:%q ok:%v, want method:%q ok:%v", got.method, ok, test.want, test.ok)
			}
		})
	}
}

func TestGenericACPCreationBoundaryComesFromNegotiatedAttachment(t *testing.T) {
	tests := []struct {
		name     string
		caps     map[string]any
		deferred bool
	}{
		{
			name: "resume session is durable",
			caps: map[string]any{
				"sessionCapabilities": map[string]any{"resume": map[string]any{}},
			},
		},
		{name: "load session waits for activity", caps: map[string]any{"loadSession": true}, deferred: true},
		{name: "resume wins when both exist", caps: map[string]any{
			"sessionCapabilities": map[string]any{"resume": map[string]any{}}, "loadSession": true,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge := &Bridge{agentCaps: test.caps}
			got := genericACPProviderAdapter.negotiatedCreationCapabilities(bridge)
			if got.DeferredUntilInput != test.deferred {
				t.Fatalf("deferred creation = %v, want %v", got.DeferredUntilInput, test.deferred)
			}
		})
	}
}
