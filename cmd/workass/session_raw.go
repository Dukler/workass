package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"workass/internal/acp"
)

const sessionWireCapacityMargin = 1 << 20

type sessionWireImageSource struct {
	ref     string
	name    string
	path    string
	file    *os.File
	rawLen  int
	quoted  []byte
	valid   bool
	warning error
}

func (s *sessionWireImageSource) encodedLen() int {
	if s == nil || !s.valid {
		return 0
	}
	if s.quoted != nil {
		return len(s.quoted)
	}
	return s.rawLen + 2
}

type sessionWireRefSummary struct {
	counts      map[string]int
	messageRefs int
}

type sessionWireHole struct {
	start  int
	end    int
	source *sessionWireImageSource
}

type sessionWireEncodeContext struct {
	liveByTab     map[string]acp.LiveSession
	root          bool
	chatSlice     bool
	chat          bool
	messageSlice  bool
	message       bool
	eventTree     bool
	messageImages []any
}

type sessionWireEncoder struct {
	sources  map[string]*sessionWireImageSource
	holes    []sessionWireHole
	warnings []error
}

// GetRawWithLiveSessions serializes the authoritative mirror directly into its
// final wire-result bytes. The ref-native tree is encoded once while stable;
// bounded image sidecars are then filled into reserved spans after releasing
// the session mutex. No decoded duplicate mirror is created.
func (s *sessionStore) GetRawWithLiveSessions(manager *acp.Manager) []byte {
	if s == nil || !s.enabled() {
		return nil
	}
	generation := s.publishedGeneration()
	if generation == nil || generation.root == nil {
		return nil
	}
	liveByTab := make(map[string]acp.LiveSession)
	if manager != nil {
		for _, binding := range manager.LiveSessions() {
			if binding.TabID != "" {
				liveByTab[binding.TabID] = binding
			}
		}
	}

	pendingArchives := pendingGetArchives(generation)
	waitSessionGetArchives(pendingArchives)
	if s.beforeGetRehydrate != nil {
		s.beforeGetRehydrate()
	}

	stateDir := filepath.Dir(s.path)
	summary := collectSessionWireRefs(generation.root)
	sources := make(map[string]*sessionWireImageSource, len(summary.counts))
	defer closeSessionWireSources(sources)
	prepareSessionWireSources(sources, summary.counts, stateDir)

	capacity := s.sessionWireCapacity(summary, sources)
	encoder := &sessionWireEncoder{sources: sources}
	raw := make([]byte, 0, capacity)
	var err error
	raw, err = encoder.appendValue(raw, generation.root, sessionWireEncodeContext{
		liveByTab: liveByTab,
		root:      true,
	})
	s.getLock.observe(0, 0)
	if err != nil {
		s.recordLoadError(err)
		return nil
	}

	var warnings []error
	for ref := range summary.counts {
		if source := sources[ref]; source != nil && source.warning != nil {
			warnings = append(warnings, source.warning)
		}
	}
	warnings = append(warnings, encoder.warnings...)
	if warning := errors.Join(warnings...); warning != nil {
		s.recordLoadError(warning)
	}
	if err := fillSessionWireHoles(raw, encoder.holes); err != nil {
		// A sidecar changed after preflight. Preserve the old degradation
		// semantics on this rare external-race path rather than returning a
		// malformed or partially filled JSON result.
		s.recordLoadError(err)
		legacy := s.GetWithLiveSessions(manager)
		fallback, marshalErr := json.Marshal(legacy)
		if marshalErr != nil {
			s.recordLoadError(marshalErr)
			return nil
		}
		out := make([]byte, len(fallback), len(fallback)+sessionWireCapacityMargin)
		copy(out, fallback)
		return out
	}
	s.wireByteEstimate.Store(int64(len(raw)))
	return raw
}

func pendingGetArchives(generation *sessionGeneration) []chan struct{} {
	if generation == nil || generation.root == nil {
		return nil
	}
	activeTabID := fieldString(generation.root, "activeId")
	pending := make([]chan struct{}, 0, len(generation.pendingArchives))
	for _, fence := range generation.pendingArchives {
		tabID, done := fence.tabID, fence.done
		if tabID == "" || (activeTabID != "" && tabID == activeTabID) {
			pending = append(pending, done)
		}
	}
	return pending
}

