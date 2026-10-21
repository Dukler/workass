-- Store-level bookkeeping shared by every migrated domain store.
--
-- migrated_stores makes a half-finished JSON -> SQLite import explicit: a
-- store is 'importing' until every source file has a receipt, then 'live'.
-- Only a 'live' store is read from SQLite; its JSON stays on disk as a
-- never-rewritten backup.
CREATE TABLE migrated_stores (
	store          TEXT PRIMARY KEY,
	status         TEXT NOT NULL CHECK (status IN ('importing', 'live')),
	source         TEXT NOT NULL,
	started_at     TEXT NOT NULL,
	completed_at   TEXT,
	binary_version TEXT NOT NULL,
	items          INTEGER NOT NULL DEFAULT 0
);

-- One row per imported source file. sha256 is the receipt identity: a file
-- whose digest matches is never imported twice. size and mtime_ns let a live
-- store notice, cheaply, that a JSON backup was written after the cutover.
CREATE TABLE import_receipts (
	store            TEXT NOT NULL,
	source           TEXT NOT NULL,
	sha256           TEXT NOT NULL,
	size             INTEGER NOT NULL,
	mtime_ns         INTEGER NOT NULL,
	companion        TEXT NOT NULL DEFAULT '',
	companion_sha256 TEXT NOT NULL DEFAULT '',
	item_key         TEXT NOT NULL,
	outcome          TEXT NOT NULL CHECK (outcome IN ('imported', 'skipped')),
	detail           TEXT NOT NULL DEFAULT '',
	imported_at      TEXT NOT NULL,
	binary_version   TEXT NOT NULL,
	PRIMARY KEY (store, source)
);
