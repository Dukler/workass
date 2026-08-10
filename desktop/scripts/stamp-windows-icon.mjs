#!/usr/bin/env node

// Replace Electron's main executable icon in place without rcedit, Wine, npm,
// or any Windows-side tool. Electron ships four RT_ICON slots (16/32/48/256);
// Workass reuses those exact resource IDs and falls back to the largest smaller
// Workass image when an encoded image cannot fit its existing slot.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const RT_ICON = 3;
const RT_GROUP_ICON = 14;
const PE32_PLUS = 0x20b;
const AMD64 = 0x8664;
const PNG_SIGNATURE = '89504e470d0a1a0a';

function requiredRange(bytes, offset, size, label) {
  if (!Number.isSafeInteger(offset) || !Number.isSafeInteger(size) || offset < 0 || size < 0 || offset + size > bytes.length) {
    throw new Error(`${label} is outside the file`);
  }
}

function parseIco(bytes) {
  requiredRange(bytes, 0, 6, 'ICO header');
  if (bytes.readUInt16LE(0) !== 0 || bytes.readUInt16LE(2) !== 1) throw new Error('icon is not a Windows ICO');
  const count = bytes.readUInt16LE(4);
  if (count < 1 || count > 64) throw new Error('icon has an invalid image count');
  requiredRange(bytes, 6, count * 16, 'ICO directory');
  const images = [];
  for (let index = 0; index < count; index += 1) {
    const entry = 6 + index * 16;
    const width = bytes[entry] || 256;
    const height = bytes[entry + 1] || 256;
    const planes = bytes.readUInt16LE(entry + 4) || 1;
    const bpp = bytes.readUInt16LE(entry + 6) || 32;
    const size = bytes.readUInt32LE(entry + 8);
    const offset = bytes.readUInt32LE(entry + 12);
    requiredRange(bytes, offset, size, `ICO image ${index + 1}`);
    const data = Buffer.from(bytes.subarray(offset, offset + size));
    if (data.subarray(0, 8).toString('hex') === PNG_SIGNATURE) {
      requiredRange(data, 0, 24, `ICO PNG image ${index + 1}`);
      if (data.readUInt32BE(16) !== width || data.readUInt32BE(20) !== height) throw new Error('ICO PNG dimensions do not match its directory');
    } else {
      requiredRange(data, 0, 16, `ICO bitmap image ${index + 1}`);
      if (data.readUInt32LE(0) < 16 || data.readInt32LE(4) !== width || Math.abs(data.readInt32LE(8)) !== height * 2) {
        throw new Error('ICO bitmap dimensions do not match its directory');
      }
    }
    images.push({ width, height, planes, bpp, data });
  }
  return images;
}

function parsePE(bytes) {
  requiredRange(bytes, 0, 0x40, 'DOS header');
  if (bytes.toString('ascii', 0, 2) !== 'MZ') throw new Error('executable has no DOS header');
  const pe = bytes.readUInt32LE(0x3c);
  requiredRange(bytes, pe, 24, 'PE header');
  if (bytes.toString('ascii', pe, pe + 4) !== 'PE\0\0') throw new Error('executable has no PE signature');
  if (bytes.readUInt16LE(pe + 4) !== AMD64) throw new Error('executable is not Windows x86-64');
  const sectionCount = bytes.readUInt16LE(pe + 6);
  const optionalSize = bytes.readUInt16LE(pe + 20);
  const optional = pe + 24;
  requiredRange(bytes, optional, optionalSize, 'PE optional header');
  if (bytes.readUInt16LE(optional) !== PE32_PLUS || optionalSize < 176) throw new Error('executable is not PE32+');

  const dataDirectories = optional + 112;
  const securityOffset = bytes.readUInt32LE(dataDirectories + 4 * 8);
  const securitySize = bytes.readUInt32LE(dataDirectories + 4 * 8 + 4);
  if (securityOffset || securitySize) throw new Error('refusing to replace the icon of a signed executable');
  const resourceRVA = bytes.readUInt32LE(dataDirectories + 2 * 8);
  const resourceSize = bytes.readUInt32LE(dataDirectories + 2 * 8 + 4);
  if (!resourceRVA || resourceSize < 24) throw new Error('executable has no resource directory');

  const sections = [];
  const sectionTable = optional + optionalSize;
  requiredRange(bytes, sectionTable, sectionCount * 40, 'PE section table');
  for (let index = 0; index < sectionCount; index += 1) {
    const entry = sectionTable + index * 40;
    sections.push({
      virtualSize: bytes.readUInt32LE(entry + 8),
      virtualAddress: bytes.readUInt32LE(entry + 12),
      rawSize: bytes.readUInt32LE(entry + 16),
      rawOffset: bytes.readUInt32LE(entry + 20),
    });
  }
  const rvaToOffset = (rva, size, label) => {
    const section = sections.find((candidate) => rva >= candidate.virtualAddress && rva - candidate.virtualAddress + size <= candidate.rawSize);
    if (!section) throw new Error(`${label} is not backed by executable data`);
    const offset = section.rawOffset + rva - section.virtualAddress;
    requiredRange(bytes, offset, size, label);
    return offset;
  };
  const resourceOffset = rvaToOffset(resourceRVA, Math.min(resourceSize, 16), 'resource directory');
  return { bytes, resourceRVA, resourceSize, resourceOffset, rvaToOffset };
}

