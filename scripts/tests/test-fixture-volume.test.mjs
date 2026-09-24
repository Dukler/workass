import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readdir, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { createTemporaryFixtureVolume } from '../test-fixture-volume.mjs';

async function testDirs(t) {
  const base = await mkdtemp(path.join(os.tmpdir(), 'workass-volume-test-'));
  t.after(() => rm(base, { recursive: true, force: true }));
  const volumes = path.join(base, 'Volumes');
  await mkdir(volumes);
  return { base, volumes };
}

test('creates and cleans only its newly allocated RAM device', async t => {
  const { volumes } = await testDirs(t);
  const calls = [];
  let volumeName;
  const execute = async (command, args) => {
    calls.push([command, ...args]);
    if (command === 'hdiutil' && args[0] === 'attach') return { stdout: '/dev/disk42\n' };
    if (command === 'diskutil' && args[0] === 'eraseVolume') {
      volumeName = args[2];
      await mkdir(path.join(volumes, volumeName));
      return { stdout: '' };
    }
    if (command === 'diskutil' && args[0] === 'info') {
      assert.equal(args[2], path.join(volumes, volumeName));
      return { stdout: `<plist><dict><key>DeviceIdentifier</key><string>disk7s1</string><key>ParentWholeDisk</key><string>disk7</string><key>APFSContainerReference</key><string>disk7</string><key>APFSPhysicalStores</key><array><dict><key>APFSPhysicalStore</key><string>disk42</string></dict></array><key>VolumeName</key><string>${volumeName}</string><key>MountPoint</key><string>${path.join(volumes, volumeName)}</string></dict></plist>` };
    }
    if (command === 'hdiutil' && args[0] === 'detach') return { stdout: '' };
    throw new Error(`unexpected command ${command} ${args.join(' ')}`);
  };
  const fixture = await createTemporaryFixtureVolume({ platform: 'darwin', volumesDir: volumes, execute });
  assert.ok(fixture.root.startsWith(path.join(volumes, volumeName) + path.sep));
  await fixture.cleanup();
  assert.deepEqual(calls.filter(call => call[0] === 'hdiutil' && call[1] === 'detach'), [['hdiutil', 'detach', '/dev/disk42']]);
  assert.deepEqual(await readdir(path.join(volumes, volumeName)), []);
});

test('ownership mismatch detaches only the newly allocated device and never uses the observed foreign mount', async t => {
  const { volumes } = await testDirs(t);
  let volumeName;
  const calls = [];
  const execute = async (command, args) => {
    calls.push([command, ...args]);
    if (command === 'hdiutil' && args[0] === 'attach') return { stdout: '/dev/disk42\n' };
    if (command === 'diskutil' && args[0] === 'eraseVolume') { volumeName = args[2]; return { stdout: '' }; }
    if (command === 'diskutil' && args[0] === 'info') return { stdout: `<plist><dict><key>DeviceIdentifier</key><string>disk7s1</string><key>APFSPhysicalStores</key><array><dict><key>APFSPhysicalStore</key><string>disk99</string></dict></array><key>VolumeName</key><string>${volumeName}</string><key>MountPoint</key><string>${path.join(volumes, volumeName)}</string></dict></plist>` };
    if (command === 'hdiutil' && args[0] === 'detach') return { stdout: '' };
    throw new Error(`unexpected command ${command} ${args.join(' ')}`);
  };
  await assert.rejects(createTemporaryFixtureVolume({ platform: 'darwin', volumesDir: volumes, execute }), /ownership verification failed/);
  assert.ok(volumeName.startsWith('WorkassTests-'));
  assert.deepEqual(calls.filter(call => call[0] === 'hdiutil' && call[1] === 'detach'), [['hdiutil', 'detach', '/dev/disk42']]);
  assert.equal(calls.some(call => call.includes('/Volumes/foreign')), false);
});

test('reports detach failure without retrying or broad device discovery', async t => {
  const { volumes } = await testDirs(t);
  let volumeName;
  const calls = [];
  const execute = async (command, args) => {
    calls.push([command, ...args]);
    if (command === 'hdiutil' && args[0] === 'attach') return { stdout: '/dev/disk42\n' };
    if (command === 'diskutil' && args[0] === 'eraseVolume') { volumeName = args[2]; await mkdir(path.join(volumes, volumeName)); return { stdout: '' }; }
    if (command === 'diskutil' && args[0] === 'info') return { stdout: `<plist><dict><key>DeviceIdentifier</key><string>disk7s1</string><key>APFSPhysicalStores</key><array><dict><key>APFSPhysicalStore</key><string>disk42</string></dict></array><key>VolumeName</key><string>${volumeName}</string><key>MountPoint</key><string>${path.join(volumes, volumeName)}</string></dict></plist>` };
    if (command === 'hdiutil' && args[0] === 'detach') throw new Error('detach rejected');
    throw new Error(`unexpected command ${command} ${args.join(' ')}`);
  };
  const fixture = await createTemporaryFixtureVolume({ platform: 'darwin', volumesDir: volumes, execute });
  await assert.rejects(fixture.cleanup(), /cleanup failed: detach rejected/);
  assert.equal(calls.filter(call => call[0] === 'hdiutil' && call[1] === 'detach').length, 1);
  assert.equal(calls.some(call => call[1] === 'info' && call[2] === '-plist' && call[3] === 'all'), false);
});

test('non-macOS uses an ordinary temporary directory', async t => {
  const { base } = await testDirs(t);
  const fixture = await createTemporaryFixtureVolume({ platform: 'linux', tempDir: base, execute: async () => { throw new Error('must not execute'); } });
  assert.ok(fixture.root.startsWith(base + path.sep));
  await fixture.cleanup();
  assert.ok(!(await readdir(base)).includes(path.basename(fixture.root)));
});
