package main

// The recovery command is intentionally a narrow, explicit escape hatch for a
// daemon that cannot boot.  It never runs during normal startup and it does not
// delete user data: a malformed startup record is moved aside beneath
// state/recovery/ first, so it can be inspected or restored later.

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type startupRepairReport struct {
	Moved []string
}

func repairStartupState(stateDir string) (startupRepairReport, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return startupRepairReport{}, errors.New("state directory is empty")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return startupRepairReport{}, err
	}
	report := startupRepairReport{}
	move := func(path string) error {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		recoveryDir := filepath.Join(stateDir, "recovery")
		if err := os.MkdirAll(recoveryDir, 0o700); err != nil {
			return err
		}
		base := filepath.Base(path)
		destination := filepath.Join(recoveryDir, fmt.Sprintf("%s.%s", base, time.Now().UTC().Format("20060102T150405.000000000Z")))
		if err := os.Rename(path, destination); err != nil {
			return fmt.Errorf("preserve %s: %w", base, err)
		}
		report.Moved = append(report.Moved, base)
		return nil
	}

	certPath := filepath.Join(stateDir, "daemon-cert.pem")
	keyPath := filepath.Join(stateDir, "daemon-key.pem")
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if (certErr == nil) != (keyErr == nil) {
		if err := move(certPath); err != nil {
			return report, err
		}
		if err := move(keyPath); err != nil {
			return report, err
		}
	} else if certErr == nil {
		if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
			if err := move(certPath); err != nil {
				return report, err
			}
			if err := move(keyPath); err != nil {
				return report, err
			}
		}
	} else if !errors.Is(certErr, os.ErrNotExist) {
		return report, certErr
	} else if !errors.Is(keyErr, os.ErrNotExist) {
		return report, keyErr
	}

	// These four JSON records are opened before the daemon can serve a health
	// response.  Preserve only syntactically malformed files; valid user data is
	// never rewritten by recovery.
	for _, name := range []string{"machine-id.json", "fleet.json", "machines.json", sessionStateFilename} {
		path := filepath.Join(stateDir, name)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return report, err
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			if err := move(path); err != nil {
				return report, err
			}
		}
	}
	return report, nil
}
