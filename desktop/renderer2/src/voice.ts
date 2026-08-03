// Microphone capture for dictation.
//
// Tap to open, tap to close. No hold-to-talk, and nothing is ever sent on the
// user's behalf: the finished text is inserted into the composer for them to
// read, fix and send.
//
// Capture lives here, in the client, rather than in the daemon. The microphone
// belongs to the machine the human is sitting at, and a daemon may be on another
// machine entirely — recording there would open a microphone in an empty room.
//
// There is no live transcript. whisper decodes a whole segment and has no
// partial hypotheses, so "live" text means re-decoding a sliding window, which
// visibly rewrites itself mid-sentence and cannot be edited while it does. We
// record, then transcribe once. The level meter is what tells the user the
// microphone is hearing them; it also catches the most common failure, which is
// recording happily from a muted or wrong input device.

export const SAMPLE_RATE = 16000;

export type VoiceState = 'idle' | 'recording' | 'transcribing';

export type VoiceStatus = {
  available: boolean;
  reason?: 'engine-missing' | 'model-missing' | 'unavailable';
  hint?: string;
  model?: string;
};

type Bridge = {
  voiceStatus?: () => Promise<VoiceStatus>;
  voiceTranscribe?: (audio: string, lang?: string, vocab?: string[]) => Promise<{ text?: string; model?: string; ms?: number }>;
};

function bridge(): Bridge | undefined {
  return typeof window !== 'undefined' ? ((window as unknown as { api?: Bridge }).api) : undefined;
}

/** A daemon too old to know the channel answers with an ordinary channel error;
 *  that is indistinguishable from "not installed" to the user, and both mean the
 *  microphone should not open. */
export async function voiceStatus(): Promise<VoiceStatus> {
  const api = bridge();
  if (!api || typeof api.voiceStatus !== 'function') {
    return { available: false, reason: 'unavailable', hint: 'este daemon no expone voz' };
  }
  try {
    const status = await api.voiceStatus();
    return status && typeof status.available === 'boolean'
      ? status
      : { available: false, reason: 'unavailable' };
  } catch (error) {
    return { available: false, reason: 'unavailable', hint: String((error as Error)?.message ?? error) };
  }
}

/** Downmix to mono and resample to 16 kHz with a plain averaging filter.
 *
 *  Averaging the samples that fall inside each output period — rather than
 *  picking one and dropping the rest — is what keeps the aliasing out. It is
 *  cheap and it is enough: the input is speech from one nearby mouth, and
 *  whisper's own front end is a mel spectrogram that discards far more than
 *  this ever could. */
export function resampleToMono16k(input: Float32Array, inputRate: number): Float32Array {
  if (inputRate === SAMPLE_RATE) return input;
  const ratio = inputRate / SAMPLE_RATE;
  const outLength = Math.floor(input.length / ratio);
  const out = new Float32Array(outLength);
  for (let i = 0; i < outLength; i++) {
    const start = Math.floor(i * ratio);
    const end = Math.min(Math.floor((i + 1) * ratio), input.length);
    let sum = 0;
    let n = 0;
    for (let j = start; j < end; j++) {
      sum += input[j];
      n++;
    }
    out[i] = n > 0 ? sum / n : 0;
  }
  return out;
}

/** Float samples to signed 16-bit little-endian, clamped.
 *
 *  Asymmetric scaling on purpose: negative amplitudes reach -32768 and positive
 *  ones only 32767, so using 0x8000 for both would clip every loud positive
 *  peak by one bit. */
export function floatToPCM16(samples: Float32Array): Uint8Array {
  const out = new Uint8Array(samples.length * 2);
  const view = new DataView(out.buffer);
  for (let i = 0; i < samples.length; i++) {
    const s = Math.max(-1, Math.min(1, samples[i]));
    view.setInt16(i * 2, s < 0 ? s * 0x8000 : s * 0x7fff, true);
  }
  return out;
}

/** Wrap PCM in a RIFF/WAVE header. Mirrors voice.WAV in the daemon; both exist
 *  because each side holds the samples at a different moment. */
export function encodeWAV(pcm: Uint8Array): Uint8Array {
  const out = new Uint8Array(44 + pcm.length);
  const view = new DataView(out.buffer);
  const ascii = (offset: number, text: string) => {
    for (let i = 0; i < text.length; i++) view.setUint8(offset + i, text.charCodeAt(i));
  };
  const blockAlign = 2; // mono, 16-bit

  ascii(0, 'RIFF');
  view.setUint32(4, 36 + pcm.length, true);
  ascii(8, 'WAVE');
  ascii(12, 'fmt ');
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true); // PCM
  view.setUint16(22, 1, true); // channels
  view.setUint32(24, SAMPLE_RATE, true);
  view.setUint32(28, SAMPLE_RATE * blockAlign, true);
  view.setUint16(32, blockAlign, true);
  view.setUint16(34, 16, true);
  ascii(36, 'data');
  view.setUint32(40, pcm.length, true);
  out.set(pcm, 44);
  return out;
}

/** base64 without a data: URL round-trip. Chunked because String.fromCharCode
 *  with a whole minute of audio spread as arguments overflows the call stack. */
