package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// The chat image store, state/images/, holds each image under its content
// digest in one of two forms. The digest never changes between them: it is
// sha256 of the standard base64 text of the image bytes, which is what every
// Ref, URL, wire field and SQLite row already names.
//
//   - images/<digest>      legacy form: the base64 TEXT, so sha256(file) is
//     the digest. Every build up to 0.1.229 writes and reads only this.
//   - images/<digest>.raw  raw form: the image BYTES, so
//     sha256(base64(file)) is the digest. A third smaller, and on APFS it is
//     usually a clone of the file the agent looked at, costing no blocks.
//
// Readers take the raw form first and fall back to the legacy one, so a store
// that holds either, or both, serves the same bytes. Base64 is produced only
// where a consumer needs it (a provider prompt, a socket image read). The
// ".raw" suffix cannot be confused with a legacy name: validSessionImageRef
// accepts exactly 64 hex characters, so no Ref can ever name a raw file.
const sessionImageRawSuffix = ".raw"

// sessionImageFile is one stored form of an image.
type sessionImageFile struct {
	name string
	path string
	raw  bool
	info os.FileInfo
}

// logicalSize is the size of the image's base64 text, the number attachments
// have always recorded as Size. A raw file of n bytes stands for exactly
// EncodedLen(n) characters, because only canonical base64 is stored raw.
func (f sessionImageFile) logicalSize() int64 {
	if f.raw {
		return int64(base64.StdEncoding.EncodedLen(int(f.info.Size())))
	}
	return f.info.Size()
}

func sessionImageLegacyPath(stateDir, name string) string {
	return filepath.Join(stateDir, sessionImageDirname, name)
}

func sessionImageRawPath(stateDir, name string) string {
	return filepath.Join(stateDir, sessionImageDirname, name+sessionImageRawSuffix)
}

// locateSessionImage finds the form a reader should use: raw when present,
// legacy otherwise. Both are bounded regular files.
func locateSessionImage(ref, stateDir string) (sessionImageFile, error) {
	name, ok := validSessionImageRef(ref)
	if !ok {
		return sessionImageFile{}, fmt.Errorf("invalid session image ref %q", ref)
	}
	if file, err := statSessionImageForm(stateDir, name, true); err == nil {
		return file, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// A raw form that is not a bounded regular file is unusable, but the
		// legacy form may still be intact.
		if legacy, legacyErr := statSessionImageForm(stateDir, name, false); legacyErr == nil {
			return legacy, nil
		}
		return sessionImageFile{}, fmt.Errorf("read session image ref %s: %w", ref, err)
	}
	file, err := statSessionImageForm(stateDir, name, false)
	if err != nil {
		return sessionImageFile{}, fmt.Errorf("read session image ref %s: %w", ref, err)
	}
	return file, nil
}

func statSessionImageForm(stateDir, name string, raw bool) (sessionImageFile, error) {
	path := sessionImageLegacyPath(stateDir, name)
	if raw {
		path = sessionImageRawPath(stateDir, name)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return sessionImageFile{}, err
	}
	file := sessionImageFile{name: name, path: path, raw: raw, info: info}
	if !info.Mode().IsRegular() || file.logicalSize() > maxPersistedSessionImageBytes {
		return sessionImageFile{}, fmt.Errorf("session image %s is not a bounded regular file", filepath.Base(path))
	}
	return file, nil
}

// sessionImageRawDigest is the content address of raw image bytes: sha256 of
// their standard base64 text, streamed so the text is never materialized.
func sessionImageRawDigest(raw []byte) string {
	hash := sha256.New()
	encoder := base64.NewEncoder(base64.StdEncoding, hash)
	_, _ = encoder.Write(raw)
	_ = encoder.Close()
	return hex.EncodeToString(hash.Sum(nil))
}

func sessionImageTextDigest(text []byte) string {
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:])
}

// decodeCanonicalSessionImage returns the bytes of base64 text when the text
// is exactly their standard encoding, the only text a raw file can stand for.
// A data URL, unpadded or line-wrapped base64, or non-zero padding bits keep
// their text identity and stay in the legacy form.
func decodeCanonicalSessionImage(text string) ([]byte, bool) {
	return decodeCanonicalSessionImageInto(nil, []byte(text))
}

// decodeCanonicalSessionImageInto is decodeCanonicalSessionImage decoding
// into dst's storage when it is large enough (a pass over the store reuses
// one buffer for every image).
func decodeCanonicalSessionImageInto(dst, text []byte) ([]byte, bool) {
	if len(text) == 0 || len(text)%4 != 0 {
		return nil, false
	}
	need := base64.StdEncoding.DecodedLen(len(text))
	if cap(dst) < need {
		dst = make([]byte, need)
	}
	dst = dst[:need]
	n, err := base64.StdEncoding.Strict().Decode(dst, text)
	if err != nil || base64.StdEncoding.EncodedLen(n) != len(text) {
		return nil, false
	}
	return dst[:n], true
}

