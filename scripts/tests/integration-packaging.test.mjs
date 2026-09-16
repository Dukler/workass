import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

// Exercise the actual staging scripts with a stale oversized output tree.
// Cached pinned archives are required, as in the offline release lane.
for (const target of ['darwin-arm64', 'windows-amd64']) {
  test(`native integration staging replaces stale engine files: ${target}`, { timeout: 120000 }, () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-integration-stage-'));
    try {
      const old = path.join(root, target, 'node_modules/@oh-my-pi/pi-coding-agent');
      fs.mkdirSync(old, { recursive: true });
      fs.writeFileSync(path.join(old, 'stale'), 'must disappear');
      execFileSync('sh', ['scripts/vendor-frontier-hosts.sh', '--target', target, '--output-root', root, '--offline']);
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
      execFileSync('sh', ['scripts/vendor-node-runtime.sh', '--target', target, '--output-root', path.join(root, 'node'), '--offline']);
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
