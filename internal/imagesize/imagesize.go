// Package imagesize reads an image's pixel size from its header. It uses only
// the standard library: image.DecodeConfig with PNG, JPEG and GIF registered,
// plus a hand-written parser for the first WebP chunk (the standard library has
// no WebP decoder and Workass takes no third-party dependency).
//
// The size is the one a browser shows. A JPEG whose EXIF orientation turns it
// a quarter (5 to 8, as phone cameras write for a portrait shot) is shown with
// its sides swapped, since image-orientation: from-image is the default, so
// its width and height are swapped here too.
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

// maxJPEGOrientationBytes bounds what ReadDimensions keeps of a JPEG's header
// to find its EXIF orientation. EXIF precedes the frame header and is one
// APP1 segment of at most 64 KiB, normally the first segment.
const maxJPEGOrientationBytes = 256 << 10

// Dimensions returns the pixel size of an encoded image held in memory, as a
// browser shows it. mimeType "image/webp" selects the WebP chunk parser; any
// other type is read with image.DecodeConfig.
func Dimensions(data []byte, mimeType string) (int, int, error) {
	if mimeType == "image/webp" {
		return webpDimensions(data)
	}
	width, height, err := decodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	if jpegOrientationSwapsSides(data) {
		width, height = height, width
	}
	return width, height, nil
}

// ReadDimensions reads only the header of an encoded image from r and returns
// its pixel size, as a browser shows it. WebP is recognised by its RIFF magic,
// so no MIME type is needed. It never reads the pixel data: DecodeConfig on a
// stream stops after the header, so a caller may pass a large file or a
// base64 decoder over one.
func ReadDimensions(r io.Reader) (int, int, error) {
	buffered := bufio.NewReader(r)
	head, _ := buffered.Peek(webpHeaderBytes)
	if isWebP(head) {
		return webpDimensions(head)
	}
	if !isJPEG(head) {
		return decodeConfig(buffered)
	}
	// The JPEG decoder reads every segment before the frame header, EXIF
	// included; keep those bytes to read the orientation from.
	seen := &prefixBuffer{limit: maxJPEGOrientationBytes}
	width, height, err := decodeConfig(io.TeeReader(buffered, seen))
	if err != nil {
		return 0, 0, err
	}
	if jpegOrientationSwapsSides(seen.data) {
		width, height = height, width
	}
	return width, height, nil
}

// prefixBuffer keeps the first limit bytes written to it and drops the rest.
type prefixBuffer struct {
	data  []byte
	limit int
}

func (b *prefixBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.data); room > 0 {
		b.data = append(b.data, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func isJPEG(head []byte) bool {
	return len(head) >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff
}

// jpegOrientationSwapsSides reports whether data is a JPEG whose EXIF
// orientation (TIFF tag 0x0112) is 5 to 8: transposed or turned a quarter, so
// it is shown with width and height swapped. Anything unreadable is the
// default orientation, as browsers treat it.
func jpegOrientationSwapsSides(data []byte) bool {
	orientation := jpegOrientation(data)
	return orientation >= 5 && orientation <= 8
}

// jpegOrientation walks the segments before the frame header and returns the
// orientation of the first EXIF APP1 segment, or 1.
func jpegOrientation(data []byte) int {
	if !isJPEG(data) {
		return 1
	}
	for pos := 2; pos+4 <= len(data); {
		if data[pos] != 0xff {
			return 1
		}
		marker := data[pos+1]
		switch {
		case marker == 0xff: // fill byte
			pos++
			continue
		case marker == 0x01 || (marker >= 0xd0 && marker <= 0xd8): // no length
			pos += 2
			continue
		case marker == 0xd9 || marker == 0xda: // end of image, start of scan
			return 1
		case marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc:
			return 1 // the frame header: EXIF comes before it
		}
		length := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		end := pos + 2 + length
		if length < 2 || end > len(data) {
			return 1
		}
		if segment := data[pos+4 : end]; marker == 0xe1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			return exifOrientation(segment[6:])
		}
		pos = end
	}
	return 1
}

// exifOrientation reads tag 0x0112 from IFD0 of a TIFF block, or returns 1.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	ifd := uint64(order.Uint32(tiff[4:8]))
	if ifd+2 > uint64(len(tiff)) {
		return 1
	}
	count := uint64(order.Uint16(tiff[ifd : ifd+2]))
	for index := uint64(0); index < count; index++ {
		entry := ifd + 2 + 12*index
		if entry+12 > uint64(len(tiff)) {
			return 1
		}
		if order.Uint16(tiff[entry:entry+2]) != 0x0112 {
			continue
		}
		// SHORT (3), one value, held in the entry itself.
		if order.Uint16(tiff[entry+2:entry+4]) != 3 || order.Uint32(tiff[entry+4:entry+8]) != 1 {
			return 1
		}
		if value := int(order.Uint16(tiff[entry+8 : entry+10])); value >= 1 && value <= 8 {
			return value
		}
		return 1
	}
	return 1
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
		width, height := int(binary.LittleEndian.Uint16(body[6:8])&0x3fff), int(binary.LittleEndian.Uint16(body[8:10])&0x3fff)
		if width == 0 || height == 0 {
			// VP8L and VP8X store size minus one; only a lossy frame can say zero.
			return 0, 0, errors.New("image has no pixels")
		}
		return width, height, nil
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
