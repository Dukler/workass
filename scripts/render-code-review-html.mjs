#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const rootDir = path.resolve(__dirname, '..');
const inputPath = process.argv[2];
const outputPath = process.argv[3];

if (!inputPath || !outputPath) {
  console.error('Usage: node scripts/render-code-review-html.mjs review-data.json review.html');
  process.exit(2);
}

const template = fs.readFileSync(path.join(rootDir, 'templates', 'code-review-template.html'), 'utf8');
const data = JSON.parse(fs.readFileSync(inputPath, 'utf8'));
const html = template
  .replaceAll('{{title}}', escapeHtml(data.title || 'Code review'))
  .replaceAll('{{jiraKey}}', escapeHtml(data.jiraKey || 'Sin Jira'))
  .replaceAll('{{repo}}', escapeHtml(data.repo || 'Repo no indicado'))
  .replaceAll('{{generatedAt}}', escapeHtml(data.generatedAt || new Date().toISOString()))
  .replaceAll('{{summary}}', escapeHtml(data.summary || 'Sin resumen.'))
  .replaceAll('{{teamsReply}}', escapeHtml(data.teamsReply || ''))
  .replaceAll('{{findings}}', renderFindings(data.findings || []));

fs.mkdirSync(path.dirname(outputPath), { recursive: true });
fs.writeFileSync(outputPath, html, 'utf8');
console.log(outputPath);

function renderFindings(findings) {
  if (!findings.length) {
    return '<div class="finding ok">No se detectaron findings bloqueantes con la informacion disponible.</div>';
  }
  return findings.map((finding) => `
    <div class="finding">
      <strong>${escapeHtml(finding.severity || 'medium')} - ${escapeHtml(finding.title || 'Finding')}</strong>
      <p>${escapeHtml(finding.detail || '')}</p>
      <p><em>${escapeHtml(finding.file || '')}${finding.line ? `:${escapeHtml(String(finding.line))}` : ''}</em></p>
    </div>
  `).join('\n');
}

function escapeHtml(value) {
  return String(value)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}
