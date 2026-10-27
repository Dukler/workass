package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	sqlite "modernc.org/sqlite"
)

// ScanTextReadOnly visits every text and blob value of every table in the
// SQLite file at path, opened read-only and without taking a lock. It is for
// files nothing writes any more (a migration backup), read by a caller that
// must know everything such a file references: a value split across overflow
// pages is read whole here, where a byte scan of the file could miss it.
func ScanTextReadOnly(ctx context.Context, path string, visit func(table string, value []byte) error) error {
	if strings.ContainsRune(path, '?') {
		return fmt.Errorf("database path %q contains '?'", path)
	}
	// A URI filename: '%' and '#' are escaped, everything else (spaces in
	// "Application Support", a Windows drive) is taken literally.
	uri := strings.NewReplacer("%", "%25", "#", "%23").Replace(filepath.ToSlash(path))
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}
	connector, err := sqlite.NewConnector("file:" + uri + "?mode=ro&immutable=1")
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	var tables []string
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, table := range tables {
		if err := scanTableText(ctx, db, table, visit); err != nil {
			return fmt.Errorf("scan %s: %w", table, err)
		}
	}
	return nil
}

func scanTableText(ctx context.Context, db *sql.DB, table string, visit func(string, []byte) error) error {
	rows, err := db.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for index := range values {
		pointers[index] = &values[index]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		for _, value := range values {
			switch typed := value.(type) {
			case string:
				if err := visit(table, []byte(typed)); err != nil {
					return err
				}
			case []byte:
				if err := visit(table, typed); err != nil {
					return err
				}
			}
		}
	}
	return rows.Err()
}
