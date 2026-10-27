//go:build darwin

package main

import "testing"

// macOS is unchanged: the raw store, its migration, reclaim and collection
// are on.
func TestMacOSUsesTheRawImageStore(t *testing.T) {
	if !sessionImageRawStoreSupported {
		t.Fatal("the raw store must be on for macOS")
	}
}
