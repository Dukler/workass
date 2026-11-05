import { useEffect, useRef, useState } from 'react';
import type { Chat } from '../store/types';
import { store, useClock } from '../store/store';
import { has } from '../wire/api';
import { IcCheck, IcChevron, IcGit, IcPerson, ModelIcon, hasModelIcon, providerIconBrand } from '../icons';
import { relTime } from '../rel-time';
import { changesDiff, openChangesWatch, setChangesChat, useChangesState } from '../changes-store';
import {
  changesView, changesWord, cleanLine, diffLines, diffPeek, filesWord,
  type ChangesFileRow, type ChangesGroup, type ChangesRepoRow, type DiffLine,
} from '../changes-view';
import type { ChangesDiff } from '../wire/types';

// «Cambios» — the activity pane's second tab (approved mock rail-cambios-v2):
// every repo the owner's chats touched recently, flat against the pane. No
// scope switch and no refresh button: the daemon looks again on its own while
// this tab is on screen (changes-store.ts), and a repo's branch label pulses
// faintly while it is being looked at.

const PEEK_DELAY_MS = 220;
const PEEK_HIDE_MS = 140;

// What the user folded or unfolded, and which diffs are expanded, for the
// window's life. A repo nobody touched follows the default: the most recently
// changed one open, the rest folded.
const folds = new Map<string, boolean>();
const expanded = new Set<string>();

/** Fold or unfold a repo row; kept for the window's life. */
export function setCambiosFold(root: string, open: boolean): void {
  folds.set(root, open);
}

/** Tests only. */
export function resetCambiosView(): void {
  folds.clear();
  expanded.clear();
}

function absolutePath(root: string, path: string): string {
  const sep = root.includes('\\') && !root.includes('/') ? '\\' : '/';
  return `${root.replace(/[/\\]+$/, '')}${sep}${path.split('/').join(sep)}`;
}

async function copyText(text: string): Promise<void> {
  if (!text || typeof navigator === 'undefined' || !navigator.clipboard) return;
  await navigator.clipboard.writeText(text).catch(() => undefined);
}

function RelAt({ at }: { at: number }) {
  useClock();
  const label = relTime(new Date(at).toISOString());
  return label ? <span className="cb-tm">{label}</span> : null;
}

function ProviderMark({ providerId }: { providerId: string }) {
  const brand = providerIconBrand(providerId, null);
  if (hasModelIcon(brand)) return <ModelIcon provider={brand} />;
  const letter = (providerId || '·').trim().charAt(0).toUpperCase() || '·';
  return <span className="cb-mk-l">{letter}</span>;
}

function Stat({ adds, dels }: { adds: number; dels: number }) {
  return (
    <>
      {adds > 0 && <span className="a">+{adds}</span>}
      {dels > 0 && <span className="d">−{dels}</span>}
    </>
  );
}

function Lines({ lines }: { lines: DiffLine[] }) {
  return (
    <>
      {lines.map((line, index) => (
        <div key={index} className={`cb-ln${line.kind === '+' ? ' cb-add' : line.kind === '-' ? ' cb-del' : line.kind === '@' || line.kind === '…' ? ' cb-hunk' : ''}`}>
          {line.kind === '…' ? '⋯' : line.text || ' '}
        </div>
      ))}
    </>
  );
}

export function Peek({ file, diff, above, onExpand, onCopy }: {
  file: ChangesFileRow; diff: ChangesDiff; above: boolean; onExpand: () => void; onCopy: () => void;
}) {
  const peek = diffPeek(diff.text);
  return (
    <div className={`cb-peek${above ? ' above' : ''}`} role="tooltip" onMouseDown={(event) => event.preventDefault()}>
      <div className="cb-peek-hd">
        <span className="cb-peek-t">{file.name} · {diff.binary ? 'binario' : changesWord(Math.max(1, peek.hunks))}</span>
        {peek.from > 0 && <span className="cb-peek-r">{peek.from === peek.to ? `línea ${peek.from}` : `líneas ${peek.from}–${peek.to}`}</span>}
      </div>
      {diff.binary
        ? <div className="cb-ln cb-info">Archivo binario</div>
        : peek.lines.length ? <Lines lines={peek.lines} /> : <div className="cb-ln cb-info">Sin líneas que mostrar</div>}
      <div className="cb-peek-ft">
        {!diff.binary && <button type="button" className="cb-link" onClick={onExpand}>Ver diff completo</button>}
        <button type="button" className="cb-link muted" onClick={onCopy}>Copiar ruta</button>
      </div>
    </div>
  );
}

