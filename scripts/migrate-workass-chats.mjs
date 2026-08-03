#!/usr/bin/env node

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

function fail(message) {
  process.stderr.write(`migrate-workass-chats: ${message}\n`);
  process.exit(1);
}

function argsOf(argv) {
  const out = { replace: false };
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === '--replace') { out.replace = true; continue; }
    if (arg === '--source-state' || arg === '--dest-root') {
      if (!argv[i + 1]) fail(`${arg} needs a value`);
      out[arg.slice(2).replace(/-([a-z])/g, (_m, c) => c.toUpperCase())] = argv[++i];
      continue;
    }
    fail(`unknown argument: ${arg}`);
  }
  if (!out.sourceState || !out.destRoot) fail('usage: migrate-workass-chats.mjs --source-state PATH --dest-root PATH [--replace]');
  return out;
}

function readJSON(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); }
  catch (error) { fail(`cannot read ${file}: ${error.message}`); }
}

function sha256File(file) {
  return crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
}

function validateState(sourceState) {
  const sessionFile = path.join(sourceState, 'session-state.json');
  const nativeFile = path.join(sourceState, 'native-sessions.json');
  const session = readJSON(sessionFile);
  const native = readJSON(nativeFile);
  if (!Array.isArray(session.chats)) fail('session-state.json has no chats array');
  if (!Array.isArray(native.bindings)) fail('native-sessions.json has no bindings array');

  const tabs = new Map();
  const chatIds = new Set();
  const messageOwners = new Map();
  for (const chat of session.chats) {
    const tabId = String(chat?.id || '').trim();
    const chatId = String(chat?.chatId || '').trim();
    if (!tabId || !chatId) fail('every chat must have stable id and chatId');
    if (tabs.has(tabId)) fail(`duplicate tab id: ${tabId}`);
    if (chatIds.has(chatId)) fail(`duplicate conversation id: ${chatId}`);
    tabs.set(tabId, chatId);
    chatIds.add(chatId);
    for (const message of Array.isArray(chat.messages) ? chat.messages : []) {
      const messageId = String(message?.id || '').trim();
      if (!messageId) fail(`chat ${tabId} contains a message without an id`);
      const owner = messageOwners.get(messageId);
      if (owner && owner !== tabId) fail(`message ${messageId} crosses chats ${owner} and ${tabId}`);
      messageOwners.set(messageId, tabId);
    }
  }
  if (session.activeId && !tabs.has(String(session.activeId))) fail(`activeId does not name an authoritative chat: ${session.activeId}`);

  const nativeOwners = new Map();
  const bindingKeys = new Set();
  for (const binding of native.bindings) {
    const tabId = String(binding?.tabId || '').trim();
    const chatId = String(binding?.chatId || '').trim();
    const providerId = String(binding?.providerId || '').trim();
    const sessionId = String(binding?.sessionId || '').trim();
    if (!tabId || !chatId || !providerId || !sessionId) fail('native binding is missing ownership identity');
    if (tabs.get(tabId) !== chatId) fail(`native binding does not belong to authoritative chat ${tabId}/${chatId}`);
    const key = `${tabId}\u0000${providerId}`;
    if (bindingKeys.has(key)) fail(`duplicate native binding for ${tabId}/${providerId}`);
    bindingKeys.add(key);
    const priorOwner = nativeOwners.get(sessionId);
    if (priorOwner && priorOwner !== key) fail(`provider-native session has multiple chat owners`);
    nativeOwners.set(sessionId, key);
  }
  return { session, native, tabs, chatIds, messageOwners, sessionFile, nativeFile };
}

function copyFile(source, destination) {
  fs.mkdirSync(path.dirname(destination), { recursive: true, mode: 0o700 });
  fs.copyFileSync(source, destination);
  fs.chmodSync(destination, 0o600);
}

