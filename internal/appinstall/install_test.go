package appinstall

import (
	"archive/zip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, extra map[string]string) Plan {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Plan{SchemaVersion: 1, Strategy: strategy, UpdateID: "upd-native-test", InstallationID: "install-" + strings.Repeat("a", 32),
		CurrentVersion: "1.1.0", TargetVersion: "1.2.0", InstallTarget: filepath.Join(base, "app"), DataRoot: filepath.Join(base, "data"), ShellPID: 4242}
	p.TransactionRoot = filepath.Join(p.DataRoot, "updates", "transactions", p.UpdateID)
	for _, dir := range []string{p.InstallTarget, p.TransactionRoot, filepath.Join(p.DataRoot, "state")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := atomicJSON(filepath.Join(p.InstallTarget, identityName), map[string]any{"schemaVersion": 1, "product": "Workass", "installationId": p.InstallationID}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.InstallTarget, "Workass.exe"), "old-app")
	write(t, filepath.Join(p.InstallTarget, "obsolete.dll"), "old-library")
	write(t, filepath.Join(p.InstallTarget, "notes.txt"), "user-file")
	write(t, filepath.Join(p.InstallTarget, "resources", "my-project", "important.txt"), "workspace")
	write(t, filepath.Join(p.DataRoot, "state", "chat.json"), "chat-state")
	if err := atomicJSON(filepath.Join(p.InstallTarget, inventoryName), inventory{1, p.InstallationID, []string{"Workass.exe", "obsolete.dll"}}); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"Workass.exe": "new-app", "workass-daemon.exe": "new-daemon", "manifest.json": "{}", "resources/app/package.json": "{}"}
	for key, value := range extra {
		files[key] = value
	}
	f, err := os.Create(filepath.Join(p.TransactionRoot, "release.zip"))
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for name, body := range files {
		entry := "Workass-1.2.0-windows-amd64/" + name
		w, err := z.Create(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "/") {
			continue
		}
		if _, err = w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIncomingDirectoryCollisionFailsBeforeDeletingOwnedFiles(t *testing.T) {
	p := fixture(t, map[string]string{"notes.txt/": ""})
	a, err := openPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.archive.Close()
	err = replace(p, a, operations{
		waitShell: func() error { return nil },
		waitFiles: func(string, []string) error { t.Fatal("file lock check should not run"); return nil },
		launch:    func(string) error { t.Fatal("unexpected launch"); return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "directory path conflicts") {
		t.Fatalf("expected directory collision, got %v", err)
	}
	contents(t, filepath.Join(p.InstallTarget, "Workass.exe"), "old-app")
	contents(t, filepath.Join(p.InstallTarget, "obsolete.dll"), "old-library")
	intactUserFiles(t, p)
}

func write(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func contents(t *testing.T, file, want string) {
	t.Helper()
	got, err := os.ReadFile(file)
	if err != nil || string(got) != want {
		t.Fatalf("%s: got %q, %v; want %q", filepath.Base(file), got, err, want)
	}
}

func intactUserFiles(t *testing.T, p Plan) {
	t.Helper()
	contents(t, filepath.Join(p.InstallTarget, "notes.txt"), "user-file")
	contents(t, filepath.Join(p.InstallTarget, "resources", "my-project", "important.txt"), "workspace")
	contents(t, filepath.Join(p.DataRoot, "state", "chat.json"), "chat-state")
	var identity map[string]any
	if err := readJSON(filepath.Join(p.InstallTarget, identityName), 4096, &identity); err != nil || identity["installationId"] != p.InstallationID {
		t.Fatal("installation identity was lost", err)
	}
}

func TestInstallReplacesOnlyOwnedFilesAndLaunchesWithoutHealthProbes(t *testing.T) {
	p := fixture(t, map[string]string{"resources/app/main.js": "new-shell"})
	a, err := openPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.archive.Close()
	var calls []string
	err = replace(p, a, operations{
		waitShell: func() error { calls = append(calls, "wait-shell"); return nil },
		waitFiles: func(root string, names []string) error {
			calls = append(calls, "wait-files")
			contents(t, filepath.Join(root, "Workass.exe"), "old-app")
			for _, name := range names {
				if name == "notes.txt" {
					t.Fatal("user file selected for deletion")
				}
			}
			return nil
		},
		launch: func(executable string) error {
			calls = append(calls, "launch")
			contents(t, executable, "new-app")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"wait-shell", "wait-files", "launch"}) {
		t.Fatal(calls)
	}
	if _, err := os.Stat(filepath.Join(p.InstallTarget, "obsolete.dll")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("obsolete application file survived", err)
	}
	intactUserFiles(t, p)
	var r receipt
	if err := readJSON(filepath.Join(p.DataRoot, "updates", "receipt.json"), 65536, &r); err != nil {
		t.Fatal(err)
	}
	if r.Phase != "installed" || r.Step != "launched" || !r.Activated {
		t.Fatalf("receipt: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(p.TransactionRoot, "release.zip")); err != nil {
		t.Fatal("download not retained", err)
	}
}

func TestShutdownOrFileLockFailureCannotDeleteFiles(t *testing.T) {
	for _, stage := range []string{"shutdown", "file-lock"} {
		t.Run(stage, func(t *testing.T) {
			p := fixture(t, nil)
			a, err := openPayload(p)
			if err != nil {
				t.Fatal(err)
			}
			defer a.archive.Close()
			err = replace(p, a, operations{
				waitShell: func() error {
					if stage == "shutdown" {
						return errors.New("shell running")
					}
					return nil
				},
				waitFiles: func(string, []string) error { return errors.New("file locked") },
				launch:    func(string) error { t.Fatal("unexpected launch"); return nil },
			})
			if err == nil {
				t.Fatal("expected failure")
			}
			contents(t, filepath.Join(p.InstallTarget, "Workass.exe"), "old-app")
			contents(t, filepath.Join(p.InstallTarget, "obsolete.dll"), "old-library")
			intactUserFiles(t, p)
		})
	}
}

func TestExtractionFailureRetainsDownloadAndDoesNotRollbackOrLaunch(t *testing.T) {
	p := fixture(t, nil)
	a, err := openPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.archive.Close()
	// Corrupt the first payload's CRC after it has been structurally opened.
	a.files["Workass.exe"].CRC32 ^= 1
	err = replace(p, a, operations{waitShell: func() error { return nil }, waitFiles: func(string, []string) error { return nil }, launch: func(string) error { t.Fatal("launched a partial installation"); return nil }})
	if err == nil {
		t.Fatal("expected ZIP CRC failure")
	}
	contents(t, filepath.Join(p.InstallTarget, "Workass.exe"), "new-app")
	intactUserFiles(t, p)
	var r receipt
	if err := readJSON(filepath.Join(p.TransactionRoot, "install-receipt.json"), 65536, &r); err != nil {
		t.Fatal(err)
	}
	if r.Phase != "failed" || r.Step != "extracting_zip" {
		t.Fatalf("receipt: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(p.TransactionRoot, "release.zip")); err != nil {
		t.Fatal(err)
	}
}

func TestUnsafeArchiveEntriesAreRejectedBeforeDeletion(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", "C:/outside", "resources/evil:stream", "CON.txt", "foo. /bar", "foo\\bar", identityName, inventoryName, "workass.EXE"} {
		t.Run(name, func(t *testing.T) {
			p := fixture(t, map[string]string{name: "bad"})
			a, err := openPayload(p)
			if err == nil {
				a.archive.Close()
				t.Fatal("accepted unsafe ZIP entry")
			}
			contents(t, filepath.Join(p.InstallTarget, "Workass.exe"), "old-app")
		})
	}
}

func TestSymlinkAndInventoryTraversalCannotReachUserData(t *testing.T) {
	p := fixture(t, nil)
	if err := atomicJSON(filepath.Join(p.InstallTarget, inventoryName), inventory{1, p.InstallationID, []string{"../data/state/chat.json"}}); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(p.InstallTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := ownedFiles(p, root, []string{"Workass.exe"}); err == nil {
		t.Fatal("accepted escaping inventory")
	}
	if err := os.Remove(filepath.Join(p.InstallTarget, inventoryName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p.DataRoot, filepath.Join(p.InstallTarget, "link")); err != nil {
		t.Skip("symlink creation unavailable:", err)
	}
	if _, err := ownedFiles(p, root, []string{"link/state/chat.json"}); err == nil {
		t.Fatal("accepted symlink overwrite")
	}
	intactUserFiles(t, p)
}

func TestPlanRejectsWrongInstallationAndOverlappingData(t *testing.T) {
	p := fixture(t, nil)
	planPath := filepath.Join(p.TransactionRoot, "native-install.json")
	executable := filepath.Join(p.TransactionRoot, "incoming-release", "workass-daemon.exe")
	if err := p.validate(planPath, executable); err != nil {
		t.Fatal(err)
	}
	wrong := p
	wrong.InstallationID = "install-" + strings.Repeat("b", 32)
	if err := wrong.validate(planPath, executable); err == nil {
		t.Fatal("accepted different installation")
	}
	wrong = p
	wrong.DataRoot = p.InstallTarget
	if err := wrong.validate(planPath, executable); err == nil {
		t.Fatal("accepted overlapping data")
	}
	if err := p.validate(planPath, filepath.Join(p.InstallTarget, "workass-daemon.exe")); err == nil {
		t.Fatal("accepted installer inside replacement tree")
	}
}

func TestCommitRequiresExplicitCompleteInputAndIsBounded(t *testing.T) {
	for _, input := range []string{"", "install", "install\r\n", "abort\n", "true\n"} {
		if committed(strings.NewReader(input), time.Second) {
			t.Fatalf("accepted %q", input)
		}
	}
	if !committed(strings.NewReader("install\n"), time.Second) {
		t.Fatal("explicit commit rejected")
	}
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	if committed(r, time.Millisecond) {
		t.Fatal("timed out input accepted")
	}
}

func TestFirstNativeUpdateWithoutInventoryPreservesUnknownFiles(t *testing.T) {
	p := fixture(t, nil)
	if err := os.Remove(filepath.Join(p.InstallTarget, inventoryName)); err != nil {
		t.Fatal(err)
	}
	a, err := openPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.archive.Close()
	err = replace(p, a, operations{waitShell: func() error { return nil }, waitFiles: func(string, []string) error { return nil }, launch: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	contents(t, filepath.Join(p.InstallTarget, "Workass.exe"), "new-app")
	contents(t, filepath.Join(p.InstallTarget, "obsolete.dll"), "old-library") // unknown until the first native inventory
	intactUserFiles(t, p)
}

func TestReceiptWriteFailureDoesNotPreventLaunchingInstalledApp(t *testing.T) {
	p := fixture(t, nil)
	// A directory at the receipt destination fails atomic replacement even
	// when the tests run as an administrator.
	if err := os.Mkdir(filepath.Join(p.TransactionRoot, "install-receipt.json"), 0755); err != nil {
		t.Fatal(err)
	}
	launches := 0
	err := launchInstalled(p, func(file string) error { launches++; return nil })
	if err == nil {
		t.Fatal("expected receipt error")
	}
	if launches != 1 {
		t.Fatalf("launches=%d", launches)
	}
	intactUserFiles(t, p)
}
