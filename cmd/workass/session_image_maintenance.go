package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The image store's background maintenance. One pass, run a couple of minutes
// after start and then daily, does three things, each resumable and safe to
// interrupt at any instant:
//
//  1. Migration. Every legacy file (base64 text) gets a raw twin
//     (images/<digest>.raw), cloned from the file a chat event names when that
//     file still holds exactly these bytes, written otherwise. The legacy file
//     stays.
//  2. Reclaim. Once every legacy image has a verified raw twin and the
//     rollback window has passed, each raw twin is verified again and its
//     legacy file is removed.
//  3. Collection. Images that only chats deleted more than a grace period ago
//     reference are removed (session_image_gc.go).
//
// ROLLBACK. Release 0.1.229 and older read only legacy files. Until reclaim,
// every image has its legacy file (this build writes one beside each new raw
// image too), so an older build sees the whole store. The window starts when a
// pass first finds the store complete; a legacy-only image newer than that
// start can only have been written by an older build (this one always writes
// the raw form first), so finding one restarts the window. After reclaim, an
// older build finds no file for a reclaimed image: the message keeps its
// metadata but the image is missing (session reads drop its data, the image
// URL answers 404, a resend of it by reference fails). Nothing is lost: the raw
// files stay, and this build or a later one serves them again.
//
// Receipts (image-store/receipts.jsonl) record every image's outcome with the
// raw file's size and mtime, like the SQLite import receipts: a later pass
// skips an image whose raw file still matches its receipt, and re-verifies
// one that does not. The marker (image-store/marker.json) records the window.
const (
	// imageStoreRollbackWindow is how long legacy files stay after the raw
	// store is complete. A constant: the release that ships this is the only
	// one whose predecessor cannot read raw files.
	imageStoreRollbackWindow = 7 * 24 * time.Hour
	// imageStoreDeletedChatGrace is how long a deleted chat's images stay.
	imageStoreDeletedChatGrace = 7 * 24 * time.Hour
	// imageStoreTouchHold keeps the collector off an image this process
	// wrote or re-used this recently: its referencing commit may be pending.
	imageStoreTouchHold  = time.Hour
	imageStoreStartDelay = 2 * time.Minute
	imageStoreInterval   = 24 * time.Hour
	// imageStoreBytesPerSecond bounds the pass's reads so chat opens and
	// streaming never wait behind it.
	imageStoreBytesPerSecond = 64 << 20
	// imageStoreTempMaxAge: a temporary file this old belongs to no live write.
	imageStoreTempMaxAge = time.Hour
	// imageStoreForegroundTail: the pass waits this long after a client's
	// last request (a chat open is a burst of them) before its next image.
	imageStoreForegroundTail = 300 * time.Millisecond

	imageStoreDirname          = "image-store"
	imageStoreMarkerFilename   = "marker.json"
	imageStoreReceiptsFilename = "receipts.jsonl"
	imageStoreMarkerSchema     = 1
)

// Receipt outcomes.
const (
	imageReceiptCloned           = "cloned"
	imageReceiptWritten          = "written"
	imageReceiptVerified         = "verified"
	imageReceiptKeptLegacy       = "kept-legacy"
	imageReceiptLegacyUnverified = "legacy-unverified"
	imageReceiptRawVerifyFailed  = "raw-verify-failed"
	imageReceiptRetired          = "retired"
	imageReceiptCollected        = "collected"
)

type imageStoreMarker struct {
	Schema int `json:"schema"`
	// WindowStartedAt is when a pass first found every legacy image with a
	// verified raw twin; zero until then, and again after an older build is
	// seen writing.
	WindowStartedAt time.Time         `json:"windowStartedAt,omitzero"`
	WindowStartedBy string            `json:"windowStartedBy,omitempty"`
	WindowResets    int               `json:"windowResets,omitempty"`
	LastPassAt      time.Time         `json:"lastPassAt,omitzero"`
	LastPassBy      string            `json:"lastPassBy,omitempty"`
	LastReport      *imageStoreReport `json:"lastReport,omitempty"`
}

func (m imageStoreMarker) reclaimAllowed(now time.Time, window time.Duration) bool {
	return !m.WindowStartedAt.IsZero() && now.Sub(m.WindowStartedAt) >= window
}

