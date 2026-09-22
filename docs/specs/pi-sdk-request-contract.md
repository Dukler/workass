# Pi Windows trust and provider-request conformance

Authority: the user's 2026-09-22 request to implement and publish the two
Workass changes identified after the `cloudstrike` investigation: Windows
system certificate roots and regression coverage for instructions/tools.

The native Pi launcher sets `NODE_USE_SYSTEM_CA=1` on Windows before starting
Node. Preserve the provider's other environment entries and certificate
configuration. Do not disable TLS verification, mutate the parent environment,
or change trust settings on other platforms.

Keep the official installed Pi SDK, resource loader, default tools, configured
tool selection and extension discovery. Do not impose a Workass tool allowlist
or return to the obsolete pi-acp launch path.

Add a deterministic provider-request test using the installed official SDK and
a loopback fixture model endpoint, with isolated settings, journals, resources
and no vendor credentials. Inspect outgoing requests, not model prose. Verify
the central Workass bootstrap, project instructions, built-in tool schemas,
extension tools, added/removed tool state, and user-configured PowerShell
selection through fresh sessions and exact resume after host replacement.
No real model or external provider may be called. The local fixture is the
oracle. If the official SDK is absent, report this integration check as skipped;
the publication host must run it with the installed SDK before publishing.

This lane changes Workass only. It does not edit installed third-party provider
packages, Pi profiles, or the release-owned host on San-laptop. No production
activation is authorized by this publication request.

## Lane manifest

- `internal/acp/native_pi.go`, `internal/acp/native_pi_test.go`
- `scripts/tests/pi-native-sdk-context.test.mjs`
- `desktop/acp/mock-pi-context-extension.mjs`
- `desktop/acp/README.md`, this spec

Validate the affected Go package, official SDK fixture boundary, Windows
cross-compilation, repository gate and healthy dev daemon before publishing
through `scripts/release/ship.sh`.