// readSessionImageForm reads one form and verifies it against its digest.
// Raw bytes come back as raw; legacy text comes back as text.
func readSessionImageForm(file sessionImageFile) ([]byte, error) {
	data, err := os.ReadFile(file.path)
	if err != nil {
		return nil, err
	}
	digest := ""
	if file.raw {
		digest = sessionImageRawDigest(data)
	} else {
		digest = sessionImageTextDigest(data)
	}
	if digest != file.name {
		return nil, fmt.Errorf("session image %s failed content verification", filepath.Base(file.path))
	}
	return data, nil
}

// readSessionImageVerified reads the image named by ref from whichever form
// verifies, raw first. raw reports which form data is.
func readSessionImageVerified(ref, stateDir string) (data []byte, raw bool, err error) {
	file, err := locateSessionImage(ref, stateDir)
	if err != nil {
		return nil, false, err
	}
	data, err = readSessionImageForm(file)
	if err == nil {
		return data, file.raw, nil
	}
	if file.raw {
		// A damaged raw file never hides an intact legacy one.
		if legacy, legacyErr := statSessionImageForm(stateDir, file.name, false); legacyErr == nil {
			if text, textErr := readSessionImageForm(legacy); textErr == nil {
				return text, false, nil
			}
		}
	}
	return nil, false, fmt.Errorf("read session image ref %s: %w", ref, err)
}

// readSessionImageBytes returns the decoded image bytes for ref.
func readSessionImageBytes(ref, stateDir string) ([]byte, error) {
	data, raw, err := readSessionImageVerified(ref, stateDir)
	if err != nil {
		return nil, err
	}
	if raw {
		return data, nil
	}
	return decodeSessionImageText(data)
}

// decodeSessionImageText decodes legacy sidecar text: base64, or a data URL
// whose base64 payload follows the first comma.
func decodeSessionImageText(body []byte) ([]byte, error) {
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

// sessionImageLegacyShadow is whether a new image also gets its legacy file.
// Until the rollback window has passed (session_image_maintenance.go), the
// previous release, which reads only legacy files, must still find every
// image, including ones written by this build. It starts on and is turned off
// only by the maintenance pass that has proven the window closed.
var sessionImageLegacyShadow atomic.Bool

func init() { sessionImageLegacyShadow.Store(true) }

// sessionImageStoreMu orders image writers against deletion. Writers and the
// migration hold it shared; the deleted-chat collector holds it exclusively
// only while it removes files, after rechecking that nothing touched them.
var sessionImageStoreMu sync.RWMutex

// sessionImageTouches remembers when this process last wrote or re-used each
// digest. A write lands before the actor commit that references it, so the
// collector must never take a digest someone is about to reference.
var sessionImageTouches sync.Map

func touchSessionImage(name string) {
	if name != "" {
		sessionImageTouches.Store(name, time.Now())
	}
}

func sessionImageTouchedSince(name string, since time.Time) bool {
	value, ok := sessionImageTouches.Load(name)
	if !ok {
		return false
	}
	at, ok := value.(time.Time)
	return ok && !at.Before(since)
}

// sessionImageWriteStats counts how new raw images were stored, for metrics.
var sessionImageWriteStats struct {
	cloned, written, legacy atomic.Int64
}

// sessionImageClone is the platform clone (cloneRawSessionImage); tests
// replace it to exercise another platform's path.
var sessionImageClone = cloneRawSessionImage

// rawSessionImageResult is how persistRawSessionImage stored an image.
type rawSessionImageResult struct {
	// method: "existing" (a verified raw file was already there), "cloned"
	// or "written".
	method string
	// source is the clone source when method is "cloned".
	source string
	// attempts holds the outcome of every candidate tried, in order.
	attempts []sessionImageCloneOutcome
}

func (r rawSessionImageResult) tried(outcome sessionImageCloneOutcome) bool {
	for _, attempt := range r.attempts {
		if attempt == outcome {
			return true
		}
	}
	return false
}

// sessionImageDurability is how hard a raw write is synced.
type sessionImageDurability int

const (
	// sessionImageDurable: the file and its directory are flushed to stable
	// storage before the write returns. Every write a chat commit waits on.
	sessionImageDurable sessionImageDurability = iota
	// sessionImageBatched: the file's data is handed to the device (plain
	// fsync) and the directory is not synced; the caller owns a later barrier
	// (sessionImageBarrier) before it relies on the file. The migration, whose
	// legacy files stay authoritative until that barrier has passed.
	sessionImageBatched
)

// sessionImageCopyBuffers are reused by every compare and verification read,
// so a pass over the store does not allocate per image.
var sessionImageCopyBuffers = sync.Pool{New: func() any { buffer := make([]byte, 256<<10); return &buffer }}

// persistRawSessionImage stores raw (already known to be the content of name)
// as images/<name>.raw: an APFS clone of one of cloneSources when one holds
// exactly these bytes, otherwise a plain write. The result is verified before
// it is renamed into place. raw is not retained.
func persistRawSessionImage(dir, name string, raw []byte, cloneSources []string, durability sessionImageDurability) (rawSessionImageResult, error) {
	final := filepath.Join(dir, name+sessionImageRawSuffix)
	if info, statErr := os.Lstat(final); statErr == nil {
		if info.Mode().IsRegular() && info.Size() == int64(len(raw)) {
			if same, readErr := sessionImageFileEquals(final, raw); readErr == nil && same {
				markSessionImageVerified(final, info)
				return rawSessionImageResult{method: "existing"}, nil
			}
		}
		// A damaged raw file is replaced below by a verified one.
	} else if !os.IsNotExist(statErr) {
		return rawSessionImageResult{}, statErr
	}
	result := rawSessionImageResult{}
	for _, candidate := range cloneSources {
		outcome, _ := sessionImageClone(dir, final, name, raw, candidate, durability)
		result.attempts = append(result.attempts, outcome)
		if outcome == cloneOK {
			result.method, result.source = "cloned", candidate
			sessionImageWriteStats.cloned.Add(1)
			return result, nil
		}
		if outcome == cloneUnsupported {
			// The volume cannot clone at all; other candidates cannot either.
			break
		}
	}
	if err := writeRawSessionImage(dir, final, name, raw, durability); err != nil {
		return result, err
	}
	result.method = "written"
	sessionImageWriteStats.written.Add(1)
	return result, nil
}

// sessionImageFileEquals compares a file with want without reading it whole.
func sessionImageFileEquals(path string, want []byte) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	return sessionImageReaderEquals(file, want)
}

