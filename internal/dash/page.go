package dash

// pageTemplate is the dashboard page. Live template actions are .Image and
// .State; everything else is plain HTML/CSS/JS.
const pageTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CCFLZ 懒加载面板 · {{.Image}}</title>
<style>
:root{
  --bg:#0d1117; --panel:#161b22; --panel2:#1c2330; --line:#30363d;
  --text:#e6edf3; --muted:#8b949e; --accent:#39d353; --accent2:#58a6ff;
  --warn:#d29922; --bad:#f85149; --header:#79c0ff;
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
header{padding:14px 20px;border-bottom:1px solid var(--line);display:flex;gap:16px;align-items:center;flex-wrap:wrap}
header h1{font-size:16px;margin:0;font-weight:600}
header .ref{color:var(--muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
button{background:var(--panel2);color:var(--text);border:1px solid var(--line);border-radius:6px;padding:6px 12px;cursor:pointer}
button:hover{border-color:var(--accent2)}
button.primary{border-color:var(--accent);color:var(--accent)}
main{padding:16px 20px;display:grid;grid-template-columns:1fr 1fr;gap:16px}
section{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px}
section h2{font-size:13px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);margin:0 0 12px}
section.wide{grid-column:1/-1}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.kv{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:10px}
.kv div{background:var(--panel2);border-radius:8px;padding:10px}
.kv .k{color:var(--muted);font-size:11px;text-transform:uppercase;letter-spacing:.05em}
.kv .v{font-size:20px;font-weight:600;margin-top:2px}
.kv .v small{font-size:12px;color:var(--muted);font-weight:400}
.layer{margin-bottom:16px}
.layer-head{display:flex;justify-content:space-between;gap:10px;font-size:12px;color:var(--muted);margin-bottom:6px;flex-wrap:wrap}
.bar{position:relative;height:34px;background:#0a0d12;border:1px solid var(--line);border-radius:6px;overflow:hidden}
.seg{position:absolute;top:0;height:100%;border-right:1px solid rgba(0,0,0,.45)}
.seg.header{background:#3b2f73}
.seg.data{background:#20324d}
.seg.trailer{background:#3d3d3d}
.seg.index{background:#6b4b00}
.seg.fetched.header{background:#7c5cff}
.seg.fetched.data{background:var(--accent)}
.seg.fetched.trailer{background:#8b949e}
.seg.fetched.index{background:var(--warn)}
.seg:hover{outline:2px solid #fff;z-index:5}
.legend{display:flex;gap:14px;font-size:11px;color:var(--muted);margin-top:6px;flex-wrap:wrap}
.legend i{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:4px;vertical-align:-1px}
.tree{max-height:360px;overflow:auto;font-family:ui-monospace,Menlo,monospace;font-size:12px}
.tree details{padding-left:4px}
.tree summary{cursor:pointer;padding:2px 4px;border-radius:4px;white-space:nowrap}
.tree summary:hover{background:var(--panel2)}
.tree .file{display:flex;justify-content:space-between;gap:12px;padding:2px 4px 2px 20px;color:var(--muted);border-radius:4px}
.tree .file:hover{background:var(--panel2);color:var(--text)}
.tree .file .pct{color:var(--accent)}
table{width:100%;border-collapse:collapse;font-size:12px;font-family:ui-monospace,Menlo,monospace}
th{color:var(--muted);text-align:left;font-weight:500;padding:4px 6px;border-bottom:1px solid var(--line);position:sticky;top:0;background:var(--panel)}
td{padding:3px 6px;border-bottom:1px solid #1f2630}
.scroll{max-height:300px;overflow:auto}
.tag{display:inline-block;padding:0 6px;border-radius:10px;font-size:11px}
.tag.cold{background:#3b2f73;color:#c9b8ff}
.tag.warm{background:#1b3b25;color:var(--accent)}
.pulse{animation:pulse 1.2s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.55}}
footer{padding:8px 20px;color:var(--muted);font-size:11px}
.err{color:var(--bad)}
</style>
</head>
<body>
<header>
  <h1>CCFLZ 按需懒加载</h1>
  <span class="ref">{{.Image}}</span>
  <button class="primary" onclick="act('/api/start')">启动容器</button>
  <button onclick="act('/api/prefetch')">预热热点块</button>
  <button onclick="act('/api/reset')">清空请求记录</button>
  <span id="status" class="ref"></span>
</header>
<main>
  <section class="wide">
    <h2>启动阶段</h2>
    <div class="kv" id="startup"></div>
  </section>

  <section class="wide">
    <h2>镜像层与取块分布</h2>
    <div id="layers"></div>
    <div class="legend">
      <span><i style="background:#3b2f73"></i>头部段</span>
      <span><i style="background:#20324d"></i>数据段</span>
      <span><i style="background:#3d3d3d"></i>归档结尾</span>
      <span><i style="background:#6b4b00"></i>索引</span>
      <span><i style="background:#39d353"></i>已取回(解压驻留)</span>
      <span><i style="background:#7c5cff"></i>已取回头部</span>
      <span>悬停看块的偏移与长度</span>
    </div>
  </section>

  <section>
    <h2>文件目录</h2>
    <div class="tree" id="tree"></div>
  </section>

  <section>
    <h2>范围请求记录</h2>
    <div class="scroll">
      <table>
        <thead><tr><th>时间</th><th>层</th><th>偏移</th><th>长度</th><th>字节</th></tr></thead>
        <tbody id="reqs"></tbody>
      </table>
    </div>
  </section>
</main>
<footer id="foot">数据每 1.5 秒刷新；启动阶段与下载比例均为本次会话实测。</footer>

<script>
const state = JSON.parse({{.State}});
let timer = null;

function fmtBytes(n){
  if(n==null) return "-";
  if(n<1024) return n+" B";
  if(n<1024*1024) return (n/1024).toFixed(1)+" KiB";
  return (n/1048576).toFixed(2)+" MiB";
}
function esc(s){return String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));}

function kv(items){
  return items.map(([k,v,extra])=>` + "`" + `<div><div class="k">${esc(k)}</div><div class="v">${v}${extra?` + "`" + ` <small>${extra}</small>` + "`" + `:''}</div></div>` + "`" + `).join('');
}

function renderStartup(st){
  const cold = st.cold||{}, warm = st.warm||{};
  const last = (st.startups||[]).slice(-1)[0]||{};
  const items = [
    ['冷启动就绪', (cold.ready_ms||0).toFixed(0), 'ms'],
    ['冷启动索引耗时', (cold.index_ms||0).toFixed(0), 'ms'],
    ['冷启动下载', fmtBytes(cold.bytes||0), (cold.fraction*100||0).toFixed(1)+'% 的层'],
    ['热启动就绪', (warm.ready_ms||0).toFixed(0), 'ms'],
    ['热启动下载', fmtBytes(warm.bytes||0), (warm.fraction*100||0).toFixed(1)+'% 的层'],
    ['最近一次', (last.ready_ms||0).toFixed(0), 'ms'],
  ];
  document.getElementById('startup').innerHTML = kv(items);
}

function renderLayers(st){
  const host = document.getElementById('layers');
  host.innerHTML = (st.layers||[]).map(L=>{
    const segs = (L.segments||[]).map(s=>{
      const cls = ['seg',s.kind,s.fetched?'fetched':''].join(' ');
      const tip = ` + "`" + `${s.kind} #${s.id} · 压缩偏移 ${s.off} · 压缩长 ${fmtBytes(s.comp_len)} · 解压长 ${fmtBytes(s.len)} · 取用 ${s.heat} 次` + "`" + `;
      return ` + "`" + `<div class="${cls}" style="left:${(s.top*100).toFixed(4)}%;width:${(s.width*100).toFixed(4)}%" title="${esc(tip)}"></div>` + "`" + `;
    }).join('');
    return ` + "`" + `<div class="layer">
      <div class="layer-head">
        <span class="mono">${esc(L.short)} · ${fmtBytes(L.size)} · ${L.files} 个文件 · ${L.segments.length} 段</span>
        <span>已下载 <b>${L.downloaded_pct.toFixed(1)}%</b> · 驻留 ${fmtBytes(L.cached_bytes)} · 引用 ${L.refs}${L.closed?' · 已释放':''} · 合并附加 ${fmtBytes(L.coalesced_hole_bytes)} · 校验失败 ${L.verify_failures}</span>
      </div>
      <div class="bar">${segs}</div>
    </div>` + "`" + `;
  }).join('') || '<div class="err">没有层数据</div>';
}

function buildTree(files){
  const root = {kids:new Map(),file:null};
  for(const f of files){
    const parts = f.path.split('/').filter(Boolean);
    let node = root;
    for(let i=0;i<parts.length;i++){
      const last = i===parts.length-1;
      if(last){ node.kids.set(parts[i],{kids:new Map(),file:f}); }
      else {
        let child = node.kids.get(parts[i]);
        if(!child){ child={kids:new Map(),file:null}; node.kids.set(parts[i],child); }
        node = child;
      }
    }
  }
  return root;
}
function renderTree(node, depth){
  const keys = [...node.kids.keys()].sort();
  let html='';
  for(const k of keys){
    const child = node.kids.get(k);
    if(child.file){
      const pct = child.file.chunks? Math.round(100*child.file.fetched_chunks/child.file.chunks):100;
      html += ` + "`" + `<div class="file"><span>${esc(k)}</span><span>${fmtBytes(child.file.size)} <span class="pct">${pct}%</span></span></div>` + "`" + `;
    }else{
      html += ` + "`" + `<details ${depth<2?'open':''}><summary>${esc(k)}/</summary>${renderTree(child,depth+1)}</details>` + "`" + `;
    }
  }
  return html;
}

function renderReqs(st){
  const rows = (st.requests||[]).slice(0,80).map(r=>{
    const t = new Date(r.at);
    return ` + "`" + `<tr><td>${t.toLocaleTimeString()}.${String(t.getMilliseconds()).padStart(3,'0')}</td><td>${esc((r.digest||'').slice(7,15))}</td><td>${r.off}</td><td>${r.len}</td><td>${fmtBytes(r.bytes)}</td></tr>` + "`" + `;
  }).join('');
  document.getElementById('reqs').innerHTML = rows || '<tr><td colspan="5" class="err">暂无请求</td></tr>';
}

function render(st){
  renderStartup(st);
  renderLayers(st);
  renderTreeInto(st);
  renderReqs(st);
  document.getElementById('foot').textContent = st.note || '';
}
function renderTreeInto(st){
  const tree = buildTree(st.files||[]);
  document.getElementById('tree').innerHTML = renderTree(tree,0) || '<div class="err">还没有文件</div>';
}

async function refresh(){
  try{
    const r = await fetch('/api/state',{cache:'no-store'});
    if(!r.ok) throw new Error(await r.text());
    render(await r.json());
    document.getElementById('status').textContent = '已刷新 '+new Date().toLocaleTimeString();
    document.getElementById('status').classList.remove('err');
  }catch(e){
    const el=document.getElementById('status'); el.textContent='刷新失败: '+e.message; el.classList.add('err');
  }
}
async function act(path){
  try{
    const r = await fetch(path,{method:'POST'});
    if(!r.ok) throw new Error(await r.text());
    await refresh();
  }catch(e){
    const el=document.getElementById('status'); el.textContent='操作失败: '+e.message; el.classList.add('err');
  }
}
render(state);
setInterval(refresh,1500);
</script>
</body>
</html>
`
