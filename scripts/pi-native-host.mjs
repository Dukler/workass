#!/usr/bin/env node
// Direct integration with the official, user-installed Pi SDK. Pi owns auth,
// journals, resources, extensions and child agents; Workass owns this transport.
import { existsSync, readFileSync, realpathSync } from 'node:fs';
import { randomUUID, createHash } from 'node:crypto';
import path from 'node:path';
import readline from 'node:readline';
import { fileURLToPath, pathToFileURL } from 'node:url';

const safe = value => String(value?.message || value || 'Pi request failed')
  .replace(/((?:api[_-]?key|token|secret|password|credential|bearer)\s*[:=]\s*)[^\s,;}]+/gi, '$1[redacted]')
  .replace(/Bearer\s+\S+/gi, 'Bearer [redacted]').slice(0, 2000);
const diagnostic = error => process.stderr.write(`Pi: ${safe(error)}\n`);
console.log = console.info = console.warn = (...args) => diagnostic(args.map(String).join(' '));

function resolvePiInstallation(executable) {
  if (!executable) throw new Error('Pi executable is missing; install @earendil-works/pi-coding-agent');
  const cli = realpathSync(executable);
  const candidates = [path.join(path.dirname(cli), 'node_modules', '@earendil-works', 'pi-coding-agent')];
  for (let dir = path.dirname(cli); dir !== path.dirname(dir); dir = path.dirname(dir)) candidates.push(dir);
  for (const candidate of candidates) {
    try {
      const pkg = JSON.parse(readFileSync(path.join(candidate,'package.json'),'utf8'));
      if (pkg.name !== '@earendil-works/pi-coding-agent') continue;
      const entry = path.resolve(candidate,pkg.exports?.['.']?.import || pkg.main);
      if (existsSync(entry)) return {entry,root:candidate};
    } catch { /* next npm install layout; the SDK has import-only exports */ }
  }
  throw new Error('The installed Pi must expose its official SDK; install the npm package @earendil-works/pi-coding-agent');
}
export const resolvePiSDK = executable => resolvePiInstallation(executable).entry;

function promptContent(prompt) {
  const blocks = typeof prompt === 'string' ? [{type:'text',text:prompt}] : prompt || [];
  if (!Array.isArray(blocks) || blocks.some(x => !x || (x.type !== 'text' && x.type !== 'image'))) throw new Error('Unsupported Pi prompt content');
  return {
    text: blocks.filter(x => x.type === 'text').map(x => String(x.text || '')).join(''),
    images: blocks.filter(x => x.type === 'image').map(x => ({type:'image',data:x.data,mimeType:x.mimeType})),
  };
}
const modelID = model => model ? `${model.provider}/${model.id}` : '';
const unknownToolOutcome = 'Tool execution outcome is unknown because the Pi session ended before a result was recorded.';
export function pairToolResults(messages) {
  const pending = new Map();
  const output = [];
  const heldSystems = [];
  let changed = false;
  const append = message => output.push(message);
  const appendHeldSystems = () => { for (const message of heldSystems.splice(0)) append(message); };
  const repairPending = () => {
    if (pending.size) {
      changed = true;
      for (const [toolCallId,toolName] of pending) append({
        role:'toolResult',toolCallId,toolName,
        content:[{type:'text',text:unknownToolOutcome}],isError:true,timestamp:Date.now(),
      });
      pending.clear();
    }
    appendHeldSystems();
  };
  for (const message of messages) {
    if (message?.role === 'system') {
      if (pending.size) { heldSystems.push(message); changed = true; }
      else append(message);
      continue;
    }
    if (message?.role === 'assistant' && ['error','aborted'].includes(message.stopReason)) {
      repairPending();
      changed = true;
      continue;
    }
    if (message?.role === 'toolResult') {
      const toolCallId = message.toolCallId;
      if (pending.has(toolCallId)) {
        append(message); pending.delete(toolCallId);
        if (!pending.size) appendHeldSystems();
      } else { repairPending(); changed = true; }
      continue;
    }
    repairPending();
    append(message);
    if (message?.role === 'assistant' && !['error','aborted'].includes(message.stopReason)) {
      for (const block of message.content || []) {
        if (block?.type === 'toolCall' && block.id) pending.set(block.id,block.name);
      }
    }
  }
  repairPending();
  return changed ? output : messages;
}
function toolContent(result) {
  return (result?.content || []).flatMap(content => {
    if (content.type === 'text') return [{type:'content',content:{type:'text',text:String(content.text || '')}}];
    if (content.type === 'image') return [{type:'content',content:{type:'image',data:content.data,mimeType:content.mimeType}}];
    return [];
  });
}