export function FullDiff({ diff, onClose }: { diff: ChangesDiff; onClose: () => void }) {
  const lines = diffLines(diff.text);
  return (
    <div className="cb-full">
      <div className="cb-full-hd">
        <span className="cb-full-t" title={diff.path}>{diff.path}</span>
        <button type="button" className="cb-link muted" onClick={onClose}>Cerrar</button>
      </div>
      <div className="cb-full-body">
        {lines.length ? <Lines lines={lines} /> : <div className="cb-ln cb-info">Sin líneas que mostrar</div>}
        {diff.truncated && <div className="cb-ln cb-info">diff recortado</div>}
      </div>
    </div>
  );
}

function FileRow({ root, file }: { root: string; file: ChangesFileRow }) {
  const key = `${root}\u0000${file.path}`;
  const stamp = `${file.adds}:${file.dels}:${file.isNew ? 1 : 0}`;
  const [peek, setPeek] = useState<{ diff: ChangesDiff; above: boolean } | null>(null);
  const [isExpanded, setExpanded] = useState(() => expanded.has(key));
  const [full, setFull] = useState<ChangesDiff | null>(null);
  const rowRef = useRef<HTMLDivElement | null>(null);
  const showTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const hideTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const hovered = useRef(false);

  useEffect(() => () => {
    if (showTimer.current) clearTimeout(showTimer.current);
    if (hideTimer.current) clearTimeout(hideTimer.current);
  }, []);

  // An expanded diff is fetched when it opens, and again when the file's
  // counts change.
  useEffect(() => {
    if (!isExpanded) { setFull(null); return; }
    let live = true;
    void changesDiff(root, file.path, stamp).then((diff) => { if (live) setFull(diff); });
    return () => { live = false; };
  }, [isExpanded, root, file.path, stamp]);

  const enter = () => {
    hovered.current = true;
    if (hideTimer.current) { clearTimeout(hideTimer.current); hideTimer.current = null; }
    if (peek || isExpanded || showTimer.current) return;
    showTimer.current = setTimeout(() => {
      showTimer.current = null;
      void changesDiff(root, file.path, stamp).then((diff) => {
        if (!diff || !hovered.current) return;
        const rect = rowRef.current?.getBoundingClientRect();
        const above = !!rect && typeof window !== 'undefined' && rect.bottom + 230 > window.innerHeight && rect.top > 240;
        setPeek({ diff, above });
      });
    }, PEEK_DELAY_MS);
  };
  const leave = () => {
    hovered.current = false;
    if (showTimer.current) { clearTimeout(showTimer.current); showTimer.current = null; }
    if (hideTimer.current) clearTimeout(hideTimer.current);
    hideTimer.current = setTimeout(() => { hideTimer.current = null; setPeek(null); }, PEEK_HIDE_MS);
  };
  const expand = () => {
    expanded.add(key);
    setExpanded(true);
    setPeek(null);
  };
  const collapse = () => {
    expanded.delete(key);
    setExpanded(false);
  };

  return (
    <>
      <div
        ref={rowRef}
        className={`cb-f${peek ? ' hv' : ''}`}
        tabIndex={0}
        onMouseEnter={enter}
        onMouseLeave={leave}
        onFocus={enter}
        onBlur={leave}
        onKeyDown={(event) => {
          if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); if (isExpanded) collapse(); else expand(); }
          if (event.key === 'Escape') setPeek(null);
        }}
      >
        <span className="cb-fn" title={file.path}>{file.name}</span>
        {file.folder && <span className="cb-dir" title={file.path}>{file.folder}</span>}
        {file.at > 0 && <RelAt at={file.at} />}
        {file.isNew && <span className="cb-tag">nuevo</span>}
        <span className="cb-s"><Stat adds={file.adds} dels={file.dels} /></span>
        {peek && (
          <Peek file={file} diff={peek.diff} above={peek.above} onExpand={expand}
            onCopy={() => { void copyText(absolutePath(root, file.path)); setPeek(null); }} />
        )}
      </div>
      {isExpanded && full && <FullDiff diff={full} onClose={collapse} />}
    </>
  );
}

