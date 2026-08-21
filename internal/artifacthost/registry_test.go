package artifacthost

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRegisterStandaloneArtifactsReturnsStableLiveURLs(t *testing.T) {
	stateDir := t.TempDir()
	workspace := t.TempDir()
	registry, err := New(stateDir, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry)
	defer server.Close()

	for _, fixture := range []struct {
		name        string
		body        string
		contentType string
	}{
		{name: "review.md", body: "# Review one", contentType: "text/markdown"},
		{name: "table.csv", body: "name,value\nalpha,1\n", contentType: "text/csv"},
		{name: "report.pdf", body: "%PDF-1.4\nworkass artifact\n", contentType: "application/pdf"},
	} {
		source := filepath.Join(workspace, fixture.name)
		if err := os.WriteFile(source, []byte(fixture.body), 0o600); err != nil {
			t.Fatal(err)
		}
		hosted, err := registry.Register(RegisterOptions{BaseDir: workspace, SourcePath: fixture.name})
		if err != nil {
			t.Fatalf("register %s: %v", fixture.name, err)
		}
		if hosted.URLPath == "" || !strings.HasPrefix(hosted.URLPath, PathPrefix+"/") || hosted.Entry != fixture.name {
			t.Fatalf("registration for %s = %#v", fixture.name, hosted)
		}
		if hosted.ContentType == "" || hosted.Markdown == "" || !strings.Contains(hosted.Markdown, hosted.URLPath) {
			t.Fatalf("agent-facing artifact metadata for %s = %#v", fixture.name, hosted)
		}
		repeated, err := registry.Register(RegisterOptions{BaseDir: workspace, SourcePath: fixture.name})
		if err != nil || repeated.ID != hosted.ID || repeated.URLPath != hosted.URLPath {
			t.Fatalf("repeat registration for %s = %#v err=%v, want stable id %q", fixture.name, repeated, err, hosted.ID)
		}
		response, err := http.Get(server.URL + hosted.URLPath)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || string(body) != fixture.body || !strings.HasPrefix(response.Header.Get("Content-Type"), fixture.contentType) {
			t.Fatalf("serve %s: status=%d type=%q body=%q", fixture.name, response.StatusCode, response.Header.Get("Content-Type"), body)
		}
	}
}

