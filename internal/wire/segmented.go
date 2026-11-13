package wire

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
)

// Compressed once, sent many times.
//
// A reply that a daemon keeps (a chat's first screen, a page of a folded
// row's calls) used to be compressed again for every socket that read it: a
// 10 MB expansion spent about 40 ms of its time in the compressor, every time.
// A SegmentedResult holds the reply's JSON as parts, each either literal bytes
// or a DEFLATE segment that was compressed once, when the reply was kept.
//
// Every connection compresses each message with a fresh window
// (server_no_context_takeover, deflateTextFrame), so a message is any run of
// DEFLATE blocks that inflates to its bytes. A segment is a stream of
// non-final blocks that ends byte-aligned (a sync flush); a literal goes out
// as stored blocks. Their concatenation is one valid message whose inflated
// bytes are exactly the parts' bytes in order: the frame a client inflates is
// byte for byte the reply it would have received uncompressed. A socket
// without permessage-deflate, and any caller that marshals the result,
// receives the plain bytes.

// SegmentedPart is one part of a SegmentedResult: exactly one field is set.
type SegmentedPart struct {
	// Literal is sent as it is.
	Literal []byte
	// Deflated is a DEFLATE segment (DeflateSegment) that inflates to Size
	// bytes; Plain, when set, is those bytes.
	Deflated []byte
	Size     int
	Plain    []byte
}

// SegmentedResult is a pre-serialized JSON result held in parts.
type SegmentedResult struct {
	Parts []SegmentedPart
}

// MarshalJSON is the result's plain bytes.
func (r SegmentedResult) MarshalJSON() ([]byte, error) {
	return r.PlainBytes()
}

// PlainBytes is the result's JSON.
func (r SegmentedResult) PlainBytes() ([]byte, error) {
	size := 0
	for _, part := range r.Parts {
		size += len(part.Literal) + part.Size
	}
	out := make([]byte, 0, size+rawReplySpareBytes)
	for _, part := range r.Parts {
		switch {
		case part.Deflated == nil:
			out = append(out, part.Literal...)
		case part.Plain != nil:
			out = append(out, part.Plain...)
		default:
			var err error
			if out, err = InflateSegment(out, part.Deflated); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// rawReplySpareBytes is room for the reply envelope, so framing the plain
// bytes does not copy them again.
const rawReplySpareBytes = 64

// SegmentLevel is the level kept segments are compressed at: they are
// compressed once and sent many times, so they are worth the default level's
// smaller output.
const SegmentLevel = flate.DefaultCompression

var segmentWriterPool = newDeflateWriterPool(SegmentLevel)

// DeflateSegment compresses plain into a segment: non-final DEFLATE blocks
// with a fresh window, ending in a sync flush.
func DeflateSegment(plain []byte) []byte {
	w, _ := segmentWriterPool.Get().(*deflateWriter)
	if w == nil {
		return nil
	}
	var buf bytes.Buffer
	buf.Grow(len(plain)/4 + 64)
	w.sink.buf = &buf
	w.fw.Reset(&w.sink)
	_, err := w.fw.Write(plain)
	if err == nil {
		err = w.fw.Flush()
	}
	w.sink.buf = nil
	segmentWriterPool.Put(w)
	if err != nil || !bytes.HasSuffix(buf.Bytes(), []byte(deflateFlushTail)) {
		return nil
	}
	return buf.Bytes()
}

var segmentReaderPool sync.Pool

// InflateSegment appends a segment's inflated bytes to out.
func InflateSegment(out, segment []byte) ([]byte, error) {
	source := &inflateSource{message: segment, tail: "\x01\x00\x00\xff\xff"}
	var reader io.ReadCloser
	if pooled, ok := segmentReaderPool.Get().(io.ReadCloser); ok {
		if err := pooled.(flate.Resetter).Reset(source, nil); err == nil {
			reader = pooled
		}
	}
	if reader == nil {
		reader = flate.NewReader(source)
	}
	buffer := bytes.NewBuffer(out)
	_, err := buffer.ReadFrom(reader)
	segmentReaderPool.Put(reader)
	if err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

var errSegmentedEmpty = errors.New("wire segmented result has no parts")

// storedBlocks appends data as non-final stored DEFLATE blocks.
func storedBlocks(out, data []byte) []byte {
	for len(data) > 0 {
		n := min(len(data), 0xffff)
		var header [5]byte
		header[0] = 0 // BFINAL 0, BTYPE 00, padded to the byte
		binary.LittleEndian.PutUint16(header[1:3], uint16(n))
		binary.LittleEndian.PutUint16(header[3:5], ^uint16(n))
		out = append(out, header[:]...)
		out = append(out, data[:n]...)
		data = data[n:]
	}
	return out
}

// storedBlocksLen is len(storedBlocks(nil, data)).
func storedBlocksLen(size int) int {
	return size + 5*((size+0xfffe)/0xffff)
}

// encodeSegmentedReplyFrame frames {"t":"reply","id":id,"result":R,"error":null}
// for a socket that negotiated permessage-deflate: a compressed (RSV1) text
// frame whose message inflates to exactly those bytes. replyBytes is the
// inflated size.
func encodeSegmentedReplyFrame(id any, result SegmentedResult) ([]byte, int, error) {
	if len(result.Parts) == 0 {
		return nil, 0, errSegmentedEmpty
	}
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, 0, err
	}
	prefix := make([]byte, 0, len(idJSON)+32)
	prefix = append(prefix, `{"t":"reply","id":`...)
	prefix = append(prefix, idJSON...)
	prefix = append(prefix, `,"result":`...)
	const suffix = `,"error":null}`
	// The message ends with the header byte of an empty stored block: the
	// receiver appends its length (RFC 7692 §7.2.2), as after a sync flush.
	compressed := storedBlocksLen(len(prefix)) + storedBlocksLen(len(suffix)) + 1
	replyBytes := len(prefix) + len(suffix)
	for _, part := range result.Parts {
		if part.Deflated != nil {
			compressed += len(part.Deflated)
			replyBytes += part.Size
		} else {
			compressed += storedBlocksLen(len(part.Literal))
			replyBytes += len(part.Literal)
		}
	}
	headerLen := textFrameHeaderLen(compressed)
	frame := make([]byte, headerLen, headerLen+compressed)
	writeTextFrameHeader(frame, compressed)
	// FIN | RSV1 | text: the message is compressed (RFC 7692 §6).
	frame[0] |= 0x40
	frame = storedBlocks(frame, prefix)
	for _, part := range result.Parts {
		if part.Deflated != nil {
			frame = append(frame, part.Deflated...)
		} else {
			frame = storedBlocks(frame, part.Literal)
		}
	}
	frame = storedBlocks(frame, []byte(suffix))
	frame = append(frame, 0)
	if len(frame) != headerLen+compressed {
		return nil, 0, errors.New("wire segmented frame length mismatch: " + strconv.Itoa(len(frame)) + " != " + strconv.Itoa(headerLen+compressed))
	}
	return frame, replyBytes, nil
}
