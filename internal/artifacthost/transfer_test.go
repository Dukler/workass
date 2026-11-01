package artifacthost

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactTransferStreamsRangesAndCloses(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "data.pdf"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := New(state, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := reg.Register(RegisterOptions{BaseDir: workspace, SourcePath: "data.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	m := NewArtifactTransferManager(reg)
	opened, err := m.Open(context.Background(), hosted.URLPath, http.MethodGet, map[string]string{"Range": "bytes=2-5"})
	if err != nil {
		t.Fatal(err)
	}
	if opened.Status != http.StatusPartialContent || opened.Headers["Content-Range"] != "bytes 2-5/10" {
		t.Fatalf("open = %#v", opened)
	}
	var got []byte
	for {
		chunk, readErr := m.Read(context.Background(), opened.TransferID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if chunk.BodyBase64 != "" {
			b, err := base64.StdEncoding.DecodeString(chunk.BodyBase64)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, b...)
		}
		if chunk.EOF {
			break
		}
	}
	if string(got) != "2345" {
		t.Fatalf("body = %q", got)
	}
	if err := m.Close(opened.TransferID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Read(context.Background(), opened.TransferID); err == nil {
		t.Fatal("closed transfer remained readable")
	}
}

func TestArtifactTransferHTTPConditionalsHeadExpiryAndCancellation(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "page.html"), []byte("artifact body"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := New(state, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := reg.Register(RegisterOptions{BaseDir: workspace, SourcePath: "page.html"})
	if err != nil {
		t.Fatal(err)
	}
	m := NewArtifactTransferManager(reg)
	head, err := m.Open(context.Background(), hosted.URLPath, http.MethodHead, nil)
	if err != nil {
		t.Fatal(err)
	}
	if head.Status != http.StatusOK {
		t.Fatalf("HEAD status = %d", head.Status)
	}
	if got, err := m.Read(context.Background(), head.TransferID); err != nil || !got.EOF {
		t.Fatalf("HEAD read = %#v err=%v", got, err)
	}
	cond, err := m.Open(context.Background(), hosted.URLPath, http.MethodGet, map[string]string{"If-None-Match": "*"})
	if err != nil {
		t.Fatal(err)
	}
	if cond.Status != http.StatusNotModified {
		t.Fatalf("conditional status = %d", cond.Status)
	}
	_, _ = m.Read(context.Background(), cond.TransferID)
	bad, err := m.Open(context.Background(), hosted.URLPath, http.MethodGet, map[string]string{"Range": "bytes=999-1000"})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Status != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("416 status = %d", bad.Status)
	}
	_, _ = m.Read(context.Background(), bad.TransferID)
	// Force the same cleanup path used by the 30-second idle timer.
	m.mu.Lock()
	for _, tr := range m.transfers {
		tr.last = time.Now().Add(-artifactIdleTTL - time.Second)
	}
	m.mu.Unlock()
	m.expire()
	if len(m.transfers) != 0 {
		t.Fatal("expired transfers remained")
	}
}

func TestArtifactTransferRejectsUnsafePathsAndHeaders(t *testing.T) {
	reg, err := New(t.TempDir(), "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	m := NewArtifactTransferManager(reg)
	for _, path := range []string{"/workass/artifacts/x/../secret", "/workass/artifacts/x/%2e%2e/secret", "/workass/artifacts/x/%2fsecret"} {
		if _, err := m.Open(context.Background(), path, http.MethodGet, nil); err == nil {
			t.Fatalf("accepted unsafe path %q", path)
		}
	}
	if _, err := m.Open(context.Background(), "/workass/artifacts/x/file", http.MethodGet, map[string]string{"Cookie": "x"}); err == nil {
		t.Fatal("accepted unsafe header")
	}
}

func TestArtifactTransferBoundedLargeMissingWithheldCancellationAndCapacity(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	large := bytes.Repeat([]byte("artifact"), 37500)
	if err := os.WriteFile(filepath.Join(workspace, "large.pdf"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".env"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := New(state, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	largeReg, err := reg.Register(RegisterOptions{BaseDir: workspace, SourcePath: "large.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	dirReg, err := reg.Register(RegisterOptions{BaseDir: workspace, SourcePath: workspace, Entry: "index.html"})
	if err != nil {
		t.Fatal(err)
	}
	m := NewArtifactTransferManager(reg)
	opened, err := m.Open(context.Background(), largeReg.URLPath, http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		chunk, err := m.Read(context.Background(), opened.TransferID)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.BodyBase64 != "" {
			b, err := base64.StdEncoding.DecodeString(chunk.BodyBase64)
			if err != nil {
				t.Fatal(err)
			}
			if len(b) > maxArtifactChunk {
				t.Fatalf("chunk size=%d", len(b))
			}
			got = append(got, b...)
		}
		if chunk.EOF {
			break
		}
	}
	if !bytes.Equal(got, large) {
		t.Fatalf("large body length=%d want=%d", len(got), len(large))
	}
	missing, err := m.Open(context.Background(), largeReg.URLPath+"/missing.pdf", http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	if missing.Status != http.StatusNotFound {
		t.Fatalf("missing status=%d", missing.Status)
	}
	_, _ = m.Read(context.Background(), missing.TransferID)
	_ = m.Close(missing.TransferID)
	withheld, err := m.Open(context.Background(), dirReg.URLPath+"/.env", http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withheld.Status != http.StatusForbidden {
		t.Fatalf("withheld status=%d", withheld.Status)
	}
	_, _ = m.Read(context.Background(), withheld.TransferID)
	_ = m.Close(withheld.TransferID)
	cancel, err := m.Open(context.Background(), largeReg.URLPath, http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancelFn := context.WithCancel(context.Background())
	cancelFn()
	if _, err := m.Read(canceled, cancel.TransferID); err == nil {
		t.Fatal("canceled read succeeded")
	}
	if _, err := m.Read(context.Background(), cancel.TransferID); err == nil {
		t.Fatal("canceled transfer remained open")
	}
	ids := make([]string, 0, maxArtifactTransfers)
	for i := 0; i < maxArtifactTransfers; i++ {
		item, err := m.Open(context.Background(), largeReg.URLPath, http.MethodGet, nil)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		ids = append(ids, item.TransferID)
	}
	if _, err := m.Open(context.Background(), largeReg.URLPath, http.MethodGet, nil); err == nil {
		t.Fatal("33rd transfer succeeded")
	}
	if err := m.Close(ids[0]); err != nil {
		t.Fatal(err)
	}
	recovered, err := m.Open(context.Background(), largeReg.URLPath, http.MethodGet, nil)
	if err != nil {
		t.Fatalf("slot did not recover: %v", err)
	}
	_ = m.Close(recovered.TransferID)
	for _, id := range ids[1:] {
		_ = m.Close(id)
	}
}

func TestArtifactTransferResponseHeadersPreserveMetadataOnly(t *testing.T) {
	headers := http.Header{}
	headers.Set("ETag", `"fixture"`)
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("Set-Cookie", "never-forward")
	got := safeHeaders(headers)
	if got["Etag"] != `"fixture"` || got["Referrer-Policy"] != "no-referrer" || got["Set-Cookie"] != "" {
		t.Fatalf("unexpected response metadata: %#v", got)
	}
}

// A sandboxed response gives Chromium's player page an opaque origin, and that
// page requests its video in CORS mode: the request fails and the player stays
// empty. Everything else hosted keeps the sandbox.
func TestHostedMediaIsPlayableAndEverythingElseStaysSandboxed(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	body := "0123456789"
	for _, name := range []string{"clip.mp4", "clip.webm", "tone.mp3", "tone.wav", "tone.ogg", "page.html", "image.svg", "report.pdf", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := New(state, "http://127.0.0.1:8788", "m-owner")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := reg.Register(RegisterOptions{BaseDir: workspace, SourcePath: ".", Entry: "page.html"})
	if err != nil {
		t.Fatal(err)
	}
	transfers := NewArtifactTransferManager(reg)
	for name, want := range map[string]string{"clip.mp4": "video/mp4", "clip.webm": "video/webm", "tone.mp3": "audio/mpeg", "tone.wav": "audio/wav", "tone.ogg": "audio/ogg"} {
		// The same response both ways: a browser on the daemon, and the transfer
		// a machine-tagged link reaches a remote owner through.
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, hosted.URLPath+name, nil)
		request.Header.Set("Range", "bytes=2-5")
		reg.ServeHTTP(recorder, request)
		opened, err := transfers.Open(context.Background(), hosted.URLPath+name, http.MethodGet, map[string]string{"Range": "bytes=2-5"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transfers.Close(opened.TransferID) })
		for route, got := range map[string]map[string]string{
			"http":     {"Content-Type": recorder.Header().Get("Content-Type"), "Content-Security-Policy": recorder.Header().Get("Content-Security-Policy"), "Content-Range": recorder.Header().Get("Content-Range"), "Accept-Ranges": recorder.Header().Get("Accept-Ranges"), "X-Content-Type-Options": recorder.Header().Get("X-Content-Type-Options")},
			"transfer": opened.Headers,
		} {
			if got["Content-Type"] != want || got["Content-Range"] != "bytes 2-5/10" || got["Accept-Ranges"] != "bytes" || got["X-Content-Type-Options"] != "nosniff" {
				t.Fatalf("%s %s headers = %#v", route, name, got)
			}
			if got["Content-Security-Policy"] != "default-src 'none'; media-src 'self'; frame-ancestors 'self'" {
				t.Fatalf("%s %s policy = %q", route, name, got["Content-Security-Policy"])
			}
		}
		if recorder.Code != http.StatusPartialContent || opened.Status != http.StatusPartialContent || recorder.Body.String() != "2345" {
			t.Fatalf("%s status http=%d transfer=%d body=%q", name, recorder.Code, opened.Status, recorder.Body.String())
		}
	}
	for _, name := range []string{"page.html", "image.svg", "report.pdf", "notes.txt"} {
		recorder := httptest.NewRecorder()
		reg.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, hosted.URLPath+name, nil))
		if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox allow-scripts") {
			t.Fatalf("%s policy = %q", name, got)
		}
	}
}
