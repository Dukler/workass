package acp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// visitJSONLRecords reads complete physical JSONL records without bufio.Scanner's
// token ceiling. Workass archive rows may legitimately contain the bounded
// inline raster media allowed by the ACP contract, so a record can be larger
// than the old 4 MiB scanner limit. The callback is synchronous and the record
// buffer is released before the next line is read.
func visitJSONLRecords(r io.Reader, visit func([]byte)) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			visit(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
