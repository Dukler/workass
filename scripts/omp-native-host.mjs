#!/usr/bin/env node

// Workass-owned Oh My Pi host.  This is deliberately a thin private-contract
// adapter around the published SDK; it never starts OMP's ACP compatibility
// server or translates through an ACP process.
import { randomUUID, createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import readline from 'node:readline';
import { pathToFileURL, fileURLToPath } from 'node:url';

const sessions = new Map();
const pending = new Map();
let seq = 0;
let protocolOutput = process.stdout;
const write = (x) => protocolOutput.write(`${JSON.stringify(x)}\n`);
const respond = (id, result) => write({ jsonrpc: '2.0', id, result });
const safe = (x) => String(x?.message || x || 'OMP request failed')
  .replace(/((?:api[_-]?key|token|secret|password|credential|bearer)\s*[:=]\s*)[^\s,;}]+/gi, '$1[redacted]')
  .replace(/Bearer\s+\S+/gi, 'Bearer [redacted]').slice(0, 2000);
const fail = (id, code, error) => write({ jsonrpc: '2.0', id, error: { code, message: safe(error) } });
const diagnostic = (label, error) => process.stderr.write(`${label}: ${safe(error)}\n`);
console.log = (...values) => diagnostic('OMP', values.map(String).join(' '));
console.info = console.log;
const notify = (sessionId, update) => write({ jsonrpc: '2.0', method: 'session/update', params: { sessionId, update } });
function peerRequest(method, params, signal) {
  if (signal?.aborted) return Promise.resolve(null);
  const id = `omp-host-${++seq}`;
  return new Promise(resolve => {
    const finish = value => { pending.delete(id); signal?.removeEventListener('abort', abort); resolve(value); };
    const abort = () => finish(null);
    pending.set(id, finish);
    signal?.addEventListener('abort', abort, {once:true});
    write({jsonrpc:'2.0',id,method,params});
  });
}
let sdkPromise;
async function sdk() {
  if (!sdkPromise) {
    const configured = String(process.env.WORKASS_OMP_SDK_MODULE || '@oh-my-pi/pi-coding-agent').trim();
    sdkPromise = import(path.isAbsolute(configured) ? pathToFileURL(configured).href : configured);
  }
  return sdkPromise;
}
async function instructions() {
  const file = String(process.env.WORKASS_INSTRUCTIONS_FILE || '').trim();
  if (!file) return '';
  return String(await readFile(file, 'utf8')).trim();
}
const modelID = model => model ? `${model.provider}/${model.id}` : '';
function modelRows(value) {
  const rows = Array.isArray(value) ? value : [];
  return rows.map(m => ({ modelId: modelID(m), name: String(m.name || m.id || 'Model'), description: String(m.description || '') })).filter(m => m.modelId);
}
function contentText(prompt) {
  if (typeof prompt === 'string') return prompt;
  return (Array.isArray(prompt) ? prompt : []).map(x => typeof x === 'string' ? x : String(x?.text || '')).join('');
}
function promptContent(prompt) {
  const blocks = typeof prompt === 'string' ? [{type:'text',text:prompt}] : prompt || [];
  if (!Array.isArray(blocks) || blocks.some(x => !x || (x.type !== 'text' && x.type !== 'image'))) throw new Error('Unsupported OMP prompt content');
  return {text:contentText(blocks),images:blocks.filter(x => x.type === 'image').map(x => ({type:'image',data:x.data,mimeType:x.mimeType}))};
}
function renderToolContent(value) {
  if (value === undefined || value === null) return '';
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.map(renderToolContent).filter(Boolean).join('\n');
  if (typeof value === 'object' && typeof value.text === 'string') return value.text;
  try { return JSON.stringify(value); } catch { return String(value); }
}
export class OmpSession {
  constructor(id, cwd, resume, params) { this.id = id; this.cwd = cwd; this.resume = resume; this.params = params; this.native = null; this.manager = null; this.models = []; this.modelObjects = new Map(); this.currentModel = String(params?.model || ''); this.currentEffort = params?.effort; this.currentMode = params?.mode; this.pendingInput = null; this.lastStopReason = 'end_turn'; }
  async start() {
    const mod = await sdk();
    let manager;
    if (this.resume) {
      const requested = String(this.params.sessionFile || this.params.sessionId || '').trim();
      const listed = await mod.SessionManager.list(this.cwd);
      const found = listed.find(x => String(x.id || '') === requested);
      if (found?.path) {
        const rows = (await readFile(found.path, 'utf8')).split('\n').filter(Boolean).map(line => JSON.parse(line));
        if (!rows.some(x => x.type === 'session' && x.id === requested)) throw new Error('OMP session header identity mismatch');
      }
      if (!found?.path) throw Object.assign(new Error('OMP provider candidate was never materialized'), { rpcCode: -32044 });
      manager = await mod.SessionManager.open(found.path, undefined, undefined, { initialCwd: this.cwd, suppressBreadcrumb: true });
      if (manager.getSessionId() !== requested) throw Object.assign(new Error('OMP resume identity mismatch'), { rpcCode: -32044 });
    } else manager = mod.SessionManager.create(this.cwd);
    this.manager = manager;
    const extra = await instructions();
    const result = await mod.createAgentSession({ cwd: this.cwd, sessionManager: manager, model: this.params.modelObject, thinkingLevel: this.currentEffort, appendSystemPrompt: extra || undefined, hasUI: false, interactivePrompts: true, autoApprove: false, agentRegistry: new mod.AgentRegistry() });
    this.native = result.session;
    await manager.ensureOnDisk();
    await manager.flush();
    const ui = {
      select: async (title, options, dialog) => {
        const labels = options.map(x => typeof x === 'string' ? x : x.label);
        const answer = await peerRequest('session/request_permission', {sessionId:this.id,toolCall:{toolCallId:randomUUID(),title},options:labels.map((name,i)=>({optionId:String(i),name,kind:name==='Deny'?'reject_once':'allow_once'}))}, dialog?.signal || this.turnAbort?.signal);
        return answer?.outcome?.outcome === 'selected' ? labels[Number(answer.outcome.optionId)] : undefined;
      },
      confirm: async (title, message) => (await peerRequest('session/request_permission', { sessionId: this.id, toolCall: { toolCallId: randomUUID(), toolName: 'confirm', title, rawInput: { message } }, options: [{ optionId: 'yes', name: 'Yes', kind: 'allow_once' }, { optionId: 'no', name: 'No', kind: 'reject_once' }] }, this.turnAbort?.signal))?.outcome?.optionId === 'yes',
      input: async () => undefined,
      notify: message => diagnostic('OMP notice', message),
      onTerminalInput: () => () => {}, setStatus: () => {}, setWorkingMessage: () => {}, setWidget: () => {}, setFooter: () => {}, setHeader: () => {}, setTitle: () => {}, custom: async () => undefined, setEditorText: () => {}, pasteToEditor: () => {}, getEditorText: () => '', addAutocompleteProvider: () => {}, setEditorComponent: () => {}, theme: {}, getAllThemes: async () => [], getTheme: async () => undefined, setTheme: async () => ({ success: false }), getToolsExpanded: () => false, setToolsExpanded: () => {}, setTerminalTitle: () => {}, setTerminalInput: () => {}, getTerminalInput: () => '', getTerminalSize: () => ({ rows: 0, columns: 0 }), getCurrentWorkingDirectory: () => this.cwd,
    };
    this.ui = ui;
    result.setToolUIContext(ui, true);
    await this.configureExtensions(ui);
    if (!this.resume) this.id = manager.getSessionId();
    this.unsubscribe = this.native.subscribe(event => this.event(event));
    const available = this.native.modelRegistry.getAvailable();
    for (const model of Array.isArray(available) ? available : []) this.modelObjects.set(modelID(model), model);
    this.models = modelRows(available);
    this.currentModel = modelID(this.native.model);
    this.currentEffort = this.native.thinkingLevel;
    this.currentMode = this.native.getPlanModeState?.()?.enabled ? 'plan' : 'default';
    this.native.setPlanProposalHandler?.(title => this.native.preparePlanForReview(title));
    return result;
  }
  async configureExtensions(ui) {
    const session = this.native, runner = session.extensionRunner;
    if (!runner) return;
    const compact = value => session.compact(typeof value === 'string' ? value : undefined, typeof value === 'object' ? value : undefined);
    const unsupported = async () => { throw new Error('Native session replacement requires a new Workass lane'); };
    runner.initialize({
      sendMessage:(message,options)=>{void session.sendCustomMessage(message,options).catch(e=>diagnostic('OMP extension',e));},
      sendUserMessage:(content,options)=>{void session.sendUserMessage(content,options).catch(e=>diagnostic('OMP extension',e));},
      appendEntry:(type,data)=>this.manager.appendCustomEntry(type,data),
      setLabel:(id,label)=>this.manager.appendLabelChange(id,label),
      getActiveTools:()=>session.getEnabledToolNames(),getAllTools:()=>session.getAllToolInfos(),
      setActiveTools:names=>session.setActiveToolsByName(names),getCommands:()=>runner.getRegisteredCommands(),
      setModel:async model=>{if(!await session.modelRegistry.getApiKey(model))return false;await session.setModel(model);return true;},
      getThinkingLevel:()=>session.thinkingLevel,setThinkingLevel:value=>session.setThinkingLevel(value),
      getServiceTiers:()=>session.serviceTierByFamily,setServiceTier:(family,tier)=>session.setServiceTierFamily(family,tier),
      getSessionName:()=>this.manager.getSessionName(),setSessionName:name=>this.manager.setSessionName(name,'user'),
    }, {
      getModel:()=>session.model,isIdle:()=>!session.isStreaming,abort:()=>session.abort(),
      hasPendingMessages:()=>session.queuedMessageCount>0,shutdown:()=>session.abort(),
      getContextUsage:()=>session.getContextUsage(),getSystemPrompt:()=>session.systemPrompt,compact,
    }, {
      getContextUsage:()=>session.getContextUsage(),waitForIdle:()=>session.agent.waitForIdle(),
      newSession:unsupported,branch:unsupported,navigateTree:unsupported,switchSession:unsupported,
      reload:()=>session.reload(),compact,
    },ui,'rpc');
    await runner.emit({type:'session_start'});
  }
  event(event) {
    const type = String(event?.type || '');
    if (type === 'message_end' && event.message?.role === 'user' && this.pendingInput) {
      const accepted = this.pendingInput;
      const promptId = String(event.message?.id || '');
      this.receipt = Promise.resolve(this.manager.ensureOnDisk()).then(() => this.manager.flush()).then(() => {
        notify(this.id, { sessionUpdate: '_workass_input_consumed', clientUserMessageId: accepted, promptId });
      });
      this.receipt.catch(() => {});
      this.pendingInput = null;
    } else if (type === 'message_update') {
      const d = event.assistantMessageEvent || event.event || event;
      if (d.type === 'text_delta' || d.type === 'text') notify(this.id, { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: String(d.delta || d.text || '') } });
      else if (d.type === 'thinking_delta' || d.type === 'reasoning_delta') notify(this.id, { sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: String(d.delta || d.text || '') } });
    } else if (type === 'tool_execution_start') notify(this.id, { sessionUpdate: 'tool_call', toolCallId: event.toolCallId, title: String(event.toolName || 'tool'), kind: 'other', status: 'in_progress', rawInput: event.args || {} });
    else if (type === 'tool_execution_update') notify(this.id, { sessionUpdate: 'tool_call_update', toolCallId: event.toolCallId, status: 'in_progress', content: [{ type: 'content', content: { type: 'text', text: renderToolContent(event.partialResult || event.output) } }] });
    else if (type === 'tool_execution_end') notify(this.id, { sessionUpdate: 'tool_call_update', toolCallId: event.toolCallId, status: event.isError ? 'failed' : 'completed', content: [{ type: 'content', content: { type: 'text', text: renderToolContent(event.result) } }] });
    else if (type === 'message_end' && event.message?.role === 'assistant') {
      if (event.message.stopReason === 'error') this.turnError = new Error(safe(event.message.errorMessage || 'OMP model request failed'));
      if (event.message.stopReason === 'aborted') this.lastStopReason = 'cancelled';
    }
    else if (type === 'auto_compaction_start') notify(this.id, {sessionUpdate:'_workass_compaction',phase:'started'});
    else if (type === 'auto_compaction_end' && event.result && !event.aborted && !event.errorMessage) {
      const checkpointId = this.manager.getLeafId();
      const digest = createHash('sha256').update(JSON.stringify(event.result)).digest('hex');
      notify(this.id, {sessionUpdate:'_workass_compaction',phase:'checkpoint',checkpointId,digest});
    }
  }
  options() {
    const modes = [{value:'default',name:'Default'}, {value:'always-ask',name:'Always ask'}, {value:'write',name:'Write'}, {value:'yolo',name:'Yolo'}];
    if (this.native.settings.get('plan.enabled')) modes.push({value:'plan',name:'Plan'});
    const result = [
      {id:'model',category:'model',name:'Model',type:'select',currentValue:this.currentModel,options:this.models.map(m => ({value:m.modelId,name:m.name,description:m.description}))},
      {id:'mode',category:'mode',name:'Mode',type:'select',currentValue:this.currentMode,options:modes},
    ];
    if (this.native.model?.reasoning) result.push({id:'effort',category:'thought_level',name:'Effort',type:'select',currentValue:this.currentEffort || 'off',options:['off','minimal','low','medium','high','xhigh'].map(value => ({value,name:value}))});
    return result;
  }
  async prompt(value) {
    if (this.running) throw new Error('OMP session already has an active prompt');
    const {text,images} = promptContent(value.prompt);
    this.pendingInput = String(value.clientUserMessageId || '');
    this.receipt = null;
    this.turnError = null;
    this.lastStopReason = 'end_turn';
    this.running = true;
    this.turnId = randomUUID();
    this.turnAbort = new AbortController();
    try {
      await this.native.prompt(text, {images,userInitiated:true});
      if (this.receipt) await this.receipt;
      if (this.turnError) throw this.turnError;
      return {stopReason:this.lastStopReason};
    } finally { this.turnAbort?.abort(); this.running = false; this.turnId = null; this.pendingInput = null; }
  }
  async steer(value) {
    if (!this.running || this.turnAbort?.signal.aborted || !this.native.isStreaming) throw new Error('OMP has no active turn to steer');
    if (typeof this.native.steer !== 'function') throw new Error('OMP SDK does not support native steering');
    const {text,images} = promptContent(value.prompt);
    if (!text.trim() && !images.length) throw new Error('OMP steer requires content');
    const turnId = this.turnId;
    await this.native.steer(text, images);
    return {turnId};
  }
  async setConfig(id, value) {
    if (this.running) throw new Error('Cannot change OMP configuration during an active prompt');
    value = String(value);
    if (id === 'model') {
      const model = this.modelObjects.get(value);
      if (!model) throw new Error('OMP model is unavailable');
      await this.native.setModel(model);
      this.currentModel = modelID(this.native.model);
      this.currentEffort = this.native.thinkingLevel;
    } else if (id === 'effort') {
      if (!this.native.model?.reasoning || !['off','minimal','low','medium','high','xhigh'].includes(value)) throw new Error('OMP effort is unavailable');
      this.native.setThinkingLevel(value);
      this.currentEffort = this.native.thinkingLevel;
    } else if (id === 'mode') {
      if (!this.options().find(x=>x.id==='mode').options.some(x=>x.value===value)) throw new Error('Unsupported OMP mode');
      if (value === 'default') this.native.settings.clearOverride('tools.approvalMode');
      else this.native.settings.override('tools.approvalMode', value === 'plan' ? 'always-ask' : value);
      const priorPlan = this.native.getPlanModeState?.();
      this.native.setPlanModeState(value === 'plan' ? {...priorPlan,enabled:true,planFilePath:priorPlan?.planFilePath || 'local://PLAN.md',workflow:priorPlan?.workflow || 'parallel',reentry:Boolean(priorPlan)} : undefined);
      this.native.setPlanProposalHandler?.(value === 'plan' ? title => this.native.preparePlanForReview(title) : null);
      this.currentMode = value;
    } else throw new Error('Unsupported OMP configuration option');
    const configOptions = this.options();
    notify(this.id, {sessionUpdate:'config_options_update',configOptions});
    return {configOptions};
  }
  async close() { this.turnAbort?.abort(); await this.native?.abort?.(); this.unsubscribe?.(); await this.native?.dispose?.(); }
}
async function open(params, resume) { const id = String(params?.sessionId || '').trim() || randomUUID(); if (resume && !params?.sessionFile && !params?.sessionId) throw new Error('OMP session/resume requires exact session id'); if (sessions.has(id)) return sessions.get(id); const s = new OmpSession(id, String(params?.cwd || process.cwd()), resume, params); await s.start(); if (!resume) s.id = String(s.native?.sessionManager?.getSessionId?.() || s.native?.sessionId || s.id); sessions.set(s.id, s); return s; }
async function request(m) {
  const { id, method, params = {} } = m;
  if (method === 'initialize') return respond(id, { protocolVersion: Number(params.protocolVersion || 1), agentInfo: { name: 'oh-my-pi', version: String((await sdk()).VERSION || 'unknown') }, agentCapabilities: { sessionCapabilities: { resume: {}, close: {} }, promptCapabilities: { image: true, audio: false, embeddedContext: false }, mcpCapabilities: { http: false, sse: false } }, authMethods: [], _meta: { workassNativeOMP: true, workassStableTurnInputV1: true, workassOMPSteerRequest: true } });
  if (method === 'session/new' || method === 'session/resume' || method === 'session/load') { const s = await open(params, method !== 'session/new'); return respond(id, { ...(method === 'session/new' ? { sessionId: s.id } : {}), configOptions: s.options(), availableModels: s.models, _meta: { workassProviderRealm: { accountScope: 'unverified-account', installScope: 'omp-sdk', verified: false } } }); }
  const s = sessions.get(String(params.sessionId || '')); if (!s) throw Object.assign(new Error('OMP session not found'), { rpcCode: -32000 });
  if (method === 'session/prompt') return respond(id, await s.prompt(params));
  if (method === '_workass/omp/steer') return respond(id, await s.steer(params));
  if (method === 'session/set_config_option') return respond(id, await s.setConfig(String(params.configId || ''), params.value));
  if (method === 'session/set_model') return respond(id, await s.setConfig('model', params.modelId ?? params.model));
  if (method === 'session/set_mode') return respond(id, await s.setConfig('mode', params.modeId ?? params.mode));
  if (method === 'session/close') { await s.close(); sessions.delete(s.id); return respond(id, {}); }
  throw Object.assign(new Error(`OMP host method not found: ${method}`), { rpcCode: -32601 });
}
export function serveOMP({ sdkModule, input = process.stdin, output = process.stdout } = {}) {
if (sdkModule) sdkPromise = Promise.resolve(sdkModule);
protocolOutput = output;
const lines = readline.createInterface({ input, crlfDelay: Infinity });
lines.on('line', line => { let m; try { m = JSON.parse(line); } catch { return diagnostic('OMP host invalid JSON', 'parse failure'); } if (m && Object.hasOwn(m, 'id') && !m.method) { const p = pending.get(String(m.id)); if (p) { pending.delete(String(m.id)); p(m.result ?? null); } return; } if (m?.method && Object.hasOwn(m, 'id')) void request(m).catch(e => fail(m.id, e.rpcCode || -32603, e)); if (m?.method === 'session/cancel') { const s=sessions.get(String(m.params?.sessionId || '')); if(s) { s.turnAbort?.abort(); s.lastStopReason='cancelled'; void s.native.abort({reason:'interrupted'}).catch(e=>diagnostic('OMP cancel failed',e)); } } });
return new Promise(resolve => lines.on('close', async () => { await Promise.allSettled([...sessions.values()].map(s => s.close())); sessions.clear(); resolve(); }));

}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) await serveOMP();
