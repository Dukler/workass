// Ajustes · Mantenimiento (Settings rework, mock .dev/mocks/settings-v2.html
// page 10): the engine fleet and its lifecycle defaults.

import { useApp, useProc } from '../../store/store';
import { fleetLabel } from './fleet';

// Read-only daemon defaults read as plain values, never as a disabled field.
function EngineDefault({ n, d, value, suf }: { n: string; d: string; value: string; suf: string }) {
  return (
    <div className="set">
      <div className="lbl"><div className="n">{n}</div><div className="d">{d}</div></div>
      <div className="ctl"><span className="stat mono">{value} {suf}</span></div>
    </div>
  );
}
export function MantenimientoPage() {
  const app = useApp();
  useProc();
  const fleet = fleetLabel(app.processes);
  return (
    <section className="stgs-panel">

      <div className="gtitle">Flota</div>
      <div className="group">
        <div className="set">
          <div className="lbl"><div className="n">Estado actual</div><div className="d">Engines vivos y memoria residente agregada.</div></div>
          <div className="ctl">
            {app.hasProcChannels
              ? <span className="stat"><span className={`dot ${fleet.count ? 'run' : ''}`} />{fleet.count} {fleet.count === 1 ? 'engine' : 'engines'} · <span className="mono" style={{ color: 'var(--ink2)' }}>{fleet.rss}</span></span>
              : <span className="stat"><span className="dot" />sin datos de proceso</span>}
          </div>
        </div>
      </div>

      <div className="gtitle" title="Flags del daemon: --hibernate-ttl, --engine-max-rss-kb, --rss-sample-interval. Ningún canal los edita todavía.">Ciclo de vida <span className="gtag">del daemon · solo lectura</span></div>
      <div className="group">
        <EngineDefault n="TTL de hibernación" d="Tiempo inactivo antes de que un chat suspenda su engine. Los prompts en vuelo se anclan." value="20" suf="min" />
        <EngineDefault n="Umbral de reciclado de RAM" d="Cuando un engine supera este RSS, se recicla en el próximo momento ocioso." value="4" suf="GB" />
        <EngineDefault n="Intervalo de muestreo" d="Cada cuánto el reaper muestrea RSS y barre las sesiones inactivas." value="30" suf="s" />
      </div>
    </section>
  );
}
