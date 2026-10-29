package headlessupdate

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxArchiveEntries = 120000
	maxExpandedBytes  = int64(12) << 30
)

// ExtractTarGZ expands one Workass headless release archive into root.
//
// The archive is untrusted data until its digest matched, and a release is not
// a place to accept surprises even after it does: absolute paths, `..`
// segments, symlinks, hard links, devices and FIFOs are all rejected rather
// than sanitized, so a malformed archive fails loudly instead of writing one
// file outside the staging tree.
func ExtractTarGZ(archive io.Reader, root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("extraction root must be absolute")
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return "", fmt.Errorf("update archive is not gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	reader := tar.NewReader(gz)
	entries := 0
	var expanded int64
	topLevel := ""
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("update archive is unreadable: %w", err)
		}
		entries++
		if entries > maxArchiveEntries {
			return "", errors.New("update archive has too many entries")
		}
		name := strings.TrimPrefix(filepath.ToSlash(header.Name), "./")
		if name == "" || name == "." {
			continue
		}
		if strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
			return "", fmt.Errorf("update archive entry is not relative: %s", header.Name)
		}
		parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
		for _, part := range parts {
			if part == ".." || part == "" || part == "." {
				return "", fmt.Errorf("update archive entry escapes its root: %s", header.Name)
			}
		}
		if topLevel == "" {
			topLevel = parts[0]
		} else if parts[0] != topLevel {
			return "", errors.New("update archive must contain exactly one top-level directory")
		}
		target := filepath.Join(root, filepath.Join(parts...))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			expanded += header.Size
			if expanded > maxExpandedBytes {
				return "", errors.New("update archive expands past its allowed size")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return "", err
			}
			written, copyErr := io.Copy(file, io.LimitReader(reader, header.Size+1))
			closeErr := file.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			if written != header.Size {
				return "", fmt.Errorf("update archive entry %s is truncated", header.Name)
			}
		default:
			return "", fmt.Errorf("update archive entry %s uses an unsupported type", header.Name)
		}
	}
	if topLevel == "" {
		return "", errors.New("update archive is empty")
	}
	return filepath.Join(root, topLevel), nil
}
