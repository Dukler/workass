import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

// 2026-11-08: Esc on an opened image closed it and also stopped the running
// turn, because the viewer listened beside the app's Esc instead of above it.
// Overlays take Esc through the shared escape stack, which handles it in the
// capture phase and stops it there.
test('the image viewer and the full-screen browser own Esc while open', () => {
  for (const file of ['ImageLightbox.tsx', 'FloatingBrowser.tsx']) {
    const source = readFileSync(new URL(`../src/components/${file}`, import.meta.url), 'utf8');
    assert.match(source, /pushEscapeLayer\(\(\) => store\.(closeImageLightbox|toggleBrowserFullscreen)\(\)\)/, file);
    assert.doesNotMatch(source, /key === 'Escape'\) store\.(closeImageLightbox|toggleBrowserFullscreen)/, file);
  }
});
