/* Generates the Windows app icon from Workass's canonical macOS artwork.
   No third-party image library is needed: the source RGBA PNG is decoded with
   node:zlib, area-resampled with premultiplied alpha, then packed into a
   multi-size ICO. Small entries use BMP DIBs for Windows shell compatibility. */

import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const assetsDir = path.resolve(__dirname, '..', 'assets');

function requiredRange(bytes, offset, size, label) {
  if (!Number.isSafeInteger(offset) || !Number.isSafeInteger(size) || offset < 0 || size < 0 || offset + size > bytes.length) {
    throw new Error(`${label} is outside the file`);
  }
}

function paeth(left, above, upperLeft) {
  const estimate = left + above - upperLeft;
  const leftDistance = Math.abs(estimate - left);
  const aboveDistance = Math.abs(estimate - above);
  const upperLeftDistance = Math.abs(estimate - upperLeft);
  if (leftDistance <= aboveDistance && leftDistance <= upperLeftDistance) return left;
  return aboveDistance <= upperLeftDistance ? above : upperLeft;
}

function decodeRGBA8PNG(bytes) {
  const signature = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]);
  requiredRange(bytes, 0, signature.length, 'PNG signature');
  if (!bytes.subarray(0, signature.length).equals(signature)) throw new Error('icon source is not a PNG');
  let cursor = signature.length;
  let header = null;
  const compressed = [];
  let ended = false;
  while (cursor < bytes.length) {
    requiredRange(bytes, cursor, 12, 'PNG chunk');
    const length = bytes.readUInt32BE(cursor);
    const type = bytes.toString('ascii', cursor + 4, cursor + 8);
    requiredRange(bytes, cursor + 8, length + 4, `PNG ${type} chunk`);
    const data = bytes.subarray(cursor + 8, cursor + 8 + length);
    const expectedCRC = bytes.readUInt32BE(cursor + 8 + length);
    const actualCRC = crc32(Buffer.concat([Buffer.from(type, 'ascii'), data]));
    if (expectedCRC !== actualCRC) throw new Error(`PNG ${type} checksum does not match`);
    if (type === 'IHDR') {
      if (header || length !== 13) throw new Error('icon source has an invalid PNG header');
      header = Buffer.from(data);
    } else if (type === 'IDAT') compressed.push(Buffer.from(data));
    else if (type === 'IEND') { ended = true; break; }
    cursor += length + 12;
  }
  if (!header || !compressed.length || !ended) throw new Error('icon source PNG is incomplete');
  const width = header.readUInt32BE(0);
  const height = header.readUInt32BE(4);
  if (!width || !height || width > 4096 || height > 4096 || width !== height) throw new Error('icon source PNG must be a bounded square');
  if (header[8] !== 8 || header[9] !== 6 || header[10] !== 0 || header[11] !== 0 || header[12] !== 0) {
    throw new Error('icon source PNG must be non-interlaced 8-bit RGBA');
  }
  const stride = width * 4;
  const filtered = zlib.inflateSync(Buffer.concat(compressed));
  if (filtered.length !== height * (stride + 1)) throw new Error('icon source PNG scanlines have an invalid size');
  const rgba = Buffer.alloc(width * height * 4);
  for (let y = 0; y < height; y += 1) {
    const filter = filtered[y * (stride + 1)];
    if (filter > 4) throw new Error('icon source PNG uses an unsupported row filter');
    const input = y * (stride + 1) + 1;
    const output = y * stride;
    for (let x = 0; x < stride; x += 1) {
      const encoded = filtered[input + x];
      const left = x >= 4 ? rgba[output + x - 4] : 0;
      const above = y > 0 ? rgba[output + x - stride] : 0;
      const upperLeft = y > 0 && x >= 4 ? rgba[output + x - stride - 4] : 0;
      let predictor = 0;
      if (filter === 1) predictor = left;
      else if (filter === 2) predictor = above;
      else if (filter === 3) predictor = Math.floor((left + above) / 2);
      else if (filter === 4) predictor = paeth(left, above, upperLeft);
      rgba[output + x] = (encoded + predictor) & 0xff;
    }
  }
  return { width, height, rgba };
}

