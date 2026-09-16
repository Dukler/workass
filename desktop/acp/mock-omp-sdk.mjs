// Deterministic native SDK boundary fixture. Never loads the user's OMP state.
import fs from 'node:fs';
import path from 'node:path';
import {randomUUID} from 'node:crypto';
const root = process.env.WORKASS_OMP_FIXTURE_DIR;
if (!root) throw new Error('isolated OMP fixture directory required');
fs.mkdirSync(root,{recursive:true});
export class AgentRegistry {}
export class SessionManager {
  constructor(id,cwd,file) { Object.assign(this,{id,cwd,file,entries:[]}); }
  static create(cwd) { return new SessionManager(randomUUID(),cwd,null); }
  static async list() { return fs.readdirSync(root).filter(x=>x.endsWith('.jsonl')).map(x=>({id:x.slice(0,-6),path:path.join(root,x)})); }
  static async open(file) { const rows=fs.readFileSync(file,'utf8').trim().split('\n').map(JSON.parse); const m=new SessionManager(rows[0].id,rows[0].cwd,file); m.entries=rows.slice(1); return m; }
  getSessionId() { return this.id; }
  getSessionFile() { return this.file; }
  getLeafId() { return 'fixture-checkpoint'; }
  async ensureOnDisk() { this.file ||= path.join(root,`${this.id}.jsonl`); await this.flush(); }
  async flush() { if(this.file) fs.writeFileSync(this.file,[{type:'session',id:this.id,cwd:this.cwd},...this.entries].map(x=>JSON.stringify(x)).join('\n')+'\n'); }
}
export async function createAgentSession({sessionManager:manager,appendSystemPrompt}={}) {
  const listeners=new Set(), overrides=new Map();
  const emit=e=>listeners.forEach(fn=>fn(e));
  const models=[{provider:'fixture',id:'same',name:'Fixture Same',reasoning:true},{provider:'other',id:'same',name:'Other Same',reasoning:true}];
  let release, ui;
  const session={
    sessionManager:manager, modelRegistry:{getAvailable:()=>models}, model:models[0],thinkingLevel:'off',
    settings:{get:k=>overrides.has(k)?overrides.get(k):k==='plan.enabled',override:(k,v)=>overrides.set(k,v),clearOverride:k=>overrides.delete(k)},
    subscribe(fn){listeners.add(fn);return()=>listeners.delete(fn)},
    setClientBridge(b){this.bridge=b}, setModel(m){this.model=m}, setThinkingLevel(v){this.thinkingLevel=v}, setPlanModeState(v){this.plan=v},
    async prompt(text, options) {
      if(text==='FAIL') throw Error('native startup failure');
      const message={role:'user',content:text};
      emit({type:'message_start',message});
      manager.entries.push({type:'message',message});
      emit({type:'message_end',message});
      if(text==='PERMISSION' && overrides.get('tools.approvalMode')!=='yolo') {
        const choice=await ui.select('bash approval',['Approve','Deny']);
        const outcome={optionId:choice};
        emit({type:'tool_execution_end',toolCallId:'tool',result:{content:[{type:'text',text:outcome.optionId}]}});
      }
      if(text==='WAIT') await new Promise(resolve=>{release=resolve});
      if(text==='COMPACT') { emit({type:'auto_compaction_start'}); emit({type:'auto_compaction_end',result:{summary:'fixture native summary'}}); }
      const answer= text==='STATE' ? JSON.stringify({approval:overrides.get('tools.approvalMode'),plan:this.plan,images:options.images}) : 'Fixture answer';
      emit({type:'message_update',assistantMessageEvent:{type:'text_delta',delta:answer}});
      const assistant={role:'assistant',stopReason:text==='ERROR'?'error':'stop',errorMessage:text==='ERROR'?'fixture model failed':undefined};
      manager.entries.push({type:'message',message:assistant});
      await manager.ensureOnDisk();
      emit({type:'message_end',message:assistant});
      emit({type:'agent_end',messages:[assistant]});
    },
    async abort(){release?.()}, async dispose(){await manager.flush()},
  };
  fs.writeFileSync(path.join(root,'prompt.txt'),'NATIVE DEFAULT\n'+(appendSystemPrompt||''));
  return {session,setToolUIContext(value){ui=value}};
}
