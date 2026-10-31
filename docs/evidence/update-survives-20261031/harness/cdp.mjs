// cdp.mjs PORT 'expr' : evaluate in the page served by the shell/daemon
const [port, expr, match='http'] = process.argv.slice(2);
const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
const page = list.find(p => p.type==='page' && p.url.startsWith(match) && !p.url.includes('devtools'));
const ws = new WebSocket(page.webSocketDebuggerUrl);
await new Promise(r=>ws.onopen=r);
const out = await new Promise(res=>{ ws.onmessage=e=>{const m=JSON.parse(e.data); if(m.id===1) res(m)}; ws.send(JSON.stringify({id:1,method:'Runtime.evaluate',params:{expression:expr,awaitPromise:true,returnByValue:true}})); });
const v = out.result?.result; console.log(out.result?.exceptionDetails? 'EXC '+JSON.stringify(out.result.exceptionDetails).slice(0,600) : typeof v?.value==='string'? v.value : JSON.stringify(v?.value)); ws.close();
