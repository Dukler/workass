package acp

import (
	"errors"
	"fmt"
	"testing"

	providercontract "workass/internal/provider"
)

func TestExactAttachmentAbsenceIsProviderOwned(t *testing.T) {
	for _, providerID := range append(append([]string{}, providerRegistrationOrder...), "descriptor-only-dummy") {
		t.Run(providerID, func(t *testing.T) {
			policy := providerAdapterForID(providerID).context
			for _, test := range []struct {
				name    string
				method  exactSessionAttachmentMethod
				code    int
				message string
				missing bool
			}{
				{"exact Devin load absence", exactSessionLoad, -32016, "Session not found", providerID == "devin"},
				{"resume is not load", exactSessionResume, -32016, "Session not found", false},
				{"control is not load", "session/set_config_option", -32016, "Session not found", false},
				{"resource not found", exactSessionLoad, -32002, "Resource not found", false},
				{"wrong code matching message", exactSessionLoad, -32002, "Session not found", false},
				{"wrong message matching code", exactSessionLoad, -32016, "Resource not found", false},
				{"message suffix", exactSessionLoad, -32016, "Session not found in workspace", false},
				{"message case", exactSessionLoad, -32016, "session not found", false},
				{"message whitespace", exactSessionLoad, -32016, "Session not found ", false},
				{"existing absence code", exactSessionLoad, -32044, "Unknown persistent mock ACP session.", true},
			} {
				t.Run(test.name, func(t *testing.T) {
					rpcErr := &acpError{Code: test.code, Msg: test.message, Data: map[string]any{"fixture": true}}
					original := fmt.Errorf("attachment: %w", rpcErr)
					got := policy.ExactAttachmentError(test.method, original)
					want := providercontract.ErrorTransientTransport
					if test.missing {
						want = providercontract.ErrorNativeThreadMissing
					}
					if !providercontract.ErrorIs(got, want) {
						t.Fatalf("classification = %v, want %s", got, want)
					}
					var retained *acpError
					if !errors.Is(got, original) || !errors.As(got, &retained) || retained != rpcErr {
						t.Fatal("classification discarded the original RPC error")
					}
				})
			}
			if got := policy.ExactAttachmentError(exactSessionLoad, nil); got != nil {
				t.Fatalf("nil error = %v", got)
			}
			plain := errors.New("Session not found")
			if got := policy.ExactAttachmentError(exactSessionLoad, plain); !providercontract.ErrorIs(got, providercontract.ErrorTransientTransport) {
				t.Fatalf("unstructured error became absence: %v", got)
			}
			typed := &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Cause: &acpError{Code: -32016, Msg: "Session not found"}}
			if got := policy.ExactAttachmentError(exactSessionLoad, typed); got != typed {
				t.Fatalf("existing typed error was reclassified: %v", got)
			}
		})
	}
}

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
