package httpserve

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func cacheFixture() *Server {
	handler := New("", nil, nil)
	bundle := strings.Repeat("console.log('bundle');\n", 400)
	handler.RendererFS = fstest.MapFS{
		"index.html":                  &fstest.MapFile{Data: []byte(`<!doctype html><head><script type="module" src="./assets/index-DsVAJI7v.js"></script></head><body></body>`)},
		"assets/index-DsVAJI7v.js":    &fstest.MapFile{Data: []byte(bundle)},
		"assets/index-Bwz3BJ4y.css":   &fstest.MapFile{Data: []byte(strings.Repeat("a{color:red}\n", 200))},
		"assets/workass-BpzNkGV0.png": &fstest.MapFile{Data: []byte("\x89PNG fixture")},
		"assets/unversioned.js":       &fstest.MapFile{Data: []byte("window.loose=true;")},
		"favicon.svg":                 &fstest.MapFile{Data: []byte("<svg/>")},
	}
	return handler
}

func serve(handler http.Handler, path string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.test"+path, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	for key, value := range header {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// A reload must not re-download the bundle: content-hashed files are kept for
// a year, and everything a new build can change (index.html, the bridge, any
// unhashed file) revalidates and answers 304 while unchanged.
func TestRendererCachePolicy(t *testing.T) {
	handler := cacheFixture()
	for _, tc := range []struct {
		path      string
		cache     string
		immutable bool
	}{
		{"/assets/index-DsVAJI7v.js", immutableCacheControl, true},
		{"/assets/index-Bwz3BJ4y.css", immutableCacheControl, true},
		{"/assets/workass-BpzNkGV0.png", immutableCacheControl, true},
		{"/assets/unversioned.js", "no-cache", false},
		{"/favicon.svg", "no-cache", false},
		{"/", "no-cache", false},
		{"/index.html", "no-cache", false},
		{"/lan-bridge.js", "no-cache", false},
	} {
		first := serve(handler, tc.path, nil)
		if first.Code != http.StatusOK {
			t.Fatalf("%s status = %d", tc.path, first.Code)
		}
		if got := first.Header().Get("Cache-Control"); got != tc.cache {
			t.Fatalf("%s cache-control = %q, want %q", tc.path, got, tc.cache)
		}
		etag := first.Header().Get("ETag")
		if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 10 {
			t.Fatalf("%s etag = %q", tc.path, etag)
		}
		again := serve(handler, tc.path, map[string]string{"If-None-Match": etag})
		if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
			t.Fatalf("%s revalidation = %d with %d bytes, want an empty 304", tc.path, again.Code, again.Body.Len())
		}
		stale := serve(handler, tc.path, map[string]string{"If-None-Match": `"something-else"`})
		if stale.Code != http.StatusOK || !bytes.Equal(stale.Body.Bytes(), first.Body.Bytes()) {
			t.Fatalf("%s with a stale tag = %d", tc.path, stale.Code)
		}
	}
	// The injection the bridge depends on is unchanged.
	if body := serve(handler, "/", nil).Body.String(); !strings.Contains(body, `<script src="/lan-bridge.js"></script>`) {
		t.Fatalf("index lost the bridge injection: %s", body)
	}
}

// Text goes gzip-encoded to a client that accepts it, under its own tag, and
// byte-identical (unencoded) to one that does not.
func TestRendererGzipEncoding(t *testing.T) {
	handler := cacheFixture()
	plain := serve(handler, "/assets/index-DsVAJI7v.js", nil)
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("a client that did not ask got %q", plain.Header().Get("Content-Encoding"))
	}
	encoded := serve(handler, "/assets/index-DsVAJI7v.js", map[string]string{"Accept-Encoding": "gzip, deflate, br, zstd"})
	if encoded.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(encoded.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("encoded headers = %v", encoded.Header())
	}
	if encoded.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Fatal("the gzip variant must carry its own tag")
	}
	if encoded.Body.Len() >= plain.Body.Len()/4 {
		t.Fatalf("gzip body %d bytes for %d plain", encoded.Body.Len(), plain.Body.Len())
	}
	reader, err := gzip.NewReader(bytes.NewReader(encoded.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(decoded, plain.Body.Bytes()) {
		t.Fatalf("gzip body does not decode to the file: %v", err)
	}
	refused := serve(handler, "/assets/index-DsVAJI7v.js", map[string]string{"Accept-Encoding": "gzip;q=0"})
	if refused.Header().Get("Content-Encoding") != "" {
		t.Fatal("gzip;q=0 still got gzip")
	}
	// Images are never gzipped.
	if png := serve(handler, "/assets/workass-BpzNkGV0.png", map[string]string{"Accept-Encoding": "gzip"}); png.Header().Get("Content-Encoding") != "" {
		t.Fatal("an image was gzipped")
	}
}

func TestEtagMatching(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{`"abc"`, true}, {`W/"abc"`, true}, {`"x", "abc"`, true}, {`*`, true},
		{`"abcd"`, false}, {``, false}, {`abc`, false},
	} {
		if got := etagMatches(tc.header, `"abc"`); got != tc.want {
			t.Fatalf("etagMatches(%q) = %v", tc.header, got)
		}
	}
}
