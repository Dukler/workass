package provider

import (
	"strings"
	"testing"
)

func TestDeriveJobIDIsStableAndActorScoped(t *testing.T) {
	first := DeriveJobID("chat-a", "operation-a")
	if first == "" || first != DeriveJobID(" chat-a ", " operation-a ") {
		t.Fatalf("job id is not stable: %q", first)
	}
	if first == DeriveJobID("chat-b", "operation-a") {
		t.Fatal("job id did not include immutable chat identity")
	}
	if first == DeriveJobID("chat-a", "operation-b") {
		t.Fatal("job id did not include immutable operation identity")
	}
	if !strings.HasPrefix(first, "app-chat-") || len(first) != len("app-chat-")+32 {
		t.Fatalf("job id has unexpected length: %q", first)
	}
	if DeriveJobID("", "operation-a") != "" || DeriveJobID("chat-a", "") != "" {
		t.Fatal("incomplete actor identity produced a public job id")
	}
	if DeriveJobID(strings.Repeat("c", 257), "operation-a") != "" ||
		DeriveJobID("chat-a", OperationID(strings.Repeat("o", 257))) != "" {
		t.Fatal("unbounded actor identity produced a public job id")
	}
	if DeriveJobID("chat with spaces", "operation-a") != "" || DeriveJobID("chat-a", "bearer:credential") != "" {
		t.Fatal("invalid or secret-shaped actor identity produced a public job id")
	}
}

func TestValidateOperationIDPreservesSafeIdentityAndRejectsUnsafeInput(t *testing.T) {
	got, err := ValidateOperationID("  turn:abc-123_4  ")
	if err != nil || got != "turn:abc-123_4" {
		t.Fatalf("valid operation = %q, %v", got, err)
	}
	for name, raw := range map[string]string{
		"empty":    "  ",
		"too-long": strings.Repeat("a", 257),
		"secret":   "bearer:credential",
		"invalid":  "turn with spaces",
		"prefix":   ":turn",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateOperationID(raw); err == nil {
				t.Fatalf("unsafe operation %q was accepted", raw)
			}
		})
	}
}
