package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Display renditions. A transcript shows most images far smaller than they
// are: a tool's screenshot gallery draws ~2,000 px captures in 156 px cells.
// Measured on 300 images of the 225 MB chat, a rendition sized to the frame at
// devicePixelRatio 2 (320 px) cut the bytes from 111 MB to 21 MB, the
// browser's decode time from 1.18 s to 0.16 s and the decoded pixels from
// 1.3 GB to 92 MB. The renderer asks for one (?w=<width>) only when the frame
// is much narrower than the image; the full image still opens in the viewer.
//
// Renditions are pure Go (area-averaged, so a downscale never aliases),
// encoded losslessly as PNG (JPEG stays JPEG), and cached on disk under
// image-thumbs/, bounded, keyed by digest and width. Images that are already
// narrow enough, animated or not PNG/JPEG, or too large to decode safely are
// served as they are.

// chatImageThumbWidths are the widths a rendition may have. A request rounds
// up to the next one, so an unknown width never mints a new cache entry.
var chatImageThumbWidths = []int{160, 240, 320, 480, 640, 960, 1280, 1920}

const (
	chatImageThumbDirname = "image-thumbs"
	// The disk cache stays under this; the oldest renditions go first.
	chatImageThumbCacheBytes = 256 << 20
	// Decoding is bounded: a 48 MP image is ~190 MB of pixels.
	chatImageThumbMaxPixels = 48_000_000
)

// chatImageThumbWidth parses ?w= and rounds it up to a rendition width; 0
// means the full image.
func chatImageThumbWidth(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	width, err := strconv.Atoi(raw)
	if err != nil || width <= 0 {
		return 0
	}
	for _, candidate := range chatImageThumbWidths {
		if width <= candidate {
			return candidate
		}
	}
	return 0
}

type chatImageThumbCall struct {
	done        chan struct{}
	data        []byte
	contentType string
	err         error
}

type chatImageThumbs struct {
	dir   string
	slots chan struct{}

	mu       sync.Mutex
	inflight map[string]*chatImageThumbCall
	sized    bool
	size     int64
}

func newChatImageThumbs(stateDir string) *chatImageThumbs {
	workers := runtime.GOMAXPROCS(0) / 2
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4
	}
	return &chatImageThumbs{
		dir:      filepath.Join(stateDir, chatImageThumbDirname),
		slots:    make(chan struct{}, workers),
		inflight: make(map[string]*chatImageThumbCall),
	}
}

// rendition returns the image at most width pixels wide. original reads the
// verified full image; it runs only when no cached rendition exists.
func (t *chatImageThumbs) rendition(digest string, width int, original func() ([]byte, error)) ([]byte, string, error) {
	key := fmt.Sprintf("%s-w%d", digest, width)
	path := filepath.Join(t.dir, key)
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		return data, http.DetectContentType(data), nil
	}
	t.mu.Lock()
	if call, ok := t.inflight[key]; ok {
		t.mu.Unlock()
		<-call.done
		return call.data, call.contentType, call.err
	}
	call := &chatImageThumbCall{done: make(chan struct{})}
	t.inflight[key] = call
	t.mu.Unlock()

	call.data, call.contentType, call.err = t.render(path, width, original)

	t.mu.Lock()
	delete(t.inflight, key)
	t.mu.Unlock()
	close(call.done)
	return call.data, call.contentType, call.err
}

func (t *chatImageThumbs) render(path string, width int, original func() ([]byte, error)) ([]byte, string, error) {
	data, err := original()
	if err != nil {
		return nil, "", err
	}
	contentType := http.DetectContentType(data)
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width <= width ||
		config.Width <= 0 || config.Height <= 0 || config.Width*config.Height > chatImageThumbMaxPixels {
		return data, contentType, nil
	}
	t.slots <- struct{}{}
	encoded, encodedType, ok := encodeChatImageRendition(data, format, width)
	<-t.slots
	if !ok || len(encoded) >= len(data) {
		// Not decodable after all, or no smaller than the original.
		return data, contentType, nil
	}
	t.store(path, encoded)
	return encoded, encodedType, nil
}

func encodeChatImageRendition(data []byte, format string, width int) ([]byte, string, bool) {
	var src image.Image
	var err error
	if format == "png" {
		src, err = png.Decode(bytes.NewReader(data))
	} else {
		src, err = jpeg.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return nil, "", false
	}
	scaled := downscaleImage(src, width)
	var out bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&out, scaled, &jpeg.Options{Quality: 90}); err != nil {
			return nil, "", false
		}
		return out.Bytes(), "image/jpeg", true
	}
	if err := png.Encode(&out, scaled); err != nil {
		return nil, "", false
	}
	return out.Bytes(), "image/png", true
}

