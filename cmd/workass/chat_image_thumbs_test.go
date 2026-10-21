package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/httpserve"
	"workass/internal/wire"
)

func TestChatImageRenditionWidthsRoundUpToAFixedSet(t *testing.T) {
	for raw, want := range map[string]int{"": 0, "x": 0, "-5": 0, "1": 160, "160": 160, "161": 240, "300": 320, "1920": 1920, "1921": 0} {
		if got := chatImageThumbWidth(raw); got != want {
			t.Fatalf("width %q -> %d, want %d", raw, got, want)
		}
	}
}

// A gallery cell asks for a rendition sized to it; the server sends a smaller
// PNG with the same aspect, caches it on disk, and never renders it twice.
func TestChatImageRenditionIsSmallerCachedAndKeepsTheAspect(t *testing.T) {
	stateDir := t.TempDir()
	data := testPNG(t, 1000, 600)
	ref := chatImageSidecar(t, stateDir, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data))
	digest := strings.TrimPrefix(ref, sessionImageDirname+"/")
	runtime := screenshotHistory(t, stateDir, ref, 1, 1, false)
	server := httpserve.New("", wire.NewHub(), nil)
	server.ChatImages = newChatImageHTTPHandler(runtime)
	get := func(query string) (int, http.Header, []byte) {
		recorder := (chatImageServer{handler: server}).get(t, http.MethodGet, httpserve.ChatImagePathPrefix+"screenshot-tab/"+digest+query, "127.0.0.1:1", nil)
		return recorder.Code, recorder.Header(), recorder.Body.Bytes()
	}
	code, header, body := get("?w=300")
	if code != http.StatusOK || header.Get("Content-Type") != "image/png" || header.Get("ETag") != `"`+digest+`-w320"` {
		t.Fatalf("rendition = %d %q %q", code, header.Get("Content-Type"), header.Get("ETag"))
	}
	config, err := png.DecodeConfig(bytes.NewReader(body))
	if err != nil || config.Width != 320 || config.Height != 192 {
		t.Fatalf("rendition is %dx%d (err=%v), want 320x192", config.Width, config.Height, err)
	}
	if len(body) >= len(data) {
		t.Fatalf("rendition is %d bytes, original %d", len(body), len(data))
	}
	cached := filepath.Join(stateDir, chatImageThumbDirname, digest+"-w320")
	if stored, err := os.ReadFile(cached); err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("rendition not cached on disk (err=%v)", err)
	}
	// The cache answers without the original.
	thumbs := newChatImageThumbs(stateDir)
	again, contentType, err := thumbs.rendition(digest, 320, func() ([]byte, error) { t.Fatal("rendered twice"); return nil, nil })
	if err != nil || contentType != "image/png" || !bytes.Equal(again, body) {
		t.Fatalf("cached rendition = %d bytes %q err=%v", len(again), contentType, err)
	}
	// A width at or above the image's own is the image itself.
	if code, header, body := get("?w=1280"); code != http.StatusOK || !bytes.Equal(body, data) || header.Get("ETag") != `"`+digest+`-w1280"` {
		t.Fatalf("wide rendition = %d, %d bytes, etag %q; want the original", code, len(body), header.Get("ETag"))
	}
	if code, _, body := get(""); code != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("full image = %d, %d bytes", code, len(body))
	}
}

// Area averaging over premultiplied color: a uniform image stays uniform, and
// a transparent neighbour lowers alpha without darkening the color.
func TestDownscaleAveragesAreasWithoutDarkeningTransparentEdges(t *testing.T) {
	flat := image.NewNRGBA(image.Rect(0, 0, 301, 97))
	for y := 0; y < 97; y++ {
		for x := 0; x < 301; x++ {
			flat.SetNRGBA(x, y, color.NRGBA{R: 40, G: 120, B: 200, A: 255})
		}
	}
	scaled := downscaleImage(flat, 160)
	if scaled.Bounds().Dx() != 160 || scaled.Bounds().Dy() != 52 {
		t.Fatalf("scaled to %v", scaled.Bounds())
	}
	for y := 0; y < scaled.Bounds().Dy(); y++ {
		for x := 0; x < 160; x++ {
			if got := scaled.RGBAAt(x, y); got != (color.RGBA{R: 40, G: 120, B: 200, A: 255}) {
				t.Fatalf("uniform image changed at %d,%d: %v", x, y, got)
			}
		}
	}
	edge := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	edge.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	edge.SetNRGBA(1, 0, color.NRGBA{G: 255, A: 0})
	got := color.NRGBAModel.Convert(downscaleImage(edge, 1).At(0, 0)).(color.NRGBA)
	if got.R < 250 || got.G > 2 || got.A < 126 || got.A > 129 {
		t.Fatalf("half-transparent average = %v, want red at half alpha", got)
	}
}

func TestRenditionCacheDropsTheOldestFirst(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"a", "b", "c", "d"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, bytes.Repeat([]byte{1}, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if total := pruneChatImageThumbs(dir, 250); total != 200 {
		t.Fatalf("pruned to %d bytes, want 200", total)
	}
	for name, want := range map[string]bool{"a": false, "b": false, "c": true, "d": true} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
			t.Fatalf("%s kept=%v, want %v", name, err == nil, want)
		}
	}
}
