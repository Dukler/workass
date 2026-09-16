import assert from 'node:assert/strict';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';
import { connectedArtifactURL } from '../src/connected-artifacts.ts';

test('artifact URL qualification preserves local paths and binds remote paths to the local shell', () => {
  assert.equal(connectedArtifactURL('', '/workass/artifacts/local-id/'), '/workass/artifacts/local-id/');
  assert.equal(
    connectedArtifactURL('san-laptop', '/workass/artifacts/remote-id/', 'http://127.0.0.1:8799', true),
    'http://127.0.0.1:8799/workass/connected-artifacts/san-laptop/remote-id/',
  );
  assert.equal(connectedArtifactURL('san-laptop', 'https://already.absolute/workass/artifacts/id/', 'http://127.0.0.1:8799', true), 'http://127.0.0.1:8799/workass/connected-artifacts/san-laptop/id/');
  assert.equal(connectedArtifactURL('', 'https://example.com/a'), 'https://example.com/a');
});

test('visualized artifact navigation keeps the cross-origin iframe decision executable', async (t) => {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  t.after(async () => { await server.close(); });
  const { visualizationNeedsTopLevelOpen } = await server.ssrLoadModule('/src/markdown/VisualizeBlock.tsx') as {
    visualizationNeedsTopLevelOpen: (origin?: string, currentOrigin?: string) => boolean;
  };
  assert.equal(visualizationNeedsTopLevelOpen(''), false);
  assert.equal(visualizationNeedsTopLevelOpen('https://san-laptop.example:8788', 'http://127.0.0.1:8799'), true);
  assert.equal(visualizationNeedsTopLevelOpen('http://127.0.0.1:8799', 'http://127.0.0.1:8799'), false);
});

