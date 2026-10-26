import assert from 'node:assert/strict';
import test from 'node:test';
import { browserPageHosted } from '../src/browser.ts';

const hostingApi = { hostMode: () => 'webview' as const, registerGuest: async () => ({ ok: true }) };

test('a page is drawn here only when this renderer hosts pages and the page itself is hosted', () => {
  // The renderer asked for the webview host; a page made before it asked is
  // still a native view, stacked over the window, and is treated as one.
  assert.equal(browserPageHosted(hostingApi as never, 'view'), false);
  assert.equal(browserPageHosted(hostingApi as never, 'webview'), true);
  // A state that does not say yet follows the renderer's host.
  assert.equal(browserPageHosted(hostingApi as never, undefined), true);
  // A shell without the webview host: never hosted.
  assert.equal(browserPageHosted({ hostMode: () => 'view' as const } as never, 'webview'), false);
  assert.equal(browserPageHosted(undefined, 'webview'), false);
});
