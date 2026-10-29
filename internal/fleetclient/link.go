package fleetclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Link is one authenticated duplex connection to one peer daemon.
//
// It speaks the frozen `invoke/reply/event` protocol as a CLIENT — the one
// capability this daemon has never had. `internal/wire` only ever *accepted*
// websockets, which is why the renderer was the fleet's only bridge; this file
// is the other half of that socket, written against the same frames the hub
// already writes and reads.
//
// Deliberately small: dial, invoke, receive events, heartbeat, state. It holds
// no Workass semantics at all, because §6's whole point is that a link carries
// the command surface that already exists rather than a second API.
type Link struct {
	peerID  string
	address string
	conn    net.Conn
	reader  *bufio.Reader

	writeMu sync.Mutex
	seq     atomic.Int64

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  atomic.Pointer[error]

	mu      sync.Mutex
	pending map[string]chan reply
	// access is the peer's verdict on this socket. The hub publishes it as an
	// event rather than as a reply — a rejected client is told and then dropped,
	// so the refusal may well arrive instead of the reply, not after it.
	accessState  string
	accessReason string

	onEvent func(peerID, channel string, payload any)
	logf    func(string, ...any)
}

// AccessState is the peer's verdict on this socket, if it stated one.
func (l *Link) AccessState() (string, string) {
	if l == nil {
		return "", ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accessState, l.accessReason
}

type reply struct {
	result any
	err    error
}

// writeTimeout bounds one frame write. A peer that stops reading must not park
// an invoke forever behind a full socket buffer.
const writeTimeout = 15 * time.Second

// ErrLinkClosed reports an invoke on a link whose socket is gone.
var ErrLinkClosed = errors.New("peer link closed")

// ErrAccessRejected reports a peer that refused this daemon's credential. It is
// its own error because the answer is not "retry": either the token is stale
// (enrol again) or the link was revoked (stop).
var ErrAccessRejected = errors.New("peer refused this link")

type dialOptions struct {
	address     string
	peerID      string
	deviceName  string
	deviceToken string
	tlsConfig   *tls.Config
	onEvent     func(peerID, channel string, payload any)
	logf        func(string, ...any)
}

// dialLink opens one socket. It performs no identity check of its own: the
// caller has already proven the address IS this peer, which is the rule that
// makes presenting a credential here safe (§4).
func dialLink(ctx context.Context, opts dialOptions) (*Link, error) {
	dialer := tls.Dialer{NetDialer: &net.Dialer{}, Config: opts.tlsConfig}
	conn, err := dialer.DialContext(ctx, "tcp", opts.address)
	if err != nil {
		return nil, err
	}
	path := "/?deviceName=" + url.QueryEscape(opts.deviceName)
	if strings.TrimSpace(opts.deviceToken) != "" {
		path += "&deviceToken=" + url.QueryEscape(opts.deviceToken)
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nUser-Agent: workass-fleet-link\r\n\r\n",
		path, opts.address, key)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, request); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(strings.TrimSpace(status), "HTTP/1.1 101") {
		_ = conn.Close()
		return nil, fmt.Errorf("peer %s answered the websocket handshake with %q", opts.address, strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	_ = conn.SetDeadline(time.Time{})
	link := &Link{
		peerID:  opts.peerID,
		address: opts.address,
		conn:    conn,
		reader:  reader,
		closed:  make(chan struct{}),
		pending: make(map[string]chan reply),
		onEvent: opts.onEvent,
		logf:    opts.logf,
	}
	go link.readLoop()
	return link, nil
}

// Peer is the machine id on the other end.
func (l *Link) Peer() string { return l.peerID }

// Address is the route this link is using.
func (l *Link) Address() string { return l.address }

// Done closes when the socket is gone.
func (l *Link) Done() <-chan struct{} { return l.closed }

// Err reports why the link ended, if it has.
func (l *Link) Err() error {
	if stored := l.closeErr.Load(); stored != nil {
		return *stored
	}
	return nil
}

// Invoke sends one `invoke` and waits for its `reply`, which is exactly what a
// renderer does. A forwarded Workass command is one of these and nothing more.
func (l *Link) Invoke(ctx context.Context, channel string, args ...any) (any, error) {
	if l == nil {
		return nil, ErrLinkClosed
	}
	id := strconv.FormatInt(l.seq.Add(1), 10)
	replies := make(chan reply, 1)
	l.mu.Lock()
	if l.pending == nil {
		l.mu.Unlock()
		return nil, ErrLinkClosed
	}
	l.pending[id] = replies
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, id)
		l.mu.Unlock()
	}()
	if args == nil {
		args = []any{}
	}
	payload, err := json.Marshal(map[string]any{"t": "invoke", "id": id, "channel": channel, "args": args})
	if err != nil {
		return nil, err
	}
	if err := l.writeFrame(maskedTextFrame(payload)); err != nil {
		return nil, err
	}
	select {
	case answer := <-replies:
		return answer.result, answer.err
	case <-l.closed:
		if err := l.Err(); err != nil {
			return nil, err
		}
		return nil, ErrLinkClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close ends the link. It is idempotent, so a heartbeat and a revocation
// racing to close one socket is not a double-close panic.
func (l *Link) Close(cause error) {
	if l == nil {
		return
	}
	l.closeOnce.Do(func() {
		if cause != nil {
			l.closeErr.CompareAndSwap(nil, &cause)
		}
		_ = l.conn.Close()
		close(l.closed)
		l.mu.Lock()
		pending := l.pending
		l.pending = nil
		l.mu.Unlock()
		for _, waiter := range pending {
			select {
			case waiter <- reply{err: wrapClosed(cause)}:
			default:
			}
		}
	})
}

func wrapClosed(cause error) error {
	if cause == nil {
		return ErrLinkClosed
	}
	return fmt.Errorf("%w: %v", ErrLinkClosed, cause)
}

func (l *Link) readLoop() {
	for {
		payload, err := l.readDataFrame()
		if err != nil {
			l.Close(err)
			return
		}
		var frame struct {
			T       string          `json:"t"`
			ID      json.RawMessage `json:"id"`
			Channel string          `json:"channel"`
			Result  any             `json:"result"`
			Error   *string         `json:"error"`
			Payload any             `json:"payload"`
		}
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.UseNumber()
		if err := decoder.Decode(&frame); err != nil {
			continue
		}
		switch frame.T {
		case "reply":
			id := strings.Trim(strings.TrimSpace(string(frame.ID)), `"`)
			l.mu.Lock()
			waiter := l.pending[id]
			if waiter != nil {
				delete(l.pending, id)
			}
			l.mu.Unlock()
			if waiter == nil {
				continue
			}
			answer := reply{result: frame.Result}
			if frame.Error != nil && strings.TrimSpace(*frame.Error) != "" {
				answer.err = errors.New(*frame.Error)
			}
			select {
			case waiter <- answer:
			default:
			}
		case "event":
			if frame.Channel == "lan:access-state" {
				state, _ := frame.Payload.(map[string]any)
				l.mu.Lock()
				l.accessState, _ = state["state"].(string)
				l.accessReason, _ = state["reason"].(string)
				l.mu.Unlock()
			}
			if l.onEvent != nil {
				l.onEvent(l.peerID, frame.Channel, frame.Payload)
			}
		}
	}
}

func (l *Link) writeFrame(frame []byte) error {
	select {
	case <-l.closed:
		return ErrLinkClosed
	default:
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	_ = l.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	written, err := l.conn.Write(frame)
	if err == nil && written != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

// readDataFrame returns the next complete text message, answering pings along
// the way. The hub pings idle clients and closes the ones that never answer,
// so a link that only ever read replies would be dropped for silence.
func (l *Link) readDataFrame() ([]byte, error) {
	var fragment []byte
	for {
		fin, opcode, payload, err := readFrame(l.reader)
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 0x1:
			if fin {
				return payload, nil
			}
			fragment = append(fragment[:0], payload...)
		case 0x0:
			fragment = append(fragment, payload...)
			if fin {
				return append([]byte(nil), fragment...), nil
			}
		case 0x8:
			return nil, io.EOF
		case 0x9:
			pong := maskedTextFrame(payload)
			pong[0] = 0x8A
			if err := l.writeFrame(pong); err != nil {
				return nil, err
			}
		case 0xA:
		default:
			return nil, fmt.Errorf("peer sent an unexpected websocket opcode %d", opcode)
		}
	}
}

// inboundFrameLimit bounds one frame a peer may send us. The hub's own inbound
// limit is the mirror of this; a peer that declares more than this is either
// broken or hostile, and either way the link ends rather than allocating it.
const inboundFrameLimit = 256 << 20

func readFrame(reader *bufio.Reader) (bool, byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return false, 0, nil, err
	}
	fin := header[0]&0x80 != 0
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if length > inboundFrameLimit {
		return false, 0, nil, fmt.Errorf("peer declared a %d byte websocket frame", length)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return fin, opcode, payload, nil
}

// maskedTextFrame builds one client text frame. A client MUST mask (RFC 6455
// §5.3) and the hub enforces nothing about it, but an unmasked client frame is
// a protocol violation that a stricter peer would close the socket over.
func maskedTextFrame(payload []byte) []byte {
	headerLen := 2
	switch {
	case len(payload) < 126:
	case len(payload) <= 0xffff:
		headerLen = 4
	default:
		headerLen = 10
	}
	out := make([]byte, headerLen+4+len(payload))
	out[0] = 0x81
	switch {
	case len(payload) < 126:
		out[1] = 0x80 | byte(len(payload))
	case len(payload) <= 0xffff:
		out[1] = 0x80 | 126
		binary.BigEndian.PutUint16(out[2:4], uint16(len(payload)))
	default:
		out[1] = 0x80 | 127
		binary.BigEndian.PutUint64(out[2:10], uint64(len(payload)))
	}
	mask := out[headerLen : headerLen+4]
	if _, err := rand.Read(mask); err != nil {
		copy(mask, []byte{1, 2, 3, 4})
	}
	for i, b := range payload {
		out[headerLen+4+i] = b ^ mask[i&3]
	}
	return out
}
