import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const settings = readFileSync(new URL('../src/components/Settings.tsx', import.meta.url), 'utf8');
const commandBar = readFileSync(new URL('../src/components/CommandBar.tsx', import.meta.url), 'utf8');

test('the pairing UI exposes one discovery-and-approval flow', () => {
  const start = settings.indexOf('function MaquinasPanel()');
  const end = settings.indexOf('function MachineRow', start);
  assert.ok(start >= 0 && end > start, 'machine settings panel exists');
  const panel = settings.slice(start, end);

  assert.match(panel, /Workass descubiertas en esta red/);
  assert.match(panel, /incoming\.map\(\(r\) => <RequestRow/);
  assert.match(panel, /Sin direcciones, PIN ni claves/);
  assert.doesNotMatch(panel, /addMachine|setFleetKey|FleetKeyOfThisMachine|192\.168\.|type="password"/);
});

test('an unpaired discovered machine has one action and cannot be deleted accidentally', () => {
  const start = settings.indexOf('function MachineRow');
  const end = settings.indexOf('// ---- shell', start);
  const row = settings.slice(start, end);

  assert.match(row, /Solicitar conexión/);
  assert.match(row, /m\.paired && <button/);
  assert.match(row, />Desconectar<\/button>/);
  assert.doesNotMatch(row, />Quitar<\/button>/);
});

test('command search names the same two concepts as Settings', () => {
  assert.match(commandBar, /title: 'Conectar Workass'/);
  assert.match(commandBar, /title: 'Acceso aprobado'/);
  assert.doesNotMatch(commandBar, /Agregar una máquina por dirección|clave de la flota/);
});

test('approved access is described as client access, not another pairing method', () => {
  const start = settings.indexOf('function DispositivosPanel()');
  const end = settings.indexOf('// ---- 4 · Engines', start);
  const panel = settings.slice(start, end);

  assert.match(panel, /Cada cliente aprobado recibe su propio acceso/);
  assert.doesNotMatch(panel, /allow-list de IPs|modo espejo/);
});
