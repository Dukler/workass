import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat } from '../src/store/types.ts';
import type { CatalogGroup, ChatCatalog } from '../src/wire/types.ts';

let vite: ViteDevServer;
let Composer: React.ComponentType<{ chat: Chat | null }>;
let appStore: {
  state: { chats: Chat[]; activeId: string | null; groups: CatalogGroup[]; connection: string };
  catalogGroupsForChat(chat: Chat | string): CatalogGroup[];
  providerName(providerId: string, chat: Chat): string | null;
  providerBrand(providerId: string, chat: Chat): string;
  onCatalog(catalog: ChatCatalog): void;
};
let projectRemoteEvent: (method: string, machineId: string, payload: unknown) => unknown;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  ({ Composer } = await vite.ssrLoadModule('/src/components/Composer.tsx') as {
    Composer: React.ComponentType<{ chat: Chat | null }>;
  });
  appStore = (await vite.ssrLoadModule('/src/store/store.ts') as { store: typeof appStore }).store;
  ({ projectRemoteEvent } = await vite.ssrLoadModule('/src/wire/machineRouter.ts') as {
    projectRemoteEvent: typeof projectRemoteEvent;
  });
});

after(async () => { await vite.close(); });

function chat(machineId: string, modelId: string): Chat {
  const prefix = machineId ? `M~${machineId}~` : '';
  return {
    id: `${prefix}tab`, chatId: `${prefix}chat`, machineId: machineId || undefined,
    sessionId: `${prefix}session`, sessionProviderId: 'shared',
    title: machineId || 'Local', titleLocked: true, group: null, cwd: null,
    providerId: 'shared', providerName: null,
    currentModelId: modelId, currentModeId: 'ask', pending: false,
    messages: [], draft: '',
  } as Chat;
}

function group(name: string, brand: string, ...models: string[]): CatalogGroup {
  return {
    providerId: 'shared', providerName: name, assistantBrand: brand,
    models: models.map((modelId) => ({ modelId, name: modelId.toUpperCase() })),
    modes: [{ id: 'ask', name: 'Ask' }],
  };
}

test('local and remote chats render and validate only their owning machine catalog', () => {
  const local = chat('', 'local-one');
  const desktoppc = chat('m-desktoppc', 'desktoppc-two');
  const san = chat('m-work', 'san-one');
  appStore.state.chats = [local, desktoppc, san];
  appStore.state.activeId = desktoppc.id;
  appStore.state.connection = 'connected';

  const localGroup = group('Local Agent', 'local-brand', 'local-one');
  const desktoppcGroup = group('DesktopPC Agent', 'desktoppc-brand', 'desktoppc-one', 'desktoppc-two');
  const sanGroup = group('San Agent', 'san-brand', 'san-one');
  appStore.onCatalog({ groups: [localGroup], models: [], modes: [] });
  appStore.onCatalog(projectRemoteEvent('onChatCatalog', 'm-desktoppc', {
    groups: [desktoppcGroup], models: [], modes: [],
  }) as ChatCatalog);
  appStore.onCatalog(projectRemoteEvent('onChatCatalog', 'm-work', {
    groups: [sanGroup], models: [], modes: [],
  }) as ChatCatalog);

  assert.deepEqual(appStore.catalogGroupsForChat(local), [localGroup]);
  assert.deepEqual(appStore.catalogGroupsForChat(desktoppc), [desktoppcGroup]);
  assert.deepEqual(appStore.catalogGroupsForChat(san), [sanGroup]);
  assert.equal(appStore.providerName('shared', desktoppc), 'DesktopPC Agent');
  assert.equal(appStore.providerBrand('shared', desktoppc), 'desktoppc-brand');

  const localMarkup = renderToStaticMarkup(React.createElement(Composer, { chat: local }));
  const desktoppcMarkup = renderToStaticMarkup(React.createElement(Composer, { chat: desktoppc }));
  const sanMarkup = renderToStaticMarkup(React.createElement(Composer, { chat: san }));
  assert.match(localMarkup, />LOCAL-ONE<\/button>/);
  assert.match(desktoppcMarkup, />DESKTOPPC-TWO<\/button>/);
  assert.match(sanMarkup, />SAN-ONE<\/button>/);
  assert.doesNotMatch(desktoppcMarkup, /LOCAL-ONE|SAN-ONE/);
});

test('remote composer exposes a saved model rejected by the live host instead of displaying its default', () => {
  const san = chat('m-work', 'live-default');
  san.configuredModelId = 'retired-model[high]';
  appStore.onCatalog(projectRemoteEvent('onChatCatalog', 'm-work', {
    groups: [{ ...group('San Agent', 'san-brand', 'live-default'), status: 'ready' }],
    models: [], modes: [],
  }) as ChatCatalog);
  const markup = renderToStaticMarkup(React.createElement(Composer, { chat: san }));
  assert.match(markup, /Modelo no disponible/);
  assert.doesNotMatch(markup, />LIVE-DEFAULT<\/button>/);
});
