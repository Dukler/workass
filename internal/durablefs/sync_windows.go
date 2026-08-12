//go:build windows

package durablefs

// SyncDirectory is intentionally a no-op on Windows. Go directory handles do
// not support File.Sync there; atomic replacement durability is provided by
// the filesystem rename boundary instead.
func SyncDirectory(string) error { return nil }
