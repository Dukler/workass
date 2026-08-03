package acp

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func parseTasklistRSSKB(output string, pid int) (int, error) {
	reader := csv.NewReader(strings.NewReader(output))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	wantPID := pidString(pid)
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("parse tasklist csv: %w", err)
		}
		if len(record) == 0 {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(record[0]), "INFO:") {
			continue
		}
		if len(record) < 5 || strings.EqualFold(record[0], "Image Name") {
			continue
		}
		if strings.TrimSpace(record[1]) != wantPID {
			continue
		}
		return parseTasklistMemKB(record[4])
	}
	return 0, fmt.Errorf("tasklist did not report pid %d", pid)
}

func parseTasklistMemKB(value string) (int, error) {
	var digits strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	raw := digits.String()
	if raw == "" {
		return 0, fmt.Errorf("parse tasklist memory %q: no digits", value)
	}
	kb, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse tasklist memory %q: %w", value, err)
	}
	return kb, nil
}
