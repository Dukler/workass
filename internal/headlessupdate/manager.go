package headlessupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Strategy names this activation mechanism in every receipt, the way
// "windows-zip" names the portable Windows one.
const Strategy = "linux-systemd-user"

// RestartExitCode is the exit status the daemon uses when it has already
// replaced its own files and wants systemd to bring the new binary up. It is
// deliberately a failure code: a `systemd --user` unit in the field is written
// with Restart=on-failure, and a clean exit there would leave the machine down.
const RestartExitCode = 75

// StagingDirName holds downloads, the staged tree and the rollback backup. It
// lives inside the install root so every rename is same-filesystem and
// therefore atomic.
const StagingDirName = ".workass-update"

const (
	checkTTL          = 60 * time.Second
	initialCheckDelay = 15 * time.Second
	checkInterval     = time.Hour
	restartGrace      = 1500 * time.Millisecond
	maxBootAttempts   = 3
	maxOperations     = 32
	maxPreflightWait  = 20 * time.Second
	maxOperationsSize = 256 * 1024
)

// operationIDPattern is the headless updater's own check on a caller-stable
// operation id. It accepts the SAME character set the product-wide contract
// accepts (provider.ValidateOperationID) and the Electron shell's own
// validator already accepted, which is what it should always have done: a
// stricter alphabet here meant one updater half rejected an id the other half
// took. `update-duklerver-0.1.237-20261030` was refused for its dots alone,
// and the refusal did not say so.
var operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/+=@-]{7,95}$`)

// operationIDRequirement is the format, in the refusal itself. A caller that
// is told only "invalid" has to guess which of length, alphabet and leading
// character it broke; a caller that is told the rule fixes it in one try.
const operationIDRequirement = "authorized update requires a valid caller-stable operation_id: " +
	"8 to 96 characters, starting with a letter or digit, from A-Z a-z 0-9 and _ . : / + = @ -"

// Options configures one Manager. Every external dependency is injectable so
// the activation contract is testable off a Linux service host.
type Options struct {
	Executable     string
	StateDir       string
	Version        string
	GOOS           string
	GOARCH         string
	FeedURL        string
	HTTPClient     *http.Client
	Env            func(string) string
	Run            func(name string, args ...string) ([]byte, error)
	ReadFile       func(string) ([]byte, error)
	Now            func() time.Time
	Log            func(format string, args ...any)
	RequestRestart func()
}

type resolvedOptions struct {
	executable     string
	stateDir       string
	version        string
	goos           string
	goarch         string
	feedURL        string
	client         *http.Client
	env            func(string) string
	run            func(name string, args ...string) ([]byte, error)
	readFile       func(string) ([]byte, error)
	now            func() time.Time
	log            func(format string, args ...any)
	requestRestart func()
}

// Receipt is the durable record of one activation. It survives the restart it
// causes, which is the whole point: the process that writes "activating" is
// not the process that reports "healthy".
type Receipt struct {
	SchemaVersion    int      `json:"schemaVersion"`
	Strategy         string   `json:"strategy"`
	UpdateID         string   `json:"updateId"`
	Unit             string   `json:"unit"`
	InstallTarget    string   `json:"installTarget"`
	PreviousVersion  string   `json:"previousVersion"`
	TargetVersion    string   `json:"targetVersion"`
	InstalledVersion string   `json:"installedVersion,omitempty"`
	Phase            string   `json:"phase"`
	Step             string   `json:"step"`
	Replaced         []string `json:"replaced,omitempty"`
	BackupRoot       string   `json:"backupRoot,omitempty"`
	BootAttempts     int      `json:"bootAttempts"`
	Activated        bool     `json:"activated"`
	UpdatedAt        string   `json:"updatedAt"`
	Error            string   `json:"error,omitempty"`
	RollbackError    string   `json:"rollbackError,omitempty"`
}

// AuthorizationReceipt is the replay key for one operation_id, identical in
// purpose to the Electron updater's agent-operations journal.
type AuthorizationReceipt struct {
	OperationID    string `json:"operationId"`
	MachineID      string `json:"machineId"`
	CurrentVersion string `json:"currentVersion"`
	TargetVersion  string `json:"targetVersion"`
	AcceptedAt     string `json:"acceptedAt"`
}

// ApplyRequest is the authorized activation a human ordered now for this exact
// machine. The fields and the authorization sentence match the Electron path
// exactly so one agent contract covers every platform.
type ApplyRequest struct {
	OperationID            string
	MachineID              string
	ExpectedCurrentVersion string
	ExpectedTargetVersion  string
	Authorization          string
}

// Manager owns this machine's headless update state.
type Manager struct {
	options     resolvedOptions
	installRoot string

	mu          sync.Mutex
	service     *Service
	blockers    []string
	unsupported string
	available   *Manifest
	checkedAt   time.Time
	checkError  string
	phase       string
	progress    int
	applying    bool
	lastError   string
}

// New resolves this install and returns a Manager. It never fails: an install
// that cannot update reports supported=false with the exact reason, because an
// agent asking "can this machine update" deserves an answer, not an error.
func New(options Options) *Manager {
	resolved := resolveOptions(options)
	manager := &Manager{options: resolved, phase: "idle"}
	manager.installRoot = filepath.Dir(resolved.executable)
	manager.refreshSupport()
	return manager
}

func resolveOptions(options Options) resolvedOptions {
	resolved := resolvedOptions{
		executable: strings.TrimSpace(options.Executable),
		stateDir:   strings.TrimSpace(options.StateDir),
		version:    strings.TrimSpace(options.Version),
		goos:       options.GOOS,
		goarch:     options.GOARCH,
		feedURL:    strings.TrimSpace(options.FeedURL),
		client:     options.HTTPClient,
		env:        options.Env,
		run:        options.Run,
		readFile:   options.ReadFile,
		now:        options.Now,
		log:        options.Log,
	}
	if resolved.goos == "" {
		resolved.goos = runtime.GOOS
	}
	if resolved.goarch == "" {
		resolved.goarch = runtime.GOARCH
	}
	if resolved.client == nil {
		resolved.client = &http.Client{Timeout: downloadTimeout}
	}
	if resolved.env == nil {
		resolved.env = os.Getenv
	}
	if resolved.readFile == nil {
		resolved.readFile = os.ReadFile
	}
	if resolved.now == nil {
		resolved.now = time.Now
	}
	if resolved.log == nil {
		resolved.log = func(string, ...any) {}
	}
	if resolved.run == nil {
		resolved.run = runCommand
	}
	if resolved.feedURL == "" {
		resolved.feedURL = strings.TrimSpace(resolved.env(FeedURLEnv))
	}
	resolved.requestRestart = options.RequestRestart
	return resolved
}

// Platform is the release platform token, matching the feed file name.
func (m *Manager) Platform() string { return m.options.goos }

// Arch is the release arch token: Go's amd64/arm64, as the feeds already use.
func (m *Manager) Arch() string { return m.options.goarch }

// InstallRoot is the directory the running daemon binary lives in and the only
// tree this updater ever replaces.
func (m *Manager) InstallRoot() string { return m.installRoot }

func (m *Manager) refreshSupport() {
	if m.options.goos != "linux" {
		m.setUnsupported("the headless daemon updater is supported only on Linux service installs", nil)
		return
	}
	if !filepath.IsAbs(m.options.executable) {
		m.setUnsupported("this daemon cannot resolve its own executable path", nil)
		return
	}
	if !ValidVersion(m.options.version) {
		m.setUnsupported(fmt.Sprintf("this daemon reports version %q, which is not a published release", m.options.version), nil)
		return
	}
	service, err := describeService(m.options)
	if err != nil {
		m.setUnsupported(err.Error(), nil)
		return
	}
	if err := writableDirectory(m.installRoot); err != nil {
		m.setUnsupported("this daemon cannot write its own install directory "+m.installRoot, nil)
		return
	}
	m.mu.Lock()
	m.service = service
	m.unsupported = ""
	m.blockers = nil
	m.mu.Unlock()
}

func (m *Manager) setUnsupported(reason string, blockers []string) {
	m.mu.Lock()
	m.unsupported = reason
	m.blockers = blockers
	m.phase = "unavailable"
	m.mu.Unlock()
}

// UnsupportedReason is the exact sentence an operator needs when this install
// cannot update itself. It is empty when it can.
func (m *Manager) UnsupportedReason() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unsupported
}

// Supported reports whether this install can activate its own updates.
func (m *Manager) Supported() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unsupported == ""
}

func (m *Manager) updateDir() string { return filepath.Join(m.installRoot, StagingDirName) }

func (m *Manager) receiptPath() string {
	return filepath.Join(m.options.stateDir, "headless-update", "receipt.json")
}

func (m *Manager) operationsPath() string {
	return filepath.Join(m.options.stateDir, "headless-update", "agent-operations.json")
}

// Check reads the release feed. A bounded cache keeps a chatty status tool from
// hitting GitHub on every poll while still noticing a new release quickly.
func (m *Manager) Check(ctx context.Context, force bool) (*Manifest, error) {
	m.mu.Lock()
	if !force && m.available != nil && m.options.now().Sub(m.checkedAt) < checkTTL {
		manifest := m.available
		m.mu.Unlock()
		return manifest, nil
	}
	m.mu.Unlock()

	target, err := FeedURL(m.options.feedURL, m.options.goos, m.options.goarch)
	if err != nil {
		m.recordCheckError(err)
		return nil, err
	}
	client := *m.options.client
	client.Timeout = feedTimeout
	raw, err := fetchWithContext(ctx, &client, target, maxManifestBytes)
	if err != nil {
		m.recordCheckError(err)
		return nil, err
	}
	manifest, err := ValidateManifest(raw, m.options.goos, m.options.goarch)
	if err != nil {
		m.recordCheckError(err)
		return nil, err
	}
	m.mu.Lock()
	m.available = manifest
	m.checkedAt = m.options.now()
	m.checkError = ""
	if !m.applying && m.unsupported == "" {
		if newer, cmpErr := CompareVersions(manifest.Version, m.options.version); cmpErr == nil && newer > 0 {
			m.phase = "available"
		} else if m.phase == "idle" || m.phase == "available" {
			m.phase = "idle"
		}
	}
	m.mu.Unlock()
	return manifest, nil
}

func (m *Manager) recordCheckError(err error) {
	m.mu.Lock()
	m.checkError = firstLine(err.Error())
	m.checkedAt = m.options.now()
	m.mu.Unlock()
}

// Run is the background discovery loop. Availability has to be discovered
// somewhere, and it is deliberately not in Status: the read tools promise they
// never check for an update, and that promise is worth keeping literally.
func (m *Manager) Run(ctx context.Context) {
	if !m.Supported() {
		return
	}
	timer := time.NewTimer(initialCheckDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := m.Check(ctx, true); err != nil && ctx.Err() == nil {
			m.options.log("[update] release feed check failed: %v", firstLine(err.Error()))
		}
		timer.Reset(checkInterval)
	}
}

// Status is the diagnostics document the update.status tool returns. Its shape
// mirrors UpdateManager.diagnostics() so one agent reads either platform. It
// is a pure read of cached state: no feed fetch, no download, no staging.
func (m *Manager) Status(_ context.Context) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	availableVersion := any(nil)
	targetVersion := any(nil)
	if m.available != nil {
		availableVersion = m.available.Version
		if newer, err := CompareVersions(m.available.Version, m.options.version); err == nil && newer > 0 {
			targetVersion = m.available.Version
		}
	}
	checkedAt := any(nil)
	if !m.checkedAt.IsZero() {
		checkedAt = m.checkedAt.UTC().Format(time.RFC3339)
	}
	errorText := any(nil)
	switch {
	case m.unsupported != "":
		errorText = m.unsupported
	case m.lastError != "":
		errorText = m.lastError
	case m.checkError != "":
		errorText = m.checkError
	}
	blockers := any(nil)
	if len(m.blockers) > 0 {
		blockers = m.blockers
	}
	state := map[string]any{
		"supported":        m.unsupported == "",
		"phase":            m.phase,
		"currentVersion":   m.options.version,
		"targetVersion":    targetVersion,
		"availableVersion": availableVersion,
		"checkedAt":        checkedAt,
		"progress":         m.progress,
		"error":            errorText,
		"blockers":         blockers,
	}
	service := any(nil)
	if m.service != nil {
		service = map[string]any{"unit": m.service.Unit, "restart": m.service.Restart, "user": m.service.User}
	}
	out := map[string]any{
		"schemaVersion": 1,
		"strategy":      Strategy,
		"platform":      m.options.goos,
		"arch":          m.options.goarch,
		"installRoot":   m.installRoot,
		"stateDir":      m.options.stateDir,
		"service":       service,
		"state":         state,
		"receipt":       receiptValue(m.readReceipt()),
		"replacedPaths": replacedPathNames(replacementPlan(m.options.goos, m.options.goarch)),
	}
	if operations := m.operations(); len(operations) > 0 {
		out["lastAgentAuthorization"] = operations[len(operations)-1]
	} else {
		out["lastAgentAuthorization"] = nil
	}
	return out
}

func receiptValue(receipt *Receipt) any {
	if receipt == nil {
		return nil
	}
	return receipt
}

// Apply activates the exact authorized target. It is the only method here that
// changes the machine, and it only ever runs from a human order relayed for
// this exact machine id, version pair and operation_id.
func (m *Manager) Apply(ctx context.Context, request ApplyRequest) (map[string]any, error) {
	operationID := strings.TrimSpace(request.OperationID)
	machineID := strings.TrimSpace(request.MachineID)
	currentVersion := strings.TrimSpace(request.ExpectedCurrentVersion)
	targetVersion := strings.TrimSpace(request.ExpectedTargetVersion)
	if !operationIDPattern.MatchString(operationID) {
		return nil, errors.New(operationIDRequirement)
	}
	if machineID == "" {
		return nil, errors.New("authorized update requires an exact machine_id")
	}
	if !ValidVersion(currentVersion) || !ValidVersion(targetVersion) {
		return nil, errors.New("authorized update requires exact X.Y.Z current and target versions")
	}
	expected := fmt.Sprintf("update %s from %s to %s", machineID, currentVersion, targetVersion)
	if strings.TrimSpace(request.Authorization) != expected {
		return nil, errors.New("authorized update text does not exactly match the machine and versions")
	}

	// Durable replay first: a lost reply must be able to re-ask without
	// starting a second activation.
	for _, existing := range m.operations() {
		if existing.OperationID != operationID {
			continue
		}
		if existing.MachineID != machineID || existing.CurrentVersion != currentVersion || existing.TargetVersion != targetVersion {
			return nil, errors.New("authorized update operation_id was reused for a different machine or version")
		}
		out := m.Status(ctx)
		out["authorizationReceipt"] = existing
		out["replayed"] = true
		return out, nil
	}

	if !m.Supported() {
		m.mu.Lock()
		reason := m.unsupported
		m.mu.Unlock()
		return nil, errors.New(reason)
	}
	if currentVersion != m.options.version {
		return nil, fmt.Errorf("Workass version changed: expected %s, found %s", currentVersion, m.options.version)
	}
	manifest, err := m.Check(ctx, true)
	if err != nil {
		return nil, err
	}
	if targetVersion != manifest.Version {
		return nil, fmt.Errorf("Workass update target changed: expected %s, found %s", targetVersion, manifest.Version)
	}
	if newer, err := CompareVersions(currentVersion, targetVersion); err != nil || newer >= 0 {
		return nil, errors.New("authorized update target is not newer than the installed version")
	}

	m.mu.Lock()
	if m.applying {
		m.mu.Unlock()
		return nil, errors.New("this machine is already activating an update")
	}
	m.applying = true
	m.phase = "downloading"
	m.progress = 0
	m.lastError = ""
	m.mu.Unlock()

	authorization := AuthorizationReceipt{
		OperationID:    operationID,
		MachineID:      machineID,
		CurrentVersion: currentVersion,
		TargetVersion:  targetVersion,
		AcceptedAt:     m.options.now().UTC().Format(time.RFC3339),
	}
	if err := m.rememberOperation(authorization); err != nil {
		m.failApply(err)
		return nil, err
	}

	if err := m.activate(ctx, manifest, operationID); err != nil {
		m.failApply(err)
		return nil, err
	}

	m.mu.Lock()
	m.applying = false
	m.phase = "activating"
	m.progress = 100
	m.mu.Unlock()

	// The reply has to reach the caller before the process goes away, so the
	// restart is scheduled rather than immediate. The receipt is already
	// durable: if the grace is lost to a crash, systemd restarts us anyway and
	// the boot arbiter settles exactly the same way.
	m.scheduleRestart()

	out := m.Status(context.Background())
	out["authorizationReceipt"] = authorization
	out["replayed"] = false
	return out, nil
}

func (m *Manager) failApply(err error) {
	m.mu.Lock()
	m.applying = false
	m.phase = "failed"
	m.lastError = firstLine(err.Error())
	m.mu.Unlock()
}

func (m *Manager) scheduleRestart() {
	restart := m.options.requestRestart
	if restart == nil {
		m.options.log("[update] activation staged but this daemon has no restart authority")
		return
	}
	go func() {
		time.Sleep(restartGrace)
		m.options.log("[update] replaced files are in place; exiting for systemd to start the new binary")
		restart()
	}()
}

func (m *Manager) operations() []AuthorizationReceipt {
	raw, err := os.ReadFile(m.operationsPath())
	if err != nil || len(raw) > maxOperationsSize {
		return nil
	}
	var stored struct {
		SchemaVersion int                    `json:"schemaVersion"`
		Operations    []AuthorizationReceipt `json:"operations"`
	}
	if json.Unmarshal(raw, &stored) != nil || stored.SchemaVersion != 1 {
		return nil
	}
	kept := make([]AuthorizationReceipt, 0, len(stored.Operations))
	for _, entry := range stored.Operations {
		if operationIDPattern.MatchString(entry.OperationID) {
			kept = append(kept, entry)
		}
	}
	if len(kept) > maxOperations {
		kept = kept[len(kept)-maxOperations:]
	}
	return kept
}

func (m *Manager) rememberOperation(receipt AuthorizationReceipt) error {
	operations := append(m.operations(), receipt)
	if len(operations) > maxOperations {
		operations = operations[len(operations)-maxOperations:]
	}
	return writeJSONAtomic(m.operationsPath(), map[string]any{"schemaVersion": 1, "operations": operations})
}

func (m *Manager) readReceipt() *Receipt {
	raw, err := os.ReadFile(m.receiptPath())
	if err != nil {
		return nil
	}
	var receipt Receipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.SchemaVersion != 1 {
		return nil
	}
	return &receipt
}

func (m *Manager) writeReceipt(receipt *Receipt) error {
	receipt.SchemaVersion = 1
	receipt.Strategy = Strategy
	receipt.UpdatedAt = m.options.now().UTC().Format(time.RFC3339)
	return writeJSONAtomic(m.receiptPath(), receipt)
}

func fetchWithContext(ctx context.Context, client *http.Client, target string, maxBytes int64) ([]byte, error) {
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := fetch(client, target, maxBytes)
		done <- result{body: body, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-done:
		return out.body, out.err
	}
}
