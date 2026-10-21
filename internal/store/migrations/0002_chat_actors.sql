-- Durable chat actors, formerly provider-chats/<sha256>.json + .events.
--
-- chats.state_json is the actor's chat.State in the same JSON encoding the
-- file envelope used, minus its ledger whenever the ledger has rows.
-- envelope_version mirrors the envelope's "v", so an older build keeps
-- failing closed on a newer actor shape. revision is the actor revision of
-- the last checkpoint; ledger_len is how many messages rows it owns.
CREATE TABLE chats (
	chat_id          TEXT PRIMARY KEY,
	envelope_version INTEGER NOT NULL,
	revision         INTEGER NOT NULL CHECK (revision >= 0),
	state_json       TEXT NOT NULL,
	ledger_len       INTEGER NOT NULL DEFAULT 0 CHECK (ledger_len >= 0),
	journal_bytes    INTEGER NOT NULL DEFAULT 0,
	updated_at       TEXT NOT NULL
);

-- The actor ledger, one row per chat.LedgerEvent. seq is the 1-based ledger
-- position (the reducer's Ledger[seq-1]); event_json is the event in its
-- actor JSON encoding and is authoritative. message_id and role are copied
-- out for paging and inspection. Rows keep the import streaming: a 300 MB
-- actor is written one message at a time, never held whole.
CREATE TABLE messages (
	chat_id    TEXT NOT NULL REFERENCES chats(chat_id) ON DELETE CASCADE,
	seq        INTEGER NOT NULL CHECK (seq >= 1),
	message_id TEXT NOT NULL,
	role       TEXT NOT NULL,
	event_json TEXT NOT NULL,
	PRIMARY KEY (chat_id, seq)
);

-- Replaces the .events journal: each row is one journal record (the JSON
-- payload a file frame carried) committed after the checkpoint. A checkpoint
-- deletes the chat's rows in the same transaction.
CREATE TABLE chat_events (
	chat_id       TEXT NOT NULL REFERENCES chats(chat_id) ON DELETE CASCADE,
	revision      INTEGER NOT NULL,
	base_revision INTEGER NOT NULL,
	record        TEXT NOT NULL,
	PRIMARY KEY (chat_id, revision),
	CHECK (base_revision + 1 = revision)
);