function directoryEntries(pe, relative, label) {
  if (relative < 0 || relative + 16 > pe.resourceSize) throw new Error(`${label} directory is invalid`);
  const offset = pe.resourceOffset + relative;
  requiredRange(pe.bytes, offset, 16, `${label} directory`);
  const count = pe.bytes.readUInt16LE(offset + 12) + pe.bytes.readUInt16LE(offset + 14);
  if (count < 1 || count > 4096 || relative + 16 + count * 8 > pe.resourceSize) throw new Error(`${label} directory has an invalid entry count`);
  requiredRange(pe.bytes, offset + 16, count * 8, `${label} entries`);
  const entries = [];
  for (let index = 0; index < count; index += 1) {
    const at = offset + 16 + index * 8;
    const name = pe.bytes.readUInt32LE(at);
    const target = pe.bytes.readUInt32LE(at + 4);
    entries.push({
      named: (name & 0x80000000) !== 0,
      id: name & 0xffff,
      directory: (target & 0x80000000) !== 0,
      relative: target & 0x7fffffff,
    });
  }
  return entries;
}

function resourceLeaf(pe, typeID, resourceID = null) {
  const type = directoryEntries(pe, 0, 'root').find((entry) => !entry.named && entry.id === typeID);
  if (!type?.directory) throw new Error(`executable has no resource type ${typeID}`);
  const names = directoryEntries(pe, type.relative, `resource type ${typeID}`).filter((entry) => !entry.named);
  const name = resourceID == null ? names[0] : names.find((entry) => entry.id === resourceID);
  if (!name?.directory) throw new Error(`executable has no resource ${typeID}/${resourceID ?? '*'}`);
  const language = directoryEntries(pe, name.relative, `resource ${typeID}/${name.id}`)[0];
  if (!language || language.directory) throw new Error(`resource ${typeID}/${name.id} has no data leaf`);
  if (language.relative + 16 > pe.resourceSize) throw new Error(`resource ${typeID}/${name.id} data entry is invalid`);
  const dataEntryOffset = pe.resourceOffset + language.relative;
  requiredRange(pe.bytes, dataEntryOffset, 16, `resource ${typeID}/${name.id} data entry`);
  const dataRVA = pe.bytes.readUInt32LE(dataEntryOffset);
  const size = pe.bytes.readUInt32LE(dataEntryOffset + 4);
  const dataOffset = pe.rvaToOffset(dataRVA, size, `resource ${typeID}/${name.id}`);
  return { id: name.id, dataEntryOffset, dataOffset, size };
}

function parseIconGroup(pe) {
  const leaf = resourceLeaf(pe, RT_GROUP_ICON);
  requiredRange(pe.bytes, leaf.dataOffset, 6, 'icon group');
  if (pe.bytes.readUInt16LE(leaf.dataOffset) !== 0 || pe.bytes.readUInt16LE(leaf.dataOffset + 2) !== 1) {
    throw new Error('main icon group is invalid');
  }
  const count = pe.bytes.readUInt16LE(leaf.dataOffset + 4);
  if (count < 1 || count > 64 || 6 + count * 14 > leaf.size) throw new Error('main icon group has an invalid image count');
  const entries = [];
  for (let index = 0; index < count; index += 1) {
    const offset = leaf.dataOffset + 6 + index * 14;
    entries.push({
      offset,
      width: pe.bytes[offset] || 256,
      height: pe.bytes[offset + 1] || 256,
      planes: pe.bytes.readUInt16LE(offset + 4),
      bpp: pe.bytes.readUInt16LE(offset + 6),
      size: pe.bytes.readUInt32LE(offset + 8),
      id: pe.bytes.readUInt16LE(offset + 12),
    });
  }
  return entries;
}

