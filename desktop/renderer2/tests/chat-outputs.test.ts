import assert from 'node:assert/strict';
import test from 'node:test';
import type { Msg, ToolEvent } from '../src/store/types.ts';
import { chatOutputs, OUTPUT_TAG } from '../src/chat-outputs.ts';

const RESULT = '{"htmlRender":{"id":"cobertura-por-paquete-3f9a2b1c4d5e","title":"Cobertura por paquete","urlPath":"/workass/artifacts/cobertura-por-paquete-3f9a2b1c4d5e/"},"message":"Shown to the reader."}\n';
const tool = (over: Partial<ToolEvent>): ToolEvent => ({
  key: over.id ?? 'tool', at: 0, kind: 'tool', id: 'tool', toolKind: 'execute', title: 'Bash', status: 'completed',
  command: null, terminalId: null, input: null, output: null, location: null, ...over,
});
const reply = (id: string, over: Partial<Msg> = {}): Msg => ({ id, role: 'assistant', content: '', status: 'done', at: null, events: [], ...over });

test('Salidas lists published pages, reply and tool images and hosted files, newest first', () => {
  const png = { mimeType: 'image/png', data: 'AAAA', name: 'Captura del panel' };
  const messages: Msg[] = [
    reply('a1', { events: [tool({ id: 'render', command: '"$WORKASS_TOOLS_COMMAND" tools call workass_render_html', output: RESULT })] }),
    { id: 'u1', role: 'user', content: 'mirá [esto](/workass/artifacts/user-link/)', status: 'done', at: null, events: [] },
    reply('a2', { images: [png], content: 'Te dejo el informe: [**informe.pdf**](/workass/artifacts/informe-1/informe.pdf) y la [página](/workass/artifacts/cobertura-por-paquete-3f9a2b1c4d5e/).' }),
    reply('a3', { events: [tool({ id: 'shot', toolKind: 'read', title: 'Read', images: [{ mimeType: 'image/png', data: 'BBBB' }] })] }),
  ];
  const outputs = chatOutputs(messages);
  assert.deepEqual(outputs.map((output) => [OUTPUT_TAG[output.kind], output.title]), [
    ['IMAGEN', 'Read'],
    ['ARCHIVO', 'informe.pdf'],
    ['IMAGEN', 'Captura del panel'],
    ['PÁGINA', 'Cobertura por paquete'],
  ]);
  assert.equal(outputs.find((output) => output.kind === 'page')?.target, '/workass/artifacts/cobertura-por-paquete-3f9a2b1c4d5e/');
  assert.equal(outputs.find((output) => output.kind === 'file')?.target, '/workass/artifacts/informe-1/informe.pdf');
});

test('a subagent call, an unfinished call, a fenced link and a repeat add nothing', () => {
  const messages: Msg[] = [
    reply('a1', { events: [
      tool({ id: 'sub', subagentId: 'agent-1', command: 'tools call workass_render_html', output: RESULT }),
      tool({ id: 'live', status: 'in_progress', command: 'tools call workass_render_html', output: RESULT }),
    ] }),
    reply('a2', { content: '```md\n[ejemplo](/workass/artifacts/no-es-real/)\n```\n[datos.csv](/workass/artifacts/datos/datos.csv)' }),
    reply('a3', { content: 'Otra vez: [datos.csv](/workass/artifacts/datos/datos.csv)' }),
  ];
  assert.deepEqual(chatOutputs(messages).map((output) => output.title), ['datos.csv']);
  assert.deepEqual(chatOutputs([]), []);
  assert.deepEqual(chatOutputs(undefined), []);
});

test('a live reply is read again when its call completes', () => {
  const call = tool({ id: 'render', status: 'in_progress', command: 'tools call workass_render_html' });
  const live = reply('a1', { status: 'running', events: [call] });
  assert.equal(chatOutputs([live]).length, 0);
  call.status = 'completed';
  call.output = RESULT;
  assert.deepEqual(chatOutputs([live]).map((output) => output.kind), ['page']);
});