test('remote hosted artifact links and images use the owning machine private bridge', async (t) => {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  t.after(async () => { await server.close(); });

  const { renderInline } = await server.ssrLoadModule('/src/markdown/inline.tsx') as {
    renderInline: (text: string, keyBase?: string, allowLinks?: boolean, media?: unknown) => React.ReactNode[];
  };
  const remoteArtifact = 'http://127.0.0.1:8799/workass/connected-artifacts/san-laptop/report-id/';
  const nodes = renderInline(
    '[report](/workass/artifacts/report-id/) ![preview](/workass/artifacts/report-id/preview.png)',
    'remote',
    true,
    {
      revision: 'remote-v1',
      resolve: () => null,
      resolveLink: (target: string) => connectedArtifactURL('san-laptop', target, 'http://127.0.0.1:8799', true),
      open: () => {},
    },
  );
  const html = renderToStaticMarkup(React.createElement(React.Fragment, null, nodes));

  // The daemon deliberately emits a relative path. A remote chat must qualify
  // it before the browser/image request reaches the controller's daemon.
  assert.ok(html.includes(`href="${remoteArtifact}"`));
  assert.ok(html.includes(`src="${remoteArtifact}preview.png"`));
  assert.doesNotMatch(html, /href="\/workass\/artifacts\//);
  assert.doesNotMatch(html, /src="\/workass\/artifacts\//);
  let opened = '';
  const clickNodes = renderInline(
    '[report](/workass/artifacts/report-id/)', 'click', true, {
      revision: 'click-v1', resolve: () => null,
      resolveLink: (target: string) => connectedArtifactURL('san-laptop', target, 'http://127.0.0.1:8799', true),
      openLink: (target: string) => { opened = target; return true; }, open: () => {},
    },
  );
  const link = clickNodes.find((node) => React.isValidElement(node) && node.type === 'a') as React.ReactElement<{ href?: string; onClick?: (event: { preventDefault(): void }) => void }>;
  assert.equal(link.props.href, remoteArtifact);
  let prevented = false;
  link.props.onClick?.({ preventDefault: () => { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(opened, '/workass/artifacts/report-id/');

  const ordinary = renderInline('[site](https://example.com)', 'ordinary', true, {
    revision: 'ordinary-v1', resolve: () => null, openLink: () => false, open: () => {},
  }).find((node) => React.isValidElement(node) && node.type === 'a') as React.ReactElement<{ onClick?: (event: { preventDefault(): void }) => void }>;
  let ordinaryPrevented = false;
  ordinary.props.onClick?.({ preventDefault: () => { ordinaryPrevented = true; } });
  assert.equal(ordinaryPrevented, false);

  const unavailable = renderInline('![preview](/workass/artifacts/report-id/preview.png)', 'missing', true, {
    revision: 'missing-v1', resolve: () => null, resolveLink: () => '', open: () => {},
  });
  assert.match(renderToStaticMarkup(React.createElement(React.Fragment, null, unavailable)), /Imagen no disponible/);
  assert.doesNotMatch(renderToStaticMarkup(React.createElement(React.Fragment, null, unavailable)), /<img/);
  const unavailableLink = renderInline('[report](/workass/artifacts/report-id/)', 'missing-link', true, {
    revision: 'missing-v1', resolve: () => null, resolveLink: () => '', open: () => {},
  });
  const unavailableHTML = renderToStaticMarkup(React.createElement(React.Fragment, null, unavailableLink));
  assert.match(unavailableHTML, /No disponible/);
  assert.doesNotMatch(unavailableHTML, /href=/);

  const browserOnly = renderInline('[report](/workass/artifacts/report-id/)', 'browser-only', true, {
    revision: 'browser-v1', resolve: () => null, openLink: () => false, open: () => {},
  }).find((node) => React.isValidElement(node) && node.type === 'a') as React.ReactElement<{ onClick?: (event: { preventDefault(): void }) => void }>;
  let browserOnlyPrevented = false;
  browserOnly.props.onClick?.({ preventDefault: () => { browserOnlyPrevented = true; } });
  assert.equal(browserOnlyPrevented, false);
});

test('artifact clicks navigate the exact remote chat repeatedly without changing the active local chat', async (t) => {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  t.after(() => server.close());
  const { Store } = await server.ssrLoadModule('/src/store/store.ts');
  const previousWindow = (globalThis as any).window;
  const calls: unknown[][] = [];
  const nativeBrowser = { supported: true, command: async (...args: unknown[]) => { calls.push(args); } };
  (globalThis as any).window = { location: { origin: 'http://127.0.0.1:8799' }, workassBrowser: nativeBrowser };
  t.after(() => {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  });
  const subject = new Store();
  const local = { id: 'tab-same', chatId: 'chat-same', pane: 'rail' };
  const remote = { id: 'M~san-laptop~tab-same', chatId: 'M~san-laptop~chat-same', machineId: 'san-laptop', pane: 'rail' };
  subject.state.chats = [local, remote];
  subject.state.activeId = local.id;
  subject.state.machines = [{ machineId: 'san-laptop', address: '192.0.2.10:8788', secure: true }];
  // Keep this test's state in memory; exercise the real open/navigation method.
  subject.bumpChat = () => {};
  const notices: unknown[][] = [];
  subject.addToast = (...args: unknown[]) => notices.push(args);
  subject.state.machines[0].link = 'ready';
  (globalThis as any).window.workassArtifacts = { supported: true };
  const origin = subject.browserArtifactOrigin(remote.machineId);
  assert.equal(origin, 'san-laptop');
  const artifact = '/workass/artifacts/report/';
  assert.equal(subject.openHostedArtifact(remote.id, artifact, origin), true);
  assert.equal(subject.openHostedArtifact(remote.id, artifact, origin), true);
  assert.deepEqual(calls, [
    [remote.id, 'navigate', 'http://127.0.0.1:8799/workass/connected-artifacts/san-laptop/report/'],
    [remote.id, 'navigate', 'http://127.0.0.1:8799/workass/connected-artifacts/san-laptop/report/'],
  ]);
  assert.equal(remote.pane, 'browser');
  assert.equal(local.pane, 'rail');
  assert.equal(subject.state.activeId, local.id);
  assert.equal(subject.openHostedArtifact(remote.id, artifact, ''), true);
  assert.equal(calls.length, 2);
  assert.equal(notices.length, 1);
  nativeBrowser.supported = false;
  assert.equal(subject.openHostedArtifact(remote.id, artifact, origin), false);
  assert.equal(calls.length, 2);
});