type imageStoreReceipt struct {
	Digest        string `json:"digest"`
	Outcome       string `json:"outcome"`
	Detail        string `json:"detail,omitempty"`
	Source        string `json:"source,omitempty"`
	RawSize       int64  `json:"rawSize,omitempty"`
	RawMTimeNS    int64  `json:"rawMtimeNs,omitempty"`
	LegacySize    int64  `json:"legacySize,omitempty"`
	At            string `json:"at"`
	BinaryVersion string `json:"binaryVersion,omitempty"`
}

// imageStoreReport counts one pass. Bytes are on-disk legacy (base64) bytes
// unless named raw.
type imageStoreReport struct {
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Phase      string    `json:"phase"`
	Error      string    `json:"error,omitempty"`

	LegacyFiles  int `json:"legacyFiles"`
	RawFiles     int `json:"rawFiles"`
	Processed    int `json:"processed"`
	HintedImages int `json:"hintedImages"`

	Cloned          int64 `json:"cloned"`
	ClonedRawBytes  int64 `json:"clonedRawBytes"`
	Written         int64 `json:"written"`
	WrittenRawBytes int64 `json:"writtenRawBytes"`
	AlreadyRaw      int64 `json:"alreadyRaw"`
	RawRepaired     int64 `json:"rawRepaired"`

	// Why a written image was not cloned (one reason per image, the first
	// that applies in this order).
	SkippedProtected     int64 `json:"skippedProtected"`
	CandidateDifferent   int64 `json:"candidateDifferent"`
	CandidateMissing     int64 `json:"candidateMissing"`
	CandidateOtherVolume int64 `json:"candidateOtherVolume"`
	CloneFailed          int64 `json:"cloneFailed"`
	CloneUnsupported     int64 `json:"cloneUnsupported"`
	NoCandidate          int64 `json:"noCandidate"`

	KeptLegacy       int64 `json:"keptLegacy"`
	LegacyUnverified int64 `json:"legacyUnverified"`
	// VerifyFailures: a raw file that failed verification after it was
	// written or cloned, or before its legacy file was to be retired.
	VerifyFailures int64 `json:"verifyFailures"`
	WriteErrors    int64 `json:"writeErrors"`

	OlderBuildWrites int64     `json:"olderBuildWrites"`
	WindowStartedAt  time.Time `json:"windowStartedAt,omitzero"`
	ReclaimAllowed   bool      `json:"reclaimAllowed"`
	Retired          int64     `json:"retired"`
	RetiredBytes     int64     `json:"retiredBytes"`
	// RetireSkipped: legacy files kept because their raw twin could not be
	// made durable this pass (the next pass tries again).
	RetireSkipped int64 `json:"retireSkipped"`

	DeletedChatsEligible int    `json:"deletedChatsEligible"`
	Collected            int64  `json:"collected"`
	CollectedBytes       int64  `json:"collectedBytes"`
	CollectSkipped       string `json:"collectSkipped,omitempty"`

	ReadBytes int64 `json:"readBytes"`
}

type imageStoreMaintenance struct {
	stateDir, imagesDir, thumbsDir, storeDir string
	refs                                     imageReferenceSource
	version                                  string
	logf                                     func(string, ...any)

	now       func() time.Time
	window    time.Duration
	grace     time.Duration
	touchHold time.Duration
	// startDelay and interval pace run.
	startDelay, interval time.Duration
	rate                 int64
	sleep                func(context.Context, time.Duration) error
	// hook, tests only, runs after each durable step; an error stops the pass
	// there, as a crash would.
	hook func(step, digest string) error
	// foreground reports a chat read in progress or just finished; the pass
	// waits for it (bounded) before each image, so opening a chat never
	// shares the machine with it.
	foreground func() bool

	// Reused by every image of a pass: one image is in flight at a time.
	textBuffer, rawBuffer []byte

	status atomic.Pointer[imageStoreReport]
	runMu  sync.Mutex
}

func newImageStoreMaintenance(stateDir, version string, refs imageReferenceSource, logf func(string, ...any)) *imageStoreMaintenance {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &imageStoreMaintenance{
		stateDir:   stateDir,
		imagesDir:  filepath.Join(stateDir, sessionImageDirname),
		thumbsDir:  filepath.Join(stateDir, chatImageThumbDirname),
		storeDir:   filepath.Join(stateDir, imageStoreDirname),
		refs:       refs,
		version:    version,
		logf:       logf,
		now:        time.Now,
		window:     imageStoreRollbackWindow,
		grace:      imageStoreDeletedChatGrace,
		touchHold:  imageStoreTouchHold,
		startDelay: imageStoreStartDelay,
		interval:   imageStoreInterval,
		rate:       imageStoreBytesPerSecond,
		sleep:      sleepContext,
	}
}

