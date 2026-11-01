package fleetclient

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workass/internal/tlscert"
)

// forgedDaemon is the attack itself, not a mock of the code under test. It sits at
// an address the victim was led to, serves its own certificate, names the real
// machine, and passes the real daemon's fleet challenge through to the victim. Any
// proof the victim answers with is forwarded, and if the real daemon accepts it,
// the forger's own socket is the one that becomes approved.
type forgedDaemon struct {
	address       string
	enrolAttempts atomic.Int32
	tokensSeen    atomic.Int32
	upstream      atomic.Pointer[Link]
	// rewrite decides what the victim sees of the real challenge.
	rewrite func(challenge map[string]any, ownFingerprint string)
}

func newForgedDaemon(t *testing.T, real *peerDaemon, rewrite func(map[string]any, string)) *forgedDaemon {
	t.Helper()
	certificate, err := tlscert.Ensure(t.TempDir())
	if err != nil {
		t.Fatalf("forged certificate: %v", err)
	}
	forged := &forgedDaemon{rewrite: rewrite}
	mux := http.NewServeMux()
	mux.HandleFunc("/workass/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "workass", "machineId": real.machineID, "displayName": real.machineID, "name": real.machineID,
			"wireVersion": 1, "secure": true, "certFingerprint": certificate.Fingerprint, "fleetIds": real.keys.KeyIDs(),
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		forged.relay(t, w, r, real, certificate.Fingerprint)
	})
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("forged listen: %v", err)
	}
	forged.address = raw.Addr().String()
	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(tls.NewListener(raw, &tls.Config{
			Certificates: []tls.Certificate{certificate.TLS}, MinVersion: tls.VersionTLS13,
		}))
	}()
	t.Cleanup(func() {
		_ = server.Close()
		if upstream := forged.upstream.Load(); upstream != nil {
			upstream.Close(nil)
		}
	})
	return forged
}

