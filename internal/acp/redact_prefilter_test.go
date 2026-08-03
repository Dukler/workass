package acp

import (
	"math/rand"
	"strings"
	"testing"
)

// mayContainSecret is allowed to over-accept but never to under-accept: a false
// there means redactSensitiveText skips the regex entirely, so any string the
// regex would have rewritten must make it return true.
func TestPrefilterNeverSkipsARedactableString(t *testing.T) {
	corpus := []string{
		"",
		"nothing interesting here at all",
		"Authorization: Bearer sk-ABCdef0123456789",
		"authorization: bearer sk-ABCdef0123456789",
		"BEARER\tsk-tabbed-value",
		"bearer\n  sk-newline-value",
		"api_key=abc123",
		"api-key: 'quoted value'",
		"apikey   :   \"double quoted\"",
		"API_KEY = trailing;",
		"token:abc",
		"TOKEN = 12345",
		"secret=shh",
		"Secret : 'x'",
		"password=hunter2",
		"PASSWORD: correcthorse",
		"credential=xyz",
		"CREDENTIAL : abc",
		"a sentence mentioning a bear and a key but no pair",
		"keystone arch, tokenizer, secretary, passwordless",
		"tokenizer: value",
		"bear er sk-not-a-match",
		"the quick brown fox jumps over the lazy dog",
		strings.Repeat("plain body text. ", 200),
		strings.Repeat("x", 4096),
		"ünïcodé bödy with no secrets",
		"mixed BeArEr sk-case",
		"json {\"password\":\"p\"}",
		"trailing bearer ",
		"bearer",
		"key",
		"KEY=value",
	}
	// Plus randomized strings, so the property is not only checked against cases
	// chosen to pass it.
	rng := rand.New(rand.NewSource(20260725))
	alphabet := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 :=_-'\"{},;\n\t")
	fragments := []string{"bearer ", "token", "secret", "password", "credential", "api_key", "apikey", "api-key", "key", "="}
	for i := 0; i < 4000; i++ {
		var b strings.Builder
		for n := rng.Intn(40); n > 0; n-- {
			if rng.Intn(6) == 0 {
				b.WriteString(fragments[rng.Intn(len(fragments))])
				continue
			}
			b.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		corpus = append(corpus, b.String())
	}

	skipped, filtered := 0, 0
	for _, s := range corpus {
		regexResult := secretTextRE.ReplaceAllStringFunc(s, func(match string) string {
			lower := strings.ToLower(match)
			if strings.HasPrefix(lower, "bearer ") {
				return match[:strings.Index(strings.ToLower(match), "bearer ")+7] + "[redacted]"
			}
			if i := strings.IndexAny(match, ":="); i >= 0 {
				return match[:i+1] + "[redacted]"
			}
			return "[redacted]"
		})
		prefilter := mayContainSecret(s)
		if !prefilter {
			skipped++
			if regexResult != s {
				t.Fatalf("prefilter skipped a string the regex rewrites\ninput: %q\nregex: %q", s, regexResult)
			}
		} else {
			filtered++
		}
		// And the shipped function must equal the unfiltered regex either way.
		if got := redactSensitiveText(s); got != regexResult {
			t.Fatalf("redactSensitiveText diverged from the raw regex\ninput: %q\nwant:  %q\ngot:   %q", s, regexResult, got)
		}
	}
	if skipped == 0 {
		t.Fatal("prefilter never skipped anything; the fast path is not being exercised")
	}
	t.Logf("prefilter skipped %d strings, ran the regex on %d", skipped, filtered)
}

func TestPrefilterFoldsASCIICaseOnly(t *testing.T) {
	for _, s := range []string{"bearer", "BEARER", "BeArEr", "token", "TOKEN", "kEy", "SECRET", "Password", "CREDENTIAL"} {
		if !mayContainSecret(s) {
			t.Fatalf("trigger word %q was not detected", s)
		}
	}
	// Bytes that would collide if the fold were applied blindly to non-letters.
	for _, s := range []string{"\x02\x14\x13\x10\x03", "BEAR\x00ER", "!@#$%^&*()", "\x42\x00"} {
		if mayContainSecret(s) && !strings.ContainsAny(strings.ToLower(s), "btspck") {
			t.Fatalf("non-letter bytes matched a trigger: %q", s)
		}
	}
}

func BenchmarkRedactSensitiveTextCleanBody(b *testing.B) {
	body := strings.Repeat("ordinary assistant output with no credentials in it. ", 4000)
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if got := redactSensitiveText(body); len(got) != len(body) {
			b.Fatal("unexpected rewrite")
		}
	}
}