// Development and test profiles may shorten the clocks (a scratch copy proves
// the whole pass in one run); production always uses the constants.
const (
	imageStoreWindowEnv     = "WORKASS_IMAGE_STORE_ROLLBACK_WINDOW"
	imageStoreGraceEnv      = "WORKASS_IMAGE_STORE_DELETED_GRACE"
	imageStoreRateEnv       = "WORKASS_IMAGE_STORE_RATE"
	imageStoreStartDelayEnv = "WORKASS_IMAGE_STORE_START_DELAY"
)

// imageStoreMaintenanceFor wires the maintenance to where chat actors live.
func imageStoreMaintenanceFor(stateDir, version string, backend chatActorBackend, runtime *providerChatRuntime, logf func(string, ...any)) *imageStoreMaintenance {
	var refs imageReferenceSource
	if sqlBackend, ok := backend.(sqlActorBackend); ok {
		refs = sqlImageReferences{actors: sqlBackend.actors}
	} else {
		refs = fileImageReferences{stateDir: stateDir, runtime: runtime}
	}
	m := newImageStoreMaintenance(stateDir, version, refs, logf)
	if profile := workassRuntimeProfile(); profile != "dev" && profile != "test" {
		return m
	}
	duration := func(name string, into *time.Duration) {
		if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
			if value, err := time.ParseDuration(raw); err == nil && value >= 0 {
				*into = value
				m.logf("[image-store] %s=%s (development override)", name, value)
			}
		}
	}
	duration(imageStoreWindowEnv, &m.window)
	duration(imageStoreGraceEnv, &m.grace)
	duration(imageStoreStartDelayEnv, &m.startDelay)
	if raw := strings.TrimSpace(os.Getenv(imageStoreRateEnv)); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value >= 0 {
			m.rate = value
			m.logf("[image-store] %s=%d bytes/s (development override; 0 is unthrottled)", imageStoreRateEnv, value)
		}
	}
	return m
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// applyMarker sets whether new images also get a legacy file from the
// recorded window, before any write happens.
func (m *imageStoreMaintenance) applyMarker() {
	marker, _ := m.loadMarker()
	sessionImageLegacyShadow.Store(!marker.reclaimAllowed(m.now(), m.window))
}

// run is the daemon-lifetime loop.
func (m *imageStoreMaintenance) run(ctx context.Context) {
	if err := m.sleep(ctx, m.startDelay); err != nil {
		return
	}
	for {
		report, err := m.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.logf("[image-store] pass stopped: %v", err)
		} else {
			m.logf("[image-store] pass done in %s: %s", report.FinishedAt.Sub(report.StartedAt).Round(time.Second), report.summary())
		}
		if err := m.sleep(ctx, m.interval); err != nil {
			return
		}
	}
}

func (r imageStoreReport) summary() string {
	return fmt.Sprintf("legacy %d, raw %d; cloned %d (%s), written %d (%s), already raw %d, protected %d, kept legacy %d, legacy unverified %d, verify failures %d, retired %d (%s), collected %d (%s)",
		r.LegacyFiles, r.RawFiles, r.Cloned, humanBytes(r.ClonedRawBytes), r.Written, humanBytes(r.WrittenRawBytes), r.AlreadyRaw,
		r.SkippedProtected, r.KeptLegacy, r.LegacyUnverified, r.VerifyFailures, r.Retired, humanBytes(r.RetiredBytes),
		r.Collected, humanBytes(r.CollectedBytes))
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// metrics is the latest report, for GET /workass/metrics.
func (m *imageStoreMaintenance) metrics() map[string]any {
	out := map[string]any{
		"legacyShadow": sessionImageLegacyShadow.Load(),
		"newImages": map[string]any{
			"cloned": sessionImageWriteStats.cloned.Load(), "written": sessionImageWriteStats.written.Load(),
			"legacyOnly": sessionImageWriteStats.legacy.Load(),
		},
	}
	if m == nil {
		return out
	}
	if report := m.status.Load(); report != nil {
		out["pass"] = *report
	}
	return out
}

func (m *imageStoreMaintenance) publish(report *imageStoreReport) {
	snapshot := *report
	m.status.Store(&snapshot)
}

func (m *imageStoreMaintenance) loadMarker() (imageStoreMarker, error) {
	data, err := os.ReadFile(filepath.Join(m.storeDir, imageStoreMarkerFilename))
	if errors.Is(err, os.ErrNotExist) {
		return imageStoreMarker{Schema: imageStoreMarkerSchema}, nil
	}
	if err != nil {
		return imageStoreMarker{Schema: imageStoreMarkerSchema}, err
	}
	var marker imageStoreMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		// An unreadable marker starts the window over: never reclaim on doubt.
		return imageStoreMarker{Schema: imageStoreMarkerSchema}, err
	}
	return marker, nil
}

