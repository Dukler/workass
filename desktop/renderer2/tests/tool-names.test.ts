import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { ToolEvent } from '../src/store/types.ts';
import { spawnedWorkActivity, spawnedWorkKindWord, toolPresentation } from '../src/tool-names.ts';

function tool(partial: Partial<ToolEvent> & Pick<ToolEvent, 'title'>): ToolEvent {
  return {
    kind: 'tool', key: 'k', toolKind: 'execute', id: null, status: 'completed',
    command: null, location: null, input: null, output: null, terminalId: null,
    ...partial,
  } as ToolEvent;
}

test('Claude tool ids become action labels with the matching glyph', () => {
  const cases: Array<[string, string, string, string]> = [
    // title, toolKind, label, icon
    ['Bash', 'execute', 'Ejecutar un comando', 'run'],
    ['Read', 'read', 'Leer un archivo', 'read'],
    ['Edit', 'edit', 'Editar un archivo', 'edit'],
    ['Write', 'edit', 'Escribir un archivo', 'write'],
    ['Grep', 'search', 'Buscar en el código', 'search'],
    ['Web search', 'search', 'Buscar en la web', 'web'],
    ['WebFetch', 'fetch', 'Abrir una página web', 'fetch'],
    ['TodoWrite', 'other', 'Actualizar el plan', 'plan'],
    ['ToolSearch', 'read', 'Buscar herramientas', 'search'],
  ];
  for (const [title, toolKind, label, icon] of cases) {
    const act = toolPresentation(tool({ title, toolKind }));
    assert.equal(act.label, label, title);
    assert.equal(act.icon, icon, title);
    assert.equal(act.raw, title);
    assert.equal(act.evidence, undefined, title);
  }
});

test('Codex phrase titles and raw command titles both read as actions', () => {
  const patch = toolPresentation(tool({ title: 'Editing files', toolKind: 'edit' }));
  assert.equal(patch.label, 'Editar archivos');
  assert.equal(patch.icon, 'edit');
  assert.equal(patch.evidence, undefined);

  // Codex titles an execute call with the whole command line: the row shows the
  // action and hands the command to the detail column instead of eating the line.
  const cmd = '/bin/zsh -lc "ssh lagpc-vpn \'powershell -NoProfile -Command …\'"';
  const ran = toolPresentation(tool({ title: cmd, toolKind: 'execute' }));
  assert.equal(ran.label, 'Ejecutar un comando');
  assert.equal(ran.icon, 'run');
  assert.equal(ran.evidence, cmd);
  assert.equal(ran.raw, cmd);

  const path = '/Users/dev/Workspace/workass/internal/acp/bridge.go';
  const readRow = toolPresentation(tool({ title: path, toolKind: 'read' }));
  assert.equal(readRow.label, 'Leer un archivo');
  assert.equal(readRow.evidence, path);
});

test('Workass MCP tools are named; other servers are humanized from their id', () => {
  const ours = toolPresentation(tool({ title: 'mcp__workass-agent__workass_cancel_subagent', toolKind: 'other' }));
  assert.equal(ours.label, 'Cancelar un subagente');
  assert.equal(ours.tag, 'workass');
  assert.equal(ours.icon, 'agent');

  const browser = toolPresentation(tool({ title: 'mcp__workass-browser__workass_browser_screenshot', toolKind: 'other' }));
  assert.equal(browser.label, 'Capturar el navegador');
  assert.equal(browser.tag, 'workass');

  const third = toolPresentation(tool({ title: 'mcp__linear__create_issue', toolKind: 'other' }));
  assert.equal(third.label, 'Create issue');
  assert.equal(third.tag, 'linear');
  assert.equal(third.icon, 'mcp');
});

test('unknown kinds keep identifying text instead of flattening to «herramienta»', () => {
  const unknown = toolPresentation(tool({ title: 'SomeNewTool', toolKind: 'other' }));
  assert.equal(unknown.label, 'Some new tool');
  assert.equal(unknown.icon, 'tool');
  assert.equal(unknown.raw, 'SomeNewTool');

  // A plain phrase is a NAME, not evidence: demoting it to the detail column
  // would leave the row with no identity (regression, failure-cues.test.ts).
  const phrase = toolPresentation(tool({ title: 'Call hidden tool', toolKind: 'other' }));
  assert.equal(phrase.label, 'Call hidden tool');
  assert.equal(phrase.evidence, undefined);

  // No kind and no name match: the title is still the only evidence there is.
  const bare = toolPresentation(tool({ title: '', toolKind: '' }));
  assert.equal(bare.label, 'Usar una herramienta');
  assert.equal(bare.icon, 'tool');
});

