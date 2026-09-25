// Deterministic official Pi SDK boundary fixture; never uses the user's profile.
import fs from 'node:fs';
import path from 'node:path';
import {randomUUID} from 'node:crypto';
const root = process.env.WORKASS_PI_FIXTURE_DIR;
if (!root) throw new Error('isolated Pi fixture directory required');
export const getAgentDir = () => root;
export class SessionManager {
  constructor(id,cwd,entries=[]) { Object.assign(this,{id,cwd,entries});this.file=path.join(root,`${id}.jsonl`); }
  static create(cwd) { return new SessionManager(randomUUID(),cwd); }
  static findById(_cwd,id) { const f=path.join(root,`${id}.jsonl`);return fs.existsSync(f)?f:undefined; }
  static open(file) { const [h,...entries]=fs.readFileSync(file,'utf8').trim().split('\n').map(JSON.parse);return new SessionManager(h.id,h.cwd,entries); }
  getSessionId() { return this.id; }
  getSessionFile() { return this.file; }
  getEntries() { return this.entries; }
  getLeafId() { return this.entries.at(-1)?.id; }
  appendMessage(message) {
    this.entries.push({type:'message',id:randomUUID(),message});
    // Match Pi: in-memory user input precedes its first assistant; the public
    // message_end callback precedes the append. Never force journal creation.
    if (message.content==='DISK_FAIL') throw new Error('fixture journal write failed');
    if (this.entries.some(e=>e.message.role==='assistant')) fs.writeFileSync(this.file,[{type:'session',id:this.id,cwd:this.cwd},...this.entries].map(JSON.stringify).join('\n')+'\n');
  }
}
export class DefaultResourceLoader {
  constructor(options) { this.options=options; }
  async reload() { fs.writeFileSync(path.join(root,'prompt.txt'),this.options.appendSystemPromptOverride(['NATIVE DEFAULT','PROJECT INSTRUCTIONS']).join('\n')); }
}
export async function createAgentSession({sessionManager:manager}) {
  const listeners=new Set();
  const emit=e=>listeners.forEach(fn=>fn(e));
  const end=message=>{emit({type:'message_end',message});manager.appendMessage(message)};
  const models=[{provider:'fixture',id:'same',name:'Fixture Same'},{provider:'other',id:'same',name:'Other Same'}];
  let release,ui,aborted=false;
  const steers=[];
  const session={
    isStreaming:false,model:models[0],thinkingLevel:'off',modelRuntime:{getAvailable:async()=>process.env.WORKASS_PI_FIXTURE_NO_MODELS ? [] : models},
    extensionRunner:{emit:async()=>{}},
    subscribe(fn){listeners.add(fn);return()=>listeners.delete(fn)},
    async bindExtensions(bindings){ui=bindings.uiContext},
    getAvailableThinkingLevels:()=>['off','low','high'],getContextUsage:()=>({tokens:42,contextWindow:4096}),
    async setModel(model,options){if(options.persist!==false)throw Error('user settings mutation');this.model=model},
    setThinkingLevel(level,options){if(options.persist!==false)throw Error('user settings mutation');this.thinkingLevel=level},
    async prompt(text,options) {
      if (this.isStreaming) throw Error('duplicate native prompt');
      if (options.expandPromptTemplates!==false) throw Error('imported context dispatched a native command');
      if (text==='FAIL') throw Error('native startup failure');
      this.isStreaming=true;aborted=false;
      fs.writeFileSync(path.join(root,`${manager.id}.started`),text);
      try {
        end({role:'user',content:text,images:options.images});
        emit({type:'message_update',assistantMessageEvent:{type:'thinking_delta',delta:'Fixture thinking'}});
        // Tests can steer/cancel before the first assistant creates the journal.
        if(text==='WAIT' || text.includes('[fixture:wait]')) await new Promise(resolve=>{release=resolve});
        if (!aborted) for (const steer of steers.splice(0)) end({role:'user',content:[{type:'text',text:steer.text},...steer.images]});
        if(text==='PERMISSION') {
          const choice=await ui.select('Extension guard',['Approve','Deny']);
          emit({type:'tool_execution_end',toolCallId:'permission',result:{content:[{type:'text',text:String(choice)}]}});
        }
        if(text==='TOOL') {
          emit({type:'tool_execution_start',toolCallId:'subagent-1',toolName:'subagent',args:{task:'fixture'}});
          emit({type:'tool_execution_update',toolCallId:'subagent-1',partialResult:{content:[{type:'text',text:'child working'}]}});
          emit({type:'tool_execution_end',toolCallId:'subagent-1',result:{content:[{type:'text',text:'child done'}]}});
        }
        if(text==='COMPACT') {emit({type:'compaction_start'});emit({type:'compaction_end',result:{summary:'fixture summary'}})}
        const answer=text==='STATE'?JSON.stringify({model:this.model,effort:this.thinkingLevel,images:options.images}):'Fixture answer';
        emit({type:'message_update',assistantMessageEvent:{type:'text_delta',delta:answer}});
        end({role:'assistant',stopReason:aborted?'aborted':text==='ERROR'?'error':'stop',errorMessage:text==='ERROR'?'fixture model failed':undefined});
        emit({type:'turn_end'});
      } finally {this.isStreaming=false;release=undefined;emit({type:'agent_settled'})}
    },
    async steer(text,images) {
      if(!this.isStreaming)throw Error('idle steer');
      if(text==='REJECT')throw Error('fixture steer rejected');
      fs.appendFileSync(path.join(root,'calls.jsonl'),JSON.stringify({method:'steer',text,images})+'\n');
      steers.push({text,images});
      if (text === '[fixture:consume]') release?.();
    },
    async followUp(){throw Error('a steer must not call followUp')},
    async abort(){aborted=true;release?.()},async waitForIdle(){},dispose(){},
  };
  return {session,extensionsResult:{errors:[]}};
}
