package httpserve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMetricsReportsRuntimeAndDaemonCounters(t *testing.T) {
	server := New(t.TempDir(), nil, nil)
	server.Metrics = func() map[string]any {
		return map[string]any{"session": map[string]any{"chats": 3}}
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, metricsPath, nil)
	request.RemoteAddr = "127.0.0.1:54321"
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("metrics body is not JSON: %v", err)
	}
	for _, key := range []string{"goroutines", "heap", "gc", "uptimeSeconds", "session"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("metrics payload is missing %q: %v", key, payload)
		}
	}
	// The daemon-owned counters must reach the caller unchanged: this endpoint
	// exists so a memory storm is a query rather than a hunch.
	session, _ := payload["session"].(map[string]any)
	if session == nil || session["chats"] != float64(3) {
		t.Fatalf("daemon counters were not merged: %v", payload["session"])
	}
	heap, _ := payload["heap"].(map[string]any)
	if heap == nil || heap["allocBytes"] == nil {
		t.Fatalf("heap counters missing: %v", payload["heap"])
	}
}

func TestMetricsRefusesNonLoopbackClients(t *testing.T) {
	server := New(t.TempDir(), nil, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, metricsPath, nil)
	request.RemoteAddr = "203.0.113.7:5000"
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote metrics status = %d, want 403", recorder.Code)
	}
}

func TestPprofIsOptInAndLoopbackOnly(t *testing.T) {
	server := New(t.TempDir(), nil, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, pprofPathRoot+"/", nil)
	request.RemoteAddr = "127.0.0.1:54321"
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("pprof status without %s = %d, want 404", pprofEnableEnv, recorder.Code)
	}

	t.Setenv(pprofEnableEnv, "1")
	enabled := httptest.NewRecorder()
	server.ServeHTTP(enabled, request)
	if enabled.Code != http.StatusOK {
		t.Fatalf("pprof status with %s=1 = %d, want 200", pprofEnableEnv, enabled.Code)
	}

	remote := httptest.NewRecorder()
	remoteRequest := httptest.NewRequest(http.MethodGet, pprofPathRoot+"/", nil)
	remoteRequest.RemoteAddr = "203.0.113.7:5000"
	server.ServeHTTP(remote, remoteRequest)
	if remote.Code != http.StatusNotFound {
		t.Fatalf("remote pprof status = %d, want 404", remote.Code)
	}
}