export class PiSession {
  constructor(sdk, params, notify, peerRequest) {
    Object.assign(this, {sdk,params,notify,peerRequest});
    this.cwd = String(params.cwd || process.cwd());
    this.running = false;
    this.pendingSteers = [];
  }
  async start(resume) {
    const {SessionManager,DefaultResourceLoader,createAgentSession,getAgentDir} = this.sdk;
    let manager;
    if (resume) {
      const id = String(this.params.sessionId || '');
      if (!id) throw new Error('Pi resume requires an exact session id');
      const file = SessionManager.findById(this.cwd, id);
      if (!file || !existsSync(file)) throw Object.assign(new Error('Pi provider candidate was never materialized'), {rpcCode:-32044});
      const header = JSON.parse(readFileSync(file,'utf8').split('\n')[0]);
      if (header.type !== 'session' || header.id !== id || path.resolve(header.cwd) !== path.resolve(this.cwd)) throw new Error('Pi resume identity mismatch');
      manager = SessionManager.open(file);
      if (manager.getSessionId() !== id) throw new Error('Pi resume identity mismatch');
    } else manager = SessionManager.create(this.cwd);
    this.manager = manager;
    this.id = manager.getSessionId();
    const instructionFile = process.env.WORKASS_INSTRUCTIONS_FILE;
    const extra = instructionFile ? readFileSync(instructionFile,'utf8').trim() : '';
    this.loader = new DefaultResourceLoader({
      cwd:this.cwd, agentDir:getAgentDir(),
      appendSystemPromptOverride: current => extra ? [...current,extra] : current,
    });
    await this.loader.reload();
    const result = await createAgentSession({cwd:this.cwd, sessionManager:manager, resourceLoader:this.loader});
    this.native = result.session;
    const agent = this.native.agent;
    if (typeof agent?.convertToLlm === 'function') {
      const convertToLlm = agent.convertToLlm.bind(agent);
      agent.convertToLlm = async messages => pairToolResults(await convertToLlm(messages));
    }
    // Discovery is Pi's own DefaultResourceLoader, including pi-subagents and
    // any user/project extensions. A broken configured extension is explicit.
    if (result.extensionsResult?.errors?.length) {
      this.native.dispose();
      throw new Error(`Pi extension failed to load: ${safe(result.extensionsResult.errors[0].error)}`);
    }
    const select = async (title, options, dialog) => {
      const labels = options.map(String);
      const answer = await this.peerRequest('session/request_permission', {
        sessionId:this.id, toolCall:{toolCallId:randomUUID(),title},
        options:labels.map((name,i)=>({optionId:String(i),name,kind:/^(no|deny|reject|cancel)\b/i.test(name) ? 'reject_once':'allow_once'})),
      }, dialog?.signal || this.turnAbort?.signal);
      return answer?.outcome?.outcome === 'selected' && labels.some((_,i)=>String(i)===answer.outcome.optionId) ? labels[Number(answer.outcome.optionId)] : undefined;
    };
    const noop = () => {};
    const ui = {
      select, confirm:async (title,message,dialog)=>(await select(`${title}\n${message}`,['Yes','No'],dialog))==='Yes',
      input:async()=>undefined, editor:async()=>undefined, custom:async()=>undefined,
      notify:diagnostic, onTerminalInput:()=>noop,
      setStatus:noop,setWorkingMessage:noop,setWorkingVisible:noop,setWorkingIndicator:noop,setHiddenThinkingLabel:noop,setWidget:noop,setFooter:noop,setHeader:noop,setTitle:noop,
      setEditorText:noop,pasteToEditor:noop,getEditorText:()=>'',setEditorComponent:noop,
      getEditorComponent:()=>undefined,addAutocompleteProvider:noop,
      getToolsExpanded:()=>false,setToolsExpanded:noop,theme:{fg:(_color,text)=>text,bg:(_color,text)=>text,bold:text=>text,italic:text=>text,dim:text=>text},getAllThemes:()=>[],getTheme:()=>undefined,setTheme:()=>({success:false}),
    };
    this.ui = ui;
    const unsupported = async () => { throw new Error('Replacing a native Pi session requires a new Workass lane'); };
    await this.native.bindExtensions({uiContext:ui,mode:'rpc',onError:diagnostic,
      commandContextActions:{waitForIdle:()=>this.native.waitForIdle(),newSession:unsupported,fork:unsupported,navigateTree:unsupported,switchSession:unsupported,reload:()=>this.native.reload()},
    });
    this.unsubscribe = this.native.subscribe(event => this.event(event));
    this.models = await this.native.modelRuntime.getAvailable();
    if (!this.models.length) throw new Error('Authentication required for Pi: no authenticated models. Run pi and /login, or configure a local model in Pi.');
    return this;
  }
  update(update) { this.notify(this.id,update); }
  commitInput() {
    // Pi emits message_end before its synchronous journal append and defers a
    // new journal until its first assistant message. Never invent a receipt or
    // force a header/assistant into Pi's history just to make resume possible.
    if (!this.pendingInput || !this.inputMessage) return;
    const entry = this.manager.getEntries().find(e => e.type === 'message' && e.message === this.inputMessage);
    const file = this.manager.getSessionFile();
    if (!entry || !file || !existsSync(file)) return;
    // A pre-existing journal alone is insufficient: a failed append must not
    // acknowledge new input. Inspect only for this native entry's identity.
    const persisted = readFileSync(file,'utf8').split('\n').some(line => {
      try { const row = JSON.parse(line); return row.id === entry.id && row.type === 'message' && row.message?.role === 'user'; } catch { return false; }
    });
    if (!persisted) return;
    this.update({sessionUpdate:'_workass_input_consumed',clientUserMessageId:this.pendingInput});
    this.pendingInput = null;
  }
  commitSteers() {
    if (!this.pendingSteers.some(steer => steer.accepted && steer.message)) return;
    const file = this.manager.getSessionFile();
    if (!file || !existsSync(file)) return;
    const entries = this.manager.getEntries();
    const candidates = this.pendingSteers.flatMap(steer => {
      if (!steer.accepted || !steer.message) return [];
      const entry = entries.find(e => e.type === 'message' && e.message === steer.message);
      return entry ? [{steer,entry}] : [];
    });
    if (!candidates.length) return;
    const persisted = new Set(readFileSync(file,'utf8').split('\n').flatMap(line => {
      try { const row = JSON.parse(line); return row.type === 'message' && row.message?.role === 'user' ? [row.id] : []; }
      catch { return []; }
    }));
    for (const {steer,entry} of candidates) {
      if (!persisted.has(entry.id)) continue;
      const index = this.pendingSteers.indexOf(steer);
      if (index < 0) continue;
      this.pendingSteers.splice(index,1);
      this.update({sessionUpdate:'_workass_pi_steer_consumed',clientUserMessageId:steer.id});
    }
  }
  event(event) {
    if (event.type === 'message_end') {
      if (event.message?.role === 'user' && !this.inputMessage) this.inputMessage = event.message;
      else if (event.message?.role === 'user') {
        const blocks = event.message.content;
        const text = Array.isArray(blocks) ? blocks.filter(block => block?.type === 'text').map(block => block.text).join('') : blocks;
        const images = Array.isArray(blocks) ? blocks.filter(block => block?.type === 'image') : [];
        const steer = this.pendingSteers.find(item => !item.message && item.text === text &&
          item.images.length === images.length && item.images.every((image,index) =>
            image.data === images[index].data && image.mimeType === images[index].mimeType));
        if (steer) steer.message = event.message;
      }
      if (event.message?.role === 'assistant') this.lastAssistant = event.message;
      queueMicrotask(() => { try { this.commitInput(); this.commitSteers(); } catch (error) { diagnostic(error); } });
    } else if (event.type === 'message_update') {
      const d = event.assistantMessageEvent;
      if (d?.type === 'text_delta' || d?.type === 'thinking_delta') this.update({sessionUpdate:d.type === 'text_delta' ? 'agent_message_chunk':'agent_thought_chunk',content:{type:'text',text:String(d.delta || '')}});
    } else if (event.type === 'tool_execution_start') {
      this.update({sessionUpdate:'tool_call',toolCallId:event.toolCallId,title:event.toolName,kind:'other',status:'in_progress',rawInput:event.args});
    } else if (event.type === 'tool_execution_update' || event.type === 'tool_execution_end') {
      this.update({sessionUpdate:'tool_call_update',toolCallId:event.toolCallId,status:event.type === 'tool_execution_update' ? 'in_progress' : event.isError ? 'failed':'completed',content:toolContent(event.partialResult || event.result)});
    } else if (event.type === 'compaction_start') this.update({sessionUpdate:'_workass_compaction',phase:'started'});
    else if (event.type === 'compaction_end' && event.result && !event.aborted && !event.errorMessage) {
      this.update({sessionUpdate:'_workass_compaction',phase:'checkpoint',checkpointId:this.manager.getLeafId(),digest:createHash('sha256').update(JSON.stringify(event.result)).digest('hex')});
    }
    if (event.type === 'turn_end' || event.type === 'agent_settled') {
      const usage = this.native.getContextUsage();
      if (usage && Number.isFinite(usage.tokens) && Number.isFinite(usage.contextWindow)) this.update({sessionUpdate:'usage_update',used:usage.tokens,size:usage.contextWindow});
    }
  }
  options() {
    const options = [
      {id:'model',category:'model',name:'Model',type:'select',currentValue:modelID(this.native.model),options:this.models.map(m=>({value:modelID(m),name:m.name || m.id}))},
      {id:'mode',category:'mode',name:'Mode',type:'select',currentValue:'native',options:[{value:'native',name:'Pi defaults'}]},
    ];
    const levels = this.native.getAvailableThinkingLevels();
    if (levels.length > 1) options.push({id:'effort',category:'thought_level',name:'Effort',type:'select',currentValue:this.native.thinkingLevel,options:levels.map(value=>({value,name:value}))});
    return options;
  }
  async setConfig(id,value) {
    if (this.running) throw new Error('Cannot change Pi configuration during an active prompt');
    if (id === 'model') {
      const model = this.models.find(m=>modelID(m)===value);
      if (!model) throw new Error('Pi model is unavailable');
      await this.native.setModel(model,{persist:false});
    } else if (id === 'effort') {
      if (!this.native.getAvailableThinkingLevels().includes(value)) throw new Error('Pi effort is unavailable');
      this.native.setThinkingLevel(value,{persist:false});
    } else if (id !== 'mode' || value !== 'native') throw new Error('Unsupported Pi configuration option');
    const configOptions = this.options();
    this.update({sessionUpdate:'config_options_update',configOptions});
    return {configOptions};
  }
  async prompt(params) {
    if (this.running) throw new Error('Pi already has an active prompt');
    const {text,images} = promptContent(params.prompt);
    this.running = true;
    this.turnId = randomUUID();
    this.turnAbort = new AbortController();
    this.pendingInput = String(params.clientUserMessageId || '');
    this.inputMessage = null;
    this.lastAssistant = null;
    try {
      // Context imported by the actor is data, not a native extension command.
      await this.native.prompt(text,{images,source:'rpc',expandPromptTemplates:false});
      await this.native.waitForIdle();
      this.commitInput();
      this.commitSteers();
      if (this.lastAssistant?.stopReason === 'error') throw new Error(safe(this.lastAssistant.errorMessage || 'Pi model request failed'));
      return {stopReason:this.turnAbort.signal.aborted || this.lastAssistant?.stopReason === 'aborted' ? 'cancelled':'end_turn'};
    } finally { this.turnAbort.abort(); this.running = false; this.turnId = null; this.pendingInput = null; this.pendingSteers = []; }
  }
  async steer(params) {
    if (!this.running || this.turnAbort.signal.aborted || !this.native.isStreaming) throw new Error('Pi has no active turn to steer');
    const {text,images} = promptContent(params.prompt);
    if (!text.trim() && !images.length) throw new Error('Pi steer requires content');
    const turnId = this.turnId;
    const steer = {id:String(params.clientUserMessageId || ''),text,images,accepted:false,message:null};
    if (steer.id) this.pendingSteers.push(steer);
    try { await this.native.steer(text,images,{source:'rpc'}); }
    catch (error) {
      const index = this.pendingSteers.indexOf(steer);
      if (index >= 0) this.pendingSteers.splice(index,1);
      throw error;
    }
    steer.accepted = true;
    if (steer.id) this.commitSteers();
    return {turnId};
  }
  async cancel() { this.turnAbort?.abort(); await this.native.abort(); }
  async close() {
    if (!this.native) return;
    try {
      await this.cancel();
      await this.native.extensionRunner?.emit({type:'session_shutdown'});
    } finally { this.unsubscribe?.(); this.native.dispose(); }
  }
}

