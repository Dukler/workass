// Package appinstall installs a downloaded Windows release without rollback or
// provider/UI health probes. The running shell owns download and authorization;
// this command owns only the explicit stop -> replace -> launch handoff.
package appinstall

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const strategy = "windows-zip"
const inventoryName = ".workass-release-files.json"
const identityName = ".workass-installation.json"
const maxEntries = 120000
const maxExpandedBytes = uint64(12 << 30)

type interruptedWork struct {
	ForegroundTurns   int  `json:"foregroundTurns"`
	BackgroundWork    int  `json:"backgroundWork"`
	ProviderUpdates   int  `json:"providerUpdates"`
	Admissions        int  `json:"admissions"`
	DaemonUnavailable bool `json:"daemonUnavailable,omitempty"`
}

type Plan struct {
	WorkerID        string          `json:"workerId,omitempty"` // Only used by the older shell's progress receipt.
	SchemaVersion   int             `json:"schemaVersion"`
	Strategy        string          `json:"strategy"`
	UpdateID        string          `json:"updateId"`
	InstallationID  string          `json:"installationId"`
	CurrentVersion  string          `json:"currentVersion"`
	TargetVersion   string          `json:"targetVersion"`
	InstallTarget   string          `json:"installTarget"`
	DataRoot        string          `json:"dataRoot"`
	TransactionRoot string          `json:"transactionRoot"`
	ShellPID        int             `json:"shellPID"`
	InterruptedWork interruptedWork `json:"interruptedWork"`
}

type receipt struct {
	WorkerID         string          `json:"workerId,omitempty"`
	WorkerPID        int             `json:"workerPID"`
	SchemaVersion    int             `json:"schemaVersion"`
	Strategy         string          `json:"strategy"`
	UpdateID         string          `json:"updateId"`
	InstallationID   string          `json:"installationId"`
	InstallTarget    string          `json:"installTarget"`
	PreviousVersion  string          `json:"previousVersion"`
	TargetVersion    string          `json:"targetVersion"`
	InstalledVersion string          `json:"installedVersion,omitempty"`
	Phase            string          `json:"phase"`
	Step             string          `json:"step"`
	UpdatedAt        string          `json:"updatedAt"`
	Activated        bool            `json:"activated"`
	Error            string          `json:"error,omitempty"`
	InterruptedWork  interruptedWork `json:"interruptedWork"`
}

type inventory struct {
	SchemaVersion  int      `json:"schemaVersion"`
	InstallationID string   `json:"installationId"`
	Files          []string `json:"files"`
}

var updateIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,96}$`)
var identityPattern = regexp.MustCompile(`^install-[a-f0-9]{32}$`)
var versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func readJSON(file string, limit int64, value any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return errors.New("update metadata exceeds its size limit")
	}
	return json.Unmarshal(b, value)
}

func atomicJSON(file string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".install-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Retry only this atomic metadata write if a Windows scanner briefly holds
	// the destination. Never retry the installation or file replacement.
	for attempt := 0; ; attempt++ {
		err = os.Rename(f.Name(), file)
		if err == nil || attempt == 19 || !retryableRename(err) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func within(parent, child string) bool {
	r, err := filepath.Rel(strings.ToLower(parent), strings.ToLower(child))
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

func (p Plan) validate(planPath, executable string) error {
	if p.SchemaVersion != 1 || p.Strategy != strategy || !updateIDPattern.MatchString(p.UpdateID) ||
		!identityPattern.MatchString(p.InstallationID) || !versionPattern.MatchString(p.CurrentVersion) ||
		!versionPattern.MatchString(p.TargetVersion) || p.ShellPID < 2 {
		return errors.New("invalid Windows install request")
	}
	for _, dir := range []string{p.InstallTarget, p.DataRoot, p.TransactionRoot} {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || filepath.Dir(dir) == dir {
			return errors.New("installation paths must be absolute non-root directories")
		}
		actual, err := filepath.EvalSymlinks(dir)
		if err != nil || !strings.EqualFold(actual, dir) {
			return errors.New("installation paths must not traverse links")
		}
	}
	if within(p.InstallTarget, p.DataRoot) || within(p.DataRoot, p.InstallTarget) {
		return errors.New("application and mutable data directories must be separate")
	}
	if p.TransactionRoot != filepath.Join(p.DataRoot, "updates", "transactions", p.UpdateID) ||
		planPath != filepath.Join(p.TransactionRoot, "native-install.json") ||
		!strings.EqualFold(executable, filepath.Join(p.TransactionRoot, "incoming-release", "workass-daemon.exe")) {
		return errors.New("installer must run from this exact downloaded release")
	}
	var identity struct {
		SchemaVersion  int    `json:"schemaVersion"`
		Product        string `json:"product"`
		InstallationID string `json:"installationId"`
	}
	if err := readJSON(filepath.Join(p.InstallTarget, identityName), 4096, &identity); err != nil {
		return err
	}
	if identity.SchemaVersion != 1 || identity.Product != "Workass" || identity.InstallationID != p.InstallationID {
		return errors.New("installation identity changed before update")
	}
	return nil
}

// Reject Windows aliases even when running archive tests on another OS. No
// alternate streams, reserved devices, links, absolute paths, or parent walks.
func safeName(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\:\x00<>\"|?*\r\n") || strings.HasPrefix(name, "/") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.TrimRight(part, ". ") != part {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
			(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9') {
			return false
		}
	}
	return !strings.EqualFold(name, inventoryName) && !strings.EqualFold(name, identityName)
}

type payload struct {
	archive     *zip.ReadCloser
	files       map[string]*zip.File
	names       []string
	directories []string
}

func openPayload(p Plan) (*payload, error) {
	z, err := zip.OpenReader(filepath.Join(p.TransactionRoot, "release.zip"))
	if err != nil {
		return nil, err
	}
	out := &payload{archive: z, files: make(map[string]*zip.File)}
	ok := false
	defer func() {
		if !ok {
			z.Close()
		}
	}()
	if len(z.File) == 0 || len(z.File) > maxEntries {
		return nil, errors.New("invalid release entry count")
	}
	prefix := "Workass-" + p.TargetVersion + "-windows-amd64/"
	seen := make(map[string]bool)
	fileNames := make(map[string]bool)
	var total uint64
	for _, f := range z.File {
		if f.Name == prefix && f.FileInfo().IsDir() {
			continue
		}
		name, found := strings.CutPrefix(f.Name, prefix)
		name = strings.TrimSuffix(name, "/")
		if !found || !safeName(name) || f.Mode()&os.ModeSymlink != 0 || (!f.FileInfo().IsDir() && !f.Mode().IsRegular()) {
			return nil, errors.New("release ZIP contains an unsafe entry")
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, errors.New("release ZIP contains duplicate Windows paths")
		}
		seen[key] = true
		if f.FileInfo().IsDir() {
			out.directories = append(out.directories, name)
			continue
		}
		if f.UncompressedSize64 > maxExpandedBytes-total {
			return nil, errors.New("release ZIP exceeds expanded size limit")
		}
		total += f.UncompressedSize64
		out.files[name] = f
		fileNames[key] = true
		out.names = append(out.names, name)
	}
	for _, required := range []string{"Workass.exe", "workass-daemon.exe", "workass-tools.exe", "resources/app/package.json", "manifest.json"} {
		if out.files[required] == nil {
			return nil, fmt.Errorf("release ZIP is missing %s", required)
		}
	}
	for _, executable := range []string{"Workass.exe", "workass-daemon.exe", "workass-tools.exe"} {
		if err := verifyWindowsPE(out.files[executable], executable); err != nil {
			return nil, err
		}
	}
	for name := range seen {
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			ancestor := strings.Join(parts[:i], "/")
			if fileNames[strings.ToLower(ancestor)] {
				return nil, errors.New("release ZIP has a file/directory conflict")
			}
		}
	}
	sort.Strings(out.names)
	ok = true
	return out, nil
}

func verifyWindowsPE(file *zip.File, label string) error {
	if file.UncompressedSize64 < 64 {
		return fmt.Errorf("release %s is not a Windows executable", label)
	}
	reader, err := file.Open()
	if err != nil {
		return fmt.Errorf("read release %s: %w", label, err)
	}
	defer reader.Close()
	dos := make([]byte, 64)
	if _, err := io.ReadFull(reader, dos); err != nil || string(dos[:2]) != "MZ" {
		return fmt.Errorf("release %s is not a Windows executable", label)
	}
	peOffset := binary.LittleEndian.Uint32(dos[0x3c:])
	if peOffset < 64 || peOffset > 1<<20 || uint64(peOffset)+26 > file.UncompressedSize64 {
		return fmt.Errorf("release %s has an invalid PE header", label)
	}
	if _, err := io.CopyN(io.Discard, reader, int64(peOffset)-64); err != nil {
		return fmt.Errorf("release %s has an invalid PE header", label)
	}
	pe := make([]byte, 26)
	if _, err := io.ReadFull(reader, pe); err != nil || string(pe[:4]) != "PE\x00\x00" ||
		binary.LittleEndian.Uint16(pe[4:]) != 0x8664 || binary.LittleEndian.Uint16(pe[24:]) != 0x20b {
		return fmt.Errorf("release %s is not PE32+ x86-64", label)
	}
	return nil
}

func (p Plan) writeReceipt(phase, step, message string, installed bool) error {
	r := receipt{WorkerID: p.WorkerID, WorkerPID: os.Getpid(), SchemaVersion: 2, Strategy: strategy, UpdateID: p.UpdateID, InstallationID: p.InstallationID,
		InstallTarget: p.InstallTarget, PreviousVersion: p.CurrentVersion, TargetVersion: p.TargetVersion,
		Phase: phase, Step: step, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Activated: installed,
		Error: message, InterruptedWork: p.InterruptedWork}
	if installed {
		r.InstalledVersion = p.TargetVersion
	}
	if err := atomicJSON(filepath.Join(p.TransactionRoot, "install-receipt.json"), r); err != nil {
		return err
	}
	return atomicJSON(filepath.Join(p.DataRoot, "updates", "receipt.json"), r)
}

func ownedFiles(p Plan, root *os.Root, incoming []string) ([]string, error) {
	files := make(map[string]string)
	checked := make(map[string]bool)
	prior := inventory{}
	err := readJSON(filepath.Join(p.InstallTarget, inventoryName), 16<<20, &prior)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && (prior.SchemaVersion != 1 || prior.InstallationID != p.InstallationID || len(prior.Files) > maxEntries) {
		return nil, errors.New("invalid installed application file inventory")
	}
	for _, name := range append(prior.Files, incoming...) {
		if !safeName(name) {
			return nil, errors.New("unsafe installed application file path")
		}
		// A ZIP must not overwrite user data through a directory junction or
		// symlink, even if that link happens to resolve inside the install root.
		parts := strings.Split(name, "/")
		for i := range parts {
			relative := filepath.Join(parts[:i+1]...)
			if i < len(parts)-1 && checked[relative] {
				continue
			}
			stat, e := root.Lstat(relative)
			if errors.Is(e, os.ErrNotExist) {
				break
			}
			if e != nil {
				return nil, e
			}
			if stat.Mode()&os.ModeSymlink != 0 ||
				(i < len(parts)-1 && !stat.IsDir()) ||
				(i == len(parts)-1 && !stat.Mode().IsRegular()) {
				return nil, errors.New("application file path conflicts with an existing directory or link")
			}
			checked[relative] = stat.IsDir()
		}
		files[strings.ToLower(name)] = name
	}
	names := make([]string, 0, len(files))
	for _, name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func validateIncomingDirectories(root *os.Root, directories []string) error {
	for _, name := range directories {
		parts := strings.Split(name, "/")
		for i := range parts {
			stat, err := root.Lstat(filepath.Join(parts[:i+1]...))
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			if stat.Mode()&os.ModeSymlink != 0 || !stat.IsDir() {
				return errors.New("application directory path conflicts with an existing file or link")
			}
		}
	}
	return nil
}

type operations struct {
	waitShell func() error
	waitFiles func(string, []string) error
	launch    func(string) error
	status    func(string)
}

func (ops operations) report(step string) {
	if ops.status != nil {
		ops.status(step)
	}
}

func replace(p Plan, archive *payload, ops operations) (err error) {
	step, installed := "waiting_for_shutdown", false
	ops.report(step)
	defer func() {
		if err != nil {
			err = errors.Join(err, p.writeReceipt("failed", step, err.Error(), installed))
		}
	}()
	if err = p.writeReceipt("activating", step, "", false); err != nil {
		return err
	}
	if err = ops.waitShell(); err != nil {
		return err
	}
	step = "checking_paths"
	ops.report(step)
	if err = p.writeReceipt("activating", step, "", false); err != nil {
		return err
	}
	root, err := os.OpenRoot(p.InstallTarget)
	if err != nil {
		return err
	}
	defer root.Close()
	names, err := ownedFiles(p, root, archive.names)
	if err != nil {
		return err
	}
	if err = validateIncomingDirectories(root, archive.directories); err != nil {
		return err
	}
	// Check every file before deleting any. A surviving provider or antivirus
	// lock causes a clean failure, never a process kill or a blind partial purge.
	step = "waiting_for_files"
	ops.report(step)
	if err = p.writeReceipt("activating", step, "", false); err != nil {
		return err
	}
	if err = ops.waitFiles(p.InstallTarget, names); err != nil {
		return err
	}
	if err = atomicJSON(filepath.Join(p.InstallTarget, inventoryName), inventory{1, p.InstallationID, names}); err != nil {
		return err
	}
	step = "replacing_files"
	ops.report(step)
	if err = p.writeReceipt("activating", step, "", false); err != nil {
		return err
	}
	for _, name := range names {
		if e := root.Remove(filepath.FromSlash(name)); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	step = "extracting_zip"
	ops.report(step)
	if err = p.writeReceipt("activating", step, "", false); err != nil {
		return err
	}
	for _, name := range archive.directories {
		if err = root.MkdirAll(filepath.FromSlash(name), 0755); err != nil {
			return err
		}
	}
	for _, name := range archive.names {
		f := archive.files[name]
		relative := filepath.FromSlash(name)
		if err = root.MkdirAll(filepath.Dir(relative), 0755); err != nil {
			return err
		}
		if err = extractFile(root, relative, f); err != nil {
			return err
		}
	}
	inventoryErr := atomicJSON(filepath.Join(p.InstallTarget, inventoryName), inventory{1, p.InstallationID, archive.names})
	installed = true
	step = "launching"
	ops.report(step)
	// Record completed replacement before launching. The new shell never
	// resumes this installer or interprets normal provider startup as failure.
	return errors.Join(inventoryErr, launchInstalled(p, ops.launch))
}

func launchInstalled(p Plan, launch func(string) error) error {
	// Once replacement is complete, receipt I/O must not keep the runnable app
	// closed. Launch once and report either failure without attempting rollback.
	before := p.writeReceipt("installed", "launching", "", true)
	if err := launch(filepath.Join(p.InstallTarget, "Workass.exe")); err != nil {
		return errors.Join(before, err)
	}
	return errors.Join(before, p.writeReceipt("installed", "launched", "", true))
}

func extractFile(root *os.Root, name string, file *zip.File) error {
	in, err := file.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	return errors.Join(copyErr, closeErr)
}

func committed(input io.Reader, timeout time.Duration) bool {
	commit := make(chan bool, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(input, 16)).ReadString('\n')
		commit <- err == nil && line == "install\n"
	}()
	select {
	case accepted := <-commit:
		return accepted
	case <-time.After(timeout):
		return false
	}
}

// Run arms the incoming native binary, then waits for one explicit commit on
// stdin. EOF/timeout means no installation. There is no restart/retry loop.
func Run(planPath string, input io.Reader, output io.Writer) (err error) {
	if err := supportedPlatform(); err != nil {
		return err
	}
	var p Plan
	if err := readJSON(planPath, 64<<10, &p); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err = p.validate(planPath, executable); err != nil {
		return err
	}
	archive, err := openPayload(p)
	if err != nil {
		return err
	}
	defer archive.archive.Close()
	ops, closeProcess, err := platformOperations(p)
	if err != nil {
		return err
	}
	defer closeProcess()
	if err = p.writeReceipt("armed", "waiting_for_commit", "", false); err != nil {
		return err
	}
	if _, err = fmt.Fprintln(output, `{"ready":true}`); err != nil {
		return err
	}
	if !committed(input, 30*time.Second) {
		return p.writeReceipt("failed", "waiting_for_commit", "installation was not committed; application files unchanged", false)
	}
	status, finish := installerUI(p.TargetVersion)
	defer func() { finish(err) }()
	ops.status = status
	return replace(p, archive, ops)
}