func waitSessionGetArchives(pending []chan struct{}) {
	if len(pending) == 0 {
		return
	}
	timer := time.NewTimer(sessionGetArchiveFenceTimeout)
deferStop:
	for _, done := range pending {
		select {
		case <-done:
		case <-timer.C:
			break deferStop
		}
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func collectSessionWireRefs(snapshot map[string]any) sessionWireRefSummary {
	summary := sessionWireRefSummary{counts: make(map[string]int)}
	collectExternalSessionWireRefs(snapshot, summary.counts)
	for _, rawChat := range anySlice(snapshot["chats"]) {
		for _, rawMessage := range messageSlice(mapFromAnyMain(rawChat)) {
			message := mapFromAnyMain(rawMessage)
			messageImages := anySlice(message["images"])
			for _, rawEvent := range anySlice(message["events"]) {
				for _, rawImage := range anySlice(mapFromAnyMain(rawEvent)["images"]) {
					index, present := intFieldPresent(mapFromAnyMain(rawImage), sessionMessageImageRefField)
					if !present || index < 0 || index >= len(messageImages) {
						continue
					}
					summary.messageRefs++
					collectExternalSessionWireRefs(messageImages[index], summary.counts)
				}
			}
		}
	}
	return summary
}

func collectExternalSessionWireRefs(value any, counts map[string]int) {
	switch item := value.(type) {
	case map[string]any:
		if ref := fieldString(item, sessionImageDataRefField); ref != "" {
			counts[ref]++
		}
		for _, child := range item {
			collectExternalSessionWireRefs(child, counts)
		}
	case []any:
		for _, child := range item {
			collectExternalSessionWireRefs(child, counts)
		}
	}
}

func missingSessionWireRefs(counts map[string]int, sources map[string]*sessionWireImageSource) map[string]int {
	missing := make(map[string]int)
	for ref, count := range counts {
		if _, exists := sources[ref]; !exists {
			missing[ref] = count
		}
	}
	return missing
}

func prepareSessionWireSources(sources map[string]*sessionWireImageSource, refs map[string]int, stateDir string) {
	for ref := range refs {
		if _, exists := sources[ref]; exists {
			continue
		}
		sources[ref] = prepareSessionWireSource(ref, stateDir)
	}
}

func prepareSessionWireSource(ref, stateDir string) *sessionWireImageSource {
	source := &sessionWireImageSource{ref: ref}
	name, path, info, err := externalSessionImageInfo(ref, stateDir)
	if err != nil {
		source.warning = err
		return source
	}
	file, err := os.Open(path)
	if err != nil {
		source.warning = fmt.Errorf("read session image ref %s: %w", ref, err)
		return source
	}
	source.name, source.path, source.file, source.rawLen = name, path, file, int(info.Size())

	hash := sha256.New()
	safe := true
	var buffer [32 * 1024]byte
	for {
		n, readErr := file.Read(buffer[:])
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
			if safe && !safeJSONStringBytes(buffer[:n]) {
				safe = false
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			source.warning = fmt.Errorf("read session image ref %s: %w", ref, readErr)
			_ = file.Close()
			source.file = nil
			return source
		}
	}
	var encodedHash [64]byte
	hex.Encode(encodedHash[:], hash.Sum(nil))
	if string(encodedHash[:]) != name {
		source.warning = fmt.Errorf("session image ref %s failed content verification", ref)
		_ = file.Close()
		source.file = nil
		return source
	}
	if !safe {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			source.warning = fmt.Errorf("read session image ref %s: %w", ref, err)
			_ = file.Close()
			source.file = nil
			return source
		}
		data, err := io.ReadAll(io.LimitReader(file, maxPersistedSessionImageBytes+1))
		if err != nil || len(data) > maxPersistedSessionImageBytes {
			if err == nil {
				err = errors.New("image payload exceeds wire limit")
			}
			source.warning = fmt.Errorf("read session image ref %s: %w", ref, err)
			_ = file.Close()
			source.file = nil
			return source
		}
		source.quoted = appendJSONString(nil, string(data))
	}
	source.valid = true
	return source
}

func safeJSONStringBytes(data []byte) bool {
	for _, value := range data {
		if value < 0x20 || value >= utf8.RuneSelf || value == '"' || value == '\\' || value == '<' || value == '>' || value == '&' {
			return false
		}
	}
	return true
}

func closeSessionWireSources(sources map[string]*sessionWireImageSource) {
	for _, source := range sources {
		if source != nil && source.file != nil {
			_ = source.file.Close()
			source.file = nil
		}
	}
}

// sessionWireCapacity sizes the wire buffer so the encode is a single
// allocation. That is the whole point of this path — a session with megabytes
// of image sidecars must not be copied around to be read — so the estimate has
// to cover the result rather than usually cover it.
//
// The image sidecars are the only part that can jump by megabytes between two
// reads, and their exact encoded size is already known by the time we get here:
// prepareSessionWireSources has stat'ed every one. Summing them costs a walk of
// a map that was just built. The previous wire length is kept as a floor for the
// tree's JSON expansion, but it cannot stand in for the images: a chat that
// gains a screenshot outgrows "last read plus a fixed margin" in one step, and
// the buffer then has to be grown mid-encode, which is the copy this design
// exists to avoid.
func (s *sessionStore) sessionWireCapacity(summary sessionWireRefSummary, sources map[string]*sessionWireImageSource) int {
	estimate := s.snapshotByteEstimate.Load()
	if estimate < 64<<10 {
		estimate = 64 << 10
	}
	for ref, count := range summary.counts {
		source := sources[ref]
		if source == nil || !source.valid {
			continue
		}
		estimate += int64(count) * int64(source.encodedLen()+256)
	}
	estimate += int64(summary.messageRefs * 512)
	if last := s.wireByteEstimate.Load(); last > estimate {
		estimate = last
	}
	return boundedSessionWireCapacity(estimate + sessionWireCapacityMargin)
}

func boundedSessionWireCapacity(value int64) int {
	maxInt := int64(^uint(0) >> 1)
	if value <= 0 {
		return sessionWireCapacityMargin
	}
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}

func fillSessionWireHoles(raw []byte, holes []sessionWireHole) error {
	for _, hole := range holes {
		if hole.source == nil || hole.source.file == nil || hole.start < 0 || hole.end < hole.start || hole.end > len(raw) {
			return errors.New("invalid session image wire span")
		}
		target := raw[hole.start:hole.end]
		n, err := hole.source.file.ReadAt(target, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read session image ref %s: %w", hole.source.ref, err)
		}
		if n != len(target) {
			return fmt.Errorf("read session image ref %s: short read", hole.source.ref)
		}
		sum := sha256.Sum256(target)
		var encodedHash [64]byte
		hex.Encode(encodedHash[:], sum[:])
		if string(encodedHash[:]) != hole.source.name {
			return fmt.Errorf("session image ref %s changed during serialization", hole.source.ref)
		}
	}
	return nil
}

func (e *sessionWireEncoder) appendValue(dst []byte, value any, context sessionWireEncodeContext) ([]byte, error) {
	switch item := value.(type) {
	case nil:
		return append(dst, "null"...), nil
	case map[string]any:
		return e.appendMap(dst, item, context)
	case []any:
		return e.appendSlice(dst, item, context)
	case string:
		return appendJSONString(dst, item), nil
	case json.Number:
		return append(dst, item.String()...), nil
	case bool:
		return strconv.AppendBool(dst, item), nil
	case int:
		return strconv.AppendInt(dst, int64(item), 10), nil
	case int8:
		return strconv.AppendInt(dst, int64(item), 10), nil
	case int16:
		return strconv.AppendInt(dst, int64(item), 10), nil
	case int32:
		return strconv.AppendInt(dst, int64(item), 10), nil
	case int64:
		return strconv.AppendInt(dst, item, 10), nil
	case uint:
		return strconv.AppendUint(dst, uint64(item), 10), nil
	case uint8:
		return strconv.AppendUint(dst, uint64(item), 10), nil
	case uint16:
		return strconv.AppendUint(dst, uint64(item), 10), nil
	case uint32:
		return strconv.AppendUint(dst, uint64(item), 10), nil
	case uint64:
		return strconv.AppendUint(dst, item, 10), nil
	case float32:
		return appendJSONFloat(dst, float64(item), 32)
	case float64:
		return appendJSONFloat(dst, item, 64)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return dst, err
		}
		return append(dst, encoded...), nil
	}
}

