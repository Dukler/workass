// R5 Revisar diff viewer — a right-side panel (rail-wide width) listing the
// chat's changed files (chat:env-get) with a per-file unified diff (chat:diff)
// from the latest turn checkpoint to the current worktree. Diff lines are block-
// rendered with add/del semantics; the daemon's truncation flag is surfaced.

import { store, useApp } from '../store/store';

function DiffView({ text, truncated }: { text: string; truncated: boolean }) {
  const lines = text.length ? text.split('\n') : [];
  if (lines.length === 0) return <div className="rvnote">Sin diferencias de texto.</div>;
  return (
    <div className="diffbox mono">
      {lines.map((ln, i) => {
        let cls = 'dl';
        if (ln.startsWith('+++') || ln.startsWith('---') || ln.startsWith('diff ') || ln.startsWith('index ')) cls = 'dl dmeta';
        else if (ln.startsWith('@@')) cls = 'dl dhunk';
        else if (ln.startsWith('+')) cls = 'dl dadd';
        else if (ln.startsWith('-')) cls = 'dl ddel';
        return <div key={i} className={cls}>{ln === '' ? ' ' : ln}</div>;
      })}
      {truncated && <div className="dl dtrunc">… diff truncado (límite 200 KiB)</div>}
    </div>
  );
}

export function ReviewPanel() {
  const app = useApp();
  const rv = app.review;
  if (!rv.open) return null;
  const totalFiles = rv.repos.reduce((s, r) => s + r.files.length, 0);

  return (
    <aside className="reviewpanel" role="dialog" aria-label="Revisar cambios">
      <div className="rvhead">
        <b>Revisar cambios</b>
        <span className="sp" />
        <button className="tico" title="Cerrar panel" onClick={() => store.closeReview()}>✕</button>
      </div>

      {rv.error && <div className="rvnote">{rv.error}</div>}
      {rv.loading && <div className="rvnote">Cargando cambios…</div>}
      {!rv.loading && !rv.error && totalFiles === 0 && <div className="rvnote">Sin cambios en este chat.</div>}

      {totalFiles > 0 && (
        <div className="rvbody">
          <div className="rvfiles">
            {rv.repos.filter((r) => r.files.length).map((r) => (
              <div key={r.name} className="rvrepo">
                {rv.repos.filter((x) => x.files.length).length > 1 && <div className="rvrepohd">{r.name}</div>}
                {r.files.map((f) => {
                  const on = rv.active?.repo === r.name && rv.active?.path === f.path;
                  return (
                    <button key={f.path} className={`rvfile ${on ? 'on' : ''}`} title={f.path}
                      onClick={() => void store.selectDiffFile(r.name, f.path)}>
                      <span className="fp">{f.path}</span>
                      <span className="fnum"><span className="stat-a">+{f.adds}</span> <span className="stat-d">−{f.dels}</span></span>
                    </button>
                  );
                })}
              </div>
            ))}
          </div>
          <div className="rvdiff">
            {rv.diffLoading && <div className="rvnote">Cargando diff…</div>}
            {!rv.diffLoading && rv.diff && <DiffView text={rv.diff.text} truncated={rv.diff.truncated} />}
            {!rv.diffLoading && !rv.diff && rv.active && !rv.error && <div className="rvnote">Sin diff para este archivo.</div>}
            {!rv.diffLoading && !rv.active && <div className="rvnote">Elegí un archivo para ver su diff.</div>}
          </div>
        </div>
      )}
    </aside>
  );
}
