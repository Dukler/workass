package toolcli

import (
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
)

func TestClientRejectsNonlocalAndAmbiguousEndpointsBeforeReadingAuthority(t *testing.T) {
	for _, endpoint := range []string{
		"http://tools.localhost:8788/workass/tools",
		"https://example.com:443/workass/tools",
		"https://tools.localhost/workass/tools",
		"https://owner@tools.localhost:8788/workass/tools",
		"https://tools.localhost:8788/workass/mcp/agent",
		"https://tools.localhost:8788/workass/tools?other=1",
		"https://tools.localhost:8788/workass/tools#fragment",
	} {
		_, err := Do(context.Background(), Config{Endpoint: endpoint, Credential: "test-private-authority"}, "", nil)
		if err == nil || err.Error() != "invalid Workass tool endpoint" {
			t.Fatalf("unsafe endpoint was not rejected before use: %s", endpoint)
		}
		if strings.Contains(err.Error(), "test-private-authority") {
			t.Fatal("error exposed authority")
		}
	}
}

func TestReadConfigRejectsDirectoryOversizedAndSymlink(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadConfig(dir); err == nil {
		t.Fatal("accepted a directory")
	}
	large := filepath.Join(dir, "large.json")
	if err := os.WriteFile(large, make([]byte, 65537), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(large); err == nil {
		t.Fatal("accepted oversized context")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(large, link); err == nil {
		if _, err := ReadConfig(link); err == nil {
			t.Fatal("accepted a symlink")
		}
	}
}

func TestReadConfigDistinguishesMissingContextFile(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "missing.json"), ""} {
		if _, err := ReadConfig(path); !errors.Is(err, ErrContextMissing) {
			t.Fatalf("missing context was not ErrContextMissing: %v", err)
		}
	}
	if _, err := ReadConfig(dir); errors.Is(err, ErrContextMissing) {
		t.Fatal("unusable context must not read as missing")
	}
}

// fixtureConfig pins one loopback TLS server valid for tools.localhost and
// answering the fixed tool response.
func fixtureConfig(t *testing.T, status int, body string) Config {
	t.Helper()
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
		_ = json.NewEncoder(w).Encode(Response{Error: body})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return Config{Endpoint: "https://tools.localhost:" + port + "/workass/tools", CAFile: ca,
		Credential: "test-private-authority", ChatID: "chat-a", TabID: "tab-a"}
}

func TestDoReportsRejectionStatus(t *testing.T) {
	config := fixtureConfig(t, http.StatusUnauthorized, "unauthorized")
	response, err := Do(context.Background(), config, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != http.StatusUnauthorized || response.Error != "unauthorized" {
		t.Fatalf("rejected request: status=%d error=%q", response.Status, response.Error)
	}
}
