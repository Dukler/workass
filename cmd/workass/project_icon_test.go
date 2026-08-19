package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/wire"
)

var tinyProjectPNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
}

func writeProjectIconFixture(t *testing.T, root, relativePath string, data []byte) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir project icon fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write project icon fixture: %v", err)
	}
	return path
}

func TestProjectIconPrefersConfiguredPathAndReturnsNoFilesystemPath(t *testing.T) {
	root := t.TempDir()
	configured := append(append([]byte{}, tinyProjectPNG...), []byte("configured")...)
	writeProjectIconFixture(t, root, "public/favicon.png", append(append([]byte{}, tinyProjectPNG...), []byte("fallback")...))
	writeProjectIconFixture(t, root, "brand/project.png", configured)
	if err := os.WriteFile(filepath.Join(root, "t3.json"), []byte(`{"iconPath":"brand/project.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	result := projectIconForWorkspace(root)
	if !result.Found || result.MIMEType != "image/png" {
		t.Fatalf("project icon result = %#v", result)
	}
	decoded, err := base64.StdEncoding.DecodeString(result.Base64)
	if err != nil {
		t.Fatalf("decode project icon: %v", err)
	}
	if string(decoded) != string(configured) {
		t.Fatalf("configured icon did not win: %q", decoded)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), root) || strings.Contains(string(encoded), "project.png") {
		t.Fatalf("daemon-local path escaped in icon reply: %s", encoded)
	}
}

func TestProjectIconReadsDeclaredRootRelativeSVG(t *testing.T) {
	root := t.TempDir()
	writeProjectIconFixture(t, root, "public/assets/brand.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h8v8H0z"/></svg>`))
	writeProjectIconFixture(t, root, "index.html", []byte(`<link href="/assets/brand.svg?v=2" sizes="any" rel="shortcut icon">`))

	result := projectIconForWorkspace(root)
	if !result.Found || result.MIMEType != "image/svg+xml" {
		t.Fatalf("declared project SVG = %#v", result)
	}
}

func TestProjectIconRejectsUnsafeCandidatesAndFallsThrough(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeProjectIconFixture(t, outside, "outside.png", tinyProjectPNG)
	if err := os.Symlink(filepath.Join(outside, "outside.png"), filepath.Join(root, "favicon.png")); err != nil {
		t.Fatalf("symlink fixture: %v", err)
	}
	writeProjectIconFixture(t, root, "favicon.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	writeProjectIconFixture(t, root, "assets/icon.png", tinyProjectPNG)
	if err := os.WriteFile(filepath.Join(root, "t3.json"), []byte(`{"iconPath":"../outside.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	result := projectIconForWorkspace(root)
	if !result.Found || result.MIMEType != "image/png" {
		t.Fatalf("safe fallback icon = %#v", result)
	}
	decoded, err := base64.StdEncoding.DecodeString(result.Base64)
	if err != nil || string(decoded) != string(tinyProjectPNG) {
		t.Fatalf("safe fallback bytes = %x, %v", decoded, err)
	}
}

func TestProjectIconWireReadDegradesToMissing(t *testing.T) {
	root := t.TempDir()
	hub := wire.NewHub()
	registerDaemonHandlers(hub, root, nil)

	raw, err := hub.Invoke("project:icon", []any{map[string]any{"chatId": "chat-1", "cwd": root}})
	if err != nil {
		t.Fatalf("project:icon invoke: %v", err)
	}
	result, ok := raw.(projectIconResult)
	if !ok || result.Found || result.MIMEType != "" || result.Base64 != "" {
		t.Fatalf("missing project icon reply = %#v", raw)
	}
}
