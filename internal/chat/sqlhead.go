package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// The actor head (chats.state_json) is the State's JSON. On a long chat it is
// megabytes: on a 62k-item chat 5 MB, of which the outbox is 3.2 MB and the
// background registry 1.4 MB. Every command that is not a journaled provider
// event rewrites it, and a send commits four such commands before its prompt
// reaches the provider.
//
// SQLite rewrites only the pages that differ when a row is replaced by one of
// exactly the same size (btreeOverwriteCell); a row of another size is freed
// and written again in full, 1,200+ WAL frames for a 5 MB head, which then
// pushes a WAL checkpoint into the commit. encodeSQLHead keeps the size and
// the unchanged prefix stable, so a checkpoint writes what changed:
//
//   - The fields that are largest and move least come first (the background
//     registry, then the outbox, which grows at its end). Every other field
//     follows in declaration order. A JSON object's member order carries no
//     meaning: every reader (encoding/json, SQLite's json functions, every
//     older Workass) decodes the same State.
//   - The JSON is followed by spaces up to a size bucket. Trailing whitespace
//     is valid JSON for the same readers, and a head that grows or shrinks
//     within its bucket keeps the row's size.
//
// updated_at, the only other column whose width can vary, is written with a
// fixed nine-digit fraction (sqlNow).
var sqlHeadLeadingFields = []string{"Background", "Outbox"}

type sqlHeadField struct {
	index     int
	key       []byte // `"Name":`
	omitEmpty bool
}

var (
	sqlHeadFieldsOnce sync.Once
	sqlHeadFields     []sqlHeadField
)

func sqlHeadFieldOrder() []sqlHeadField {
	sqlHeadFieldsOnce.Do(func() {
		typ := reflect.TypeFor[State]()
		var leading, rest []sqlHeadField
		for index := 0; index < typ.NumField(); index++ {
			field := typ.Field(index)
			if !field.IsExported() {
				continue
			}
			name := field.Name
			omitEmpty := false
			if tag, ok := field.Tag.Lookup("json"); ok {
				if tag == "-" {
					continue
				}
				parts := strings.Split(tag, ",")
				if parts[0] != "" {
					name = parts[0]
				}
				for _, option := range parts[1:] {
					if option == "omitempty" {
						omitEmpty = true
					}
				}
			}
			key, err := json.Marshal(name)
			if err != nil {
				panic(fmt.Sprintf("chat head field %s: %v", field.Name, err))
			}
			entry := sqlHeadField{index: index, key: append(key, ':'), omitEmpty: omitEmpty}
			if sqlHeadLeadingIndex(field.Name) >= 0 {
				leading = append(leading, entry)
			} else {
				rest = append(rest, entry)
			}
		}
		ordered := make([]sqlHeadField, len(sqlHeadLeadingFields))
		for _, entry := range leading {
			ordered[sqlHeadLeadingIndex(typ.Field(entry.index).Name)] = entry
		}
		sqlHeadFields = append(ordered, rest...)
	})
	return sqlHeadFields
}

func sqlHeadLeadingIndex(name string) int {
	for index, leading := range sqlHeadLeadingFields {
		if leading == name {
			return index
		}
	}
	return -1
}

// encodeSQLHead encodes the State without a non-empty Ledger (the rows hold
// it). An empty ledger stays in the head so nil and [] round-trip exactly.
func encodeSQLHead(state State) ([]byte, error) {
	return encodeSQLHeadSized(state, 0)
}

// encodeSQLHeadSized is encodeSQLHead with the size of this chat's previous
// head, so a large head is encoded into one buffer of the right size.
func encodeSQLHeadSized(state State, sizeHint int) ([]byte, error) {
	head := state
	if len(state.Ledger) > 0 {
		head.Ledger = nil
	}
	// Tool owners are chat_tools rows (sqltools.go).
	head.Tools = nil
	value := reflect.ValueOf(head)
	var out bytes.Buffer
	out.Grow(min(sizeHint, 256<<20) + 4096)
	encoder := json.NewEncoder(&out)
	out.WriteByte('{')
	first := true
	for _, field := range sqlHeadFieldOrder() {
		fieldValue := value.Field(field.index)
		if field.omitEmpty && fieldValue.IsZero() {
			continue
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(field.key)
		if err := encoder.Encode(fieldValue.Interface()); err != nil {
			return nil, fmt.Errorf("encode chat head %s: %w", reflect.TypeFor[State]().Field(field.index).Name, err)
		}
		// Encode ends every value with a newline.
		out.Truncate(out.Len() - 1)
	}
	out.WriteByte('}')
	padded := sqlHeadPaddedSize(out.Len())
	for out.Len() < padded {
		out.WriteByte(' ')
	}
	return out.Bytes(), nil
}

// sqlHeadPaddedSize is the size bucket a head of n bytes is stored in: a
// multiple of a power of two between 1/32 and 1/16 of n (at least 512 B), so
// the padding costs under 6.25% and a 5 MB head keeps its size across up to
// 256 KiB of growth.
func sqlHeadPaddedSize(n int) int {
	unit := 512
	for unit*32 < n {
		unit *= 2
	}
	return (n + unit - 1) / unit * unit
}
