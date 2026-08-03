import { useConnStatus } from '../store/store';

// Slim, quiet status strip pinned directly above the composer while the daemon
// socket is down. Not an alarm: a muted surface with a hairline and a subtle
// amber-tinted spinner — never red, never green. It reserves its own row in the
// composer stack so appearing/vanishing slides the composer without overlap.
export function ConnectionBanner() {
  const connection = useConnStatus();
  const offline = connection !== 'connected';
  if (!offline) return null;
  return (
    <div className="connwrap">
      <div className="connbar" role="status" aria-live="polite">
        <span className="connspin" aria-hidden="true" />
        <span className="conntext">Sin conexión con el daemon · reintentando…</span>
      </div>
    </div>
  );
}
