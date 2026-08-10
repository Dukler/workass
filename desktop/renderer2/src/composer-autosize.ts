const DEFAULT_MAX_HEIGHT = 180;
const WIDTH_EPSILON = 0.5;

type ResizeObserverLike = Pick<ResizeObserver, 'observe' | 'disconnect'>;
type ResizeObserverCtor = new (callback: ResizeObserverCallback) => ResizeObserverLike;

export function syncComposerTextareaFade(el: HTMLTextAreaElement): void {
  el.parentElement?.classList.toggle('scrolled', el.scrollTop > 1);
}

export function autosizeComposerTextarea(el: HTMLTextAreaElement, maxHeight = DEFAULT_MAX_HEIGHT): void {
  // Empty box: drop the inline height entirely and let CSS collapse it — never
  // trust a measurement that can only make an empty field taller.
  if (!el.value) {
    el.style.height = '';
    el.style.overflowY = 'hidden';
    syncComposerTextareaFade(el);
    return;
  }

  el.style.height = 'auto';
  const full = el.scrollHeight;
  el.style.height = `${Math.min(full, maxHeight)}px`;
  // Below the cap the box always grows to fit, so it must not scroll. Only once
  // the draft reaches the cap do wheel/caret scrolling and the top fade engage.
  el.style.overflowY = full > maxHeight ? 'auto' : 'hidden';
  syncComposerTextareaFade(el);
}

/**
 * A chat switch can change the per-chat right pane after the new Composer has
 * mounted. The first autosize therefore measures the previous chat's width.
 * Observe width only (height changes are caused by autosize itself) and repeat
 * the measurement once the selected chat's layout has actually landed.
 */
export function observeComposerTextareaWidth(
  el: HTMLTextAreaElement,
  onWidthChange: () => void,
  ObserverCtor?: ResizeObserverCtor,
): () => void {
  const Ctor = ObserverCtor ?? (typeof ResizeObserver === 'undefined' ? null : ResizeObserver);
  if (!Ctor) return () => undefined;

  let width = el.getBoundingClientRect().width;
  const observer = new Ctor((entries) => {
    const entry = entries.find((candidate) => candidate.target === el) ?? entries[0];
    const next = entry?.contentRect.width ?? el.getBoundingClientRect().width;
    if (!Number.isFinite(next) || Math.abs(next - width) < WIDTH_EPSILON) return;
    width = next;
    onWidthChange();
  });
  observer.observe(el);
  return () => observer.disconnect();
}
