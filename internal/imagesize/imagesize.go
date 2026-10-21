// Package imagesize reads an image's pixel size from its header. It uses only
// the standard library: image.DecodeConfig with PNG, JPEG and GIF registered,
// plus a hand-written parser for the first WebP chunk (the standard library has
// no WebP decoder and Workass takes no third-party dependency).
package imagesize

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
)

// webpHeaderBytes is enough for the RIFF header, the first chunk header and
// every field the size lives in (VP8, VP8L and VP8X).
const webpHeaderBytes = 30

// Dimensions returns the pixel size of an encoded image held in memory.
// mimeType "image/webp" selects the WebP chunk parser; any other type is read
// with image.DecodeConfig.
func Dimensions(data []byte, mimeType string) (int, int, error) {
	if mimeType == "image/webp" {
		return webpDimensions(data)
	}
	return decodeConfig(bytes.NewReader(data))
}

// ReadDimensions reads only the header of an encoded image from r and returns
// its pixel size. WebP is recognised by its RIFF magic, so no MIME type is
// needed. It never reads the pixel data: DecodeConfig on a stream stops after
// the header, so a caller may pass a large file or a base64 decoder over one.
func ReadDimensions(r io.Reader) (int, int, error) {
	buffered := bufio.NewReader(r)
	if head, _ := buffered.Peek(webpHeaderBytes); isWebP(head) {
		return webpDimensions(head)
	}
	return decodeConfig(buffered)
}

func decodeConfig(r io.Reader) (int, int, error) {
	config, _, err := image.DecodeConfig(r)
	if err != nil {
		return 0, 0, err
	}
	if config.Width <= 0 || config.Height <= 0 {
		return 0, 0, errors.New("image has no pixels")
	}
	return config.Width, config.Height, nil
}

func isWebP(head []byte) bool {
	return len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP"
}

// webpDimensions reads the canvas size from the first RIFF chunk.
func webpDimensions(data []byte) (int, int, error) {
	if len(data) < webpHeaderBytes || !isWebP(data) {
		return 0, 0, errors.New("truncated WebP header")
	}
	chunk, body := string(data[12:16]), data[20:]
	switch chunk {
	case "VP8 ":
		if len(body) < 10 || body[3] != 0x9d || body[4] != 0x01 || body[5] != 0x2a {
			return 0, 0, errors.New("invalid VP8 frame header")
		}
		return int(binary.LittleEndian.Uint16(body[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(body[8:10]) & 0x3fff), nil
	case "VP8L":
		if len(body) < 5 || body[0] != 0x2f {
			return 0, 0, errors.New("invalid VP8L header")
		}
		bits := binary.LittleEndian.Uint32(body[1:5])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	case "VP8X":
		if len(body) < 10 {
			return 0, 0, errors.New("invalid VP8X header")
		}
		width := int(body[4]) | int(body[5])<<8 | int(body[6])<<16
		height := int(body[7]) | int(body[8])<<8 | int(body[9])<<16
		return width + 1, height + 1, nil
	}
	return 0, 0, fmt.Errorf("unsupported WebP chunk %q", chunk)
}
