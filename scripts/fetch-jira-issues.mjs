#!/usr/bin/env node
/* Pure-logic Jira sync (no Devin/AI): fetches the current user's open-sprint
   issues via the Atlassian MCP HTTP API (cached OAuth token) and writes
   out/jira-issues.json. */

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { AtlassianMcpClient, readAtlassianOAuth, refreshAtlassianOAuth } from './atlassian-mcp.mjs';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const rootDir = path.resolve(__dirname, '..');
const outDir = process.env.TEAMS_MONITOR_OUT_DIR || path.join(rootDir, 'out');
const outPath = path.join(outDir, 'jira-issues.json');

const JQL = process.env.ASSISTANT_JIRA_JQL
  || 'assignee = currentUser() AND sprint in openSprints() AND statusCategory != Done ORDER BY Rank ASC';

async function main() {
  fs.mkdirSync(outDir, { recursive: true });
  console.log('Refrescando OAuth de Atlassian (si hace falta)…');
  refreshAtlassianOAuth();

  const oauth = readAtlassianOAuth();
  const client = new AtlassianMcpClient(oauth);
  console.log('Conectando a Atlassian MCP…');
  await client.initialize();
  const cloudId = await client.getCloudId();

  console.log(`JQL: ${JQL}`);
  const issues = await client.searchJql(cloudId, JQL);
  console.log(`Issues encontrados: ${issues.length}`);

  const payload = { syncedAt: new Date().toISOString(), jql: JQL, issues };
  fs.writeFileSync(outPath, JSON.stringify(payload, null, 2), 'utf8');
  console.log(`Escrito: ${outPath}`);
}

main().catch((error) => {
  console.error(error.stack || error.message || String(error));
  process.exit(1);
});