func (m *imageStoreMaintenance) saveMarker(marker imageStoreMarker) error {
	marker.Schema = imageStoreMarkerSchema
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.storeDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.storeDir, ".marker-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(m.storeDir, imageStoreMarkerFilename)); err != nil {
		return err
	}
	return syncDirectory(m.storeDir)
}

// imageStoreReceipts is the append-only receipt journal. A torn last line
// (a crash mid-append) is ignored when it is read back.
type imageStoreReceipts struct {
	file     *os.File
	writer   *bufio.Writer
	pending  int
	byDigest map[string]imageStoreReceipt
	version  string
	// barrier runs before buffered receipts reach the disk: a receipt never
	// becomes durable before the batched raw writes it vouches for.
	barrier func() error
}

func openImageStoreReceipts(dir, version string) (*imageStoreReceipts, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, imageStoreReceiptsFilename)
	receipts := &imageStoreReceipts{byDigest: make(map[string]imageStoreReceipt), version: version}
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			var receipt imageStoreReceipt
			if strings.TrimSpace(line) == "" || json.Unmarshal([]byte(line), &receipt) != nil || receipt.Digest == "" {
				continue
			}
			switch receipt.Outcome {
			case imageReceiptCloned, imageReceiptWritten, imageReceiptVerified:
				receipts.byDigest[receipt.Digest] = receipt
			case imageReceiptRawVerifyFailed, imageReceiptCollected:
				delete(receipts.byDigest, receipt.Digest)
			}
		}
		if len(data) > 0 && data[len(data)-1] != '\n' {
			// Terminate a torn line so the next record starts clean.
			if file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				_, _ = file.Write([]byte("\n"))
				_ = file.Close()
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	receipts.file = file
	receipts.writer = bufio.NewWriterSize(file, 64<<10)
	return receipts, nil
}

// raw returns the receipt that vouches for name's raw file, when that file
// still has the size and mtime the receipt recorded.
func (r *imageStoreReceipts) raw(name string, info os.FileInfo) bool {
	receipt, ok := r.byDigest[name]
	return ok && info != nil && receipt.RawSize == info.Size() && receipt.RawMTimeNS == info.ModTime().UnixNano()
}

func (r *imageStoreReceipts) record(receipt imageStoreReceipt) error {
	receipt.At = time.Now().UTC().Format(time.RFC3339Nano)
	receipt.BinaryVersion = r.version
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if _, err := r.writer.Write(append(data, '\n')); err != nil {
		return err
	}
	switch receipt.Outcome {
	case imageReceiptCloned, imageReceiptWritten, imageReceiptVerified:
		r.byDigest[receipt.Digest] = receipt
	case imageReceiptRawVerifyFailed, imageReceiptCollected:
		delete(r.byDigest, receipt.Digest)
	}
	if r.pending++; r.pending >= 256 {
		return r.flush()
	}
	return nil
}

func (r *imageStoreReceipts) flush() error {
	r.pending = 0
	if r.barrier != nil {
		if err := r.barrier(); err != nil {
			return err
		}
	}
	if err := r.writer.Flush(); err != nil {
		return err
	}
	return r.file.Sync()
}

func (r *imageStoreReceipts) close() error {
	flushErr := r.flush()
	return errors.Join(flushErr, r.file.Close())
}

// imageStoreThrottle paces the pass's reads to rate bytes per second.
type imageStoreThrottle struct {
	rate  int64
	start time.Time
	bytes int64
	sleep func(context.Context, time.Duration) error
	// busy, when set, is waited out (up to imageStoreYieldLimit) first.
	busy func() bool
}

// imageStoreYieldLimit bounds one wait for the foreground, so a client that
// polls without pause slows the pass but cannot stop it.
const imageStoreYieldLimit = 2 * time.Second

