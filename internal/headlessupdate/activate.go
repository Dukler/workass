package headlessupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workass/internal/durablefs"
)

// replacement is one path the release owns inside the install root.
type replacement struct {
	// Path is relative to the install root and to the archive's top-level
	// directory: one name, two trees, no translation table.
	Path string
	// OnlyIfMissing covers the bundled Node runtime. A hand-provisioned host
	// may hold a fuller Node than the release prunes to (the one on duklerver
	// keeps npm, which is how Pi and Codex were installed beside it), and an
	// update is not a licence to take an operator's tool away. The release
	// still carries Node so a first install from the release alone is complete.
	OnlyIfMissing bool
	// Required rejects an archive that is missing the daemon itself.
	Required bool
}

func replacementPlan(platform, arch string) []replacement {
	target := platform + "-" + arch
	return []replacement{
		{Path: "workass", Required: true},
		{Path: "workass-tools"},
		{Path: "workass-agent"},
		{Path: "frontier-hosts/" + target},
		{Path: "manifest.json"},
		{Path: "node/" + target, OnlyIfMissing: true},
	}
}

func replacedPathNames(plan []replacement) []string {
	names := make([]string, 0, len(plan))
	for _, entry := range plan {
		name := entry.Path
		if entry.OnlyIfMissing {
			name += " (only when absent)"
		}
		names = append(names, name)
	}
	return names
}

