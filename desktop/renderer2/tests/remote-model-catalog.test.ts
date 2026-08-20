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
  const lagpc = chat('m-lagpc', 'lagpc-two');
  const san = chat('m-san', 'san-one');
  appStore.state.chats = [local, lagpc, san];
  appStore.state.activeId = lagpc.id;
  appStore.state.connection = 'connected';

  const localGroup = group('Local Agent', 'local-brand', 'local-one');
  const lagpcGroup = group('LagPC Agent', 'lagpc-brand', 'lagpc-one', 'lagpc-two');
  const sanGroup = group('San Agent', 'san-brand', 'san-one');
  appStore.onCatalog({ groups: [localGroup], models: [], modes: [] });
  appStore.onCatalog(projectRemoteEvent('onChatCatalog', 'm-lagpc', {
    groups: [lagpcGroup], models: [], modes: [],
  }) as ChatCatalog);
  appStore.onCatalog(projectRemoteEvent('onChatCatalog', 'm-san', {
    groups: [sanGroup], models: [], modes: [],
  }) as ChatCatalog);

  assert.deepEqual(appStore.catalogGroupsForChat(local), [localGroup]);
  assert.deepEqual(appStore.catalogGroupsForChat(lagpc), [lagpcGroup]);
  assert.deepEqual(appStore.catalogGroupsForChat(san), [sanGroup]);
  assert.equal(appStore.providerName('shared', lagpc), 'LagPC Agent');
  assert.equal(appStore.providerBrand('shared', lagpc), 'lagpc-brand');

  const localMarkup = renderToStaticMarkup(React.createElement(Composer, { chat: local }));
  const lagpcMarkup = renderToStaticMarkup(React.createElement(Composer, { chat: lagpc }));
  const sanMarkup = renderToStaticMarkup(React.createElement(Composer, { chat: san }));
  assert.match(localMarkup, />LOCAL-ONE<\/button>/);
  assert.match(lagpcMarkup, />LAGPC-TWO<\/button>/);
  assert.match(sanMarkup, />SAN-ONE<\/button>/);
  assert.doesNotMatch(lagpcMarkup, /LOCAL-ONE|SAN-ONE/);
});

