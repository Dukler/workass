package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"workass/internal/httpserve"
	"workass/internal/lease"
	"workass/internal/wire"
)

// chatImageSidecar writes a content-addressed sidecar the way the session store
// does (a data URL) and returns its reference.
func chatImageSidecar(t *testing.T, stateDir, text string) string {
	t.Helper()
	name := sessionImageName(text)
	if err := os.MkdirAll(filepath.Join(stateDir, sessionImageDirname), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, sessionImageDirname, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return sessionImageDirname + "/" + name
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 255 / width), G: uint8(y * 255 / height), B: uint8((x + y) % 256), A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type chatImageServer struct {
	handler http.Handler
	manager *lease.Manager
	key     string
	digest  string
	png     []byte
}

func newChatImageServer(t *testing.T) chatImageServer {
	t.Helper()
	stateDir := t.TempDir()
	data := testPNG(t, 64, 40)
	ref := chatImageSidecar(t, stateDir, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data))
	runtime := screenshotHistory(t, stateDir, ref, 2, 1, false)
	manager, err := lease.NewManager(lease.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := manager.ApproveDevice("phone", "192.168.0.9")
	if err != nil {
		t.Fatal(err)
	}
	hub := wire.NewHub(wire.Options{Lease: manager})
	server := httpserve.New("", hub, nil)
	server.ChatImages = newChatImageHTTPHandler(runtime)
	// The key reaches the device on its socket's lan:access-state.
	client := dialTestWSHandler(t, server, "/?deviceToken="+token)
	t.Cleanup(func() { _ = client.conn.Close() })
	access := client.waitFor(t, 5*time.Second, func(msg wsMessage) bool { return msg.T == "event" && msg.Channel == "lan:access-state" })
	key := fieldString(mapFromAnyMain(access.Payload), "imageKey")
	if key == "" {
		t.Fatalf("approved access state carried no image key: %#v", access.Payload)
	}
	return chatImageServer{handler: server, manager: manager, key: key, digest: strings.TrimPrefix(ref, sessionImageDirname+"/"), png: data}
}

func (s chatImageServer) get(t *testing.T, method, target, remote string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://daemon.test"+target, nil)
	request.RemoteAddr = remote
	for name, values := range header {
		request.Header[name] = values
	}
	recorder := httptest.NewRecorder()
	s.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestChatImageIsServedByURLToAnApprovedDevice(t *testing.T) {
	s := newChatImageServer(t)
	path := httpserve.ChatImagePathPrefix + "screenshot-tab/" + s.digest + "?k=" + s.key
	got := s.get(t, http.MethodGet, path, "192.168.0.9:5000", nil)
	if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), s.png) {
		t.Fatalf("GET = %d with %d bytes, want 200 with the decoded PNG", got.Code, got.Body.Len())
	}
	header := got.Header()
	for name, want := range map[string]string{
		"Content-Type": "image/png", "ETag": `"` + s.digest + `"`, "X-Content-Type-Options": "nosniff",
		"Cache-Control": "private, max-age=31536000, immutable", "Content-Security-Policy": "default-src 'none'; sandbox",
	} {
		if header.Get(name) != want {
			t.Fatalf("%s = %q, want %q", name, header.Get(name), want)
		}
	}
	// The digest names one content: a cached copy is always current.
	revalidated := s.get(t, http.MethodGet, path, "192.168.0.9:5000", http.Header{"If-None-Match": {`"` + s.digest + `"`}})
	if revalidated.Code != http.StatusNotModified || revalidated.Body.Len() != 0 {
		t.Fatalf("revalidation = %d with %d bytes, want 304", revalidated.Code, revalidated.Body.Len())
	}
	head := s.get(t, http.MethodHead, path, "127.0.0.1:5000", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(s.png)) {
		t.Fatalf("HEAD = %d body=%d length=%q", head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
	}
}

func TestChatImageRequestsNeedAnApprovedDeviceKeyAndTheChatsOwnImage(t *testing.T) {
	s := newChatImageServer(t)
	base := httpserve.ChatImagePathPrefix + "screenshot-tab/"
	cases := []struct {
		name, method, target, remote string
		want                         int
	}{
		{"no key from the LAN", http.MethodGet, base + s.digest, "192.168.0.9:5000", http.StatusForbidden},
		// Any local page can point an <img> at localhost.
		{"no key from loopback", http.MethodGet, base + s.digest, "127.0.0.1:5000", http.StatusForbidden},
		{"forged key", http.MethodGet, base + s.digest + "?k=" + s.key[:len(s.key)-2] + "00", "127.0.0.1:5000", http.StatusForbidden},
		{"device token as key", http.MethodGet, base + s.digest + "?k=" + strings.Repeat("a", 64), "127.0.0.1:5000", http.StatusForbidden},
		{"an image the chat does not own", http.MethodGet, base + strings.Repeat("1", 64) + "?k=" + s.key, "127.0.0.1:5000", http.StatusNotFound},
		{"an unknown chat", http.MethodGet, httpserve.ChatImagePathPrefix + "other-tab/" + s.digest + "?k=" + s.key, "127.0.0.1:5000", http.StatusNotFound},
		{"not a digest", http.MethodGet, base + "..%2F..%2Fdevices.json?k=" + s.key, "127.0.0.1:5000", http.StatusNotFound},
		{"no tab", http.MethodGet, httpserve.ChatImagePathPrefix + s.digest + "?k=" + s.key, "127.0.0.1:5000", http.StatusNotFound},
		{"a write", http.MethodPost, base + s.digest + "?k=" + s.key, "127.0.0.1:5000", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		got := s.get(t, c.method, c.target, c.remote, nil)
		if got.Code != c.want {
			t.Fatalf("%s: status %d, want %d", c.name, got.Code, c.want)
		}
		if got.Code != http.StatusOK && strings.Contains(got.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("%s: a refusal was marked cacheable", c.name)
		}
	}
	devices := s.manager.Devices()
	if !s.manager.RevokeDevice(devices[0].ID) {
		t.Fatal("revoke failed")
	}
	if got := s.get(t, http.MethodGet, base+s.digest+"?k="+s.key, "192.168.0.9:5000", nil); got.Code != http.StatusForbidden {
		t.Fatalf("revoked device's key: status %d, want 403", got.Code)
	}
}

// Only raster images go by URL; anything else stays on the socket read.
func TestChatImageEndpointServesRasterImagesOnly(t *testing.T) {
	stateDir := t.TempDir()
	ref := chatImageSidecar(t, stateDir, "data:image/svg+xml;base64,"+base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)))
	runtime := screenshotHistory(t, stateDir, ref, 1, 1, false)
	server := httpserve.New("", wire.NewHub(), nil)
	server.ChatImages = newChatImageHTTPHandler(runtime)
	request := httptest.NewRequest(http.MethodGet, "http://daemon.test"+httpserve.ChatImagePathPrefix+"screenshot-tab/"+strings.TrimPrefix(ref, sessionImageDirname+"/"), nil)
	request.RemoteAddr = "127.0.0.1:5000"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	body, _ := io.ReadAll(recorder.Result().Body)
	if recorder.Code != http.StatusUnsupportedMediaType || bytes.Contains(body, []byte("<script")) {
		t.Fatalf("SVG by URL = %d %q, want 415 without its bytes", recorder.Code, body)
	}
}
