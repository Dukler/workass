package main

import (
	"os"
	"path/filepath"
	"strings"
)

// The release gate (scripts/test-suite.mjs) puts every test's temp dir on a
// scratch APFS volume it mounts under /Volumes. The clone guard refuses all of
// /Volumes, so tests that clone real files would see their own fixtures as
// protected; exempt exactly that one volume, never /Volumes as a whole.
func init() {
	dir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return
	}
	rest, ok := strings.CutPrefix(filepath.ToSlash(dir), "/Volumes/")
	if !ok {
		return
	}
	if name, _, _ := strings.Cut(rest, "/"); name != "" {
		sessionImageExemptRoots = append(sessionImageExemptRoots, "/Volumes/"+name)
	}
}