// activate downloads, verifies, preflights and swaps the release in. It leaves
// the process running and a durable `activating` receipt behind; the restart is
// the caller's, and the verdict is the next boot's.
func (m *Manager) activate(ctx context.Context, manifest *Manifest, updateID string) error {
	plan := replacementPlan(m.options.goos, m.options.goarch)
	root := m.updateDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// A previous attempt's scratch is never reused: it is the one place a
	// half-written tree could survive a crash. Older rollback backups go too,
	// so the staging directory holds exactly one previous version rather than
	// one per release ever installed.
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		name := entry.Name()
		stale := name == "download" || name == "staged" || name == "incoming-backup" ||
			(strings.HasPrefix(name, "backup-") && name != "backup-"+m.options.version)
		if !stale {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	downloadRoot := filepath.Join(root, "download")
	if err := os.MkdirAll(downloadRoot, 0o755); err != nil {
		return err
	}
	archivePath := filepath.Join(downloadRoot, manifest.Artifacts.Update.Name)
	if filepath.Dir(archivePath) != downloadRoot {
		return errors.New("release artifact name is not a plain file name")
	}

	m.setStep("downloading", 10)
	file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	digest, _, downloadErr := downloadArtifactContext(ctx, m.options.client, manifest.Artifacts.Update, file)
	closeErr := file.Close()
	if downloadErr != nil {
		return downloadErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !strings.EqualFold(digest, manifest.Artifacts.Update.SHA256) {
		return fmt.Errorf("update download digest %s does not match the manifest", digest)
	}

	m.setStep("verifying", 40)
	stagedRoot := filepath.Join(root, "staged")
	if err := os.MkdirAll(stagedRoot, 0o755); err != nil {
		return err
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	stagedTree, extractErr := ExtractTarGZ(archive, stagedRoot)
	_ = archive.Close()
	if extractErr != nil {
		return extractErr
	}
	if err := m.verifyStagedTree(stagedTree, manifest, plan); err != nil {
		return err
	}

	m.setStep("activating", 70)
	backupRoot := filepath.Join(root, "backup-"+m.options.version)
	if err := os.RemoveAll(backupRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		return err
	}
	replaced, swapErr := m.swapIn(stagedTree, backupRoot, plan)
	if swapErr != nil {
		// Nothing is ever left half-replaced: the same backup rolls the
		// already-moved entries straight back before the error returns.
		rollbackErr := m.restore(backupRoot, replaced)
		return errors.Join(swapErr, rollbackErr)
	}

	receipt := &Receipt{
		UpdateID:        updateID,
		InstallTarget:   m.installRoot,
		PreviousVersion: m.options.version,
		TargetVersion:   manifest.Version,
		Phase:           "activating",
		Step:            "awaiting-service-restart",
		Replaced:        replaced,
		BackupRoot:      backupRoot,
		BootAttempts:    0,
	}
	if m.service != nil {
		receipt.Unit = m.service.Unit
	}
	if err := m.writeReceipt(receipt); err != nil {
		rollbackErr := m.restore(backupRoot, replaced)
		return errors.Join(err, rollbackErr)
	}
	return nil
}

func (m *Manager) setStep(phase string, progress int) {
	m.mu.Lock()
	m.phase = phase
	m.progress = progress
	m.mu.Unlock()
}

// verifyStagedTree refuses an archive that is not this release before anything
// is moved. Running the staged daemon's own `version` is the cheap proof that
// matters: a truncated download, the wrong architecture or a missing loader
// all fail here, where failing costs nothing.
func (m *Manager) verifyStagedTree(stagedTree string, manifest *Manifest, plan []replacement) error {
	for _, entry := range plan {
		if !entry.Required {
			continue
		}
		info, err := os.Stat(filepath.Join(stagedTree, filepath.FromSlash(entry.Path)))
		if err != nil || info.IsDir() {
			return fmt.Errorf("release archive is missing %s", entry.Path)
		}
	}
	daemon := filepath.Join(stagedTree, "workass")
	if err := os.Chmod(daemon, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), maxPreflightWait)
	defer cancel()
	command := exec.CommandContext(ctx, daemon, "version")
	command.Env = append(os.Environ(), "WORKASS_DISABLE_BEACON=1")
	out, err := command.Output()
	if err != nil {
		return fmt.Errorf("the staged Workass daemon does not run on this machine: %s", firstLine(err.Error()))
	}
	reported := ""
	var payload struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(out, &payload) == nil {
		reported = strings.TrimSpace(payload.Version)
	}
	if reported == "" {
		reported = firstLine(string(out))
	}
	if reported != manifest.Version {
		return fmt.Errorf("the staged Workass daemon reports %q, not the %s its manifest binds", reported, manifest.Version)
	}
	return nil
}

// swapIn moves the current entries aside and the staged ones in. Every move is
// a same-filesystem rename, so no path is ever observed half-written, and
// replacing a file that is currently executing is safe on Linux because the
// running image holds the old inode.
func (m *Manager) swapIn(stagedTree, backupRoot string, plan []replacement) ([]string, error) {
	replaced := []string(nil)
	for _, entry := range plan {
		relative := filepath.FromSlash(entry.Path)
		source := filepath.Join(stagedTree, relative)
		if _, err := os.Lstat(source); err != nil {
			if entry.Required {
				return replaced, fmt.Errorf("release archive is missing %s", entry.Path)
			}
			continue
		}
		destination := filepath.Join(m.installRoot, relative)
		_, existsErr := os.Lstat(destination)
		if entry.OnlyIfMissing && existsErr == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return replaced, err
		}
		if existsErr == nil {
			backup := filepath.Join(backupRoot, relative)
			if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
				return replaced, err
			}
			if err := os.Rename(destination, backup); err != nil {
				return replaced, err
			}
		}
		if err := os.Rename(source, destination); err != nil {
			return replaced, err
		}
		_ = durablefs.SyncDirectory(filepath.Dir(destination))
		replaced = append(replaced, entry.Path)
	}
	sort.Strings(replaced)
	return replaced, nil
}

// restore puts the backup back. It is the rollback, and it is deliberately the
// exact inverse of swapIn rather than a second download.
func (m *Manager) restore(backupRoot string, replaced []string) error {
	errs := []error(nil)
	for _, name := range replaced {
		relative := filepath.FromSlash(name)
		backup := filepath.Join(backupRoot, relative)
		if _, err := os.Lstat(backup); err != nil {
			continue
		}
		destination := filepath.Join(m.installRoot, relative)
		if err := os.RemoveAll(destination); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(backup, destination); err != nil {
			errs = append(errs, err)
			continue
		}
		_ = durablefs.SyncDirectory(filepath.Dir(destination))
	}
	return errors.Join(errs...)
}

