import { execFile as nodeExecFile } from 'node:child_process';
import { promisify } from 'node:util';
import { lstat, mkdir, mkdtemp, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { randomUUID } from 'node:crypto';

const execFile = promisify(nodeExecFile);

function diskNumber(device) {
  const match = /^\/dev\/(disk\d+)$/.exec(device);
  if (!match) throw new Error(`hdiutil returned an invalid RAM disk device: ${device}`);
  return match[1];
}

async function run(command, args, execute) {
  const result = await execute(command, args, { encoding: 'utf8', maxBuffer: 1024 * 1024 });
  return typeof result === 'string' ? result : result.stdout;
}

function plistString(plist, key) {
  const escaped = key.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const match = new RegExp(`<key>${escaped}<\\/key>\\s*<string>([^<]*)<\\/string>`).exec(plist);
  return match?.[1]?.replaceAll('&amp;', '&').replaceAll('&lt;', '<').replaceAll('&gt;', '>');
}

function apfsPhysicalStores(plist) {
  const match = /<key>APFSPhysicalStores<\/key>\s*<array>([\s\S]*?)<\/array>/.exec(plist);
  if (!match) return null;
  const stores = [...match[1].matchAll(/<key>APFSPhysicalStore<\/key>\s*<string>(disk\d+)<\/string>/g)].map(item => item[1]);
  const entries = [...match[1].matchAll(/<dict>/g)];
  return entries.length === 1 && stores.length === 1 ? stores : null;
}

async function exists(file) {
  try { await lstat(file); return true; }
  catch (error) { if (error.code === 'ENOENT') return false; throw error; }
}

export async function createTemporaryFixtureVolume({
  platform = process.platform,
  execute = execFile,
  tempDir = os.tmpdir(),
  volumesDir = '/Volumes',
} = {}) {
  if (platform !== 'darwin') {
    const root = await mkdtemp(path.join(tempDir, 'workass-test-fixtures-'));
    return { root, async cleanup() { await rm(root, { recursive: true, force: true }); } };
  }

  let device;
  let root;
  let ownedVolume;
  let cleaned = false;
  const cleanup = async () => {
    if (cleaned) return;
    const errors = [];
    if (root) {
      try { await rm(root, { recursive: true, force: true }); }
      catch (error) { errors.push(error); }
    }
    if (device) {
      try { await run('hdiutil', ['detach', device], execute); }
      catch (error) {
        if (ownedVolume && /Resource busy/i.test(error.message)) {
          try {
            await run('diskutil', ['unmount', 'force', `/dev/${ownedVolume}`], execute);
            await run('hdiutil', ['detach', device], execute);
          } catch (fallbackError) { errors.push(fallbackError); }
        } else errors.push(error);
      }
    }
    cleaned = true;
    if (errors.length) throw new Error(`temporary fixture volume cleanup failed: ${errors.map(error => error.message).join('; ')}`);
  };

  try {
    const attached = (await run('hdiutil', ['attach', '-nomount', 'ram://2097152'], execute)).trim();
    const lines = attached.split(/\r?\n/).map(line => line.trim()).filter(Boolean);
    if (lines.length !== 1 || !/^\/dev\/disk\d+$/.test(lines[0])) throw new Error(`hdiutil did not return exactly one RAM disk device: ${attached}`);
    device = lines[0];
    const disk = diskNumber(device);
    const volumeName = `WorkassTests-${randomUUID()}`;
    const mountPoint = path.join(volumesDir, volumeName);
    if (await exists(mountPoint)) throw new Error(`refusing to use pre-existing fixture mountpoint: ${mountPoint}`);
    await run('diskutil', ['eraseVolume', 'APFS', volumeName, device], execute);
    const info = await run('diskutil', ['info', '-plist', mountPoint], execute);
    const identifier = plistString(info, 'DeviceIdentifier');
    const observedName = plistString(info, 'VolumeName');
    const observedMount = plistString(info, 'MountPoint');
    const stores = apfsPhysicalStores(info);
    if (!identifier || !/^disk\d+s\d+$/.test(identifier) || observedName !== volumeName || observedMount !== mountPoint || !stores || stores.length !== 1 || stores[0] !== disk) {
      throw new Error(`fixture volume ownership verification failed for ${device}`);
    }
    ownedVolume = identifier;
    if (!(await exists(mountPoint))) throw new Error(`fixture volume mountpoint is missing: ${mountPoint}`);
    root = await mkdtemp(path.join(mountPoint, 'fixtures-'));
    return { root, device, mountPoint, async cleanup() { await cleanup(); } };
  } catch (error) {
    try { await cleanup(); }
    catch (cleanupError) { throw new AggregateError([error, cleanupError], 'fixture volume provisioning and cleanup failed'); }
    throw error;
  }
}