function areaResizeRGBA(source, size) {
  const { width, height, rgba } = source;
  if (!Number.isInteger(size) || size < 1 || size > width || size > height) throw new Error('icon output size is invalid');
  const output = Buffer.alloc(size * size * 4);
  for (let y = 0; y < size; y += 1) {
    const top = y * height / size;
    const bottom = (y + 1) * height / size;
    for (let x = 0; x < size; x += 1) {
      const left = x * width / size;
      const right = (x + 1) * width / size;
      let weightedAlpha = 0;
      let weightedRed = 0;
      let weightedGreen = 0;
      let weightedBlue = 0;
      for (let sourceY = Math.floor(top); sourceY < Math.ceil(bottom); sourceY += 1) {
        const vertical = Math.min(bottom, sourceY + 1) - Math.max(top, sourceY);
        for (let sourceX = Math.floor(left); sourceX < Math.ceil(right); sourceX += 1) {
          const horizontal = Math.min(right, sourceX + 1) - Math.max(left, sourceX);
          const weight = vertical * horizontal;
          const sourceOffset = (sourceY * width + sourceX) * 4;
          const alpha = rgba[sourceOffset + 3] / 255;
          weightedAlpha += alpha * weight;
          weightedRed += rgba[sourceOffset] * alpha * weight;
          weightedGreen += rgba[sourceOffset + 1] * alpha * weight;
          weightedBlue += rgba[sourceOffset + 2] * alpha * weight;
        }
      }
      const area = (right - left) * (bottom - top);
      const outputOffset = (y * size + x) * 4;
      if (weightedAlpha > 0) {
        output[outputOffset] = Math.round(weightedRed / weightedAlpha);
        output[outputOffset + 1] = Math.round(weightedGreen / weightedAlpha);
        output[outputOffset + 2] = Math.round(weightedBlue / weightedAlpha);
        output[outputOffset + 3] = Math.round(255 * weightedAlpha / area);
      }
    }
  }
  return output;
}

/* ---------- PNG encoder ---------- */
const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();
function crc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}
function chunk(type, data) {
  const len = Buffer.alloc(4); len.writeUInt32BE(data.length, 0);
  const typeBuf = Buffer.from(type, 'ascii');
  const body = Buffer.concat([typeBuf, data]);
  const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(body), 0);
  return Buffer.concat([len, body, crc]);
}
function encodePNG(N, rgba) {
  const sig = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]);
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(N, 0); ihdr.writeUInt32BE(N, 4);
  ihdr[8] = 8; ihdr[9] = 6; ihdr[10] = 0; ihdr[11] = 0; ihdr[12] = 0;
  const raw = Buffer.alloc(N * (N * 4 + 1));
  for (let y = 0; y < N; y++) {
    const row = y * (N * 4 + 1);
    raw[row] = 1; // Sub: preserves the exact pixels and compresses gradients.
    for (let x = 0; x < N * 4; x++) {
      const current = rgba[y * N * 4 + x];
      const left = x >= 4 ? rgba[y * N * 4 + x - 4] : 0;
      raw[row + 1 + x] = (current - left + 256) & 0xff;
    }
  }
  const idat = zlib.deflateSync(raw, { level: 9 });
  return Buffer.concat([sig, chunk('IHDR', ihdr), chunk('IDAT', idat), chunk('IEND', Buffer.alloc(0))]);
}

/* ---------- BMP (DIB) encoder for ICO small sizes ----------
   Windows taskbar/shell is finicky about PNG-compressed entries at small
   sizes and will silently fall back to the host exe icon (the Electron logo).
   Encoding <=64px as classic uncompressed 32-bit BMP DIBs (with AND mask)
   is the most compatible and makes the taskbar pick up our icon. */
