import test from 'node:test';
import assert from 'node:assert/strict';

import {
  SAMPLE_RATE,
  encodeWAV,
  floatToPCM16,
  insertAtCaret,
  resampleToMono16k,
  toBase64,
} from '../src/voice.ts';

test('resampling 48k to 16k averages each window instead of dropping samples', () => {
  // A constant signal must survive resampling untouched; picking one sample per
  // window would too, but averaging is what keeps aliasing out of real speech.
  const input = new Float32Array(480).fill(0.5);
  const out = resampleToMono16k(input, 48000);
  assert.equal(out.length, 160);
  for (const v of out) assert.ok(Math.abs(v - 0.5) < 1e-6);
});

test('resampling is a no-op when the context already runs at 16k', () => {
  const input = new Float32Array([0.1, -0.2, 0.3]);
  assert.equal(resampleToMono16k(input, SAMPLE_RATE), input);
});

test('a 48k ramp averages rather than decimates', () => {
  // Three input samples per output sample: the mean of 0,1,2 is 1 — decimation
  // would return 0 (first) or 2 (last).
  const input = new Float32Array([0, 1, 2, 3, 4, 5]);
  const out = resampleToMono16k(input, SAMPLE_RATE * 3);
  assert.equal(out.length, 2);
  assert.ok(Math.abs(out[0] - 1) < 1e-6);
  assert.ok(Math.abs(out[1] - 4) < 1e-6);
});

test('float to pcm16 clamps without wrapping at either rail', () => {
  const pcm = floatToPCM16(new Float32Array([0, 1, -1, 2, -2]));
  const view = new DataView(pcm.buffer);
  assert.equal(view.getInt16(0, true), 0);
  assert.equal(view.getInt16(2, true), 32767);
  assert.equal(view.getInt16(4, true), -32768);
  // Out-of-range input clamps to the same rails; wrapping would turn a loud
  // peak into a full-amplitude click of the opposite sign.
  assert.equal(view.getInt16(6, true), 32767);
  assert.equal(view.getInt16(8, true), -32768);
});

test('the WAV header declares 16k mono 16-bit and the right data length', () => {
  const pcm = new Uint8Array(320);
  const wav = encodeWAV(pcm);
  const view = new DataView(wav.buffer);
  const text = (offset: number, length: number) =>
    String.fromCharCode(...wav.subarray(offset, offset + length));

  assert.equal(wav.length, 44 + pcm.length);
  assert.equal(text(0, 4), 'RIFF');
  assert.equal(text(8, 8), 'WAVEfmt ');
  assert.equal(text(36, 4), 'data');
  assert.equal(view.getUint16(20, true), 1, 'PCM format tag');
  assert.equal(view.getUint16(22, true), 1, 'mono');
  assert.equal(view.getUint32(24, true), SAMPLE_RATE);
  assert.equal(view.getUint32(28, true), SAMPLE_RATE * 2, 'byte rate');
  assert.equal(view.getUint16(32, true), 2, 'block align');
  assert.equal(view.getUint16(34, true), 16, 'bits per sample');
  assert.equal(view.getUint32(40, true), pcm.length);
  assert.equal(view.getUint32(4, true), 36 + pcm.length);
});

test('base64 survives audio longer than one chunk', () => {
  // 0x8000 is the chunk size; spreading a whole minute of audio as call
  // arguments in one go overflows the stack, so this must not regress.
  const bytes = new Uint8Array(0x8000 * 2 + 5);
  for (let i = 0; i < bytes.length; i++) bytes[i] = i % 256;
  const encoded = toBase64(bytes);
  const decoded = Buffer.from(encoded, 'base64');
  assert.equal(decoded.length, bytes.length);
  assert.ok(decoded.equals(Buffer.from(bytes)));
});

test('dictation lands at the caret and leaves the rest of the draft alone', () => {
  // Caret right after "Revisá", before the space that follows it.
  const { text, caret } = insertAtCaret('Revisá el reducer.', 'y el fleet key', 6, 6);
  assert.equal(text, 'Revisá y el fleet key el reducer.');
  assert.equal(text.slice(caret), ' el reducer.');
});

test('spacing follows what is already around the caret', () => {
  // appended to a word: needs a leading space
  assert.equal(insertAtCaret('hola', 'mundo', 4, 4).text, 'hola mundo');
  // already spaced: must not double it
  assert.equal(insertAtCaret('hola ', 'mundo', 5, 5).text, 'hola mundo');
  // empty draft: no stray leading space
  assert.equal(insertAtCaret('', 'mundo', 0, 0).text, 'mundo');
  // before punctuation: no space wedged in front of it
  assert.equal(insertAtCaret('.', 'hola', 0, 0).text, 'hola.');
});

test('dictation replaces a selection rather than stacking onto it', () => {
  const { text } = insertAtCaret('borrar esto ahora', 'guardar', 0, 12);
  assert.equal(text, 'guardar ahora');
});

test('an empty transcription leaves the draft byte-identical', () => {
  // whisper returns nothing for a silent recording; an accidental tap on the
  // microphone must not perturb the draft or move the caret.
  const { text, caret } = insertAtCaret('sin cambios', '', 3, 3);
  assert.equal(text, 'sin cambios');
  assert.equal(caret, 3);
});
