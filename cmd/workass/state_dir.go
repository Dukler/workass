package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// testRootEnvVar names the test profile's only data root. test.env derives
// WORKASS_DATA_ROOT from it and the shell and Electron loaders require it; the
// daemon follows the same mapping, so state lives in <root>/state and the files
// kept beside state (app-config.json, providers.json, run/) live in <root>.
const testRootEnvVar = "WORKASS_TEST_ROOT"

// updatePreflightEnv marks a daemon the in-app updater started from a staged
// release to prove it works before anything closes (main.go: loopback only,
// admission closed, no beacon, fleet links or schedules).
const updatePreflightEnv = "WORKASS_UPDATE_PREFLIGHT"

// daemonStateDirRequest is what decides a daemon's state directory. It is a value
// rather than a read of the process so every refusal can be tested without
// starting a daemon.
type daemonStateDirRequest struct {
	Flag     string
	FlagSet  bool
	Profile  string
	TestRoot string
	CWD      string
	// ProtectedRoots are this machine's installed data roots. A test root that
	// overlaps one would be the real state under a test label.
	ProtectedRoots []string
	Home           string
}

func daemonStateDirRequestFromEnv(flagValue string, flagSet bool, cwd string) daemonStateDirRequest {
	home, _ := os.UserHomeDir()
	return daemonStateDirRequest{
		Flag:           flagValue,
		FlagSet:        flagSet,
		Profile:        workassRuntimeProfile(),
		TestRoot:       os.Getenv(testRootEnvVar),
		CWD:            cwd,
		ProtectedRoots: installedDataRoots(runtime.GOOS, home, os.Getenv("LOCALAPPDATA")),
		Home:           home,
	}
}

// resolveDaemonStateDir refuses every start whose state would land somewhere the
// operator did not choose. The flag's default is relative to the working
// directory, so a daemon started by hand in a checkout used to adopt whatever
// git-ignored state/ was there, write to it, and write app-config.json and
// providers.json beside it. An explicit --state-dir outside the test profile is
// the operator's choice and is taken as given.
func resolveDaemonStateDir(req daemonStateDirRequest) (string, error) {
	explicit := req.FlagSet && strings.TrimSpace(req.Flag) != ""
	testRoot := strings.TrimSpace(req.TestRoot)
	requested := ""
	if explicit {
		requested = req.Flag
		if !filepath.IsAbs(requested) {
			requested = filepath.Join(req.CWD, requested)
		}
		requested = filepath.Clean(requested)
	}
	switch {
	case req.Profile == "test":
		if testRoot == "" {
			// scripts/canary names its own scratch state and drives real, logged-in
			// providers, so an explicit directory without a root stays valid and
			// unconfined. What is refused is having neither.
			if explicit {
				return requested, nil
			}
			return "", fmt.Errorf("WORKASS_PROFILE=test requires %s, a fresh temporary directory", testRootEnvVar)
		}
		if !filepath.IsAbs(testRoot) {
			return "", fmt.Errorf("%s must be absolute: %q", testRootEnvVar, testRoot)
		}
		testRoot = filepath.Clean(testRoot)
		if err := refuseUnsafeTestRoot(testRoot, req); err != nil {
			return "", err
		}
		stateDir := requested
		if stateDir == "" {
			stateDir = filepath.Join(testRoot, "state")
		}
		// Strictly inside: the daemon also writes beside its state directory, so
		// a state directory equal to the root would write above the root.
		if !pathStrictlyInside(testRoot, stateDir) {
			return "", fmt.Errorf("the state directory %s is outside %s=%s; a test daemon writes only under its test root", stateDir, testRootEnvVar, testRoot)
		}
		return stateDir, nil
	case explicit:
		return requested, nil
	case testRoot != "":
		return "", fmt.Errorf("%s is set but WORKASS_PROFILE is %q; set WORKASS_PROFILE=test, or pass --state-dir", testRootEnvVar, req.Profile)
	case req.Profile == "dev":
		return "", fmt.Errorf("the dev profile has no default state directory; pass --state-dir, or start it with desktop/scripts/dev-launch-macos.sh")
	case isWorkassSourceCheckout(req.CWD):
		return "", fmt.Errorf("refusing to use %s: this is a source checkout; for a scratch daemon set WORKASS_PROFILE=test and %s=$(mktemp -d), or pass --state-dir", filepath.Join(req.CWD, "state"), testRootEnvVar)
	}
	return filepath.Join(req.CWD, "state"), nil
}

