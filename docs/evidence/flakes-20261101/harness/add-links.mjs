// add-links.mjs BIN: two real daemons on one Mac, each with its own scratch state, HOME and identity,
// sharing a fleet key. The owner listens on loopback, the host on the LAN address. machines:add on
// the owner, then no restart: does `fleet links` go up, and a command reach the host over the mesh?
import {spawn,execFileSync} from 'node:child_process';import fs from 'node:fs';import os from 'node:os';import path from 'node:path';
process.env.NODE_TLS_REJECT_UNAUTHORIZED='0';
const BIN=path.resolve(process.argv[2]);const OP=Number(process.env.RIG_OWNER_PORT||28891),HP=Number(process.env.RIG_HOST_PORT||28892),WAIT=Number(process.env.RIG_WAIT_SECS||45);
const ROOT=fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()),'wa-addlinks-'));const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const envFor=n=>{const d=`${ROOT}/${n}`;for(const s of ['home','tmp','logs','state'])fs.mkdirSync(`${d}/${s}`,{recursive:true});return{HOME:`${d}/home`,TMPDIR:`${d}/tmp`,PATH:process.env.PATH,LANG:'en_US.UTF-8',WORKASS_PROFILE:'dev',WORKASS_DATA_ROOT:d,WORKASS_LOG_ROOT:`${d}/logs`,WORKASS_UPDATE_CHANNEL:'local'}};
const O=envFor('o'),H=envFor('h'),OS=`${ROOT}/o/state`,HS=`${ROOT}/h/state`;
const key=execFileSync(BIN,['fleet','key','--state-dir',OS],{env:O,encoding:'utf8'}).split('\n').map(l=>l.trim()).find(l=>/^[A-Za-z0-9_+/=-]{20,}$/.test(l));
execFileSync(BIN,['fleet','join','--state-dir',HS],{env:H,input:key+'\n',encoding:'utf8',stdio:['pipe','ignore','ignore']});
const boot=(n,env,state,port,bind)=>{const l=fs.openSync(`${ROOT}/${n}/logs/daemon.log`,'a');return spawn(BIN,['--headless','--state-dir',state,'--port',String(port),'--bind',bind,'--beacon=false'],{cwd:ROOT,stdio:['ignore',l,l],env})};
const od=boot('o',O,OS,OP,'localhost'),hd=boot('h',H,HS,HP,'lan');const bye=()=>{for(const p of [od,hd])try{p.kill('SIGTERM')}catch{}};process.on('exit',bye);
const health=async p=>{for(let i=0;i<240;i++){try{if((await fetch(`https://127.0.0.1:${p}/workass/health`)).ok)return}catch{}await sleep(250)}throw Error('no health '+p)};
await Promise.all([health(OP),health(HP)]);
const lan=execFileSync('/usr/sbin/ipconfig',['getifaddr','en0'],{encoding:'utf8'}).trim();
const call=(port,channel,...args)=>new Promise((resolve,reject)=>{const ws=new WebSocket(`wss://127.0.0.1:${port}/ws`);const timer=setTimeout(()=>{try{ws.close()}catch{};reject(Error('timeout '+channel))},45000);ws.onopen=()=>ws.send(JSON.stringify({t:'invoke',id:'x',channel,args}));ws.onmessage=e=>{const m=JSON.parse(e.data);if(m.t==='reply'&&m.id==='x'){clearTimeout(timer);ws.close();m.error?reject(Error(JSON.stringify(m.error))):resolve(m.result)}};ws.onerror=()=>{clearTimeout(timer);reject(Error('ws '+channel))}});
const links=()=>execFileSync(BIN,['fleet','links','--state-dir',OS],{env:O,encoding:'utf8'}).replace(new RegExp(lan.replaceAll('.','\\.'),'g'),'LAN').replace(/name="[^"]*"/g,'name=HOST').replace(/\s+/g,' ').trim();
console.log('before add:',links());
const t0=Date.now();const added=await call(OP,'machines:add',{address:`${lan}:${HP}`});console.log('machines:add ok=',added.ok,added.error||'');
let up=null,last='';for(;Date.now()-t0<WAIT*1000;await sleep(500)){last=links();if(/\bUP\b/.test(last)){up=Date.now()-t0;break}}
console.log(up===null?`NOT LINKED after ${WAIT}s without a restart`:`LINKED ${(up/1000).toFixed(1)}s after machines:add, no restart`);console.log('links:',last.slice(0,240));
bye();await sleep(1500);fs.rmSync(ROOT,{recursive:true,force:true});process.exit(up===null?1:0);
