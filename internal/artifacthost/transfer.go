package artifacthost

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	maxArtifactTransfers = 32
	maxArtifactChunk     = 128 * 1024
	artifactIdleTTL      = 30 * time.Second
	artifactIOTimeout    = 5 * time.Second
)

var artifactHeaderAllowlist = map[string]struct{}{
	"Range": {}, "If-Range": {}, "If-None-Match": {}, "If-Modified-Since": {},
}

var artifactResponseAllowlist = map[string]struct{}{
	"Content-Type": {}, "Content-Length": {}, "Content-Range": {}, "Content-Disposition": {},
	"Accept-Ranges": {}, "Etag": {}, "Last-Modified": {}, "Cache-Control": {},
	"Content-Security-Policy": {}, "X-Content-Type-Options": {}, "X-Workass-Withheld": {}, "Location": {}, "Referrer-Policy": {},
}

type ArtifactTransferManager struct {
	registry  *Registry
	mu        sync.Mutex
	transfers map[string]*artifactTransfer
}

type artifactTransfer struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	cancel context.CancelFunc
	last   time.Time
	readMu sync.Mutex
	timer  *time.Timer
}

type ArtifactOpenResult struct {
	TransferID string            `json:"transferId"`
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
}

type ArtifactReadResult struct {
	BodyBase64 string `json:"bodyBase64"`
	EOF        bool   `json:"eof"`
}

func NewArtifactTransferManager(registry *Registry) *ArtifactTransferManager {
	return &ArtifactTransferManager{registry: registry, transfers: make(map[string]*artifactTransfer)}
}

func (m *ArtifactTransferManager) Open(ctx context.Context, path, method string, headers map[string]string) (ArtifactOpenResult, error) {
	if m == nil || m.registry == nil {
		return ArtifactOpenResult{}, errors.New("artifact transfer unavailable")
	}
	if err := validateArtifactTransferPath(path); err != nil {
		return ArtifactOpenResult{}, err
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method != http.MethodGet && method != http.MethodHead {
		return ArtifactOpenResult{}, errors.New("artifact method must be GET or HEAD")
	}
	requestHeaders := make(http.Header)
	if len(headers) > 8 {
		return ArtifactOpenResult{}, errors.New("too many artifact headers")
	}
	for k, v := range headers {
		canonical := http.CanonicalHeaderKey(k)
		if _, ok := artifactHeaderAllowlist[canonical]; !ok {
			return ArtifactOpenResult{}, fmt.Errorf("artifact header is not allowed: %s", k)
		}
		if len(k) > 128 || len(v) > 8192 || strings.ContainsAny(v, "\r\n\x00") {
			return ArtifactOpenResult{}, errors.New("artifact header is too large")
		}
		requestHeaders.Set(canonical, v)
	}
	m.expire()
	m.mu.Lock()
	if len(m.transfers) >= maxArtifactTransfers {
		m.mu.Unlock()
		return ArtifactOpenResult{}, errors.New("artifact transfer limit reached")
	}
	transferID, err := randomID()
	if err != nil {
		m.mu.Unlock()
		return ArtifactOpenResult{}, err
	}
	readCtx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	t := &artifactTransfer{reader: pr, writer: pw, cancel: cancel, last: time.Now()}
	m.transfers[transferID] = t
	t.timer = time.AfterFunc(artifactIdleTTL, func() { m.expireTransfer(transferID) })
	m.mu.Unlock()

	r, err := http.NewRequestWithContext(readCtx, method, "http://artifact.invalid"+path, nil)
	if err != nil {
		m.Close(transferID)
		return ArtifactOpenResult{}, err
	}
	r.Header = requestHeaders
	rw := &transferResponseWriter{pipe: pw, headerReady: make(chan struct{})}
	go func() { m.registry.ServeHTTP(rw, r); rw.finish() }()
	timer := time.NewTimer(artifactIOTimeout)
	defer timer.Stop()
	select {
	case <-rw.headerReady:
	case <-timer.C:
		m.Close(transferID)
		return ArtifactOpenResult{}, errors.New("artifact response timed out")
	case <-ctx.Done():
		m.Close(transferID)
		return ArtifactOpenResult{}, ctx.Err()
	}
	return ArtifactOpenResult{TransferID: transferID, Status: rw.status, Headers: rw.sentHeaders}, nil
}

func (m *ArtifactTransferManager) Read(ctx context.Context, id string) (ArtifactReadResult, error) {
	if err := ctx.Err(); err != nil {
		m.Close(id)
		return ArtifactReadResult{}, err
	}
	t, ok := m.lookup(id)
	if !ok {
		return ArtifactReadResult{}, errors.New("artifact transfer expired or unknown")
	}
	if !t.readMu.TryLock() {
		return ArtifactReadResult{}, errors.New("artifact read already in progress")
	}
	defer t.readMu.Unlock()
	readCtx, cancel := context.WithTimeout(ctx, artifactIOTimeout)
	defer cancel()
	buf := make([]byte, maxArtifactChunk)
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() { n, err := t.reader.Read(buf); ch <- result{n, err} }()
	select {
	case <-readCtx.Done():
		m.Close(id)
		return ArtifactReadResult{}, readCtx.Err()
	case res := <-ch:
		m.touch(id)
		if res.n > 0 {
			eof := res.err == io.EOF
			if eof {
				m.Close(id)
			}
			return ArtifactReadResult{BodyBase64: base64.StdEncoding.EncodeToString(buf[:res.n]), EOF: eof}, nil
		}
		if res.err == io.EOF {
			m.Close(id)
			return ArtifactReadResult{EOF: true}, nil
		}
		if res.err != nil {
			m.Close(id)
			return ArtifactReadResult{}, res.err
		}
		return ArtifactReadResult{}, nil
	}
}

func (m *ArtifactTransferManager) Close(id string) error {
	m.mu.Lock()
	t, ok := m.transfers[id]
	if ok {
		delete(m.transfers, id)
	}
	if ok && t.timer != nil {
		t.timer.Stop()
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	t.cancel()
	_ = t.reader.Close()
	_ = t.writer.CloseWithError(context.Canceled)
	return nil
}

func (m *ArtifactTransferManager) lookup(id string) (*artifactTransfer, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.transfers[id]
	if !ok {
		return nil, false
	}

	return t, true
}
func (m *ArtifactTransferManager) touch(id string) {
	m.mu.Lock()
	if t := m.transfers[id]; t != nil {
		t.last = time.Now()
		if t.timer != nil {
			t.timer.Reset(artifactIdleTTL)
		}
	}
	m.mu.Unlock()
}
func (m *ArtifactTransferManager) expireTransfer(id string) {
	m.mu.Lock()
	t := m.transfers[id]
	if t == nil {
		m.mu.Unlock()
		return
	}
	if remaining := artifactIdleTTL - time.Since(t.last); remaining > 0 {
		t.timer.Reset(remaining)
		m.mu.Unlock()
		return
	}
	delete(m.transfers, id)
	m.mu.Unlock()
	t.cancel()
	_ = t.reader.Close()
	_ = t.writer.CloseWithError(context.DeadlineExceeded)
}

func (m *ArtifactTransferManager) expire() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.transfers))
	for id := range m.transfers {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.expireTransfer(id)
	}
}

