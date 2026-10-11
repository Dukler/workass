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
	// Shapes without a keyword (2026-10-11 QA): vendor tokens, Basic auth, URL
	// credentials, PEM keys; plus private_key and passwd. Negatives stay intact.
	{"{\"private_key\":\"pk-123\"} passwd=hunter3", "{\"private_key\":[redacted]} passwd=[redacted]"},
	{"Authorization: Basic dXNlcjpwYXNz rest", "Authorization: Basic [redacted] rest"},
	{"postgres://admin:S3cretPw@db.local:5432/app and https://example.com/x", "postgres://admin:[redacted]@db.local:5432/app and https://example.com/x"},
	{"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\n-----END PRIVATE KEY----- after", "[redacted] after"},
	{"key sk-ant-api03-abcdefghijklmnopqrstuvwx end", "key [redacted] end"},
	{"ghp_abcdefghijklmnopqrstuvwxyz0123456789 and AKIAABCDEFGHIJKLMNOP", "[redacted] and [redacted]"},
	{"xoxb-1234567890-abcdefghij jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnop", "[redacted] jwt [redacted]"},
	{"basic understanding of https://example.com/path and email a@b.com", "basic understanding of https://example.com/path and email a@b.com"},
	{"sk-learn is a library; the github repo; eyJ alone", "sk-learn is a library; the github repo; eyJ alone"},
	{"postgres://admin:[redacted]@db.local/app", "postgres://admin:[redacted]@db.local/app"},
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
