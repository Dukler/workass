// Composer input belongs to this controller, not the shared chat snapshot.
// A separate key keeps typed drafts across reloads without importing another
// controller's editor or resurrecting legacy actor-owned draft text.
function key(tabId: string, chatId: string): string {
  return `workass.renderer2.composer.v1:${JSON.stringify([tabId, chatId])}`;
}

export function loadComposerDraft(tabId: string, chatId: string): string {
  try { return localStorage.getItem(key(tabId, chatId)) ?? ''; } catch { return ''; }
}

export function saveComposerDraft(tabId: string, chatId: string, value: string): void {
  try { localStorage.setItem(key(tabId, chatId), value); } catch { /* preserve the live editor if storage is unavailable */ }
}
