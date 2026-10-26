package httpserve

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Renderer files are served so a reload costs nothing it does not need.
//
//   - A content-hashed bundle file (Vite's assets/<name>-<hash>.<ext>) never
//     changes under its name, so the browser keeps it for a year without asking.
//     A new build is a new name, reached through a new index.html.
//   - index.html, the bridge and any unhashed file revalidate on every load
//     (no-cache + a strong ETag): an unchanged one costs a 304, a promoted one
//     arrives at once.
//   - Text files go gzip-encoded to a client that accepts it, compressed once
//     per content and kept.
//
// Dynamic and secret responses (health, metrics, mocks, chat images that fail
// authorization, artifacts) keep their own no-store headers; nothing here
// touches them. Before this every reload re-fetched the whole 926 KB bundle.

const immutableCacheControl = "private, max-age=31536000, immutable"
const revalidateCacheControl = "no-cache"

// Vite names every emitted asset <name>-<8 base64url chars>.<ext>.
var hashedAssetName = regexp.MustCompile(`-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$`)

func isImmutableRendererAsset(rel string) bool {
	return strings.HasPrefix(rel, "assets/") && hashedAssetName.MatchString(path.Base(rel))
}

// Below this a gzip header and the extra round of work cost more than they save.
const gzipMinBytes = 1024

// A handful of renderer files is all this ever holds; a renderer directory that
// is promoted many times over a daemon's life only resets it.
const gzipCacheLimit = 64

var gzipCache = struct {
	sync.Mutex
	entries map[string][]byte
}{entries: map[string][]byte{}}

func gzipped(tag string, data []byte) []byte {
	gzipCache.Lock()
	if cached, ok := gzipCache.entries[tag]; ok {
		gzipCache.Unlock()
		return cached
	}
	gzipCache.Unlock()
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.DefaultCompression)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(data); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	encoded := out.Bytes()
	gzipCache.Lock()
	if len(gzipCache.entries) >= gzipCacheLimit {
		gzipCache.entries = map[string][]byte{}
	}
	gzipCache.entries[tag] = encoded
	gzipCache.Unlock()
	return encoded
}

func compressibleType(contentType string) bool {
	contentType = strings.ToLower(contentType)
	return strings.HasPrefix(contentType, "text/") ||
		strings.HasPrefix(contentType, "application/json") ||
		strings.HasPrefix(contentType, "image/svg+xml")
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		params = strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		return params != "q=0" && params != "q=0.0" && params != "q=0.00" && params != "q=0.000"
	}
	return false
}

// etagMatches implements If-None-Match for strong tags: any listed tag equal to
// ours (weak or not, since a 304 only needs a weak match), or "*".
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func contentTag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// writeRendererFile sends one renderer file with the cache policy above. An
// encoded response carries its own tag, so a cache never answers a client that
// cannot decode gzip with the gzip bytes.
func writeRendererFile(w http.ResponseWriter, r *http.Request, data []byte, contentType string, immutable bool) {
	header := w.Header()
	header.Set("Content-Type", contentType)
	if immutable {
		header.Set("Cache-Control", immutableCacheControl)
	} else {
		header.Set("Cache-Control", revalidateCacheControl)
	}
	tag := contentTag(data)
	body := data
	etag := `"` + tag + `"`
	if len(data) >= gzipMinBytes && compressibleType(contentType) {
		header.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			if encoded := gzipped(tag, data); encoded != nil && len(encoded) < len(data) {
				body = encoded
				etag = `"` + tag + `-gz"`
				header.Set("Content-Encoding", "gzip")
			}
		}
	}
	header.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		header.Del("Content-Type")
		header.Del("Content-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}
