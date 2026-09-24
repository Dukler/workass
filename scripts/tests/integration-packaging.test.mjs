import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import { execFileSync } from 'node:child_process';

// Exercise the actual staging scripts with a stale oversized output tree.
// Cached pinned archives are required, as in the offline release lane.
for (const target of ['darwin-arm64', 'windows-amd64']) {
  test(`native integration staging replaces stale engine files: ${target}`, { timeout: 120000 }, () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-integration-stage-'));
    try {
      // Run the real stager from an isolated fixture repository. It has no SDK
      // input override, so provide a local package archive and pin its exact
      // checksum/version in this private copy of the script. This exercises
      // the same checksum and manifest gates without touching repo .dev cache.
      const fixtureRepo = path.join(root, 'fixture-repo');
      const fixtureScripts = path.join(fixtureRepo, 'scripts');
      const fixtureDownloads = path.join(fixtureRepo, '.dev/downloads/claude-agent-sdk/0.3.217');
      const nodeDownloads = path.join(fixtureRepo, '.dev/downloads/node/24.17.0');
      const fixturePackage = path.join(root, 'package');
      fs.mkdirSync(fixtureScripts, { recursive: true });
      fs.mkdirSync(fixtureDownloads, { recursive: true });
      fs.mkdirSync(nodeDownloads, { recursive: true });
      fs.mkdirSync(fixturePackage, { recursive: true });
      const stagingSources = fs.readFileSync('scripts/vendor-frontier-hosts.sh', 'utf8');
      assert.match(stagingSources, /claude_sdk_version=0\.3\.217/);
      assert.match(stagingSources, /claude_sdk_sha256=[a-f0-9]{64}/);
      let fixtureStager = stagingSources;
      fs.copyFileSync('desktop/acp/mock-claude-agent-sdk.mjs', path.join(fixturePackage, 'sdk.mjs'));
      fs.writeFileSync(path.join(fixturePackage, 'package.json'), JSON.stringify({ name: '@anthropic-ai/claude-agent-sdk', version: '0.3.217' }));
      fs.writeFileSync(path.join(fixturePackage, 'LICENSE.md'), 'Deterministic test fixture license.');
      fs.writeFileSync(path.join(fixturePackage, 'README.md'), 'Deterministic test fixture package.');
      const archive = path.join(fixtureDownloads, 'claude-agent-sdk-0.3.217.tgz');
      execFileSync('tar', ['-czf', archive, '-C', root, 'package']);
      const fixtureHash = crypto.createHash('sha256').update(fs.readFileSync(archive)).digest('hex');
      fixtureStager = fixtureStager.replace(/claude_sdk_sha256=[a-f0-9]{64}/, `claude_sdk_sha256=${fixtureHash}`);
      fs.writeFileSync(path.join(fixtureScripts, 'vendor-frontier-hosts.sh'), fixtureStager);
      for (const file of ['claude-native-host.mjs', 'codex-native-host.mjs', 'omp-native-host.mjs', 'omp-installed-host.mjs', 'omp-sdk-extension.mjs', 'pi-native-host.mjs']) {
        fs.copyFileSync(path.join('scripts', file), path.join(fixtureScripts, file));
      }
      let nodeStager = fs.readFileSync('scripts/vendor-node-runtime.sh', 'utf8');
      const nodeArchive = target === 'darwin-arm64' ? 'node-v24.17.0-darwin-arm64.tar.xz' : 'node-v24.17.0-win-x64.zip';
      const nodeTree = path.join(root, target === 'darwin-arm64' ? 'node-v24.17.0-darwin-arm64' : 'node-v24.17.0-win-x64');
      fs.mkdirSync(nodeTree, { recursive: true });
      fs.writeFileSync(path.join(nodeTree, 'LICENSE'), 'Deterministic test fixture runtime license.');
      if (target === 'darwin-arm64') {
        fs.mkdirSync(path.join(nodeTree, 'bin'), { recursive: true });
        fs.writeFileSync(path.join(nodeTree, 'bin/node'), '#!/bin/sh\nexit 0\n', { mode: 0o755 });
        execFileSync('tar', ['-cJf', path.join(nodeDownloads, nodeArchive), '-C', root, path.basename(nodeTree)]);
      } else {
        fs.writeFileSync(path.join(nodeTree, 'node.exe'), 'fixture executable');
        execFileSync('zip', ['-qr', path.join(nodeDownloads, nodeArchive), path.basename(nodeTree)], { cwd: root });
      }
      const nodeHash = crypto.createHash('sha256').update(fs.readFileSync(path.join(nodeDownloads, nodeArchive))).digest('hex');
      nodeStager = nodeStager.replace(target === 'darwin-arm64'
        ? /cf7e9152d7bd86c140f6eccf3577abfbaf8960be1ca49d9d900e8484984dcb9a/
        : /f2aa33b35b75aca5f3f7b85675a6f6423201053e9381911e64961f3bda2528ab/, nodeHash);
      fs.writeFileSync(path.join(fixtureScripts, 'vendor-node-runtime.sh'), nodeStager);
      const old = path.join(root, target, 'node_modules/@oh-my-pi/pi-coding-agent');
      fs.mkdirSync(old, { recursive: true });
      fs.writeFileSync(path.join(old, 'stale'), 'must disappear');
      execFileSync('sh', [path.join(fixtureScripts, 'vendor-frontier-hosts.sh'), '--target', target, '--output-root', root, '--offline']);
      const staged = path.join(root, target);
      for (const provider of ['claude', 'codex', 'omp']) assert.ok(fs.existsSync(path.join(staged, `${provider}-native-host.mjs`)));
      assert.ok(fs.existsSync(path.join(staged, 'omp-sdk-extension.mjs')));
      assert.ok(fs.existsSync(path.join(staged, 'omp-installed-host.mjs')));
      assert.deepEqual(fs.readdirSync(path.join(staged, 'node_modules')), ['@anthropic-ai']);
      assert.deepEqual(fs.readdirSync(path.join(staged, 'node_modules/@anthropic-ai')), ['claude-agent-sdk']);
      const sdk = path.join(staged, 'node_modules/@anthropic-ai/claude-agent-sdk');
      assert.deepEqual(fs.readdirSync(sdk).sort(), ['LICENSE.md','README.md','package.json','sdk.mjs'].sort());
      // Import the actual staged client with no inference or user auth access.
      execFileSync(process.execPath, ['--input-type=module', '-e', `const sdk = await import(${JSON.stringify('file://' + path.join(sdk, 'sdk.mjs'))}); if (typeof sdk.query !== 'function') throw new Error('Missing Claude query');`]);
      assert.ok(!fs.existsSync(path.join(staged, 'bun')));
      assert.ok(!fs.existsSync(path.join(staged, 'bun.exe')));
      let bytes = 0;
      function size(dir) { for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const file = path.join(dir, entry.name);
        if (entry.isDirectory()) size(file); else bytes += fs.statSync(file).size;
      } }
      size(staged);
      assert.ok(bytes < 2_000_000, `connection hosts unexpectedly grew to ${bytes} bytes`);
      execFileSync('sh', [path.join(fixtureScripts, 'vendor-node-runtime.sh'), '--target', target, '--output-root', path.join(root, 'node'), '--offline']);
      const node = path.join(root, 'node', target);
      assert.ok(fs.existsSync(path.join(node, target.startsWith('windows') ? 'node.exe' : 'bin/node')));
      assert.ok(fs.existsSync(path.join(node, 'LICENSE')));
      assert.deepEqual(fs.readdirSync(node).sort(), target.startsWith('windows') ? ['LICENSE','node.exe'] : ['LICENSE','bin']);
      for (const relative of ['node_modules', 'lib/node_modules', 'include', 'share', 'bin/npm', 'npm.ps1', 'npx.ps1', 'install_tools.bat']) {
        assert.ok(!fs.existsSync(path.join(node, relative)), `unnecessary Node file: ${relative}`);
      }
    } finally { fs.rmSync(root, { recursive: true, force: true }); }
  });
}
