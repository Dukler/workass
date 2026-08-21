import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const script = fs.readFileSync(path.join(repoRoot, 'scripts', 'package-workass-macos.sh'), 'utf8');
const installer = fs.readFileSync(path.join(repoRoot, 'scripts', 'install-workass-macos.sh'), 'utf8');
const localUpdater = fs.readFileSync(path.join(repoRoot, 'scripts', 'stage-macos-local-update.sh'), 'utf8');
const electronPin = fs.readFileSync(path.join(repoRoot, 'config', 'macos', 'electron.version'), 'utf8').trim();

test('macOS package keeps both bundle and runtime Dock icon resources', () => {
  assert.match(script, /iconutil -c icns .*Workass\.icns/);
  assert.match(script, /cp "\$icon_source" "\$stage\/Contents\/Resources\/Workass\.png"/);
  assert.match(script, /app-icon\.js/);
});

test('macOS package includes every standalone shell safety module without rerunning their gate', () => {
  assert.match(script, /for shell_file in[^\n]*image-copy\.js/);
  assert.match(script, /for shell_file in[^\n]*profile-singleton\.js/);
  assert.match(script, /for shell_file in[^\n]*update-lock-recovery\.js/);
  assert.match(script, /for shell_file in[^\n]*update-progress\.js/);
  assert.match(script, /for shell_file in[^\n]*update-manager\.js/);
  assert.match(script, /for shell_file in[^\n]*update-worker\.js/);
  assert.doesNotMatch(script, /desktop\/shell\/[^\s]+\.test\.js/);
  assert.match(script, /node --test scripts\/tests\/package-workass-macos\.test\.mjs/);
});

test('an explicit package activation carries one controller recovery attempt', () => {
  assert.match(installer, /open -na "\$installed"[^\n]*\\\n[\s\S]*WORKASS_CONTROLLER_RECOVERY=1/);
});

test('candidate and rollback health waits can recover the renderer in place once', () => {
  assert.match(installer, /recover_shell_in_place\(\)/);
  assert.match(installer, /-X POST[\s\S]*\/__workass-shell\/reload\?recoverController=1/);
  assert.match(installer, /wait_for_new_release\(\)[\s\S]*recovery_attempted=0[\s\S]*recover_shell_in_place/);
  assert.match(installer, /wait_for_previous_release\(\)[\s\S]*recovery_attempted=0[\s\S]*recover_shell_in_place/);
});

test('macOS package invalidates LaunchServices icon caches on every build', () => {
  assert.match(script, /bundle_build=\$\(date -u \+%Y%m%d%H%M%S\)/);
  assert.match(script, /CFBundleVersion -string "\$bundle_build"/);
  assert.match(installer, /lsregister -f "\$installed"/);
  assert.doesNotMatch(script, /CFBundleVersion -string 1(?:\s|$)/);
});

