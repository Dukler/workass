// Package agenttext owns Workass-authored model instructions and tool guidance.
// Edit catalog.json for wording; callers only select and assemble entries.
package agenttext

import (
	_ "embed"
	"encoding/json"
)

//go:embed catalog.json
var catalogJSON []byte

var entries = func() map[string]string {
	var values map[string]string
	if err := json.Unmarshal(catalogJSON, &values); err != nil {
		panic(err)
	}
	return values
}()

// Get fails loudly for a programmer error rather than silently omitting policy.
func Get(key string) string {
	value, ok := entries[key]
	if !ok {
		panic("unknown agent text key: " + key)
	}
	return value
}
