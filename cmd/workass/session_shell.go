package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"

	"workass/internal/wire"
)

// sessionShellReply answers session:get {shell}. The shell names its own
// content hash (etag) so a client that kept the last shell it read — a page
// reload, a reconnect — can send it back as {ifNoneMatch} and get a tiny
// {unchanged, etag} reply while nothing changed, as state:digest already does.
// The projection is deterministic (sorted keys, no clocks), so an unchanged
// actor state always hashes the same. A read without shell never reaches here
// and keeps its byte-identical reply.
func sessionShellReply(raw []byte, ifNoneMatch string) wire.EncodedResult {
	sum := sha256.Sum256(raw)
	etag := hex.EncodeToString(sum[:])
	if ifNoneMatch != "" && ifNoneMatch == etag {
		return wire.EncodedResult(`{"etag":"` + etag + `","unchanged":true}`)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return wire.EncodedResult(raw)
	}
	out := make([]byte, 0, len(trimmed)+len(etag)+12)
	out = append(out, `{"etag":"`...)
	out = append(out, etag...)
	out = append(out, '"')
	if rest := bytes.TrimSpace(trimmed[1:]); len(rest) > 0 && rest[0] != '}' {
		out = append(out, ',')
	}
	out = append(out, trimmed[1:]...)
	return wire.EncodedResult(out)
}