func (t *imageStoreThrottle) add(ctx context.Context, n int64) error {
	t.bytes += n
	if t.busy != nil {
		for waited := time.Duration(0); waited < imageStoreYieldLimit && t.busy(); waited += 50 * time.Millisecond {
			if err := t.sleep(ctx, 50*time.Millisecond); err != nil {
				return err
			}
		}
	}
	if t.rate <= 0 {
		return ctx.Err()
	}
	due := time.Duration(float64(t.bytes) / float64(t.rate) * float64(time.Second))
	if ahead := due - time.Since(t.start); ahead > 0 {
		return t.sleep(ctx, ahead)
	}
	return ctx.Err()
}

type imageStoreEntry struct {
	name   string
	legacy os.FileInfo
	raw    os.FileInfo
}

// listImageStore reads images/ once: every digest with its legacy and raw
// files. Anything else in the directory is left alone.
func (m *imageStoreMaintenance) listImageStore() ([]imageStoreEntry, error) {
	entries, err := os.ReadDir(m.imagesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*imageStoreEntry)
	get := func(name string) *imageStoreEntry {
		entry := byName[name]
		if entry == nil {
			entry = &imageStoreEntry{name: name}
			byName[name] = entry
		}
		return entry
	}
	for _, dirEntry := range entries {
		name := dirEntry.Name()
		raw := strings.HasSuffix(name, sessionImageRawSuffix)
		digest := strings.TrimSuffix(name, sessionImageRawSuffix)
		if _, ok := validSessionImageRef(sessionImageDirname + "/" + digest); !ok {
			continue
		}
		info, err := dirEntry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if raw {
			get(digest).raw = info
		} else {
			get(digest).legacy = info
		}
	}
	out := make([]imageStoreEntry, 0, len(byName))
	for _, entry := range byName {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// cleanupTemps removes temporary files no live write can own any more.
func (m *imageStoreMaintenance) cleanupTemps() {
	entries, err := os.ReadDir(m.imagesDir)
	if err != nil {
		return
	}
	cutoff := m.now().Add(-imageStoreTempMaxAge)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".image-") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(m.imagesDir, name))
		}
	}
}

