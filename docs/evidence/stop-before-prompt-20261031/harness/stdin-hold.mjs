// Rig only: makes a native host read session/prompt together with whatever the daemon writes in the next 150 ms, as a busy event loop does.
import fs from 'node:fs';
if ((process.argv[1]||'').includes('-native-host')) {
  const log=process.env.RIG_HOLD_LOG; if(log) fs.appendFileSync(log,`host ${process.pid} ${process.argv[1].split('/').pop()}\n`);
  const emit=process.stdin.emit.bind(process.stdin); let held=null;
  process.stdin.emit=(ev,...a)=>{ if(ev!=='data') return emit(ev,...a);
    const c=String(a[0]);
    if(held!==null){held+=c;return true}
    if(c.includes('"session/prompt"')){held=c;if(log)fs.appendFileSync(log,`holding ${process.pid}\n`);setTimeout(()=>{const h=held;held=null;if(log)fs.appendFileSync(log,`read prompt cancel=${h.includes('session/cancel')}\n`);emit('data',Buffer.from(h))},400);return true}
    return emit(ev,...a)};
}
