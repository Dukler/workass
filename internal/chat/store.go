package chat

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"workass/internal/durablefs"
	"workass/internal/provider"
)

type StateStore interface {
	Load(chatID string) (State, bool, error)
	Save(State) error
}

type FileStore struct{ Path string }

const currentStateEnvelopeVersion = 22

const (
	providerEventJournalVersion         = 1
	providerEventJournalCheckpointBytes = 1 << 20
	providerEventJournalMaxRecordBytes  = 64 << 20
)

const providerEventJournalMagic = "WORKASS_PROVIDER_EVENTS\x00\x01"

// providerEventStateStore is the narrow durable fast path used by Engine for
// high-frequency, effect-free provider observations. The journal remains part
// of the same logical actor: Load folds it over the canonical snapshot before
// returning any state, and every ordinary actor save checkpoints it.
type providerEventStateStore interface {
	StateStore
	commitProviderEvent(baseRevision uint64, next State, command ProviderEventReceived) error
}

type providerEventJournalRecord struct {
	Version      int                   `json:"v"`
	BaseRevision uint64                `json:"baseRevision"`
	Revision     uint64                `json:"revision"`
	Command      ProviderEventReceived `json:"command"`
}

type stateEnvelope struct {
	Version int   `json:"v"`
	State   State `json:"state"`
}