// runOnce is one complete pass. It never runs twice at once.
func (m *imageStoreMaintenance) runOnce(ctx context.Context) (report imageStoreReport, err error) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	report = imageStoreReport{StartedAt: m.now(), Phase: "migrate"}
	defer func() {
		report.FinishedAt = m.now()
		if err != nil {
			report.Error = err.Error()
		}
		report.Phase = "done"
		m.publish(&report)
	}()
	marker, markerErr := m.loadMarker()
	if markerErr != nil {
		m.logf("[image-store] marker unreadable, the rollback window starts over: %v", markerErr)
	}
	m.cleanupTemps()
	entries, err := m.listImageStore()
	if err != nil {
		return report, err
	}
	throttle := &imageStoreThrottle{rate: m.rate, start: time.Now(), sleep: m.sleep, busy: m.foreground}
	for _, entry := range entries {
		if entry.legacy != nil {
			report.LegacyFiles++
		}
		if entry.raw != nil {
			report.RawFiles++
		}
	}
	// An older build writes legacy files only. One with no raw twin, newer
	// than the window's start, means an older build ran after the window
	// began: the window starts over and nothing is reclaimed this pass.
	if !marker.WindowStartedAt.IsZero() {
		for _, entry := range entries {
			if entry.legacy != nil && entry.raw == nil && entry.legacy.ModTime().After(marker.WindowStartedAt) &&
				m.canonicalLegacyImage(entry.name) {
				report.OlderBuildWrites++
			}
		}
		if report.OlderBuildWrites > 0 {
			m.logf("[image-store] %d images written by an older Workass since %s; the rollback window starts over",
				report.OlderBuildWrites, marker.WindowStartedAt.Format(time.RFC3339))
			marker.WindowStartedAt = time.Time{}
			marker.WindowResets++
		}
	}
	m.publish(&report)

	var receipts *imageStoreReceipts
	defer func() {
		if receipts != nil {
			err = errors.Join(err, receipts.close())
		}
	}()
	openReceipts := func() error {
		if receipts != nil {
			return nil
		}
		var openErr error
		receipts, openErr = openImageStoreReceipts(m.storeDir, m.version)
		if receipts != nil {
			receipts.barrier = func() error {
				if err := sessionImageBarrier(m.imagesDir); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				return nil
			}
		}
		return openErr
	}
	var hints imageCloneHints
	if !sessionImageClonesPossible {
		// No clone can come of a hint here: never read the chats for them.
		hints = imageCloneHints{}
	}
	hintsFor := func(name string) []string {
		if hints == nil {
			started := time.Now()
			collected, hintErr := collectImageCloneHints(ctx, m.refs, func() error { return m.sleep(ctx, time.Millisecond) })
			if hintErr != nil && ctx.Err() == nil {
				m.logf("[image-store] clone hints incomplete: %v", hintErr)
			}
			hints = collected
			if hints == nil {
				hints = imageCloneHints{}
			}
			report.HintedImages = len(hints)
			m.logf("[image-store] %d images have clone candidates (read in %s)", len(hints), time.Since(started).Round(time.Millisecond))
		}
		return hints[name]
	}

	// 1. Migration.
	verified := make(map[string]bool, len(entries))
	for index, entry := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if entry.legacy == nil {
			continue
		}
		if err := openReceipts(); err != nil {
			return report, err
		}
		ok, migrateErr := m.migrateOne(ctx, entry, receipts, hintsFor, throttle, &report)
		if migrateErr != nil {
			return report, migrateErr
		}
		if ok {
			verified[entry.name] = true
		}
		report.Processed = index + 1
		if report.Processed%2000 == 0 {
			m.publish(&report)
			m.logf("[image-store] %d/%d: cloned %d, written %d, already raw %d", report.Processed, len(entries), report.Cloned, report.Written, report.AlreadyRaw)
		}
	}
	if receipts != nil {
		if err := receipts.flush(); err != nil {
			return report, err
		}
	}
	// Complete: every legacy image has a verified raw twin, except those that
	// can never have one (kept legacy, legacy unverified), which keep their
	// legacy file forever anyway.
	complete := report.WriteErrors == 0
	if complete && marker.WindowStartedAt.IsZero() {
		marker.WindowStartedAt = m.now()
		marker.WindowStartedBy = m.version
	}
	report.WindowStartedAt = marker.WindowStartedAt
	report.ReclaimAllowed = complete && report.OlderBuildWrites == 0 && marker.reclaimAllowed(m.now(), m.window)
	marker.LastPassAt, marker.LastPassBy = m.now(), m.version
	if err := m.saveMarker(marker); err != nil {
		return report, err
	}
	sessionImageLegacyShadow.Store(!report.ReclaimAllowed)

	// 2. Reclaim.
	if report.ReclaimAllowed {
		report.Phase = "reclaim"
		m.publish(&report)
		if err := openReceipts(); err != nil {
			return report, err
		}
		// Each raw twin is verified and handed to the device; one barrier per
		// batch makes the batch durable; only then do its legacy files go.
		var batch []retireCandidate
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if !verified[entry.name] {
				continue
			}
			candidate, ok, err := m.prepareRetire(ctx, entry, receipts, throttle, &report)
			if err != nil {
				return report, err
			}
			if ok {
				batch = append(batch, candidate)
			}
			if len(batch) >= imageStoreRetireBatch {
				if err := m.retireBatch(batch, receipts, &report); err != nil {
					return report, err
				}
				batch = batch[:0]
			}
		}
		if err := m.retireBatch(batch, receipts, &report); err != nil {
			return report, err
		}
	}

	// 3. Collection of deleted chats' images.
	report.Phase = "collect"
	m.publish(&report)
	lazyReceipts := func() (*imageStoreReceipts, error) {
		err := openReceipts()
		return receipts, err
	}
	if err := m.collectDeletedChatImages(ctx, lazyReceipts, throttle, &report); err != nil {
		if ctx.Err() != nil {
			return report, err
		}
		report.CollectSkipped = err.Error()
		m.logf("[image-store] deleted-chat images kept: %v", err)
	}
	report.ReadBytes = throttle.bytes
	report.FinishedAt = m.now()
	marker.LastReport = &report
	if err := m.saveMarker(marker); err != nil {
		return report, err
	}
	return report, nil
}

// readLegacy reads a legacy file into the pass's reusable buffer. The result
// is valid until the next call.
func (m *imageStoreMaintenance) readLegacy(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := int(info.Size())
	if cap(m.textBuffer) < size {
		m.textBuffer = make([]byte, size)
	}
	buffer := m.textBuffer[:size]
	if _, err := io.ReadFull(file, buffer); err != nil {
		return nil, err
	}
	return buffer, nil
}

