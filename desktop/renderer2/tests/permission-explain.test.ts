import assert from 'node:assert/strict';
import test from 'node:test';

import { allowsEverything, commandPreview, explainPermission, type WhyPart } from '../src/permission-explain.ts';

const text = (why: WhyPart[]) => why.map((part) => (typeof part === 'string' ? part : `\`${part.code}\``)).join('');

// The exact reason Claude Code sent in the DepthVision chat (2026-10-24).
const DEPTHVISION_REASON = 'Dangerous rm operation on possibly-empty variable path: $V/*.png in `rm -f $V/*.png` (rewrite it as "${V:?}"/*.png or use a literal path)';

test('rm on a possibly-empty variable says what it would delete from the root', () => {
  const explained = explainPermission({
    title: 'Bash', kind: 'execute', modeId: 'bypassPermissions',
    context: { tool: 'Bash', reason: DEPTHVISION_REASON, command: 'V=$D/verify; rm -f $V/*.png' },
  });
  assert.equal(explained.tone, 'safety');
  assert.equal(explained.title, 'Confirmá este borrado');
  assert.equal(text(explained.why),
    'El comando borra `$V/*.png`. Si `$V` quedara vacía, borraría `/*.png` desde la raíz del disco. ' +
    'Claude Code no puede comprobar que tenga un valor, así que pregunta aunque este chat tenga todos los permisos.');
  assert.equal(explained.providerReason, undefined);
  assert.deepEqual(explained.subject, { kind: 'command', text: 'V=$D/verify; rm -f $V/*.png' });
});

test('outside a full-access chat the same check does not claim the chat allows everything', () => {
  const explained = explainPermission({ title: 'Bash', kind: 'execute', modeId: 'default', context: { reason: DEPTHVISION_REASON } });
  assert.match(text(explained.why), /así que pide confirmación\.$/);
});

test('a braced variable and a relative remainder still read naturally', () => {
  const braced = explainPermission({ title: 'Bash', kind: null, context: { reason: 'Dangerous rm operation on possibly-empty variable path: ${OUT}/cache in `rm -rf ${OUT}/cache`' } });
  assert.match(text(braced.why), /Si `\$\{OUT\}` quedara vacía, borraría `\/cache` desde la raíz del disco/);
  const relative = explainPermission({ title: 'Bash', kind: null, context: { reason: 'Dangerous rm operation on possibly-empty variable path: $NAME.bak in `rm $NAME.bak`' } });
  assert.match(text(relative.why), /borraría otra cosa que la que se quiere/);
});

test('rm aimed at a system path names it', () => {
  const explained = explainPermission({ title: 'Bash', kind: 'execute', context: { reason: "Dangerous rm operation detected: '/usr'" } });
  assert.equal(text(explained.why), 'El comando borra `/usr`, una carpeta clave del sistema o tu carpeta personal. Claude Code siempre pide confirmación para esto.');
  assert.equal(explainPermission({ title: 'Bash', kind: null, context: { reason: 'Removal targets a protected system path' } }).title, 'Confirmá este borrado');
});

test('an rm too tangled to check asks for a review', () => {
  const explained = explainPermission({
    title: 'Bash', kind: 'execute', modeId: 'bypassPermissions',
    context: { reason: 'This command could not be fully checked for dangerous rm operations: not all of its nested commands could be analyzed.' },
  });
  assert.equal(explained.title, 'Confirmá este borrado');
  assert.match(text(explained.why), /demasiado complejo para que Claude Code verifique qué borra/);
});

test('a sensitive file and a path outside the folder are named', () => {
  const sensitive = explainPermission({ title: 'Edit', kind: 'edit', context: { tool: 'Edit', path: '/repo/.git/config', reason: 'Claude requested permissions to edit /repo/.git/config which is a sensitive file.' } });
  assert.equal(sensitive.title, 'Confirmá esta edición');
  assert.match(text(sensitive.why), /^Va a modificar `\/repo\/.git\/config`, un archivo sensible/);
  const outside = explainPermission({ title: 'Read', kind: 'read', context: { tool: 'Read', blockedPath: '/etc/hosts' } });
  assert.equal(outside.title, 'Confirmá esta lectura');
  assert.equal(text(outside.why), 'Va a usar `/etc/hosts`, que está fuera de la carpeta de trabajo de este chat, así que pide confirmación.');
});

test('an unknown reason gets a Spanish lead and keeps Claude Code words', () => {
  const explained = explainPermission({ title: 'Bash', kind: 'execute', modeId: 'bypassPermissions', context: { reason: 'Some brand new check' } });
  assert.equal(explained.tone, 'safety');
  assert.match(text(explained.why), /^Claude Code frenó esta acción por seguridad/);
  assert.equal(explained.providerReason, 'Some brand new check');
});

test('no reason: the chat mode explains it, per kind of action', () => {
  assert.equal(text(explainPermission({ title: 'Bash', kind: 'execute', modeId: 'default' }).why), 'Este chat pide confirmación antes de ejecutar comandos.');
  assert.equal(text(explainPermission({ title: 'Write', kind: 'edit', modeId: 'default' }).why), 'Este chat pide confirmación antes de modificar archivos.');
  assert.equal(text(explainPermission({ title: 'mcp__atlassian__get_issue', kind: 'other', modeId: 'default' }).why), 'Este chat pide confirmación antes de usar herramientas externas (atlassian).');
  assert.equal(explainPermission({ title: 'ExitPlanMode', kind: 'other' }).title, 'Aprobá el plan');
  assert.match(text(explainPermission({ title: 'Bash', kind: 'execute', modeId: 'bypassPermissions' }).why), /no dijo por qué/);
});

test('an old daemon without context still gets a sentence, from the title', () => {
  const explained = explainPermission({ title: 'Run fixture command', kind: 'execute', modeId: null });
  assert.equal(explained.title, 'Confirmá este comando');
  assert.equal(text(explained.why), 'Este chat pide confirmación antes de ejecutar comandos.');
});

test('full-access mode ids across providers', () => {
  assert.equal(allowsEverything('bypassPermissions'), true);
  assert.equal(allowsEverything('agent-full-access'), true);
  assert.equal(allowsEverything('default'), false);
  assert.equal(allowsEverything(null), false);
});

test('the collapsed command shows its first real line and counts the rest', () => {
  assert.deepEqual(commandPreview('\n  cd /repo\n  rm -f $V/*.png\n'), { line: 'cd /repo', more: 1 });
  const long = commandPreview('x'.repeat(200));
  assert.equal(long.line.length, 96);
  assert.ok(long.line.endsWith('…'));
});

test('the collapsed command shows the line with the flagged value, kept in view', () => {
  assert.deepEqual(commandPreview('cd /repo\nV=$D/v; rm -f $V/*.png\nls', '$V/*.png'), { line: 'V=$D/v; rm -f $V/*.png', more: 2 });
  const long = commandPreview('a'.repeat(150) + ' rm -f $V/*.png ' + 'b'.repeat(60), '$V/*.png');
  assert.ok(long.line.includes('$V/*.png'), long.line);
  assert.ok(long.line.length <= 96, String(long.line.length));
  assert.ok(long.line.startsWith('…'));
});

test('the DepthVision explanation points the preview at the rm', () => {
  assert.equal(explainPermission({ title: 'Bash', kind: null, context: { reason: DEPTHVISION_REASON } }).focus, '$V/*.png');
});
