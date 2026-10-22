package chat

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"

	"workass/internal/store"
)

// Tool owners in SQLite (migration 0005).
//
// State.Tools maps every tool call an actor has seen to the activity that owns
// it; a long chat holds tens of thousands. They live in chat_tools, one row
// per tool call, outside the head, so a checkpoint writes the owners that
// changed instead of re-encoding them all: on a 1,434-message chat the head
// was 19 MB, 14 MB of it owners, rewritten by every command.
//
// The store handle's cache keeps an exact copy of the owners it last read or
// wrote, next to its ledger rows and under the same generation, so the two
// are trusted or distrusted together.

// sqlToolRow is one chat_tools row to write.
type sqlToolRow struct {
	id    string
	owner []byte
}

// sqlToolsPlan is the chat_tools change one checkpoint writes.
type sqlToolsPlan struct {
	full    bool
	upserts []sqlToolRow
	deletes []string
}

// planSQLTools works out, before the write transaction, which owner rows a
// checkpoint writes: every one when full (no trusted copy of the stored
// rows), else those added or changed since stored, and the removed ones.
func planSQLTools(stored, tools map[string]ToolState, full bool) (sqlToolsPlan, error) {
	plan := sqlToolsPlan{full: full}
	ids := make([]string, 0)
	for id, owner := range tools {
		if !full {
			if previous, ok := stored[id]; ok && previous == owner {
				continue
			}
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	plan.upserts = make([]sqlToolRow, 0, len(ids))
	for _, id := range ids {
		raw, err := json.Marshal(tools[id])
		if err != nil {
			return sqlToolsPlan{}, err
		}
		plan.upserts = append(plan.upserts, sqlToolRow{id: id, owner: raw})
	}
	if !full {
		for id := range stored {
			if _, ok := tools[id]; !ok {
				plan.deletes = append(plan.deletes, id)
			}
		}
		sort.Strings(plan.deletes)
	}
	return plan, nil
}

func (p sqlToolsPlan) apply(tx *store.Tx, chatID string) error {
	if p.full {
		if _, err := tx.Exec(`DELETE FROM chat_tools WHERE chat_id = ?`, chatID); err != nil {
			return err
		}
	}
	for _, id := range p.deletes {
		if _, err := tx.Exec(`DELETE FROM chat_tools WHERE chat_id = ? AND tool_id = ?`, chatID, id); err != nil {
			return err
		}
	}
	if len(p.upserts) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO chat_tools(chat_id, tool_id, owner_json) VALUES (?, ?, ?)
		ON CONFLICT(chat_id, tool_id) DO UPDATE SET owner_json = excluded.owner_json`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range p.upserts {
		if _, err := stmt.Exec(chatID, row.id, sqlText(row.owner)); err != nil {
			return err
		}
	}
	return nil
}

// nextToolsCache is the stored owners after a committed plan: the previous
// copy with the plan's rows applied, or a fresh copy after a full plan.
func (p sqlToolsPlan) nextToolsCache(previous, tools map[string]ToolState) map[string]ToolState {
	if p.full || previous == nil {
		return maps.Clone(tools)
	}
	for _, id := range p.deletes {
		delete(previous, id)
	}
	for _, row := range p.upserts {
		previous[row.id] = tools[row.id]
	}
	return previous
}

// readSQLTools copies a chat's owner rows; decodeSQLTools decodes them after
// the read, so decoding never holds the connection.
func readSQLTools(tx *store.Tx, chatID string) ([]sqlToolRow, error) {
	rows, err := tx.Query(`SELECT tool_id, owner_json FROM chat_tools WHERE chat_id = ?`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sqlToolRow
	for rows.Next() {
		var row sqlToolRow
		if err := rows.Scan(&row.id, &row.owner); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func decodeSQLTools(rows []sqlToolRow) (map[string]ToolState, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	tools := make(map[string]ToolState, len(rows))
	for index := range rows {
		var owner ToolState
		if err := json.Unmarshal(rows[index].owner, &owner); err != nil {
			return nil, fmt.Errorf("decode chat tool owner %q: %w", rows[index].id, err)
		}
		tools[rows[index].id] = owner
		rows[index].owner = nil
	}
	return tools, nil
}

// withSQLTools puts the stored owners into a decoded head, keeping a copy of
// them as stored (journal replay may change the head's map afterwards).
func withSQLTools(state *State, rows []sqlToolRow) (map[string]ToolState, error) {
	tools, err := decodeSQLTools(rows)
	if err != nil {
		return nil, err
	}
	if len(state.Tools) > 0 {
		return nil, fmt.Errorf("chat %q keeps tool owners in its head and in chat_tools", state.ChatID)
	}
	if tools != nil {
		state.Tools = tools
	}
	return maps.Clone(tools), nil
}

// writeSQLTools replaces a chat's owner rows wholesale.
func writeSQLTools(tx *store.Tx, chatID string, tools map[string]ToolState) error {
	plan, err := planSQLTools(nil, tools, true)
	if err != nil {
		return err
	}
	return plan.apply(tx, chatID)
}