// canonicalLegacyImage reports that a legacy file holds verified canonical
// base64, the only legacy-only image this build never writes (a data URL it
// keeps in the legacy form is its own, not an older build's).
func (m *imageStoreMaintenance) canonicalLegacyImage(name string) bool {
	text, err := os.ReadFile(sessionImageLegacyPath(m.stateDir, name))
	if err != nil || sessionImageTextDigest(text) != name {
		return false
	}
	_, canonical := decodeCanonicalSessionImage(string(text))
	return canonical
}

func (m *imageStoreMaintenance) step(name, digest string) error {
	if m.hook == nil {
		return nil
	}
	return m.hook(name, digest)
}

// migrateOne makes sure a legacy image has a verified raw twin. ok means the
// raw twin is verified (now or by a receipt that still matches it).
func (m *imageStoreMaintenance) migrateOne(ctx context.Context, entry imageStoreEntry, receipts *imageStoreReceipts,
	hintsFor func(string) []string, throttle *imageStoreThrottle, report *imageStoreReport) (bool, error) {
	name := entry.name
	rawPath := sessionImageRawPath(m.stateDir, name)
	if entry.raw != nil {
		if receipts.raw(name, entry.raw) {
			report.AlreadyRaw++
			return true, nil
		}
		if err := throttle.add(ctx, entry.raw.Size()); err != nil {
			return false, err
		}
		if verifyRawSessionImageFile(rawPath, name, entry.raw.Size()) == nil {
			report.AlreadyRaw++
			return true, receipts.record(imageStoreReceipt{Digest: name, Outcome: imageReceiptVerified,
				RawSize: entry.raw.Size(), RawMTimeNS: entry.raw.ModTime().UnixNano(), LegacySize: entry.legacy.Size()})
		}
		// A damaged raw twin is rebuilt from the legacy file below.
		report.RawRepaired++
	}
	if entry.legacy.Size() > maxPersistedSessionImageBytes {
		report.LegacyUnverified++
		return false, receipts.record(imageStoreReceipt{Digest: name, Outcome: imageReceiptLegacyUnverified, Detail: "too large", LegacySize: entry.legacy.Size()})
	}
	legacyPath := sessionImageLegacyPath(m.stateDir, name)
	text, err := m.readLegacy(legacyPath)
	if errors.Is(err, os.ErrNotExist) {
		// Removed since the listing (collected, or retired by a writer race).
		return false, nil
	}
	if err != nil {
		report.WriteErrors++
		m.logf("[image-store] read %s: %v", name, err)
		return false, nil
	}
	if err := throttle.add(ctx, int64(len(text))); err != nil {
		return false, err
	}
	if sessionImageTextDigest(text) != name {
		// The legacy file does not hold its own content: leave it exactly as
		// it is, never derive anything from it.
		report.LegacyUnverified++
		return false, receipts.record(imageStoreReceipt{Digest: name, Outcome: imageReceiptLegacyUnverified, Detail: "digest mismatch", LegacySize: int64(len(text))})
	}
	raw, canonical := decodeCanonicalSessionImageInto(m.rawBuffer, text)
	if cap(raw) > cap(m.rawBuffer) {
		m.rawBuffer = raw[:0]
	}
	if !canonical {
		report.KeptLegacy++
		return false, receipts.record(imageStoreReceipt{Digest: name, Outcome: imageReceiptKeptLegacy, Detail: "not canonical base64", LegacySize: int64(len(text))})
	}
	sources := hintsFor(name)
	if entry.raw != nil {
		// Rebuild: the damaged twin goes first so it is never served.
		_ = os.Remove(rawPath)
	}
	sessionImageStoreMu.RLock()
	// Batched: the legacy file stays authoritative until the receipts'
	// barrier (and a reclaim's own) has made this raw file durable.
	result, err := persistRawSessionImage(m.imagesDir, name, raw, sources, sessionImageBatched)
	sessionImageStoreMu.RUnlock()
	if err != nil {
		if errors.Is(err, errRawSessionImageVerify) {
			report.VerifyFailures++
		}
		report.WriteErrors++
		m.logf("[image-store] store %s raw: %v", name, err)
		return false, nil
	}
	info, err := os.Lstat(rawPath)
	if err != nil {
		report.WriteErrors++
		return false, nil
	}
	// A write is read back once to verify it; a clone also had its source
	// read before.
	readBack := int64(len(raw))
	if result.method == "cloned" {
		readBack *= 2
	}
	if err := throttle.add(ctx, readBack); err != nil {
		return false, err
	}
	outcome := imageReceiptWritten
	switch result.method {
	case "cloned":
		report.Cloned++
		report.ClonedRawBytes += int64(len(raw))
		outcome = imageReceiptCloned
	case "existing":
		report.AlreadyRaw++
		outcome = imageReceiptVerified
	default:
		report.Written++
		report.WrittenRawBytes += int64(len(raw))
		switch {
		case len(sources) == 0:
			report.NoCandidate++
		case result.tried(cloneProtected):
			report.SkippedProtected++
		case result.tried(cloneDifferent):
			report.CandidateDifferent++
		case result.tried(cloneOtherVolume):
			report.CandidateOtherVolume++
		case result.tried(cloneFailed):
			report.CloneFailed++
		case result.tried(cloneUnsupported):
			report.CloneUnsupported++
		default:
			report.CandidateMissing++
		}
	}
	if err := receipts.record(imageStoreReceipt{Digest: name, Outcome: outcome, Source: result.source,
		RawSize: info.Size(), RawMTimeNS: info.ModTime().UnixNano(), LegacySize: int64(len(text))}); err != nil {
		return false, err
	}
	return true, m.step("raw", name)
}

