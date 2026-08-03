package artifacthost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	PathPrefix             = "/workass/artifacts"
	LegacyPathPrefix       = "/workass/html"
	registryVersion        = 1
	registryFilename       = "artifact-hosts.json"
	legacyRegistryFilename = "html-hosts.json"
	maxRegistryBytes       = 4 * 1024 * 1024
	maxInspectedEntries    = 4000
	maxWithheldReported    = 20
	maxLoggedWithholds     = 512
)

// A hosted name is filtered by the SHAPE of a credential file, never by
// vocabulary. Until 2026-07-26 this reused the redaction word list
// (api_key|token|secret|password|credential|bearer) as a substring match over
// the whole request path. That list is right for scrubbing secret VALUES out of
// running text and wrong for filenames: it withheld _tokens.css,
// design-tokens.json, password-reset.html and credential-flow.svg — most of
// what a design mock ships — and it withheld them as a bare 403, so the page
// itself still returned 200 with its stylesheet missing and nothing said why.

// Key and keystore containers. None of these are in allowedArtifactExtensions
// today; naming them separately keeps the rule true if that list ever grows.
var credentialExtensions = map[string]bool{
	".pem": true, ".key": true, ".p8": true, ".p12": true, ".pfx": true,
	".jks": true, ".keystore": true, ".kdbx": true, ".ppk": true,
	".asc": true, ".gpg": true, ".pgp": true, ".env": true,
	".npmrc": true, ".netrc": true, ".pgpass": true, ".htpasswd": true,
}

// Formats named after their subject matter rather than their content. A
// stylesheet, an icon, a font or a document cannot carry a usable credential
// here, so its name is never inspected: password-reset.html is a mock of a
// password reset, not a password.
var presentationExtensions = map[string]bool{
	".css": true, ".svg": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".avif": true, ".ico": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".mp4": true, ".webm": true, ".mp3": true, ".wav": true, ".ogg": true,
	".gltf": true, ".glb": true,
	".pdf": true, ".rtf": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".md": true, ".markdown": true, ".html": true, ".htm": true,
}

// Word sequences that name a credential FILE. A name is credential-shaped when
// its own words START with one of these, so api-token.txt and
// credentials-export/ stay private while design-tokens.json publishes. Bare
// "token"/"tokens" is deliberately absent: it is design-system vocabulary far
// more often than it is a credential, and a filename filter cannot tell
// Style Dictionary's tokens.json from an OAuth cache anyway. The formats that
// could hold one are covered above; a secret VALUE is the redaction path's job.
var credentialPhrases = [][]string{
	{"credential"}, {"credentials"},
	{"secret"}, {"secrets"},
	{"password"}, {"passwords"}, {"passwd"}, {"htpasswd"},
	{"api", "key"}, {"api", "keys"}, {"apikey"}, {"apikeys"},
	{"api", "token"}, {"apitoken"},
	{"access", "token"}, {"refresh", "token"}, {"auth", "token"}, {"bearer", "token"},
	{"id", "token"}, {"session", "token"},
	{"client", "secret"}, {"private", "key"}, {"signing", "key"},
	{"service", "account"},
	{"id", "rsa"}, {"id", "dsa"}, {"id", "ecdsa"}, {"id", "ed25519"},
	{"authorized", "keys"},
	{"npmrc"}, {"netrc"}, {"pgpass"},
}

var unprintableRE = regexp.MustCompile(`[^\x20-\x7e]`)

// Artifact hosting is deliberately allowlisted. It covers ordinary web trees,
// documents, tabular/data exports, media, fonts, and common downloadable
// bundles without turning Workass into an arbitrary executable file server.
var allowedArtifactExtensions = map[string]bool{
	".html": true, ".htm": true,
	".css": true, ".js": true, ".mjs": true, ".wasm": true,
	".json": true, ".jsonl": true, ".ndjson": true, ".geojson": true, ".map": true,
	".md": true, ".markdown": true, ".txt": true, ".log": true,
	".csv": true, ".tsv": true, ".xml": true, ".yaml": true, ".yml": true, ".toml": true,
	".webmanifest": true,
	".pdf":         true, ".rtf": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true, ".ico": true, ".avif": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".mp4": true, ".webm": true, ".mp3": true, ".wav": true, ".ogg": true,
	".gltf": true, ".glb": true,
	".zip": true, ".tar": true, ".gz": true, ".tgz": true,
}

