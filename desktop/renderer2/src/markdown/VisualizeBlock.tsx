import { useEffect, useState } from 'react';
import { callThrow } from '../wire/api';
import type { VisualizationRegistration } from '../wire/types';
import type { VisualizeSpec } from '../visualize';

type HostState =
  | { phase: 'loading' }
  | { phase: 'ready'; registration: VisualizationRegistration }
  | { phase: 'error'; message: string; retryable: boolean };

const inflight = new Map<string, Promise<VisualizationRegistration>>();

function visualizationErrorMessage(message: string): string {
  const compact = String(message || '').replace(/\s+/g, ' ').trim();
  if (!compact) return 'El daemon no informó la causa.';
  if (compact.includes('must stay inside Workass visualizations storage')) {
    return 'El archivo quedó fuera de la carpeta de visualizaciones permitida para este proyecto.';
  }
  if (compact.includes('visualization path is not readable') || compact.includes('visualization path is not a regular file')) {
    return 'El archivo de la visualización ya no está disponible.';
  }
  if (compact.includes('visualization chat context is unavailable')) {
    return 'Esta visualización perdió el contexto de su conversación.';
  }
  return compact.length > 240 ? `${compact.slice(0, 239)}…` : compact;
}

function safeArtifactPath(path: string): boolean {
  return path.startsWith('/workass/artifacts/') && !path.includes('://') && !path.includes('\\');
}

function hostVisualization(tabId: string, chatId: string, spec: VisualizeSpec): Promise<VisualizationRegistration> {
  const key = JSON.stringify([tabId, chatId, spec.path, spec.mode ?? '', spec.title ?? '']);
  const existing = inflight.get(key);
  if (existing) return existing;
  const request = callThrow('visualizeHost', {
    tabId,
    chatId,
    path: spec.path,
    ...(spec.mode ? { mode: spec.mode } : {}),
    ...(spec.title ? { title: spec.title } : {}),
  }).then((registration) => {
    if (!registration || !safeArtifactPath(registration.urlPath)) {
      throw new Error('the daemon returned an invalid visualization URL');
    }
    return registration;
  });
  inflight.set(key, request);
  request.catch(() => { if (inflight.get(key) === request) inflight.delete(key); });
  return request;
}

export function VisualizeBlock({
  spec,
  error,
  tabId,
  chatId,
}: {
  spec?: VisualizeSpec;
  error?: string;
  tabId?: string;
  chatId?: string;
}) {
  const [state, setState] = useState<HostState>({ phase: error || !spec ? 'error' : 'loading', message: error ?? 'missing visualization metadata', retryable: false });
  const [attempt, setAttempt] = useState(0);
  const title = spec?.title || 'Visualization';

  useEffect(() => {
    let live = true;
    if (!spec || !tabId || !chatId || error) {
      setState({ phase: 'error', message: error ?? 'visualization chat context is unavailable', retryable: false });
      return () => { live = false; };
    }
    setState({ phase: 'loading' });
    hostVisualization(tabId, chatId, spec).then((registration) => {
      if (live) setState({ phase: 'ready', registration });
    }).catch((cause: unknown) => {
      if (live) setState({ phase: 'error', message: cause instanceof Error ? cause.message : 'visualization hosting failed', retryable: true });
    });
    return () => { live = false; };
  }, [tabId, chatId, error, spec, attempt]);

  const wide = spec?.mode === 'wide';
  return (
    <section className={`visualize-card${wide ? ' visualize-wide' : ''}`} aria-label={title}>
      <div className="visualize-head">
        <span className="visualize-title">{title}</span>
        {state.phase === 'ready' && <a className="visualize-open" href={state.registration.urlPath} target="_blank" rel="noreferrer">Abrir en navegador</a>}
      </div>
      {state.phase === 'loading' && <div className="visualize-status" role="status">Cargando visualización…</div>}
      {state.phase === 'error' && (
        <div className="visualize-status visualize-failure" role="alert">
          <span className="visualize-failure-mark" aria-hidden="true">!</span>
          <span className="visualize-failure-copy">
            <strong>No se pudo cargar</strong>
            <span>{visualizationErrorMessage(state.message)}</span>
          </span>
          {state.retryable && <button type="button" className="visualize-retry" onClick={() => setAttempt((value) => value + 1)}>Reintentar</button>}
        </div>
      )}
      {state.phase === 'ready' && (
        <iframe
          className="visualize-frame"
          title={title}
          src={state.registration.urlPath}
          sandbox="allow-scripts"
          referrerPolicy="no-referrer"
          loading="lazy"
        />
      )}
    </section>
  );
}
