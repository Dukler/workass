import assert from 'node:assert/strict';
import test from 'node:test';
import { fmtTokens, mcpEmptyLine, mcpServerDetail, mcpStatusLabel, projectName } from '../src/components/settings/herramientas.ts';

test('token counts read as estimates, with a decimal comma above a thousand', () => {
  assert.equal(fmtTokens(0), '≈ 0');
  assert.equal(fmtTokens(49), '≈ 49');
  assert.equal(fmtTokens(263), '≈ 260');
  assert.equal(fmtTokens(608), '≈ 610');
  assert.equal(fmtTokens(996), '≈ 1,0 k');
  assert.equal(fmtTokens(1859), '≈ 1,9 k');
  assert.equal(fmtTokens(2795), '≈ 2,8 k');
  assert.equal(fmtTokens(Number.NaN), '≈ 0');
});

test('an MCP server says where it is configured, by the sidebar name of its project', () => {
  const workspaces = [{ path: '/w/proyecto', name: 'Mi proyecto' }];
  assert.equal(mcpServerDetail({ name: 'figma', scope: 'user', file: '~/.config.json' }, workspaces), 'Usuario · ~/.config.json');
  assert.equal(mcpServerDetail({ name: 'db', scope: 'project', project: '/w/proyecto', file: '/w/proyecto/.mcp.json' }, workspaces), 'Proyecto Mi proyecto · .mcp.json');
  assert.equal(mcpServerDetail({ name: 'db', scope: 'local', project: '/w/otro/', file: '~/.config.json' }, workspaces), 'Solo vos, en otro · ~/.config.json');
  assert.equal(projectName('/w/proyecto/', workspaces), 'Mi proyecto');
});

test('status shows only what the config files say', () => {
  assert.deepEqual(mcpStatusLabel('off'), { text: 'Apagado', tone: '' });
  assert.deepEqual(mcpStatusLabel('pending'), { text: 'Sin aprobar', tone: 'warn' });
  assert.equal(mcpStatusLabel(''), null);
  assert.equal(mcpStatusLabel(undefined), null);
});

test('no servers names the agents that were read', () => {
  assert.equal(mcpEmptyLine(['Agente A']), 'Agente A no tiene servidores MCP configurados.');
  assert.equal(mcpEmptyLine(['Agente A', 'Agente B']), 'Agente A y Agente B no tienen servidores MCP configurados.');
  assert.equal(mcpEmptyLine(['A', 'B', 'C']), 'A, B y C no tienen servidores MCP configurados.');
  assert.equal(mcpEmptyLine([]), 'Ningún agente tiene servidores MCP configurados.');
});
