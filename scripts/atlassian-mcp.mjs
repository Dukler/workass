/* Shared, dependency-free Atlassian MCP client (pure logic, no Devin/AI).
   Reuses the locally cached Atlassian OAuth token and talks to the Atlassian
   MCP HTTP endpoint via JSON-RPC. Used by the Jira enrich + Jira sync scripts. */

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const oauthDir = path.join(os.homedir(), 'AppData', 'Roaming', 'devin', 'mcp', 'oauth');
const refreshHelper = path.join(os.homedir(), '.config', 'devin', 'scripts', 'Refresh-AtlassianMcpOAuth.ps1');
const JIRA_SITE = process.env.ASSISTANT_JIRA_SITE || 'https://san-sar-basic.atlassian.net';

export function refreshAtlassianOAuth() {
  if (!fs.existsSync(refreshHelper)) return;
  try {
    execFileSync('powershell.exe', [
      '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', refreshHelper,
      '-Quiet', '-RefreshWithinSeconds', '7200',
    ], { stdio: 'ignore' });
  } catch {
    // The MCP call will surface a clearer auth error if a refresh was required.
  }
}

export function readAtlassianOAuth() {
  const files = fs.readdirSync(oauthDir)
    .filter((name) => name.endsWith('.json'))
    .map((name) => path.join(oauthDir, name));
  for (const file of files) {
    const data = JSON.parse(fs.readFileSync(file, 'utf8'));
    if (data.server_name === 'atlassian' && data.access_token) return data;
  }
  throw new Error(`No Atlassian OAuth cache found under ${oauthDir}`);
}

/** Flatten a Jira description (plain string or ADF object) to plain text. */
export function adfToText(node) {
  if (node == null) return '';
  if (typeof node === 'string') return node;
  if (Array.isArray(node)) return node.map(adfToText).join('');
  let out = '';
  if (node.text) out += node.text;
  if (node.content) out += adfToText(node.content);
  if (node.type === 'paragraph' || node.type === 'heading' || node.type === 'rule') out += '\n';
  if (node.type === 'hardBreak' || node.type === 'listItem') out += '\n';
  return out;
}

export class AtlassianMcpClient {
  constructor(oauth) {
    this.oauth = oauth;
    this.sessionId = null;
    this.nextId = 1;
  }

  async initialize() {
    const response = await this.post({
      jsonrpc: '2.0', id: this.nextId++, method: 'initialize',
      params: { protocolVersion: '2024-11-05', capabilities: {}, clientInfo: { name: 'work-assistant', version: '0.1.0' } },
    });
    this.sessionId = response.sessionId;
    await this.post({ jsonrpc: '2.0', method: 'notifications/initialized', params: {} }, { expectNoJson: true });
  }

  async getCloudId() {
    const resources = await this.callTool('getAccessibleAtlassianResources', {});
    const resource = resources.find((item) => item.url === JIRA_SITE && item.scopes?.includes('read:jira-work'))
      || resources.find((item) => item.scopes?.includes('read:jira-work'))
      || resources[0];
    if (!resource?.id) throw new Error('No Atlassian Jira cloudId available');
    return resource.id;
  }

  async getJiraDescription(cloudId, key) {
    const issue = await this.callTool('getJiraIssue', { cloudId, issueIdOrKey: key, fields: ['summary', 'description'] });
    return {
      requestedKey: key,
      key: issue.key || key,
      summary: issue.fields?.summary || '',
      description: typeof issue.fields?.description === 'string' ? issue.fields.description : adfToText(issue.fields?.description),
      fetchedAt: new Date().toISOString(),
    };
  }

  async searchJql(cloudId, jql, fields = ['summary', 'status', 'issuetype', 'assignee', 'priority', 'description'], maxResults = 50) {
    const result = await this.callTool('searchJiraIssuesUsingJql', { cloudId, jql, fields, maxResults });
    const issues = Array.isArray(result) ? result : (result.issues || result.results || []);
    return issues.map((issue) => {
      const f = issue.fields || {};
      return {
        key: issue.key,
        summary: f.summary || '',
        status: f.status?.name || (typeof f.status === 'string' ? f.status : ''),
        type: f.issuetype?.name || (typeof f.issuetype === 'string' ? f.issuetype : ''),
        assignee: f.assignee?.displayName || (typeof f.assignee === 'string' ? f.assignee : ''),
        priority: f.priority?.name || (typeof f.priority === 'string' ? f.priority : ''),
        url: `${JIRA_SITE}/browse/${issue.key}`,
        description: (typeof f.description === 'string' ? f.description : adfToText(f.description)).replace(/\n{3,}/g, '\n\n').trim(),
      };
    });
  }

  async callTool(name, args) {
    const payload = await this.post({ jsonrpc: '2.0', id: this.nextId++, method: 'tools/call', params: { name, arguments: args } });
    const result = payload.result;
    if (result?.isError) throw new Error(`${name} failed: ${JSON.stringify(result.content)}`);
    const text = result?.content?.find((item) => item.type === 'text')?.text;
    return text ? JSON.parse(text) : result;
  }

  async post(body, { expectNoJson = false } = {}) {
    const headers = {
      authorization: `Bearer ${this.oauth.access_token}`,
      accept: 'application/json, text/event-stream',
      'content-type': 'application/json',
    };
    if (this.sessionId) headers['mcp-session-id'] = this.sessionId;

    const response = await fetch(this.oauth.url, { method: 'POST', headers, body: JSON.stringify(body) });
    const text = await response.text();
    if (!response.ok) throw new Error(`MCP HTTP ${response.status}: ${text.slice(0, 500)}`);
    if (expectNoJson || response.status === 202 || !text.trim()) {
      return { sessionId: response.headers.get('mcp-session-id') };
    }
    const data = parseSseOrJson(text);
    if (data.error) throw new Error(`MCP error ${data.error.code}: ${data.error.message}`);
    return { ...data, sessionId: response.headers.get('mcp-session-id') || this.sessionId };
  }
}

function parseSseOrJson(text) {
  const trimmed = text.trim();
  if (trimmed.startsWith('{')) return JSON.parse(trimmed);
  const dataLine = trimmed.split(/\r?\n/).find((line) => line.startsWith('data: '));
  if (!dataLine) throw new Error(`Unexpected MCP response: ${trimmed.slice(0, 200)}`);
  return JSON.parse(dataLine.slice('data: '.length));
}
