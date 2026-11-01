package machinebook

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"time"
)

const associateTimeout = 450 * time.Millisecond

// OnAssociatedNetwork reports whether a machine already in the book is here right
// now. Automatic sweeps run only then: on a hotel or office network, knocking on
// every address looks like a port scan and finds nothing of ours.
//
// Only a completed TLS handshake against the stored certificate counts. A subnet
// proves nothing, because private ranges repeat across networks, and a card can
// say anything.
func (b *Book) OnAssociatedNetwork(ctx context.Context) bool {
	return b.associated(ctx, privateLANNetworks())
}

func (b *Book) associated(ctx context.Context, networks []*net.IPNet) bool {
	for _, entry := range b.List() {
		fingerprint := strings.ToLower(strings.TrimSpace(entry.CertFingerprint))
		if fingerprint == "" {
			continue
		}
		for _, endpoint := range entry.Endpoints {
			if !addressOnNetworks(endpoint.Address, networks) {
				continue
			}
			if ctx.Err() != nil {
				return false
			}
			if handshakeMatches(ctx, endpoint.Address, fingerprint) {
				return true
			}
		}
	}
	return false
}

func addressOnNetworks(address string, networks []*net.IPNet) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range networks {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func handshakeMatches(ctx context.Context, address, fingerprint string) bool {
	dialCtx, cancel := context.WithTimeout(ctx, associateTimeout)
	defer cancel()
	dialer := tls.Dialer{Config: &tls.Config{
		MinVersion: tls.VersionTLS13,
		// #nosec G402 -- daemons serve self-signed certificates; the pin below is
		// the whole identity check, and nothing is sent over this connection.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) > 0 {
				sum := sha256.Sum256(raw[0])
				if hex.EncodeToString(sum[:]) == fingerprint {
					return nil
				}
			}
			return errors.New("certificate is not the stored one")
		},
	}}
	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