export async function servePi({sdkModule,input=process.stdin,output=process.stdout}={}) {
  const configured = process.env.WORKASS_PI_SDK_MODULE;
  const install = !sdkModule && !configured ? resolvePiInstallation(process.env.WORKASS_PI_EXECUTABLE) : null;
  // The subagent extension supports SDK hosts outside Pi's own package. Give
  // its child launcher the selected install instead of relying on NODE_PATH or
  // accidentally loading a second SDK from the extension's dependencies.
  if (install) process.env.PI_SUBAGENTS_PI_CODING_AGENT_PACKAGE_ROOT ??= install.root;
  const sdk = sdkModule || await import(pathToFileURL(configured || install.entry).href);
  const sessions = new Map(), pending = new Map();
  let sequence = 0;
  const write = value => output.write(JSON.stringify(value)+'\n');
  const notify = (sessionId,update) => write({jsonrpc:'2.0',method:'session/update',params:{sessionId,update}});
  const peerRequest = (method,params,signal) => {
    if (signal?.aborted) return Promise.resolve(null);
    const id = `pi-host-${++sequence}`;
    return new Promise(resolve=>{
      const finish = result => {pending.delete(id);signal?.removeEventListener('abort',abort);resolve(result)};
      const abort = () => finish(null);
      pending.set(id,finish);signal?.addEventListener('abort',abort,{once:true});
      write({jsonrpc:'2.0',id,method,params});
    });
  };
  const request = async ({method,params={}}) => {
    if (method === 'initialize') return {protocolVersion:1,agentInfo:{name:'Pi',version:'sdk'},agentCapabilities:{sessionCapabilities:{resume:{},close:{}},promptCapabilities:{image:true},mcpCapabilities:{http:false,sse:false}},authMethods:[],_meta:{workassNativePi:true,workassStableTurnInputV1:true,workassPiSteerRequest:true,workassPiSteerReceipt:true}};
    if (['session/new','session/resume','session/load'].includes(method)) {
      if (sessions.has(params.sessionId)) throw new Error('Pi session already attached');
      const s = new PiSession(sdk,params,notify,peerRequest);
      try { await s.start(method !== 'session/new'); } catch (error) { await s.close().catch(diagnostic); throw error; }
      sessions.set(s.id,s);
      return {sessionId:s.id,configOptions:s.options(),availableModels:s.models.map(m=>({modelId:modelID(m),name:m.name || m.id})),_meta:{workassProviderRealm:{accountScope:'unverified-account',installScope:'pi-sdk',verified:false}}};
    }
    const s = sessions.get(params.sessionId);
    if (!s) throw new Error('Pi session not found');
    if (method === 'session/prompt') return s.prompt(params);
    if (method === '_workass/pi/steer') return s.steer(params);
    if (method === 'session/set_model') return s.setConfig('model',params.modelId ?? params.model);
    if (method === 'session/set_mode') return s.setConfig('mode',params.modeId ?? params.mode);
    if (method === 'session/set_config_option') return s.setConfig(params.configId,params.value);
    if (method === 'session/close') {await s.close();sessions.delete(s.id);return {}}
    throw Object.assign(new Error(`Pi host method not found: ${method}`),{rpcCode:-32601});
  };
  const lines = readline.createInterface({input,crlfDelay:Infinity});
  lines.on('line',line=>{
    let m;try {m=JSON.parse(line)} catch {diagnostic('Invalid host JSON');return}
    if (m.id !== undefined && !m.method) {pending.get(m.id)?.(m.result ?? null);return}
    if (m.method === 'session/cancel') {void sessions.get(m.params?.sessionId)?.cancel().catch(diagnostic);return}
    if (m.method && m.id !== undefined) void request(m).then(result=>write({jsonrpc:'2.0',id:m.id,result}),error=>write({jsonrpc:'2.0',id:m.id,error:{code:error.rpcCode || -32603,message:safe(error)}}));
  });
  return new Promise(resolve=>lines.on('close',async()=>{await Promise.allSettled([...sessions.values()].map(s=>s.close()));sessions.clear();for (const finish of pending.values()) finish(null);resolve()}));
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) await servePi();
