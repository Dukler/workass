package chat

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"workass/internal/store"
)

// ActorStoreName is the migrated_stores / import_receipts key of the chat
// actor store.
const ActorStoreName = "chats"

// ImportOptions configures ImportFileActors.
type ImportOptions struct {
	// StateDir is the daemon state directory; receipts record sources
	// relative to it (e.g. "provider-chats/<sha256>.json").
	StateDir string
	// ActorDir is the provider-chats directory. Defaults to
	// StateDir/provider-chats.
	ActorDir string
	// ExpectedFileName, when set, maps a ChatID to the file name the runtime
	// opens it by. A file named otherwise is not an addressable actor today
	// (FileStore is always opened by that name), so it is skipped with a
	// receipt instead of becoming a chat that never existed.
	ExpectedFileName func(chatID string) string
	// Logf receives one line per imported or skipped file.
	Logf func(format string, args ...any)
	// afterRows, tests only, runs inside an actor's import transaction after
	// its ledger rows were written and before its receipt is; an error aborts
	// that transaction.
	afterRows func(chatID string, rows int) error
}

// ImportResult counts what one ImportFileActors run did.
type ImportResult struct {
	Imported  int
	Unchanged int
	Skipped   int
	Removed   int
	Live      bool
}

// ImportFileActors performs the one-shot, resumable import of
// provider-chats/*.json (and their .events journals) into SQLite.
//
//   - Each source file is imported in one transaction together with its
//     sha256 receipt, so a crash at any point leaves every actor either fully
//     imported or untouched; a re-run resumes from the receipts.
//   - Files are stream-decoded: the ledger goes to the messages table one
//     LedgerEvent at a time, so a 300 MB actor is never held whole.
//   - A file whose digest matches its receipt is not imported again. A file
//     that changed while the store is still importing (the JSON is still
//     authoritative) is re-imported in place.
//   - The JSON is only read. It is never rewritten, renamed or deleted.
//
// When every file has a receipt the store flips to live. ImportFileActors on
// a live store does nothing; use VerifyLiveFileActors to detect a JSON
// backup written after the cutover.
func ImportFileActors(ctx context.Context, db *store.DB, opts ImportOptions) (ImportResult, error) {
	stateDir, actorDir, source, err := importDirs(opts)
	if err != nil {
		return ImportResult{}, err
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var result ImportResult
	live := false
	if err := db.Write(ctx, func(tx *store.Tx) error {
		status, ok, err := tx.MigratedStoreStatus(ActorStoreName)
		if err != nil {
			return err
		}
		if ok && status.Status == store.StoreLive {
			live = true
			return nil
		}
		return tx.BeginStoreImport(ActorStoreName, source, db.BinaryVersion())
	}); err != nil {
		return ImportResult{}, err
	}
	if live {
		result.Live = true
		return result, nil
	}

	files, err := listActorFiles(actorDir)
	if err != nil {
		return ImportResult{}, err
	}
	present := make(map[string]struct{}, len(files))
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		rel := path.Join(filepath.ToSlash(relOrBase(stateDir, actorDir)), name)
		present[rel] = struct{}{}
		outcome, err := importActorFile(ctx, db, opts, filepath.Join(actorDir, name), rel)
		if err != nil {
			return result, fmt.Errorf("import chat actor %q: %w", name, err)
		}
		switch outcome.kind {
		case importOutcomeImported:
			result.Imported++
			logf("[store] imported chat actor %s (%s, %d ledger rows)", name, outcome.chatID, outcome.rows)
		case importOutcomeUnchanged:
			result.Unchanged++
		case importOutcomeSkipped:
			result.Skipped++
			logf("[store] skipped chat actor %s: %s", name, outcome.detail)
		}
	}

	// A source that vanished while the store was still importing was deleted
	// by its authoritative owner; its imported copy goes with it.
	if err := db.Write(ctx, func(tx *store.Tx) error {
		receipts, err := tx.ImportReceipts(ActorStoreName)
		if err != nil {
			return err
		}
		removed := 0
		for _, receipt := range receipts {
			if _, ok := present[receipt.Source]; ok {
				continue
			}
			if receipt.Outcome == store.OutcomeImported {
				if _, err := tx.Exec(`DELETE FROM chats WHERE chat_id = ?`, receipt.ItemKey); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`DELETE FROM import_receipts WHERE store = ? AND source = ?`, ActorStoreName, receipt.Source); err != nil {
				return err
			}
			removed++
		}
		var receiptCount, chatCount int
		if err := tx.QueryRow(`SELECT count(*) FROM import_receipts WHERE store = ? AND outcome = ?`, ActorStoreName, store.OutcomeImported).Scan(&receiptCount); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT count(*) FROM chats`).Scan(&chatCount); err != nil {
			return err
		}
		if receiptCount != chatCount {
			return fmt.Errorf("import is inconsistent: %d imported receipts, %d chat rows", receiptCount, chatCount)
		}
		result.Removed = removed
		return tx.CompleteStoreImport(ActorStoreName, chatCount)
	}); err != nil {
		return result, err
	}
	result.Live = true
	return result, nil
}

// ErrFileActorsChangedAfterCutover means a provider-chats JSON file was
// written after the chat store went live in SQLite (an older or
// SQLite-disabled build ran against this state directory). Neither copy can
// be trusted to be the newer one, so the daemon refuses to guess.
var ErrFileActorsChangedAfterCutover = errors.New("provider-chats JSON changed after the chat store moved to SQLite")

// VerifyLiveFileActors checks, cheaply (directory listing and stat; a hash
// only for journals), that no JSON actor was created or changed since its
// receipt.
func VerifyLiveFileActors(ctx context.Context, db *store.DB, opts ImportOptions) error {
	stateDir, actorDir, _, err := importDirs(opts)
	if err != nil {
		return err
	}
	files, err := listActorFiles(actorDir)
	if err != nil {
		return err
	}
	var receipts map[string]store.ImportReceipt
	if err := db.Read(ctx, func(tx *store.Tx) error {
		list, err := tx.ImportReceipts(ActorStoreName)
		if err != nil {
			return err
		}
		receipts = make(map[string]store.ImportReceipt, len(list))
		for _, receipt := range list {
			receipts[receipt.Source] = receipt
		}
		return nil
	}); err != nil {
		return err
	}
	for _, name := range files {
		rel := path.Join(filepath.ToSlash(relOrBase(stateDir, actorDir)), name)
		receipt, ok := receipts[rel]
		if !ok {
			return fmt.Errorf("%w: %s has no import receipt", ErrFileActorsChangedAfterCutover, rel)
		}
		info, err := os.Stat(filepath.Join(actorDir, name))
		if err != nil {
			return err
		}
		if info.Size() != receipt.Size || info.ModTime().UnixNano() != receipt.MTimeNS {
			return fmt.Errorf("%w: %s was modified after its import", ErrFileActorsChangedAfterCutover, rel)
		}
		companion := filepath.Join(actorDir, name+".events")
		digest, exists, err := fileSHA256(companion)
		if err != nil {
			return err
		}
		if exists != (receipt.Companion != "") || (exists && digest != receipt.CompanionSHA256) {
			return fmt.Errorf("%w: %s.events changed after its import", ErrFileActorsChangedAfterCutover, rel)
		}
	}
	return nil
}

func importDirs(opts ImportOptions) (stateDir, actorDir, source string, err error) {
	stateDir = strings.TrimSpace(opts.StateDir)
	actorDir = strings.TrimSpace(opts.ActorDir)
	if stateDir == "" {
		return "", "", "", errors.New("chat actor import requires the state directory")
	}
	if actorDir == "" {
		actorDir = filepath.Join(stateDir, "provider-chats")
	}
	return stateDir, actorDir, filepath.ToSlash(relOrBase(stateDir, actorDir)), nil
}

func relOrBase(stateDir, dir string) string {
	rel, err := filepath.Rel(stateDir, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(dir)
	}
	return rel
}

// listActorFiles mirrors DiscoverFileStates' selection: regular *.json
// entries, in name order.
func listActorFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

type importOutcomeKind int

const (
	importOutcomeImported importOutcomeKind = iota
	importOutcomeUnchanged
	importOutcomeSkipped
)

type importOutcome struct {
	kind   importOutcomeKind
	chatID string
	rows   int
	detail string
}

func importActorFile(ctx context.Context, db *store.DB, opts ImportOptions, filePath, rel string) (importOutcome, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return importOutcome{}, err
	}
	companionPath := filePath + ".events"
	companionDigest, companionExists, err := fileSHA256(companionPath)
	if err != nil {
		return importOutcome{}, err
	}
	companionRel := ""
	if companionExists {
		companionRel = rel + ".events"
	}

	var existing store.ImportReceipt
	var hasReceipt bool
	if err := db.Read(ctx, func(tx *store.Tx) error {
		var err error
		existing, hasReceipt, err = tx.ImportReceiptFor(ActorStoreName, rel)
		return err
	}); err != nil {
		return importOutcome{}, err
	}
	if hasReceipt {
		digest, _, err := fileSHA256(filePath)
		if err != nil {
			return importOutcome{}, err
		}
		if digest == existing.SHA256 && companionDigest == existing.CompanionSHA256 && companionRel == existing.Companion {
			if existing.Size != info.Size() || existing.MTimeNS != info.ModTime().UnixNano() {
				// Same bytes, new metadata (a copy or a touch): refresh the
				// receipt so the live check stays a cheap stat.
				existing.Size, existing.MTimeNS = info.Size(), info.ModTime().UnixNano()
				if err := db.Write(ctx, func(tx *store.Tx) error { return tx.PutImportReceipt(existing) }); err != nil {
					return importOutcome{}, err
				}
			}
			return importOutcome{kind: importOutcomeUnchanged, chatID: existing.ItemKey}, nil
		}
	}

	receipt := store.ImportReceipt{
		Store: ActorStoreName, Source: rel, Size: info.Size(), MTimeNS: info.ModTime().UnixNano(),
		Companion: companionRel, CompanionSHA256: companionDigest, BinaryVersion: db.BinaryVersion(),
	}
	var outcome importOutcome
	err = db.Write(ctx, func(tx *store.Tx) error {
		var err error
		outcome, err = streamActorFile(tx, opts, filePath, companionPath, companionExists, &receipt, existing, hasReceipt)
		return err
	})
	if err != nil {
		return importOutcome{}, err
	}
	return outcome, nil
}

var errSkipActorFile = errors.New("skip actor file")

// streamActorFile imports one actor inside tx. It reads the file once, hashing
// while it decodes.
func streamActorFile(tx *store.Tx, opts ImportOptions, filePath, companionPath string, companionExists bool, receipt *store.ImportReceipt, previous store.ImportReceipt, hadPrevious bool) (importOutcome, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return importOutcome{}, err
	}
	defer file.Close()
	hasher := sha256.New()
	tee := io.TeeReader(bufio.NewReaderSize(file, 1<<20), hasher)

	importer := &actorStreamImporter{tx: tx, opts: opts, source: receipt.Source, previous: previous, hadPrevious: hadPrevious}
	decodeErr := importer.decodeEnvelope(json.NewDecoder(tee))
	if decodeErr != nil && !errors.Is(decodeErr, errSkipActorFile) {
		return importOutcome{}, decodeErr
	}
	// Hash every byte, including anything the decoder never needed.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return importOutcome{}, err
	}
	receipt.SHA256 = hex.EncodeToString(hasher.Sum(nil))

	if errors.Is(decodeErr, errSkipActorFile) {
		// The skip decision precedes any row of this file; only an earlier
		// import of the same source can be left to remove.
		if hadPrevious && previous.Outcome == store.OutcomeImported {
			if _, err := tx.Exec(`DELETE FROM chats WHERE chat_id = ?`, previous.ItemKey); err != nil {
				return importOutcome{}, err
			}
		}
		receipt.Outcome = store.OutcomeSkipped
		receipt.ItemKey = importer.chatID
		receipt.Detail = importer.skipReason
		if err := tx.PutImportReceipt(*receipt); err != nil {
			return importOutcome{}, err
		}
		return importOutcome{kind: importOutcomeSkipped, chatID: importer.chatID, detail: importer.skipReason}, nil
	}

	if companionExists {
		if err := importer.importJournal(companionPath); err != nil {
			return importOutcome{}, fmt.Errorf("import provider event journal: %w", err)
		}
		if err := checkpointImportedJournal(tx, importer.chatID); err != nil {
			return importOutcome{}, fmt.Errorf("checkpoint imported journal: %w", err)
		}
	}
	if opts.afterRows != nil {
		if err := opts.afterRows(importer.chatID, importer.rows); err != nil {
			return importOutcome{}, err
		}
	}
	receipt.Outcome = store.OutcomeImported
	receipt.ItemKey = importer.chatID
	if err := tx.PutImportReceipt(*receipt); err != nil {
		return importOutcome{}, err
	}
	return importOutcome{kind: importOutcomeImported, chatID: importer.chatID, rows: importer.rows}, nil
}

type actorStreamImporter struct {
	tx          *store.Tx
	opts        ImportOptions
	source      string
	previous    store.ImportReceipt
	hadPrevious bool

	chatID     string
	skipReason string
	rows       int
	revision   uint64
}

// decodeEnvelope walks {"v":N,"state":{...}} with the same key matching and
// trailing-data rules json.Unmarshal applies to the file.
func (im *actorStreamImporter) decodeEnvelope(dec *json.Decoder) error {
	if err := expectDelim(dec, '{'); err != nil {
		return fmt.Errorf("decode chat state: %w", err)
	}
	version := 0
	sawState := false
	for dec.More() {
		key, err := tokenString(dec)
		if err != nil {
			return fmt.Errorf("decode chat state: %w", err)
		}
		switch {
		case strings.EqualFold(key, "v"):
			if err := dec.Decode(&version); err != nil {
				return fmt.Errorf("decode chat state: %w", err)
			}
		case strings.EqualFold(key, "state"):
			if sawState {
				return errors.New("decode chat state: duplicate state object")
			}
			sawState = true
			// errSkipActorFile stops decoding where it is; the caller
			// still hashes the rest of the file.
			if err := im.decodeState(dec); err != nil {
				return err
			}
		default:
			var ignored json.RawMessage
			if err := dec.Decode(&ignored); err != nil {
				return fmt.Errorf("decode chat state: %w", err)
			}
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return fmt.Errorf("decode chat state: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("decode chat state: invalid data after top-level value")
	}
	if version != currentStateEnvelopeVersion {
		return fmt.Errorf("unsupported chat state version %d", version)
	}
	if !sawState || im.chatID == "" {
		return errors.New("chat actor has no immutable chat id")
	}
	return nil
}

// decodeState streams the state object: every field except the ledger is
// kept raw for the head; ledger elements become rows as they are decoded.
func (im *actorStreamImporter) decodeState(dec *json.Decoder) error {
	if err := expectDelim(dec, '{'); err != nil {
		return fmt.Errorf("decode chat state: %w", err)
	}
	var head bytes.Buffer
	head.WriteByte('{')
	fields := 0
	ledgerKind := "" // "", "null", "empty", "rows"
	insert, err := im.tx.Prepare(`INSERT INTO messages(chat_id, seq, message_id, role, event_json) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	headReady := false
	for dec.More() {
		key, err := tokenString(dec)
		if err != nil {
			return fmt.Errorf("decode chat state: %w", err)
		}
		if strings.EqualFold(key, "Ledger") {
			if ledgerKind != "" {
				return errors.New("decode chat state: duplicate ledger")
			}
			token, err := dec.Token()
			if err != nil {
				return fmt.Errorf("decode chat state: %w", err)
			}
			if token == nil {
				ledgerKind = "null"
				continue
			}
			if delim, ok := token.(json.Delim); !ok || delim != '[' {
				return errors.New("decode chat state: ledger is not an array")
			}
			ledgerKind = "empty"
			for dec.More() {
				if !headReady {
					// The chat id must be known before the first row is
					// written. json.Marshal emits ChatID first; anything
					// else is not a file this daemon wrote.
					if err := im.prepareChat(head.Bytes()); err != nil {
						return err
					}
					headReady = true
				}
				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					return fmt.Errorf("decode chat ledger row %d: %w", im.rows+1, err)
				}
				var meta struct {
					MessageID string
					Role      string
				}
				if err := json.Unmarshal(raw, &meta); err != nil {
					return fmt.Errorf("decode chat ledger row %d: %w", im.rows+1, err)
				}
				im.rows++
				ledgerKind = "rows"
				if _, err := insert.Exec(im.chatID, im.rows, meta.MessageID, meta.Role, sqlText(raw)); err != nil {
					return err
				}
			}
			if err := expectDelim(dec, ']'); err != nil {
				return fmt.Errorf("decode chat state: %w", err)
			}
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("decode chat state field %q: %w", key, err)
		}
		if fields > 0 {
			head.WriteByte(',')
		}
		quoted, _ := json.Marshal(key)
		head.Write(quoted)
		head.WriteByte(':')
		head.Write(raw)
		fields++
	}
	if err := expectDelim(dec, '}'); err != nil {
		return fmt.Errorf("decode chat state: %w", err)
	}
	switch ledgerKind {
	case "null", "":
		appendHeadField(&head, &fields, "Ledger", "null")
	case "empty":
		appendHeadField(&head, &fields, "Ledger", "[]")
	}
	if !headReady {
		if err := im.prepareChat(head.Bytes()); err != nil {
			return err
		}
	}
	head.WriteByte('}')
	// The complete head (every non-ledger field) is now known.
	var state State
	if err := json.Unmarshal(head.Bytes(), &state); err != nil {
		return fmt.Errorf("decode chat state: %w", err)
	}
	if strings.TrimSpace(state.ChatID) != im.chatID {
		return errors.New("chat actor changed its chat id mid-file")
	}
	im.revision = state.Revision
	_, err = im.tx.Exec(`UPDATE chats SET revision = ?, state_json = ?, ledger_len = ? WHERE chat_id = ?`,
		int64(state.Revision), sqlText(head.Bytes()), im.rows, im.chatID)
	return err
}

func appendHeadField(head *bytes.Buffer, fields *int, key, raw string) {
	if *fields > 0 {
		head.WriteByte(',')
	}
	head.WriteString(`"` + key + `":` + raw)
	*fields++
}

// prepareChat learns the chat id from the head fields decoded so far and
// creates (or recreates) the chat row the ledger rows hang off. It returns
// errSkipActorFile for a file that is not an addressable actor.
func (im *actorStreamImporter) prepareChat(partialHead []byte) error {
	// partialHead is "{" plus the fields so far; closing it makes it valid.
	probe := append(append([]byte(nil), partialHead...), '}')
	var header struct {
		ChatID string `json:"ChatID"`
	}
	if err := json.Unmarshal(probe, &header); err != nil {
		return fmt.Errorf("decode chat state: %w", err)
	}
	chatID := strings.TrimSpace(header.ChatID)
	if chatID == "" {
		return errors.New("chat actor has no immutable chat id")
	}
	im.chatID = chatID
	if im.opts.ExpectedFileName != nil {
		if want := im.opts.ExpectedFileName(chatID); want != "" && want != path.Base(im.source) {
			im.skipReason = fmt.Sprintf("file name does not match chat id %q (want %s)", chatID, want)
			return errSkipActorFile
		}
	}
	// Another source already imported this chat: the directory holds two
	// actors for one chat, which discovery has always refused.
	var otherSource string
	err := im.tx.QueryRow(`SELECT source FROM import_receipts WHERE store = ? AND item_key = ? AND outcome = ? AND source != ?`,
		ActorStoreName, chatID, store.OutcomeImported, im.source).Scan(&otherSource)
	if err == nil {
		return fmt.Errorf("duplicate durable chat actor %q (also in %s)", chatID, otherSource)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if im.hadPrevious && im.previous.Outcome == store.OutcomeImported && im.previous.ItemKey != chatID {
		if _, err := im.tx.Exec(`DELETE FROM chats WHERE chat_id = ?`, im.previous.ItemKey); err != nil {
			return err
		}
	}
	// Re-import replaces the chat wholesale (rows and journal cascade).
	if _, err := im.tx.Exec(`DELETE FROM chats WHERE chat_id = ?`, chatID); err != nil {
		return err
	}
	_, err = im.tx.Exec(`INSERT INTO chats(chat_id, envelope_version, revision, state_json, ledger_len, journal_bytes, updated_at)
		VALUES (?, ?, 0, '{}', 0, 0, ?)`, chatID, currentStateEnvelopeVersion, sqlNow())
	return err
}

// importJournal copies the .events records the file store would replay
// (revision above the checkpoint) into chat_events, unchanged. Load replays
// them exactly as FileStore.Load replays the file.
func (im *actorStreamImporter) importJournal(journalPath string) error {
	file, err := os.Open(journalPath)
	if err != nil {
		return err
	}
	defer file.Close()
	frames, err := newProviderEventJournalFrameReader(bufio.NewReader(file))
	if err != nil || frames == nil {
		return err
	}
	var journalBytes int64
	for {
		payload, err := frames.next()
		if err != nil {
			return err
		}
		if payload == nil {
			break
		}
		record, err := decodeProviderEventJournalRecord(payload)
		if err != nil {
			return err
		}
		if record.Revision <= im.revision {
			continue
		}
		if journalBytes == 0 {
			journalBytes = int64(len(providerEventJournalMagic))
		}
		journalBytes += int64(providerEventJournalFrameLen(payload))
		if _, err := im.tx.Exec(`INSERT INTO chat_events(chat_id, revision, base_revision, record) VALUES (?, ?, ?, ?)`,
			im.chatID, int64(record.Revision), int64(record.BaseRevision), sqlText(payload)); err != nil {
			return err
		}
	}
	_, err = im.tx.Exec(`UPDATE chats SET journal_bytes = ? WHERE chat_id = ?`, journalBytes, im.chatID)
	return err
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != want {
		return fmt.Errorf("expected %q, got %v", want, token)
	}
	return nil
}

func tokenString(dec *json.Decoder) (string, error) {
	token, err := dec.Token()
	if err != nil {
		return "", err
	}
	key, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("expected an object key, got %v", token)
	}
	return key, nil
}

// fileSHA256 streams a file through sha256. exists is false for a missing
// file (digest "").
func fileSHA256(filePath string) (digest string, exists bool, err error) {
	file, err := os.Open(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	var hasher hash.Hash = sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", true, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), true, nil
}

// checkpointImportedJournal folds an imported .events journal into its
// checkpoint inside the import transaction, exactly as the first actor open
// would (Load replays, Save checkpoints). Afterwards the imported actor's
// heads and ledger rows describe its committed state, and chat_events only
// ever holds journal records the SQLite store wrote itself. This is the one
// import step that decodes a whole actor; it runs only for chats a crash or
// shutdown left with an open journal.
func checkpointImportedJournal(tx *store.Tx, chatID string) error {
	var pending int
	if err := tx.QueryRow(`SELECT count(*) FROM chat_events WHERE chat_id = ?`, chatID).Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		return nil
	}
	state, found, err := loadSQLActor(tx, chatID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("imported actor disappeared before its journal checkpoint")
	}
	if err := state.Validate(); err != nil {
		return err
	}
	head, rows, err := encodeSQLActor(state)
	if err != nil {
		return err
	}
	return writeSQLCheckpoint(tx, chatID, state.Revision, head, rows)
}
