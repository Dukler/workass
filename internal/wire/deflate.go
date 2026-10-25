package wire

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// RFC 7692 permessage-deflate. Compression is a transport detail beneath the
// frozen invoke/reply/event protocol: the JSON a client reads and writes is
// byte-identical either way, and a client that does not offer the extension
// (Go tests, CLI tools, audit clients) gets exactly the uncompressed frames it
// always got.
//
// Both directions run without context takeover. Every message is compressed
// and inflated with a fresh context, so the daemon keeps no per-socket
// compression state: a socket costs nothing extra while idle, and a pooled
// writer serves any client.

// deflateExtensionResponse is the only acceptance the server sends. RFC 7692
// §7.1.1.1 and §7.1.1.2 let a server add both no-context-takeover parameters
// even when the offer lacked them; client_no_context_takeover is what lets the
// reader inflate each message with a fresh decompressor.
const deflateExtensionResponse = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"

// Below this size the compressed frame saves too little to pay for the
// compressor; such frames go out exactly as encoded.
const deflateMinPayload = 1024

// The level follows the payload, by measurement on real profile JSON (M-series
// Mac, 2026-10-25).
//   - Below deflateRatioMinPayload: BestSpeed. On the 1-16 KiB events that make
//     up most traffic the fresh-context Reset of levels 2+ costs 1.5-2.4x per
//     message, for a few bytes.
//   - Up to deflateRatioMaxPayload: the default level. The reads a connect and
//     a chat open wait on are here, and it sends a fifth fewer bytes for about
//     2x the CPU: the 297 KB session shell goes 67.5 -> 54.7 KB for 2 ms, an
//     8.4 MB history page 1.15 -> 0.83 MB for 35 ms. On a 400 kbps link that is
//     seconds; on a LAN a millisecond or two.
//   - Above it: BestSpeed again, so a 25 MB snapshot costs ~85 ms, not ~175.
const deflateLevel = flate.BestSpeed
const deflateRatioLevel = flate.DefaultCompression
const deflateRatioMinPayload = 16 << 10
const deflateRatioMaxPayload = 8 << 20

func deflateLevelFor(payload int) int {
	if payload >= deflateRatioMinPayload && payload <= deflateRatioMaxPayload {
		return deflateRatioLevel
	}
	return deflateLevel
}

// inboundMessageByteLimit bounds one client message: its declared frame
// lengths, its reassembled fragments, and its size after inflation. It matches
// the outbound ceiling; an invoke is far smaller than a snapshot reply.
const inboundMessageByteLimit = outboundFrameByteLimit

// Compressed frames are built behind this much reserved room so the header can
// be written in place once the compressed length is known.
const maxServerFrameHeader = 10

// Output buffers larger than this are left to the collector instead of pinning
// a snapshot-sized allocation in the pool.
const deflateBufferPoolCap = 1 << 20

// RFC 7692 §7.2.2: a sender strips the empty stored block a sync flush ends
// with; the receiver appends it back. The final empty stored block after it
// makes the decompressor finish with io.EOF instead of io.ErrUnexpectedEOF.
const deflateFlushTail = "\x00\x00\xff\xff"
const inflateMessageTail = deflateFlushTail + "\x01\x00\x00\xff\xff"

var (
	errFrameReservedBits      = errors.New("websocket frame sets a reserved bit")
	errFrameCompressionBit    = errors.New("websocket frame sets RSV1 where it cannot mean compressed")
	errInboundMessageTooLarge = errors.New("websocket message exceeds the inbound limit")
)

// deflateSink lets a pooled flate.Writer point at a different buffer per
// message and at nothing while pooled, so the pool never pins a buffer.
type deflateSink struct {
	buf *bytes.Buffer
}

func (s *deflateSink) Write(p []byte) (int, error) {
	if s.buf == nil {
		return 0, io.ErrClosedPipe
	}
	return s.buf.Write(p)
}

type deflateWriter struct {
	sink deflateSink
	fw   *flate.Writer
}

// flate.NewWriter allocates its window and match tables; Reset reuses them.
// One pool per level: a writer keeps the level it was made with.
func newDeflateWriterPool(level int) *sync.Pool {
	return &sync.Pool{New: func() any {
		w := &deflateWriter{}
		fw, err := flate.NewWriter(&w.sink, level)
		if err != nil {
			return nil
		}
		w.fw = fw
		return w
	}}
}

var deflateWriterPool = newDeflateWriterPool(deflateLevel)
var deflateRatioWriterPool = newDeflateWriterPool(deflateRatioLevel)

var deflateBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

var inflateReaderPool sync.Pool

