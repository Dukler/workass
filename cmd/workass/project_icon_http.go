package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"workass/internal/httpserve"
	"workass/internal/wire"
)

// A project's icon by URL, so the sidebar's icons are images the browser
// fetches, decodes and caches, not base64 strings inside JSON replies the
// renderer parses before its list can settle (2026-10-25: six icons were
// 15 KB of every cold connect's JSON, the same pieces t3code serves by URL).
//
// The socket still says which icon a folder has: project:icon {cwd,
// ifNoneMatch, byURL} answers a raster icon with {etag, found, mimeType,
// byURL} and no bytes, where etag is the hash of the reply the read without
// byURL would send. GET /workass/project-icon/<etag>?cwd=<path>&k=<image key>
// then serves exactly that icon, immutably cached under its hash; a folder
// whose icon changed since answers 404 and the renderer reads the socket
// again. An SVG icon, a missing one, an older bridge or an older daemon keep
// the base64 reply, unchanged.
type projectIconHTTP struct{}

func projectIconReplyRaw(cwd string) (projectIconResult, []byte, string, error) {
	icon := projectIconForWorkspace(cwd)
	raw, err := json.Marshal(icon)
	if err != nil {
		return icon, nil, "", err
	}
	sum := sha256.Sum256(raw)
	return icon, raw, hex.EncodeToString(sum[:]), nil
}

func projectIconByURL(icon projectIconResult) bool {
	switch icon.MIMEType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/x-icon":
		return icon.Found && icon.Base64 != ""
	}
	return false
}

// projectIconWireReply answers project:icon {ifNoneMatch, byURL}.
func projectIconWireReply(cwd, ifNoneMatch string, byURL bool) (any, error) {
	icon, raw, etag, err := projectIconReplyRaw(cwd)
	if err != nil {
		return nil, err
	}
	if !byURL || !projectIconByURL(icon) || etag == ifNoneMatch {
		return etagReply(raw, ifNoneMatch), nil
	}
	encoded, err := json.Marshal(map[string]any{"etag": etag, "found": true, "mimeType": icon.MIMEType, "byURL": true})
	if err != nil {
		return nil, err
	}
	return wire.EncodedResult(encoded), nil
}

// The sidebar draws a project icon 16 CSS px wide, so the URL serves a PNG or
// JPEG icon at most projectIconRenditionWidth px wide (enough for a 4x screen),
// area-averaged like a chat image rendition: a 512 px favicon was 11 KB for 16
// px on screen. The rendition is derived from the icon its etag names, so it
// stays immutable under that etag; a few are kept in memory.
const (
	projectIconRenditionWidth = 64
	projectIconRenditionsKept = 64
)

var projectIconRenditions = struct {
	sync.Mutex
	byEtag map[string]projectIconRenditionEntry
	order  []string
}{byEtag: map[string]projectIconRenditionEntry{}}

type projectIconRenditionEntry struct {
	data        []byte
	contentType string
}

func projectIconRendition(etag string, data []byte, mimeType string) ([]byte, string) {
	format := ""
	switch mimeType {
	case "image/png":
		format = "png"
	case "image/jpeg":
		format = "jpeg"
	default:
		return data, mimeType
	}
	projectIconRenditions.Lock()
	if kept, ok := projectIconRenditions.byEtag[etag]; ok {
		projectIconRenditions.Unlock()
		return kept.data, kept.contentType
	}
	projectIconRenditions.Unlock()
	out, contentType := data, mimeType
	if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil && config.Width > projectIconRenditionWidth &&
		config.Width*config.Height <= chatImageThumbMaxPixels {
		if encoded, encodedType, ok := encodeChatImageRendition(data, format, projectIconRenditionWidth); ok && len(encoded) < len(data) {
			out, contentType = encoded, encodedType
		}
	}
	projectIconRenditions.Lock()
	defer projectIconRenditions.Unlock()
	if _, ok := projectIconRenditions.byEtag[etag]; !ok {
		projectIconRenditions.byEtag[etag] = projectIconRenditionEntry{data: out, contentType: contentType}
		projectIconRenditions.order = append(projectIconRenditions.order, etag)
		for len(projectIconRenditions.order) > projectIconRenditionsKept {
			delete(projectIconRenditions.byEtag, projectIconRenditions.order[0])
			projectIconRenditions.order = projectIconRenditions.order[1:]
		}
	}
	return out, contentType
}

// providersListReply answers providers:list. With {ifNoneMatch}, the etag a
// client computes for the list it holds (wire.ReplayEtag, the same hash the
// connect replay skips it by), the reply is a tiny {unchanged, etag} while the
// list is that one, and the list itself, exactly as before, otherwise.
func providersListReply(list any, ifNoneMatch string) any {
	if ifNoneMatch != "" {
		if etag, ok := wire.ReplayEtag(list); ok && etag == ifNoneMatch {
			return map[string]any{"unchanged": true, "etag": etag}
		}
	}
	return list
}

func (projectIconHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	etag, found := strings.CutPrefix(r.URL.Path, httpserve.ProjectIconPathPrefix)
	if decoded, err := hex.DecodeString(etag); !found || err != nil || len(decoded) != sha256.Size {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	icon, _, current, err := projectIconReplyRaw(r.URL.Query().Get("cwd"))
	if err != nil || current != etag || !projectIconByURL(icon) {
		header.Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	data, err := base64.StdEncoding.DecodeString(icon.Base64)
	if err != nil {
		header.Set("Cache-Control", "no-store")
		http.Error(w, "project icon unavailable", http.StatusInternalServerError)
		return
	}
	data, contentType := projectIconRendition(etag, data, icon.MIMEType)
	quoted := `"` + etag + `"`
	header.Set("ETag", quoted)
	// The URL names one content; private, as a device's own.
	header.Set("Cache-Control", "private, max-age=31536000, immutable")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if etagMatches(r.Header.Get("If-None-Match"), quoted) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Content-Type", contentType)
	header.Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
