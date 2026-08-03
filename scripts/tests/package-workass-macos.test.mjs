import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const script = fs.readFileSync(path.join(repoRoot, 'scripts', 'package-workass-macos.sh'), 'utf8');
const electronPin = fs.readFileSync(path.join(repoRoot, 'config', 'macos', 'electron.version'), 'utf8').trim();

test('macOS package keeps both bundle and runtime Dock icon resources', () => {
  assert.match(script, /iconutil -c icns .*Workass\.icns/);
  assert.match(script, /cp "\$icon_source" "\$stage\/Contents\/Resources\/Workass\.png"/);
  assert.match(script, /app-icon\.js/);
});

test('macOS package includes every standalone shell safety module', () => {
  assert.match(script, /for shell_file in[^\n]*image-copy\.js/);
  assert.match(script, /for shell_file in[^\n]*profile-singleton\.js/);
  assert.match(script, /desktop\/shell\/image-copy\.test\.js/);
  assert.match(script, /desktop\/shell\/profile-singleton\.test\.js/);
});

test('an explicit package activation carries one controller recovery attempt', () => {
  assert.match(script, /open -na "\$installed"[^\n]*\\\n[\s\S]*WORKASS_CONTROLLER_RECOVERY=1/);
});

test('macOS package invalidates LaunchServices icon caches on every build', () => {
  assert.match(script, /bundle_build=\$\(date -u \+%Y%m%d%H%M%S\)/);
  assert.match(script, /CFBundleVersion -string "\$bundle_build"/);
  assert.match(script, /lsregister -f "\$installed"/);
  assert.doesNotMatch(script, /CFBundleVersion -string 1(?:\s|$)/);
});

test('production package requires one persistent non-ad-hoc signing identity', () => {
  assert.match(script, /workass-codesign\.sh/);
  assert.match(script, /workass_codesign_prepare/);
  assert.match(script, /workass_codesign_sign_app "\$stage"/);
  assert.match(script, /workass_codesign_sign_binary[\s\S]*"\$WORKASS_BUNDLE_ID\.daemon"/);
  assert.doesNotMatch(script, /codesign [^\n]*--sign -/);
});

test('public artifact mode is self-contained and never installs into the live profile', () => {
  assert.match(script, /--artifact-only/);
  assert.match(script, /--portable-runtime/);
  assert.match(script, /--runtime-root/);
  assert.match(script, /Contents\/Resources\/runtime/);
  assert.match(script, /runtime-bootstrap\.js/);
  assert.match(script, /workass_codesign_prepare distribution/);
  assert.match(script, /workass_codesign_sign_app_distribution/);
  assert.match(script, /WORKASS_MACOS_APP_ARTIFACT_READY/);
});

test('macOS package contains native frontier hosts and retires legacy adapter bundles', () => {
  assert.match(script, /frontier-hosts/);
  assert.match(script, /claude-native-host\.mjs/);
  assert.match(script, /codex-native-host\.mjs/);
  assert.match(script, /@anthropic-ai.*claude-agent-sdk.*sdk\.mjs/s);
  assert.match(script, /rm -rf "\$WORKASS_DATA_ROOT\/runtime\/adapters"/);
  assert.doesNotMatch(script, /dist-bin\/adapters|vendor-adapters|@agentclientprotocol\/claude-agent-acp|@agentclientprotocol\/codex-acp/);
});

test('local production install refreshes native provider hosts before copying the runtime', () => {
  const vendorIndex = script.indexOf('vendor-frontier-hosts.sh" --target darwin-arm64 --offline');
  const runtimeCopyIndex = script.indexOf('ditto "$repo_root/dist-bin/frontier-hosts/darwin-arm64" "$WORKASS_DATA_ROOT/runtime/frontier-hosts"');
  assert.ok(vendorIndex >= 0, 'native provider host refresh is missing');
  assert.ok(runtimeCopyIndex > vendorIndex, 'runtime hosts are copied before they are refreshed');
});

test('macOS package consumes the exact audited Electron runtime', () => {
  assert.equal(electronPin, '43.1.1');
  assert.match(script, /workass-electron\.sh/);
  assert.match(script, /--electron-app/);
  assert.match(script, /workass_electron_resolve "\$electron_app_input"/);
  assert.doesNotMatch(script, /npm root -g/);
});

test('production update rejects incompatible identities unless migration is explicit', () => {
  assert.match(script, /--migrate-signing-identity/);
  assert.match(script, /workass_codesign_mutually_compatible "\$installed" "\$stage"/);
  assert.match(script, /workass_codesign_is_legacy_adhoc "\$installed"/);
  assert.match(script, /refusing an update that would reset macOS privacy grants/);
});

test('production update stops old code before swapping the bundle and can roll back', () => {
  const stopIndex = script.indexOf('if ! stop_installed_shell; then');
  const backupIndex = script.indexOf('mv "$installed" "$backup"');
  const installIndex = script.indexOf('mv "$incoming" "$installed"');
  assert.ok(stopIndex >= 0, 'old process stop is missing');
  assert.ok(backupIndex > stopIndex, 'installed bundle moved before old process stopped');
  assert.ok(installIndex > backupIndex, 'incoming bundle installed before rollback backup');
  assert.match(script, /rollback_install\(\)/);
  assert.match(script, /Workass update rolled back/);
});
