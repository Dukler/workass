package httpserve

// LANBridgeJS mirrors desktop/lan-server.js LAN_BRIDGE_JS for browser clients.
const LANBridgeJS = `(() => {
  const TOKEN_KEY = 'workass.lan.deviceToken';
  const DEVICE_ID_KEY = 'workass.lan.deviceId';
  const DEVICE_NAME_KEY = 'workass.lan.deviceName';
  let ws, open = false, ready = false; const pending = new Map(); const subs = {}; const eventCache = Object.create(null); let seq = 0; let socketGen = 0; const queue = [];
  // The channels an unapproved client may send before it is ready. Everything
  // else queues until approval, and these are exactly the calls that PRODUCE an
  // approval — queue them and the client waits for a reply to an invoke it never
  // sent while the daemon waits for a request that never arrives.
  const PRE_READY_CHANNELS = new Set(['lan:pairing-info', 'fleet:challenge', 'fleet:enroll']);
  const INVOKE_TIMEOUT_MS = 30000;
  const SESSION_INVOKE_TIMEOUT_MS = 120000;
  class WorkassInvokeError extends Error {
    constructor(code, channel, generation) {
      super('Workass invoke ' + channel + ' failed: ' + code);
      this.name = 'WorkassInvokeError';
      this.code = code;
      this.channel = channel;
      this.generation = generation;
    }
  }
  function getStored(k) { try { return localStorage.getItem(k) || ''; } catch (e) { return ''; } }
  function setStored(k, v) { try { if (v) localStorage.setItem(k, v); } catch (e) {} }
  function removeStored(k) { try { localStorage.removeItem(k); } catch (e) {} }
  function deviceName() {
    const existing = getStored(DEVICE_NAME_KEY);
    if (existing) return existing;
    const base = (navigator.platform || 'Browser') + ' ' + (new Date()).toISOString().slice(0, 10);
    setStored(DEVICE_NAME_KEY, base);
    return base;
  }
  function socketURL() {
    const params = new URLSearchParams();
    const token = getStored(TOKEN_KEY);
    if (token) params.set('deviceToken', token);
    params.set('deviceName', deviceName());
    const qs = params.toString();
    return (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + (qs ? '?' + qs : '');
  }
  function rejectInvoke(id, error) {
    const item = pending.get(id);
    if (!item) return;
    pending.delete(id);
    clearTimeout(item.timer);
    const queued = queue.findIndex((entry) => entry.id === id);
    if (queued >= 0) queue.splice(queued, 1);
    item.reject(error);
  }
  function rejectPending(code, generation) {
    for (const [id, item] of pending) {
      rejectInvoke(id, new WorkassInvokeError(code, item.channel, generation));
    }
    queue.length = 0;
  }
  function send(entry) {
    if (open && ws && ws.readyState === WebSocket.OPEN) ws.send(entry.data);
    else queue.push(entry);
  }
  function flush() {
    while (ready && queue.length) {
      const entry = queue.shift();
      if (pending.has(entry.id)) ws.send(entry.data);
    }
  }
  function connect() {
    if (pending.size) rejectPending('socket-replaced', socketGen);
    if (ws) {
      const prior = ws;
      ws = null;
      try { prior.close(); } catch (e) {}
    }
    open = false; ready = false;
    const gen = ++socketGen;
    const socket = new WebSocket(socketURL());
    ws = socket;
    socket.onopen = () => {
      if (ws !== socket || gen !== socketGen) return;
      window.__workassSocketGen = gen;
      window.dispatchEvent(new CustomEvent('workass:socket-open', { detail: { gen } }));
      open = true;
    };
    socket.onclose = () => {
      if (ws !== socket || gen !== socketGen) return;
      rejectPending('socket-closed', gen);
      ws = null; open = false; ready = false;
      setTimeout(connect, 1500);
    };
    socket.onmessage = (ev) => {
      if (ws !== socket || gen !== socketGen) return;
      let m; try { m = JSON.parse(ev.data); } catch { return; }
      if (m.t === 'reply') {
        const p = pending.get(m.id);
        if (p && p.generation === gen) {
          pending.delete(m.id);
          clearTimeout(p.timer);
          m.error ? p.reject(new Error(m.error)) : p.resolve(m.result);
        }
      }
      else if (m.t === 'event') {
        eventCache[m.channel] = m.payload;
        if (m.channel === 'lan:access-state') handleAccessState(m.payload || {});
        (subs[m.channel] || []).forEach((cb) => { try { cb(m.payload); } catch (e) {} });
      }
    };
  }
  function handleAccessState(state) {
    window.__workassLanAccess = state;
    if (state.state === 'approved') {
      if (state.deviceToken) setStored(TOKEN_KEY, state.deviceToken);
      if (state.deviceId) setStored(DEVICE_ID_KEY, state.deviceId);
      if (state.name) setStored(DEVICE_NAME_KEY, state.name);
      ready = true; flush();
      return;
    }
    ready = false;
    if (state.state === 'rejected' || state.state === 'denied' || state.state === 'timeout') {
      if (state.reason === 'invalid-token' || state.state === 'rejected') removeStored(TOKEN_KEY);
    }
  }
  connect();
  function invoke(channel, ...args) {
    return new Promise((resolve, reject) => {
      const id = ++seq;
      const generation = socketGen;
      const timeoutMs = channel === 'session:get' || channel === 'session:save' ? SESSION_INVOKE_TIMEOUT_MS : INVOKE_TIMEOUT_MS;
      const timer = setTimeout(() => {
        rejectInvoke(id, new WorkassInvokeError('invoke-timeout', channel, generation));
      }, timeoutMs);
      pending.set(id, { resolve, reject, timer, channel, generation });
      const data = JSON.stringify({ t: 'invoke', id, channel, args });
      const entry = { id, data, generation };
      (ready || PRE_READY_CHANNELS.has(channel)) ? send(entry) : queue.push(entry);
    });
  }
  const on = (channel, cb) => {
    (subs[channel] = subs[channel] || []).push(cb);
    if (Object.prototype.hasOwnProperty.call(eventCache, channel)) {
      const payload = eventCache[channel];
      Promise.resolve().then(() => {
        if ((subs[channel] || []).indexOf(cb) < 0) return;
        try { cb(payload); } catch (e) {}
      });
    }
    return () => {
      const list = subs[channel] || [];
      const idx = list.indexOf(cb);
      if (idx >= 0) list.splice(idx, 1);
    };
  };
  window.api = {
    isLanClient: true,
    lanPairingInfo: () => invoke('lan:pairing-info'), lanTakeControl: () => invoke('lan:take-control'),
    lanAccessDecide: (requestId, allow) => invoke('lan:access-decide', { requestId, allow }),
    lanDevices: () => invoke('lan:devices'), lanRevoke: (deviceId) => invoke('lan:revoke', { deviceId }),
    // E1/E3: the machine book, reachable by a client. Additive — an older
    // renderer simply never calls them, and an older daemon answers "unknown
    // channel", which feature detection upstream reads as absent.
    machinesList: () => invoke('machines:list'), machinesAdd: (address) => invoke('machines:add', { address }),
    machinesForget: (machineId) => invoke('machines:forget', { machineId }), machinesRefresh: () => invoke('machines:refresh'),
    // The fleet key, readable without a terminal. Listing is non-secret; the
    // other three answer fleet:not-local unless this client is on the machine
    // that holds the key, so the secret never crosses the network.
    fleetKeys: () => invoke('fleet:keys'), fleetReveal: (keyId) => invoke('fleet:reveal', { keyId }),
    fleetMint: () => invoke('fleet:mint'), fleetForget: (keyId) => invoke('fleet:forget', { keyId }),
    getState: () => invoke('state:get'), jiraGet: () => invoke('jira:get'), activityGet: () => invoke('activity:get'),
    deployCatalog: () => invoke('deploy:catalog'), deployVersions: (o) => invoke('deploy:versions', o), deployPreflight: (o) => invoke('deploy:preflight', o),
    listSkills: () => invoke('skills:list'), appMeta: () => invoke('app:meta'), stateDigest: () => invoke('state:digest'), getSettings: () => invoke('settings:get'), setSettings: (s) => invoke('settings:set', s),
    getConfig: () => invoke('config:get'), setConfig: (p) => invoke('config:set', p), getSession: () => invoke('session:get'), saveSession: (s) => invoke('session:save', s),
    // Dictation. The microphone belongs to the machine the human is sitting at,
    // so a remote view records here and sends the audio to whichever daemon owns
    // the chat — the reverse would record a machine with nobody in front of it.
    voiceStatus: () => invoke('voice:status'), voiceTranscribe: (audio, lang, vocab) => invoke('voice:transcribe', { audio, lang, vocab }),
    archiveAppend: (tabId, messages) => invoke('chat:archive-append', { tabId, messages }), archiveLoad: (tabId) => invoke('chat:archive-load', tabId),
    refresh: () => invoke('teams:refresh'), jiraSync: () => invoke('jira:sync'), deployAuth: (o) => invoke('deploy:auth', o), startJob: (o) => invoke('job:start', o), cancelJob: (id) => invoke('job:cancel', id), killTerminal: (t) => invoke('chat:kill-terminal', t),
    procList: () => invoke('proc:list'), procRead: (id) => invoke('proc:read', id), procKill: (id, tree) => invoke('proc:kill', { id, tree }), procKillAll: () => invoke('proc:kill-all'),
    clearActivity: (id) => invoke('activity:clear', id), appChatReset: () => invoke('app-chat:reset'), appChatNewSession: (o) => invoke('app-chat:new-session', o),
    appChatSteer: (sessionId, prompt, images, clientUserMessageId, continuationAssistantMessageId, boundary) => invoke('app-chat:steer', { sessionId, prompt, images, clientUserMessageId, continuationAssistantMessageId, boundary }),
    appChatUseRateLimitReset: (providerId, sessionId, idempotencyKey, creditId) => invoke('app-chat:use-rate-limit-reset', { providerId, sessionId, idempotencyKey, creditId }),
    spawnedWorkList: (tabId, chatId) => invoke('spawned-work:list', { tabId, chatId }),
    chatCommandsGet: (tabId, chatId) => invoke('chat:commands-get', { tabId, chatId }),
    spawnedWorkRead: (tabId, chatId, id, tailBytes) => invoke('spawned-work:read', { tabId, chatId, id, tailBytes }),
    spawnedWorkStop: (tabId, chatId, id) => invoke('spawned-work:stop', { tabId, chatId, id }),
    appChatDetectAcp: (opts) => invoke('app-chat:detect-acp', opts || {}),
    appChatFork: (o) => invoke('app-chat:fork', o),
    chatCheckpoints: (o) => invoke('chat:checkpoints', o), chatRewind: (o) => invoke('chat:rewind', o),
    chatDiff: (o) => invoke('chat:diff', o), chatEnvGet: (o) => invoke('chat:env-get', o),
    providersList: () => invoke('providers:list'), providersDetect: (o) => invoke('providers:detect', o || {}), providersUpdate: (providerId) => invoke('providers:update', { providerId }), providersToggle: (id, enabled) => invoke('providers:toggle', { id, enabled }),
    pickDirectory: () => invoke('dialog:pick-directory'), listDir: (p) => invoke('fs:list-dir', p), appChatCloseSession: (s) => invoke('app-chat:close-session', s),
    codeUnlock: (pin) => invoke('code:unlock', pin), codeLock: () => invoke('code:lock'), codeTree: () => invoke('code:tree'), codeRead: (rel) => invoke('code:read', rel),
    appChatSetModel: (s, m) => invoke('app-chat:set-model', { sessionId: s, modelId: m }), appChatSetMode: (s, m) => invoke('app-chat:set-mode', { sessionId: s, modeId: m }),
    chatPermissionDecide: (id, optionId) => invoke('chat:permission-decide', { id, optionId }),
    chatPendingPermissions: () => invoke('chat:permissions-pending'),
    saveDraft: (k, t) => invoke('draft:save', { key: k, text: t }), setStatus: (k, st) => invoke('status:set', { key: k, status: st }),
    writeClipboard: (t) => invoke('clipboard:write', t), notify: (ti, b) => invoke('notify', { title: ti, body: b }), openExternal: (u) => invoke('external:open', u), openReview: (k) => invoke('review:open', k),
    browserCaptureRegion: (rect) => invoke('browser:capture-region', rect),
    onJobEvent: (cb) => on('job:event', cb), onChatCatalog: (cb) => on('chat:catalog', cb), onChatSessionReplaced: (cb) => on('chat:session-replaced', cb), onChatCompacted: (cb) => on('chat:compacted', cb), onProcChanged: (cb) => on('proc:changed', cb), onRefreshEvent: (cb) => on('refresh:event', cb), onAgentNavigate: (cb) => on('agent:navigate', cb),
    onSettingsChanged: (cb) => on('settings:changed', cb), onConfigChanged: (cb) => on('config:changed', cb), onAgentApply: (cb) => on('agent:apply', cb),
    onChatCommands: (cb) => on('chat:commands', cb),
    onChatPermissionRequest: (cb) => on('chat:permission-request', cb), onChatPermissionResolved: (cb) => on('chat:permission-resolved', cb), onLanControllerChanged: (cb) => on('lan:controller-changed', cb), onLanAccessState: (cb) => on('lan:access-state', cb), onLanAccessRequest: (cb) => on('lan:access-request', cb),
    onChatCheckpointRestored: (cb) => on('chat:checkpoint-restored', cb), onChatEngineRecovered: (cb) => on('chat:engine-recovered', cb), onChatEnv: (cb) => on('chat:env', cb),
    onNotify: (cb) => on('notify', cb), onNotifyBacklog: (cb) => on('notify:backlog', cb),
    onChatPlanUsage: (cb) => on('chat:plan-usage', cb),
    onMachinesChanged: (cb) => on('machines:changed', cb),
    onSpawnedWorkChanged: (cb) => on('spawned-work:changed', cb),
    onProvidersList: (cb) => on('providers:list', cb), onProvidersUpdates: (cb) => on('providers:updates', cb), onProvidersUpdateProgress: (cb) => on('providers:update-progress', cb), onAppUpdate: (cb) => on('app:update', cb),
  };
})();`