func (e *sessionWireEncoder) appendSlice(dst []byte, values []any, context sessionWireEncodeContext) ([]byte, error) {
	dst = append(dst, '[')
	for index, value := range values {
		if index > 0 {
			dst = append(dst, ',')
		}
		child := sessionWireEncodeContext{
			liveByTab:     context.liveByTab,
			eventTree:     context.eventTree,
			messageImages: context.messageImages,
		}
		if context.chatSlice {
			child.chat = true
			child.eventTree = false
			child.messageImages = nil
		}
		if context.messageSlice {
			child.message = true
			child.eventTree = false
			child.messageImages = nil
		}
		var err error
		dst, err = e.appendValue(dst, value, child)
		if err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

func (e *sessionWireEncoder) appendMap(dst []byte, item map[string]any, context sessionWireEncodeContext) ([]byte, error) {
	omitMessageRef := false
	if context.eventTree {
		if index, present := intFieldPresent(item, sessionMessageImageRefField); present {
			if index >= 0 && index < len(context.messageImages) {
				child := sessionWireEncodeContext{liveByTab: context.liveByTab}
				return e.appendValue(dst, context.messageImages[index], child)
			}
			omitMessageRef = true
			e.warnings = append(e.warnings, fmt.Errorf("session event image ref %d is outside its owning message", index))
		}
	}

	ref := fieldString(item, sessionImageDataRefField)
	source := e.sources[ref]
	hasExternalRef := ref != ""
	includeData := hasExternalRef && source != nil && source.valid && !omitMessageRef

	var liveInfo any
	includeLive := false
	if context.chat {
		if binding, exists := context.liveByTab[fieldString(item, "id")]; exists {
			chatID := fieldString(item, "chatId")
			if binding.ChatID != "" && chatID != "" && binding.ChatID == chatID {
				liveInfo = binding.Info
				includeLive = true
			}
		}
	}

	keyCount := len(item) + 2
	var localKeys [48]string
	keys := localKeys[:0]
	if keyCount > len(localKeys) {
		keys = make([]string, 0, keyCount)
	}
	for key := range item {
		if hasExternalRef && (key == sessionImageDataRefField || key == "data") {
			continue
		}
		if omitMessageRef && (key == sessionMessageImageRefField || key == "data") {
			continue
		}
		if includeLive && key == "liveSession" {
			continue
		}
		keys = append(keys, key)
	}
	if includeData {
		keys = append(keys, "data")
	}
	if includeLive {
		keys = append(keys, "liveSession")
	}
	sort.Strings(keys)

	dst = append(dst, '{')
	for index, key := range keys {
		if index > 0 {
			dst = append(dst, ',')
		}
		dst = appendJSONString(dst, key)
		dst = append(dst, ':')
		switch {
		case includeData && key == "data":
			dst = e.appendImageSource(dst, source)
		case includeLive && key == "liveSession":
			var err error
			dst, err = e.appendValue(dst, liveInfo, sessionWireEncodeContext{liveByTab: context.liveByTab})
			if err != nil {
				return dst, err
			}
		default:
			child := sessionWireEncodeContext{
				liveByTab:     context.liveByTab,
				eventTree:     context.eventTree,
				messageImages: context.messageImages,
			}
			if context.root && key == "chats" {
				child = sessionWireEncodeContext{liveByTab: context.liveByTab, chatSlice: true}
			}
			if context.chat && key == "messages" {
				child = sessionWireEncodeContext{liveByTab: context.liveByTab, messageSlice: true}
			}
			if context.message && key == "events" {
				child = sessionWireEncodeContext{
					liveByTab: context.liveByTab, eventTree: true, messageImages: anySlice(item["images"]),
				}
			}
			var err error
			dst, err = e.appendValue(dst, item[key], child)
			if err != nil {
				return dst, err
			}
		}
	}
	return append(dst, '}'), nil
}

func (e *sessionWireEncoder) appendImageSource(dst []byte, source *sessionWireImageSource) []byte {
	if source.quoted != nil {
		return append(dst, source.quoted...)
	}
	dst = append(dst, '"')
	start := len(dst)
	// sessionWireCapacity is a hint, not a bound: its fast path sizes this buffer
	// from the previous read plus a fixed margin, and a session can grow past that
	// between two reads. Reslicing beyond cap panics, and a panic here takes the
	// whole daemon down with every chat's engine, so grow before reserving.
	dst = slices.Grow(dst, source.rawLen)
	dst = dst[:start+source.rawLen]
	end := len(dst)
	dst = append(dst, '"')
	e.holes = append(e.holes, sessionWireHole{start: start, end: end, source: source})
	return dst
}

func appendJSONFloat(dst []byte, value float64, bits int) ([]byte, error) {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return dst, fmt.Errorf("json: unsupported value: %s", strconv.FormatFloat(value, 'g', -1, bits))
	}
	abs := math.Abs(value)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, value, format, -1, bits)
	if format == 'e' {
		n := len(dst)
		if n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst, nil
}

func appendJSONString(dst []byte, value string) []byte {
	const hexChars = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for index := 0; index < len(value); {
		if value[index] < utf8.RuneSelf {
			char := value[index]
			if char >= 0x20 && char != '\\' && char != '"' && char != '<' && char != '>' && char != '&' {
				index++
				continue
			}
			dst = append(dst, value[start:index]...)
			switch char {
			case '\\', '"':
				dst = append(dst, '\\', char)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexChars[char>>4], hexChars[char&0x0f])
			}
			index++
			start = index
			continue
		}
		runeValue, size := utf8.DecodeRuneInString(value[index:])
		if runeValue == utf8.RuneError && size == 1 {
			dst = append(dst, value[start:index]...)
			dst = append(dst, `\ufffd`...)
			index++
			start = index
			continue
		}
		if runeValue == '\u2028' || runeValue == '\u2029' {
			dst = append(dst, value[start:index]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexChars[runeValue&0x0f])
			index += size
			start = index
			continue
		}
		index += size
	}
	dst = append(dst, value[start:]...)
	dst = append(dst, '"')
	return dst
}
