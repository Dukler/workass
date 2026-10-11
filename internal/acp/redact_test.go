package acp

import (
	"testing"

	"workass/internal/agentserver"
)

// Secrets in JSON shape and common env names slipped past every redactor
// (2026-10-11 reviews). The renderer's tests/redact.test.ts carries these same
// pairs as byte-parity fixtures.
var redactionCases = [][2]string{
	{`{"token":"tok-123","password":"hunter2","api_key":"k-9"}`, `{"token":[redacted],"password":[redacted],"api_key":[redacted]}`},
	{`{"password": "spaced"}`, `{"password":[redacted]}`},
	{`secret_key=abc AWS_SECRET_ACCESS_KEY=AKIAXYZ secret-key: zz`, `secret_key=[redacted] AWS_SECRET_ACCESS_KEY=[redacted] secret-key:[redacted]`},
	{`client_secret='cs-1' and accessToken: at-2`, `client_secret=[redacted] and accessToken:[redacted]`},
	{`tokens: 85 y max_tokens=4096`, `tokens: 85 y max_tokens=4096`},
	{`{"token":[redacted]}`, `{"token":[redacted]}`},
}

func TestRedactionCoversJSONKeysAndSecretKeyNames(t *testing.T) {
	for _, pair := range redactionCases {
		if got := redactSensitiveText(pair[0]); got != pair[1] {
			t.Errorf("daemon redaction of %q = %q, want %q", pair[0], got, pair[1])
		}
		if got := agentserver.RedactSensitiveText(pair[0]); got != pair[1] {
			t.Errorf("agent server redaction of %q = %q, want %q", pair[0], got, pair[1])
		}
	}
}