export function toBase64(bytes: Uint8Array): string {
  let binary = '';
  const CHUNK = 0x8000;
  for (let i = 0; i < bytes.length; i += CHUNK) {
    binary += String.fromCharCode(...bytes.subarray(i, i + CHUNK));
  }
  return btoa(binary);
}

export type RecorderOptions = {
  /** 0..1, roughly peak amplitude. Drives the level meter. */
  onLevel?: (level: number) => void;
  /** A recording nobody stops must still end. */
  maxSeconds?: number;
  onAutoStop?: () => void;
};

/** An open microphone. Created by startRecording, ended by stop(). */
export type Recorder = {
  stop: () => Promise<Uint8Array>;
  cancel: () => void;
};

/** Opens the microphone and accumulates 16 kHz mono samples until stopped.
 *
 *  Throws if permission is refused or no input device exists — the caller shows
 *  that, because a microphone button that silently does nothing is worse than
 *  one that explains itself. */
export async function startRecording(opts: RecorderOptions = {}): Promise<Recorder> {
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: {
      channelCount: 1,
      echoCancellation: true,
      noiseSuppression: true,
      autoGainControl: true,
    },
  });

  const ctx = new AudioContext();
  const source = ctx.createMediaStreamSource(stream);
  // ScriptProcessor is deprecated in favour of AudioWorklet, which needs a
  // separate module file served alongside the bundle. Dictation is a few
  // seconds of speech on the main thread's terms, not a live audio graph, and
  // this keeps the whole capture path in one file that cannot get out of sync
  // with its worklet. Revisit if a live-conversation mode ever needs it.
  const node = ctx.createScriptProcessor(4096, 1, 1);

  const chunks: Float32Array[] = [];
  let total = 0;
  let stopped = false;
  const maxSamples = Math.floor((opts.maxSeconds ?? 120) * SAMPLE_RATE);

  node.onaudioprocess = (event) => {
    if (stopped) return;
    const input = event.inputBuffer.getChannelData(0);
    const resampled = resampleToMono16k(new Float32Array(input), ctx.sampleRate);
    chunks.push(resampled);
    total += resampled.length;

    if (opts.onLevel) {
      let peak = 0;
      for (let i = 0; i < resampled.length; i++) {
        const v = Math.abs(resampled[i]);
        if (v > peak) peak = v;
      }
      opts.onLevel(peak);
    }
    if (total >= maxSamples) {
      stopped = true;
      opts.onAutoStop?.();
    }
  };

  source.connect(node);
  // A ScriptProcessor only fires while it is connected to a destination. Routing
  // it through a silent gain node keeps the callbacks coming without playing the
  // microphone back through the speakers.
  const mute = ctx.createGain();
  mute.gain.value = 0;
  node.connect(mute);
  mute.connect(ctx.destination);

  const teardown = () => {
    stopped = true;
    node.onaudioprocess = null;
    try { node.disconnect(); } catch { /* already torn down */ }
    try { source.disconnect(); } catch { /* already torn down */ }
    try { mute.disconnect(); } catch { /* already torn down */ }
    // Releases the OS recording indicator. Leaving it on after a dictation is
    // both a privacy problem and a support call.
    stream.getTracks().forEach((track) => track.stop());
    void ctx.close().catch(() => { /* closing a closed context is fine */ });
  };

  return {
    async stop() {
      teardown();
      const merged = new Float32Array(total);
      let offset = 0;
      for (const chunk of chunks) {
        merged.set(chunk, offset);
        offset += chunk.length;
      }
      return encodeWAV(floatToPCM16(merged));
    },
    cancel() {
      teardown();
      chunks.length = 0;
    },
  };
}

/** Sends one finished utterance for transcription and returns the text. */
export async function transcribe(wav: Uint8Array, lang?: string, vocab?: string[]): Promise<string> {
  const api = bridge();
  if (!api || typeof api.voiceTranscribe !== 'function') throw new Error('este daemon no expone voz');
  const res = await api.voiceTranscribe(toBase64(wav), lang, vocab);
  return typeof res?.text === 'string' ? res.text : '';
}

/** Inserts dictated text at the caret without disturbing the rest of the draft.
 *
 *  Spacing is decided from what is already there: dictation appended to a draft
 *  that ends mid-word needs a space, and one that already ends in whitespace or
 *  an opening bracket does not. Returns the new text and where the caret lands,
 *  so the caller can restore the selection. */
export function insertAtCaret(
  text: string,
  insert: string,
  selectionStart: number,
  selectionEnd: number,
): { text: string; caret: number } {
  if (!insert) return { text, caret: selectionEnd };
  const start = Math.max(0, Math.min(selectionStart, text.length));
  const end = Math.max(start, Math.min(selectionEnd, text.length));
  const before = text.slice(0, start);
  const after = text.slice(end);

  const needsLeading = before.length > 0 && !/[\s([{"'¿¡]$/.test(before);
  const needsTrailing = after.length > 0 && !/^[\s)\]}.,;:!?]/.test(after);
  const body = (needsLeading ? ' ' : '') + insert + (needsTrailing ? ' ' : '');

  return { text: before + body + after, caret: before.length + body.length };
}
