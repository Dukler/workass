package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"workass/internal/httpserve"
)

// chatImageHTTP serves a chat's stored images by content reference, so the
// renderer can render <img src=url> and let the browser fetch, decode and
// cache them off its main thread (t3code serves its attachments the same way).
// Over the socket each image was a ~620 KB base64 string inside a JSON reply:
// a 225 MB screenshot chat carried 816 MB of them, parsed and turned into data
// URLs on the main thread, while the next history page waited behind it.
//
// GET /workass/chat-image/<tabId>/<sha256>?k=<image key>
//
// httpserve has already checked the image key (an approved device); this
// handler applies the same ownership check as the socket's archive image read
// (the tab's chat exists, is not deleted, and owns the reference), then sends
// the decoded bytes. Sidecars are content-addressed, so the response is
// immutable and its ETag is the digest.
type chatImageHTTP struct {
	chats *providerChatRuntime
}

func newChatImageHTTPHandler(chats *providerChatRuntime) http.Handler {
	if chats == nil {
		return nil
	}
	return chatImageHTTP{chats: chats}
}

func (h chatImageHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tabID, digest, ok := parseChatImagePath(r.URL.EscapedPath())
	if !ok {
		http.NotFound(w, r)
		return
	}
	ref := sessionImageDirname + "/" + digest
	owned, err := h.chats.authorizeSessionImageByTab(tabID, ref)
	if err != nil || !owned {
		// Unknown chat, deleted chat or someone else's image: all "not here".
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	etag := `"` + digest + `"`
	header := w.Header()
	header.Set("ETag", etag)
	// private: the bytes are a device's, never a shared cache's. immutable: a
	// digest names exactly one content.
	header.Set("Cache-Control", "private, max-age=31536000, immutable")
	header.Set("X-Content-Type-Options", "nosniff")
	// Opened directly, the response is an inert sandboxed document.
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, err := readChatImageBytes(ref, filepath.Dir(h.chats.sessions.path))
	if err != nil {
		header.Del("ETag")
		header.Set("Cache-Control", "no-store")
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "chat image unavailable", http.StatusInternalServerError)
		}
		return
	}
	contentType := http.DetectContentType(data)
	if !strings.HasPrefix(contentType, "image/") {
		// Raster images only. Anything else (an SVG sniffs as text/xml) stays
		// on the socket read, where the renderer builds a data URL as before.
		header.Del("ETag")
		header.Set("Cache-Control", "no-store")
		http.Error(w, "not a raster image", http.StatusUnsupportedMediaType)
		return
	}
	header.Set("Content-Type", contentType)
	header.Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// parseChatImagePath splits /workass/chat-image/<escaped tab id>/<digest>.
func parseChatImagePath(escapedPath string) (tabID, digest string, ok bool) {
	rest, found := strings.CutPrefix(escapedPath, httpserve.ChatImagePathPrefix)
	if !found {
		return "", "", false
	}
	escapedTab, digest, found := strings.Cut(rest, "/")
	if !found || escapedTab == "" {
		return "", "", false
	}
	tabID, err := url.PathUnescape(escapedTab)
	if err != nil || strings.TrimSpace(tabID) == "" {
		return "", "", false
	}
	if _, valid := validSessionImageRef(sessionImageDirname + "/" + digest); !valid {
		return "", "", false
	}
	return tabID, digest, true
}

// readChatImageBytes reads a verified sidecar (base64 text, or a data URL the
// store wrote) and returns the image bytes.
func readChatImageBytes(ref, stateDir string) ([]byte, error) {
	text, err := readExternalSessionImage(ref, stateDir)
	if err != nil {
		return nil, err
	}
	body := []byte(text)
	if bytes.HasPrefix(body, []byte("data:")) {
		head := body
		if len(head) > sessionImageDataURLPrefixLimit {
			head = head[:sessionImageDataURLPrefixLimit]
		}
		comma := bytes.IndexByte(head, ',')
		if comma < 0 || !bytes.Contains(head[:comma], []byte(";base64")) {
			return nil, errors.New("session image is not base64")
		}
		body = body[comma+1:]
	}
	data := make([]byte, base64.StdEncoding.DecodedLen(len(body)))
	n, err := base64.StdEncoding.Decode(data, body)
	if err != nil {
		return nil, err
	}
	return data[:n], nil
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