func TestArtifactHostServesLiveEditsByteRangesAndSurvivesReload(t *testing.T) {
	stateDir := t.TempDir()
	workspace := t.TempDir()
	source := filepath.Join(workspace, "capture.png")
	first := []byte("\x89PNG\r\n\x1a\nfirst-image-payload")
	if err := os.WriteFile(source, first, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := New(stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := registry.Register(RegisterOptions{BaseDir: workspace, SourcePath: source, Label: "Capture"})
	if err != nil {
		t.Fatal(err)
	}
	if hosted.Markdown != "![capture]("+hosted.URLPath+")" {
		t.Fatalf("image markdown = %q", hosted.Markdown)
	}
	updated := []byte("\x89PNG\r\n\x1a\nsecond-image-payload")
	if err := os.WriteFile(source, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://workass.test"+hosted.URLPath, nil)
	request.Header.Set("Range", "bytes=8-13")
	recorder := httptest.NewRecorder()
	reloaded.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "second" {
		t.Fatalf("range after reload = status %d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("accept-ranges = %q", got)
	}
}

func TestRegisterArtifactDirectoryAcceptsNonHTMLEntryAndServesAssets(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "bundle")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "report.pdf"), []byte("%PDF-1.4\nbundle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "notes.md"), []byte("# Notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := registry.Register(RegisterOptions{BaseDir: workspace, SourcePath: "bundle", Entry: "report.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry)
	defer server.Close()
	for _, suffix := range []string{"", "notes.md"} {
		response, err := http.Get(server.URL + hosted.URLPath + suffix)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("artifact suffix %q status=%d", suffix, response.StatusCode)
		}
	}
}

func TestArtifactHostRejectsOutsideWorkspaceAndSecretNamedFiles(t *testing.T) {
	workspace := t.TempDir()
	registry, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.pdf")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(RegisterOptions{BaseDir: workspace, SourcePath: outside}); err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("outside artifact error = %v", err)
	}
	secret := filepath.Join(workspace, "api-token.txt")
	if err := os.WriteFile(secret, []byte("must stay private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(RegisterOptions{BaseDir: workspace, SourcePath: secret}); err == nil {
		t.Fatal("secret-shaped artifact name was accepted")
	}
	secretDirectory := filepath.Join(workspace, "credentials-export")
	if err := os.MkdirAll(secretDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDirectory, "index.html"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(RegisterOptions{BaseDir: workspace, SourcePath: secretDirectory}); err == nil {
		t.Fatal("secret-shaped artifact directory was accepted")
	}
}

func TestRegisterCapturedHTMLIsDurableAndBounded(t *testing.T) {
	stateDir := t.TempDir()
	registry, err := New(stateDir, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	original := []byte("<section id=visual>captured</section>")
	hosted, err := registry.RegisterCapturedHTML("Captured view", original)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if hosted.LocalURL == "" || strings.Contains(hosted.URLPath, "/Users/") {
		t.Fatalf("captured registration leaked a local path: %#v", hosted)
	}

	response := httptest.NewRecorder()
	registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, hosted.URLPath+hosted.Entry, nil))
	if response.Code != http.StatusOK || response.Body.String() != string(original) {
		t.Fatalf("captured response status=%d body=%q", response.Code, response.Body.String())
	}

	reloaded, err := New(stateDir, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	reloaded.ServeHTTP(response, httptest.NewRequest(http.MethodGet, hosted.URLPath+hosted.Entry, nil))
	if response.Code != http.StatusOK || response.Body.String() != string(original) {
		t.Fatalf("reloaded captured response status=%d body=%q", response.Code, response.Body.String())
	}

	if _, err := registry.RegisterCapturedHTML("too large", make([]byte, maxCapturedHTMLBytes+1)); err == nil {
		t.Fatal("oversized captured HTML was accepted")
	}
}

// A design mock is most of what artifact hosting is for, and design vocabulary
// collides with the redaction word list constantly. Reported 2026-07-26: an
// index.html served 200 while its _tokens.css answered 403, so the page
// rendered with every custom property undefined and nothing said why.
func TestArtifactHostPublishesDesignVocabularyAndWithholdsCredentialShapes(t *testing.T) {
	workspace := t.TempDir()
	mocks := filepath.Join(workspace, "mocks")
	if err := os.MkdirAll(mocks, 0o700); err != nil {
		t.Fatal(err)
	}
	published := map[string]string{
		"index.html":          "<link rel=stylesheet href=_tokens.css>",
		"_tokens.css":         ":root{--bg:#fff}",
		"design-tokens.json":  `{"color":{"bg":"#fff"}}`,
		"password-reset.html": "<h1>reset</h1>",
		"credential-flow.svg": "<svg/>",
		"secrets-policy.md":   "# how we store secrets",
		"bearer-auth.md":      "# bearer auth",
	}
	for name, body := range published {
		if err := os.WriteFile(filepath.Join(mocks, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withheld := map[string]string{
		"credentials.json": `{"token":"live"}`,
		"api-token.txt":    "live",
		"apiToken.json":    `{"t":"live"}`,
		"deploy.pem":       "-----BEGIN PRIVATE KEY-----",
		"tokenizer.ts":     "export const tokenize = () => {}",
	}
	for name, body := range withheld {
		if err := os.WriteFile(filepath.Join(mocks, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(mocks, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mocks, "secrets", "note.txt"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}

	registry, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	logged := make([]string, 0, 8)
	registry.SetLogger(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	hosted, err := registry.Register(RegisterOptions{BaseDir: workspace, SourcePath: "mocks"})
	if err != nil {
		t.Fatalf("register design mocks: %v", err)
	}
	server := httptest.NewServer(registry)
	defer server.Close()

	for name, body := range published {
		response, err := http.Get(server.URL + hosted.URLPath + name)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || string(got) != body {
			t.Fatalf("%s: status=%d body=%q, want the file served", name, response.StatusCode, got)
		}
	}
	for name := range withheld {
		response, err := http.Get(server.URL + hosted.URLPath + name)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status=%d, want 403", name, response.StatusCode)
		}
		// A refusal that cannot say why is the defect, not the 403 itself.
		if response.Header.Get("X-Workass-Withheld") == "" || !strings.Contains(string(got), "withheld: ") {
			t.Fatalf("%s: header=%q body=%q, want a stated reason", name, response.Header.Get("X-Workass-Withheld"), got)
		}
	}
	nested, err := http.Get(server.URL + hosted.URLPath + "secrets/note.txt")
	if err != nil {
		t.Fatal(err)
	}
	nested.Body.Close()
	if nested.StatusCode != http.StatusForbidden {
		t.Fatalf("secrets/note.txt: status=%d, want 403", nested.StatusCode)
	}

	// The receipt names what will not serve, at host time, before a page is
	// ever loaded — the round trip that finding this cost.
	reported := make(map[string]string, len(hosted.Withheld))
	for _, asset := range hosted.Withheld {
		reported[asset.Path] = asset.Reason
	}
	for _, want := range []string{"credentials.json", "api-token.txt", "apiToken.json", "deploy.pem", "tokenizer.ts", "secrets/"} {
		if reported[want] == "" {
			t.Fatalf("withheld receipt %#v, want it to name %q", hosted.Withheld, want)
		}
	}
	for name := range published {
		if _, listed := reported[name]; listed {
			t.Fatalf("%s was reported as withheld but it is published", name)
		}
	}
	if len(logged) == 0 {
		t.Fatal("nothing reached the daemon log; silence is what made this cost a round trip")
	}
}

func TestOperationReadbackPreservesExactWithheldRegistrationAcrossRestart(t *testing.T) {
	stateDir := t.TempDir()
	workspace := t.TempDir()
	site := filepath.Join(workspace, "site")
	if err := os.MkdirAll(site, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>safe</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxWithheldReported+5; index++ {
		name := fmt.Sprintf("credential-%02d.json", index)
		if err := os.WriteFile(filepath.Join(site, name), []byte(`{"secret":"not-public"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	registry, err := New(stateDir, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	operationID := "artifact-withheld-parity"
	digest := strings.Repeat("a", sha256.Size*2)
	first, err := registry.RegisterForOperation(RegisterOptions{
		BaseDir: workspace, SourcePath: "site", Entry: "index.html", Label: "Site",
	}, operationID, digest)
	if err != nil {
		t.Fatalf("first operation-backed registration: %v", err)
	}
	if len(first.Withheld) != maxWithheldReported || first.WithheldMore != 5 {
		t.Fatalf("first withheld projection = %d entries, more=%d; want %d and 5", len(first.Withheld), first.WithheldMore, maxWithheldReported)
	}

	restarted, err := New(stateDir, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	readback, found, err := restarted.ReadOperation(operationID, digest)
	if err != nil {
		t.Fatalf("restart readback: %v", err)
	}
	if !found {
		t.Fatal("restart readback did not find the operation receipt")
	}
	if !reflect.DeepEqual(readback, first) {
		t.Fatalf("restart readback = %#v, first result = %#v", readback, first)
	}
}

func TestOperationCapturedHTMLReadbackPreservesFullRegistrationAcrossRestart(t *testing.T) {
	stateDir := t.TempDir()
	content := []byte(`<html><body>captured</body></html>`)
	digest := strings.Repeat("b", sha256.Size*2)
	registry, err := New(stateDir, "http://first-origin.invalid:8788")
	if err != nil {
		t.Fatal(err)
	}
	first, err := registry.RegisterCapturedHTMLForOperation("Captured", content, "visualize-operation", digest)
	if err != nil {
		t.Fatalf("first captured registration: %v", err)
	}
	if first.LocalURL == "" {
		t.Fatalf("first captured registration lost LocalURL: %#v", first)
	}

	restarted, err := New(stateDir, "http://different-origin.invalid:8788")
	if err != nil {
		t.Fatal(err)
	}
	readback, found, err := restarted.ReadOperation("visualize-operation", digest)
	if err != nil {
		t.Fatalf("captured HTML restart readback: %v", err)
	}
	if !found {
		t.Fatal("captured HTML operation receipt was not found after restart")
	}
	if !reflect.DeepEqual(readback, first) {
		t.Fatalf("captured HTML restart readback = %#v, first result = %#v", readback, first)
	}
}

func TestArtifactHostRejectsSymlinkEscapesHiddenFilesAndMutatingMethods(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "site")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte("<h1>safe</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("SECRET=value"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	registry, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := registry.Register(RegisterOptions{BaseDir: workspace, SourcePath: directory})
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".env", "escape.txt", "../index.html", "%2e%2e/index.html"} {
		request := httptest.NewRequest(http.MethodGet, "http://workass.test"+hosted.URLPath+suffix, nil)
		recorder := httptest.NewRecorder()
		registry.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusOK || strings.Contains(recorder.Body.String(), "outside") || strings.Contains(recorder.Body.String(), "SECRET=value") {
			t.Fatalf("unsafe artifact %q escaped: status=%d body=%q", suffix, recorder.Code, recorder.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodPost, "http://workass.test"+hosted.URLPath, strings.NewReader("replace"))
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("mutating request = status %d allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}
	for name, want := range map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := recorder.Header().Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Fatalf("content-security-policy = %q", got)
	}
}
