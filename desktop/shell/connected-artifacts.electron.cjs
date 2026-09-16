'use strict';

// Run with node --test. The child is a real, disposable Electron app; it never
// opens either Workass profile or loads credentials from those profiles.
const assert = require('node:assert/strict');
const path = require('node:path');
const fs = require('node:fs');
const os = require('node:os');

if (!process.argv.includes('--child')) {
  const { spawn } = require('node:child_process');
  require('node:test')('private bridge in real Electron: frames, relative assets and external denial', async (t) => {
    const executable = path.resolve(__dirname, '../../.dev/runtime/electron/darwin-arm64/Electron.app/Contents/MacOS/Electron');
    assert.ok(fs.existsSync(executable), 'canonical dev Electron runtime is required');
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-artifact-electron-'));
    t.after(() => fs.rmSync(root, { recursive: true, force: true }));
    const env = { ...process.env, WORKASS_ARTIFACT_TEST_ROOT: root };
    delete env.ELECTRON_RUN_AS_NODE;
    const child = spawn(executable, [__filename, '--child'], { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let output = '';
    child.stdout.on('data', (chunk) => { output += chunk; });
    child.stderr.on('data', (chunk) => { output += chunk; });
    const code = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error(`Electron timeout\n${output}`)); }, 25000);
      child.once('error', reject);
      child.once('exit', (value) => { clearTimeout(timer); resolve(value); });
    });
    assert.equal(code, 0, output);
    assert.match(output, /ARTIFACT_ELECTRON_PASS/);
  });
} else {
  const { app, BrowserWindow, ipcMain, session } = require('electron');
  const http = require('node:http');
  const { createConnectedArtifactBridge, shouldInjectArtifactHeader } = require('./connected-artifacts');
  app.setPath('userData', process.env.WORKASS_ARTIFACT_TEST_ROOT);
  void app.whenReady().then(async () => {
    const artifactHTML = `<!doctype html><link rel="stylesheet" href="style.css"><img id="icon" src="icon.svg"><button id="test" onclick="this.textContent='passed'">test</button><script>window.onload=()=>parent.postMessage({fixture:true,color:getComputedStyle(document.body).backgroundColor,image:document.querySelector('img').naturalWidth},'*')</script>`;
    const files = {
      '/workass/artifacts/a/index.html': ['text/html', artifactHTML],
      '/workass/artifacts/a/style.css': ['text/css', 'body { background: rgb(230, 245, 235); }'],
      '/workass/artifacts/a/icon.svg': ['image/svg+xml', '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16"><circle cx="8" cy="8" r="7" fill="green"/></svg>'],
    };
    const csp = "sandbox allow-scripts allow-forms allow-downloads; connect-src https:; frame-ancestors 'self'";
    const bootstrap = `<script>
      const files=${JSON.stringify(files).replaceAll('<', '\\u003c')};
      let serial=0; const transfers=new Map(); window.fixtureResult=null;
      addEventListener('message',event=>{if(event.data?.fixture)window.fixtureResult=event.data;});
      window.workassArtifacts.onRequest(r=>{
        let reply={requestId:r.requestId,ok:true};
        if(r.op==='open'){
          const file=files[r.path]; const id=String(++serial); transfers.set(id,file?.[1]||'missing');
          Object.assign(reply,{transferId:id,status:file?200:404,headers:{'Content-Type':file?.[0]||'text/plain','Content-Security-Policy':${JSON.stringify(csp)}}});
        } else if(r.op==='read') Object.assign(reply,{bodyBase64:btoa(transfers.get(r.transferId)||''),eof:true});
        else transfers.delete(r.transferId);
        window.workassArtifacts.reply(reply);
      });
    </script>`;
    let bridge;
    const server = http.createServer((req,res)=> {
      if (req.url === '/') { res.setHeader('Content-Type','text/html'); res.end(bootstrap); }
      else void bridge.handle(req,res);
    });
    const external = http.createServer((_req,res)=>{res.setHeader('Content-Type','text/html');res.end('<p>external</p>');});
    await Promise.all([new Promise(r=>server.listen(0,'127.0.0.1',r)),new Promise(r=>external.listen(0,'127.0.0.1',r))]);
    const origin = `http://127.0.0.1:${server.address().port}`;
    const target = `${origin}/workass/connected-artifacts/m/a/index.html`;
    const main = new BrowserWindow({show:false,webPreferences:{contextIsolation:true,sandbox:true,preload:path.join(__dirname,'preload.js')}});
    const guest = new BrowserWindow({show:false,webPreferences:{contextIsolation:true,sandbox:true}});
    bridge = createConnectedArtifactBridge({win:main,viewServer:{url:origin},getOwnedWebContents:()=>[guest.webContents]});
    ipcMain.handle('workass-artifact:reply',(event,payload)=>bridge.reply(event,payload));
    let navigation = '';
    session.defaultSession.webRequest.onBeforeSendHeaders((details,callback)=>{
      const headers={...details.requestHeaders};
      for(const key of Object.keys(headers))if(key.toLowerCase()===bridge.accessHeader)delete headers[key];
      if(shouldInjectArtifactHeader(details,{origin,targetURL:details.url,owned:bridge.owned,authorizedNavigation:(wc,url)=>{
        if(wc!==guest.webContents||navigation!==url)return false;
        navigation=''; return true;
      }}))headers[bridge.accessHeader]=bridge.capability;
      if(details.url.includes('/connected-artifacts/')) console.log('fixture-request',details.resourceType,details.frame?.url,details.frame?.parent?.url,!!headers[bridge.accessHeader]);
      callback({requestHeaders:headers});
    });
    await main.loadURL(origin);
    assert.equal((await fetch(target)).status,403);
    navigation=target;
    await guest.loadURL(target);
    const loaded=await guest.webContents.executeJavaScript(`({text:document.body.innerText,color:getComputedStyle(document.body).backgroundColor,image:document.querySelector('img')?.naturalWidth})`);
    assert.equal(loaded.color,'rgb(230, 245, 235)',JSON.stringify(loaded));
    assert.equal(loaded.image,16);
    assert.equal(await guest.webContents.executeJavaScript(`document.getElementById('test').click();document.getElementById('test').textContent`),'passed');
    await main.webContents.executeJavaScript(`new Promise((resolve,reject)=>{const frame=document.createElement('iframe');frame.sandbox='allow-scripts';frame.src=${JSON.stringify(target)};frame.onload=resolve;frame.onerror=reject;document.body.append(frame);})`);
    const iframe=await main.webContents.executeJavaScript('window.fixtureResult');
    assert.equal(iframe?.color,'rgb(230, 245, 235)',JSON.stringify(iframe));
    assert.equal(iframe?.image,16);
    await guest.loadURL(`http://127.0.0.1:${external.address().port}`);
    const denied=await guest.webContents.executeJavaScript(`fetch(${JSON.stringify(target)}).then(r=>r.status).catch(()=>0)`);
    assert.ok(denied===0||denied===403);
    console.log('ARTIFACT_ELECTRON_PASS');
    bridge.close(); main.destroy(); guest.destroy();
    server.closeAllConnections(); external.closeAllConnections(); server.close(); external.close();
    app.exit(0);
  }).catch(error=>{console.error(error.stack);app.exit(1);});
}
