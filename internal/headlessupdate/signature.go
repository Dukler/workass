package headlessupdate

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// pinnedReleaseManifestKeys are the public halves of the offline release signing
// keys. It is a list so a replacement can ship before the old key stops signing;
// rotation is in docs/DISTRIBUTION.md. Mirrored in JS by
// desktop/shell/release-manifest-signature.js, and a test keeps the two equal.
//
// Nothing outside a test can change this: Options.ReleaseManifestKeys is not
// reachable from a flag or an environment variable, because a key an update could
// name would be no check at all.
var pinnedReleaseManifestKeys = []string{
	"DIV37U57AIJdwl6JGN4njQdN6OCHv39K+WgZMmlo92s=",
}

const canonicalManifestContext = "workass-release-manifest-v1"

// ManifestSignature is the additive field a release manifest carries. Clients
// that predate it ignore it, so they keep accepting signed releases.
type ManifestSignature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"keyId"`
	Value     string `json:"value"`
}

func canonicalField(builder *strings.Builder, name, value string) {
	builder.WriteString(name)
	builder.WriteByte('=')
	builder.WriteString(strconv.Itoa(len(value)))
	builder.WriteByte(':')
	builder.WriteString(value)
	builder.WriteByte('\n')
}

// CanonicalManifest is the exact text the signature covers. Length-prefixed lines
// rather than JSON: Go, the Electron updater and the publisher have to produce the
// same bytes, a field can hold a newline, and no value can read as the next field.
func CanonicalManifest(manifest *Manifest) []byte {
	var builder strings.Builder
	builder.WriteString(canonicalManifestContext)
	builder.WriteByte('\n')
	build := ""
	if manifest.Build != nil {
		build = strconv.FormatInt(*manifest.Build, 10)
	}
	artifact := manifest.Artifacts.Update
	canonicalField(&builder, "schemaVersion", strconv.Itoa(manifest.SchemaVersion))
	canonicalField(&builder, "product", manifest.Product)
	canonicalField(&builder, "version", manifest.Version)
	canonicalField(&builder, "build", build)
	canonicalField(&builder, "platform", manifest.Platform)
	canonicalField(&builder, "arch", manifest.Arch)
	canonicalField(&builder, "portable", strconv.FormatBool(manifest.Portable))
	canonicalField(&builder, "headless", strconv.FormatBool(manifest.Headless))
	canonicalField(&builder, "designatedRequirement", manifest.DesignatedRequirement)
	canonicalField(&builder, "notes", manifest.Notes)
	canonicalField(&builder, "artifact.name", artifact.Name)
	canonicalField(&builder, "artifact.url", artifact.URL)
	canonicalField(&builder, "artifact.sha256", strings.ToLower(artifact.SHA256))
	canonicalField(&builder, "artifact.size", strconv.FormatInt(artifact.Size, 10))
	return []byte(builder.String())
}

func decodeStrictBase64(value string, size int) ([]byte, bool) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != size {
		return nil, false
	}
	return decoded, true
}

// ReleaseManifestKeyID is the short name a signature carries for its key.
func ReleaseManifestKeyID(publicKey string) (string, error) {
	raw, ok := decodeStrictBase64(publicKey, ed25519.PublicKeySize)
	if !ok {
		return "", errors.New("release manifest public key is not 32 bytes of base64")
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:8]), nil
}

// VerifyManifestSignature returns a plain sentence on a missing, malformed, wrong
// or tampered signature. keys nil means the keys pinned in this binary. The caller
// stops there, before anything is downloaded or staged.
func VerifyManifestSignature(manifest *Manifest, keys []string) error {
	if manifest == nil || manifest.Signature == nil {
		return errors.New("this Workass release has no signature, so it was refused")
	}
	if manifest.Signature.Algorithm != "ed25519" {
		return errors.New("this Workass release uses an unknown signature algorithm, so it was refused")
	}
	signature, ok := decodeStrictBase64(manifest.Signature.Value, ed25519.SignatureSize)
	if !ok {
		return errors.New("this Workass release has a malformed signature, so it was refused")
	}
	if keys == nil {
		keys = pinnedReleaseManifestKeys
	}
	message := CanonicalManifest(manifest)
	for _, key := range keys {
		raw, ok := decodeStrictBase64(key, ed25519.PublicKeySize)
		if !ok {
			return fmt.Errorf("release manifest public key %q is not 32 bytes of base64", key)
		}
		if ed25519.Verify(ed25519.PublicKey(raw), message, signature) {
			return nil
		}
	}
	return errors.New("this Workass release is not signed by a key this build trusts, or it was changed after signing, so it was refused")
}