// DiscoverFileStates enumerates durable chat actors without consulting the
// renderer mirror. The directory is actor storage, so any unreadable or
// duplicate state fails closed instead of silently falling back to UI mirror
// history. Callers may then open exact actors by ChatID through FileStore.
func DiscoverFileStates(dir string) ([]State, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("chat state directory is empty")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	states := make([]State, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read chat actor %q: %w", entry.Name(), err)
		}
		var header struct {
			State struct {
				ChatID string `json:"ChatID"`
			} `json:"state"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, fmt.Errorf("decode chat actor %q: %w", entry.Name(), err)
		}
		chatID := strings.TrimSpace(header.State.ChatID)
		if chatID == "" {
			return nil, fmt.Errorf("chat actor %q has no immutable chat id", entry.Name())
		}
		if _, duplicate := seen[chatID]; duplicate {
			return nil, fmt.Errorf("duplicate durable chat actor %q", chatID)
		}
		state, ok, err := (FileStore{Path: path}).Load(chatID)
		if err != nil {
			return nil, fmt.Errorf("load chat actor %q: %w", chatID, err)
		}
		if !ok {
			return nil, fmt.Errorf("chat actor %q disappeared during discovery", chatID)
		}
		seen[chatID] = struct{}{}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ChatID < states[j].ChatID })
	return states, nil
}

func (s FileStore) Load(chatID string) (State, bool, error) {
	path := strings.TrimSpace(s.Path)
	if path == "" {
		return State{}, false, errors.New("chat state store path is empty")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	var envelope stateEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return State{}, false, fmt.Errorf("decode chat state: %w", err)
	}
	if envelope.Version != currentStateEnvelopeVersion {
		return State{}, false, fmt.Errorf("unsupported chat state version %d", envelope.Version)
	}
	if strings.TrimSpace(envelope.State.ChatID) != strings.TrimSpace(chatID) {
		return State{}, false, errors.New("chat state file belongs to another chat")
	}
	normalizeStateMaps(&envelope.State)
	if err := envelope.State.Validate(); err != nil {
		return State{}, false, fmt.Errorf("validate chat state: %w", err)
	}
	state, err := replayProviderEventJournal(path, envelope.State)
	if err != nil {
		return State{}, false, fmt.Errorf("replay provider event journal: %w", err)
	}
	return state, true, nil
}

func normalizeStateMaps(state *State) {
	if state.Lanes == nil {
		state.Lanes = make(map[provider.LaneID]LaneState)
	}
	if state.Operations == nil {
		state.Operations = make(map[provider.OperationID]struct{})
	}
	if state.QueueMutationReceipts == nil {
		state.QueueMutationReceipts = make(map[provider.OperationID]QueueMutationReceipt)
	}
	if state.PresentationMutationReceipts == nil {
		state.PresentationMutationReceipts = make(map[provider.OperationID]PresentationMutationReceipt)
	}
	if state.RuntimeControlMutationReceipts == nil {
		state.RuntimeControlMutationReceipts = make(map[provider.OperationID]RuntimeControlMutationReceipt)
	}
	if state.WorkspaceMutationReceipts == nil {
		state.WorkspaceMutationReceipts = make(map[provider.OperationID]WorkspaceMutationReceipt)
	}
	if state.LaneSelectionMutationReceipts == nil {
		state.LaneSelectionMutationReceipts = make(map[provider.OperationID]LaneSelectionMutationReceipt)
	}
	if state.AgentWaitObservationReceipts == nil {
		state.AgentWaitObservationReceipts = make(map[provider.OperationID]AgentWaitObservationReceipt)
	}
	normalizeCancelMutationReceipts(state)
	if state.Tools == nil {
		state.Tools = make(map[string]ToolState)
	}
	if state.Plans == nil {
		state.Plans = make(map[provider.OperationID]PlanState)
	}
	if state.Permissions == nil {
		state.Permissions = make(map[string]PermissionState)
	}
	if state.Background == nil {
		state.Background = make(map[string]BackgroundState)
	}
	if state.Usage == nil {
		state.Usage = make(map[provider.LaneID]provider.UsageEvent)
	}
	if state.Compactions == nil {
		state.Compactions = make(map[provider.LaneID]CompactionState)
	}
	if state.Transport == nil {
		state.Transport = make(map[provider.LaneID]provider.TransportHealthEvent)
	}
}

func normalizeCancelMutationReceipts(state *State) {
	if state.CancelMutationReceipts == nil {
		state.CancelMutationReceipts = make(map[provider.OperationID]CancelMutationReceipt)
	}
}

func (s FileStore) Save(state State) error {
	if err := saveStateEnvelope(s.Path, state, currentStateEnvelopeVersion); err != nil {
		return err
	}
	return resetProviderEventJournal(s.Path)
}

func (s FileStore) commitProviderEvent(baseRevision uint64, next State, command ProviderEventReceived) error {
	if !journalableProviderEvent(command.Event.Kind) {
		return s.Save(next)
	}
	if next.Revision == 0 || baseRevision != next.Revision-1 {
		return errors.New("provider event journal revision is not contiguous")
	}
	if strings.TrimSpace(next.ChatID) == "" || command.Event.Identity.ChatID != next.ChatID {
		return errors.New("provider event journal changed chat ownership")
	}
	if err := command.Event.Validate(); err != nil {
		return fmt.Errorf("validate provider event journal command: %w", err)
	}
	record := providerEventJournalRecord{
		Version: providerEventJournalVersion, BaseRevision: baseRevision,
		Revision: next.Revision, Command: command,
	}
	frame, err := encodeProviderEventJournalFrame(record)
	if err != nil {
		return err
	}
	appended, err := appendProviderEventJournalFrame(s.Path, frame)
	if err != nil {
		return err
	}
	if appended {
		return nil
	}
	// Bound restart work and disk growth. A checkpoint is still one ordinary
	// authoritative actor commit; it replaces hundreds or thousands of
	// per-chunk whole-state rewrites rather than weakening durability.
	return s.Save(next)
}

func journalableProviderEvent(kind provider.EventKind) bool {
	switch kind {
	case provider.EventAssistantChunk,
		provider.EventAssistantMedia,
		provider.EventThinkingUpdate,
		provider.EventToolUpdate,
		provider.EventPlanUpdate,
		provider.EventUsageUpdated,
		provider.EventCompactionStarted,
		provider.EventCompactionCheckpoint,
		provider.EventBackgroundWork:
		return true
	default:
		return false
	}
}

func providerEventJournalPath(actorPath string) string {
	return strings.TrimSpace(actorPath) + ".events"
}

func encodeProviderEventJournalFrame(record providerEventJournalRecord) ([]byte, error) {
	if record.Version != providerEventJournalVersion || record.Revision == 0 || record.BaseRevision != record.Revision-1 {
		return nil, errors.New("provider event journal record has invalid version or revision")
	}
	if !journalableProviderEvent(record.Command.Event.Kind) {
		return nil, errors.New("provider event is not eligible for the durable stream journal")
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode provider event journal record: %w", err)
	}
	if len(payload) == 0 || len(payload) > providerEventJournalMaxRecordBytes {
		return nil, errors.New("provider event journal record exceeds its bounded size")
	}
	frame := make([]byte, 4+len(payload)+4)
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	binary.BigEndian.PutUint32(frame[4+len(payload):], crc32.ChecksumIEEE(payload))
	return frame, nil
}

func appendProviderEventJournalFrame(actorPath string, frame []byte) (bool, error) {
	journalPath := providerEventJournalPath(actorPath)
	if strings.TrimSpace(actorPath) == "" {
		return false, errors.New("chat state store path is empty")
	}
	if len(frame) == 0 {
		return false, errors.New("provider event journal frame is empty")
	}
	dir := filepath.Dir(journalPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}

	info, err := os.Stat(journalPath)
	created := false
	switch {
	case errors.Is(err, os.ErrNotExist):
		if int64(len(providerEventJournalMagic)+len(frame)) > providerEventJournalCheckpointBytes {
			return false, nil
		}
		created = true
	case err != nil:
		return false, err
	case !info.Mode().IsRegular():
		return false, errors.New("provider event journal is not a regular file")
	}

	flags := os.O_RDWR | os.O_APPEND
	if created {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(journalPath, flags, 0o600)
	if err != nil {
		return false, err
	}
	closeWithError := func(cause error) (bool, error) {
		return false, errors.Join(cause, file.Close())
	}

	openedInfo, err := file.Stat()
	if err != nil {
		return closeWithError(err)
	}
	originalSize := openedInfo.Size()
	initializedEmpty := originalSize == 0
	if originalSize > 0 {
		if originalSize < int64(len(providerEventJournalMagic)) {
			return closeWithError(errors.New("provider event journal header is truncated"))
		}
		header := make([]byte, len(providerEventJournalMagic))
		if _, err := file.ReadAt(header, 0); err != nil {
			return closeWithError(err)
		}
		if string(header) != providerEventJournalMagic {
			return closeWithError(errors.New("provider event journal has an unsupported schema"))
		}
	}
	additional := int64(len(frame))
	if initializedEmpty {
		additional += int64(len(providerEventJournalMagic))
	}
	if originalSize+additional > providerEventJournalCheckpointBytes {
		if err := file.Close(); err != nil {
			return false, err
		}
		return false, nil
	}

	rollback := func(cause error) (bool, error) {
		truncateErr := file.Truncate(originalSize)
		syncErr := file.Sync()
		closeErr := file.Close()
		if created && originalSize == 0 {
			removeErr := os.Remove(journalPath)
			if errors.Is(removeErr, os.ErrNotExist) {
				removeErr = nil
			}
			return false, errors.Join(cause, truncateErr, syncErr, closeErr, removeErr)
		}
		return false, errors.Join(cause, truncateErr, syncErr, closeErr)
	}
	if initializedEmpty {
		if err := writeFull(file, []byte(providerEventJournalMagic)); err != nil {
			return rollback(err)
		}
	}
	if err := writeFull(file, frame); err != nil {
		return rollback(err)
	}
	if err := file.Sync(); err != nil {
		return closeWithError(err)
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if created || initializedEmpty {
		if err := durablefs.SyncDirectory(dir); err != nil {
			return false, err
		}
	}
	return true, nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func replayProviderEventJournal(actorPath string, state State) (State, error) {
	file, err := os.Open(providerEventJournalPath(actorPath))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return State{}, err
	}
	defer file.Close()

	header := make([]byte, len(providerEventJournalMagic))
	if _, err := io.ReadFull(file, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// A header that never completed could not have acknowledged an event.
			return state, nil
		}
		return State{}, err
	}
	if string(header) != providerEventJournalMagic {
		return State{}, errors.New("provider event journal has an unsupported schema")
	}

	applied := false
	for {
		var sizeBytes [4]byte
		if _, err := io.ReadFull(file, sizeBytes[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return State{}, err
		}
		size := binary.BigEndian.Uint32(sizeBytes[:])
		if size == 0 || uint64(size) > providerEventJournalMaxRecordBytes {
			return State{}, errors.New("provider event journal record has invalid size")
		}
		payload := make([]byte, int(size))
		if _, err := io.ReadFull(file, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return State{}, err
		}
		var checksumBytes [4]byte
		if _, err := io.ReadFull(file, checksumBytes[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return State{}, err
		}
		if got, want := binary.BigEndian.Uint32(checksumBytes[:]), crc32.ChecksumIEEE(payload); got != want {
			return State{}, errors.New("provider event journal checksum mismatch")
		}
		var record providerEventJournalRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return State{}, fmt.Errorf("decode provider event journal record: %w", err)
		}
		if record.Version != providerEventJournalVersion {
			return State{}, fmt.Errorf("unsupported provider event journal version %d", record.Version)
		}
		if record.Revision == 0 || record.BaseRevision != record.Revision-1 {
			return State{}, errors.New("provider event journal record revision is invalid")
		}
		if !journalableProviderEvent(record.Command.Event.Kind) {
			return State{}, errors.New("provider event journal contains an effectful event")
		}
		if record.Revision <= state.Revision {
			continue
		}
		if record.BaseRevision != state.Revision {
			return State{}, fmt.Errorf("provider event journal revision gap: got base %d, want %d", record.BaseRevision, state.Revision)
		}
		if record.Command.Event.Identity.ChatID != state.ChatID {
			return State{}, errors.New("provider event journal belongs to another chat")
		}
		effects, err := reduceProviderEvent(&state, record.Command)
		if err != nil {
			return State{}, err
		}
		if len(effects) != 0 {
			return State{}, errors.New("provider event journal unexpectedly produced external effects")
		}
		state.Revision++
		if state.Revision != record.Revision {
			return State{}, errors.New("provider event journal replay changed actor revision")
		}
		applied = true
	}
	if applied {
		if err := state.Validate(); err != nil {
			return State{}, fmt.Errorf("validate replayed chat state: %w", err)
		}
	}
	return state, nil
}

func resetProviderEventJournal(actorPath string) error {
	path := providerEventJournalPath(actorPath)
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return durablefs.SyncDirectory(filepath.Dir(path))
}

func saveStateEnvelope(path string, state State, version int) error {
	if err := state.Validate(); err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("chat state store path is empty")
	}
	raw, err := json.Marshal(stateEnvelope{Version: version, State: state})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".chat-state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
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
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return durablefs.SyncDirectory(dir)
}
