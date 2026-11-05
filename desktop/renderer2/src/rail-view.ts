// What the activity pane remembers per chat for the session: the open tab and
// whether the queue is open. Kept for the window's life (sessionStorage, so a
// renderer reload keeps it too) and never written to the daemon: it is how
// this window looks, not a property of the chat.

export type RailTab = 'trabajos' | 'cambios' | 'salidas';
export const RAIL_TABS: readonly RailTab[] = ['trabajos', 'cambios', 'salidas'];

interface RailView { tab?: RailTab; queueOpen?: boolean }

const STORAGE_KEY = 'workass.railView.v1';
const LIMIT = 200;
let views: Map<string, RailView> | null = null;

function storage(): Storage | null {
  try { return typeof sessionStorage !== 'undefined' ? sessionStorage : null; } catch { return null; }
}

function load(): Map<string, RailView> {
  if (views) return views;
  views = new Map();
  try {
    const raw = storage()?.getItem(STORAGE_KEY);
    const parsed = raw ? JSON.parse(raw) as unknown : null;
    if (parsed && typeof parsed === 'object') {
      for (const [key, value] of Object.entries(parsed as Record<string, RailView>)) {
        if (value && typeof value === 'object') views.set(key, value);
      }
    }
  } catch { /* a corrupt entry only costs the remembered view */ }
  return views;
}

function save(): void {
  const all = load();
  while (all.size > LIMIT) all.delete(all.keys().next().value as string);
  try { storage()?.setItem(STORAGE_KEY, JSON.stringify(Object.fromEntries(all))); } catch { /* private mode */ }
}

function update(chatKey: string, patch: RailView): void {
  if (!chatKey) return;
  const all = load();
  const next = { ...all.get(chatKey), ...patch };
  all.delete(chatKey);
  all.set(chatKey, next);
  save();
}

export function railTab(chatKey: string): RailTab {
  const tab = load().get(chatKey)?.tab as string | undefined;
  // «Archivos» became «Cambios»; a window that remembered it keeps its place.
  if (tab === 'archivos') return 'cambios';
  return tab && (RAIL_TABS as readonly string[]).includes(tab) ? tab as RailTab : 'trabajos';
}
export function setRailTab(chatKey: string, tab: RailTab): void { update(chatKey, { tab }); }

export function railQueueOpen(chatKey: string): boolean { return load().get(chatKey)?.queueOpen === true; }
export function setRailQueueOpen(chatKey: string, open: boolean): void { update(chatKey, { queueOpen: open }); }

/** Tests only: forget every remembered view. */
export function resetRailViews(): void {
  views = new Map();
  try { storage()?.removeItem(STORAGE_KEY); } catch { /* nothing kept */ }
}