test('history predating toolKind still classifies from the title', () => {
  const legacy = toolPresentation(tool({ title: 'git status --short', toolKind: '' }));
  assert.equal(legacy.label, 'Ejecutar un comando');
  assert.equal(legacy.icon, 'run');
  assert.equal(legacy.evidence, 'git status --short');
});

// ── The rail's running-work rows (approved mock rail-actions, 2026-07-27) ─────
// The daemon mirrors two different vocabularies into the same two fields, and
// only what we can classify earns a glyph.

test('a subagent in a tool call is named by the action vocabulary', () => {
  const claude = spawnedWorkActivity({ lastToolName: 'tool', summary: 'Read' });
  assert.equal(claude?.label, 'Leer un archivo');
  assert.equal(claude?.icon, 'read');
  assert.equal(claude?.unclassified, undefined);

  // Codex sends the whole command line as the title: it becomes the evidence,
  // not the name, exactly as in the transcript.
  const codex = spawnedWorkActivity({ lastToolName: 'tool', summary: 'go test ./internal/acp -run Subagent' });
  assert.equal(codex?.label, 'Ejecutar un comando');
  assert.equal(codex?.evidence, 'go test ./internal/acp -run Subagent');

  // A Workass MCP call keeps its origin tag.
  const mcp = spawnedWorkActivity({ lastToolName: 'tool', summary: 'mcp__workass-agent__workass_list_subagents' });
  assert.equal(mcp?.label, 'Ver los subagentes');
  assert.equal(mcp?.tag, 'workass');
});

test('daemon lifecycle phases get their own word and mark, never English', () => {
  const cases: Array<[string, string, string]> = [
    ['starting', 'Arrancando', 'proc'],
    ['initializing', 'Abriendo la sesión', 'proc'],
    ['configuring', 'Ajustando controles', 'config'],
    ['working', 'Trabajando', 'run'],
    ['thinking', 'Pensando', 'think'],
    ['planning', 'Actualizando el plan', 'plan'],
    ['waiting_permission', 'Esperando permiso', 'shield'],
    ['orphaned', 'Sin proceso', 'agent'],
  ];
  for (const [phase, label, icon] of cases) {
    const act = spawnedWorkActivity({ lastToolName: phase, summary: 'Running delegated task' });
    assert.equal(act?.label, label, phase);
    assert.equal(act?.icon, icon, phase);
  }
  // a blocked permission keeps the exact action it is waiting on
  const blocked = spawnedWorkActivity({ lastToolName: 'waiting_permission', summary: 'Permission required: rm -rf build' });
  assert.equal(blocked?.evidence, 'rm -rf build');
});

test('native Task progress sends a real tool id as the phase', () => {
  const act = spawnedWorkActivity({ lastToolName: 'Grep', summary: '' });
  assert.equal(act?.label, 'Buscar en el código');
  assert.equal(act?.icon, 'search');
});

test('prose we did not classify is passed through verbatim and earns no glyph', () => {
  const prose = spawnedWorkActivity({ lastToolName: '', summary: 'Reempaquetando la app y esperando el gate.' });
  assert.equal(prose?.label, 'Reempaquetando la app y esperando el gate.');
  assert.equal(prose?.unclassified, true);

  assert.equal(spawnedWorkActivity({ lastToolName: '', summary: '' }), null);
});

test('the work kind reads as a word, and an unknown kind is left alone', () => {
  assert.equal(spawnedWorkKindWord('subagent'), 'subagente');
  assert.equal(spawnedWorkKindWord('bash'), 'comando');
  assert.equal(spawnedWorkKindWord('workflow'), 'flujo');
  assert.equal(spawnedWorkKindWord('quantum-lane'), 'quantum-lane');
});
