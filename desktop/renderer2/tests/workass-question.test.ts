import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { mkdir, writeFile } from 'node:fs/promises';
import { readFileSync } from 'node:fs';
import { dirname } from 'node:path';
import { createServer, type ViteDevServer } from 'vite';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { encodeWorkassQuestionAnswer, limitWorkassQuestionText } from '../src/question-answer.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;
const fixtureStores: any[] = [];

function ownStore<T extends { clearToastTimers(): void }>(store: T): T {
  fixtureStores.push(store);
  return store;
}
let PermCard: (props: any) => React.ReactElement;
let rendererStore: any;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  const storeModule = await vite.ssrLoadModule('/src/store/store.ts');
  StoreCtor = storeModule.Store;
  rendererStore = storeModule.store;
  PermCard = (await vite.ssrLoadModule('/src/components/messages.tsx')).PermCard;
});

after(async () => {
  for (const store of fixtureStores) store.clearToastTimers();
  rendererStore.clearToastTimers();
  await vite.close();
});

test('question UI encodes selected ids and free text in the frozen optionId envelope', () => {
  const answer = {
    status: 'answered' as const,
    selectedOptionIds: ['production', 'canary'],
    freeText: 'Conservar este texto tal cual.  ',
  };
  const token = encodeWorkassQuestionAnswer(answer);
  assert.equal(token,
    'workass-question-v1:eyJzdGF0dXMiOiJhbnN3ZXJlZCIsInNlbGVjdGVkT3B0aW9uSWRzIjpbInByb2R1Y3Rpb24iLCJjYW5hcnkiXSwiZnJlZVRleHQiOiJDb25zZXJ2YXIgZXN0ZSB0ZXh0byB0YWwgY3VhbC4gICJ9');
  assert.match(token, /^workass-question-v1:[A-Za-z0-9_-]+$/);

  const payload = token.slice('workass-question-v1:'.length).replace(/-/g, '+').replace(/_/g, '/');
  const padded = payload + '='.repeat((4 - payload.length % 4) % 4);
  const binary = atob(padded);
  const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
  assert.deepEqual(JSON.parse(new TextDecoder().decode(bytes)), answer);
});

test('pending question takes the composer input slot while controls remain below it', () => {
  const composer = readFileSync(new URL('../src/components/Composer.tsx', import.meta.url), 'utf8');
  const dock = readFileSync(new URL('../src/components/QuestionDock.tsx', import.meta.url), 'utf8');
  assert.match(composer, /usePendingQuestion\(chat\)/);
  assert.match(composer, /<QuestionDock chatId=\{chat\.id\} pending=\{pendingQuestion\} \/>/);
  assert.match(composer, /<div className="comp" hidden=\{!!pendingQuestion\}>/);
  assert.match(dock, /export function findPendingQuestion/);
  assert.match(dock, /<PermCard key=\{pending\.perm\.id\} perm=\{pending\.perm\}/);
  assert.match(composer, /<div className="comprow">/);
});

