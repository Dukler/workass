'use strict';

// Drives the real BrowserManager observation methods over a fake CDP debugger
// whose frames are fake DOMs in their own node:vm contexts. It deliberately
// skips the Electron view/host machinery, so these tests depend only on the
// observation layer.

const vm = require('node:vm');
const { BrowserManager } = require('./browser-manager');
const { makeDOM } = require('./browser-read-dom.test-helper');

const roundtrip = (value) => (value === undefined ? undefined : JSON.parse(JSON.stringify(value)));

function fakeBrowser({ url = 'https://shop.example/', chatId = 'chat-fake' } = {}) {
  const manager = Object.create(BrowserManager.prototype);
  manager.entries = new Map();
  manager.ensureCDP = async () => {};
  const frames = new Map();
  const objects = new Map();
  const backendByElement = new Map();
  const elementByBackend = new Map();
  let nextObject = 0;
  let nextBackend = 100;
  const commands = [];

  const contextFor = (sessionId, contextId) => {
    for (const frame of frames.values()) {
      if (sessionId && frame.sessionId === sessionId) return frame;
      if (!sessionId && contextId != null && frame.contextId === contextId) return frame;
    }
    return sessionId || contextId != null ? null : frames.get(entry.rootFrameIdForFake);
  };
  const tree = (frameId) => {
    const frame = frames.get(frameId);
    return {
      frame: { id: frame.id, parentId: frame.parentId || undefined, loaderId: frame.loaderId, url: frame.url, securityOrigin: new URL(frame.url).origin },
      childFrames: Array.from(frames.values()).filter((child) => child.parentId === frameId).map((child) => tree(child.id)),
    };
  };
  const debug = {
    isAttached: () => true,
    async sendCommand(method, params = {}, sessionId) {
      commands.push({ method, params, sessionId: sessionId || null });
      if (method === 'Page.getFrameTree') {
        if (sessionId) {
          const frame = contextFor(sessionId, null);
          return frame ? { frameTree: tree(frame.id) } : {};
        }
        return { frameTree: tree(entry.rootFrameIdForFake) };
      }
      if (method === 'Accessibility.getFullAXTree') {
        const frame = params.frameId ? frames.get(params.frameId) : null;
        return { nodes: frame && frame.axNodes ? frame.axNodes : [] };
      }
      if (method === 'Runtime.evaluate') {
        const frame = contextFor(sessionId, params.contextId);
        if (!frame) throw new Error('no such context');
        if (frame.hang) return new Promise(() => {});
        return { result: { value: roundtrip(vm.runInContext(params.expression, frame.context)) } };
      }
      if (method === 'DOM.getFrameOwner') {
        const frame = frames.get(params.frameId);
        if (!frame || !frame.owner) throw new Error('no owner');
        if (!backendByElement.has(frame.owner)) {
          nextBackend += 1;
          backendByElement.set(frame.owner, nextBackend);
          elementByBackend.set(nextBackend, { element: frame.owner, frame: frames.get(frame.parentId) });
        }
        return { backendNodeId: backendByElement.get(frame.owner) };
      }
      if (method === 'DOM.resolveNode') {
        const found = elementByBackend.get(Number(params.backendNodeId));
        if (!found) throw new Error('no node');
        nextObject += 1;
        const objectId = 'obj-' + nextObject;
        objects.set(objectId, found);
        return { object: { objectId } };
      }
      if (method === 'Runtime.callFunctionOn') {
        const found = objects.get(params.objectId);
        if (!found) throw new Error('no object');
        const fn = vm.runInContext('(' + params.functionDeclaration + ')', found.frame.context);
        const args = (params.arguments || []).map((arg) => arg.value);
        return { result: { value: roundtrip(fn.apply(found.element, args)) } };
      }
      if (method === 'Runtime.releaseObject') { objects.delete(params.objectId); return {}; }
      return {};
    },
  };
  const webContents = {
    id: 4242,
    url,
    getURL() { return webContents.url; },
    isLoadingMainFrame: () => false,
    debugger: debug,
    async executeJavaScript(script) {
      return roundtrip(vm.runInContext(script, frames.get(entry.rootFrameIdForFake).context));
    },
  };
  const entry = {
    chatId, conversationId: chatId, lifecycleToken: Symbol('fake-entry'), closed: false,
    view: { webContents }, frames: new Map(), observations: new Map(), targetSessions: new Map(),
    executionContexts: new Map(), screenshots: new Map(), diagnostics: [], diagnosticBytes: 0,
    documentGeneration: 1, viewportGeneration: 1, interactionGeneration: 0, frameTreeGeneration: 0,
    frameGenerationSequence: 0, rootTargetId: 'target-root', rootFrameIdForFake: 'root',
    viewport: { width: 1440, height: 900 }, effectiveViewport: null, url, title: '', loading: false,
    operationChain: Promise.resolve(),
  };
  manager.entries.set(chatId, entry);

  // Adds (or replaces) a frame. kind: 'root', 'same-process' or 'oopif'.
  const setFrame = ({ id, parentId = null, frameUrl, build, kind = 'same-process', owner = null, loaderId = 'L1' }) => {
    const dom = makeDOM({ url: frameUrl });
    if (build) build(dom);
    const context = vm.createContext(dom.context);
    const frame = { id, parentId, url: frameUrl, loaderId, dom, context, owner, kind, sessionId: null, contextId: null };
    if (kind === 'same-process') {
      frame.contextId = 1000 + frames.size;
      entry.executionContexts.set(entry.rootTargetId + ':' + id, frame.contextId);
    }
    if (kind === 'oopif') {
      frame.sessionId = 'session-' + id;
      entry.targetSessions.set(frame.sessionId, { targetId: 'target-' + id, frameId: id, parentSessionId: null });
      entry.frames.set(id, {
        id, parentFrameId: parentId, loaderId, url: frameUrl, securityOrigin: new URL(frameUrl).origin,
        generation: ++entry.frameGenerationSequence, targetId: 'target-' + id, sessionId: frame.sessionId,
        contextId: null, access: 'owned_cdp_frame', unavailableReason: null, detached: false,
      });
    }
    frames.set(id, frame);
    return frame;
  };
  const loadRoot = (pageUrl, build) => {
    for (const id of Array.from(frames.keys())) frames.delete(id);
    entry.frames.clear();
    entry.targetSessions.clear();
    entry.executionContexts.clear();
    entry.observations.clear();
    webContents.url = pageUrl;
    entry.url = pageUrl;
    const root = setFrame({ id: 'root', frameUrl: pageUrl, build, kind: 'root', loaderId: 'L' + entry.documentGeneration });
    return root;
  };
  const navigate = (pageUrl, build) => {
    entry.documentGeneration += 1;
    entry.frameTreeGeneration += 1;
    return loadRoot(pageUrl, build);
  };
  // Gives a node a CDP backend node id, as DOM.getFrameOwner would.
  const backendIdFor = (element, frameId = 'root') => {
    if (!backendByElement.has(element)) {
      nextBackend += 1;
      backendByElement.set(element, nextBackend);
      elementByBackend.set(nextBackend, { element, frame: frames.get(frameId) });
    }
    return backendByElement.get(element);
  };
  return { manager, entry, frames, commands, setFrame, loadRoot, navigate, webContents, backendIdFor };
}

module.exports = { fakeBrowser };