var artifactMIMEByExtension = map[string]string{
	".md":       "text/markdown; charset=utf-8",
	".markdown": "text/markdown; charset=utf-8",
	".txt":      "text/plain; charset=utf-8",
	".log":      "text/plain; charset=utf-8",
	".csv":      "text/csv; charset=utf-8",
	".tsv":      "text/tab-separated-values; charset=utf-8",
	".yaml":     "application/yaml; charset=utf-8",
	".yml":      "application/yaml; charset=utf-8",
	".toml":     "application/toml; charset=utf-8",
	".jsonl":    "application/x-ndjson; charset=utf-8",
	".ndjson":   "application/x-ndjson; charset=utf-8",
	".geojson":  "application/geo+json; charset=utf-8",
	".pdf":      "application/pdf",
	".docx":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx":     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".wasm":     "application/wasm",
	".gltf":     "model/gltf+json",
	".glb":      "model/gltf-binary",
	".zip":      "application/zip",
	".tar":      "application/x-tar",
	".gz":       "application/gzip",
	".tgz":      "application/gzip",
}

type artifactRecord struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	SourcePath string `json:"sourcePath"`
	RootPath   string `json:"rootPath"`
	Entry      string `json:"entry"`
	Kind       string `json:"kind"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

type registryFile struct {
	Version   int              `json:"version"`
	Artifacts []artifactRecord `json:"artifacts,omitempty"`
	Hosts     []artifactRecord `json:"hosts,omitempty"`
}

// RegisterOptions describes one source exposed by the daemon. SourcePath may
// be relative to BaseDir, but its resolved target must remain under that exact
// ACP session working directory.
type RegisterOptions struct {
	BaseDir    string
	SourcePath string
	Entry      string
	Label      string
}

// WithheldAsset names one member of a hosted tree that will answer 403, and
// why. A page whose stylesheet is withheld still returns 200 and renders
// unstyled, so the caller has to be told at host time or it learns by curling
// every asset by hand.
type WithheldAsset struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Registration is the bounded agent-facing receipt. Local filesystem paths
// stay daemon-private.
type Registration struct {
	ID           string          `json:"id"`
	Label        string          `json:"label"`
	Kind         string          `json:"kind"`
	Entry        string          `json:"entry"`
	ContentType  string          `json:"contentType"`
	URLPath      string          `json:"urlPath"`
	LocalURL     string          `json:"localUrl,omitempty"`
	Markdown     string          `json:"markdown"`
	CreatedAt    string          `json:"createdAt"`
	UpdatedAt    string          `json:"updatedAt"`
	Withheld     []WithheldAsset `json:"withheld,omitempty"`
	WithheldMore int             `json:"withheldMore,omitempty"`
}

// Registry persists stable source registrations and serves their current
// bytes read-only. It never copies or rewrites an agent-authored artifact.
type Registry struct {
	mu         sync.RWMutex
	path       string
	legacyPath string
	origin     string
	artifacts  map[string]artifactRecord
	now        func() time.Time
	logf       func(string, ...any)
	logged     map[string]bool
}

// SetLogger routes withheld-asset notices to the daemon log. Silence was the
// real defect behind the 2026-07-26 report: the page returned 200, only its
// stylesheet 403'd, and nothing anywhere recorded the decision.
func (r *Registry) SetLogger(logf func(format string, args ...any)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logf = logf
}

// noteWithheld logs one refusal per artifact/path pair. A reloading page must
// not be able to flood the daemon log with the same line.
func (r *Registry) noteWithheld(id, path, reason string) {
	r.mu.Lock()
	logf := r.logf
	key := id + "\x00" + path
	if logf == nil || r.logged[key] {
		r.mu.Unlock()
		return
	}
	if r.logged == nil || len(r.logged) >= maxLoggedWithholds {
		r.logged = make(map[string]bool)
	}
	r.logged[key] = true
	r.mu.Unlock()
	logf("[workass] artifact %s withheld %s: %s", id, displayName(path), reason)
}

func New(stateDir, origin string) (*Registry, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return nil, errors.New("artifact hosting state directory is empty")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("initialize artifact hosting state: %w", err)
	}
	registry := &Registry{
		path:       filepath.Join(stateDir, registryFilename),
		legacyPath: filepath.Join(stateDir, legacyRegistryFilename),
		origin:     strings.TrimRight(strings.TrimSpace(origin), "/"),
		artifacts:  make(map[string]artifactRecord),
		now:        time.Now,
	}
	loaded, _, err := registry.loadPath(registry.path, false)
	if err != nil {
		return nil, err
	}
	// Always inspect the legacy file, even after migration. This keeps a
	// rollback-safe mixed-version window: if an older daemon registered another
	// HTML host, the next artifact-aware daemon folds in that missing id instead
	// of silently losing it. Current artifact records win on duplicate ids.
	legacyLoaded, legacyAdded, legacyErr := registry.loadPath(registry.legacyPath, true)
	if legacyErr != nil {
		return nil, legacyErr
	}
	if legacyLoaded && (!loaded || legacyAdded) {
		registry.mu.Lock()
		err = registry.persistLocked()
		registry.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("migrate legacy HTML hosting registry: %w", err)
		}
	}
	return registry, nil
}

func (r *Registry) Register(options RegisterOptions) (Registration, error) {
	if r == nil {
		return Registration{}, errors.New("artifact hosting is unavailable")
	}
	baseDir, err := canonicalDirectory(options.BaseDir)
	if err != nil {
		return Registration{}, fmt.Errorf("agent working directory is not readable: %w", err)
	}
	sourcePath := strings.TrimSpace(options.SourcePath)
	if sourcePath == "" {
		return Registration{}, errors.New("source_path is required")
	}
	if !filepath.IsAbs(sourcePath) {
		sourcePath = filepath.Join(baseDir, sourcePath)
	}
	sourcePath, err = canonicalExistingPath(sourcePath)
	if err != nil {
		return Registration{}, fmt.Errorf("artifact source is not readable: %w", err)
	}
	if !isWithin(baseDir, sourcePath) {
		return Registration{}, errors.New("artifact source must stay within the calling agent's working directory")
	}

	info, err := os.Stat(sourcePath)
	if err != nil {
		return Registration{}, fmt.Errorf("inspect artifact source: %w", err)
	}
	record := artifactRecord{SourcePath: sourcePath}
	switch {
	case info.Mode().IsRegular():
		entry := filepath.Base(sourcePath)
		if reason := withholdReason(entry); reason != "" {
			return Registration{}, errors.New("source_path cannot be hosted: " + reason)
		}
		record.Kind = "file"
		record.RootPath = filepath.Dir(sourcePath)
		record.Entry = entry
	case info.IsDir():
		if credentialShapedName(filepath.Base(sourcePath)) {
			return Registration{}, errors.New("source_path cannot be hosted: the directory " +
				displayName(filepath.Base(sourcePath)) + " is named like a credential store")
		}
		record.Kind = "directory"
		record.RootPath = sourcePath
		record.Entry = strings.TrimSpace(options.Entry)
		if record.Entry == "" {
			if indexPath, ok := resolveExistingUnder(record.RootPath, "index.html"); ok {
				if indexInfo, statErr := os.Stat(indexPath); statErr == nil && indexInfo.Mode().IsRegular() {
					record.Entry = "index.html"
				}
			}
		}
		if record.Entry == "" {
			return Registration{}, errors.New("entry is required for an artifact directory without index.html")
		}
		cleanEntry, ok := cleanRelativePath(record.Entry)
		if !ok {
			return Registration{}, errors.New("entry must be a relative path inside the hosted directory")
		}
		if reason := withholdReason(cleanEntry); reason != "" {
			return Registration{}, errors.New("entry cannot be hosted: " + reason)
		}
		entryPath, ok := resolveExistingUnder(record.RootPath, cleanEntry)
		if !ok {
			return Registration{}, errors.New("entry is not a readable artifact inside the hosted directory")
		}
		entryInfo, statErr := os.Stat(entryPath)
		if statErr != nil || !entryInfo.Mode().IsRegular() {
			return Registration{}, errors.New("entry is not a readable artifact inside the hosted directory")
		}
		record.Entry = cleanEntry
	default:
		return Registration{}, errors.New("artifact source must be a regular file or directory")
	}

	label := strings.TrimSpace(options.Label)
	if label == "" {
		if record.Kind == "file" {
			label = strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
		} else {
			label = filepath.Base(sourcePath)
		}
	}
	record.Label = safeLabel(label)

	// Inspect the tree before taking the lock: this is filesystem work, and the
	// answer does not depend on the registry.
	var withheld []WithheldAsset
	var withheldMore int
	if record.Kind == "directory" {
		withheld, withheldMore = inspectDirectory(record.RootPath)
	}
	registration, err := r.commit(record)
	if err != nil {
		return Registration{}, err
	}
	registration.Withheld, registration.WithheldMore = withheld, withheldMore
	for _, asset := range withheld {
		r.noteWithheld(registration.ID, asset.Path, asset.Reason)
	}
	return registration, nil
}

func (r *Registry) commit(record artifactRecord) (Registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, existing := range r.artifacts {
		if existing.SourcePath == record.SourcePath && existing.Entry == record.Entry && existing.Kind == record.Kind {
			record.ID = id
			record.CreatedAt = existing.CreatedAt
			break
		}
	}
	if record.ID == "" {
		record.ID = r.availableIDLocked(record.Label, record.SourcePath+"\x00"+record.Entry+"\x00"+record.Kind)
		record.CreatedAt = r.now().UTC().Format(time.RFC3339Nano)
	}
	record.UpdatedAt = r.now().UTC().Format(time.RFC3339Nano)
	previous, existed := r.artifacts[record.ID]
	r.artifacts[record.ID] = record
	if err := r.persistLocked(); err != nil {
		if existed {
			r.artifacts[record.ID] = previous
		} else {
			delete(r.artifacts, record.ID)
		}
		return Registration{}, err
	}
	return r.registration(record), nil
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	setHostedHeaders(w)
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requestPath, err := url.PathUnescape(request.URL.EscapedPath())
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	prefix := artifactPathPrefix(requestPath)
	if prefix == "" {
		http.NotFound(w, request)
		return
	}
	rest := strings.TrimPrefix(requestPath, prefix)
	if rest == "" || rest == "/" {
		http.NotFound(w, request)
		return
	}
	rest = strings.TrimPrefix(rest, "/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, request)
		return
	}

	r.mu.RLock()
	record, ok := r.artifacts[id]
	r.mu.RUnlock()
	if !ok {
		http.NotFound(w, request)
		return
	}
	if len(parts) == 1 {
		target := prefix + "/" + id + "/"
		if request.URL.RawQuery != "" {
			target += "?" + request.URL.RawQuery
		}
		http.Redirect(w, request, target, http.StatusTemporaryRedirect)
		return
	}

	rel := parts[1]
	if rel == "" {
		rel = record.Entry
	} else if strings.HasSuffix(rel, "/") {
		rel += "index.html"
	}
	cleanRel, clean := cleanRelativePath(rel)
	if !clean {
		http.Error(w, "forbidden: that is not a relative path inside the hosted artifact", http.StatusForbidden)
		return
	}
	if reason := withholdReason(cleanRel); reason != "" {
		r.noteWithheld(id, cleanRel, reason)
		// The header carries the decision to a browser's network panel, where a
		// missing stylesheet is otherwise the only symptom.
		w.Header().Set("X-Workass-Withheld", reason)
		http.Error(w, "withheld: "+reason, http.StatusForbidden)
		return
	}
	if record.Kind == "file" && cleanRel != record.Entry {
		http.NotFound(w, request)
		return
	}
	filePath, inside := resolveExistingUnder(record.RootPath, cleanRel)
	if !inside {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	info, err := os.Stat(filePath)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, request)
		return
	}
	file, err := os.Open(filePath)
	if err != nil {
		http.NotFound(w, request)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", artifactContentType(cleanRel))
	if disposition := mime.FormatMediaType("inline", map[string]string{"filename": filepath.Base(cleanRel)}); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	http.ServeContent(w, request, filepath.Base(cleanRel), info.ModTime(), file)
}

func artifactPathPrefix(path string) string {
	for _, prefix := range []string{PathPrefix, LegacyPathPrefix} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return prefix
		}
	}
	return ""
}

func artifactContentType(path string) string {
	extension := strings.ToLower(filepath.Ext(path))
	if contentType := artifactMIMEByExtension[extension]; contentType != "" {
		return contentType
	}
	contentType := mime.TypeByExtension(extension)
	if contentType == "" {
		return "application/octet-stream"
	}
	if isTextualArtifact(extension) && !strings.Contains(strings.ToLower(contentType), "charset=") {
		return strings.TrimSpace(strings.Split(contentType, ";")[0]) + "; charset=utf-8"
	}
	return contentType
}

func isTextualArtifact(extension string) bool {
	switch extension {
	case ".html", ".htm", ".css", ".js", ".mjs", ".json", ".map", ".xml", ".webmanifest", ".svg":
		return true
	default:
		return false
	}
}

func (r *Registry) registration(record artifactRecord) Registration {
	path := PathPrefix + "/" + record.ID + "/"
	localURL := ""
	if r.origin != "" {
		localURL = r.origin + path
	}
	return Registration{
		ID: record.ID, Label: record.Label, Kind: record.Kind, Entry: record.Entry,
		ContentType: artifactContentType(record.Entry), URLPath: path, LocalURL: localURL,
		Markdown:  artifactMarkdown(record.Label, record.Entry, path),
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

func artifactMarkdown(label, entry, path string) string {
	label = strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(strings.TrimSpace(label))
	if label == "" {
		label = "artifact"
	}
	switch strings.ToLower(filepath.Ext(entry)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif":
		return "![" + label + "](" + path + ")"
	default:
		return "[Open " + label + "](" + path + ")"
	}
}

func (r *Registry) availableIDLocked(label, seed string) string {
	sum := sha256.Sum256([]byte(seed))
	hash := hex.EncodeToString(sum[:])
	for size := 12; size <= len(hash); size += 4 {
		id := label + "-" + hash[:size]
		if _, ok := r.artifacts[id]; !ok {
			return id
		}
	}
	for suffix := 2; ; suffix++ {
		id := fmt.Sprintf("%s-%s-%d", label, hash, suffix)
		if _, ok := r.artifacts[id]; !ok {
			return id
		}
	}
}

func (r *Registry) loadPath(path string, keepExisting bool) (bool, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("open artifact hosting registry: %w", err)
	}
	defer file.Close()
	var stored registryFile
	decoder := json.NewDecoder(io.LimitReader(file, maxRegistryBytes))
	if err := decoder.Decode(&stored); err != nil {
		return false, false, fmt.Errorf("read artifact hosting registry: %w", err)
	}
	if stored.Version != registryVersion {
		return false, false, fmt.Errorf("unsupported artifact hosting registry version %d", stored.Version)
	}
	records := stored.Artifacts
	if len(records) == 0 {
		records = stored.Hosts
	}
	added := false
	for _, record := range records {
		if !validStoredRecord(record) {
			return false, false, fmt.Errorf("invalid artifact hosting registry record %q", record.ID)
		}
		if _, exists := r.artifacts[record.ID]; exists && keepExisting {
			continue
		}
		r.artifacts[record.ID] = record
		added = true
	}
	return true, added, nil
}

func (r *Registry) persistLocked() error {
	records := make([]artifactRecord, 0, len(r.artifacts))
	for _, record := range r.artifacts {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	data, err := json.MarshalIndent(registryFile{Version: registryVersion, Artifacts: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode artifact hosting registry: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".artifact-hosts-*.tmp")
	if err != nil {
		return fmt.Errorf("write artifact hosting registry: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure artifact hosting registry: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write artifact hosting registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close artifact hosting registry: %w", err)
	}
	_ = os.Remove(r.path)
	if err := os.Rename(tmpPath, r.path); err != nil {
		return fmt.Errorf("commit artifact hosting registry: %w", err)
	}
	return nil
}

func validStoredRecord(record artifactRecord) bool {
	if record.ID == "" || record.ID != safeID(record.ID) || record.Label == "" ||
		!filepath.IsAbs(record.SourcePath) || !filepath.IsAbs(record.RootPath) ||
		(record.Kind != "file" && record.Kind != "directory") {
		return false
	}
	entry, ok := cleanRelativePath(record.Entry)
	if !ok || entry != record.Entry || !allowedArtifactPath(entry) {
		return false
	}
	source := filepath.Clean(record.SourcePath)
	root := filepath.Clean(record.RootPath)
	if record.Kind == "file" {
		return filepath.Dir(source) == root && filepath.Base(source) == entry
	}
	return source == root
}

func canonicalDirectory(path string) (string, error) {
	canonical, err := canonicalExistingPath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return canonical, nil
}

func canonicalExistingPath(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(strings.TrimSpace(path)))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func resolveExistingUnder(root, rel string) (string, bool) {
	cleanRel, ok := cleanRelativePath(rel)
	if !ok {
		return "", false
	}
	rootPath, err := canonicalDirectory(root)
	if err != nil {
		return "", false
	}
	candidate, err := canonicalExistingPath(filepath.Join(rootPath, filepath.FromSlash(cleanRel)))
	if err != nil || !isWithin(rootPath, candidate) {
		return "", false
	}
	return candidate, true
}

func isWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func cleanRelativePath(path string) (string, bool) {
	path = strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	path = strings.TrimPrefix(path, "/")
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || !fs.ValidPath(clean) {
		return "", false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return "", false
		}
	}
	return clean, true
}

func allowedArtifactPath(path string) bool {
	return withholdReason(path) == ""
}

// withholdReason explains, in the words the caller and the daemon log both see,
// why a path inside a hosted tree will not be served. It returns "" for a
// publishable path. Every refusal must be able to say why: a withheld asset
// that surfaces only as an unstyled page is the worst failure this feature has.
func withholdReason(path string) string {
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	for _, directory := range parts[:len(parts)-1] {
		if credentialShapedName(directory) {
			return "the directory " + displayName(directory) + " is named like a credential store"
		}
	}
	extension := strings.ToLower(filepath.Ext(name))
	if credentialExtensions[extension] {
		return "a " + extension + " file is a key or credential store, never a publishable artifact"
	}
	if !allowedArtifactExtensions[extension] {
		if extension == "" {
			return "a file with no extension is not a supported artifact format"
		}
		return extension + " is not a supported artifact format"
	}
	if !presentationExtensions[extension] && credentialShapedName(strings.TrimSuffix(name, filepath.Ext(name))) {
		return displayName(name) + " is named like a credential file"
	}
	return ""
}

// inspectDirectory reports what inside a hosted tree will answer 403, so the
// caller learns it at host time instead of by curling every asset by hand. The
// walk is bounded — a hosted directory can be a whole repository — and hidden
// entries are skipped: "no dotfiles" is a blanket rule of the server rather
// than a surprise, and reporting it would mean reporting all of .git.
func inspectDirectory(root string) ([]WithheldAsset, int) {
	type finding struct {
		asset   WithheldAsset
		urgency int
	}
	findings := make([]finding, 0, 16)
	inspected := 0
	truncated := false
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		inspected++
		if inspected > maxInspectedEntries {
			truncated = true
			return fs.SkipAll
		}
		if entry.IsDir() {
			if credentialShapedName(name) {
				findings = append(findings, finding{WithheldAsset{
					Path: relative + "/", Reason: "named like a credential store; nothing under it is served",
				}, 0})
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		reason := withholdReason(relative)
		if reason == "" {
			return nil
		}
		urgency := 1
		if !strings.Contains(reason, "supported artifact format") {
			urgency = 0
		}
		findings = append(findings, finding{WithheldAsset{Path: relative, Reason: reason}, urgency})
		return nil
	})
	// Credential refusals sort first so the security-relevant lines survive the
	// cap; unsupported formats are the expected, documented half.
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].urgency != findings[j].urgency {
			return findings[i].urgency < findings[j].urgency
		}
		return findings[i].asset.Path < findings[j].asset.Path
	})
	if truncated {
		findings = append(findings, finding{WithheldAsset{
			Path:   "…",
			Reason: fmt.Sprintf("stopped after %d entries; anything deeper was not inspected", maxInspectedEntries),
		}, 2})
	}
	more := 0
	if len(findings) > maxWithheldReported {
		more = len(findings) - maxWithheldReported
		findings = findings[:maxWithheldReported]
	}
	withheld := make([]WithheldAsset, 0, len(findings))
	for _, item := range findings {
		withheld = append(withheld, item.asset)
	}
	if len(withheld) == 0 {
		return nil, 0
	}
	return withheld, more
}

// credentialShapedName reports whether a bare name (a directory, or a filename
// with its extension already removed) opens with a credential phrase.
func credentialShapedName(stem string) bool {
	words := nameWords(stem)
	if len(words) == 0 {
		return false
	}
	for _, phrase := range credentialPhrases {
		if len(phrase) > len(words) {
			continue
		}
		match := true
		for i, word := range phrase {
			if words[i] != word {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// nameWords splits a name the way its author reads it: on separators and on
// camel-case boundaries, so api-token, api_token and apiToken are one shape.
// Digits stay attached to their word, keeping id_ed25519 intact.
func nameWords(name string) []string {
	words := make([]string, 0, 4)
	current := make([]rune, 0, 16)
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	previous := rune(0)
	for _, char := range name {
		switch {
		case unicode.IsUpper(char) && unicode.IsLower(previous):
			flush()
			current = append(current, char)
		case unicode.IsLetter(char) || unicode.IsDigit(char):
			current = append(current, char)
		default:
			flush()
		}
		previous = char
	}
	flush()
	return words
}

// displayName bounds a name before it reaches a log line, an HTTP header or an
// error string. The name comes from the request path, so it is untrusted text.
func displayName(name string) string {
	name = unprintableRE.ReplaceAllString(name, "")
	name = strings.NewReplacer("\"", "", "\\", "").Replace(name)
	if len(name) > 80 {
		name = name[:80] + "…"
	}
	if name == "" {
		return "that name"
	}
	return "\"" + name + "\""
}

func safeLabel(label string) string {
	var out strings.Builder
	dash := false
	for _, char := range strings.ToLower(strings.TrimSpace(label)) {
		if char <= unicode.MaxASCII && (unicode.IsLetter(char) || unicode.IsDigit(char)) {
			if dash && out.Len() > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(char)
			dash = false
		} else {
			dash = out.Len() > 0
		}
		if out.Len() >= 48 {
			break
		}
	}
	result := strings.Trim(out.String(), "-")
	// The label becomes part of a public URL, so a credential-shaped label is
	// dropped rather than published even though it names nothing on disk.
	if result == "" || credentialShapedName(result) {
		return "artifact"
	}
	return result
}

func safeID(id string) string {
	for _, char := range id {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '-' {
			return ""
		}
	}
	return id
}

func setHostedHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-downloads; connect-src https:; frame-ancestors 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
