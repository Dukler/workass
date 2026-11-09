package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workass/internal/fleet"
)

func TestLocalRecoveryShutdownHandlerAcceptsOnlyLoopbackPost(t *testing.T) {
	stopped := make(chan struct{})
	handler := localRecoveryShutdownHandler(func() { close(stopped) })
	remote := httptest.NewRequest(http.MethodPost, "https://127.0.0.1:8788/workass/recovery/shutdown", nil)
	remote.RemoteAddr = "192.0.2.10:3000"
	remoteRecorder := httptest.NewRecorder()
	handler.ServeHTTP(remoteRecorder, remote)
	if remoteRecorder.Code != http.StatusForbidden {
		t.Fatalf("remote recovery status = %d", remoteRecorder.Code)
	}

	local := httptest.NewRequest(http.MethodPost, "https://127.0.0.1:8788/workass/recovery/shutdown", nil)
	local.RemoteAddr = "127.0.0.1:3000"
	localRecorder := httptest.NewRecorder()
	handler.ServeHTTP(localRecorder, local)
	if localRecorder.Code != http.StatusAccepted {
		t.Fatalf("local recovery status = %d", localRecorder.Code)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("local recovery did not request shutdown")
	}
}

// A browser page can reach these routes on loopback without being able to read
// the reply: a cross-site form POST stopped the daemon, and a rebinding page read
// the fleet code. The source address proves only which machine, so the Host and
// Origin have to prove which page.
func TestLocalControlRoutesRefuseForeignHostAndOrigin(t *testing.T) {
	keys, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const updateBody = `{"updateId":"update-origin-1234"}`
	type route struct {
		name, method, body string
		accepted           int
		handler            func() http.Handler
	}
	stopped := func() {}
	routes := []route{
		{"recovery shutdown", http.MethodPost, "", http.StatusAccepted, func() http.Handler { return localRecoveryShutdownHandler(stopped) }},
		{"update prepare", http.MethodPost, updateBody, http.StatusOK, func() http.Handler {
			return http.HandlerFunc(newLocalUpdateControl(&fakeAppUpdateGate{}, stopped).prepare)
		}},
		{"update cancel", http.MethodPost, updateBody, http.StatusOK, func() http.Handler {
			return http.HandlerFunc(newLocalUpdateControl(&fakeAppUpdateGate{}, stopped).cancel)
		}},
		{"update commit", http.MethodPost, updateBody, http.StatusConflict, func() http.Handler {
			return http.HandlerFunc(newLocalUpdateControl(&fakeAppUpdateGate{}, stopped).commit)
		}},
		{"update work", http.MethodPost, "", http.StatusOK, func() http.Handler {
			return http.HandlerFunc(newLocalUpdateControl(&fakeAppUpdateGate{}, stopped).work)
		}},
		{"fleet code", http.MethodGet, "", http.StatusOK, func() http.Handler { return newFleetQRHandler(keys, 8788, "lan", nil) }},
	}
	send := func(r route, target, host string, headers map[string]string) int {
		request := httptest.NewRequest(r.method, target+"?host=192.0.2.13:8788", strings.NewReader(r.body))
		request.RemoteAddr = "127.0.0.1:41000"
		if host != "" {
			request.Host = host
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		recorder := httptest.NewRecorder()
		r.handler().ServeHTTP(recorder, request)
		return recorder.Code
	}
	const own = "https://127.0.0.1:8788/workass/route"
	for _, r := range routes {
		for name, tc := range map[string]struct {
			host    string
			headers map[string]string
		}{
			"rebinding host":        {"rebind.attacker.example:8788", nil},
			"rebinding host+origin": {"rebind.attacker.example:8788", map[string]string{"Origin": "https://rebind.attacker.example:8788"}},
			"no host":               {" ", nil},
			"foreign origin":        {"", map[string]string{"Origin": "https://attacker.example"}},
			"opaque origin":         {"", map[string]string{"Origin": "null"}},
			"other loopback port":   {"", map[string]string{"Origin": "http://localhost:3000"}},
			"same port, plain http": {"", map[string]string{"Origin": "http://127.0.0.1:8788"}},
			"cross-site fetch":      {"", map[string]string{"Sec-Fetch-Site": "cross-site"}},
			"same-site fetch":       {"", map[string]string{"Sec-Fetch-Site": "same-site"}},
		} {
			if code := send(r, own, tc.host, tc.headers); code != http.StatusForbidden {
				t.Errorf("%s, %s: status = %d, want 403", r.name, name, code)
			}
		}
		// What the shell, the updater and a same-origin window really send.
		for name, tc := range map[string]struct {
			target  string
			headers map[string]string
		}{
			"node client":      {own, nil},
			"by name":          {"https://localhost:8788/workass/route", nil},
			"over ipv6":        {"https://[::1]:8788/workass/route", nil},
			"same-origin page": {own, map[string]string{"Origin": "https://127.0.0.1:8788", "Sec-Fetch-Site": "same-origin"}},
			"typed address":    {own, map[string]string{"Sec-Fetch-Site": "none"}},
		} {
			if code := send(r, tc.target, "", tc.headers); code != r.accepted {
				t.Errorf("%s, legitimate %s: status = %d, want %d", r.name, name, code, r.accepted)
			}
		}
	}
}
