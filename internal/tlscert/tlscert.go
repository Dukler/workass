// Package tlscert gives a daemon its own certificate.
//
// remote-plan E5. There is no certificate authority, nothing to buy and no
// internet involved: the daemon mints one self-signed certificate into its state
// dir, keeps it forever, and clients recognise it by its fingerprint — the same
// shape as an SSH host key.
//
// The fingerprint is what carries trust, not the chain, and that matters for two
// reasons. A daemon is reached by IP, so no name in a certificate could ever be
// validated the ordinary way. And a client holding the fleet key can do better
// than trusting the first fingerprint it sees: the daemon proves the fingerprint
// under that key (fleet.CertProof), so an active attacker who swapped in their
// own certificate fails immediately instead of being believed once.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	CertFileName = "daemon-cert.pem"
	KeyFileName  = "daemon-key.pem"
	// Ten years. A daemon certificate that quietly expires is worse than one
	// that never rotates: rotation is visible (the fingerprint changes and every
	// pinned client says so), expiry is a machine that stops answering at 3am.
	validity = 10 * 365 * 24 * time.Hour
)

// Certificate is the loaded pair plus the name clients know it by.
type Certificate struct {
	TLS tls.Certificate
	// Fingerprint is the SHA-256 of the DER certificate, lowercase hex. This is
	// the value a client pins and the value the daemon proves under the fleet key.
	Fingerprint string
	Minted      bool
}

// Ensure loads this machine's certificate, minting one on first use.
//
// A corrupt or unreadable pair is an error rather than something to silently
// replace: re-minting would change the fingerprint, and every client that had
// pinned the old one would refuse to connect without being told why.
func Ensure(stateDir string) (Certificate, error) {
	if stateDir == "" {
		return Certificate{}, errors.New("tls state directory is empty")
	}
	certPath := filepath.Join(stateDir, CertFileName)
	keyPath := filepath.Join(stateDir, KeyFileName)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		loaded, err := load(certPEM, keyPEM)
		if err != nil {
			return Certificate{}, fmt.Errorf("read daemon certificate: %w", err)
		}
		return loaded, nil
	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
	case certErr != nil && !errors.Is(certErr, os.ErrNotExist):
		return Certificate{}, fmt.Errorf("read daemon certificate: %w", certErr)
	case keyErr != nil && !errors.Is(keyErr, os.ErrNotExist):
		return Certificate{}, fmt.Errorf("read daemon key: %w", keyErr)
	default:
		// Exactly one half present: the pair is broken, and minting over it
		// would strand whatever pinned the missing half.
		return Certificate{}, errors.New("daemon certificate and key must exist together; one of them is missing")
	}

	certPEM, keyPEM, err := mint()
	if err != nil {
		return Certificate{}, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return Certificate{}, err
	}
	if err := writeFile(keyPath, keyPEM, 0o600); err != nil {
		return Certificate{}, fmt.Errorf("write daemon key: %w", err)
	}
	if err := writeFile(certPath, certPEM, 0o644); err != nil {
		return Certificate{}, fmt.Errorf("write daemon certificate: %w", err)
	}
	loaded, err := load(certPEM, keyPEM)
	if err != nil {
		return Certificate{}, err
	}
	loaded.Minted = true
	return loaded, nil
}

func load(certPEM, keyPEM []byte) (Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Certificate{}, err
	}
	if len(pair.Certificate) == 0 {
		return Certificate{}, errors.New("certificate file contains no certificate")
	}
	return Certificate{TLS: pair, Fingerprint: FingerprintOf(pair.Certificate[0])}, nil
}

// FingerprintOf names a certificate by its DER bytes.
func FingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func mint() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("mint daemon key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	names := []string{"localhost"}
	if host != "" {
		names = append(names, host)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: firstNonEmpty(host, "workass")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// Its own root, so a person who chooses to trust this machine can import
		// one file rather than a chain that does not exist.
		IsCA:        true,
		DNSNames:    names,
		IPAddresses: localAddresses(),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("mint daemon certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// localAddresses lists what this machine answers on today.
//
// These are a courtesy for a browser, which validates names: a pinning client
// never looks at them. They are also why a certificate is not re-minted when an
// address changes — DHCP moving this machine must not change its fingerprint and
// lock out every client that pinned it. A browser may warn after such a move;
// that is the honest cost of an address-based identity.
func localAddresses() []net.IP {
	out := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP == nil || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, network.IP)
	}
	return out
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
