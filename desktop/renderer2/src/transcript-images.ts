type ImageVisibilityObserver = Pick<IntersectionObserver, 'observe' | 'disconnect'>;
type ImageVisibilityObserverConstructor = new (
  callback: IntersectionObserverCallback,
  options?: IntersectionObserverInit,
) => ImageVisibilityObserver;

// Loading is a one-way transition for a mounted transcript image. Clearing its
// source when it leaves the viewport also collapses its height and changes the
// reader's scroll position, repeatedly fetching the same image on the way back.
export function observeTranscriptImageOnce(
  element: Element,
  onVisible: () => void,
  Observer: ImageVisibilityObserverConstructor | undefined = globalThis.IntersectionObserver,
): () => void {
  let active = true;
  let revealed = false;
  let observer: ImageVisibilityObserver | undefined;
  const reveal = () => {
    if (!active || revealed) return;
    revealed = true;
    observer?.disconnect();
    onVisible();
  };
  if (Observer) {
    observer = new Observer((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) reveal();
    }, { rootMargin: '400px' });
    observer.observe(element);
  } else {
    reveal();
  }
  return () => { active = false; observer?.disconnect(); };
}
