import { useEffect, useState } from 'react';
import type { RemoteMachineBadge } from '../machine-label';
import { cachedProjectIcon, isProjectIconURL, loadProjectIcon, loadProjectIconBytes } from '../project-icon';
import { IcFolder } from '../icons';

export function ProjectIcon({
  chatId,
  cwd,
  remote,
  className = '',
}: {
  chatId: string;
  cwd: string;
  remote?: RemoteMachineBadge | null;
  className?: string;
}) {
  const [source, setSource] = useState<string | null | undefined>(() => cachedProjectIcon(chatId, cwd));
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let live = true;
    setSource(cachedProjectIcon(chatId, cwd));
    setFailed(false);
    void loadProjectIcon(chatId, cwd).then((next) => { if (live) setSource(next); });
    return () => { live = false; };
  }, [chatId, cwd]);

  const accessibility = remote
    ? { role: 'img', 'aria-label': remote.title }
    : { 'aria-hidden': true as const };

  return (
    <span className={`sv2-picon ${className}`.trim()} title={remote?.title} {...accessibility}>
      {source && !failed
        ? <img src={source} alt="" onError={() => {
          // An icon by URL that did not load is read over the socket instead.
          if (isProjectIconURL(source)) void loadProjectIconBytes(chatId, cwd).then((next) => { if (next) setSource(next); else setFailed(true); });
          else setFailed(true);
        }} />
        : <IcFolder />}
      {remote && <span className="sv2-remote" aria-hidden="true">{remote.initial}</span>}
    </span>
  );
}