func sessionImageReaderEquals(reader io.Reader, want []byte) (bool, error) {
	bufferRef := sessionImageCopyBuffers.Get().(*[]byte)
	defer sessionImageCopyBuffers.Put(bufferRef)
	buffer := *bufferRef
	offset := 0
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if offset+n > len(want) || !bytes.Equal(buffer[:n], want[offset:offset+n]) {
				return false, nil
			}
			offset += n
		}
		if errors.Is(err, io.EOF) {
			return offset == len(want), nil
		}
		if err != nil {
			return false, err
		}
	}
}

// writeRawSessionImage writes raw through a synced temporary file, verifies
// what reached the disk, and renames it into place.
func writeRawSessionImage(dir, final, name string, raw []byte, durability sessionImageDurability) error {
	tmp, err := os.CreateTemp(dir, ".image-raw-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := syncSessionImageData(tmp, durability); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := verifyRawSessionImageFile(tmpName, name, int64(len(raw))); err != nil {
		return err
	}
	return publishRawSessionImage(tmpName, final, dir, durability)
}

// errRawSessionImageVerify marks a raw file whose bytes are not its digest's.
var errRawSessionImageVerify = errors.New("raw session image failed verification")

// verifyRawSessionImageFile reads path back and checks it is name's content.
func verifyRawSessionImageFile(path, name string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return fmt.Errorf("%w: %s has the wrong size", errRawSessionImageVerify, name)
	}
	hash := sha256.New()
	encoder := base64.NewEncoder(base64.StdEncoding, hash)
	bufferRef := sessionImageCopyBuffers.Get().(*[]byte)
	_, err = io.CopyBuffer(encoder, file, *bufferRef)
	sessionImageCopyBuffers.Put(bufferRef)
	if err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	var sum [sha256.Size]byte
	if hex.EncodeToString(hash.Sum(sum[:0])) != name {
		return fmt.Errorf("%w: %s", errRawSessionImageVerify, name)
	}
	return nil
}

func publishRawSessionImage(tmpName, final, dir string, durability sessionImageDurability) error {
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	if durability == sessionImageDurable {
		if err := syncDirectory(dir); err != nil {
			return err
		}
	}
	if info, err := os.Lstat(final); err == nil {
		markSessionImageVerified(final, info)
	}
	return nil
}

// syncSessionImageData makes a written file durable to the requested degree.
func syncSessionImageData(file *os.File, durability sessionImageDurability) error {
	if durability == sessionImageDurable {
		return file.Sync()
	}
	return syncSessionImageDataToDevice(file)
}

// sessionImageBarrier makes every batched write before it durable: the
// directory's entries and, on macOS, the device's write cache (one
// F_FULLFSYNC flushes all data already handed to the device).
func sessionImageBarrier(dir string) error {
	return syncDirectory(dir)
}