// imageStoreRetireBatch is how many legacy files one durability barrier
// covers during reclaim.
const imageStoreRetireBatch = 256

// sessionImageRetireSync hands a verified raw twin's data to the device
// before its legacy file goes; tests replace it.
var sessionImageRetireSync = syncSessionImageDataToDevice

type retireCandidate struct {
	name string
	raw  os.FileInfo
}

// prepareRetire verifies a raw twin right now and hands its data to the
// device; ok means its legacy file may go after the batch's barrier.
func (m *imageStoreMaintenance) prepareRetire(ctx context.Context, entry imageStoreEntry, receipts *imageStoreReceipts, throttle *imageStoreThrottle, report *imageStoreReport) (retireCandidate, bool, error) {
	name := entry.name
	rawPath := sessionImageRawPath(m.stateDir, name)
	info, err := os.Lstat(rawPath)
	if err != nil || !info.Mode().IsRegular() {
		return retireCandidate{}, false, nil
	}
	if err := throttle.add(ctx, info.Size()); err != nil {
		return retireCandidate{}, false, err
	}
	if err := verifyRawSessionImageFile(rawPath, name, info.Size()); err != nil {
		report.VerifyFailures++
		m.logf("[image-store] %s raw failed verification; its legacy file stays: %v", name, err)
		return retireCandidate{}, false, receipts.record(imageStoreReceipt{Digest: name, Outcome: imageReceiptRawVerifyFailed, Detail: err.Error()})
	}
	file, err := os.Open(rawPath)
	if err != nil {
		return retireCandidate{}, false, nil
	}
	syncErr := sessionImageRetireSync(file)
	_ = file.Close()
	if syncErr != nil {
		// Not provably durable: this legacy file stays, the rest of the
		// reclaim (and the collection after it) goes on.
		report.RetireSkipped++
		m.logf("[image-store] %s raw could not be synced; its legacy file stays: %v", name, syncErr)
		return retireCandidate{}, false, nil
	}
	return retireCandidate{name: name, raw: info}, true, nil
}

// retireBatch makes a batch of verified raw twins durable with one barrier,
// then removes their legacy files.
func (m *imageStoreMaintenance) retireBatch(batch []retireCandidate, receipts *imageStoreReceipts, report *imageStoreReport) error {
	if len(batch) == 0 {
		return nil
	}
	if err := sessionImageBarrier(m.imagesDir); err != nil {
		return err
	}
	for _, candidate := range batch {
		legacyPath := sessionImageLegacyPath(m.stateDir, candidate.name)
		legacy, err := os.Lstat(legacyPath)
		if err != nil {
			continue
		}
		if err := os.Remove(legacyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		verifiedSessionImages.Delete(legacyPath)
		report.Retired++
		report.RetiredBytes += legacy.Size()
		if err := receipts.record(imageStoreReceipt{Digest: candidate.name, Outcome: imageReceiptRetired, LegacySize: legacy.Size(),
			RawSize: candidate.raw.Size(), RawMTimeNS: candidate.raw.ModTime().UnixNano()}); err != nil {
			return err
		}
		if err := m.step("retire", candidate.name); err != nil {
			return err
		}
	}
	return syncDirectory(m.imagesDir)
}
