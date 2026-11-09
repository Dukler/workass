// The transcript's text glides to where the right pane's rewrap puts it
// (owner 2026-11-09: opening the pane made the text jump up).
//
// A column that changes width rewraps once. A reader following the bottom
// keeps the bottom, so everything above moves up by however much the text
// below it grew, all in one frame. Moving the whole transcript as one piece
// cannot hide that: paragraphs grow by different amounts, so whichever line
// is held, lines far from it still jump (half the visible growth with the
// middle line held). Each block on screen moves instead (FLIP): it starts
// where it was and glides to its new place in the same 220 ms as the pane. A
// block that grew shows only its old height at first and uncovers its new
// last lines as it goes, so a block never draws over the next one: the space
// between two blocks stays what it is in the new layout.

/** Where a block is on screen, relative to the transcript's viewport. */
export interface BlockBox { top: number; height: number }

/** The frames that take one block from where it was to where it is now. */
export function blockFlipFrames(before: BlockBox, after: BlockBox): Keyframe[] | null {
  const dy = before.top - after.top;
  const grew = after.height - before.height;
  if (Math.abs(dy) < 0.5 && grew < 0.5) return null;
  if (grew < 0.5) return [{ transform: `translateY(${dy}px)` }, { transform: 'translateY(0px)' }];
  // Only the bottom edge is clipped: sideways, marks hung outside the box
  // (list bullets, a code panel's shadow) stay visible.
  return [
    { transform: `translateY(${dy}px)`, clipPath: `inset(0px -100vw ${grew}px -100vw)` },
    { transform: 'translateY(0px)', clipPath: 'inset(0px -100vw 0px -100vw)' },
  ];
}

function transparent(color: string): boolean {
  return color === 'transparent' || /^rgba\(.*,\s*0\)$/.test(color) || /\/\s*0\)$/.test(color);
}

/** A box that only lays out the blocks inside it and draws nothing itself. */
function isFlowWrapper(el: HTMLElement, style: CSSStyleDeclaration, styleOf: (el: Element) => CSSStyleDeclaration): boolean {
  if (!['block', 'flow-root', 'list-item'].includes(style.display)) return false;
  // A box that clips or scrolls (a wide table's sideways scroller) moves as
  // one: a block moved inside it would be cut off, or scroll it.
  if (style.overflowX !== 'visible' || style.overflowY !== 'visible') return false;
  if (!transparent(style.backgroundColor) || style.backgroundImage !== 'none' || style.boxShadow !== 'none') return false;
  if (parseFloat(style.borderTopWidth) || parseFloat(style.borderBottomWidth) || parseFloat(style.borderLeftWidth) || parseFloat(style.borderRightWidth)) return false;
  let blocks = false;
  for (const node of el.childNodes) {
    if (node.nodeType === 3 && node.textContent?.trim()) return false; // its own text moves with it
    if (node.nodeType !== 1) continue;
    const display = styleOf(node as Element).display;
    if (display !== 'none' && !display.startsWith('inline')) blocks = true;
  }
  return blocks;
}

/**
 * The blocks that make up the transcript between `top` and `bottom` (client
 * coordinates): the innermost boxes that draw something or hold text, never
 * one inside another, so moving each by its own offset moves everything once.
 */
export function flipBlocks(root: Element, top: number, bottom: number, styleOf: (el: Element) => CSSStyleDeclaration = getComputedStyle): HTMLElement[] {
  const out: HTMLElement[] = [];
  const visit = (parent: Element) => {
    for (const element of parent.children) {
      const child = element as HTMLElement;
      if (!('offsetParent' in child)) continue; // HTML only: an SVG is never a block here
      const style = styleOf(child);
      if (style.display === 'contents') { visit(child); continue; }
      if (style.display === 'none' || style.position === 'absolute' || style.position === 'fixed' || !child.firstChild) continue;
      const box = child.getBoundingClientRect();
      if (box.height === 0 || box.bottom < top || box.top > bottom) continue;
      if (isFlowWrapper(child, style, styleOf)) visit(child);
      else out.push(child);
    }
  };
  visit(root);
  return out;
}

/**
 * A view that rewraps when its column changes width (the transcript). The
 * pane slide calls `capture` with the slide's timing before the new width is
 * applied: the view notes where its blocks are drawn, and once its own layout
 * pass for the new width has run (its scroll rule applied: the bottom kept, or
 * the reading line held) it starts the blocks' glide with that timing.
 */
export interface ColumnSlideView { capture(timing: KeyframeAnimationOptions): void }

const views = new Set<ColumnSlideView>();

export function registerColumnSlideView(view: ColumnSlideView): () => void {
  views.add(view);
  return () => { views.delete(view); };
}

export function columnSlideViews(): ColumnSlideView[] {
  return [...views];
}