func (f *forgedDaemon) relay(t *testing.T, w http.ResponseWriter, r *http.Request, real *peerDaemon, ownFingerprint string) {
	if r.URL.Query().Get("deviceToken") != "" {
		f.tokensSeen.Add(1)
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+
		base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	upstream, err := dialLink(ctx, dialOptions{
		address: real.address, peerID: real.machineID, deviceName: "forged", tlsConfig: pinnedTLSConfig(real.fingerprint),
	})
	if err != nil {
		t.Errorf("forger could not reach the real daemon: %v", err)
		return
	}
	f.upstream.Store(upstream)
	for {
		frame, err := readClientFrame(rw.Reader)
		if err != nil {
			return
		}
		var message struct {
			ID      any    `json:"id"`
			Channel string `json:"channel"`
			Args    []any  `json:"args"`
		}
		if json.Unmarshal(frame, &message) != nil {
			continue
		}
		var result any
		switch message.Channel {
		case "fleet:challenge":
			answer, err := upstream.Invoke(ctx, "fleet:challenge")
			if err != nil {
				t.Errorf("real challenge: %v", err)
				return
			}
			challenge := mapOf(answer)
			f.rewrite(challenge, ownFingerprint)
			result = challenge
		case "fleet:enroll":
			f.enrolAttempts.Add(1)
			result, err = upstream.Invoke(ctx, "fleet:enroll", message.Args...)
			if err != nil {
				t.Logf("real daemon refused the relayed proof: %v", err)
				return
			}
		default:
			continue
		}
		body, _ := json.Marshal(map[string]any{"t": "reply", "id": message.ID, "result": result})
		if err := writeServerFrame(conn, body); err != nil {
			return
		}
	}
}

func readClientFrame(reader *bufio.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(reader, extended); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(reader, extended); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(extended)
	}
	mask := make([]byte, 4)
	if _, err := io.ReadFull(reader, mask); err != nil {
		return nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return payload, nil
}

func writeServerFrame(conn net.Conn, payload []byte) error {
	frame := []byte{0x81}
	switch {
	case len(payload) < 126:
		frame = append(frame, byte(len(payload)))
	case len(payload) < 65536:
		frame = append(frame, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		extended := make([]byte, 8)
		binary.BigEndian.PutUint64(extended, uint64(len(payload)))
		frame = append(append(frame, 127), extended...)
	}
	_, err := conn.Write(append(frame, payload...))
	return err
}

// The attack from docs/specs/portable-networking.md §3, on the Go mesh: a forged
// machine must never receive an enrolment proof, however it presents the challenge,
// and the real daemon must never end up approving the forger's socket.
func TestAForgedMachineNeverGetsAProofToRelay(t *testing.T) {
	cases := map[string]func(map[string]any, string){
		"proofs stripped": func(challenge map[string]any, _ string) {
			delete(challenge, "certProofs")
		},
		"proofs stripped and the forger's own fingerprint claimed": func(challenge map[string]any, own string) {
			delete(challenge, "certProofs")
			challenge["certFingerprint"] = own
		},
		"the real proofs replayed for the forger's certificate": func(challenge map[string]any, own string) {
			challenge["certFingerprint"] = own
		},
		"proofs empty": func(challenge map[string]any, _ string) {
			challenge["certProofs"] = map[string]any{}
		},
	}
	for name, rewrite := range cases {
		t.Run(name, func(t *testing.T) {
			secret := newTestSecret(t)
			home := newPeerDaemon(t, "m-home", secret)
			forged := newForgedDaemon(t, home, rewrite)
			mesh, store := newDialer(t, "m-victim", secret)
			if err := mesh.AddRoute(home.machineID, "home", KindLAN, forged.address, SourceManual); err != nil {
				t.Fatalf("add route: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := mesh.Invoke(ctx, home.machineID, "test:echo"); err == nil {
				t.Fatal("the victim linked to a forged machine")
			}
			if attempts := forged.enrolAttempts.Load(); attempts != 0 {
				t.Fatalf("the victim sent %d enrolment proof(s) to a forged machine", attempts)
			}
			if upstream := forged.upstream.Load(); upstream != nil {
				probe, cancelProbe := context.WithTimeout(context.Background(), time.Second)
				defer cancelProbe()
				if _, err := upstream.Invoke(probe, "test:echo"); err == nil {
					t.Fatal("the real daemon approved the forger's socket")
				}
			}
			if peer, ok := store.Peer(home.machineID); ok && peer.Linked() {
				t.Fatalf("the victim stored a credential for the forged route: %#v", peer)
			}
		})
	}
}

// A daemon holding two keys answers with proofs for both. One the dialler does not
// hold is not evidence of anything, and a key whose proof is simply absent must be
// skipped rather than condemn the address forever.
func TestAMissingProofForOneKeyDoesNotRefuseTheRouteForever(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	forged := newForgedDaemon(t, home, func(challenge map[string]any, _ string) { delete(challenge, "certProofs") })
	mesh, store := newDialer(t, "m-victim", secret)
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, forged.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mesh.Invoke(ctx, home.machineID, "test:echo"); err == nil {
		t.Fatal("the victim linked to a forged machine")
	}
	peer, _ := store.Peer(home.machineID)
	for _, route := range peer.Routes {
		if route.Refused {
			t.Fatalf("a stripped proof permanently refused the address: %#v", route)
		}
	}
}

// Found in review: a TLS chain is only bytes the peer chose to send. A forger can put
// the real certificate after its own leaf and redirect its health check to the real
// daemon, so the card reads true. Neither may satisfy the pin.
func TestTheRealCertificateInAForgedChainDoesNotSatisfyThePin(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	own, err := tlscert.Ensure(t.TempDir())
	if err != nil {
		t.Fatalf("forged certificate: %v", err)
	}
	realDER, err := tlscert.Ensure(home.stateDir)
	if err != nil {
		t.Fatalf("real certificate: %v", err)
	}
	var enrolAttempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/workass/health", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+home.address+"/workass/health", http.StatusFound)
	})
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) { enrolAttempts.Add(1) })
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(tls.NewListener(raw, &tls.Config{
			MinVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{{
				Certificate: [][]byte{own.TLS.Certificate[0], realDER.TLS.Certificate[0]},
				PrivateKey:  own.TLS.PrivateKey,
			}},
		}))
	}()
	t.Cleanup(func() { _ = server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link, err := dialLink(ctx, dialOptions{
		address: raw.Addr().String(), peerID: home.machineID, deviceName: "victim", tlsConfig: pinnedTLSConfig(home.fingerprint),
	})
	if err == nil {
		link.Close(nil)
		t.Fatal("a forged leaf passed the pin because the real certificate rode behind it")
	}

	mesh, _ := newDialer(t, "m-victim", secret)
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, raw.Addr().String(), SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := mesh.Invoke(ctx, home.machineID, "test:echo"); err == nil {
		t.Fatal("the victim linked through a redirected card and a forged chain")
	}
	if attempts := enrolAttempts.Load(); attempts != 0 {
		t.Fatalf("the forger received %d websocket attempt(s)", attempts)
	}
}

// Junk from anyone on the path must not cut a route off until a human restores it.
func TestAJunkCertificateProofDoesNotRefuseTheRouteForever(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	forged := newForgedDaemon(t, home, func(challenge map[string]any, _ string) {
		junk := map[string]any{}
		for _, id := range home.keys.KeyIDs() {
			junk[id] = strings.Repeat("0", 64)
		}
		challenge["certProofs"] = junk
	})
	mesh, store := newDialer(t, "m-victim", secret)
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, forged.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mesh.Invoke(ctx, home.machineID, "test:echo"); err == nil {
		t.Fatal("the victim linked to a forged machine")
	}
	if attempts := forged.enrolAttempts.Load(); attempts != 0 {
		t.Fatalf("the victim sent %d proof(s) after a junk certificate proof", attempts)
	}
	peer, _ := store.Peer(home.machineID)
	for _, route := range peer.Routes {
		if route.Refused {
			t.Fatalf("junk permanently refused the address: %#v", route)
		}
	}
}

// A credential from an old peer file has no pin. It must not go to whatever certificate
// a card happens to name.
func TestAStoredTokenWithNoPinIsNeverSentToAForgedMachine(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	forged := newForgedDaemon(t, home, func(challenge map[string]any, _ string) { delete(challenge, "certProofs") })
	mesh, store := newDialer(t, "m-victim", secret)
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, forged.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := store.SaveLink(home.machineID, "device-1", "key-1", "old-token", "", "home"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mesh.Invoke(ctx, home.machineID, "test:echo"); err == nil {
		t.Fatal("the victim linked to a forged machine")
	}
	if seen := forged.tokensSeen.Load(); seen != 0 {
		t.Fatalf("the forger received a device token %d time(s)", seen)
	}
}

// A forger answering once must not cut off the real machine after it leaves.
func TestARouteRecoversAfterTheForgerLeaves(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	mesh, store := newDialer(t, "m-victim", secret)
	address := home.address
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if err := store.NoteFailure(home.machineID, address, "a forged certificate answered here", time.Nanosecond); err != nil {
		t.Fatalf("note failure: %v", err)
	}
	time.Sleep(time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mesh.Invoke(ctx, home.machineID, "test:echo"); err != nil {
		t.Fatalf("the route did not recover: %v", err)
	}
}