function encodeBmpDib(N, rgba) {
  const header = Buffer.alloc(40);
  header.writeUInt32LE(40, 0);          // biSize
  header.writeInt32LE(N, 4);            // biWidth
  header.writeInt32LE(N * 2, 8);        // biHeight (image + AND mask)
  header.writeUInt16LE(1, 12);          // biPlanes
  header.writeUInt16LE(32, 14);         // biBitCount
  header.writeUInt32LE(0, 16);          // biCompression = BI_RGB
  const xor = Buffer.alloc(N * N * 4);  // BGRA, bottom-up
  for (let y = 0; y < N; y++) {
    const srcY = N - 1 - y;
    for (let x = 0; x < N; x++) {
      const s = (srcY * N + x) * 4;
      const d = (y * N + x) * 4;
      xor[d] = rgba[s + 2];     // B
      xor[d + 1] = rgba[s + 1]; // G
      xor[d + 2] = rgba[s];     // R
      xor[d + 3] = rgba[s + 3]; // A
    }
  }
  const maskRow = Math.ceil(N / 32) * 4; // 1bpp, dword-aligned
  const andMask = Buffer.alloc(maskRow * N);
  for (let y = 0; y < N; y++) {
    const srcY = N - 1 - y;
    for (let x = 0; x < N; x++) {
      const a = rgba[(srcY * N + x) * 4 + 3];
      if (a < 128) andMask[y * maskRow + (x >> 3)] |= (0x80 >> (x & 7));
    }
  }
  return Buffer.concat([header, xor, andMask]);
}

/* ---------- ICO packer (BMP for small, PNG for large) ---------- */
function buildIco(images) {
  const count = images.length;
  const header = Buffer.alloc(6);
  header.writeUInt16LE(0, 0); header.writeUInt16LE(1, 2); header.writeUInt16LE(count, 4);
  const dir = Buffer.alloc(16 * count);
  let offset = 6 + 16 * count;
  const datas = [];
  images.forEach((img, idx) => {
    const e = idx * 16;
    dir[e] = img.size >= 256 ? 0 : img.size;
    dir[e + 1] = img.size >= 256 ? 0 : img.size;
    dir[e + 2] = 0; dir[e + 3] = 0;
    dir.writeUInt16LE(1, e + 4);   // planes
    dir.writeUInt16LE(32, e + 6);  // bpp
    dir.writeUInt32LE(img.data.length, e + 8);
    dir.writeUInt32LE(offset, e + 12);
    offset += img.data.length;
    datas.push(img.data);
  });
  return Buffer.concat([header, dir, ...datas]);
}

/* ---------- build ---------- */
const sizes = [16, 24, 32, 48, 64, 128, 256];

function generatedIcons(sourceFile) {
  const source = decodeRGBA8PNG(fs.readFileSync(sourceFile));
  const images = sizes.map((size) => {
    const rgba = areaResizeRGBA(source, size);
    // BMP DIB for small sizes (taskbar-compatible), PNG for large (size-efficient).
    const png = encodePNG(size, rgba);
    return { size, data: size <= 64 ? encodeBmpDib(size, rgba) : png, png, rgba };
  });
  return {
    png: images[images.length - 1].png,
    ico: buildIco(images),
    images,
  };
}

function atomicWrite(file, bytes) {
  const incoming = `${file}.incoming-${process.pid}`;
  try {
    fs.writeFileSync(incoming, bytes);
    fs.renameSync(incoming, file);
  } finally {
    try { fs.rmSync(incoming, { force: true }); } catch { /* rename already consumed it */ }
  }
}

function parseArgs(argv) {
  const options = {
    source: path.join(assetsDir, 'workass-macos.png'),
    outputDir: assetsDir,
    verify: false,
  };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === '--verify') options.verify = true;
    else if (argument === '--source' && argv[index + 1]) options.source = path.resolve(argv[++index]);
    else if (argument === '--output-dir' && argv[index + 1]) options.outputDir = path.resolve(argv[++index]);
    else throw new Error(`unknown or incomplete argument: ${argument}`);
  }
  return options;
}

