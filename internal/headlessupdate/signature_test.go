package headlessupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testReleaseKey is generated per test run. No test ever reads the real signing
// key, and these keys reach a Manager only through Options.
var testReleasePublicKey, testReleasePrivateKey = func() (string, ed25519.PrivateKey) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(public), private
}()

// signManifestJSON signs a manifest the way the publisher does: over the canonical
// form of what a client decodes, added as one field.
func signManifestJSON(t *testing.T, manifest map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Manifest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	signed := map[string]any{}
	for key, value := range manifest {
		signed[key] = value
	}
	signed["signature"] = map[string]any{
		"algorithm": "ed25519", "keyId": "test",
		"value": base64.StdEncoding.EncodeToString(ed25519.Sign(testReleasePrivateKey, CanonicalManifest(&decoded))),
	}
	out, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPinnedReleaseKeyIsTheOwnersPublicKey(t *testing.T) {
	if len(pinnedReleaseManifestKeys) == 0 || pinnedReleaseManifestKeys[0] != "DIV37U57AIJdwl6JGN4njQdN6OCHv39K+WgZMmlo92s=" {
		t.Fatalf("pinned keys = %v", pinnedReleaseManifestKeys)
	}
	for _, key := range pinnedReleaseManifestKeys {
		if _, err := ReleaseManifestKeyID(key); err != nil {
			t.Fatalf("pinned key %q: %v", key, err)
		}
	}
}

// The other half of this agreement is release-manifest-signature.test.js, which
// reads the same file: neither language can change the signed form alone.
func TestSignedFormMatchesTheElectronUpdaterAndThePublisher(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "release-manifest-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		PublicKey string `json:"publicKey"`
		Cases     []struct {
			Name      string          `json:"name"`
			Canonical string          `json:"canonical"`
			Manifest  json.RawMessage `json:"manifest"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) != 3 {
		t.Fatalf("vectors = %d", len(vectors.Cases))
	}
	for _, item := range vectors.Cases {
		var manifest Manifest
		if err := json.Unmarshal(item.Manifest, &manifest); err != nil {
			t.Fatalf("%s: %v", item.Name, err)
		}
		if got := string(CanonicalManifest(&manifest)); got != item.Canonical {
			t.Fatalf("%s canonical form differs:\n%s\nwant\n%s", item.Name, got, item.Canonical)
		}
		if err := VerifyManifestSignature(&manifest, []string{vectors.PublicKey}); err != nil {
			t.Fatalf("%s: %v", item.Name, err)
		}
		if err := VerifyManifestSignature(&manifest, nil); err == nil {
			t.Fatalf("%s: a vector made with a throwaway key verified against the pinned keys", item.Name)
		}
	}
}

func TestEverySignedFieldIsCovered(t *testing.T) {
	base := map[string]any{
		"schemaVersion": 1, "product": "Workass", "version": "1.2.4", "platform": "linux", "arch": "amd64",
		"headless": true, "service": "systemd-user",
		"artifacts": map[string]any{"update": map[string]any{
			"name": "a.tar.gz", "url": "https://github.example/a.tar.gz", "sha256": strings.Repeat("a", 64), "size": 10,
		}},
	}
	signed := signManifestJSON(t, base)
	decode := func(t *testing.T, raw []byte) *Manifest {
		t.Helper()
		manifest, err := ValidateManifest(raw, "linux", "amd64")
		if err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	keys := []string{testReleasePublicKey}
	if err := VerifyManifestSignature(decode(t, signed), keys); err != nil {
		t.Fatalf("a good signature was refused: %v", err)
	}
	for name, tamper := range map[string]func(*Manifest){
		"version":  func(m *Manifest) { m.Version = "1.2.5" },
		"platform": func(m *Manifest) { m.Platform = "windows" },
		"arch":     func(m *Manifest) { m.Arch = "arm64" },
		"url":      func(m *Manifest) { m.Artifacts.Update.URL = "https://attacker.example/a.tar.gz" },
		"sha256":   func(m *Manifest) { m.Artifacts.Update.SHA256 = strings.Repeat("b", 64) },
		"size":     func(m *Manifest) { m.Artifacts.Update.Size = 11 },
		"name":     func(m *Manifest) { m.Artifacts.Update.Name = "b.tar.gz" },
		"notes":    func(m *Manifest) { m.Notes = "run this script" },
		"headless": func(m *Manifest) { m.Headless = false },
	} {
		manifest := decode(t, signed)
		tamper(manifest)
		err := VerifyManifestSignature(manifest, keys)
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("tampered %s: %v", name, err)
		}
	}
	// A signature from a key this build does not pin is the compromised-release
	// case itself, however well formed it is.
	if err := VerifyManifestSignature(decode(t, signed), nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a key that is not pinned was accepted: %v", err)
	}
	for name, signature := range map[string]*ManifestSignature{
		"missing":   nil,
		"algorithm": {Algorithm: "rsa", Value: decode(t, signed).Signature.Value},
		"malformed": {Algorithm: "ed25519", Value: "not base64"},
		"short":     {Algorithm: "ed25519", Value: "AAAA"},
	} {
		manifest := decode(t, signed)
		manifest.Signature = signature
		if err := VerifyManifestSignature(manifest, keys); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("%s signature: %v", name, err)
		}
	}
}

func TestDaemonRefusesAnUnsignedOrForgedFeedBeforeStagingAnything(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any) []byte{
		"unsigned": func(manifest map[string]any) []byte {
			out, _ := json.Marshal(manifest)
			return out
		},
		"forged by a compromised release": func(manifest map[string]any) []byte {
			_, attacker, _ := ed25519.GenerateKey(rand.Reader)
			encoded, _ := json.Marshal(manifest)
			var decoded Manifest
			_ = json.Unmarshal(encoded, &decoded)
			manifest["signature"] = map[string]any{
				"algorithm": "ed25519", "value": base64.StdEncoding.EncodeToString(ed25519.Sign(attacker, CanonicalManifest(&decoded))),
			}
			out, _ := json.Marshal(manifest)
			return out
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarnessWithFeed(t, "1.2.3", "1.2.4", "1.2.4", "on-failure", mutate)
			_, err := h.manager.Apply(context.Background(), authorized("m-1", "1.2.3", "1.2.4", "op-a1b2c3d4"))
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("apply = %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(h.installRoot, StagingDirName)); statErr == nil {
				t.Fatal("something was staged for a release that did not verify")
			}
			if receipt := h.manager.readReceipt(); receipt != nil {
				t.Fatalf("a refused release left a receipt: %#v", receipt)
			}
			if operations := h.manager.operations(); len(operations) != 0 {
				t.Fatalf("a refused release consumed an operation: %#v", operations)
			}
		})
	}
}

// An old signed release is a real signature on a real manifest, so only the
// version says it must not run: replaying it can never move a machine backwards.
func TestDaemonRefusesToReplayAnOlderSignedRelease(t *testing.T) {
	h := newHarness(t, "1.2.3", "1.2.2", "1.2.2", "on-failure")
	manifest, err := h.manager.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("the signature on the older release is good: %v", err)
	}
	if manifest.Version != "1.2.2" {
		t.Fatalf("feed = %s", manifest.Version)
	}
	status := h.manager.Status(context.Background())
	if state := status["state"].(map[string]any); state["targetVersion"] != nil {
		t.Fatalf("an older release was offered as a target: %#v", state)
	}
	if _, err := h.manager.Apply(context.Background(), authorized("m-1", "1.2.3", "1.2.2", "op-a1b2c3d4")); err == nil {
		t.Fatal("an older signed release was applied")
	}
	if receipt := h.manager.readReceipt(); receipt != nil {
		t.Fatalf("the replay left a receipt: %#v", receipt)
	}
}

// What an installed older client does with a signed manifest: the field is simply
// not one it decodes. Its decoder is ValidateManifest, unchanged in this respect.
func TestASignatureIsAnAdditiveField(t *testing.T) {
	raw := signManifestJSON(t, map[string]any{
		"schemaVersion": 1, "product": "Workass", "version": "1.2.4", "platform": "linux", "arch": "amd64", "headless": true,
		"artifacts": map[string]any{"update": map[string]any{
			"name": "a.tar.gz", "url": "https://github.example/a.tar.gz", "sha256": strings.Repeat("a", 64), "size": 10,
		}},
	})
	var legacy struct {
		SchemaVersion int    `json:"schemaVersion"`
		Version       string `json:"version"`
		Artifacts     struct {
			Update Artifact `json:"update"`
		} `json:"artifacts"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&legacy); err != nil || legacy.Version != "1.2.4" {
		t.Fatalf("legacy decode = %#v err=%v", legacy, err)
	}
	if !strings.Contains(string(raw), `"signature"`) {
		t.Fatal(fmt.Sprintf("no signature in %s", raw))
	}
}