function Group({ root, group }: { root: string; group: ChangesGroup }) {
  const count = filesWord(group.files.length);
  return (
    <div className="cb-grp">
      <div className={`cb-who ${group.kind}`}>
        <span className="cb-mk" aria-hidden="true">{group.kind === 'outside' ? <IcPerson /> : <ProviderMark providerId={group.providerId} />}</span>
        {group.kind === 'outside'
          ? <span className="cb-who-t">Fuera de Workass · {count}</span>
          : (
            <span className="cb-who-t">
              <b title={group.title}>{group.title}</b>
              <span className="cb-who-m">{group.kind === 'this' ? ` · este chat · ${count}` : ` · ${count}`}</span>
            </span>
          )}
      </div>
      {group.files.map((file) => <FileRow key={file.path} root={root} file={file} />)}
    </div>
  );
}

function RepoRow({ repo, defaultOpen }: { repo: ChangesRepoRow; defaultOpen: boolean }) {
  const [, setTick] = useState(0);
  const open = folds.has(repo.root) ? folds.get(repo.root) === true : defaultOpen;
  const toggle = () => {
    setCambiosFold(repo.root, !open);
    setTick((tick) => tick + 1);
  };
  return (
    <div className={`cb-repo${open ? ' open' : ''}`}>
      <button type="button" className="cb-rh" aria-expanded={open} onClick={toggle}>
        <span className="cb-chev" aria-hidden="true"><IcChevron /></span>
        <span className="cb-name" title={repo.root}>{repo.name}</span>
        {repo.branch && (
          <span className={`cb-br${repo.checking ? ' pulse' : ''}`} title={repo.branch}>
            <IcGit /><span className="cb-br-t">{repo.branch}</span>
          </span>
        )}
        {repo.ahead > 0 && <span className="cb-sync" title={`${repo.ahead} sin subir`}>↑{repo.ahead}</span>}
        {repo.behind > 0 && <span className="cb-sync" title={`${repo.behind} por bajar`}>↓{repo.behind}</span>}
        <span className="cb-ds">
          <span className="cb-blk" aria-hidden="true">{repo.bar.map((cell, index) => <i key={index} className={cell} />)}</span>
          <Stat adds={repo.adds} dels={repo.dels} />
        </span>
      </button>
      {open && (
        <div className="cb-body">
          {repo.groups.map((group) => <Group key={group.key} root={repo.root} group={group} />)}
          {repo.filesTruncated && <div className="cb-trunc">{`se muestran ${repo.shown} de ${repo.fileCount} archivos`}</div>}
        </div>
      )}
    </div>
  );
}

/** How many files the last snapshot shows changed in this chat's repos, for
 *  the tab's count. */
export function cambiosCount(state: ReturnType<typeof useChangesState>, chatId: string): number {
  return changesView(state, { chatId }).fileCount;
}

export function CambiosTab({ chat }: { chat: Chat | null }) {
  const state = useChangesState();
  const chatId = chat?.chatId ?? '';
  // Before the watch opens, so its first request already names the chat.
  useEffect(() => setChangesChat(chatId), [chatId]);
  // The watch lives exactly as long as this tab is on screen.
  useEffect(() => openChangesWatch(), []);
  const chats = store.state.chats ?? [];
  const view = changesView(state, {
    chatId,
    chatTitle: (chatId) => chats.find((item) => item.chatId === chatId)?.title || undefined,
    chatProvider: (chatId) => chats.find((item) => item.chatId === chatId)?.providerId || undefined,
  });

  if (!has('changesWatch') || view.empty) return <div className="rp-empty">Sin cambios todavía</div>;
  if (view.pending) return <div className="cb-list" aria-busy="true" />;
  return (
    <div className="cb-list">
      {view.repos.map((repo) => <RepoRow key={repo.root} repo={repo} defaultOpen={repo.root === view.openRoot} />)}
      {view.clean.count > 0 && (
        <div className="cb-clean" title={view.clean.names.join(', ')}>
          <IcCheck />{cleanLine(view.clean.count)}
        </div>
      )}
    </div>
  );
}
