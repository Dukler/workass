'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const { runNuclearUpdate } = require('./update-nuke');

function makeTrees(root) {
  const installDir = path.join(root, 'install');
  fs.mkdirSync(installDir, { recursive: true });
  fs.writeFileSync(path.join(installDir, 'Workass.exe'), 'old-exe');
  fs.writeFileSync(path.join(installDir, 'stale.txt'), 'delete-me');
  return installDir;
}

test('the nuclear run kills, downloads, verifies, swaps, boots, and cleans up', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-nuke-ok-'));
  const installDir = makeTrees(root);
  const stagedRoot = path.join(root, 'extracted', 'Workass-1.2.0-windows-amd64');
  fs.mkdirSync(stagedRoot, { recursive: true });
  fs.writeFileSync(path.join(stagedRoot, 'Workass.exe'), 'new-exe');
  const kills = [];
  let launched = false;
  const receipt = await runNuclearUpdate({
    platform: 'darwin',
    downloadUrl: 'https://releases.example.test/release.zip',
    sha256: '',
    targetVersion: '1.2.0',
    installDir,
    trashDir: path.join(root, 'trash'),
    workRoot: root,
    healthURL: '',
    logPath: path.join(root, 'nuke.log'),
    env: {},
  }, {
    kill: () => kills.push(1),
    download: async (request) => {
      const zipPath = path.join(request.workRoot, 'release.zip');
      fs.writeFileSync(zipPath, 'zip');
      return zipPath;
    },
    extract: (request) => {
      assert.equal(fs.existsSync(path.join(request.workRoot, 'release.zip')), true);
      return stagedRoot;
    },
    waitHealthy: async () => true,
    sleep: async () => {},
    launch: () => { launched = true; },
  });

  assert.equal(receipt.ok, true);
  assert.equal(kills.length, 2, 'processes are killed before and after failure paths only');
  assert.equal(launched, true);
  assert.equal(fs.readFileSync(path.join(installDir, 'Workass.exe'), 'utf8'), 'new-exe');
  assert.equal(fs.existsSync(path.join(root, 'trash')), false, 'trash is deleted after a healthy boot');
  assert.equal(fs.existsSync(path.join(installDir, 'stale.txt')), false, 'old contents do not survive the swap');
});

test('a download failure puts the previous contents back and relaunches them', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-nuke-dl-'));
  const installDir = makeTrees(root);
  const receipt = await runNuclearUpdate({
    platform: 'darwin',
    downloadUrl: 'https://releases.example.test/release.zip',
    sha256: '', targetVersion: '1.2.0',
    installDir, trashDir: path.join(root, 'trash'), workRoot: root,
    healthURL: '', logPath: path.join(root, 'nuke.log'), env: {},
  }, {
    kill: () => {},
    download: async () => { throw new Error('offline'); },
    sleep: async () => {},
    launch: () => {},
  });

  assert.equal(receipt.ok, false);
  assert.equal(receipt.restored, true);
  assert.match(receipt.error, /offline/);
  assert.equal(fs.readFileSync(path.join(installDir, 'Workass.exe'), 'utf8'), 'old-exe',
    'the previous tree must be back in place after a failed download');
});

test('an unhealthy new release restores the previous contents through the trash', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-nuke-sick-'));
  const installDir = makeTrees(root);
  const stagedRoot = path.join(root, 'extracted', 'tree');
  fs.mkdirSync(stagedRoot, { recursive: true });
  fs.writeFileSync(path.join(stagedRoot, 'Workass.exe'), 'sick-exe');
  const receipt = await runNuclearUpdate({
    platform: 'darwin',
    downloadUrl: 'https://releases.example.test/release.zip',
    sha256: '', targetVersion: '1.2.0',
    installDir, trashDir: path.join(root, 'trash'), workRoot: root,
    healthURL: '', logPath: path.join(root, 'nuke.log'), env: {},
  }, {
    kill: () => {},
    download: async (request) => {
      const zipPath = path.join(request.workRoot, 'release.zip');
      fs.writeFileSync(zipPath, 'zip');
      return zipPath;
    },
    extract: () => stagedRoot,
    waitHealthy: async () => false,
    sleep: async () => {},
    launch: () => {},
  });

  assert.equal(receipt.ok, false);
  assert.equal(receipt.restored, true);
  assert.equal(fs.readFileSync(path.join(installDir, 'Workass.exe'), 'utf8'), 'old-exe',
    'rollback must bring the previous executable back');
  assert.equal(fs.existsSync(path.join(root, 'trash')), false, 'trash is consumed by the restore');
});

test('a swap that cannot clear the install folder aborts without losing anything', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-nuke-lock-'));
  const installDir = makeTrees(root);
  const receipt = await runNuclearUpdate({
    platform: 'darwin',
    downloadUrl: 'https://releases.example.test/release.zip',
    sha256: '', targetVersion: '1.2.0',
    installDir, trashDir: path.join(root, 'trash'), workRoot: root,
    healthURL: '', logPath: path.join(root, 'nuke.log'), env: {},
  }, {
    kill: () => {},
    download: async (request) => {
      const zipPath = path.join(request.workRoot, 'release.zip');
      fs.writeFileSync(zipPath, 'zip');
      return zipPath;
    },
    extract: () => path.join(root, 'extracted-tree'),
    moveToTrash: () => { throw new Error('EBUSY: files locked'); },
    waitHealthy: async () => true,
    sleep: async () => {},
    launch: () => {},
  });

  assert.equal(receipt.ok, false);
  assert.equal(receipt.restored, true);
  assert.match(receipt.error, /EBUSY|locked/);
  assert.equal(fs.readFileSync(path.join(installDir, 'Workass.exe'), 'utf8'), 'old-exe');
});
