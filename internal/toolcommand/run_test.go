package toolcommand

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/toolcli"
)

// fixtureContextFile pins one loopback TLS server answering the fixed tool
// response and writes the matching private context file for the CLI to read.
func fixtureContextFile(t *testing.T, status int, responseError string) string {
	t.Helper()
	return fixtureContextFileResponse(t, status, toolcli.Response{Error: responseError})
}

func fixtureContextFileResponse(t *testing.T, status int, response toolcli.Response) string {
	t.Helper()
	root := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tools.localhost"},
		DNSNames:     []string{"tools.localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(response)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	config := toolcli.Config{Endpoint: "https://tools.localhost:" + port + "/workass/tools", CAFile: ca,
		Credential: "wct_private_test_capability", ChatID: "chat-a", TabID: "tab-a"}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "context.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runList(t *testing.T) error {
	t.Helper()
	var output, diagnostics bytes.Buffer
	return Run(context.Background(), []string{"list"}, strings.NewReader(""), &output, &diagnostics)
}

func TestRunExplainsProcessNotLaunchedByWorkassChatEngine(t *testing.T) {
	t.Setenv("WORKASS_TOOL_CONTEXT", "")
	err := runList(t)
	if err == nil || !strings.Contains(err.Error(), "this process was not launched by a Workass chat engine") {
		t.Fatalf("unset WORKASS_TOOL_CONTEXT: %v", err)
	}
}

func TestRunExplainsDeletedChatBehindContextFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone", "context.json")
	t.Setenv("WORKASS_TOOL_CONTEXT", path)
	err := runList(t)
	if err == nil || !errors.Is(err, toolcli.ErrContextMissing) || !strings.Contains(err.Error(), "was deleted") || !strings.Contains(err.Error(), "workass_schedule_message") {
		t.Fatalf("missing context file: %v", err)
	}
	if strings.Contains(err.Error(), path) {
		t.Fatalf("missing-context error leaked the private path %q: %v", path, err)
	}
}

func TestRunExplainsUnusableContextFile(t *testing.T) {
	t.Setenv("WORKASS_TOOL_CONTEXT", t.TempDir())
	err := runList(t)
	if err == nil || !strings.Contains(err.Error(), "unavailable or invalid") {
		t.Fatalf("directory context file: %v", err)
	}
}

func TestRunExplainsRevokedCredentialWithoutLeakingIt(t *testing.T) {
	const credential = "wct_private_test_capability"
	path := fixtureContextFile(t, http.StatusUnauthorized, "unauthorized")
	t.Setenv("WORKASS_TOOL_CONTEXT", path)
	err := runList(t)
	if err == nil {
		t.Fatal("revoked credential request passed")
	}
	if !strings.Contains(err.Error(), "Workass rejected this context's credential: the chat that launched this process was deleted, or the context was issued by an engine that has since stopped") || !strings.Contains(err.Error(), "workass_schedule_message") {
		t.Fatalf("revoked credential: %v", err)
	}
	if strings.Contains(err.Error(), credential) {
		t.Fatal("error exposed the credential")
	}
	if strings.Contains(err.Error(), path) {
		t.Fatal("error exposed the context file path")
	}
}

func TestRunSurfacesOtherDaemonRejectionsUnchanged(t *testing.T) {
	path := fixtureContextFile(t, http.StatusForbidden, "forbidden")
	t.Setenv("WORKASS_TOOL_CONTEXT", path)
	err := runList(t)
	if err == nil || err.Error() != "forbidden" {
		t.Fatalf("forbidden request: %v", err)
	}
}

func TestRunPrintsURLsInResultsLiterally(t *testing.T) {
	const href = "https://shop.example/itm/1?hash=item1&var=<2>"
	path := fixtureContextFileResponse(t, http.StatusOK, toolcli.Response{Result: map[string]any{"href": href}})
	t.Setenv("WORKASS_TOOL_CONTEXT", path)
	var output, diagnostics bytes.Buffer
	if err := Run(context.Background(), []string{"call", "workass_browser_read"}, strings.NewReader("{}"), &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), href) {
		t.Fatalf("output = %s", output.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || decoded["href"] != href {
		t.Fatalf("decoded = %#v, err=%v", decoded, err)
	}
}