// store writes a rendition atomically and keeps the cache under its bound.
// A failed write only means the next request renders again.
func (t *chatImageThumbs) store(path string, data []byte) {
	if err := os.MkdirAll(t.dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(t.dir, ".thumb-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil || os.Rename(tmpName, path) != nil {
		_ = os.Remove(tmpName)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.sized {
		t.size = 0
		if entries, err := os.ReadDir(t.dir); err == nil {
			for _, entry := range entries {
				if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
					t.size += info.Size()
				}
			}
		}
		t.sized = true
	} else {
		t.size += int64(len(data))
	}
	if t.size > chatImageThumbCacheBytes {
		t.size = pruneChatImageThumbs(t.dir, chatImageThumbCacheBytes*3/4)
	}
}

// pruneChatImageThumbs removes the oldest renditions until the directory
// holds at most limit bytes, and returns what it holds.
func pruneChatImageThumbs(dir string, limit int64) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	type file struct {
		name string
		size int64
		mod  int64
	}
	files := make([]file, 0, len(entries))
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, file{entry.Name(), info.Size(), info.ModTime().UnixNano()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod < files[j].mod })
	for _, f := range files {
		if total <= limit {
			break
		}
		if os.Remove(filepath.Join(dir, f.name)) == nil {
			total -= f.size
		}
	}
	return total
}

// downscaleImage resizes src to width pixels (height in proportion) with an
// exact area average over premultiplied color, so every source pixel counts
// by the area it covers and transparent edges do not darken.
func downscaleImage(src image.Image, width int) *image.RGBA {
	bounds := src.Bounds()
	sw, sh := bounds.Dx(), bounds.Dy()
	height := int(math.Round(float64(sh) * float64(width) / float64(sw)))
	if height < 1 {
		height = 1
	}
	columns := areaWeights(sw, width)
	rows := areaWeights(sh, height)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	line := make([]float32, sw*4)
	horizontal := make([]float32, width*4)
	acc := make([]float32, width*4)
	for y := 0; y < height; y++ {
		for i := range acc {
			acc[i] = 0
		}
		for _, tap := range rows[y] {
			readPremultipliedRow(src, bounds.Min.X, bounds.Min.Y+tap.index, sw, line)
			for x := 0; x < width; x++ {
				var r, g, b, a float32
				for _, c := range columns[x] {
					p := line[c.index*4:]
					r += p[0] * c.weight
					g += p[1] * c.weight
					b += p[2] * c.weight
					a += p[3] * c.weight
				}
				horizontal[x*4], horizontal[x*4+1], horizontal[x*4+2], horizontal[x*4+3] = r, g, b, a
			}
			for i, v := range horizontal {
				acc[i] += v * tap.weight
			}
		}
		out := dst.Pix[y*dst.Stride:]
		for x := 0; x < width; x++ {
			// Premultiplied: rounding must never lift a channel above alpha.
			a := clampByte(acc[x*4+3])
			out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] = min(clampByte(acc[x*4]), a), min(clampByte(acc[x*4+1]), a), min(clampByte(acc[x*4+2]), a), a
		}
	}
	return dst
}

type areaTap struct {
	index  int
	weight float32
}

// areaWeights maps each of n output samples onto the source samples it
// covers, weighted by the covered fraction.
func areaWeights(source, n int) [][]areaTap {
	scale := float64(source) / float64(n)
	weights := make([][]areaTap, n)
	for i := 0; i < n; i++ {
		start, end := float64(i)*scale, float64(i+1)*scale
		for s := int(start); s < source && float64(s) < end; s++ {
			overlap := math.Min(end, float64(s+1)) - math.Max(start, float64(s))
			if overlap > 0 {
				weights[i] = append(weights[i], areaTap{index: s, weight: float32(overlap / scale)})
			}
		}
	}
	return weights
}

// readPremultipliedRow reads one source row as premultiplied 8-bit channels.
func readPremultipliedRow(src image.Image, x0, y, width int, out []float32) {
	switch img := src.(type) {
	case *image.NRGBA:
		row := img.Pix[img.PixOffset(x0, y):]
		for x := 0; x < width; x++ {
			p := row[x*4:]
			alpha := float32(p[3]) / 255
			out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] = float32(p[0])*alpha, float32(p[1])*alpha, float32(p[2])*alpha, float32(p[3])
		}
	case *image.RGBA:
		row := img.Pix[img.PixOffset(x0, y):]
		for x := 0; x < width*4; x++ {
			out[x] = float32(row[x])
		}
	case *image.Gray:
		row := img.Pix[img.PixOffset(x0, y):]
		for x := 0; x < width; x++ {
			v := float32(row[x])
			out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] = v, v, v, 255
		}
	case *image.YCbCr:
		for x := 0; x < width; x++ {
			c := img.YCbCrAt(x0+x, y)
			r, g, b := color.YCbCrToRGB(c.Y, c.Cb, c.Cr)
			out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] = float32(r), float32(g), float32(b), 255
		}
	default:
		for x := 0; x < width; x++ {
			r, g, b, a := src.At(x0+x, y).RGBA()
			out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] = float32(r>>8), float32(g>>8), float32(b>>8), float32(a>>8)
		}
	}
}

func clampByte(v float32) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}
