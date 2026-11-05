import type { Chat } from '../store/types';
import { store, useChatEnv } from '../store/store';
import { envView, splitPath, type EnvGroup, type EnvFileRow } from '../env-files';
import { IcFile } from '../icons';

// How many file rows a repo group shows before the rest fold into a disclosure.
const ROW_CAP = 8;

async function copyPath(path: string) {
  if (!path || typeof navigator === 'undefined' || !navigator.clipboard) return;
  await navigator.clipboard.writeText(path).catch(() => undefined);
}

// The muted folder beside a name: the context that tells same-name files apart
// when there is one, else the file's own folder (its last two segments).
function folderOf(row: EnvFileRow): string {
  if (row.parent) return row.parent;
  const dirs = splitPath(row.path).slice(0, -1);
  if (dirs.length === 0) return '';
  return dirs.length > 2 ? `…/${dirs.slice(-2).join('/')}` : dirs.join('/');
}

function FileRow({ row }: { row: EnvFileRow }) {
  const folder = folderOf(row);
  return (
    <button type="button" className="efile" title={`Copiar ruta · ${row.path}`} onClick={() => void copyPath(row.path)}>
      <span className="efic" aria-hidden="true"><IcFile /></span>
      <span className="efname">{row.name}</span>
      {folder && <span className="efpar">{folder}</span>}
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
          <span className="egname" title={group.name}>{group.name}</span>
          {group.branch && <span className="egbranch" title={group.branch}>{group.branch}</span>}
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

/** How many files this chat's current or most recent turn changed. */
export function envFileCount(chat: Chat | null): number {
  return chat ? envView(store.chatEnv(chat)).fileCount : 0;
}

// «Archivos» — the activity pane's second tab: the filename-first list of what
// this chat's current or most recent turn changed, flat rows against the pane
// (mock rail-v7). A row copies its path; the branch closes the list. The pane
// reads the snapshot on mount (RightRail), so this tab only draws it.
export function EnvCard({ chat }: { chat: Chat }) {
  useChatEnv();
  const view = envView(store.chatEnv(chat));
  const multi = view.groups.length > 1;
  const soleRepo = !multi ? view.groups[0] : undefined;

  if (!view.hasChanges) return <div className="rp-empty">Sin cambios</div>;

  return (
    <div className="elist">
      {view.groups.map((group) => <RepoGroup key={group.name} group={group} withHead={multi} />)}
      {soleRepo?.branch && <div className="efoot" title={`${soleRepo.name} · ${soleRepo.branch}`}>{soleRepo.name} · {soleRepo.branch}</div>}
      {view.reposTruncated && <div className="etrunc">más repositorios sin mostrar</div>}
    </div>
  );
}