// Settle is the boot arbiter. It runs before this daemon listens, reads the
// receipt its predecessor left, and decides whether this boot is the new
// version arriving, the swap having failed, or a crash loop that has to go
// back. It returns true when the caller must exit for systemd to restart the
// restored binary.
func (m *Manager) Settle() (restartRequired bool) {
	receipt := m.readReceipt()
	if receipt == nil {
		return false
	}
	switch receipt.Phase {
	case "activating", "activated":
	default:
		return false
	}
	receipt.BootAttempts++
	switch {
	case m.options.version == receipt.TargetVersion && receipt.BootAttempts <= maxBootAttempts:
		// The new binary is running but has not served a request yet. Confirm
		// turns this into "healthy"; a crash before that lands here again with
		// a higher attempt count.
		receipt.Phase = "activated"
		receipt.Step = "awaiting-health"
		receipt.InstalledVersion = m.options.version
		receipt.Activated = true
		if err := m.writeReceipt(receipt); err != nil {
			m.options.log("[update] could not record the activation boot: %v", err)
		}
		m.mu.Lock()
		m.phase = "activated"
		m.mu.Unlock()
		m.options.log("[update] booted %s from %s; confirming health", receipt.TargetVersion, receipt.PreviousVersion)
		return false
	case m.options.version == receipt.PreviousVersion && receipt.BootAttempts <= 1:
		// The files were replaced and systemd still started the old image.
		// Nothing to roll back; say so instead of guessing.
		receipt.Phase = "failed"
		receipt.Step = "service-started-previous-version"
		receipt.InstalledVersion = m.options.version
		receipt.Activated = false
		receipt.Error = "the service restarted on " + receipt.PreviousVersion + " after the update was staged"
		_ = m.writeReceipt(receipt)
		m.setApplyFailure(receipt.Error)
		return false
	default:
		rollbackErr := m.restore(receipt.BackupRoot, receipt.Replaced)
		receipt.Phase = "rolled_back"
		receipt.Step = "restored-previous-version"
		receipt.InstalledVersion = receipt.PreviousVersion
		receipt.Activated = false
		receipt.Error = fmt.Sprintf("%s did not become healthy in %d boots", receipt.TargetVersion, receipt.BootAttempts-1)
		if rollbackErr != nil {
			receipt.RollbackError = firstLine(rollbackErr.Error())
		}
		_ = m.writeReceipt(receipt)
		m.setApplyFailure(receipt.Error)
		m.options.log("[update] rolled back to %s: %s", receipt.PreviousVersion, receipt.Error)
		return rollbackErr == nil
	}
}

func (m *Manager) setApplyFailure(reason string) {
	m.mu.Lock()
	m.phase = "failed"
	m.lastError = reason
	m.mu.Unlock()
}

// Confirm is called once this daemon is actually serving. It is the health
// gate: until it runs, the receipt says "activated", and a boot that never
// reaches it is a boot that gets rolled back.
func (m *Manager) Confirm() {
	receipt := m.readReceipt()
	if receipt == nil || receipt.Phase != "activated" {
		return
	}
	if m.options.version != receipt.TargetVersion {
		return
	}
	receipt.Phase = "healthy"
	receipt.Step = "serving"
	receipt.InstalledVersion = m.options.version
	receipt.Activated = true
	receipt.Error = ""
	if err := m.writeReceipt(receipt); err != nil {
		m.options.log("[update] could not record the healthy activation: %v", err)
		return
	}
	m.mu.Lock()
	m.phase = "installed"
	m.progress = 100
	m.mu.Unlock()
	m.options.log("[update] %s is serving; previous version kept in %s", m.options.version, receipt.BackupRoot)
	m.pruneStaging(receipt.BackupRoot)
}

// pruneStaging drops the downloads and the staged tree but keeps exactly one
// backup: the rollback for the version now running.
func (m *Manager) pruneStaging(keep string) {
	entries, err := os.ReadDir(m.updateDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		full := filepath.Join(m.updateDir(), entry.Name())
		if full == keep {
			continue
		}
		if err := os.RemoveAll(full); err != nil {
			m.options.log("[update] could not clear %s: %v", full, err)
		}
	}
}

func downloadArtifactContext(ctx context.Context, client *http.Client, artifact Artifact, writer io.Writer) (string, int64, error) {
	type result struct {
		digest string
		size   int64
		err    error
	}
	done := make(chan result, 1)
	go func() {
		digest, size, err := downloadArtifact(client, artifact, writer)
		done <- result{digest: digest, size: size, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", 0, ctx.Err()
	case out := <-done:
		return out.digest, out.size, out.err
	}
}

func runCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

func writeJSONAtomic(file string, value any) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	incoming := fmt.Sprintf("%s.incoming.%d", file, os.Getpid())
	if err := os.WriteFile(incoming, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(incoming, file); err != nil {
		_ = os.Remove(incoming)
		return err
	}
	return durablefs.SyncDirectory(filepath.Dir(file))
}