function chooseImage(images, slot, capacity) {
  const candidates = images
    .filter((image) => image.width <= slot.width && image.height <= slot.height && image.data.length <= capacity)
    .sort((left, right) => right.width - left.width || right.height - left.height || right.bpp - left.bpp);
  const exact = candidates.find((image) => image.width === slot.width && image.height === slot.height);
  return exact || candidates[0] || null;
}

function inspectStampedIcon(executableBytes, icoBytes) {
  const pe = parsePE(executableBytes);
  const images = parseIco(icoBytes);
  const group = parseIconGroup(pe);
  const stamped = [];
  for (const slot of group) {
    const leaf = resourceLeaf(pe, RT_ICON, slot.id);
    const expected = images.find((image) => image.width === slot.width && image.height === slot.height && image.data.length === leaf.size);
    if (!expected || !executableBytes.subarray(leaf.dataOffset, leaf.dataOffset + leaf.size).equals(expected.data)) {
      throw new Error(`executable icon ${slot.width}x${slot.height} does not match Workass`);
    }
    stamped.push(slot.width);
  }
  return stamped;
}

function stampWindowsIcon(executableBytes, icoBytes) {
  const pe = parsePE(executableBytes);
  const images = parseIco(icoBytes);
  const group = parseIconGroup(pe);
  const stamped = [];
  for (const slot of group) {
    const leaf = resourceLeaf(pe, RT_ICON, slot.id);
    const image = chooseImage(images, slot, leaf.size);
    if (!image) throw new Error(`Workass icon has no image that fits the ${slot.width}x${slot.height} executable slot`);
    image.data.copy(executableBytes, leaf.dataOffset);
    executableBytes.fill(0, leaf.dataOffset + image.data.length, leaf.dataOffset + leaf.size);
    executableBytes.writeUInt32LE(image.data.length, leaf.dataEntryOffset + 4);
    executableBytes[slot.offset] = image.width === 256 ? 0 : image.width;
    executableBytes[slot.offset + 1] = image.height === 256 ? 0 : image.height;
    executableBytes[slot.offset + 2] = 0;
    executableBytes[slot.offset + 3] = 0;
    executableBytes.writeUInt16LE(image.planes, slot.offset + 4);
    executableBytes.writeUInt16LE(image.bpp, slot.offset + 6);
    executableBytes.writeUInt32LE(image.data.length, slot.offset + 8);
    stamped.push(image.width);
  }
  inspectStampedIcon(executableBytes, icoBytes);
  return stamped;
}

function parseArgs(argv) {
  const options = { verify: false, exe: '', icon: '' };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === '--verify') options.verify = true;
    else if (arg === '--exe' && argv[index + 1]) options.exe = argv[++index];
    else if (arg === '--icon' && argv[index + 1]) options.icon = argv[++index];
    else throw new Error(`unknown or incomplete argument: ${arg}`);
  }
  if (!path.isAbsolute(options.exe) || !path.isAbsolute(options.icon)) throw new Error('--exe and --icon must be absolute paths');
  return options;
}

function main(argv = process.argv.slice(2)) {
  const options = parseArgs(argv);
  const executable = fs.readFileSync(options.exe);
  const icon = fs.readFileSync(options.icon);
  const sizes = options.verify ? inspectStampedIcon(executable, icon) : stampWindowsIcon(executable, icon);
  if (!options.verify) {
    const stat = fs.statSync(options.exe);
    const incoming = `${options.exe}.incoming-${process.pid}`;
    try {
      fs.writeFileSync(incoming, executable, { mode: stat.mode & 0o777 });
      fs.renameSync(incoming, options.exe);
    } finally {
      try { fs.rmSync(incoming, { force: true }); } catch { /* atomic rename already consumed it */ }
    }
  }
  process.stdout.write(`WORKASS_WINDOWS_ICON_${options.verify ? 'VERIFIED' : 'STAMPED'} sizes=${sizes.join(',')}\n`);
}

const invoked = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (invoked) {
  try { main(); }
  catch (error) {
    process.stderr.write(`workass windows icon failed: ${error?.message || error}\n`);
    process.exitCode = 1;
  }
}

export { inspectStampedIcon, parseIco, stampWindowsIcon };