func validateArtifactTransferPath(path string) error {
	if path == "" || !strings.HasPrefix(path, PathPrefix+"/") || strings.Contains(path, "\\") || strings.ContainsAny(path, "\r\n") {
		return errors.New("invalid artifact path")
	}
	pathPart := strings.SplitN(path, "?", 2)[0]
	u, err := url.ParseRequestURI(path)
	if err != nil || u.EscapedPath() == "" {
		return errors.New("invalid artifact path")
	}
	if strings.Contains(strings.ToLower(pathPart), "%2f") || strings.Contains(strings.ToLower(pathPart), "%5c") || strings.Contains(strings.ToLower(pathPart), "%2e") {
		return errors.New("invalid artifact path")
	}
	for _, p := range strings.Split(strings.TrimPrefix(pathPart, PathPrefix+"/"), "/") {
		if p == ".." || p == "." || p == "" {
			if p == "" {
				continue
			}
			return errors.New("invalid artifact path")
		}
	}
	return nil
}

func randomID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type transferResponseWriter struct {
	pipe        *io.PipeWriter
	header      http.Header
	status      int
	headerReady chan struct{}
	sentHeaders map[string]string
}

func (w *transferResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *transferResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.sentHeaders = safeHeaders(w.header)
	select {
	case <-w.headerReady:
	default:
		close(w.headerReady)
	}
}
func (w *transferResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.pipe.Write(p)
}
func (w *transferResponseWriter) finish() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_ = w.pipe.Close()
}

func safeHeaders(in http.Header) map[string]string {
	out := make(map[string]string)
	for k, vals := range in {
		if _, ok := artifactResponseAllowlist[http.CanonicalHeaderKey(k)]; ok && len(vals) > 0 {
			if strings.EqualFold(k, "Location") && !strings.HasPrefix(vals[0], PathPrefix+"/") {
				continue
			}
			out[http.CanonicalHeaderKey(k)] = vals[0]
		}
	}
	return out
}
