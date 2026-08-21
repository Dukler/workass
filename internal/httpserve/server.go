package httpserve

import (
	"encoding/json"
	"errors"
	"html"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"workass/internal/artifacthost"
	"workass/internal/wire"
)

var mimeByExt = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".svg":   "image/svg+xml",
	".webp":  "image/webp",
	".gif":   "image/gif",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
}

const mocksPathPrefix = "/workass/mocks"

// AllowFunc decides whether a non-local client IP may access HTTP/WS.
type AllowFunc func(ip string) bool

// IdentityFunc supplies the machine identity fields merged into
// /workass/health. It is a func, not a struct, because parts of it — the
// provider list above all — settle after the server is already listening.
type IdentityFunc func() map[string]any

// Server serves the renderer and delegates WebSocket upgrades to a wire hub.
type Server struct {
	RendererDir   string
	RendererFS    fs.FS
	MocksDir      string
	ArtifactHosts http.Handler
	Metrics       MetricsFunc
	Allow         AllowFunc
	Hub           *wire.Hub
	Version       string
	Name          string
	Identity      IdentityFunc
}

// New creates an HTTP renderer server. With a nil Allow hook, clients may load
// the renderer; WebSocket pairing/controller checks live in the wire hub.
func New(rendererDir string, hub *wire.Hub, allow AllowFunc) *Server {
	return &Server{RendererDir: rendererDir, Hub: hub, Allow: allow}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL != nil && r.URL.Path == "/workass/health" {
		s.serveHealth(w, r)
		return
	}

	if r.URL != nil && r.URL.Path == metricsPath {
		s.serveMetrics(w, r)
		return
	}

	if r.URL != nil && (r.URL.Path == pprofPathRoot || strings.HasPrefix(r.URL.Path, pprofPathRoot+"/")) {
		s.servePprof(w, r)
		return
	}

	if isWebSocketUpgrade(r) {
		if !s.allowed(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.Hub == nil {
			http.Error(w, "websocket unavailable", http.StatusNotFound)
			return
		}
		s.Hub.HandleUpgrade(w, r)
		return
	}

	if !s.allowed(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><body style="font:15px system-ui;background:#06110d;color:#cfe;display:grid;place-items:center;height:100vh;margin:0"><div style="text-align:center"><h2>Acceso pendiente o denegado</h2><p style="color:#8fb3a6">Pedile al anfitrión que permita este dispositivo, y recargá.</p></div></body>`))
		return
	}

	rel, err := url.PathUnescape(r.URL.EscapedPath())
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	if rel == "/" || rel == "" {
		rel = "/index.html"
	}
	if rel == mocksPathPrefix || strings.HasPrefix(rel, mocksPathPrefix+"/") {
		s.serveMocks(w, r, rel)
		return
	}
	if rel == artifacthost.PathPrefix || strings.HasPrefix(rel, artifacthost.PathPrefix+"/") {
		if s.ArtifactHosts == nil {
			http.NotFound(w, r)
			return
		}
		s.ArtifactHosts.ServeHTTP(w, r)
		return
	}

	if rel == "/lan-bridge.js" {
		w.Header().Set("Content-Type", mimeByExt[".js"])
		_, _ = w.Write([]byte(LANBridgeJS))
		return
	}

	if rel == "/index.html" {
		html, err := s.readRendererFile("index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		injected := strings.Replace(string(html), "</head>", "  <script src=\"/lan-bridge.js\"></script>\n</head>", 1)
		w.Header().Set("Content-Type", mimeByExt[".html"])
		_, _ = w.Write([]byte(injected))
		return
	}

	cleanRel, ok := cleanRendererRel(rel)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	data, err := s.readRendererFile(cleanRel)
	if errors.Is(err, errUnsafeRendererPath) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mimeByExt[strings.ToLower(filepath.Ext(cleanRel))])
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	_, _ = w.Write(data)
}

func (s *Server) serveMocks(w http.ResponseWriter, r *http.Request, requestPath string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(s.MocksDir) == "" {
		http.NotFound(w, r)
		return
	}

	rel := strings.TrimPrefix(requestPath, mocksPathPrefix)
	if rel == "" || rel == "/" {
		data, err := s.mockIndex()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mimeByExt[".html"])
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
		return
	}

	cleanRel, ok := cleanRendererRel(rel)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	file, ok := resolveUnder(s.MocksDir, cleanRel)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	contentType := mimeByExt[strings.ToLower(filepath.Ext(cleanRel))]
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (s *Server) mockIndex() ([]byte, error) {
	root, err := filepath.Abs(filepath.Clean(s.MocksDir))
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".html") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	var index strings.Builder
	index.WriteString("<!doctype html><meta charset=\"utf-8\"><title>Workass mocks</title><h1>Workass mocks</h1><ul>")
	for _, rel := range files {
		parts := strings.Split(rel, "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		href := strings.Join(parts, "/")
		index.WriteString("<li><a href=\"")
		index.WriteString(html.EscapeString(href))
		index.WriteString("\">")
		index.WriteString(html.EscapeString(rel))
		index.WriteString("</a></li>")
	}
	index.WriteString("</ul>")
	return []byte(index.String()), nil
}

// publicIdentityKeys is everything an unpaired caller across the network is
// told: enough to recognise a machine and decide whether to pair with it, and
// no more. The rest of the identity — what CLIs are installed, which profile is
// running, which architecture — describes the box rather than the invitation,
// and health answers before any pairing check, so a stranger would otherwise
// read it by connecting.
var publicIdentityKeys = map[string]bool{
	"app":         true,
	"version":     true,
	"name":        true,
	"machineId":   true,
	"displayName": true,
	"wireVersion": true,
	"secure":      true,
	// Public on purpose. A fleet id is a one-way hash of the key, and publishing
	// it is what lets a client recognise a machine as one of its own and enrol
	// with no human action — the property that produces one chat list across
	// machines. It names the fleet; it does not open it.
	"fleetIds": true,
	// The certificate to expect. A caller across the network needs it precisely
	// because it cannot validate a chain to an IP address.
	"certFingerprint": true,
}

// serveHealth answers "who are you?" for probes, for the machine book a client
// keeps, and for the beacon. The original {app, version, name} keys are always
// present and keep their old meaning; Identity only adds to them, so a client
// built before machine identity existed still reads this document.
func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		if host, err := os.Hostname(); err == nil {
			name = host
		}
	}
	version := strings.TrimSpace(s.Version)
	if version == "" {
		version = "unknown"
	}
	doc := map[string]any{"app": "workass", "version": version, "name": name}
	if s.Identity != nil {
		for key, value := range s.Identity() {
			doc[key] = value
		}
	}
	// The app key names the protocol a client is about to speak; nothing gets
	// to overwrite it into something else.
	doc["app"] = "workass"
	if displayName, ok := doc["name"].(string); !ok || strings.TrimSpace(displayName) == "" {
		doc["name"] = name
	}
	if r != nil && !IsLocalIP(ClientIP(r.RemoteAddr)) {
		for key := range doc {
			if !publicIdentityKeys[key] {
				delete(doc, key)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(doc)
}

var errUnsafeRendererPath = errors.New("unsafe renderer path")

func (s *Server) readRendererFile(rel string) ([]byte, error) {
	cleanRel, ok := cleanRendererRel(rel)
	if !ok {
		return nil, errUnsafeRendererPath
	}
	if s.RendererFS != nil {
		return fs.ReadFile(s.RendererFS, cleanRel)
	}
	file, ok := s.resolve(cleanRel)
	if !ok {
		return nil, errUnsafeRendererPath
	}
	return os.ReadFile(file)
}

func cleanRendererRel(rel string) (string, bool) {
	cleanRel := filepath.Clean(strings.TrimPrefix(rel, "/"))
	if cleanRel == "." {
		cleanRel = "index.html"
	}
	if cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	cleanRel = filepath.ToSlash(cleanRel)
	if !fs.ValidPath(cleanRel) {
		return "", false
	}
	return cleanRel, true
}

func (s *Server) resolve(rel string) (string, bool) {
	return resolveUnder(s.RendererDir, rel)
}

func resolveUnder(root, rel string) (string, bool) {
	base, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", false
	}
	cleanRel, ok := cleanRendererRel(rel)
	if !ok {
		return "", false
	}
	file := filepath.Join(base, filepath.FromSlash(cleanRel))
	absFile, err := filepath.Abs(filepath.Clean(file))
	if err != nil {
		return "", false
	}
	back, err := filepath.Rel(base, absFile)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return absFile, true
}

func (s *Server) allowed(r *http.Request) bool {
	ip := ClientIP(r.RemoteAddr)
	if IsLocalIP(ip) {
		return true
	}
	if s.Allow != nil {
		return s.Allow(ip)
	}
	return true
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// ClientIP normalizes a remote address to a bare IPv4/IPv6 string.
func ClientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if strings.HasPrefix(host, "::ffff:") {
		host = strings.TrimPrefix(host, "::ffff:")
	}
	return host
}

// IsLocalIP reports whether an address is the always-allowed localhost class.
func IsLocalIP(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1" || ip == "localhost" || ip == ""
}
