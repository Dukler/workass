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
//     LedgerEvent at a time, so a 300 MB file is never held whole.
//   - Each imported actor is then opened once, in the same transaction, as
//     FileStore discovery loads it (decode, validate, journal replay). An
//     actor that does not open fails the import where JSON mode would fail
//     to start: the store stays importing, the JSON stays authoritative, and
//     the decision is logged and kept in ImportRefusalFileName.
//   - A file whose digest matches its receipt is not imported again. A file
//     that changed while the store is still importing (the JSON is still
//     authoritative) is re-imported in place.
//   - The JSON is only read. It is never rewritten, renamed or deleted.
//   - It runs at boot, before any chat actor exists. A file's transaction
//     holds the shared connection for the whole file (a 300 MB actor takes
//     seconds), which is acceptable only because nothing else can commit
//     yet; runtime reads and checkpoints keep their transactions short.
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
		filePath := filepath.Join(actorDir, name)
		outcome, err := importActorFile(ctx, db, opts, filePath, rel)
		if err != nil {
			if ctx.Err() != nil {
				return result, fmt.Errorf("import chat actor %q: %w", name, err)
			}
			// File discovery refuses to start on this file too. The import
			// stops here, as discovery does: no receipt in the store for it,
			// so the store cannot go live without it, and its JSON untouched.
			receiptPath := recordImportRefusal(stateDir, rel, filePath, err, db.BinaryVersion(), logf)
			if receiptPath != "" {
				return result, fmt.Errorf("import chat actor %q: %w (the JSON is left in place and chats stay in it until this file is fixed or removed; receipt %s)", name, err, receiptPath)
			}
			return result, fmt.Errorf("import chat actor %q: %w (the JSON is left in place and chats stay in it until this file is fixed or removed)", name, err)
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
	clearImportRefusal(stateDir, logf)
	return result, nil
}

// ErrFileActorsChangedAfterCutover means a provider-chats JSON file was
// written after the chat store went live in SQLite (an older or
// SQLite-disabled build ran against this state directory). Neither copy can
// be trusted to be the newer one, so the daemon refuses to guess.
var ErrFileActorsChangedAfterCutover = errors.New("provider-chats JSON changed after the chat store moved to SQLite")

// changedAfterCutover names the file that diverged and what to do about it.
// The usual cause is a rollback cycle: an older or JSON-only build ran on this
// state directory and rewrote the JSON, then state.db was put back.
func changedAfterCutover(stateDir, rel, what string) error {
	dbPath := filepath.Join(stateDir, store.FileName)
	return fmt.Errorf("%w: %s %s; a build without the SQLite chat store wrote it, so it and %s disagree. "+
		"To start from the JSON as it is now, quit Workass and move %s, %s-wal and %s-shm (those that exist) out of %s: "+
		"the next start imports the current JSON, and chat changes saved only in the moved %s since the switch are not carried over",
		ErrFileActorsChangedAfterCutover, filepath.Join(stateDir, filepath.FromSlash(rel)), what, dbPath,
		store.FileName, store.FileName, store.FileName, stateDir, store.FileName)
}