/* Verification compares decoded artwork pixels, never compressed bytes:
   deflate output differs between Node/zlib versions, so a byte comparison
   would reject canonical artwork regenerated by any other interpreter. */
function readIcoImages(bytes) {
  if (bytes.length < 6 || bytes.readUInt16LE(0) !== 0 || bytes.readUInt16LE(2) !== 1) {
    throw new Error('tracked Windows icons do not match canonical Workass artwork');
  }
  const count = bytes.readUInt16LE(4);
  const images = [];
  for (let index = 0; index < count; index += 1) {
    const entry = index * 16 + 6;
    const width = bytes[entry] || 256;
    const size = bytes.readUInt32LE(entry + 8);
    const offset = bytes.readUInt32LE(entry + 12);
    requiredRange(bytes, offset, size, `the ICO ${width}px image`);
    images.push({ width, data: bytes.subarray(offset, offset + size) });
  }
  return images;
}

function decodeIcoEntryRGBA(image) {
  const pngSignature = 137;
  if (image.data[0] === pngSignature && image.data[1] === 80) return decodeRGBA8PNG(image.data).rgba;
  const width = image.data.readInt32LE(4);
  if (width !== image.width || Math.abs(image.data.readInt32LE(8)) !== width * 2) {
    throw new Error('tracked Windows icons do not match canonical Workass artwork');
  }
  if (image.data.readUInt16LE(14) !== 32) {
    throw new Error('tracked Windows icons do not match canonical Workass artwork');
  }
  const pixels = Buffer.alloc(width * width * 4);
  for (let y = 0; y < width; y += 1) {
    for (let x = 0; x < width; x += 1) {
      const src = 40 + (y * width + x) * 4;
      const dst = ((width - 1 - y) * width + x) * 4;
      pixels[dst] = image.data[src + 2];
      pixels[dst + 1] = image.data[src + 1];
      pixels[dst + 2] = image.data[src];
      pixels[dst + 3] = image.data[src + 3];
    }
  }
  return pixels;
}

function verifyArtwork(options, generated) {
  const mismatch = () => new Error('tracked Windows icons do not match canonical Workass artwork');
  const trackedPng = decodeRGBA8PNG(fs.readFileSync(path.join(options.outputDir, 'icon.png'))).rgba;
  const expectedPng = generated.images[generated.images.length - 1].rgba;
  if (!trackedPng.equals(expectedPng)) throw mismatch();
  const tracked = readIcoImages(fs.readFileSync(path.join(options.outputDir, 'icon.ico')));
  if (tracked.length !== generated.images.length) throw mismatch();
  for (const expectedImage of generated.images) {
    const entry = tracked.find((candidate) => candidate.width === expectedImage.size);
    if (!entry || !decodeIcoEntryRGBA(entry).equals(expectedImage.rgba)) throw mismatch();
  }
}

function main(argv = process.argv.slice(2)) {
  const options = parseArgs(argv);
  const generated = generatedIcons(options.source);
  const pngFile = path.join(options.outputDir, 'icon.png');
  const icoFile = path.join(options.outputDir, 'icon.ico');
  if (options.verify) {
    verifyArtwork(options, generated);
    process.stdout.write(`WORKASS_WINDOWS_ICON_ARTWORK_VERIFIED sizes=${sizes.join(',')}\n`);
    return;
  }
  fs.mkdirSync(options.outputDir, { recursive: true });
  atomicWrite(pngFile, generated.png);
  atomicWrite(icoFile, generated.ico);
  process.stdout.write(`icon.png (256) + icon.ico (${sizes.join(',')}) written to ${options.outputDir}\n`);
}

const invoked = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (invoked) {
  try { main(); }
  catch (error) {
    process.stderr.write(`workass icon generation failed: ${error?.message || error}\n`);
    process.exitCode = 1;
  }
}

export { areaResizeRGBA, buildIco, decodeRGBA8PNG, encodePNG, generatedIcons };
