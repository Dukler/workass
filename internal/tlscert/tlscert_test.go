package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"workass/internal/fleet"
)

// The property everything else rests on: the fingerprint is stable. A client
// pins it, so a daemon that re-minted on restart would lock out every client it
// had ever paired with — and would look like an attacker while doing it.
func TestCertificateIsMintedOnceAndKeptForever(t *testing.T) {
	stateDir := t.TempDir()
	first, err := Ensure(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Minted || first.Fingerprint == "" {
		t.Fatalf("first Ensure = %+v", first)
	}
	second, err := Ensure(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if second.Minted {
		t.Fatal("a second start minted a new certificate; every pinned client would refuse it")
	}
	if second.Fingerprint != first.Fingerprint {
		t.Fatalf("fingerprint moved: %s → %s", first.Fingerprint, second.Fingerprint)
	}

	// The key is the machine's; the certificate is public by nature.
	info, err := os.Stat(filepath.Join(stateDir, KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("private key mode = %v, want 0600", mode)
	}
}

func TestHalfAPairIsAnErrorRatherThanASilentReMint(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := Ensure(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(stateDir, KeyFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(stateDir); err == nil {
		t.Fatal("a broken pair was silently replaced; pinned clients would be stranded with no reason given")
	}
}

func TestLoopbackServerCertificateChainsToPermanentRoot(t *testing.T) {
	root, err := Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leafPair, err := IssueLoopbackServerCertificate(root, "mcp.localhost")
	if err != nil {
		t.Fatal(err)
	}
	if len(leafPair.Certificate) != 2 {
		t.Fatalf("certificate chain length = %d, want leaf + root", len(leafPair.Certificate))
	}
	leaf, err := x509.ParseCertificate(leafPair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA {
		t.Fatal("loopback server certificate is a CA")
	}
	roots := x509.NewCertPool()
	rootCertificate, err := x509.ParseCertificate(root.TLS.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(rootCertificate)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "mcp.localhost"}); err != nil {
		t.Fatalf("verify loopback leaf: %v", err)
	}
	if FingerprintOf(root.TLS.Certificate[0]) != root.Fingerprint {
		t.Fatal("issuing a loopback leaf changed the permanent root identity")
	}
}

func TestLoopbackServerCertificateRejectsNonLocalhostName(t *testing.T) {
	root, err := Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := IssueLoopbackServerCertificate(root, "example.com"); err == nil {
		t.Fatal("issued a loopback certificate for a non-localhost name")
	}
}

// It has to actually serve. A certificate that loads but cannot complete a
// handshake is the failure that only shows up on the network.
func TestItServesAndThePinnedFingerprintIsWhatArrives(t *testing.T) {
	certificate, err := Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "workass")
	})}
	config := &tls.Config{Certificates: []tls.Certificate{certificate.TLS}, MinVersion: tls.VersionTLS12}
	go func() { _ = server.Serve(tls.NewListener(listener, config)) }()
	defer server.Close()

	seen := ""
	client := &http.Client{Transport: &http.Transport{
		// A pinning client does exactly this: skip the chain it cannot validate
		// against an IP, and check the fingerprint instead.
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) > 0 {
					seen = FingerprintOf(rawCerts[0])
				}
				return nil
			},
		},
	}}
	response, err := client.Get("https://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "workass" {
		t.Fatalf("body = %q", body)
	}
	if seen != certificate.Fingerprint {
		t.Fatalf("fingerprint on the wire %s, expected %s", seen, certificate.Fingerprint)
	}
}

// Why this beats trust-on-first-use: a client holding the fleet key can check
// the certificate instead of believing whichever one it met first.
func TestTheFleetKeyProvesTheCertificateInsteadOfTrustingIt(t *testing.T) {
	certificate, err := Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := store.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := fleet.NewNonce()
	if err != nil {
		t.Fatal(err)
	}

	proof := fleet.CertProof(key.Secret, nonce, certificate.Fingerprint)
	if proof == "" {
		t.Fatal("no proof for a real fingerprint")
	}
	if got := fleet.CertProof(key.Secret, nonce, certificate.Fingerprint); got != proof {
		t.Fatal("the proof is not deterministic")
	}

	// An attacker who substitutes their own certificate cannot produce a proof
	// that matches it, and cannot reuse this one: it names a different hash.
	impostor, err := Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if impostor.Fingerprint == certificate.Fingerprint {
		t.Fatal("two independent mints collided")
	}
	if fleet.CertProof(key.Secret, nonce, impostor.Fingerprint) == proof {
		t.Fatal("a substituted certificate produced the same proof")
	}
	// Nor can it be replayed onto another connection: the nonce is in the input.
	other, _ := fleet.NewNonce()
	if fleet.CertProof(key.Secret, other, certificate.Fingerprint) == proof {
		t.Fatal("the proof replays across connections")
	}
	// And a stranger's key proves nothing here.
	strangerKey, _ := fleet.NewSecret()
	if fleet.CertProof(strangerKey, nonce, certificate.Fingerprint) == proof {
		t.Fatal("a key from another fleet produced this fleet's proof")
	}
}
