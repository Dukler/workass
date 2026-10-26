//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

// Every spelling APFS itself resolves to ~/Desktop must be refused by the
// guard: brute force over the BMP, each rune substituted for each letter of
// "Desktop" and inserted into it, asking the volume (not a model of it) which
// spellings open the real folder.
func TestAcceptGuardRefusesEverySpellingAPFSResolvesToDesktop(t *testing.T) {
	if testing.Short() {
		t.Skip("brute force")
	}
	home := filepath.Join(t.TempDir(), "home")
	must(t, os.MkdirAll(filepath.Join(home, "Desktop"), 0o700))
	must(t, os.WriteFile(filepath.Join(home, "Desktop", "probe"), []byte("x"), 0o600))
	guard := sessionImageSourceGuard{home: home, caseInsensitive: true}
	base := "Desktop"
	aliases, refused := 0, 0
	var leaks []string
	try := func(name string) {
		path := filepath.Join(home, name, "probe")
		if _, err := os.Lstat(path); err != nil {
			return
		}
		aliases++
		if guard.check(path) != nil {
			refused++
			return
		}
		if len(leaks) < 20 {
			leaks = append(leaks, name)
		}
	}
	for r := rune(0x80); r <= 0xFFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF || !utf8.ValidRune(r) {
			continue
		}
		s := string(r)
		try("Des" + s + "ktop")
		for i := 0; i < len(base); i++ {
			try(base[:i] + s + base[i+1:])
		}
	}
	t.Logf("%d non-ASCII spellings open ~/Desktop on this volume; the guard refused %d", aliases, refused)
	if len(leaks) > 0 {
		t.Fatalf("the guard allows spellings APFS opens as ~/Desktop: %q", leaks)
	}
}
