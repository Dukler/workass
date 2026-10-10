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

test('middle-click on returned artifact Markdown stays in the authorized Workass browser', async (t) => {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  t.after(() => server.close());
  const { renderInline } = await server.ssrLoadModule('/src/markdown/inline.tsx');
  const { Store, store } = await server.ssrLoadModule('/src/store/store.ts');
  const previousWindow = (globalThis as any).window;
  const navigations: unknown[][] = [];
  (globalThis as any).window = {
    location: { origin: 'http://127.0.0.1:8799' },
    workassArtifacts: { supported: true, onRequest: () => () => {}, onCancel: () => () => {}, reply: async () => {} },
    workassBrowser: { supported: true, command: async (...args: unknown[]) => { navigations.push(args); } },
  };
  const subject = new Store();
  subject.state.chats = [{ id: 'artifact-tab', chatId: 'artifact-chat', pane: 'rail' }];
  subject.selfMachineId = 'm-3a099d407d6e7a67fcaa7ed8d421b9c1';
  subject.bumpChat = () => {};
  t.after(() => {
    subject.clearToastTimers(); store.clearToastTimers();
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  });
  // Exact public Markdown from the incident's .md and .txt host receipts.
  for (const label of [
    'depthvision-speed-research-plan-da05e54d7f9d',
    'depthvision-research-plan-text-bf72919a42d7',
  ]) {
    const target = `/workass/artifacts/@m-3a099d407d6e7a67fcaa7ed8d421b9c1/${label}/`;
    const name = label.slice(0, label.lastIndexOf('-'));
    const nodes = renderInline(`[Open ${name}](${target})`, 'returned', true, {
      revision: target, resolve: () => null, open: () => {},
      resolveLink: (href: string) => href,
      openLink: (href: string) => subject.openHostedArtifact('artifact-tab', href),
    });
    const anchor = nodes.find((node: unknown) => React.isValidElement(node) && node.type === 'a');
    let prevented = false;
    anchor.props.onAuxClick?.({ button: 1, preventDefault: () => { prevented = true; } });
    assert.equal(prevented, true, 'middle-click must not open the private URL in an external browser');
    assert.deepEqual(navigations.at(-1), ['artifact-tab', 'navigate', `http://127.0.0.1:8799${target}`]);
    const count = navigations.length;
    anchor.props.onAuxClick?.({ button: 2, preventDefault: () => { throw new Error('right-click must retain its context menu'); } });
    assert.equal(navigations.length, count);
  }
  const ordinary = renderInline('[site](https://example.com)', 'ordinary', true, {
    revision: 'ordinary', resolve: () => null, open: () => {}, openLink: () => { throw new Error('ordinary links must keep native behavior'); },
  }).find((node: unknown) => React.isValidElement(node) && node.type === 'a');
  assert.equal(ordinary.props.onAuxClick, undefined);
});
