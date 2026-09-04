import { useEffect, useRef, useState } from 'react';
import { IcStampCopy } from '../icons';
import type { ImageClipboardBridge } from '../image-copy';
import { codeLanguageLabel, renderCode } from './inline';

type ClipboardWriter = { writeText: (text: string) => Promise<void> };

export async function copyCodeText(
  raw: string,
  clipboard?: ClipboardWriter | null,
  shellBridge?: Pick<ImageClipboardBridge, 'supported' | 'copyText'> | null,
): Promise<boolean> {
  const nativeBridge = shellBridge === undefined
    ? (typeof window === 'undefined' ? null : window.workassClipboard)
    : shellBridge;
  if (nativeBridge?.supported && typeof nativeBridge.copyText === 'function') {
    try {
      if (await nativeBridge.copyText(raw)) return true;
    } catch {
      // Plain-browser clipboard remains the fallback when the shell rejects a
      // write or an older preload disappears during an Electron reload.
    }
  }
  const writer = clipboard === undefined
    ? (typeof navigator === 'undefined' ? null : navigator.clipboard)
    : clipboard;
  if (!writer || typeof writer.writeText !== 'function') return false;
  try {
    await writer.writeText(raw);
    return true;
  } catch {
    return false;
  }
}

type CopyState = 'idle' | 'copied' | 'failed';

export function CodeBlock({ raw, language }: { raw: string; language: string }) {
  const [copyState, setCopyState] = useState<CopyState>('idle');
  const resetTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const label = codeLanguageLabel(language);

  useEffect(() => {
    setCopyState('idle');
  }, [raw]);

  useEffect(() => () => {
    if (resetTimer.current !== null) clearTimeout(resetTimer.current);
  }, []);

  const copy = async () => {
    const copied = await copyCodeText(raw);
    setCopyState(copied ? 'copied' : 'failed');
    if (resetTimer.current !== null) clearTimeout(resetTimer.current);
    resetTimer.current = setTimeout(() => setCopyState('idle'), 1800);
  };

  const copyLabel = copyState === 'copied' ? 'Copiado' : copyState === 'failed' ? 'Reintentar' : 'Copiar';
  return (
    <div className="codebox" data-language={label}>
      <div className="code-head">
        <span className="code-lang">{label}</span>
        <button
          type="button"
          className="code-copy"
          data-state={copyState}
          title={copyState === 'failed' ? 'No se pudo copiar el código' : `${copyLabel} código`}
          aria-label={copyState === 'failed' ? 'No se pudo copiar el código; reintentar' : `${copyLabel} código`}
          onClick={() => { void copy(); }}
        >
          <IcStampCopy />
          <span aria-live="polite">{copyLabel}</span>
        </button>
      </div>
      <pre className="code-body"><code>{renderCode(raw, language)}</code></pre>
    </div>
  );
}
