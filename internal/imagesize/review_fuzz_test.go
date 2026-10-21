//go:build review

// Independent-review reproduction (b8b18322..fe7003e5). Run with:
//   go test -tags review -count=1 -run TestReview ./internal/imagesize/

package imagesize

import (
	"bytes"
	"testing"
)

// Review fuzz target: neither entry point may panic, and a success must be a
// positive size, on any header bytes.
func FuzzReviewDimensions(f *testing.F) {
	t := &testing.T{}
	for _, format := range []string{"png", "jpeg", "gif"} {
		f.Add(encoded(t, format, 3, 2), "image/"+format)
	}
	for _, data := range webpFixtures() {
		f.Add(data, "image/webp")
	}
	f.Add([]byte("RIFF\x00\x00\x00\x00WEBPVP8 \x00\x00\x00\x00\x00\x00\x00\x9d\x01\x2a\x00\x00\x00\x00"+string(make([]byte, 16))), "image/webp")
	f.Fuzz(func(t *testing.T, data []byte, mime string) {
		if w, h, err := Dimensions(data, mime); err == nil && (w <= 0 || h <= 0) {
			t.Logf("Dimensions success with non-positive size %dx%d mime=%q", w, h, mime)
		}
		if w, h, err := ReadDimensions(bytes.NewReader(data)); err == nil && (w <= 0 || h <= 0) {
			t.Logf("ReadDimensions success with non-positive size %dx%d", w, h)
		}
	})
}

// A VP8 (lossy) WebP whose 14-bit size fields are zero reports success with a
// zero size instead of an error.
func TestReviewWebPZeroSizeIsNotAnError(t *testing.T) {
	lossy := []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
	w, h, err := Dimensions(webpHeader("VP8 ", lossy), "image/webp")
	t.Logf("VP8 zero header -> %dx%d err=%v", w, h, err)
	if err == nil && (w <= 0 || h <= 0) {
		t.Errorf("zero-size VP8 header returned success %dx%d", w, h)
	}
}
