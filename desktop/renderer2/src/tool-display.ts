// Tool-call detail display (approved mock 2026-07-15, toolrow-redesign v2):
// paths shrink to what identifies them — relative to the chat cwd when inside
// it, «~/» for the home folder — and when a path still can't fit, CSS collapses
// the MIDDLE so the filename never drops (two-segment head/tail split below).

// Home root inferred from the chat cwd (macOS /Users/x, Linux /home/x).
export function homeOf(cwd: string | null): string | null {
  const m = /^(\/(?:Users|home)\/[^/]+)(?:\/|$)/.exec(cwd ?? '');
  return m ? m[1] : null;
}

// Shorten every cwd/home occurrence inside a path OR command string.
export function displayDetail(text: string, cwd: string | null): string {
  let out = text;
  if (cwd && cwd !== '/') {
    out = out.split(cwd + '/').join('');
    out = out.split(cwd).join('.');
  }
  const home = homeOf(cwd);
  if (home) {
    out = out.split(home + '/').join('~/');
    out = out.split(home).join('~');
  }
  return out;
}

// A lone path (no spaces) splits at the last slash so CSS can ellipsize the
// head while the filename tail never shrinks. Commands don't split — their
// informative part is the head (the verb), so plain end-ellipsis is right.
export function splitTail(detail: string): { head: string; tail: string } | null {
  if (/\s/.test(detail)) return null;
  const cut = detail.lastIndexOf('/');
  if (cut <= 0 || cut === detail.length - 1) return null;
  return { head: detail.slice(0, cut + 1), tail: detail.slice(cut + 1) };
}