func refuseUnsafeTestRoot(root string, req daemonStateDirRequest) error {
	if isWorkassSourceCheckout(root) {
		return fmt.Errorf("%s=%s is a source checkout; use a fresh temporary directory", testRootEnvVar, root)
	}
	if home := strings.TrimSpace(req.Home); home != "" && (pathSameOrInside(root, home)) {
		return fmt.Errorf("%s=%s holds the home directory; use a fresh temporary directory", testRootEnvVar, root)
	}
	for _, protected := range req.ProtectedRoots {
		if pathSameOrInside(root, protected) || pathSameOrInside(protected, root) {
			return fmt.Errorf("%s=%s overlaps the installed Workass data at %s; use a fresh temporary directory", testRootEnvVar, root, protected)
		}
	}
	return nil
}

// applyTestRootEnv confines what this process and its children inherit. A test
// daemon started from a Workass agent's shell otherwise hands its children the
// installed daemon's data root and that turn's tool credential, so `workass run`
// or a provider host would report to and write in the real state. HOME moves too:
// provider detection launches the real claude and codex CLIs, and those write
// their config, databases and skills under HOME. A rig that needs logged-in
// providers uses the dev profile with an explicit --state-dir.
func applyTestRootEnv(root string) error {
	root = filepath.Clean(root)
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create the test home: %w", err)
	}
	set := map[string]string{
		dataRootEnvVar:     root,
		"WORKASS_LOG_ROOT": filepath.Join(root, "logs"),
		"HOME":             home,
		"XDG_CONFIG_HOME":  filepath.Join(home, ".config"),
		"XDG_DATA_HOME":    filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":   filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":   filepath.Join(home, ".cache"),
	}
	if file := strings.TrimSpace(os.Getenv("WORKASS_BROWSER_CONTROL_FILE")); file == "" || !pathStrictlyInside(root, file) {
		set["WORKASS_BROWSER_CONTROL_FILE"] = filepath.Join(root, "run", "browser-control.json")
	}
	if strings.TrimSpace(os.Getenv("USERPROFILE")) != "" {
		set["USERPROFILE"] = home
		set["APPDATA"] = filepath.Join(home, "AppData", "Roaming")
		set["LOCALAPPDATA"] = filepath.Join(home, "AppData", "Local")
	}
	for name, value := range set {
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("set %s: %w", name, err)
		}
	}
	// Each of these points a child at a file or directory it would write or trust.
	for _, name := range []string{"WORKASS_TOOL_CONTEXT", "WORKASS_INSTRUCTIONS_FILE", "WORKASS_TOOLS_GUIDE", "WORKASS_RUN_TOKEN_FILE", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" && !pathStrictlyInside(root, value) {
			if err := os.Unsetenv(name); err != nil {
				return fmt.Errorf("unset %s: %w", name, err)
			}
		}
	}
	return nil
}

// testProfileStateDir is <root>/state when this process runs under the test
// profile with a test root, and empty otherwise. The fleet and run commands use
// it ahead of an inherited WORKASS_DATA_ROOT, which names the installed daemon
// when the shell belongs to one of its agents.
func testProfileStateDir() (stateDir, root string) {
	if workassRuntimeProfile() != "test" {
		return "", ""
	}
	root = strings.TrimSpace(os.Getenv(testRootEnvVar))
	if root == "" || !filepath.IsAbs(root) {
		return "", ""
	}
	return filepath.Join(filepath.Clean(root), "state"), filepath.Clean(root)
}

func installedDataRoots(goos, home, localAppData string) []string {
	var roots []string
	switch goos {
	case "darwin":
		if home != "" {
			roots = append(roots, filepath.Join(home, "Library", "Application Support", "Workass"), filepath.Join(home, "Library", "Logs", "Workass"))
		}
	case "windows":
		if localAppData != "" {
			roots = append(roots, filepath.Join(localAppData, "Workass"))
		}
	default:
		if home != "" {
			roots = append(roots, filepath.Join(home, "workass"))
		}
	}
	return roots
}

// isWorkassSourceCheckout recognises the repository by the daemon's own source,
// which an installed or unpacked release does not ship.
func isWorkassSourceCheckout(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "cmd", "workass", "main.go"))
	return err == nil && !info.IsDir()
}

func pathStrictlyInside(root, path string) bool {
	rel, ok := relativeToRoot(root, path)
	return ok && rel != "."
}

func pathSameOrInside(root, path string) bool {
	_, ok := relativeToRoot(root, path)
	return ok
}

func relativeToRoot(root, path string) (string, bool) {
	rel, err := filepath.Rel(canonicalPath(root), canonicalPath(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// canonicalPath resolves symlinks through the part of a path that exists, so
// /tmp/x/state compares equal to /private/tmp/x/state before state exists.
func canonicalPath(path string) string {
	path = filepath.Clean(path)
	rest := ""
	for current := path; ; {
		if real, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}
