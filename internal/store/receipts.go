package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// StoreStatus is a migrated store's lifecycle state.
type StoreStatus string

const (
	// StoreImporting: the JSON source is authoritative; the import may be
	// resumed and nothing reads the store's tables.
	StoreImporting StoreStatus = "importing"
	// StoreLive: SQLite is authoritative and the JSON is a frozen backup.
	StoreLive StoreStatus = "live"
)

// MigratedStore is one row of migrated_stores.
type MigratedStore struct {
	Store         string
	Status        StoreStatus
	Source        string
	StartedAt     string
	CompletedAt   string
	BinaryVersion string
	Items         int
}

// ImportReceipt is one row of import_receipts. Source is the file path
// relative to the state directory, slash-separated.
type ImportReceipt struct {
	Store           string
	Source          string
	SHA256          string
	Size            int64
	MTimeNS         int64
	Companion       string
	CompanionSHA256 string
	ItemKey         string
	Outcome         string
	Detail          string
	ImportedAt      string
	BinaryVersion   string
}

const (
	OutcomeImported = "imported"
	OutcomeSkipped  = "skipped"
)

// MigratedStoreStatus reads one store's row; ok is false when the store has
// never started importing.
func (t *Tx) MigratedStoreStatus(store string) (MigratedStore, bool, error) {
	var record MigratedStore
	var completed sql.NullString
	err := t.QueryRow(`SELECT store, status, source, started_at, completed_at, binary_version, items FROM migrated_stores WHERE store = ?`, store).
		Scan(&record.Store, &record.Status, &record.Source, &record.StartedAt, &completed, &record.BinaryVersion, &record.Items)
	if errors.Is(err, sql.ErrNoRows) {
		return MigratedStore{}, false, nil
	}
	if err != nil {
		return MigratedStore{}, false, err
	}
	record.CompletedAt = completed.String
	return record, true, nil
}

// BeginStoreImport records that store is importing from source. It is a
// no-op for a store already importing from the same source and an error for
// a live store or a different source.
func (t *Tx) BeginStoreImport(store, source, binaryVersion string) error {
	store, source = strings.TrimSpace(store), strings.TrimSpace(source)
	if store == "" || source == "" {
		return errors.New("migrated store requires a name and a source")
	}
	existing, ok, err := t.MigratedStoreStatus(store)
	if err != nil {
		return err
	}
	if ok {
		if existing.Status == StoreLive {
			return fmt.Errorf("store %q is already live", store)
		}
		if existing.Source != source {
			return fmt.Errorf("store %q is importing from %q, not %q", store, existing.Source, source)
		}
		return nil
	}
	_, err = t.Exec(`INSERT INTO migrated_stores(store, status, source, started_at, binary_version) VALUES (?, ?, ?, ?, ?)`,
		store, StoreImporting, source, nowText(), binaryVersion)
	return err
}

// CompleteStoreImport flips an importing store to live.
func (t *Tx) CompleteStoreImport(store string, items int) error {
	result, err := t.Exec(`UPDATE migrated_stores SET status = ?, completed_at = ?, items = ? WHERE store = ? AND status = ?`,
		StoreLive, nowText(), items, store, StoreImporting)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("store %q is not importing", store)
	}
	return nil
}

// ImportReceiptFor reads the receipt for one source file.
func (t *Tx) ImportReceiptFor(store, source string) (ImportReceipt, bool, error) {
	var receipt ImportReceipt
	err := t.QueryRow(`SELECT store, source, sha256, size, mtime_ns, companion, companion_sha256, item_key, outcome, detail, imported_at, binary_version
		FROM import_receipts WHERE store = ? AND source = ?`, store, source).
		Scan(&receipt.Store, &receipt.Source, &receipt.SHA256, &receipt.Size, &receipt.MTimeNS, &receipt.Companion, &receipt.CompanionSHA256,
			&receipt.ItemKey, &receipt.Outcome, &receipt.Detail, &receipt.ImportedAt, &receipt.BinaryVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportReceipt{}, false, nil
	}
	if err != nil {
		return ImportReceipt{}, false, err
	}
	return receipt, true, nil
}

// ImportReceipts lists every receipt of one store, ordered by source.
func (t *Tx) ImportReceipts(store string) ([]ImportReceipt, error) {
	rows, err := t.Query(`SELECT store, source, sha256, size, mtime_ns, companion, companion_sha256, item_key, outcome, detail, imported_at, binary_version
		FROM import_receipts WHERE store = ? ORDER BY source`, store)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var receipts []ImportReceipt
	for rows.Next() {
		var receipt ImportReceipt
		if err := rows.Scan(&receipt.Store, &receipt.Source, &receipt.SHA256, &receipt.Size, &receipt.MTimeNS, &receipt.Companion, &receipt.CompanionSHA256,
			&receipt.ItemKey, &receipt.Outcome, &receipt.Detail, &receipt.ImportedAt, &receipt.BinaryVersion); err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

// PutImportReceipt inserts or replaces the receipt for receipt.Source. It
// belongs in the same transaction as the rows it vouches for.
func (t *Tx) PutImportReceipt(receipt ImportReceipt) error {
	if strings.TrimSpace(receipt.Store) == "" || strings.TrimSpace(receipt.Source) == "" || len(receipt.SHA256) != 64 {
		return errors.New("import receipt requires a store, a source and a sha256 digest")
	}
	if receipt.Outcome != OutcomeImported && receipt.Outcome != OutcomeSkipped {
		return fmt.Errorf("import receipt outcome %q is invalid", receipt.Outcome)
	}
	if receipt.ImportedAt == "" {
		receipt.ImportedAt = nowText()
	}
	_, err := t.Exec(`INSERT INTO import_receipts(store, source, sha256, size, mtime_ns, companion, companion_sha256, item_key, outcome, detail, imported_at, binary_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(store, source) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size, mtime_ns = excluded.mtime_ns,
			companion = excluded.companion, companion_sha256 = excluded.companion_sha256, item_key = excluded.item_key,
			outcome = excluded.outcome, detail = excluded.detail, imported_at = excluded.imported_at, binary_version = excluded.binary_version`,
		receipt.Store, receipt.Source, receipt.SHA256, receipt.Size, receipt.MTimeNS, receipt.Companion, receipt.CompanionSHA256,
		receipt.ItemKey, receipt.Outcome, receipt.Detail, receipt.ImportedAt, receipt.BinaryVersion)
	return err
}