test('the question dock renders grouped choices, free text, and an icon-only send control', () => {
  const html = renderToStaticMarkup(React.createElement(PermCard, {
    tabId: 'question-tab', msgId: 'question-message',
    perm: {
      id: 'question-request', title: 'Assistant question', kind: 'workass_question', resolved: null,
      options: [], question: {
        workassTool: true, questionId: 'deploy-target', operationId: 'ask-deploy-target',
        question: '¿Qué destino preparo?', header: 'Destino', multiSelect: true, allowFreeText: true,
        options: [{ id: 'canary', label: 'Canary', description: 'Prueba' }, { id: 'production', label: 'Producción', description: '' }],
      },
    },
  }));
  assert.match(html, /data-testid="workass-question-card"/);
  assert.match(html, /class="qdock"/);
  assert.match(html, /class="qchoice/);
  assert.match(html, /class="qchoice-end/);
  assert.match(html, /¿Qué destino preparo\?/);
  assert.match(html, /Canary/);
  assert.match(html, /Producción/);
  assert.match(html, /data-testid="workass-question-free-text"/);
  assert.match(html, /Respuesta adicional/);
  assert.match(html, /data-testid="workass-question-dismiss" aria-label="Descartar"/);
  assert.match(html, /data-testid="workass-question-submit" disabled="" aria-label="Enviar respuesta"/);
  assert.equal(html.includes('Enviar (0)'), false);
  assert.doesNotMatch(html, /run .*AskUserQuestion/);
});

test('single-choice questions answer by clicking a choice and keep optional text available', () => {
  const html = renderToStaticMarkup(React.createElement(PermCard, {
    tabId: 'question-tab', msgId: 'question-message',
    perm: {
      id: 'question-request', title: 'Assistant question', kind: 'workass_question', resolved: null,
      options: [], question: { workassTool: true, questionId: 'deploy-target', operationId: 'ask-deploy-target',
        question: '¿Deployamos?', header: 'Confirmar', multiSelect: false, allowFreeText: true,
        options: [{ id: 'yes', label: 'Yes', description: '' }, { id: 'no', label: 'No', description: '' }] },
    },
  }));
  assert.equal((html.match(/data-testid="workass-question-option"/g) ?? []).length, 2);
  assert.match(html, /data-testid="workass-question-submit" disabled=""/);
  assert.doesNotMatch(html, /aria-pressed="true"/);
  assert.match(html, /aria-keyshortcuts="1"/);
  assert.match(html, /class="qsend"/);
  assert.match(html, /Yes/);
  assert.match(html, /No/);
});

test('actual single-choice option callbacks preserve yes/no ids through the machine router and deliver once', async () => {
  const { createMachineRouter } = await import('../src/wire/machineRouter.ts');
  const machineId = 'question-owner';
  const calls: Array<{ channel: string; args: unknown[] }> = [];
  let failFirst = true;
  const router = createMachineRouter({
    local: () => ({} as never), links: () => new Map(),
    controlLinks: () => new Map([[machineId, {
      invoke: async (channel: string, ...args: unknown[]) => {
        calls.push({ channel, args });
        if (failFirst) { failFirst = false; return { ok: false }; }
        return { ok: true };
      },
      on: () => () => {},
    } as any]]),
  }) as any;
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = { api: { chatPermissionDecide: router.chatPermissionDecide } };

  const reactInternals = (React as any).__CLIENT_INTERNALS_DO_NOT_USE_OR_WARN_USERS_THEY_CANNOT_UPGRADE;
  const previousDispatcher = reactInternals.H;
  const hooks: any[] = [];
  let hookIndex = 0;
  reactInternals.H = {
    useState(initial: unknown) {
      const index = hookIndex++;
      if (!(index in hooks)) hooks[index] = initial;
      return [hooks[index], (value: unknown) => { hooks[index] = typeof value === 'function' ? value(hooks[index]) : value; }];
    },
  };
  const findOptions = (node: any, found: any[] = []): any[] => {
    if (!node) return found;
    if (Array.isArray(node)) { for (const child of node) findOptions(child, found); return found; }
    if (node.props?.['data-testid'] === 'workass-question-option') found.push(node);
    findOptions(node.props?.children, found);
    return found;
  };

  try {
    for (const selectedId of ['yes', 'no']) {
      const permission = {
        id: `M~${machineId}~request-${selectedId}`, title: 'Question', kind: 'workass_question', resolved: null,
        options: [], question: { workassTool: true, questionId: 'binary-choice', operationId: `binary-${selectedId}`,
          question: 'Continue?', multiSelect: false, allowFreeText: true,
          options: [{ id: 'yes', label: 'Yes' }, { id: 'no', label: 'No' }] },
      };
      const owner = { id: 'question-tab', chatId: 'question-chat', messages: [{
        id: 'question-message', role: 'assistant', content: '', status: 'running', at: null, events: [], permission,
      }] };
      rendererStore.state.chats = [owner];
      rendererStore.state.activeId = owner.id;
      rendererStore.bump = () => {};
      rendererStore.bumpChat = () => {};

      hooks.length = 0;
      hookIndex = 0;
      const tree = PermCard({ tabId: owner.id, msgId: 'question-message', perm: permission });
      const textarea = (tree.props.children as any[]).find((child: any) => child?.props?.['data-testid'] === 'workass-question-free-text');
      textarea.props.onChange({ target: { value: 'keep this optional detail' } });
      hookIndex = 0;
      const updatedTree = PermCard({ tabId: owner.id, msgId: 'question-message', perm: permission });
      const options = findOptions(updatedTree);
      const option = options[selectedId === 'yes' ? 0 : 1];
      assert.ok(option, `missing ${selectedId} option callback; got ${options.length}`);
      option.props.onClick();
      option.props.onClick();
      await new Promise((resolve) => setTimeout(resolve, 0));

      const expectedAfterFirstAttempt = selectedId === 'yes' ? 1 : 3;
      assert.equal(calls.length, expectedAfterFirstAttempt, 'a double click must deliver only once');
      if (selectedId === 'yes') {
        assert.equal(permission.resolved, undefined, 'a rejected first request should re-enable the card');
        option.props.onClick();
        await new Promise((resolve) => setTimeout(resolve, 0));
        assert.equal(calls.length, 2, 'the same callback should retry successfully after rejection');
      }
      const sent = calls.at(-1)!;
      assert.equal(sent.channel, 'chat:permission-decide');
      assert.equal((sent.args[0] as any).id, `request-${selectedId}`, 'the router must remove its machine tag from the request id');
      const payload = (sent.args[0] as any).optionId as string;
      assert.match(payload, /^workass-question-v1:/);
      const decoded = JSON.parse(Buffer.from(payload.slice('workass-question-v1:'.length), 'base64url').toString('utf8'));
      assert.deepEqual(decoded, { status: 'answered', selectedOptionIds: [selectedId], freeText: 'keep this optional detail' });
      if (selectedId === 'no' && process.env.WORKASS_QUESTION_ANSWER_TOKEN_FILE) {
        const tokenPath = process.env.WORKASS_QUESTION_ANSWER_TOKEN_FILE;
        await mkdir(dirname(tokenPath), { recursive: true });
        await writeFile(tokenPath, payload, { mode: 0o600 });
      }
    }
  } finally {
    reactInternals.H = previousDispatcher;
    rendererStore.state.chats = [];
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('native SDK questions keep their original one-click answer and skip behavior', () => {
  const html = renderToStaticMarkup(React.createElement(PermCard, {
    tabId: 'native-tab', msgId: 'native-message',
    perm: { id: 'native-request', title: 'AskUserQuestion', kind: 'permission', resolved: null,
      options: [{ optionId: 'yes', name: 'Yes', label: 'Yes', kind: 'answer' }, { optionId: 'skip', name: 'Skip', label: 'Skip', kind: 'cancel' }],
      question: { question: 'Continue?', options: [{ label: 'Yes', description: '' }] } },
  }));
  assert.match(html, /Continue\?/);
  assert.match(html, /Yes/);
  assert.match(html, /Skip/);
  assert.doesNotMatch(html, /Enviar respuesta/);
});

test('Unicode free text uses the shared code-point bound rather than UTF-16 length', () => {
  const unicode = '界🙂'.repeat(600);
  const limited = limitWorkassQuestionText(unicode);
  assert.equal(Array.from(limited).length, 1000);
  assert.equal(limited, unicode.slice(0, limited.length));
  assert.ok(limited.length > 1000, 'the full 1000-code-point answer should allow astral characters');
});

test('a rejected question answer unlocks the same card for an idempotent retry', async () => {
  const previousWindow = (globalThis as any).window;
  const calls: Array<[string, string]> = [];
  let attempts = 0;
  try {
    (globalThis as any).window = { api: {
      chatPermissionDecide: async (permissionId: string, optionId: string) => {
        calls.push([permissionId, optionId]);
        attempts++;
        if (attempts === 1) throw new Error('controller reconnected before the first reply was acknowledged');
        return { ok: true };
      },
    } };
    const store = ownStore(new StoreCtor());
    const question = {
      id: 'question-request', title: 'Assistant question', kind: 'workass_question',
      options: [], resolved: undefined,
      question: { workassTool: true, questionId: 'deploy-target', operationId: 'ask-deploy-target',
        question: '¿Qué destino preparo?', header: 'Destino', options: [{ id: 'canary', label: 'Canary', description: '' }],
        multiSelect: false, allowFreeText: true },
    };
    const owner = {
      id: 'question-tab', chatId: 'question-chat', messages: [{
        id: 'question-message', role: 'assistant', content: '', status: 'running', at: null, events: [], permission: question,
      }],
    };
    store.state.chats = [owner];
    store.state.activeId = owner.id;
    store.bump = () => {};
    const toasts: string[] = [];
    store.addToast = (title: string) => { toasts.push(title); };

    const answer = encodeWorkassQuestionAnswer({
      status: 'answered', selectedOptionIds: ['canary'], freeText: 'Mantener este detalle 🙂',
    });
    await store.decidePermission(owner.id, 'question-message', question.id, answer);
    assert.equal(owner.messages[0].permission.resolved, undefined, 'a rejected reply left the card disabled');
    assert.deepEqual(toasts, ['No se envió la respuesta']);

    await store.decidePermission(owner.id, 'question-message', question.id, answer);
    assert.equal(owner.messages[0].permission, undefined, 'accepted retry did not close its exact card');
    assert.deepEqual(calls, [['question-request', answer], ['question-request', answer]], 'retry must send the identical structured answer');
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
