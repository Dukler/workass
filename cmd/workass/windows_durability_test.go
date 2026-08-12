//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsCutoverReceiptsCommitWithoutDirectorySyncFailure(t *testing.T) {
	stateDir := t.TempDir()
	cutoverPath := filepath.Join(stateDir, legacyChatCutoverReceiptFilename)
	receipt := legacyChatCutoverReceipt{
		Version: legacyChatCutoverVersion, Complete: true,
		CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), ChatIDs: []string{},
	}
	if err := writeLegacyChatCutoverReceipt(cutoverPath, receipt); err != nil {
		t.Fatalf("write Windows cutover receipt: %v", err)
	}
	if _, err := os.Stat(cutoverPath); err != nil {
		t.Fatalf("Windows cutover receipt is missing: %v", err)
	}
	cleanupPath := filepath.Join(stateDir, legacyChatCleanupReceiptFilename)
	cleanup := legacyChatCleanupReceipt{
		Version: legacyChatCleanupVersion, Complete: true,
		CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), CutoverDigest: legacyChatCutoverDigest(receipt),
	}
	if err := writeLegacyChatCleanupReceipt(cleanupPath, cleanup); err != nil {
		t.Fatalf("write Windows cleanup receipt: %v", err)
	}
	if _, err := os.Stat(cleanupPath); err != nil {
		t.Fatalf("Windows cleanup receipt is missing: %v", err)
	}
}
