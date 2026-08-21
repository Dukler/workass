# Workass macOS signing and updates

macOS privacy grants belong to a code-signing designated requirement, not to an
app name or filesystem path. A release whose identity is only its CDHash is a
different app after every rebuild. Workass therefore requires one persistent
identity for `/Applications/Workass.app` and the separately launched Go daemon.

The build and release scripts now enforce five invariants:

1. Every locally built macOS binary — production app, production daemon, and the
   development artifacts in `dist-bin/` — uses one persistent, non-ad-hoc
   signing identity. `scripts/build-daemon.sh` signs the darwin outputs; the
   development profile borrows the identity bootstrapped under the production
   data root rather than minting a second one, because a second identity is a
   second application to macOS.
2. An ordinary update proceeds only when old and new code satisfy each other's
   designated requirements.
3. An update that would install identical code is not performed. The daemon
   rebuild compares the candidate's CDHash with the installed binary's and
   reports `DAEMON_ALREADY_CURRENT` instead of restarting, so live ACP engines
   survive a no-op promotion.
4. The runtime binary is staged beside its target, signed, then swapped.
   Copying onto a live binary mutates the running process's mapped image, which
   the kernel kills for an invalid signature.
5. The running shell stops before its bundle is replaced. The staged app is
   verified, swapped on the same volume, and rolled back if launch health fails.

An update mechanism does not replace signing. Signed updates preserve privacy
grants; ad-hoc updates do not.

Each daemon handoff registers its own transient launchd label. Finished labels
are swept before a new one is bootstrapped, so a rebuild does not accumulate
background items.

## Public-release identity

Install a valid `Developer ID Application` certificate and provide its SHA-1
fingerprint:

```sh
WORKASS_CODESIGN_IDENTITY=0123456789ABCDEF0123456789ABCDEF01234567 \
WORKASS_NOTARY_PROFILE=workass-notary \
  scripts/release-workass-macos.sh --version 1.0.0 --build-number 1
```

If the identity lives outside the default keychain search list, also set
`WORKASS_CODESIGN_KEYCHAIN` to its absolute keychain path.

The public command rejects a local/self-signed identity. It signs nested code
inside-out, enables the hardened runtime, submits both app and DMG to Apple's
notary service, staples their tickets, runs Gatekeeper assessment, and emits a
portable DMG plus update ZIP and receipts. The local package command still
exists only for dogfooding this Mac and never claims public-release readiness.

See `docs/DISTRIBUTION.md` for the full artifact and cross-platform road.

## Private single-Mac development identity (never distribute)

When no Developer ID identity exists, a private development install may create
one local signer:

```sh
scripts/macos/bootstrap-workass-local-signing.sh --profile prod
```

This intentionally requires one macOS authorization to trust the Workass-only
certificate for code signing. The private key lives in a dedicated keychain
under the production Workass data root. The bootstrap is never automatic and
never changes production app bytes or processes.

Deleting or rotating that identity makes macOS see a new application and
therefore requires privacy authorization again.

That one identity covers every profile. `workass_load_profile` publishes
`WORKASS_SHARED_SIGNING_ROOT`, and `workass_codesign_prepare` falls back to it
when the active profile has no signer of its own, so development builds are
signed without a second authorization. Development artifacts keep their own
identifiers (`com.workass.dev.daemon`, `com.workass.dev.agent`) so a
development binary can never satisfy the production daemon's designated
requirement.

Each unbundled binary is its own privacy client, so the first authorization is
per binary: the app, the production daemon, and the development daemon each ask
once. Rebuilds after that are silent.

A build without an identity is not blocked, only reported:
`warning: macOS will ask for permissions again after every rebuild`.

## Verification

Inspect the resulting identities:

```sh
codesign -d -r- /Applications/Workass.app
codesign -d -r- dist-bin/workass-darwin-arm64
```

Neither requirement may be CDHash-only. The package and daemon rebuild scripts
also perform this check themselves.
