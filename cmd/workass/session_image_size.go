package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"strings"
	"sync"

	"workass/internal/imagesize"
)

// Image pixel sizes let the renderer reserve an image's final frame before its
// bytes arrive, so a deferred image never pushes the rows below it when it
// loads. They are read from the image header only (PNG, JPEG, GIF, WebP) and
// are absent when the header cannot be read (SVG, damaged bytes); the image is
// otherwise unaffected. Wire images carry them as integer `width`/`height`.

// sessionImageSizeCacheEntries bounds the process cache of header reads. Keys
// are content digests, so an entry never goes stale; failures are cached too.
// One chat can hold tens of thousands of screenshots, and a cache smaller
// than one chat's page walk evicts every entry before it is read again.
const sessionImageSizeCacheEntries = 32768

type sessionImageSize struct {
	width, height int
}

type sessionImageSizeCache struct {
	mu     sync.Mutex
	byName map[string]sessionImageSize
	order  []string
	next   int
}

var sessionImageSizes = &sessionImageSizeCache{byName: make(map[string]sessionImageSize)}

func (c *sessionImageSizeCache) lookup(name string) (sessionImageSize, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size, ok := c.byName[name]
	return size, ok
}

func (c *sessionImageSizeCache) store(name string, size sessionImageSize) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.byName[name]; exists {
		c.byName[name] = size
		return
	}
	if len(c.order) < sessionImageSizeCacheEntries {
		c.order = append(c.order, name)
	} else {
		delete(c.byName, c.order[c.next])
		c.order[c.next] = name
		c.next = (c.next + 1) % sessionImageSizeCacheEntries
	}
	c.byName[name] = size
}

// sessionImageDataSize reads the size of in-memory session image data (base64
// text, the sidecar format). name is the data's content digest.
func sessionImageDataSize(name, data string) (int, int) {
	if size, ok := sessionImageSizes.lookup(name); ok {
		return size.width, size.height
	}
	size := readSessionImageSize(strings.NewReader(data))
	sessionImageSizes.store(name, size)
	return size.width, size.height
}

// sessionImageRefSize reads the size from the header of a content-addressed
// sidecar. A missing or unreadable file is not cached: it may be materialized
// later. An undecodable header is cached as "no size".
func sessionImageRefSize(ref, stateDir string) (int, int) {
	// The cache is keyed by content digest, so a hit needs no file system call:
	// a projection of a screenshot-heavy chat otherwise stats every image on
	// every session:get and history page.
	if name, ok := validSessionImageRef(ref); ok {
		if size, ok := sessionImageSizes.lookup(name); ok {
			return size.width, size.height
		}
	}
	name, path, _, err := externalSessionImageInfo(ref, stateDir)
	if err != nil {
		return 0, 0
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	size := readSessionImageSize(file)
	sessionImageSizes.store(name, size)
	return size.width, size.height
}

// sessionImageDataURLPrefixLimit bounds the scan for a "data:<mime>;base64,"
// prefix; anything longer is not a prefix this store wrote.
const sessionImageDataURLPrefixLimit = 256

// readSessionImageSize decodes only the header of session image text: plain
// base64, or a data URL whose base64 payload follows the first comma.
func readSessionImageSize(text io.Reader) sessionImageSize {
	buffered := bufio.NewReaderSize(text, sessionImageDataURLPrefixLimit)
	if head, _ := buffered.Peek(len("data:")); string(head) == "data:" {
		prefix, _ := buffered.Peek(sessionImageDataURLPrefixLimit)
		comma := bytes.IndexByte(prefix, ',')
		if comma < 0 {
			return sessionImageSize{}
		}
		if _, err := buffered.Discard(comma + 1); err != nil {
			return sessionImageSize{}
		}
	}
	width, height, err := imagesize.ReadDimensions(base64.NewDecoder(base64.StdEncoding, buffered))
	if err != nil {
		return sessionImageSize{}
	}
	return sessionImageSize{width: width, height: height}
}

func setWireImageSize(image map[string]any, width, height int) {
	if width > 0 && height > 0 {
		image["width"] = width
		image["height"] = height
	}
}

func wireImageHasSize(image map[string]any) bool {
	width, hasWidth := image["width"]
	height, hasHeight := image["height"]
	return hasWidth && hasHeight && intValue(width) > 0 && intValue(height) > 0
}

// annotateSessionImageSizes gives every projected image that still carries its
// durable reference and no size one, read from its sidecar header. It covers
// rows written before attachments recorded their size. It must run before
// deferral or rehydration strips the reference field, and it never rewrites
// actor state: the result lives only in this response and the process cache.
func annotateSessionImageSizes(value any, stateDir string) {
	switch item := value.(type) {
	case map[string]any:
		if ref := fieldString(item, sessionImageDataRefField); ref != "" && !wireImageHasSize(item) &&
			strings.HasPrefix(strings.ToLower(fieldString(item, "mimeType")), "image/") {
			width, height := sessionImageRefSize(ref, stateDir)
			setWireImageSize(item, width, height)
		}
		for _, child := range item {
			annotateSessionImageSizes(child, stateDir)
		}
	case []any:
		for _, child := range item {
			annotateSessionImageSizes(child, stateDir)
		}
	}
}