// negotiatePermessageDeflate reports whether any Sec-WebSocket-Extensions
// offer is one the server can honor, and the response value to send. The
// first acceptable offer wins; an offer with an unknown, duplicated or invalid
// parameter is declined and the next one is considered.
func negotiatePermessageDeflate(header http.Header) (string, bool) {
	for _, value := range header.Values("Sec-WebSocket-Extensions") {
		for _, offer := range splitHeaderList(value, ',') {
			parts := splitHeaderList(offer, ';')
			if len(parts) == 0 || !strings.EqualFold(strings.TrimSpace(parts[0]), "permessage-deflate") {
				continue
			}
			if response, ok := acceptDeflateOffer(parts[1:]); ok {
				return response, true
			}
		}
	}
	return "", false
}

func acceptDeflateOffer(params []string) (string, bool) {
	seen := map[string]bool{}
	serverWindowEcho := false
	for _, param := range params {
		name, value, hasValue := strings.Cut(param, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || seen[name] {
			return "", false
		}
		seen[name] = true
		if hasValue {
			unquoted, ok := unquoteHeaderValue(strings.TrimSpace(value))
			if !ok {
				return "", false
			}
			value = unquoted
		}
		switch name {
		case "server_no_context_takeover", "client_no_context_takeover":
			if hasValue {
				return "", false
			}
		case "client_max_window_bits":
			// The client may cap its own window; the 32 KiB inflate window
			// reads any size up to 15 bits.
			if hasValue && !validWindowBits(value) {
				return "", false
			}
		case "server_max_window_bits":
			// compress/flate always uses a 32 KiB window, so only 15 is a
			// limit the server can keep.
			if !hasValue || !validWindowBits(value) || value != "15" {
				return "", false
			}
			serverWindowEcho = true
		default:
			return "", false
		}
	}
	if serverWindowEcho {
		// RFC 7692 §7.1.2.1: accepting an offer that carries
		// server_max_window_bits means answering with it.
		return deflateExtensionResponse + "; server_max_window_bits=15", true
	}
	return deflateExtensionResponse, true
}

// RFC 7692 §7.1.2: a decimal 8 to 15 without leading zeros.
func validWindowBits(value string) bool {
	if value == "" || len(value) > 2 || value[0] == '0' || strings.TrimLeft(value, "0123456789") != "" {
		return false
	}
	bits, err := strconv.Atoi(value)
	return err == nil && bits >= 8 && bits <= 15
}

// splitHeaderList splits on sep outside quoted strings and drops empty items,
// which RFC 7230 list syntax allows.
func splitHeaderList(value string, sep byte) []string {
	var items []string
	start := 0
	quoted := false
	for i := 0; i < len(value); i++ {
		switch {
		case quoted && value[i] == '\\':
			i++
		case value[i] == '"':
			quoted = !quoted
		case !quoted && value[i] == sep:
			if item := strings.TrimSpace(value[start:i]); item != "" {
				items = append(items, item)
			}
			start = i + 1
		}
	}
	if item := strings.TrimSpace(value[start:]); item != "" {
		items = append(items, item)
	}
	return items
}

func unquoteHeaderValue(value string) (string, bool) {
	if !strings.HasPrefix(value, `"`) {
		return value, !strings.Contains(value, `"`)
	}
	if len(value) < 2 || !strings.HasSuffix(value, `"`) {
		return "", false
	}
	var out strings.Builder
	inner := value[1 : len(value)-1]
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '\\':
			i++
			if i == len(inner) {
				return "", false
			}
		case '"':
			return "", false
		}
		out.WriteByte(inner[i])
	}
	return out.String(), true
}

// serverTextFramePayload returns the payload of one complete, unmasked,
// unfragmented server text frame as encodeTextFrame builds it. Anything else
// is not compressed.
func serverTextFramePayload(frame []byte) ([]byte, bool) {
	if len(frame) < 2 || frame[0] != 0x81 || frame[1]&0x80 != 0 {
		return nil, false
	}
	headerLen := 2
	length := uint64(frame[1] & 0x7f)
	switch length {
	case 126:
		headerLen = 4
		if len(frame) < headerLen {
			return nil, false
		}
		length = uint64(binary.BigEndian.Uint16(frame[2:4]))
	case 127:
		headerLen = 10
		if len(frame) < headerLen {
			return nil, false
		}
		length = binary.BigEndian.Uint64(frame[2:10])
	}
	if length != uint64(len(frame)-headerLen) {
		return nil, false
	}
	return frame[headerLen:], true
}

