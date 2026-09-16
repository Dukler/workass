import assert from 'node:assert/strict';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';

test('copied artifact addresses stay native links and images across chats', async (t) => {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  t.after(() => server.close());
  const { renderInline } = await server.ssrLoadModule('/src/markdown/inline.tsx');
  const { connectedArtifactURL } = await server.ssrLoadModule('/src/connected-artifacts.ts');
  const origin = 'http://127.0.0.1:8799';
  for (const target of [
    '/workass/artifacts/@owner/report/preview.png',
    'http://127.0.0.1:8798/workass/artifacts/@owner/report/preview.png',
    'http://127.0.0.1:8798/workass/connected-artifacts/owner/report/preview.png',
  ]) {
    let opened = '';
    const media = {
      revision: target, resolve: () => null, open: () => {},
      resolveLink: (href: string) => connectedArtifactURL('different-chat-owner', href, origin, true),
      openLink: (href: string) => { opened = href; return true; },
    };
    const nodes = renderInline(`[preview](${target})`, 'link', true, media);
    const anchor = nodes.find((node: unknown) => React.isValidElement(node) && node.type === 'a');
    assert.ok(anchor, 'a hosted PNG link must not be mistaken for an inaccessible local file');
    assert.equal(anchor.props.href, `${origin}/workass/artifacts/@owner/report/preview.png`);
    let prevented = false;
    anchor.props.onClick({ preventDefault: () => { prevented = true; } });
    assert.equal(opened, target);
    assert.equal(prevented, true);
    const image = renderToStaticMarkup(React.createElement(React.Fragment, null,
      renderInline(`![preview](${target})`, 'image', true, media)));
    assert.ok(image.includes(`src="${origin}/workass/artifacts/@owner/report/preview.png"`));
  }
});
