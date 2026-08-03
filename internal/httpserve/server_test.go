package httpserve

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestIndexServedWithLanBridgeInjection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	server := httptest.NewServer(New(dir, nil, nil))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `<script src="/lan-bridge.js"></script>`) {
		t.Fatalf("missing lan bridge injection in %s", body)
	}
}

func TestEmbeddedIndexServedWithLanBridgeInjection(t *testing.T) {
	handler := New("", nil, nil)
	handler.RendererFS = fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><head></head><body></body>")},
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `<script src="/lan-bridge.js"></script>`) {
		t.Fatalf("missing lan bridge injection in %s", body)
	}
}

func TestTraversalAttemptDeniedOrNotFound(t *testing.T) {
	dir := t.TempDir()
	req := httptest.NewRequest(http.MethodGet, "http://example.test/../go.mod", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()

	New(dir, nil, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 403 or 404", rec.Code)
	}
}

func TestMocksServeFilesWithoutCachingAndWithContentTypes(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"mock.html": "text/html; charset=utf-8",
		"mock.css":  "text/css; charset=utf-8",
		"mock.js":   "text/javascript; charset=utf-8",
		"mock.png":  "image/png",
		"mock.svg":  "image/svg+xml",
		"mock.jpg":  "image/jpeg",
		"mock.jpeg": "image/jpeg",
		"mock.webp": "image/webp",
		"mock.gif":  "image/gif",
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	handler := New("", nil, nil)
	handler.MocksDir = dir
	server := httptest.NewServer(handler)
	defer server.Close()

	for name, wantType := range files {
		resp, err := http.Get(server.URL + "/workass/mocks/" + name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "fixture" {
			t.Fatalf("%s status = %d body=%q", name, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s cache-control = %q", name, got)
		}
		if got := resp.Header.Get("Content-Type"); got != wantType {
			t.Fatalf("%s content-type = %q, want %q", name, got, wantType)
		}
	}
}

func TestMocksTraversalCannotReadOutsideRoot(t *testing.T) {
	parent := t.TempDir()
	mocks := filepath.Join(parent, "mocks")
	if err := os.Mkdir(mocks, 0o755); err != nil {
		t.Fatal(err)
	}
	const secret = "outside fixture must not be served"
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := New("", nil, nil)
	handler.MocksDir = mocks
	req := httptest.NewRequest(http.MethodGet, "http://example.test/workass/mocks/../../secret.txt", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status = %d, want 4xx", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("traversal response contained the outside file")
	}
}

func TestMocksDisabledReturnsNotFound(t *testing.T) {
	handler := New("", nil, nil)
	req := httptest.NewRequest(http.MethodGet, "http://example.test/workass/mocks/mock.html", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestMocksIndexListsNestedHTMLFilesSorted(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"z.html":          "z",
		"nested/a.html":   "a",
		"nested/site.css": "css",
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	handler := New("", nil, nil)
	handler.MocksDir = dir

	for _, requestPath := range []string{"/workass/mocks", "/workass/mocks/"} {
		req := httptest.NewRequest(http.MethodGet, "http://example.test"+requestPath, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", requestPath, rec.Code, body)
		}
		first := strings.Index(body, `href="nested/a.html"`)
		second := strings.Index(body, `href="z.html"`)
		if first < 0 || second < 0 || first >= second || strings.Contains(body, "site.css") {
			t.Fatalf("%s index body = %s", requestPath, body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s cache-control = %q", requestPath, got)
		}
	}
}

func TestMocksRejectNonGetMethods(t *testing.T) {
	handler := New("", nil, nil)
	handler.MocksDir = t.TempDir()
	req := httptest.NewRequest(http.MethodPost, "http://example.test/workass/mocks/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("allow = %q", got)
	}
}

func TestArtifactHostingRoutesThroughTheAllowedDaemonHTTPServer(t *testing.T) {
	handler := New("", nil, nil)
	handler.ArtifactHosts = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Workass-Artifact-Host", "yes")
		_, _ = w.Write([]byte(r.URL.Path))
	})
	for _, requestPath := range []string{"/workass/artifacts/report-id/", "/workass/html/site-id/"} {
		request := httptest.NewRequest(http.MethodGet, "http://example.test"+requestPath, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || recorder.Header().Get("X-Workass-Artifact-Host") != "yes" ||
			recorder.Body.String() != requestPath {
			t.Fatalf("artifact host route %s response = status %d headers=%v body=%q", requestPath, recorder.Code, recorder.Header(), recorder.Body.String())
		}
	}
}

func TestLanBridgeServed(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(New(dir, nil, nil))
	defer server.Close()

	resp, err := http.Get(server.URL + "/lan-bridge.js")
	if err != nil {
		t.Fatalf("get bridge: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Fatalf("content-type = %q", got)
	}
	if !strings.Contains(string(body), "window.api") ||
		!strings.Contains(string(body), "appMeta: () => invoke('app:meta')") ||
		!strings.Contains(string(body), "stateDigest: () => invoke('state:digest')") {
		t.Fatalf("unexpected bridge body: %.120s", body)
	}
	if !strings.Contains(string(body), "appChatSteer: (sessionId, prompt, images, clientUserMessageId, continuationAssistantMessageId, boundary) => invoke('app-chat:steer'") ||
		!strings.Contains(string(body), "appChatUseRateLimitReset: (providerId, sessionId, idempotencyKey, creditId) => invoke('app-chat:use-rate-limit-reset'") ||
		!strings.Contains(string(body), "appChatDetectAcp: (opts) => invoke('app-chat:detect-acp'") {
		t.Fatalf("bridge missing app-chat steer/detect-acp methods")
	}
}

func TestLanBridgeRejectsPendingInvokeWhenSocketCloses(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not available for lan bridge behavior test: %v", err)
	}
	script := `
const assert = require('assert');

let socket;
class FakeWebSocket {
  constructor() {
    this.readyState = FakeWebSocket.OPEN;
    socket = this;
  }
  send() {}
  close() {}
}
FakeWebSocket.OPEN = 1;

Object.defineProperty(global, 'window', { value: {}, configurable: true });
Object.defineProperty(global, 'localStorage', {
  value: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
  configurable: true,
});
Object.defineProperty(global, 'navigator', { value: { platform: 'Test' }, configurable: true });
Object.defineProperty(global, 'location', { value: { protocol: 'http:', host: 'example.test' }, configurable: true });
Object.defineProperty(global, 'WebSocket', { value: FakeWebSocket, configurable: true });

eval(process.env.LAN_BRIDGE_JS);
assert(socket, 'bridge did not create a WebSocket');
const pending = window.api.appMeta();
socket.onclose();

pending.then(
  () => { throw new Error('pending invoke unexpectedly resolved'); },
  (error) => {
    assert.equal(error.name, 'WorkassInvokeError');
    assert.equal(error.code, 'socket-closed');
    assert.equal(error.channel, 'app:meta');
    process.exit(0);
  },
);
setTimeout(() => {
  console.error('pending invoke did not reject after socket close');
  process.exit(1);
}, 500);
`
	cmd := exec.Command("node", "-e", script)
	cmd.Env = append(os.Environ(), "LAN_BRIDGE_JS="+LANBridgeJS)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node bridge close behavior test failed: %v\n%s", err, out)
	}
}

func TestLanBridgeCachesLatestEventForLateListeners(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not available for lan bridge behavior test: %v", err)
	}
	script := `
const assert = require('assert');

let socket;
class FakeWebSocket {
  constructor(url) {
    this.url = url;
    this.readyState = FakeWebSocket.OPEN;
    this.sent = [];
    socket = this;
  }
  send(data) {
    this.sent.push(data);
  }
}
FakeWebSocket.OPEN = 1;

const storage = new Map();
Object.defineProperty(global, 'window', { value: {}, configurable: true });
Object.defineProperty(global, 'localStorage', {
  value: {
    getItem: (key) => storage.has(key) ? storage.get(key) : null,
    setItem: (key, value) => storage.set(key, String(value)),
    removeItem: (key) => storage.delete(key),
  },
  configurable: true,
});
Object.defineProperty(global, 'navigator', { value: { platform: 'Test' }, configurable: true });
Object.defineProperty(global, 'location', { value: { protocol: 'http:', host: 'example.test' }, configurable: true });
Object.defineProperty(global, 'WebSocket', { value: FakeWebSocket, configurable: true });

eval(process.env.LAN_BRIDGE_JS);
assert(socket, 'bridge did not create a WebSocket');

function emit(channel, payload) {
  socket.onmessage({ data: JSON.stringify({ t: 'event', channel, payload }) });
}
function tick() {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

(async () => {
  emit('chat:catalog', { version: 1 });
  const catalog = [];
  window.api.onChatCatalog((payload) => catalog.push(payload.version));
  assert.deepStrictEqual(catalog, [], 'cached payload should not be delivered synchronously');
  await tick();
  assert.deepStrictEqual(catalog, [1]);

  emit('chat:catalog', { version: 2 });
  await tick();
  assert.deepStrictEqual(catalog, [1, 2]);

  const lateCatalog = [];
  window.api.onChatCatalog((payload) => lateCatalog.push(payload.version));
  await tick();
  assert.deepStrictEqual(lateCatalog, [2], 'second event should replace the cached payload');

  emit('proc:changed', { id: 'cached-proc' });
  const procs = [];
  window.api.onProcChanged((payload) => procs.push(['a', payload.id]));
  window.api.onProcChanged((payload) => procs.push(['b', payload.id]));
  await tick();
  assert.deepStrictEqual(procs, [['a', 'cached-proc'], ['b', 'cached-proc']]);

  emit('proc:changed', { id: 'live-proc' });
  await tick();
  assert.deepStrictEqual(procs, [
    ['a', 'cached-proc'],
    ['b', 'cached-proc'],
    ['a', 'live-proc'],
    ['b', 'live-proc'],
  ]);

  emit('agent:navigate', { id: 'cached-nav' });
  const nav = [];
  const unsubscribe = window.api.onAgentNavigate((payload) => nav.push(payload.id));
  unsubscribe();
  await tick();
  emit('agent:navigate', { id: 'live-nav' });
  await tick();
  assert.deepStrictEqual(nav, [], 'unsubscribed listener should not receive cached or live payloads');
})().catch((err) => {
  console.error(err && err.stack || err);
  process.exit(1);
});
`
	cmd := exec.Command("node", "-e", script)
	cmd.Env = append(os.Environ(), "LAN_BRIDGE_JS="+LANBridgeJS)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node bridge behavior test failed: %v\n%s", err, out)
	}
}

func TestHealthEndpoint(t *testing.T) {
	dir := t.TempDir()
	handler := New(dir, nil, func(ip string) bool { return false })
	handler.Version = "test-version"
	handler.Name = "test-host"
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/workass/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body["app"] != "workass" || body["version"] != "test-version" || body["name"] != "test-host" {
		t.Fatalf("health body = %#v", body)
	}
}

func TestHealthEndpointMergesMachineIdentityWithoutLosingTheOldKeys(t *testing.T) {
	dir := t.TempDir()
	handler := New(dir, nil, func(ip string) bool { return false })
	handler.Version = "test-version"
	handler.Name = "test-host"
	handler.Identity = func() map[string]any {
		return map[string]any{
			"machineId":   "m-abc",
			"displayName": "Builder",
			"name":        "Builder",
			"wireVersion": 1,
			"providers":   []string{"claude"},
		}
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	body := getHealthDoc(t, server.URL)
	// A client written before machine identity existed reads these three.
	if body["app"] != "workass" || body["version"] != "test-version" {
		t.Fatalf("identity displaced the original keys: %#v", body)
	}
	if body["machineId"] != "m-abc" || body["displayName"] != "Builder" {
		t.Fatalf("identity was not merged: %#v", body)
	}
	if body["name"] != "Builder" {
		t.Fatalf("identity should be able to name the machine, got %#v", body["name"])
	}
	if body["wireVersion"] != float64(1) {
		t.Fatalf("wire version = %#v", body["wireVersion"])
	}
}

// The app key names the protocol a client is about to speak, and a blank name
// leaves a machine unlabelled in the machine book; neither may be overwritten
// by a malformed identity.
func TestHealthEndpointRefusesAnIdentityThatBlanksTheContract(t *testing.T) {
	dir := t.TempDir()
	handler := New(dir, nil, func(ip string) bool { return false })
	handler.Version = "test-version"
	handler.Name = "test-host"
	handler.Identity = func() map[string]any {
		return map[string]any{"app": "not-workass", "name": "   "}
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	body := getHealthDoc(t, server.URL)
	if body["app"] != "workass" {
		t.Fatalf("app key was overwritten: %#v", body["app"])
	}
	if body["name"] != "test-host" {
		t.Fatalf("a blank identity name should fall back to the host name, got %#v", body["name"])
	}
}

// Health answers before any pairing check, so a stranger on the LAN reads
// whatever it returns. It may learn enough to recognise the machine and offer
// to pair; it may not inventory the box.
func TestHealthEndpointTellsAStrangerOnlyWhatPairingNeeds(t *testing.T) {
	dir := t.TempDir()
	handler := New(dir, nil, nil)
	handler.Version = "test-version"
	handler.Name = "test-host"
	handler.Identity = func() map[string]any {
		return map[string]any{
			"machineId":   "m-abc",
			"displayName": "Builder",
			"name":        "Builder",
			"wireVersion": 1,
			"secure":      false,
			"profile":     "prod",
			"os":          "windows",
			"arch":        "amd64",
			"bind":        "lan",
			"port":        80,
			"providers":   []string{"claude", "codex"},
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/workass/health", nil)
	request.RemoteAddr = "10.0.0.5:51515"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	var body map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	for _, key := range []string{"app", "version", "name", "machineId", "displayName", "wireVersion", "secure"} {
		if _, present := body[key]; !present {
			t.Errorf("a stranger needs %q to recognise this machine, and it was withheld", key)
		}
	}
	for _, key := range []string{"profile", "os", "arch", "bind", "port", "providers"} {
		if value, present := body[key]; present {
			t.Errorf("unauthenticated caller was told %q = %v", key, value)
		}
	}

	// The same daemon, asked from its own machine, still answers in full.
	local := httptest.NewRequest(http.MethodGet, "/workass/health", nil)
	local.RemoteAddr = "127.0.0.1:51515"
	localRecorder := httptest.NewRecorder()
	handler.ServeHTTP(localRecorder, local)
	var localBody map[string]any
	if err := json.NewDecoder(localRecorder.Body).Decode(&localBody); err != nil {
		t.Fatalf("decode local health: %v", err)
	}
	if _, present := localBody["providers"]; !present {
		t.Error("a loopback caller should still get the full identity")
	}
}

func getHealthDoc(t *testing.T, baseURL string) map[string]any {
	t.Helper()
	resp, err := http.Get(baseURL + "/workass/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return body
}