// deflateTextFrame compresses an encoded server text frame into a new frame
// with RSV1 set. The input is never modified: Broadcast shares one encoded
// frame among clients. The returned buffer backs the returned frame and goes
// back through releaseDeflateBuffer once the frame is written. ok is false when
// the frame should be written as it is: not a text frame, too small, not
// smaller once compressed, or the compressor failed.
func deflateTextFrame(frame []byte) (*bytes.Buffer, []byte, bool) {
	payload, ok := serverTextFramePayload(frame)
	if !ok || len(payload) < deflateMinPayload {
		return nil, nil, false
	}
	pool := deflateWriterPool
	if deflateLevelFor(len(payload)) == deflateRatioLevel {
		pool = deflateRatioWriterPool
	}
	w, _ := pool.Get().(*deflateWriter)
	if w == nil {
		return nil, nil, false
	}
	buf := deflateBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	// JSON at BestSpeed lands near a fifth of its size; start there so a
	// snapshot does not regrow its buffer from zero.
	buf.Grow(maxServerFrameHeader + len(payload)/4 + 64)
	var reserved [maxServerFrameHeader]byte
	buf.Write(reserved[:])

	w.sink.buf = buf
	w.fw.Reset(&w.sink)
	_, err := w.fw.Write(payload)
	if err == nil {
		// A sync flush ends the message on a byte boundary without a final
		// block, so the next message's fresh context starts cleanly too.
		err = w.fw.Flush()
	}
	w.sink.buf = nil
	pool.Put(w)

	data := buf.Bytes()
	compressed := data[maxServerFrameHeader:]
	if err != nil || !bytes.HasSuffix(compressed, []byte(deflateFlushTail)) {
		releaseDeflateBuffer(buf)
		return nil, nil, false
	}
	compressed = compressed[:len(compressed)-len(deflateFlushTail)]
	if len(compressed) >= len(payload) {
		releaseDeflateBuffer(buf)
		return nil, nil, false
	}
	headerLen := textFrameHeaderLen(len(compressed))
	start := maxServerFrameHeader - headerLen
	writeTextFrameHeader(data[start:maxServerFrameHeader], len(compressed))
	// FIN | RSV1 | text: RSV1 marks the message as compressed (RFC 7692 §6).
	data[start] |= 0x40
	return buf, data[start : maxServerFrameHeader+len(compressed)], true
}

func releaseDeflateBuffer(buf *bytes.Buffer) {
	if buf == nil || buf.Cap() > deflateBufferPoolCap {
		return
	}
	buf.Reset()
	deflateBufferPool.Put(buf)
}

// writeFrame writes one queued frame, compressing it for a client that
// negotiated permessage-deflate. Compression finishes before the first write
// deadline is armed, so CPU time never counts as a stalled peer; the
// compressed frame is what goes out in progress-checked chunks, and what a
// large-frame announcement measures.
func (c *client) writeFrame(frame []byte) error {
	if c.deflate {
		if buf, compressed, ok := deflateTextFrame(frame); ok {
			err := c.writeAnnounced(compressed)
			releaseDeflateBuffer(buf)
			return err
		}
	}
	return c.writeAnnounced(frame)
}

// inflateSource feeds a compressed message and its RFC 7692 tail to the
// decompressor without copying the message. Being an io.ByteReader keeps
// flate from wrapping it in a bufio.Reader of its own.
type inflateSource struct {
	message []byte
	tail    string
}

func (s *inflateSource) Read(p []byte) (int, error) {
	if len(s.message) > 0 {
		n := copy(p, s.message)
		s.message = s.message[n:]
		return n, nil
	}
	if len(s.tail) > 0 {
		n := copy(p, s.tail)
		s.tail = s.tail[n:]
		return n, nil
	}
	return 0, io.EOF
}

func (s *inflateSource) ReadByte() (byte, error) {
	if len(s.message) > 0 {
		b := s.message[0]
		s.message = s.message[1:]
		return b, nil
	}
	if len(s.tail) > 0 {
		b := s.tail[0]
		s.tail = s.tail[1:]
		return b, nil
	}
	return 0, io.EOF
}

// inflateMessage decompresses one client message with a fresh context (the
// server demanded client_no_context_takeover). It reads at most limit+1 bytes,
// so a small message that inflates past the limit costs the limit, not the
// whole bomb.
func inflateMessage(compressed []byte, limit int) ([]byte, error) {
	src := &inflateSource{message: compressed, tail: inflateMessageTail}
	reader, _ := inflateReaderPool.Get().(io.ReadCloser)
	if reader == nil {
		reader = flate.NewReader(src)
	} else if err := reader.(flate.Resetter).Reset(src, nil); err != nil {
		return nil, err
	}
	defer func() {
		// Drop the message before pooling the decompressor.
		_ = reader.(flate.Resetter).Reset(&inflateSource{}, nil)
		inflateReaderPool.Put(reader)
	}()
	var out bytes.Buffer
	out.Grow(min(4*len(compressed)+512, limit+1))
	n, err := out.ReadFrom(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("inflate websocket message: %w", err)
	}
	if n > int64(limit) {
		return nil, errInboundMessageTooLarge
	}
	return out.Bytes(), nil
}
