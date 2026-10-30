import assert from 'node:assert/strict';
import test from 'node:test';
import { noticeWord } from '../src/notice-word.ts';
import { chatToMarkdown } from '../src/transcriptExport.ts';
import type { Chat } from '../src/store/types.ts';

// The fleet runtime's own rule, on the one surface a person reads it on:
// home_down means UNKNOWN, not dead, and a watcher must never convert it into
// an outcome (docs/specs/fleet-runtime.md §7). The notice line had two answers
// — "terminó" and "falló" — so a dark machine's agent would have been shown as
// finished, which is that conversion.

test('a notice about an unreachable machine is never shown as finished', () => {
  assert.equal(noticeWord('home_down'), 'sin contacto');
  assert.notEqual(noticeWord('home_down'), 'terminó');
  assert.notEqual(noticeWord('home_down'), 'falló');
});

test('the settle and failure words are unchanged', () => {
  assert.equal(noticeWord('done'), 'terminó');
  assert.equal(noticeWord('failed'), 'falló');
  assert.equal(noticeWord(undefined), 'terminó');
  assert.equal(noticeWord(''), 'terminó');
});

test('an attention says a decision is pending, not that the work ended', () => {
  assert.equal(noticeWord('attention'), 'espera una decisión');
});

test('an exported transcript carries the same word the line shows', () => {
  const chat = {
    id: 'c1', title: 'Fleet', messages: [
      { id: 'm1', role: 'user', content: '<task-notification>…</task-notification>', status: 'done',
        notice: { kind: 'subagent', title: 'remote reviewer', status: 'home_down' }, events: [] },
    ],
  } as unknown as Chat;
  const markdown = chatToMarkdown(chat);
  assert.match(markdown, /remote reviewer sin contacto/);
  assert.doesNotMatch(markdown, /remote reviewer terminó/);
});
