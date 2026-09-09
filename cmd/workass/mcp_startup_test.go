package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/tlscert"
)

func TestToolListenerReadinessPrecedesProviderStartupRelease(t *testing.T) {
	certificate, err := tlscert.Ensure(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("ensure readiness certificate: %v", err)
	}
	loopback, err := tlscert.NewLoopbackServerCertificateRotator(certificate, "tools.localhost")
	if err != nil {
		t.Fatalf("create loopback certificate: %v", err)
	}
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	seen := make(chan string, 2)
	for _, path := range []string{toolsPath} {
		path := path
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			seen <- path
			w.WriteHeader(http.StatusMethodNotAllowed)
		})
	}
	server := &http.Server{
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate.TLS},
			MinVersion:   tls.VersionTLS13,
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				if hello.ServerName == "tools.localhost" {
					return loopback.GetCertificate(hello)
				}
				return &certificate.TLS, nil
			},
		},
	}
	listener := tls.NewListener(tcpListener, server.TLSConfig)
	serveErr := startDaemonHTTP(server, listener)
	t.Cleanup(func() { stopStartedDaemonHTTP(server, listener) })

	readinessTLS, err := daemonReadinessTLSConfig(certificate, true)
	if err != nil {
		t.Fatalf("readiness TLS config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	released := false
	if err := releaseProviderStartupAfterHTTPReady(ctx, listener, readinessTLS, serveErr, func() error {
		select {
		case path := <-seen:
			if path != toolsPath {
				t.Fatalf("readiness probe reached %q, want %q", path, toolsPath)
			}
		default:
			t.Fatal("provider startup released before the agent Tool route answered")
		}
		released = true
		return nil
	}); err != nil {
		t.Fatalf("release provider startup after TLS Tool readiness: %v", err)
	}
	if !released {
		t.Fatal("provider startup was not released after Tool readiness")
	}
	select {
	case path := <-seen:
		t.Fatalf("unexpected extra Tool readiness request on %q", path)
	default:
	}

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("listener address: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: readinessTLS}}
	defer client.Transport.(*http.Transport).CloseIdleConnections()
	request, err := http.NewRequest(http.MethodGet, "https://127.0.0.1:"+port+toolsPath, nil)
	if err != nil {
		t.Fatalf("browser Tool probe request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("browser Tool listener probe: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("browser Tool probe status = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
	}
}
