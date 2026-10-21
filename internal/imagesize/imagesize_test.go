package imagesize

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand"
	"testing"
)

func encoded(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func webpHeader(chunk string, body []byte) []byte {
	data := append([]byte("RIFF\x00\x00\x00\x00WEBP"+chunk+"\x00\x00\x00\x00"), body...)
	return append(data, make([]byte, 16)...)
}

func webpFixtures() map[string][]byte {
	lossy := []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(lossy[6:], 1440)
	binary.LittleEndian.PutUint16(lossy[8:], 900)
	lossless := make([]byte, 5)
	lossless[0] = 0x2f
	binary.LittleEndian.PutUint32(lossless[1:], uint32(851)|uint32(719)<<14)
	extended := []byte{0, 0, 0, 0, 0xb0, 0x04, 0, 0x7f, 0x02, 0}
	return map[string][]byte{
		"VP8 1440x900":  webpHeader("VP8 ", lossy),
		"VP8L 852x720":  webpHeader("VP8L", lossless),
		"VP8X 1201x640": webpHeader("VP8X", extended),
	}
}

func TestDimensionsStandardFormats(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif"} {
		data := encoded(t, format, 37, 21)
		if w, h, err := Dimensions(data, "image/"+format); err != nil || w != 37 || h != 21 {
			t.Fatalf("%s bytes = %d x %d, %v", format, w, h, err)
		}
		if w, h, err := ReadDimensions(bytes.NewReader(data)); err != nil || w != 37 || h != 21 {
			t.Fatalf("%s reader = %d x %d, %v", format, w, h, err)
		}
	}
}

func TestDimensionsWebP(t *testing.T) {
	want := map[string][2]int{"VP8 1440x900": {1440, 900}, "VP8L 852x720": {852, 720}, "VP8X 1201x640": {1201, 640}}
	for name, data := range webpFixtures() {
		if w, h, err := Dimensions(data, "image/webp"); err != nil || w != want[name][0] || h != want[name][1] {
			t.Fatalf("%s bytes = %d x %d, %v", name, w, h, err)
		}
		// The reader form needs no MIME type: the RIFF magic selects WebP.
		if w, h, err := ReadDimensions(bytes.NewReader(data)); err != nil || w != want[name][0] || h != want[name][1] {
			t.Fatalf("%s reader = %d x %d, %v", name, w, h, err)
		}
	}
}

func TestDimensionsUnreadableInputHasNoSize(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="640" height="480"></svg>`)
	truncated := encoded(t, "png", 40, 30)[:12]
	cases := map[string][]byte{
		"svg":       svg,
		"empty":     nil,
		"damaged":   []byte("not an image at all, just text"),
		"truncated": truncated,
		"bad webp":  webpHeader("VP8 ", make([]byte, 10)),
		"odd webp":  webpHeader("ALPH", make([]byte, 10)),
	}
	for name, data := range cases {
		if w, h, err := ReadDimensions(bytes.NewReader(data)); err == nil {
			t.Fatalf("%s reader = %d x %d, want an error", name, w, h)
		}
	}
	if _, _, err := Dimensions(svg, "image/svg+xml"); err == nil {
		t.Fatal("svg bytes returned a size")
	}
	if _, _, err := Dimensions([]byte("RIFF"), "image/webp"); err == nil {
		t.Fatal("truncated webp returned a size")
	}
}

type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

// A large, incompressible image: its header is a tiny prefix of its bytes.
func TestReadDimensionsReadsOnlyTheHeader(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 900, 700))
	random := rand.New(rand.NewSource(7))
	for y := 0; y < 700; y++ {
		for x := 0; x < 900; x++ {
			img.Set(x, y, color.RGBA{uint8(random.Intn(256)), uint8(random.Intn(256)), uint8(random.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 1<<20 {
		t.Fatalf("fixture is only %d bytes", buf.Len())
	}
	raw := &countingReader{r: bytes.NewReader(buf.Bytes())}
	if w, h, err := ReadDimensions(raw); err != nil || w != 900 || h != 700 {
		t.Fatalf("raw = %d x %d, %v", w, h, err)
	}
	if raw.read > 64<<10 {
		t.Fatalf("raw header read consumed %d of %d bytes", raw.read, buf.Len())
	}
	// Sidecars hold base64 text: the same header read through a decoder.
	text := base64.StdEncoding.EncodeToString(buf.Bytes())
	wrapped := &countingReader{r: bytes.NewReader([]byte(text))}
	if w, h, err := ReadDimensions(base64.NewDecoder(base64.StdEncoding, wrapped)); err != nil || w != 900 || h != 700 {
		t.Fatalf("base64 = %d x %d, %v", w, h, err)
	}
	if wrapped.read > 64<<10 {
		t.Fatalf("base64 header read consumed %d of %d bytes", wrapped.read, len(text))
	}
}