function copyChats(validated, sourceState, staging) {
  copyFile(validated.sessionFile, path.join(staging, 'session-state.json'));
  copyFile(validated.nativeFile, path.join(staging, 'native-sessions.json'));

  const archiveSource = path.join(sourceState, 'chat-archive');
  const archiveDestination = path.join(staging, 'chat-archive');
  fs.mkdirSync(archiveDestination, { recursive: true, mode: 0o700 });
  const archiveFiles = fs.existsSync(archiveSource)
    ? fs.readdirSync(archiveSource).filter((name) => name.endsWith('.jsonl')).sort() : [];
  let archiveRows = 0;
  let legacyArchiveRows = 0;
  for (const name of archiveFiles) {
    const source = path.join(archiveSource, name);
    const tabId = name.slice(0, -'.jsonl'.length);
    const authoritative = validated.tabs.has(tabId);
    const rows = fs.readFileSync(source, 'utf8').split(/\r?\n/).filter(Boolean);
    for (const [index, row] of rows.entries()) {
      try {
        const parsed = JSON.parse(row);
        if (!String(parsed?.role || '').trim()) throw new Error('missing role');
        if (!String(parsed?.id || '').trim()) {
          if (authoritative) throw new Error('authoritative archive row is missing id');
          legacyArchiveRows += 1;
        }
      } catch (error) { fail(`invalid archive row ${name}:${index + 1}: ${error.message}`); }
    }
    archiveRows += rows.length;
    copyFile(source, path.join(archiveDestination, name));
  }

  const checkpointSource = path.join(sourceState, 'checkpoints');
  const checkpointDestination = path.join(staging, 'checkpoints');
  let checkpointCount = 0;
  if (fs.existsSync(checkpointSource)) {
    for (const chatId of [...validated.chatIds].sort()) {
      const name = `${chatId}.json`;
      const source = path.join(checkpointSource, name);
      if (!fs.existsSync(source)) continue;
      copyFile(source, path.join(checkpointDestination, name));
      checkpointCount += 1;
    }
  }
  return { archiveFiles: archiveFiles.length, archiveRows, legacyArchiveRows, checkpointCount };
}

function fileManifest(root) {
  const rows = [];
  const walk = (dir) => {
    for (const name of fs.readdirSync(dir).sort()) {
      const file = path.join(dir, name);
      const stat = fs.statSync(file);
      if (stat.isDirectory()) walk(file);
      else rows.push({ path: path.relative(root, file), bytes: stat.size, sha256: sha256File(file) });
    }
  };
  walk(root);
  return rows;
}

const options = argsOf(process.argv.slice(2));
const sourceState = path.resolve(options.sourceState);
const destRoot = path.resolve(options.destRoot);
const destination = path.join(destRoot, 'state');
if (sourceState === destination) fail('source and destination state directories are identical');
if (!fs.existsSync(sourceState)) fail(`source state directory does not exist: ${sourceState}`);
const validated = validateState(sourceState);

fs.mkdirSync(destRoot, { recursive: true, mode: 0o700 });
const stamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\.\d{3}Z$/, 'Z');
const staging = path.join(destRoot, `.chat-migration-${stamp}-${process.pid}`);
fs.mkdirSync(staging, { mode: 0o700 });
try {
  const copied = copyChats(validated, sourceState, staging);
  const manifest = {
    version: 1,
    migratedAt: new Date().toISOString(),
    sourceState,
    destination,
    activeId: validated.session.activeId || null,
    chatCount: validated.session.chats.length,
    messageCount: validated.messageOwners.size,
    nativeBindingCount: validated.native.bindings.length,
    nativeSessionHashes: validated.native.bindings.map((binding) => ({
      tabId: binding.tabId,
      chatId: binding.chatId,
      providerId: binding.providerId,
      sessionIdSha256: crypto.createHash('sha256').update(String(binding.sessionId)).digest('hex'),
    })),
    ...copied,
    files: fileManifest(staging),
  };
  fs.writeFileSync(path.join(staging, 'migration-manifest.json'), `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o600 });

  let backup = null;
  if (fs.existsSync(destination)) {
    if (!options.replace) fail(`destination already exists: ${destination} (pass --replace)`);
    const backupRoot = path.join(destRoot, 'migration-backups');
    fs.mkdirSync(backupRoot, { recursive: true, mode: 0o700 });
    backup = path.join(backupRoot, `${stamp}-state`);
    fs.renameSync(destination, backup);
  }
  fs.renameSync(staging, destination);
  process.stdout.write(`CHAT_MIGRATION_COMPLETE\nsource=${sourceState}\ndestination=${destination}\nbackup=${backup || 'none'}\nchats=${manifest.chatCount}\nmessages=${manifest.messageCount}\nnative_bindings=${manifest.nativeBindingCount}\narchives=${manifest.archiveFiles}\narchive_rows=${manifest.archiveRows}\nlegacy_archive_rows=${manifest.legacyArchiveRows}\ncheckpoints=${manifest.checkpointCount}\nmanifest=${path.join(destination, 'migration-manifest.json')}\n`);
} catch (error) {
  fs.rmSync(staging, { recursive: true, force: true });
  throw error;
}
