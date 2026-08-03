// R7 in-app toasts — the fallback surface for notifications when desktop
// Notifications are denied/unsupported/disabled, and the delivery path for the
// notify:backlog catch-up burst.

import { store, useApp } from '../store/store';

export function Toasts() {
  const app = useApp();
  if (!app.toasts.length) return null;
  return (
    <div className="toasts">
      {app.toasts.map((t) => (
        <div key={t.id} className="toast" role="status">
          <div className="tbody">
            <div className="ttl">{t.title}</div>
            {t.body && <div className="tsub">{t.body}</div>}
          </div>
          <button className="tx" title="Descartar" onClick={() => store.dismissToast(t.id)}>✕</button>
        </div>
      ))}
    </div>
  );
}
