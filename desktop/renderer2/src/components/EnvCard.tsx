import { useEffect } from 'react';
import type { Chat } from '../store/types';
import { store, useChatEnv } from '../store/store';
import { envView, type EnvGroup, type EnvFileRow } from '../env-files';

// How many file rows a repo group shows before the rest fold into a disclosure.
const ROW_CAP = 8;

async function copyPath(path: string) {
  if (!path || typeof navigator === 'undefined' || !navigator.clipboard) return;
  await navigator.clipboard.writeText(path).catch(() => undefined);
}

function FileRow({ row }: { row: EnvFileRow }) {
  return (
    <button type="button" className="efile" title={`Copiar ruta · ${row.path}`} onClick={() => void copyPath(row.path)}>
      <span className="efname">{row.name}</span>
      {row.parent && <span className="efpar">{row.parent}</span>}
      <span className="sp" />
      <span className="efstat" aria-label={`+${row.adds} −${row.dels}`}>
        {row.adds > 0 && <span className="a">+{row.adds}</span>}
        {row.dels > 0 && <span className="d">−{row.dels}</span>}
      </span>
    </button>
  );
}

function RepoGroup({ group, withHead }: { group: EnvGroup; withHead: boolean }) {
  const head = group.rows.slice(0, ROW_CAP);
  const rest = group.rows.slice(ROW_CAP);
  return (
    <div className="egroup">
      {withHead && (
        <div className="egrouphd">
          <span className="egname">{group.name}</span>
          {group.branch && <span className="egbranch">{group.branch}</span>}
        </div>
      )}
      {head.map((row) => <FileRow key={row.path} row={row} />)}
      {rest.length > 0 && (
        <details className="emore">
          <summary>{rest.length} {rest.length === 1 ? 'archivo más' : 'archivos más'}</summary>
          {rest.map((row) => <FileRow key={row.path} row={row} />)}
        </details>
      )}
      {group.filesTruncated && <div className="etrunc">lista recortada</div>}
    </div>
  );
}

// Entorno → the "Archivos" rail row: a flush count that expands to the
// filename-first list of what this chat's current/most-recent turn changed.
// De-carded (mock b3) — no bordered box, just a row in the rail column.
export function EnvCard({ chat }: { chat: Chat }) {
  useChatEnv();
  useEffect(() => { void store.refreshChatEnv(chat); }, [chat]);

  const view = envView(store.chatEnv(chat));
  const multi = view.groups.length > 1;
  const soleRepo = !multi ? view.groups[0] : undefined;

  // No changes → render nothing. A "sin cambios" row is a scrap in the idle
  // rail; the Turno empty-state carries the "nothing yet" message instead.
  if (!view.hasChanges) return null;

  return (
    <details className="r-meta">
      <summary>
        <span className="r-ml">Archivos</span>
        <span className="r-count">{view.fileCount}</span>
      </summary>
      <div className="r-meta-b">
        <div className="elist">
          {view.groups.map((group) => <RepoGroup key={group.name} group={group} withHead={multi} />)}
          {soleRepo?.branch && <div className="efoot">{soleRepo.name} · {soleRepo.branch}</div>}
          {view.reposTruncated && <div className="etrunc">más repositorios sin mostrar</div>}
        </div>
      </div>
    </details>
  );
}
