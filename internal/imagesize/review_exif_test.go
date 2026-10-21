//go:build review

// Independent-review reproduction (b8b18322..fe7003e5). Run with:
//   go test -tags review -count=1 -run TestReview ./internal/imagesize/

package imagesize

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"os"
	"testing"
)

// exifOrientationJPEG returns a width x height JPEG carrying an EXIF APP1
// segment with Orientation = orientation (6 = rotate 90 CW, as phone cameras
// write for portrait shots).
func exifOrientationJPEG(t *testing.T, width, height int, orientation uint16) []byte {
	t.Helper()
	var plain bytes.Buffer
	if err := jpeg.Encode(&plain, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatal(err)
	}
	// TIFF header (little endian) + IFD0 with one entry: 0x0112 Orientation SHORT 1.
	var tiff bytes.Buffer
	tiff.WriteString("II")
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(42))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(1))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, orientation)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var app1 bytes.Buffer
	app1.Write([]byte{0xff, 0xe1})
	_ = binary.Write(&app1, binary.BigEndian, uint16(len(payload)+2))
	app1.Write(payload)
	data := plain.Bytes()
	out := append([]byte{}, data[:2]...) // SOI
	out = append(out, app1.Bytes()...)
	return append(out, data[2:]...)
}

// The wire size was the stored raster size; a browser shows an EXIF-rotated
// JPEG with its sides swapped (image-orientation: from-image is the default).
// Fixed, inverted: the size is now the shown one. Normal-suite test:
// TestJPEGEXIFOrientationSwapsTheShownSides.
func TestReviewEXIFOrientationSizeIsUnrotated(t *testing.T) {
	data := exifOrientationJPEG(t, 400, 200, 6)
	w, h, err := Dimensions(data, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("EXIF orientation 6 JPEG: wire size %dx%d", w, h)
	if path := os.Getenv("REVIEW_EXIF_OUT"); path != "" {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if w != 200 || h != 400 {
		t.Fatalf("EXIF orientation 6 is still reported unrotated: %dx%d", w, h)
	}
}
