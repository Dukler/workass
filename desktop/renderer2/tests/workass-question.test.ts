import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { createServer, type ViteDevServer } from 'vite';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { encodeWorkassQuestionAnswer, limitWorkassQuestionText } from '../src/question-answer.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;
let PermCard: (props: any) => React.ReactElement;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
  PermCard = (await vite.ssrLoadModule('/src/components/messages.tsx')).PermCard;
});

after(async () => { await vite.close(); });

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

test('the real Workass card renders structured options, free text, and localized actions', () => {
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
  assert.match(html, /¿Qué destino preparo\?/);
  assert.match(html, /Canary/);
  assert.match(html, /Producción/);
  assert.match(html, /data-testid="workass-question-free-text"/);
  assert.match(html, /Respuesta adicional/);
  assert.match(html, /Descartar/);
  assert.match(html, /Enviar respuesta/);
  assert.doesNotMatch(html, /run .*AskUserQuestion/);
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
    const store = new StoreCtor();
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
