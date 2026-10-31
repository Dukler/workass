// wire.mjs PORT CHANNEL 'JSON args array'
process.env.NODE_TLS_REJECT_UNAUTHORIZED='0';
const [port,channel,args]=process.argv.slice(2);
const ws=new WebSocket(`wss://127.0.0.1:${port}/ws`);
ws.onopen=()=>ws.send(JSON.stringify({t:'invoke',id:'x',channel,args:JSON.parse(args||'[]')}));
ws.onmessage=e=>{const m=JSON.parse(e.data);if(m.t==='reply'&&m.id==='x'){console.log(JSON.stringify(m.error?{error:m.error}:m.result));process.exit(0)}};
ws.onerror=e=>{console.log('{"down":true}');process.exit(1)};
setTimeout(()=>{console.log('{"timeout":true}');process.exit(1)},4000);