// VerifyLiveFileActors checks that no JSON actor was created or changed since
// its receipt. It is cheap on an untouched directory: a listing and a stat
// per file, plus a hash of each journal.
//
// A file whose size and modification time still match its receipt is
// unchanged. One whose time moved but whose size did not is hashed: a copy or
// a restore (cp -R, the macOS updater's state snapshot, which keeps times
// only to the microsecond) moves the time without changing a byte. Identical
// bytes are accepted and the receipt takes the new size and time, so the
// next boot is a stat again; different bytes are a write after the cutover
// and fail closed. A different size is different bytes and is never hashed.
func VerifyLiveFileActors(ctx context.Context, db *store.DB, opts ImportOptions) error {
	stateDir, actorDir, _, err := importDirs(opts)
	if err != nil {
		return err
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
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
	var refreshed []store.ImportReceipt
	for _, name := range files {
		rel := path.Join(filepath.ToSlash(relOrBase(stateDir, actorDir)), name)
		receipt, ok := receipts[rel]
		if !ok {
			return changedAfterCutover(stateDir, rel, "was created after the switch (it has no import receipt)")
		}
		filePath := filepath.Join(actorDir, name)
		info, err := os.Stat(filePath)
		if err != nil {
			return err
		}
		if info.Size() != receipt.Size {
			return changedAfterCutover(stateDir, rel, "was modified after its import")
		}
		if info.ModTime().UnixNano() != receipt.MTimeNS {
			digest, hashed, err := statAndSHA256(filePath)
			if err != nil {
				return err
			}
			if digest != receipt.SHA256 || hashed.Size() != receipt.Size {
				return changedAfterCutover(stateDir, rel, "was modified after its import")
			}
			receipt.Size, receipt.MTimeNS = hashed.Size(), hashed.ModTime().UnixNano()
			refreshed = append(refreshed, receipt)
		}
		companion := filepath.Join(actorDir, name+".events")
		digest, exists, err := fileSHA256(companion)
		if err != nil {
			return err
		}
		if exists != (receipt.Companion != "") || (exists && digest != receipt.CompanionSHA256) {
			return changedAfterCutover(stateDir, rel+".events", "changed after its import")
		}
	}
	if len(refreshed) == 0 {
		return nil
	}
	if err := db.Write(ctx, func(tx *store.Tx) error {
		for _, receipt := range refreshed {
			if err := tx.PutImportReceipt(receipt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, receipt := range refreshed {
		logf("[store] chat actor %s has new file times but the bytes of its import; receipt refreshed", receipt.Source)
	}
	return nil
}

// statAndSHA256 hashes a file and stats the same open file once the hash is
// read, so the size and time it returns describe the bytes it hashed.
func statAndSHA256(filePath string) (string, os.FileInfo, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), info, nil
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
	}
	if err := openImportedActor(tx, importer.chatID); err != nil {
		return importOutcome{}, err
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
	// ledger_gen 1: these rows are the committed ledger (an open journal is
	// folded in below before the receipt commits).
	_, err = im.tx.Exec(`UPDATE chats SET revision = ?, state_json = ?, ledger_len = ?, ledger_gen = 1 WHERE chat_id = ?`,
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

// openImportedActor opens the actor just imported, inside the import
// transaction, exactly as the runtime will open it: decode, validate and
// journal replay, which is what FileStore.Load does for the file. File
// discovery loads every JSON actor that way at boot and refuses to start on
// one that does not load, so an actor that does not open here fails the
// import the same way, instead of importing and failing only when the chat
// is first opened, with its JSON already a frozen backup.
//
// An imported .events journal is then folded into the checkpoint, as the
// first open would (Load replays, Save checkpoints): afterwards the heads and
// ledger rows describe the committed state, and chat_events only ever holds
// journal records the SQLite store wrote itself.
func openImportedActor(tx *store.Tx, chatID string) error {
	state, found, err := loadSQLActor(tx, chatID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("imported actor disappeared before it was opened")
	}
	var pending int
	if err := tx.QueryRow(`SELECT count(*) FROM chat_events WHERE chat_id = ?`, chatID).Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		return nil
	}
	if err := state.Validate(); err != nil {
		return fmt.Errorf("validate replayed chat state: %w", err)
	}
	if err := writeSQLFullCheckpoint(tx, chatID, state); err != nil {
		return fmt.Errorf("checkpoint imported journal: %w", err)
	}
	return nil
}

// ImportRefusalFileName is the receipt, in the state directory, of the last
// chat actor file the import refused. The refused JSON is never touched and
// the chat store stays on JSON (importing) until the file is fixed or
// removed; a completed import removes the receipt.
const ImportRefusalFileName = "chat-import-refused.json"

// ImportRefusal is the content of ImportRefusalFileName.
type ImportRefusal struct {
	Store         string `json:"store"`
	Source        string `json:"source"`
	SHA256        string `json:"sha256,omitempty"`
	Size          int64  `json:"size"`
	MTimeNS       int64  `json:"mtimeNs"`
	Reason        string `json:"reason"`
	JSON          string `json:"json"`
	BinaryVersion string `json:"binaryVersion"`
	RefusedAt     string `json:"refusedAt"`
}

// recordImportRefusal writes the refusal receipt for one source file. It is
// best effort: the import fails either way, and a receipt that cannot be
// written is logged.
func recordImportRefusal(stateDir, rel, filePath string, cause error, binaryVersion string, logf func(string, ...any)) string {
	refusal := ImportRefusal{
		Store: ActorStoreName, Source: rel, Reason: cause.Error(),
		JSON:          "left in place, unchanged; chats stay in the provider-chats JSON until this file is fixed or removed",
		BinaryVersion: binaryVersion, RefusedAt: sqlNow(),
	}
	if digest, info, err := statAndSHA256(filePath); err == nil {
		refusal.SHA256, refusal.Size, refusal.MTimeNS = digest, info.Size(), info.ModTime().UnixNano()
	}
	receiptPath := filepath.Join(stateDir, ImportRefusalFileName)
	logf("[store] refused chat actor %s: %v; the JSON is left in place and the chat store stays on JSON (receipt %s)", rel, cause, receiptPath)
	raw, err := json.MarshalIndent(refusal, "", "  ")
	if err == nil {
		err = writeFileAtomically(receiptPath, append(raw, '\n'))
	}
	if err != nil {
		logf("[store] could not write the import refusal receipt %s: %v", receiptPath, err)
		return ""
	}
	return receiptPath
}

// clearImportRefusal removes a refusal receipt once the import it blocked
// has completed.
func clearImportRefusal(stateDir string, logf func(string, ...any)) {
	receiptPath := filepath.Join(stateDir, ImportRefusalFileName)
	if err := os.Remove(receiptPath); err == nil {
		logf("[store] chat import completed; removed the earlier refusal receipt %s", receiptPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		logf("[store] could not remove the import refusal receipt %s: %v", receiptPath, err)
	}
}

func writeFileAtomically(target string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, target)
}
