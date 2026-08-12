package provider

import "testing"

func TestValidateOperationIDNormalizesOnlyOuterWhitespace(t *testing.T) {
	got, err := ValidateOperationID("  agent-mcp:once  ")
	if err != nil || got != "agent-mcp:once" {
		t.Fatalf("validated operation id = %q, err=%v", got, err)
	}
}

func TestValidateOperationIDRejectsMalformedAndSecretShapedValues(t *testing.T) {
	for _, value := range []string{"", "   ", "api_key=hidden", "bearer-token", "secret-op", "bad id", "bad\nline", "-starts-with-punctuation"} {
		if got, err := ValidateOperationID(value); err == nil || got != "" {
			t.Fatalf("ValidateOperationID(%q) = %q, want rejection", value, got)
		}
	}
}