test('production package requires one persistent non-ad-hoc signing identity', () => {
  assert.match(script, /workass-codesign\.sh/);
  assert.match(script, /workass_codesign_prepare/);
  assert.match(script, /workass_codesign_sign_app "\$stage"/);
  assert.match(script, /cp "\$daemon_source" "\$runtime_stage\/workass"/);
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

test('local Mac packages use the filesystem feed and public signed packages use GitHub', () => {
  assert.match(script, /WORKASS_UPDATE_CHANNEL=local/);
  assert.match(script, /WORKASS_UPDATE_CHANNEL=github/);
  assert.match(script, /if \[ "\$release_signing" -eq 1 \]; then/);
});

test('the local Mac feed is a complete locally signed archive published manifest-last', () => {
  assert.match(localUpdater, /package-workass-macos\.sh/);
  assert.match(localUpdater, /--artifact-only/);
  assert.match(localUpdater, /codesign -d -r-/);
  assert.match(localUpdater, /shasum -a 256/);
  assert.match(localUpdater, /url: archiveName/);
  assert.ok(localUpdater.indexOf('mv -f "$archive_incoming" "$archive"') < localUpdater.indexOf('mv -f "$feed_incoming" "$feed"'));
  assert.doesNotMatch(localUpdater, /notarytool|stapler|github\.com/);
});

test('every installed macOS app is one self-contained shell and daemon release', () => {
  assert.match(script, /portable_runtime=1/);
  assert.match(script, /vendor-node-runtime\.sh" --target darwin-arm64 --offline/);
  assert.match(script, /daemon_source="\$packaged_daemon"/);
  assert.match(script, /-X main\.daemonVersion=\$bundle_version/);
  assert.match(script, /plutil -replace version -string "\$bundle_version" "\$stage\/Contents\/Resources\/app\/package\.json"/);
  assert.match(installer, /"\$shell_version" = "\$target_version"/);
});

test('macOS package contains native frontier hosts and retires legacy adapter bundles', () => {
  assert.match(script, /frontier-hosts/);
  assert.match(script, /claude-native-host\.mjs/);
  assert.match(script, /codex-native-host\.mjs/);
  assert.match(script, /@anthropic-ai.*claude-agent-sdk.*sdk\.mjs/s);
  assert.doesNotMatch(script, /WORKASS_DATA_ROOT\/runtime\/adapters/);
  assert.doesNotMatch(script, /dist-bin\/adapters|vendor-adapters|@agentclientprotocol\/claude-agent-acp|@agentclientprotocol\/codex-acp/);
});

test('local production install embeds native provider hosts instead of mutating a live runtime', () => {
  const vendorIndex = script.indexOf('vendor-frontier-hosts.sh" --target darwin-arm64 --offline');
  assert.ok(vendorIndex >= 0, 'native provider host refresh is missing');
  assert.match(script, /ditto "\$frontier_hosts_source" "\$runtime_stage\/frontier-hosts\/darwin-arm64"/);
  assert.doesNotMatch(script, /WORKASS_DATA_ROOT\/runtime\/frontier-hosts/);
});

test('manual seed install restores the prior launch agent when app health gates fail', () => {
  assert.match(installer, /launch_agent_backup/);
  assert.match(installer, /launchctl bootout "\$launchd_domain" "\$launch_agent_path"/);
  assert.match(installer, /launchctl bootstrap "\$launchd_domain" "\$launch_agent_path"/);
});

test('macOS package consumes the exact audited Electron runtime', () => {
  assert.equal(electronPin, '43.1.1');
  assert.match(script, /workass-electron\.sh/);
  assert.match(script, /--electron-app/);
  assert.match(script, /workass_electron_resolve "\$electron_app_input"/);
  assert.doesNotMatch(script, /npm root -g/);
});

test('production update rejects incompatible signing identities', () => {
  assert.doesNotMatch(script, /--migrate-signing-identity/);
  assert.doesNotMatch(installer, /--migrate-signing-identity/);
  assert.match(installer, /workass_codesign_mutually_compatible "\$installed" "\$candidate"/);
  assert.match(installer, /refusing an install that would reset macOS privacy grants/);
});

test('a broken installed seal hard-stops without a mutation path', () => {
  assert.doesNotMatch(installer, /repair-broken-installed-seal|broken_seal_repair/);
  assert.match(installer, /signature seal is broken; refusing to replace it automatically/);
});

test('production update stops old code before swapping the bundle and can roll back', () => {
  const stopIndex = installer.indexOf('if ! stop_installed_shell; then');
  const backupIndex = installer.indexOf('mv "$installed" "$backup"');
  const installIndex = installer.indexOf('mv "$incoming" "$installed"');
  assert.ok(stopIndex >= 0, 'old process stop is missing');
  assert.ok(backupIndex > stopIndex, 'installed bundle moved before old process stopped');
  assert.ok(installIndex > backupIndex, 'incoming bundle installed before rollback backup');
  assert.match(installer, /rollback_install\(\)/);
  assert.match(installer, /Workass install rolled back/);
});

test('replaying the exact healthy signed release cannot restart Workass', () => {
  assert.match(installer, /candidate_cdhash=\$\(workass_codesign_cdhash "\$candidate"\)/);
  assert.match(installer, /installed_cdhash=\$\(workass_codesign_cdhash "\$installed"\)/);
  assert.match(installer, /"\$candidate_cdhash" = "\$installed_cdhash".*new_release_healthy/);
  const noOpReceipt = installer.indexOf('WORKASS_MACOS_INSTALL_ALREADY_CURRENT');
  const stopLiveShell = installer.indexOf('if ! stop_installed_shell;');
  assert.ok(noOpReceipt >= 0 && noOpReceipt < stopLiveShell, 'identical healthy release was not fenced before the live shell stop');
});

test('candidate building fails fast on missing tools and produces a complete source artifact', () => {
  const toolCheck = script.indexOf('missing build tool before package gate');
  const gate = script.indexOf('[package] repository gate');
  assert.ok(toolCheck >= 0 && toolCheck < gate, 'toolchain check must happen before the slow gate');
  const packageContracts = script.indexOf('[package] testing installer, signing, and package contracts');
  assert.ok(packageContracts >= 0 && packageContracts < gate, 'fast package contracts must happen before the slow gate');
  assert.match(script, /if \[ "\$runtime_input_root" = "\$repo_root\/dist-bin" \]; then/);
  assert.match(script, /WORKASS_GATE_FRESH=1[\s\S]{0,80}scripts\/gate\.sh/);
  assert.doesNotMatch(script, /if \[ -z "\$artifact_output" \] && \[ "\$runtime_input_root" = "\$repo_root\/dist-bin" \]/);
  assert.match(script, /"\$repo_root\/scripts\/install-workass-macos\.sh" "\$@"/);
});

test('candidate installer has no build tool or repository gate dependency', () => {
  assert.doesNotMatch(installer, /(?:^|\n)[ \t]*(?:go|npm|npx)[ \t]|scripts\/gate\.sh/);
  assert.match(installer, /candidate shell and daemon versions differ/);
  assert.match(installer, /workass_codesign_verify_stable "\$candidate" true/);
  assert.match(installer, /appVersion/);
  assert.match(installer, /secure/);
  assert.match(installer, /persistent/);
  assert.match(installer, /cdpAttached/);
  assert.match(installer, /agentControl/);
  assert.match(installer, /WORKASS_MACOS_INSTALL_HEALTHY/);
});

test('production package and installer never create persistent submitted jobs', () => {
  assert.doesNotMatch(script, /launchctl\s+submit/);
  assert.doesNotMatch(installer, /launchctl\s+submit/);
});
