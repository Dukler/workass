/* Generates the app icon (icon.ico + icon.png) to match the in-app brand mark:
   a rounded-square tile with a conic gradient teal -> gold -> ember -> teal and
   a soft teal glow. No dependencies: PNG is encoded via node:zlib, then PNGs are
   packed into a multi-size .ico (Windows Vista+ supports PNG-in-ICO). */

import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const assetsDir = path.resolve(__dirname, '..', 'assets');
fs.mkdirSync(assetsDir, { recursive: true });

const TEAL = [47, 224, 192];
const GOLD = [243, 200, 75];
const EMBER = [255, 107, 61];
const STOPS = [[0, TEAL], [1 / 3, GOLD], [2 / 3, EMBER], [1, TEAL]];

const lerp = (a, b, t) => a + (b - a) * t;
const clamp01 = (v) => (v < 0 ? 0 : v > 1 ? 1 : v);

function gradientAt(frac) {
  frac = ((frac % 1) + 1) % 1;
  for (let i = 0; i < STOPS.length - 1; i++) {
    const [p0, c0] = STOPS[i];
    const [p1, c1] = STOPS[i + 1];
    if (frac >= p0 && frac <= p1) {
      const t = (frac - p0) / (p1 - p0);
      return [lerp(c0[0], c1[0], t), lerp(c0[1], c1[1], t), lerp(c0[2], c1[2], t)];
    }
  }
  return TEAL;
}

function renderRGBA(N) {
  const buf = Buffer.alloc(N * N * 4);
  const pad = Math.round(N * 0.09);
  const cx = N / 2;
  const cy = N / 2;
  const half = (N - 2 * pad) / 2;
  const r = 2 * half * 0.28;
  const glowReach = N * 0.06;

  for (let y = 0; y < N; y++) {
    for (let x = 0; x < N; x++) {
      const px = x + 0.5;
      const py = y + 0.5;
      const dx = px - cx;
      const dy = py - cy;

      // rounded-rect signed distance
      const qx = Math.abs(dx) - (half - r);
      const qy = Math.abs(dy) - (half - r);
      const ox = Math.max(qx, 0);
      const oy = Math.max(qy, 0);
      const dist = Math.sqrt(ox * ox + oy * oy) + Math.min(Math.max(qx, qy), 0) - r;

      const tileA = clamp01(0.5 - dist); // ~1px antialias

      // conic gradient (0deg at top, clockwise), rotated -120deg like the CSS mark
      let deg = (Math.atan2(dx, -dy) * 180) / Math.PI;
      deg = ((deg - 120) % 360 + 360) % 360;
      let [cr, cg, cb] = gradientAt(deg / 360);

      // subtle radial sheen for depth
      const rad = Math.sqrt(dx * dx + dy * dy) / half;
      const sheen = 1 + (0.12 * (1 - clamp01(rad)));
      cr = Math.min(255, cr * sheen);
      cg = Math.min(255, cg * sheen);
      cb = Math.min(255, cb * sheen);

      // soft teal glow outside the tile
      let glowA = 0;
      if (dist > 0) { const g = clamp01(1 - dist / glowReach); glowA = g * g * 0.4; }

      // composite glow under tile
      const outA = tileA + glowA * (1 - tileA);
      const i = (y * N + x) * 4;
      if (outA <= 0) { buf[i] = buf[i + 1] = buf[i + 2] = buf[i + 3] = 0; continue; }
      const gr = TEAL[0], gg = TEAL[1], gb = TEAL[2];
      buf[i] = Math.round((cr * tileA + gr * glowA * (1 - tileA)) / outA);
      buf[i + 1] = Math.round((cg * tileA + gg * glowA * (1 - tileA)) / outA);
      buf[i + 2] = Math.round((cb * tileA + gb * glowA * (1 - tileA)) / outA);
      buf[i + 3] = Math.round(outA * 255);
    }
  }
  return buf;
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
    raw[y * (N * 4 + 1)] = 0; // filter: none
    rgba.copy(raw, y * (N * 4 + 1) + 1, y * N * 4, (y + 1) * N * 4);
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
const images = sizes.map((size) => {
  const rgba = renderRGBA(size);
  // BMP DIB for small sizes (taskbar-compatible), PNG for large (size-efficient).
  const data = size <= 64 ? encodeBmpDib(size, rgba) : encodePNG(size, rgba);
  return { size, data, png: encodePNG(size, rgba) };
});

fs.writeFileSync(path.join(assetsDir, 'icon.png'), images[images.length - 1].png);
fs.writeFileSync(path.join(assetsDir, 'icon.ico'), buildIco(images));
console.log(`icon.png (256) + icon.ico (${sizes.join(',')}) written to ${assetsDir}`);
