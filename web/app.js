/* Emby 元数据编辑器 · 前端逻辑 */
'use strict';

// ---------------- 基础 ----------------
const $ = (sel, root) => (root || document).querySelector(sel);
const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));
const esc = (s) => String(s == null ? '' : s).replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// 页面里的外部图片统一走后端 /api/img：
// 白名单图床（javbus 等有 Referer 防盗链）由服务端带 Referer 代取，
// 其余公网图床服务端直接 302 回原地址，效果与直连一致。
// 哪些主机需要代取的策略只写在服务端，前端不重复判断。
function imgSrc(url) {
  if (!url) return '';
  let u;
  try { u = new URL(url, location.href); } catch (_) { return ''; }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return '';
  return '/api/img?u=' + encodeURIComponent(u.href);
}

const S = {
  cfg: null,
  view: 'stats',
  libs: [],
  lb: { start: 0, limit: 24, total: 0, items: [] },
  ps: { start: 0, limit: 48, total: 0, items: [] },
  // prof 是「演员资料」面板的界面状态。sel 用 Set 存勾选的源，
  // 重新渲染（切换视图 / 重查列表）不会丢勾选。
  // cur 是抽屉当前正在看的演员（未抓取态也要知道是谁，好去读本地作品列表）。
  // works 是抽屉里「媒体库作品」那一块：单独加载、单独分页，不参与资料抓取。
  prof: {
    sources: [], sel: new Set(), aliasN: 0, loaded: false,
    cur: { personId: '', name: '' },
    works: { personId: '', items: [], total: 0, limit: 60, ready: false },
  },
  jb: { scan: null, selected: new Set(), magnets: [], targets: [], magTab: '' },
  watching: {},
};

// AUTH_API 是认证自己的接口：它们必须自己处理 401（api() 遇到 401 会去拉登录门，
// 若在这里也走同一条路就会自己套自己）。
const AUTH_API = ['/api/auth/status', '/api/auth/login', '/api/auth/logout', '/api/auth/password'];
const isAuthAPI = (p) => AUTH_API.indexOf(p) >= 0;

// ---------------- 请求封装 ----------------
async function api(path, opts) {
  const o = Object.assign({ headers: {} }, opts || {});
  if (o.body && typeof o.body !== 'string') {
    o.headers['Content-Type'] = 'application/json';
    o.body = JSON.stringify(o.body);
  }
  const res = await fetch(path, o);
  let json = null;
  try { json = await res.json(); } catch (e) { /* 非 JSON */ }
  // 401 = 会话过期或没登录（后端 guard 中间件给的）。先把登录门拉回来再抛错，
  // 否则用户只会看到一串「请求失败：HTTP 401」，不知道要去登录。
  if (res.status === 401 && !isAuthAPI(path)) {
    showAuthGate('登录状态已失效，请重新登录');
    throw new Error('请先登录');
  }
  if (!res.ok || (json && json.ok === false)) {
    const msg = (json && json.error) || ('请求失败：HTTP ' + res.status);
    throw new Error(msg);
  }
  return json ? json.data : null;
}

function toast(msg, kind) {
  const el = document.createElement('div');
  el.className = 'toast ' + (kind || 'info');
  el.textContent = msg;
  $('#toasts').appendChild(el);
  setTimeout(() => { el.style.opacity = '0'; el.style.transition = '.3s'; }, 3600);
  setTimeout(() => el.remove(), 4100);
}

const num = (n) => (n == null ? '0' : Number(n).toLocaleString('zh-CN'));

// ---------------- 访问认证 ----------------
// 这是**进程自己的**登录（保护「谁能打开这个界面」），和 Emby 登录是两回事：
// 后端 guard 中间件会拦住所有未登录的 /api/ 请求，所以没登录时这个页面
// 拿不到任何数据，能做的只有登录。
function showAuthGate(msg) {
  $('#app').classList.add('hidden');
  $('#login').classList.add('hidden');
  $('#authGate').classList.remove('hidden');
  const box = $('#agAlert');
  if (msg) { box.textContent = msg; box.classList.remove('hidden'); }
  else { box.classList.add('hidden'); }
}

// authStatus 用裸 fetch：它必须在「还没登录」的状态下就能调通，
// 不能走 api()（那个遇到 401 会去拉登录门，会自己套自己）。
async function authStatus() {
  const res = await fetch('/api/auth/status', { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error('HTTP ' + res.status);
  const j = await res.json();
  if (!j || j.ok === false) throw new Error((j && j.error) || '无法获取登录状态');
  return j.data;
}

async function doAuthLogin() {
  const btn = $('#agBtn');
  const box = $('#agAlert');
  btn.disabled = true;
  btn.innerHTML = '<span class="spin"></span> 登录中…';
  box.classList.add('hidden');
  try {
    await api('/api/auth/login', {
      method: 'POST',
      body: { username: $('#agUser').value.trim(), password: $('#agPass').value },
    });
    $('#agPass').value = '';
    $('#authGate').classList.add('hidden');
    await startApp();
  } catch (e) {
    box.textContent = e.message;
    box.classList.remove('hidden');
  } finally {
    btn.disabled = false;
    btn.textContent = '登录';
  }
}

async function doAuthLogout() {
  try { await api('/api/auth/logout', { method: 'POST' }); } catch (e) { /* 会话可能已失效 */ }
  S.cfg = null;
  $('#app').classList.add('hidden');
  showAuthGate('已退出登录');
}

// saveAuth 改访问用户名 / 密码。旧密码必填（拿到别人没锁屏的浏览器也不能直接换掉）。
async function saveAuth() {
  const btn = $('#stAuthSave');
  const msg = $('#stAuthMsg');
  btn.disabled = true;
  msg.textContent = '保存中…';
  msg.style.color = '';
  try {
    await api('/api/auth/password', {
      method: 'POST',
      body: {
        username: $('#stAuthUser').value.trim(),
        old_password: $('#stAuthOld').value,
        new_password: $('#stAuthNew').value,
      },
    });
    $('#stAuthOld').value = '';
    $('#stAuthNew').value = '';
    await loadConfig();
    fillSettings();
    msg.textContent = '已保存，其他设备上的登录已失效';
    msg.style.color = 'var(--success)';
    toast('访问凭据已更新', 'ok');
  } catch (e) {
    msg.textContent = e.message;
    msg.style.color = 'var(--danger)';
  } finally {
    btn.disabled = false;
  }
}

// markSecret 处理「值不下发前端」的输入框。
// 后端不再把已保存的密钥发给页面，所以输入框只能是空的 ——
// 留空提交 = 不修改（这是后端的规则），框里换成提示语说明这一点。
function markSecret(id, saved) {
  const el = $(id);
  if (!el) return;
  if (el.dataset.ph == null) el.dataset.ph = el.placeholder || '';
  el.value = '';
  el.placeholder = saved ? '已保存，留空则不修改' : el.dataset.ph;
  el.classList.toggle('saved', !!saved);
}

// ---------------- Emby 图片地址 ----------------
// 图片统一由服务端带着令牌去 Emby 取（/api/emby/image）。
// 原来是直接把 token 拼进 <img src> 的 api_key 参数里 ——
// 那等于把 Emby 的管理员权限印在 DOM 上，截图、复制图片地址都会带出去；
// 顺带还解决了「页面是 http、Emby 是 https 自签证书」被浏览器拦掉的问题。
function embyImg(itemId, tag, h) {
  if (!itemId || !tag) return '';
  return '/api/emby/image?id=' + encodeURIComponent(itemId) +
    '&type=Primary&h=' + (h || 300) + '&tag=' + encodeURIComponent(tag);
}
function personImg(p, h) {
  const tag = p.ImageTags && p.ImageTags.Primary;
  return embyImg(p.Id, tag, h || 120);
}

// ---------------- 任务面板 ----------------
const JOB_TITLES = {};

function ensureJobPanel() {
  if ($('#jobPanel')) return;
  const el = document.createElement('div');
  el.className = 'jobpanel';
  el.id = 'jobPanel';
  el.innerHTML = '<div class="head"><span class="spin"></span><b id="jobTitle">任务</b>' +
    '<button class="btn btn-sm btn-ghost" id="jobCancel">取消</button>' +
    '<button class="btn btn-sm btn-ghost" id="jobClose">×</button></div>' +
    '<div class="bar"><i id="jobBar" style="width:0"></i></div>' +
    '<div class="logs" id="jobLogs"></div>';
  $('#jobHost').appendChild(el);
  $('#jobClose').onclick = () => el.remove();
}

function watchJob(jobId, title, onDone) {
  ensureJobPanel();
  const panel = $('#jobPanel');
  $('#jobTitle').textContent = title || '任务';
  $('#jobLogs').innerHTML = '';
  $('#jobBar').style.width = '0%';
  $('#jobCancel').onclick = async () => {
    try { await api('/api/jobs/' + jobId + '/cancel', { method: 'POST' }); toast('已请求取消', 'info'); }
    catch (e) { toast(e.message, 'err'); }
  };
  if (S.watching[jobId]) return;
  S.watching[jobId] = true;
  let lastLog = 0;
  const tick = async () => {
    let j;
    try { j = await api('/api/jobs/' + jobId); }
    catch (e) { delete S.watching[jobId]; return; }
    const logs = $('#jobLogs');
    if (logs && j.logs) {
      for (let i = lastLog; i < j.logs.length; i++) {
        const l = j.logs[i];
        const d = document.createElement('div');
        d.className = 'lv-' + (l.level || 'info');
        const t = new Date(l.time);
        d.innerHTML = '<span class="t">' + t.toTimeString().slice(0, 8) + '</span><span class="m">' + esc(l.message) + '</span>';
        logs.appendChild(d);
      }
      lastLog = j.logs.length;
      logs.scrollTop = logs.scrollHeight;
    }
    if ($('#jobBar')) $('#jobBar').style.width = (j.percent || 0) + '%';
    if ($('#jobTitle')) {
      $('#jobTitle').textContent = (title || '任务') +
        '　' + j.done + '/' + (j.total || '?') + (j.failed ? ' · 失败 ' + j.failed : '') +
        (j.skipped ? ' · 跳过 ' + j.skipped : '');
    }
    if (j.status === 'running') { setTimeout(tick, 700); return; }
    delete S.watching[jobId];
    const sp = $('#jobPanel .spin'); if (sp) sp.remove();
    const cb = $('#jobCancel'); if (cb) cb.textContent = j.status === 'done' ? '完成' : j.status;
    if (j.status === 'done') {
      if (j.error) toast(j.error, 'err'); else toast((title || '任务') + ' 已完成', 'ok');
      if (onDone) onDone(j);
    } else {
      toast((title || '任务') + '：' + (j.error || j.status), 'err');
      if (onDone) onDone(j);
    }
  };
  tick();
}

// ---------------- 视图切换 ----------------
const VIEW_TITLES = { stats: '概览统计', library: '媒体库刮削', persons: '演员头像', javbus: '番号补全', cn: '国产传媒', settings: '设置' };

function switchView(v) {
  S.view = v;
  $$('#nav button').forEach((b) => b.classList.toggle('on', b.dataset.view === v));
  $$('.view').forEach((s) => s.classList.toggle('on', s.id === 'view-' + v));
  $('#viewTitle').textContent = VIEW_TITLES[v] || v;
  if (v === 'stats') loadStats();
  if (v === 'library') { ensureLibs(); loadItems(0); }
  if (v === 'persons') { ensureLibs(); loadPersons(0); refreshGfState(); loadProfileSources(); }
  if (v === 'cn') { ensureLibs(); cnSyncSelUI(); }
  if (v === 'settings') fillSettings();
}

// ---------------- 概览 ----------------
async function loadStats() {
  $('#statGrid').innerHTML = '<div class="empty"><span class="spin"></span> 正在统计…</div>';
  $('#stLibs').innerHTML = '<div class="empty">加载中…</div>';
  try {
    const d = await api('/api/stats');
    const c = d.counts || {};
    const p = d.persons || {};
    const cards = [
      { k: '电影', v: num(c.MovieCount), c: 'accent' },
      { k: '剧集', v: num(c.SeriesCount), c: '' },
      { k: '集数', v: num(c.EpisodeCount), c: '' },
      { k: '演员总数', v: num(p.total), c: 'violet', s: '已扫描 ' + num(p.scanned) },
      { k: '缺头像演员', v: num(p.missing_image), c: 'warn' },
    ];
    $('#statGrid').innerHTML = cards.map((x) =>
      '<div class="stat ' + x.c + '"><div class="k">' + x.k + '</div><div class="v">' + x.v + '</div>' +
      (x.s ? '<div class="s">' + esc(x.s) + '</div>' : '') + '</div>').join('');

    const libs = d.libraries || [];
    if (!libs.length) {
      $('#stLibs').innerHTML = '<div class="empty">没有媒体库（可能未登录）</div>';
    } else {
      $('#stLibs').innerHTML = '<table class="tbl"><thead><tr><th>媒体库</th><th>类型</th>' +
        '<th class="num">条目</th><th class="num">电影</th><th class="num">剧集</th><th class="num">集</th>' +
        '</tr></thead><tbody>' +
        libs.map((l) => '<tr><td><b>' + esc(l.name) + '</b></td><td><span class="tag">' + esc(l.collection_type || 'mixed') + '</span></td>' +
          '<td class="num">' + num(l.item_count) + '</td><td class="num">' + num(l.movie_count) + '</td>' +
          '<td class="num">' + num(l.series_count) + '</td><td class="num">' + num(l.episode_count) + '</td>' +
          '</tr>').join('') +
        '</tbody></table>';
    }
    renderComponents(d);
  } catch (e) {
    $('#statGrid').innerHTML = '<div class="empty">加载失败：' + esc(e.message) + '</div>';
    $('#stLibs').innerHTML = '';
  }
}

function renderComponents(d) {
  const gf = d.gfriends || {};
  const rows = [
    ['Emby 服务器', S.cfg && S.cfg.emby_url ? esc(S.cfg.emby_url) : '未配置',
      S.cfg && S.cfg.logged_in ? 'green' : 'red', S.cfg && S.cfg.logged_in ? '已登录' : '未登录'],
    ['MetaTube', esc((S.cfg && S.cfg.metatube_url) || '未配置'), 'blue', '地址可配置'],
    ['gfriends 索引', gf.loaded ? num(gf.names) + ' 位演员 / ' + num(gf.total) + ' 张图' : '未加载',
      gf.loaded ? 'green' : 'amber', gf.last_error ? esc(gf.last_error) : (gf.loaded_at ? '更新于 ' + gf.loaded_at.replace('T', ' ').slice(0, 19) : '')],
    ['javbus', esc((S.cfg && S.cfg.javbus_url) || '未配置'), 'blue', 'Cookie 已配置'],
  ];
  $('#stComp').innerHTML = '<table class="tbl"><tbody>' + rows.map((r) =>
    '<tr><td style="width:130px;color:var(--muted)">' + r[0] + '</td><td>' + r[1] + '</td>' +
    '<td style="width:120px"><span class="tag tag-' + r[2] + '">' + r[3] + '</span></td></tr>').join('') + '</tbody></table>';
}

// ---------------- 媒体库 ----------------
async function ensureLibs() {
  if (S.libs.length) return;
  try {
    S.libs = await api('/api/libraries');
    const opts = S.libs.map((l) => '<option value="' + esc(l.Id) + '">' + esc(l.Name) + '</option>').join('');
    $('#lbLib').innerHTML = '<option value="">全部媒体库</option>' + opts;
    $('#jbLib').innerHTML = '<option value="">全部媒体库</option>' + opts;
    $('#psLib').innerHTML = '<option value="">全部媒体库</option>' + opts;
    // 国产传媒页不给「全部」这个选项：这个功能只在某个具体库里成立，
    // 留个「全部」等于留个坑。
    $('#cnLib').innerHTML = '<option value="">请选择媒体库</option>' + opts;
  } catch (e) { /* 未登录时忽略 */ }
}

async function loadItems(start) {
  const lb = S.lb;
  lb.start = start || 0;
  $('#lbGrid').innerHTML = '<div class="empty"><span class="spin"></span> 加载中…</div>';
  const params = new URLSearchParams({
    parent: $('#lbLib').value, q: $('#lbQ').value.trim(),
    start: lb.start, limit: lb.limit,
    missing_image: $('#lbNoPoster').checked ? 'true' : 'false',
  });
  try {
    const d = await api('/api/items?' + params.toString());
    lb.items = d.items || [];
    lb.total = d.total || 0;
    $('#lbCount').textContent = '共 ' + num(lb.total) + ' 条' + ($('#lbNoPoster').checked ? '（本页过滤后 ' + lb.items.length + ' 条）' : '');
    if (!lb.items.length) {
      $('#lbGrid').innerHTML = '<div class="empty">没有符合条件的条目</div>';
    } else {
      $('#lbGrid').innerHTML = lb.items.map(renderMovieCard).join('');
    }
    renderPager('#lbPager', lb, loadItems);
  } catch (e) {
    $('#lbGrid').innerHTML = '<div class="empty">加载失败：' + esc(e.message) + '</div>';
    $('#lbPager').innerHTML = '';
  }
}

function renderMovieCard(it) {
  const has = it.ImageTags && it.ImageTags.Primary;
  const img = has ? '<img loading="lazy" src="' + embyImg(it.Id, it.ImageTags.Primary, 300) + '" alt="">'
    : '<div class="ph">无海报</div>';
  const sortName = it.Number || '';
  const pid = (it.ProviderIds && it.ProviderIds.MetaTube) || '';
  return '<div class="mcard" data-id="' + esc(it.Id) + '">' +
    '<div class="poster">' + img +
    '<span class="badge' + (has ? '' : ' no') + '">' + esc(sortName || (it.ProductionYear || '—')) + '</span></div>' +
    '<div class="body"><div class="name" title="' + esc(it.Name) + '">' + esc(it.Name) + '</div>' +
    '<div class="meta">' + (it.ProductionYear || '') + (pid ? ' · ' + esc(pid) : '') + '</div>' +
    '<div class="acts">' +
    '<button class="btn btn-sm btn-primary" data-act="scrape">刮削</button>' +
    '<button class="btn btn-sm" data-act="detail">详情</button>' +
    '</div></div></div>';
}

function renderPager(sel, st, fn, note) {
  const el = $(sel);
  const from = st.start + 1;
  const to = Math.min(st.start + st.limit, st.total);
  const label = note || (st.total ? from + '–' + to + ' / ' + num(st.total) : '无数据');
  el.innerHTML = '<button class="btn btn-sm" data-p="prev"' + (st.start <= 0 ? ' disabled' : '') + '>上一页</button>' +
    '<span>' + label + '</span>' +
    '<button class="btn btn-sm" data-p="next"' + (to >= st.total ? ' disabled' : '') + '>下一页</button>';
  $$('button[data-p]', el).forEach((b) => b.onclick = () => {
    if (b.dataset.p === 'prev') fn(Math.max(0, st.start - st.limit));
    else fn(st.start + st.limit);
  });
}

async function scrapeOne(itemId, btn) {
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="spin"></span>'; }
  try {
    const r = await api('/api/items/scrape', {
      method: 'POST',
      body: {
        id: itemId,
        overwrite_images: $('#lbOverwrite').checked,
        refresh: $('#lbRefresh').checked,
      },
    });
    toast('刮削完成：' + (r.title || '') + '（' + (r.images || []).length + ' 张图片）', 'ok');
    loadItems(S.lb.start);
  } catch (e) {
    toast(e.message, 'err');
    if (btn) { btn.disabled = false; btn.textContent = '刮削'; }
  }
}

function openDrawer(html) {
  const host = $('#drawerHost');
  host.innerHTML = '<div class="drawer-mask"><div class="drawer">' + html + '</div></div>';
  $('.drawer-mask', host).onclick = (e) => { if (e.target.classList.contains('drawer-mask')) host.innerHTML = ''; };
  return host;
}

async function openItemDetail(itemId) {
  const host = openDrawer('<div class="empty"><span class="spin"></span> 加载中…</div>');
  let it;
  try { it = await api('/api/items/detail?id=' + encodeURIComponent(itemId)); }
  catch (e) { host.innerHTML = ''; toast(e.message, 'err'); return; }
  const kv = [
    ['名称', it.Name], ['原始标题', it.OriginalTitle], ['番号', it.Number],
    ['年份', it.ProductionYear], ['评分', it.CommunityRating],
    ['路径', it.Path], ['MetaTube', (it.ProviderIds || {}).MetaTube],
    ['简介', (it.Overview || '').slice(0, 220)],
  ].filter((x) => x[1]);
  const has = it.ImageTags && it.ImageTags.Primary;
  openDrawer(
    '<h3>' + esc(it.Name) + '</h3><div class="sub">' + esc(it.Path || it.Id) + '</div>' +
    (has ? '<img src="' + embyImg(it.Id, it.ImageTags.Primary, 420) + '" style="width:180px;border-radius:8px;border:1px solid var(--border);margin-bottom:16px">' : '') +
    '<div class="kv">' + kv.map((x) => '<div class="k">' + x[0] + '</div><div class="v">' + esc(x[1]) + '</div>').join('') + '</div>' +
    '<div class="card" style="box-shadow:none"><h3>MetaTube 手动匹配</h3>' +
    '<div class="row"><input class="input" id="dvQ" value="' + esc(it.Number || it.Name || '') + '">' +
    '<button class="btn" id="dvSearch" style="flex:none">搜索</button></div>' +
    '<div id="dvHits" style="margin-top:12px"></div>' +
    '<div style="display:flex;gap:8px;margin-top:14px">' +
    '<button class="btn btn-primary" id="dvAuto">自动刮削</button>' +
    '<label class="switch"><input type="checkbox" id="dvOw"> 覆盖图片</label></div></div>'
  );
  $('#dvAuto').onclick = async (e) => {
    e.target.disabled = true;
    try {
      const r = await api('/api/items/scrape', { method: 'POST', body: { id: itemId, overwrite_images: $('#dvOw').checked, refresh: true } });
      toast('已刮削：' + r.title, 'ok'); $('#drawerHost').innerHTML = ''; loadItems(S.lb.start);
    } catch (err) { toast(err.message, 'err'); e.target.disabled = false; }
  };
  const doSearch = async () => {
    const q = $('#dvQ').value.trim();
    if (!q) return;
    $('#dvHits').innerHTML = '<div class="empty"><span class="spin"></span> 搜索中…</div>';
    try {
      const list = await api('/api/metatube/search?q=' + encodeURIComponent(q));
      if (!list.length) { $('#dvHits').innerHTML = '<div class="empty">没有结果</div>'; return; }
      $('#dvHits').innerHTML = list.map((m, i) => '<div class="hit" data-i="' + i + '">' +
        (m.cover_url ? '<img loading="lazy" src="' + esc(imgSrc(m.cover_url)) + '">' : '<img>') +
        '<div class="i"><b>' + esc(m.title_zh || m.title || m.number) + '</b>' +
        '<span>' + esc(m.number) + ' · ' + esc(m.provider) + ':' + esc(m.id) + (m.score ? ' · ' + m.score : '') + '</span></div></div>').join('');
      $$('.hit', $('#dvHits')).forEach((h) => h.onclick = async () => {
        const m = list[Number(h.dataset.i)];
        h.style.opacity = '.5';
        try {
          const r = await api('/api/items/scrape', {
            method: 'POST',
            body: { id: itemId, provider: m.provider, movie_id: m.id, overwrite_images: $('#dvOw').checked, refresh: true },
          });
          toast('已应用：' + r.title, 'ok'); $('#drawerHost').innerHTML = ''; loadItems(S.lb.start);
        } catch (e) { toast(e.message, 'err'); h.style.opacity = '1'; }
      });
    } catch (e) { $('#dvHits').innerHTML = '<div class="empty">' + esc(e.message) + '</div>'; }
  };
  $('#dvSearch').onclick = doSearch;
  $('#dvQ').onkeydown = (e) => { if (e.key === 'Enter') doSearch(); };
}

// ---------------- 演员 ----------------
async function loadPersons(start) {
  const ps = S.ps;
  ps.start = start || 0;
  $('#psList').innerHTML = '<div class="empty"><span class="spin"></span> 加载中…</div>';
  const libOpt = $('#psLib').selectedOptions[0];
  const libName = libOpt ? libOpt.textContent : '全部媒体库';
  const onlyMissing = $('#psMissing').checked;
  const params = new URLSearchParams({
    q: $('#psQ').value.trim(), start: ps.start, limit: ps.limit,
    parent_id: $('#psLib').value,
    missing_image: onlyMissing ? 'true' : 'false',
  });
  try {
    const d = await api('/api/persons?' + params.toString());
    ps.items = d.items || [];
    ps.total = d.total || 0;
    if (!ps.items.length) {
      $('#psList').innerHTML = '<div class="empty">' +
        (onlyMissing ? '「' + esc(libName) + '」里没有缺头像的演员' : '没有符合条件的演员') + '</div>';
    } else {
      $('#psList').innerHTML = ps.items.map(renderPersonCard).join('');
    }
    renderPager('#psPager', ps, loadPersons,
      onlyMissing ? ('本页 ' + ps.items.length + ' 位无头像 / ' + esc(libName) + ' 共 ' + num(ps.total) + ' 位演员') : null);
  } catch (e) {
    $('#psList').innerHTML = '<div class="empty">加载失败：' + esc(e.message) + '</div>';
    $('#psPager').innerHTML = '';
  }
}

function renderPersonCard(p) {
  const img = p.has_image ? '<img loading="lazy" src="' + personImg(p, 120) + '">' : esc((p.Name || '?').slice(0, 1));
  // 已有头像的人，「刮削」按钮直接变成「重写」并带上强制覆盖 —— 这是用户明确要的能力：
  // 头像来源随时可能换（gfriends 收录了更清晰的版本），不必先去工具栏勾开关。
  const avLabel = p.has_image ? '重写头像' : '刮削头像';
  const avTitle = p.has_image ? '用 gfriends / MetaTube 的图替换掉当前头像' : '按当前来源设置抓一张头像';
  return '<div class="pcard" data-id="' + esc(p.Id) + '" data-name="' + esc(p.Name) + '">' +
    '<div class="av">' + img + '</div>' +
    '<div class="info"><b title="' + esc(p.Name) + '">' + esc(p.Name) + '</b>' +
    '<div class="sub">' + (p.has_image ? '<span class="tag tag-green">已有头像</span>'
      : (p.gfriends > 0 ? '<span class="tag tag-blue">gfriends 命中 ' + p.gfriends + '</span>' : '<span class="tag tag-amber">库中无记录</span>')) +
    '</div></div>' +
    '<div class="ops">' +
    '<button class="btn btn-sm btn-primary" data-act="av" title="' + esc(avTitle) + '">' + esc(avLabel) + '</button>' +
    '<button class="btn btn-sm" data-act="pick" title="从 gfriends 头像库里挑一张">选图</button>' +
    '<button class="btn btn-sm" data-act="prof" title="抓取简介 / 出生日期 / 出生地 / 外部 ID">资料</button>' +
    '</div></div>';
}

async function scrapeAvatar(personId, name, btn, force) {
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="spin"></span>'; }
  try {
    const r = await api('/api/persons/avatar', {
      method: 'POST',
      body: {
        person_id: personId, name: name,
        source: $('#psSource').value,
        overwrite: force === true ? true : $('#psOverwrite').checked,
      },
    });
    toast(name + '：' + r.message, 'ok');
    loadPersons(S.ps.start);
  } catch (e) {
    toast(name + '：' + e.message, 'err');
    if (btn) { btn.disabled = false; btn.innerHTML = '重写头像'; }
  }
}

// gfFileName 从搜索接口下发的完整 CDN 地址里取回裸文件名，只用于提示文案。
function gfFileName(url) {
  try {
    const p = decodeURIComponent(new URL(url, location.href).pathname);
    return p.slice(p.lastIndexOf('/') + 1);
  } catch (_) { return String(url || ''); }
}

// ---------------- 图片尺寸 / 体积 ----------------
//
// 同一个演员常有多张候选头像，缩略图都缩到同样大小，肉眼分不出哪张更清晰，
// 所以把「宽×高 · 体积」标在图下面。两者都只能由服务端代取（浏览器拿不到
// 一张图的字节数，跨域 fetch 又被 CSP 的 connect-src 'self' 挡着），
// 所以走 /api/img/info —— 顺带把图写进服务端缓存，缩略图随后秒开。

// fmtBytes 把字节数写成 KB / MB，用于候选图那一行小字。
function fmtBytes(n) {
  if (!n || n < 0) return '';
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(n < 10240 ? 1 : 0) + ' KB';
  return (n / 1048576).toFixed(2) + ' MB';
}

// imgMetaText 把探测结果拼成候选图下面那行小字。
// 三种「没有数据」要分得清：没探到、探了但取不到、取到了但读不出像素，
// 一律写「—」的话用户会以为是自己没搜到图。
function imgMetaText(it) {
  if (!it) return '尺寸未知';
  if (!it.ok) return '读不到大小';
  const dim = (it.width && it.height) ? (it.width + '×' + it.height) : '尺寸未知';
  return dim + (it.bytes ? ' · ' + fmtBytes(it.bytes) : '');
}

// loadImageInfo 批量探测。返回 url -> 结果 的 Map，失败的整批不抛错（图照样显示）。
async function loadImageInfo(urls) {
  const uniq = Array.from(new Set((urls || []).filter(Boolean)));
  const m = new Map();
  if (!uniq.length) return m;
  try {
    const d = await api('/api/img/info', { method: 'POST', body: { urls: uniq } });
    (d && d.items ? d.items : []).forEach((it) => m.set(it.url, it));
  } catch (_) { /* 探测失败只是少了那行小字，不该影响挑图 */ }
  return m;
}

async function pickAvatar(personId, name) {
  const host = openDrawer('<h3>' + esc(name) + '</h3><div class="sub">从 gfriends 头像库挑选一张</div>' +
    '<div class="field"><div class="row"><input class="input" id="pkQ" value="' + esc(name) + '">' +
    '<button class="btn" id="pkGo" style="flex:none">搜索</button></div></div>' +
    '<div id="pkList"><div class="empty"><span class="spin"></span> 加载中…</div></div>');
  const render = async () => {
    const q = $('#pkQ').value.trim();
    try {
      const hits = await api('/api/gfriends/search?q=' + encodeURIComponent(q));
      if (!hits.length) { $('#pkList').innerHTML = '<div class="empty">没有找到</div>'; return; }
      $('#pkList').innerHTML = hits.map((h) => '<div style="margin-bottom:14px"><div style="font-size:12px;color:var(--muted);margin-bottom:6px">' +
        esc(h.name) + '（' + h.entries.length + ' 张）</div><div class="pk-grid">' +
        h.entries.map((en) => '<div class="pk-item">' +
          '<img loading="lazy" data-name="' + esc(h.name) + '" data-file="' + esc(en.f) + '" data-group="' + esc(en.g) + '" ' +
          'data-url="' + esc(en.f) + '" src="' + esc(imgSrc(en.f)) + '" alt="" ' +
          'title="' + esc(en.gz + ' / ' + gfFileName(en.f)) + '">' +
          '<span class="pk-meta">…</span></div>').join('') +
        '</div></div>').join('');
      const imgs = $$('#pkList img');
      imgs.forEach((im) => im.onclick = async () => {
        im.style.opacity = '.4';
        try {
          const r = await api('/api/persons/avatar', {
            method: 'POST',
            body: { person_id: personId, name: im.dataset.name, source: 'gfriends', group: im.dataset.group, file: im.dataset.file, overwrite: true },
          });
          toast(r.message, 'ok'); $('#drawerHost').innerHTML = ''; loadPersons(S.ps.start);
        } catch (e) { toast(e.message, 'err'); im.style.opacity = '1'; }
      });
      // 尺寸与体积是异步探的（要服务端去取原图），所以先出图、后补小字：
      // 否则用户得等几十张图全探完才能看到缩略图。
      loadImageInfo(imgs.map((im) => im.dataset.url)).then((info) => {
        imgs.forEach((im) => {
          const el = $('.pk-meta', im.parentElement);
          if (el) el.textContent = imgMetaText(info.get(im.dataset.url));
        });
      });
    } catch (e) { $('#pkList').innerHTML = '<div class="empty">' + esc(e.message) + '</div>'; }
  };
  $('#pkGo').onclick = render;
  $('#pkQ').onkeydown = (e) => { if (e.key === 'Enter') render(); };
  render();
}

async function refreshGfState() {
  try {
    const st = await api('/api/gfriends/status');
    $('#gfState').textContent = st.loaded ? ('已加载 ' + num(st.names) + ' 位演员') : (st.loading ? '加载中…' : '未加载');
    $('#gfState').className = 'tag ' + (st.loaded ? 'tag-green' : 'tag-amber');
  } catch (e) { /* ignore */ }
}

// ---------------- 演员资料 ----------------
// 抓取与写回策略全在服务端（actorprofile.go / profile.go），前端只做展示与勾选。
// 界面上必须**并排显示「Emby 现有值 vs 本次抓取值」**：「只填空白」这条策略如果不可见，
// 用户看到「没写入」会以为是抓取失败，而不是被正确跳过了。
// 只列服务端真正会写的字段。**这里没有 tags**：实测这个 Emby 构建对 Person
// 不保存 Tags（POST 返回 204，但任何接口都读不回来），所以服务端把各源的标签
// 并进了简介，不再单列一个「写不进去」的字段来骗人。
const PROFILE_FIELD_HINT = {
  overview: '写入 Emby 的 Overview（各源的标签也会并成最后一行）',
  premiere_date: '写入 PremiereDate（出生日期）',
  production_year: '跟出生日期一起写入 ProductionYear',
  production_locations: '写入 ProductionLocations',
  provider_ids: '写入 ProviderIds，key 用来源名',
};

// safeLink 只放行 http(s)，避免把服务端返回的字符串直接塞进 href。
function safeLink(url) {
  try { const u = new URL(url, location.href); return (u.protocol === 'http:' || u.protocol === 'https:') ? u.href : ''; }
  catch (_) { return ''; }
}

function fmtTime(s) {
  if (!s) return '';
  const d = new Date(s);
  if (isNaN(d.getTime())) return String(s);
  const p = (n) => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

async function loadProfileSources(force) {
  const box = $('#pfSources');
  if (!box) return;
  if (S.prof.loaded && !force) { renderProfileSources(); return; }
  try {
    const d = await api('/api/profile/sources');
    S.prof.sources = d.sources || [];
    // 首次加载默认全选；之后重载要保留用户已经取消掉的项（sel 是 Set，只往里加不覆盖）
    if (!S.prof.loaded) S.prof.sources.forEach((s) => S.prof.sel.add(s.key));
    S.prof.aliasN = d.alias_groups || 0;
    S.prof.loaded = true;
    renderProfileSources();
    if ($('#pfAliasN')) $('#pfAliasN').textContent = num(S.prof.aliasN);
  } catch (e) {
    box.innerHTML = '<span class="cnhint">资料源加载失败：' + esc(e.message) + '</span>';
  }
}

function renderProfileSources() {
  const box = $('#pfSources');
  if (!box) return;
  if (!S.prof.sources.length) { box.innerHTML = '<span class="cnhint">没有可用的资料源</span>'; return; }
  box.innerHTML = S.prof.sources.map((s) => '<label class="switch pf-src" title="' + esc(s.key) + '">' +
    '<input type="checkbox" data-key="' + esc(s.key) + '"' + (S.prof.sel.has(s.key) ? ' checked' : '') + '> ' +
    esc(s.label) + '</label>').join('');
  $$('#pfSources input').forEach((cb) => {
    cb.onchange = () => {
      if (cb.checked) S.prof.sel.add(cb.dataset.key); else S.prof.sel.delete(cb.dataset.key);
      syncProfileSourceUI();
    };
  });
  updateProfileState();
}

// syncProfileSourceUI 让「资料抽屉里那组源」和「侧栏那组源」始终一致。
//
// 两处改的是同一个 S.prof.sel：抽屉里取消了某个源，侧栏必须同步显示成取消，
// 否则用户会以为抽屉里的选择没生效（或反过来，以为侧栏的才是真的）。
function syncProfileSourceUI() {
  $$('#pfSources input[data-key], #pfSrcCols input[data-key]').forEach((cb) => {
    cb.checked = S.prof.sel.has(cb.dataset.key);
  });
  // 抽屉里的整列跟着勾选上色：这样「这次从哪几个源导入」一眼可见，
  // 不用去数小方框（实测选框在深色列里不好认）。
  $$('#pfSrcCols .pf-srccol').forEach((col) => {
    const cb = $('input[data-key]', col);
    col.classList.toggle('on', !!(cb && cb.checked));
  });
  updateProfileState();
}

function updateProfileState() {
  const el = $('#pfState');
  if (!el) return;
  const n = S.prof.sel.size, all = S.prof.sources.length;
  el.textContent = all ? ('已启用 ' + n + '/' + all + ' 个源') : '未加载';
  el.className = 'tag ' + (n ? 'tag-green' : 'tag-amber');
  const pick = $('#pfPickN');
  if (pick) pick.textContent = all ? ('已选 ' + n + '/' + all) : '';
}

// profileSourcePicker 把资料源**分列**摆出来：一列一个源，写清这一列会填哪些字段。
//
// 为什么不像侧栏那样挤成一行小开关：用户在这里要做的是「这次从哪几个源导入」，
// 是个需要判断的决定 —— 得先知道每个源给什么。挤成一行只剩名字，等于没给依据。
// 顺序即优先级这件事也写在标题里，因为界面上的左右顺序就是真实取值顺序。
function profileSourcePicker() {
  const cols = S.prof.sources.map((s) => '<label class="pf-srccol" title="' + esc(s.key) + '">' +
    '<span class="pf-srcname"><input type="checkbox" data-key="' + esc(s.key) + '"' +
    (S.prof.sel.has(s.key) ? ' checked' : '') + '> <b>' + esc(s.label) + '</b></span>' +
    (s.note ? '<span class="pf-srcnote">' + esc(s.note) + '</span>' : '') +
    '</label>').join('');
  return '<div class="pf-pickhead">抓取源 <span class="tag" id="pfPickN"></span>' +
    '<span class="grow"></span>' +
    '<span class="cnhint">按需勾选；从左到右即优先级（同一字段取最靠前的源）</span></div>' +
    '<div class="pf-srcpick" id="pfSrcCols">' +
    (cols || '<span class="cnhint">没有可用的资料源，请检查网络后重开本面板</span>') + '</div>';
}

// bindProfileSourcePicker 绑定抽屉里那些源勾选框，并立刻同步一次两处状态。
function bindProfileSourcePicker() {
  $$('#pfSrcCols input[data-key]').forEach((cb) => {
    cb.onchange = () => {
      if (cb.checked) S.prof.sel.add(cb.dataset.key); else S.prof.sel.delete(cb.dataset.key);
      syncProfileSourceUI();
    };
  });
  syncProfileSourceUI();
}

// profileBody 组装抓取/写入入参；keys 只在写入时给（界面上的逐字段勾选）。
function profileBody(personId, name, keys) {
  const b = {
    person_id: personId || '',
    name: name,
    sources: Array.from(S.prof.sel),
    use_alias_memo: $('#pfAlias') ? $('#pfAlias').checked : true,
  };
  if (keys) b.keys = keys;
  return b;
}

// profileNoteTag 只用于首次渲染；之后行状态由 refreshProfileRow 就地更新。
function profileNoteTag(f) {
  if (f.will_write) return '<span class="tag tag-green">将写入</span>';
  if (f.value) return '<span class="tag tag-amber">已有值，跳过</span>';
  return '<span class="tag">未抓取到</span>';
}

// over = Emby 里已经有值，但这次也抓到了新值 —— 勾上就是覆盖。
// 这种行不预先勾（默认仍是「只填空白」），但**允许**用户主动勾选覆盖。
const rowIsOver = (f) => !!(f.value && f.emby_value);

// refreshProfileRow 勾选状态一变就重画这一行的判定文案与配色。
// 为什么不能只在提交时才体现：用户要能一眼看到「我现在勾的这一项会覆盖掉原值」，
// 而不是点完按钮才知道。
function refreshProfileRow(tr) {
  const cb = $('input[data-key]', tr);
  if (!cb) return;
  const over = tr.dataset.over === '1';
  const will = cb.checked;
  $('.pf-n', tr).innerHTML = !will
    ? (over ? '<span class="tag tag-amber">已有值，跳过</span>' : '<span class="tag">未勾选</span>')
    : (over ? '<span class="tag tag-red">将覆盖原值</span>' : '<span class="tag tag-green">将写入</span>');
  tr.classList.toggle('pf-over', over && will);
  tr.classList.toggle('pf-will', will && !over);
}

// refreshProfileApply 让按钮上的数字和勾选实时一致，并在有覆盖时变成危险色。
// 覆盖是不可逆动作（有快照可回滚，但仍要显眼），所以不用 window.confirm ——
// 那个在无头浏览器里会卡死（这个项目里踩过），改用「按钮自己变色 + 写清数量」。
function refreshProfileApply() {
  const rows = $$('#pfBody .pf-tbl tbody tr');
  const picked = rows.filter((tr) => { const c = $('input[data-key]', tr); return c && c.checked; });
  const over = picked.filter((tr) => tr.dataset.over === '1').length;
  const b = $('#pfApply');
  if (!b) return;
  b.disabled = picked.length === 0;
  b.textContent = picked.length
    ? ('写入 ' + picked.length + ' 个字段' + (over ? '（含 ' + over + ' 项覆盖）' : ''))
    : '没有可写入的字段';
  b.classList.toggle('btn-danger', over > 0);
  b.classList.toggle('btn-primary', over === 0);
  const hint = $('#pfCount');
  if (hint) {
    hint.textContent = picked.length
      ? (picked.length + ' 项待写入' + (over ? '，其中 ' + over + ' 项会覆盖 Emby 原值' : '，都是 Emby 里空着的字段'))
      : '当前没有勾选任何字段';
    hint.className = 'cnhint' + (over ? ' pf-warn' : '');
  }
}

function renderProfileFact(f) {
  const sizes = [f.bust, f.waist, f.hip].filter(Boolean).join(' / ');
  const kv = [
    ['出生日期', f.birth_date], ['出生地', f.birth_place],
    ['身高', f.height && (f.height + ' cm')],
    ['三围', sizes], ['罩杯', f.cup], ['血型', f.blood_type],
    ['出道', f.debut_date || f.debut_span],
    ['经纪', f.agency && (f.agency + (f.agency_span ? '（' + f.agency_span + '）' : ''))],
    ['兴趣/特长', f.hobby],
    ['别名', (f.aliases || []).join('、')],
    ['源站 ID', f.provider_id],
  ].filter((x) => x[1]);
  const score = f.match_score || 0;
  const cls = score >= 95 ? 'tag-green' : (score >= 80 ? 'tag-amber' : 'tag-red');
  const link = safeLink(f.source_url);
  return '<div class="pf-fact">' +
    '<div class="pf-facthead">' +
    '<span class="tag ' + cls + '" title="姓名置信度：95 以上才算确认是同一人">' +
    esc(f.source_label || f.source) + ' · 匹配 ' + score + '</span>' +
    '<b>' + esc(f.matched_name || '') + '</b>' +
    (f.elapsed_ms ? '<span class="cnhint">' + f.elapsed_ms + ' ms</span>' : '') +
    '<span class="grow"></span>' +
    (link ? '<a class="pf-link" target="_blank" rel="noopener noreferrer" href="' + esc(link) + '">源站页面 ↗</a>' : '') +
    '</div>' +
    (kv.length
      ? '<div class="pf-kv">' + kv.map((x) => '<span class="k">' + x[0] + '</span><span class="v">' + esc(x[1]) + '</span>').join('') + '</div>'
      : '<div class="cnhint">这个源只提供了外部 ID，没有可用的资料字段</div>') +
    '</div>';
}

function renderProfilePanel(personId, name, prof) {
  const body = $('#pfBody');
  if (!body) return;
  const fields = prof.fields || [];
  const srcTags = (prof.sources || []).map((s) => '<span class="tag tag-blue">' + esc(s) + '</span>').join('');
  const warns = (prof.warnings || []).map((w) => '<div class="alert">' + esc(w) + '</div>').join('');

  const rows = fields.map((f) => {
    const over = rowIsOver(f);
    // 抓到值就能勾（哪怕 Emby 里已经有值 —— 那是「覆盖」）；
    // 没抓到值的字段勾了也没东西可写，直接禁用。
    const canPick = !!f.value;
    const cls = f.will_write ? 'pf-will' : (over ? 'pf-skip' : '');
    return '<tr class="' + cls + '" data-over="' + (over ? '1' : '') + '">' +
      '<td class="pf-f"><label class="switch" title="' + esc(PROFILE_FIELD_HINT[f.key] || '') + '">' +
      '<input type="checkbox" data-key="' + esc(f.key) + '"' +
      (f.will_write ? ' checked' : '') + (canPick ? '' : ' disabled') + '>' +
      '<b>' + esc(f.label) + '</b></label></td>' +
      '<td class="pf-v pf-old">' + (f.emby_value
        ? '<div class="pf-pre">' + esc(f.emby_value) + '</div>'
        : '<span class="pf-dash">（空）</span>') + '</td>' +
      '<td class="pf-v pf-new">' + (f.value
        ? '<div class="pf-pre">' + esc(f.value) + '</div>' +
          (f.source ? '<div class="pf-from">来自 ' + esc(f.source) + '</div>' : '')
        : '<span class="pf-dash">—</span>') + '</td>' +
      '<td class="pf-n">' + profileNoteTag(f) + '</td>' +
      '</tr>';
  }).join('');

  const facts = (prof.facts || []).map(renderProfileFact).join('');
  const aliases = prof.aliases || [];
  const aliasLine = aliases.length
    ? '<div class="pf-alias">抓到的别名 ' + aliases.map((a) => '<span class="tag">' + esc(a) + '</span>').join('') +
      '<span class="cnhint">写入成功后会记进别名记忆，下次各源搜索命中率更高</span></div>'
    : '';

  body.innerHTML = warns +
    profileSourcePicker() +
    '<div class="pf-srcbar">命中来源：' + (srcTags || '<span class="cnhint">无</span>') +
    '<span class="grow"></span>' +
    '<span class="cnhint">Emby 里为空的字段 ' + (prof.write_count || 0) + ' 个</span></div>' +
    '<div class="scroll-x"><table class="tbl pf-tbl"><thead><tr>' +
    '<th style="width:92px">字段</th><th>Emby 现有值</th><th>本次抓取</th><th style="width:104px">结果</th>' +
    '</tr></thead><tbody>' + rows + '</tbody></table></div>' +
    aliasLine +
    '<div class="pf-act">' +
    '<button class="btn btn-primary" id="pfApply">写入勾选字段</button>' +
    '<button class="btn" id="pfFetch">抓取资料</button>' +
    '<span class="grow"></span><span class="cnhint" id="pfCount"></span>' +
    '</div>' +
    '<div class="cnhint" style="margin-top:8px">默认只勾 Emby 里空着的字段。' +
    '已有值的字段<strong>也可以勾上覆盖</strong> —— 覆盖前的原值同样会进快照，「同步历史」里随时可还原。' +
    '抓不到值的字段不会写（覆盖成空等于清库）。</div>' +
    '<div class="pf-works" id="pfWorks"></div>' +
    '<details class="adv"' + (facts ? '' : ' hidden') + '><summary>各资料源明细（' + (prof.facts || []).length + ' 个源命中）</summary>' +
    facts + '</details>';

  bindProfileSourcePicker();
  // 只绑对照表里的勾选框。源勾选框由 bindProfileSourcePicker 管，
  // 两者的语义完全不同（一个选字段、一个选源），别用同一个选择器一锅端。
  $$('#pfBody .pf-tbl input[data-key]').forEach((cb) => {
    cb.onchange = () => { refreshProfileRow(cb.closest('tr')); refreshProfileApply(); };
  });
  refreshProfileApply();

  $('#pfFetch').onclick = () => fetchProfileInto();
  $('#pfApply').onclick = async () => {
    // 注意范围是 .pf-tbl：抽屉里还有一组**选源**的勾选框（#pfSrcCols）也是
    // input[data-key]，不加限定就会把源名当成字段名提交上去 ——
    // 结果是「写入 0 个字段」而按钮上明明写着「写入 3 个字段」。
    const keys = $$('#pfBody .pf-tbl input[data-key]:checked').map((c) => c.dataset.key);
    if (!keys.length) { toast('没有勾选任何字段', 'info'); return; }
    const b = $('#pfApply');
    b.disabled = true; b.innerHTML = '<span class="spin"></span> 写入中…';
    try {
      const r = await api('/api/profile/apply', { method: 'POST', body: profileBody(personId, name, keys) });
      toast(name + '：' + r.message, 'ok');
      $('#drawerHost').innerHTML = '';
      loadPersons(S.ps.start);
      loadProfileSources(true);
    } catch (e) {
      toast(e.message, 'err');
      b.disabled = false; refreshProfileApply();
    }
  };

  // 抓到之后重刷一次作品列表：它只读本地 Emby（毫秒级），
  // 不该被上面那三个外部资料源的抓取拖住，也没必要为它多等一轮。
  loadProfileWorks(personId, false);
}

// ---------------- 该演员在媒体库里的作品 ----------------
//
// 放在资料抽屉里（同一个演员的上下文）：点开一位演员，想知道的是两件事 ——
// 「他的资料对不对」和「我库里有哪些他的片」。分成两个入口反而割裂。
// 只读本地 Emby，所以它有自己的加载态、自己的分页，不参与资料抓取。

async function loadProfileWorks(personId, more) {
  const box = $('#pfWorks');
  if (!box) return;
  const w = S.prof.works;
  w.personId = personId;
  if (!more) { w.items = []; w.total = 0; w.limit = 60; w.ready = false; }
  else w.limit += 120;
  if (!w.items.length) {
    box.innerHTML = '<div class="pf-wh">媒体库作品 <span class="tag">…</span></div>' +
      '<div class="cnhint"><span class="spin"></span> 正在读取媒体库…</div>';
  }
  try {
    const d = await api('/api/profile/works?person_id=' + encodeURIComponent(personId) +
      '&start=0&limit=' + w.limit);
    w.items = d.items || [];
    w.total = d.total || 0;
    w.ready = true;
    renderProfileWorks();
  } catch (e) {
    box.innerHTML = '<div class="pf-wh">媒体库作品</div>' +
      '<div class="cnhint">读取失败：' + esc(e.message) + '</div>';
  }
}

function renderProfileWorks() {
  const box = $('#pfWorks');
  if (!box) return;
  const w = S.prof.works;
  const items = w.items || [];
  if (!w.total) {
    box.innerHTML = '<div class="pf-wh">媒体库作品 <span class="tag">0</span></div>' +
      '<div class="cnhint">这个演员在媒体库里还没有作品。若库里确实有，' +
      '多半是 Emby 里这位演员的名字和作品里的演职员名字对不上（可在 Emby 里合并同一人）。</div>';
    return;
  }
  const cards = items.map((it) => {
    // 封面走同源代取：CSP 是 img-src 'self'，外链一律显示不出来。
    const src = it.image_tag ? embyImg(it.id, it.image_tag, 200) : '';
    return '<div class="pf-wcard" title="' + esc(it.name) + '">' +
      '<div class="wc">' + (src ? '<img loading="lazy" src="' + esc(src) + '" alt="">' :
        '<span class="noimg">无封面</span>') + '</div>' +
      '<div class="wt"><b>' + esc(it.number || it.name) + '</b>' +
      '<span>' + esc(it.year ? String(it.year) : '') + (it.library ? ' · ' + esc(it.library) : '') + '</span></div>' +
      '</div>';
  }).join('');

  const rest = w.total - items.length;
  box.innerHTML = '<div class="pf-wh">媒体库作品 <span class="tag">' + w.total + '</span>' +
    '<span class="grow"></span>' +
    '<span class="cnhint">按首播日期倒序' + (rest > 0 ? '（已显示 ' + items.length + ' 部）' : '') + '</span>' +
    (rest > 0 ? '<button class="btn btn-sm" id="pfWMore">再加载 ' + Math.min(120, rest) + ' 部</button>' : '') +
    '</div><div class="pf-wgrid">' + cards + '</div>';

  const more = $('#pfWMore');
  if (more) more.onclick = () => loadProfileWorks(S.prof.works.personId, true);
}

// openProfilePanel 只**打开**面板，不自动抓取。
//
// 为什么改掉「一打开就抓」：抓一次要并发访问三个外部站点，秒级起步，
// 而多数时候用户点进来只是想看看「这个演员在库里有哪些片」，
// 或者只想补某一个字段。一进来就替他联网，既慢又白给上游添流量，
// 站点稍有波动还会让整个面板开不了。所以打开时只做两件**本地**事
// （列出可选的源 + 读 Emby 里的作品），抓取交给「抓取资料」按钮。
async function openProfilePanel(personId, name) {
  S.prof.cur = { personId: personId, name: name };
  openDrawer('<h3>' + esc(name) + '</h3>' +
    '<div class="sub">抓取简介 / 出生日期 / 出生地 / 外部 ID，与 Emby 现有值逐字段比对</div>' +
    '<div id="pfBody"></div>');
  try { await loadProfileSources(); } catch (_) { /* 源清单拿不到时下面会给出提示 */ }
  renderProfileIdle();
}

// renderProfileIdle 是面板的**未抓取态**：源选择 + 「抓取资料」按钮，
// 外加只读本地 Emby 就能拿到的「媒体库作品」。抓取是显式动作。
function renderProfileIdle() {
  const body = $('#pfBody');
  if (!body) return;
  const cur = S.prof.cur || {};
  body.innerHTML =
    profileSourcePicker() +
    '<div class="pf-act">' +
    '<button class="btn btn-primary" id="pfFetch">抓取资料</button>' +
    '<span class="grow"></span>' +
    '<span class="cnhint">点「抓取资料」才会访问上面勾选的站点</span>' +
    '</div>' +
    '<div class="cnhint" style="margin-top:8px">抓取本身只读、不写 Emby：' +
    '抓到后会列出「Emby 现有值 vs 本次抓取值」，<strong>你勾哪些才写哪些</strong>。</div>' +
    '<div class="pf-works" id="pfWorks"></div>';
  bindProfileSourcePicker();
  $('#pfFetch').onclick = () => fetchProfileInto();
  // 作品列表只读本地 Emby，毫秒级 —— 抓不抓资料都不影响它，所以一进来就加载。
  loadProfileWorks(cur.personId, false);
}

// fetchProfileInto 是「抓取资料」按钮的实现：按当前勾选的源抓一次，然后重画成
// 「现有值 vs 抓取值」对照表。失败时退回未抓取态并把原因写在最上面，
// 而不是把整个面板变成一个错误页 —— 用户还得能改源重试、也还能看作品列表。
async function fetchProfileInto() {
  const body = $('#pfBody');
  const cur = S.prof.cur || {};
  if (!body || !cur.personId) return;
  if (!S.prof.loaded) { toast('资料源还没加载好', 'err'); return; }
  if (!S.prof.sel.size) { toast('至少勾选一个资料源', 'err'); return; }
  const btn = $('#pfFetch');
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="spin"></span> 正在抓取…'; }
  try {
    const prof = await api('/api/profile/preview', { method: 'POST', body: profileBody(cur.personId, cur.name) });
    renderProfilePanel(cur.personId, cur.name, prof);
  } catch (e) {
    toast(e.message, 'err');
    renderProfileIdle();
    const b = $('#pfBody');
    if (b) b.insertAdjacentHTML('afterbegin', '<div class="alert">抓取失败：' + esc(e.message) + '</div>');
  }
}

// collectProfileTargets 按**当前列表筛选条件**（搜索词 / 媒体库 / 只看无头像）翻页取人选。
//
// 为什么不在服务端按 limit 取：那样「当前条件」只认媒体库，搜索词和「只看无头像」
// 会静默失效 —— 用户以为在批量处理屏幕上看到的这批人，实际处理的是另一批。
// 这里复用同一个 /api/persons 接口，筛选逻辑就不存在第二份实现。
async function collectProfileTargets(limit) {
  const q = $('#psQ').value.trim();
  const parentID = $('#psLib').value;
  const missing = $('#psMissing').checked;
  const out = [];
  const pageSize = 100;
  for (let start = 0; out.length < limit; start += pageSize) {
    const params = new URLSearchParams({
      q: q, start: start, limit: pageSize,
      parent_id: parentID, missing_image: missing ? 'true' : 'false',
    });
    const d = await api('/api/persons?' + params.toString());
    const items = d.items || [];
    for (const p of items) {
      out.push({ id: p.Id, name: p.Name });
      if (out.length >= limit) break;
    }
    // 翻页终止条件只看「这一页是不是满的」，**不看 d.total**：
    // /api/persons 带搜索词或缺失过滤时 total 会是 0（实测 Emby 的 /Persons
    // 在过滤场景下不回 TotalRecordCount），拿它当判据会第一页就退出，
    // 界面上写着「前 300 位」实际只处理了 100 位。
    if (items.length < pageSize) break;
  }
  return out;
}

async function batchProfile(mode) {
  if (!S.prof.loaded) { toast('资料源还没加载好', 'err'); return; }
  if (!S.prof.sel.size) { toast('至少勾选一个资料源', 'err'); return; }
  let items, title;
  try {
    if (mode === 'page') {
      items = S.ps.items.map((p) => ({ id: p.Id, name: p.Name }));
      title = '批量抓取演员资料（当前页 ' + items.length + ' 位）';
    } else {
      const limit = Math.max(1, Number($('#pfLimit').value) || 24);
      items = await collectProfileTargets(limit);
      title = '批量抓取演员资料（前 ' + limit + ' 位）';
    }
  } catch (e) { toast('取演员列表失败：' + e.message, 'err'); return; }
  if (!items.length) { toast('当前条件下没有可处理的演员', 'err'); return; }
  try {
    const r = await api('/api/profile/batch', {
      method: 'POST',
      body: {
        items: items,
        sources: Array.from(S.prof.sel),
        use_alias_memo: $('#pfAlias').checked,
      },
    });
    watchJob(r.job_id, title, () => { loadPersons(S.ps.start); loadProfileSources(true); });
  } catch (e) { toast(e.message, 'err'); }
}

async function openSyncHistory() {
  openDrawer('<h3>资料同步历史</h3>' +
    '<div class="sub">每次资料写入前都会留一份快照，可以从这里一键还原到写入前的状态</div>' +
    '<div id="shBody"><div class="empty"><span class="spin"></span> 加载中…</div></div>');
  const body = $('#shBody');
  try {
    const d = await api('/api/profile/history?limit=200');
    const recs = d.records || [];
    if (!recs.length) { body.innerHTML = '<div class="empty">还没有任何资料写入记录</div>'; return; }
    body.innerHTML = recs.map((r) => '<div class="sh-row' + (r.rolled_back ? ' sh-done' : '') + '">' +
      '<div class="sh-i"><b>' + esc(r.name) + '</b>' +
      '<span>' + fmtTime(r.created_at) + ' · ' + esc((r.sources || []).join(' / ') || '—') + '</span>' +
      '<span class="sh-ch">' + esc((r.changed || []).join('、')) + '</span></div>' +
      (r.rolled_back
        ? '<span class="tag tag-green">已回滚</span>'
        : '<button class="btn btn-sm" data-rid="' + esc(r.id) + '" data-name="' + esc(r.name) + '">回滚</button>') +
      '</div>').join('');
    // 回滚是不可逆的写操作，用「点两次」代替 window.confirm()：
    // 原生弹窗在无头浏览器（回归脚本）里会直接卡住。
    $$('#shBody button[data-rid]').forEach((btn) => {
      btn.onclick = async () => {
        if (btn.dataset.armed !== '1') {
          btn.dataset.armed = '1';
          btn.classList.add('btn-danger');
          btn.textContent = '确认回滚？';
          setTimeout(() => {
            if (btn.dataset.armed !== '1') return;
            btn.dataset.armed = '0';
            btn.classList.remove('btn-danger');
            btn.textContent = '回滚';
          }, 4000);
          return;
        }
        btn.disabled = true; btn.textContent = '回滚中…';
        try {
          const r = await api('/api/profile/rollback', { method: 'POST', body: { id: btn.dataset.rid } });
          toast(btn.dataset.name + '：' + r.message, 'ok');
          openSyncHistory();
          loadPersons(S.ps.start);
        } catch (e) {
          toast(e.message, 'err');
          btn.disabled = false; btn.textContent = '回滚';
        }
      };
    });
  } catch (e) {
    body.innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
  }
}

// ---------------- 番号补全 ----------------
async function jbScan() {
  const star = $('#jbStar').value.trim();
  if (!star) { toast('请先填写演员名或演员页地址', 'err'); return; }
  S.jb.selected.clear();
  S.jb.magnets = [];
  $('#jbSummary').innerHTML = '';
  $('#jbBody').innerHTML = '';
  try {
    const r = await api('/api/javbus/scan', {
      method: 'POST',
      body: { star, parent_id: $('#jbLib').value, max_pages: Number($('#jbPages').value) || 20 },
    });
    watchJob(r.job_id, '统计番号：' + star, (j) => {
      if (j.status !== 'done') return;
      const res = j.result;
      if (!res) return;
      S.jb.scan = res;
      renderJbSummary(res);
      renderJbMissing(res);
    });
  } catch (e) { toast(e.message, 'err'); }
}

async function jbProbe() {
  const q = $('#jbStar').value.trim();
  const btn = $('#jbProbe');
  btn.disabled = true;
  const old = btn.textContent;
  btn.textContent = '诊断中…';
  $('#jbSummary').innerHTML = '';
  $('#jbBody').innerHTML = '<div class="card"><h3>诊断中</h3><div class="empty">正在连接 javbus，最多等 45 秒…</div></div>';
  try {
    const p = await api('/api/javbus/probe' + (q ? '?q=' + encodeURIComponent(q) : ''));
    renderProbe(p);
  } catch (e) {
    $('#jbBody').innerHTML = '<div class="card"><h3>诊断请求失败</h3><div class="empty">' + esc(e.message) + '</div></div>';
  } finally {
    btn.disabled = false;
    btn.textContent = old;
  }
}

function renderProbe(p) {
  const lvl = p.ok ? 'green' : (p.blocked ? 'amber' : 'red');
  const rows = [
    ['目标地址', p.url || '未配置'],
    ['HTTP 状态', p.status ? String(p.status) : '无响应'],
    ['响应大小', (p.size || 0) + ' 字节'],
    ['耗时', (p.elapsed_ms || 0) + ' ms'],
    ['页面结构', p.looks_like_home ? '正常（含影片列表标记）' : '未匹配到影片列表'],
  ];
  let html = '<div class="card"><h3>诊断结果 <span class="tag tag-' + lvl + '">' +
    (p.ok ? '连通' : (p.blocked ? '被拦截' : '失败')) + '</span></h3>' +
    '<div style="font-size:13px;line-height:1.9;color:var(--text-2)">' + esc(p.message) + '</div>' +
    '<table style="margin-top:12px;width:100%;font-size:13px;border-collapse:collapse">' +
    rows.map((r) => '<tr><td style="padding:4px 10px 4px 0;color:var(--text-2);white-space:nowrap">' + r[0] +
      '</td><td class="mono" style="padding:4px 0">' + esc(r[1]) + '</td></tr>').join('') + '</table>';

  const mk = p.markers || {};
  const keys = Object.keys(mk).filter((k) => mk[k] > 0);
  if (keys.length) {
    html += '<div class="hint" style="margin-top:10px">页面标记：' +
      keys.map((k) => '<span class="tag" style="margin:2px">' + esc(k) + ' × ' + mk[k] + '</span>').join('') + '</div>';
  }
  if (p.dump_path) {
    html += '<div class="hint" style="margin-top:8px">原始响应已保存：<span class="mono">' + esc(p.dump_path) + '</span>（可打开看看到底返回了什么）</div>';
  }

  const s = p.search;
  if (s) {
    html += '<h3 style="margin-top:18px">演员搜索接口</h3>' +
      '<table style="width:100%;font-size:13px;border-collapse:collapse">' +
      '<tr><td style="padding:4px 10px 4px 0;color:var(--text-2)">关键词</td><td class="mono">' + esc(s.keyword) + '</td></tr>' +
      '<tr><td style="padding:4px 10px 4px 0;color:var(--text-2)">返回形态</td><td class="mono">' +
        esc(s.kind || '未知') + '（HTTP ' + (s.status || 0) + '，' + (s.size || 0) + ' 字节）</td></tr>' +
      '<tr><td style="padding:4px 10px 4px 0;color:var(--text-2)">匹配演员</td><td class="mono">' + (s.found || 0) + ' 个</td></tr>' +
      '</table>' +
      (s.error ? '<div class="alert" style="margin-top:10px">' + esc(s.error) + '</div>' : '') +
      (s.raw_sample ? '<div class="hint" style="margin-top:8px">响应片段：<span class="mono">' + esc(s.raw_sample) + '</span></div>' : '') +
      (s.dump_path ? '<div class="hint" style="margin-top:6px">原始响应：<span class="mono">' + esc(s.dump_path) + '</span></div>' : '');
  } else if (!p.ok) {
    html += '<div class="hint" style="margin-top:12px">在输入框里填一个演员名再点诊断，可以顺便测演员搜索接口是否可用。</div>';
  }

  html += '</div>';
  $('#jbBody').innerHTML = html;
}

function renderJbSummary(res) {
  const cards = [
    { k: 'javbus 作品总数', v: num(res.total), c: '' },
    { k: '本地已收录', v: num(res.matched), c: 'green' },
    { k: '缺失番号', v: num(res.missing.length), c: 'warn' },
    { k: '本地条目数', v: num(res.local_count), c: 'accent' },
  ];
  $('#jbSummary').innerHTML = '<div class="stat-grid">' + cards.map((x) =>
    '<div class="stat ' + x.c + '"><div class="k">' + x.k + '</div><div class="v">' + x.v + '</div></div>').join('') + '</div>' +
    (res.candidates && res.candidates.length > 1
      ? '<div class="card"><h3>匹配到多个演员，当前使用「' + esc(res.star.name) + '」</h3>' +
        res.candidates.map((c) => '<span class="tag" style="margin:2px">' + esc(c.name) + ' · ' + esc(c.id) + '</span>').join('') +
        '<div class="hint" style="margin-top:8px">如不正确，请改用完整演员页地址重新统计。</div></div>' : '');
}

function renderJbMissing(res) {
  const miss = res.missing || [];
  const body = $('#jbBody');
  if (!miss.length) {
    body.innerHTML = '<div class="card"><h3>没有缺失</h3><div class="empty">该演员的作品本地都已收录 🎉</div></div>';
    return;
  }
  body.innerHTML =
    '<div class="card"><h3>缺失番号（' + miss.length + '） <span class="spacer"></span>' +
    '<button class="btn btn-sm" id="jbAll">全选</button>' +
    '<button class="btn btn-sm" id="jbNone">清空</button>' +
    '<button class="btn btn-sm btn-primary" id="jbMag">抓取所选磁力</button>' +
    '<button class="btn btn-sm" id="jbExport">导出 JSON</button></h3>' +
    '<div class="missgrid" id="jbGrid">' + miss.map((m, i) => missCard(m, i)).join('') + '</div></div>' +
    '<div class="card" id="jbMagCard" style="display:none"><h3>磁力列表 <span class="spacer"></span>' +
    '<span class="cnhint">按体积从大到小</span>' +
    '<button class="btn btn-sm" id="jbCopyOne">复制当前番号</button>' +
    '<button class="btn btn-sm" id="jbCopyAll">复制全部</button></h3><div class="maglist" id="jbMagList"></div></div>';

  $$('#jbGrid .misscard').forEach((el) => el.onclick = () => {
    const n = el.dataset.n;
    if (S.jb.selected.has(n)) S.jb.selected.delete(n); else S.jb.selected.add(n);
    el.classList.toggle('sel');
  });
  $('#jbAll').onclick = () => { miss.forEach((m) => S.jb.selected.add(m.number)); $$('#jbGrid .misscard').forEach((e) => e.classList.add('sel')); };
  $('#jbNone').onclick = () => { S.jb.selected.clear(); $$('#jbGrid .misscard').forEach((e) => e.classList.remove('sel')); };
  $('#jbExport').onclick = () => exportJSON(miss);
  $('#jbMag').onclick = () => fetchMagnets(miss);
}

function missCard(m, i) {
  // javbus 封面有 Referer 防盗链，直连 403，统一交给 imgSrc 决定是否走代理。
  const src = imgSrc(m.cover);
  return '<div class="misscard" data-n="' + esc(m.number) + '" data-i="' + i + '">' +
    '<div class="cover">' + (src ? '<img loading="lazy" src="' + esc(src) + '" alt="">' : '') +
    '<div class="ck">✓</div></div>' +
    '<div class="t"><b>' + esc(m.number) + '</b><span title="' + esc(m.title) + '">' + esc(m.date || m.title || '') + '</span></div></div>';
}

function exportJSON(list) {
  const blob = new Blob([JSON.stringify(list, null, 2)], { type: 'application/json' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = 'missing-' + Date.now() + '.json';
  a.click();
  URL.revokeObjectURL(a.href);
}

async function fetchMagnets(miss) {
  let targets = miss.filter((m) => S.jb.selected.has(m.number));
  if (!targets.length) { toast('请先勾选要抓取的番号', 'err'); return; }
  try {
    const r = await api('/api/javbus/magnets', { method: 'POST', body: { targets } });
    watchJob(r.job_id, '抓取磁力', (j) => {
      if (j.status !== 'done') return;
      S.jb.magnets = j.result || [];
      S.jb.magTab = ''; // 新一批磁力，默认停在第一个番号
      renderMagnets();
    });
  } catch (e) { toast(e.message, 'err'); }
}

// 磁力列表按番号分页：上面一排番号标签，点哪个看哪个。
// 之前是一长条把所有番号的磁力堆在一起，几十条下来分不清哪条属于哪个番号。
function renderMagnets() {
  const card = $('#jbMagCard');
  if (!card) return;
  card.style.display = 'block';
  const list = $('#jbMagList');
  const groups = S.jb.magnets || [];
  if (!groups.length) {
    list.innerHTML = '<div class="magempty">没有磁力数据</div>';
    return;
  }
  // 重渲染时保持当前选中的番号；选中项已不存在就回到第一个
  if (!groups.some((g) => g.number === S.jb.magTab)) S.jb.magTab = groups[0].number;

  const tabs = groups.map((g) => {
    const n = (g.magnets || []).length;
    return '<button class="magtab' + (g.number === S.jb.magTab ? ' on' : '') + (n ? '' : ' bad') +
      '" data-num="' + esc(g.number) + '" title="' + esc(g.title || g.number) + '">' +
      esc(g.number) + '<i>' + (n || '无') + '</i></button>';
  }).join('');

  const panels = groups.map((g) => {
    const rows = (g.magnets || []).map((m) =>
      '<div class="magrow"><div class="n">' + esc(m.name) + '<br><i>' + esc(m.link.slice(0, 110)) + '…</i></div>' +
      '<span class="sz">' + esc(m.size || '') + '</span><span class="dt">' + esc(m.date || '') + '</span>' +
      '<button class="btn btn-sm" data-copy="' + esc(m.link) + '">复制</button></div>').join('');
    return '<div class="magpanel' + (g.number === S.jb.magTab ? ' on' : '') + '" data-num="' + esc(g.number) + '">' +
      (g.title ? '<div class="maghead">' + esc(g.title) + '</div>' : '') +
      (rows || '<div class="magempty">' + esc(g.error || '没有磁力链接') + '</div>') + '</div>';
  }).join('');

  list.innerHTML = '<div class="magtabs">' + tabs + '</div><div class="magpanels">' + panels + '</div>';

  $$('.magtab', list).forEach((b) => b.onclick = () => {
    S.jb.magTab = b.dataset.num;
    $$('.magtab', list).forEach((x) => x.classList.toggle('on', x === b));
    $$('.magpanel', list).forEach((p) => p.classList.toggle('on', p.dataset.num === b.dataset.num));
  });
  $$('button[data-copy]', list).forEach((b) => b.onclick = () => {
    navigator.clipboard.writeText(b.dataset.copy).then(() => toast('已复制磁力链接', 'ok'), () => toast('复制失败', 'err'));
  });

  const copy = (links, label) => {
    if (!links.length) { toast('没有磁力链接', 'err'); return; }
    navigator.clipboard.writeText(links.join('\n'))
      .then(() => toast('已复制 ' + label + ' 共 ' + links.length + ' 条磁力链接', 'ok'), () => toast('复制失败', 'err'));
  };
  const one = $('#jbCopyOne');
  if (one) one.onclick = () => {
    const g = groups.find((x) => x.number === S.jb.magTab) || {};
    copy(((g.magnets) || []).map((m) => m.link), S.jb.magTab);
  };
  const ca = $('#jbCopyAll');
  if (ca) ca.onclick = () => copy(groups.flatMap((g) => (g.magnets || []).map((m) => m.link)), '全部番号');
}

// ---------------- 国产传媒专项刮削 ----------------
//
// 交互对齐「媒体库刮削」：选库 → 查询 → 网格 → 单个 / 多选刮削 → 可编辑元数据。
//
// 刻意的取舍：
//   - 抓取字段固定全开（封面 / 标题 / 标签 / 日期），界面上不再给勾选；
//   - 四家站点的明细不在界面上摊开（要看某家站点认不认这个番号，
//     直接用 /api/cn/search?q=91CM-014 这个只读接口）；
//   - 没有「试运行」这一步，点刮削就是写入。
//
// 选中的条目 id 放在 S.cn.sel 里：翻页、重新查询、刮完刷新都不会丢，
// 和番号补全页的 S.jb.selected 一个思路。

S.cn = { start: 0, limit: 24, total: 0, items: [], sel: new Set() };

function cnSyncSelUI() {
  $('#cnSelCount').textContent = '已选 ' + S.cn.sel.size;
  const ids = S.cn.items.map((it) => it.Id);
  const on = ids.filter((id) => S.cn.sel.has(id)).length;
  const box = $('#cnSelAll');
  box.checked = ids.length > 0 && on === ids.length;
  // 本页选了一部分时给个中间态，否则「全选」看着像没生效。
  box.indeterminate = on > 0 && on < ids.length;
}

async function loadCnItems(start) {
  const cn = S.cn;
  cn.start = start || 0;
  if (!$('#cnLib').value) {
    $('#cnGrid').innerHTML = '<div class="empty">先选一个媒体库，再点「查询」</div>';
    $('#cnPager').innerHTML = '';
    $('#cnCount').textContent = '—';
    return;
  }
  $('#cnGrid').innerHTML = '<div class="empty"><span class="spin"></span> 加载中…</div>';
  const params = new URLSearchParams({
    parent: $('#cnLib').value,
    q: $('#cnQ').value.trim(),
    start: cn.start, limit: cn.limit,
    missing_image: $('#cnMissing').checked ? 'true' : 'false',
  });
  try {
    const d = await api('/api/items?' + params.toString());
    cn.items = d.items || [];
    cn.total = d.total || 0;
    $('#cnCount').textContent = '共 ' + num(cn.total) + ' 条' +
      ($('#cnMissing').checked ? '（本页过滤后 ' + cn.items.length + ' 条）' : '');
    if (!cn.items.length) {
      $('#cnGrid').innerHTML = '<div class="empty">没有符合条件的条目</div>';
    } else {
      $('#cnGrid').innerHTML = cn.items.map(renderCnCard).join('');
    }
    renderPager('#cnPager', cn, loadCnItems);
    cnSyncSelUI();
  } catch (e) {
    $('#cnGrid').innerHTML = '<div class="empty">加载失败：' + esc(e.message) + '</div>';
    $('#cnPager').innerHTML = '';
  }
}

function renderCnCard(it) {
  const has = it.ImageTags && it.ImageTags.Primary;
  const img = has ? '<img loading="lazy" src="' + embyImg(it.Id, it.ImageTags.Primary, 300) + '" alt="">'
    : '<div class="ph">无封面</div>';
  const number = it.Number || '';
  const sel = S.cn.sel.has(it.Id);
  // 片名和路径里都没有番号时搜不了，按钮直接禁用 —— 点下去只会换回一句报错。
  const scrape = number
    ? '<button class="btn btn-sm btn-primary" data-act="cnscrape">刮削</button>'
    : '<button class="btn btn-sm" disabled title="片名和路径里都没有番号，无法搜索">刮削</button>';
  return '<div class="mcard' + (sel ? ' sel' : '') + '" data-id="' + esc(it.Id) + '">' +
    '<div class="poster">' + img +
    '<label class="pick" title="勾选后可批量刮削"><input type="checkbox" data-act="cnpick"' + (sel ? ' checked' : '') + '></label>' +
    '<span class="badge' + (has ? '' : ' no') + '">' + esc(number || (it.ProductionYear || '—')) + '</span></div>' +
    '<div class="body"><div class="name" title="' + esc(it.Name) + '">' + esc(it.Name) + '</div>' +
    '<div class="meta">' + (it.ProductionYear || '') + '</div>' +
    '<div class="acts">' + scrape +
    '<button class="btn btn-sm" data-act="cndetail">编辑</button>' +
    '</div></div></div>';
}

// cnOutcome 把一次刮削结果压成一句人话。
//
// 界面不摊四家站点的明细，但「四家都没收录」和「站点根本抓不通」必须分得清 ——
// 前者该换个番号写法，后者该去查网络或站点地址。
function cnOutcome(r) {
  if (r.applied) return r.message || '已写入';
  const sites = r.sites || [];
  if (sites.length && sites.every((s) => !s.ok)) {
    return '站点全部抓取失败，检查网络或「设置 → 国产传媒站点」';
  }
  if (!(r.matched || []).length) {
    return '四家站点都没有「' + (r.number || '') + '」的精确结果';
  }
  return r.message || '没有需要写入的字段';
}

// 单个刮削。字段固定全开，所以不传 fields；不传 dry_run 就是写入。
async function cnScrapeOne(itemId, btn) {
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="spin"></span>'; }
  try {
    const r = await api('/api/cn/scrape', {
      method: 'POST',
      body: {
        id: itemId,
        overwrite_images: $('#cnOverwriteImg').checked,
        overwrite_title: $('#cnOverwriteTitle').checked,
      },
    });
    toast(cnOutcome(r), r.applied ? 'ok' : 'err');
    if (r.applied) loadCnItems(S.cn.start);
    else if (btn) { btn.disabled = false; btn.textContent = '刮削'; }
  } catch (e) {
    toast(e.message, 'err');
    if (btn) { btn.disabled = false; btn.textContent = '刮削'; }
  }
}

// 批量刮削勾选的条目：把 id 明确传给后端，不用「整库前 N 条」那种模糊范围。
async function cnScrapeSelected() {
  const ids = [...S.cn.sel];
  if (!ids.length) { toast('先勾选要刮削的条目', 'err'); return; }
  try {
    const r = await api('/api/cn/scrape-batch', {
      method: 'POST',
      body: {
        ids,
        overwrite_images: $('#cnOverwriteImg').checked,
        overwrite_title: $('#cnOverwriteTitle').checked,
      },
    });
    watchJob(r.job_id, '国产传媒刮削（' + ids.length + ' 条）', () => {
      S.cn.sel.clear();
      cnSyncSelUI();
      loadCnItems(S.cn.start);
    });
  } catch (e) { toast(e.message, 'err'); }
}

// cnSplitList 把「逗号分隔」的输入拆成数组，中英文逗号、顿号都认。
function cnSplitList(s) {
  return String(s || '').split(/[,，、]/).map((x) => x.trim()).filter(Boolean);
}

// 元数据编辑抽屉。
//
// 关键：只提交**改动过**的字段。后端用指针区分「没提交」和「清空」，
// 而 Emby 的 POST /Items/{id} 是整对象替换 —— 把没碰过的字段一起发过去，
// 一次误操作就能抹掉别人攒了很久的元数据（这个项目已经出过一次事故）。
async function openCnEditor(itemId) {
  const host = openDrawer('<div class="empty"><span class="spin"></span> 加载中…</div>');
  let it;
  try { it = await api('/api/items/detail?id=' + encodeURIComponent(itemId)); }
  catch (e) { host.innerHTML = ''; toast(e.message, 'err'); return; }

  // 这个 Emby 构建（4.9.0.42）的详情接口**不返回 `Tags`**（实测永远是 null），
  // 标签只体现在 `TagItems` 里。写的时候仍然发 `Tags`（服务端认这个字段），
  // 读的时候两边都看一眼，免得编辑框莫名其妙是空的。
  const tags = (Array.isArray(it.Tags) && it.Tags.length)
    ? it.Tags
    : (it.TagItems || []).map((t) => t && t.Name).filter(Boolean);
  const orig = {
    name: it.Name || '',
    original_title: it.OriginalTitle || '',
    overview: it.Overview || '',
    official_rating: it.OfficialRating || '',
    premiere_date: (it.PremiereDate || '').slice(0, 10),
    production_year: it.ProductionYear || 0,
    tags: cnSplitList(tags.join(',')),
    genres: cnSplitList((it.Genres || []).join(',')),
  };

  const has = it.ImageTags && it.ImageTags.Primary;
  openDrawer(
    '<h3>编辑元数据</h3><div class="sub">' + esc(it.Name || '') + '</div>' +
    (has ? '<img src="' + embyImg(it.Id, it.ImageTags.Primary, 420) + '" style="width:150px;border-radius:8px;border:1px solid var(--border);margin-bottom:14px">' : '') +
    '<div class="field"><label>名称</label><input class="input" id="ceName"></div>' +
    '<div class="field"><label>原始标题</label><input class="input" id="ceOrig"></div>' +
    '<div class="field"><label>简介</label><textarea class="input" id="ceOv" rows="6"></textarea></div>' +
    '<div class="row">' +
    '<div class="field"><label>发行日期（YYYY-MM-DD）</label><input class="input" id="ceDate" placeholder="留空 = 不修改"></div>' +
    '<div class="field"><label>年份</label><input class="input" id="ceYear" type="number" min="0" max="2999" placeholder="留空 = 不修改"></div>' +
    '</div>' +
    '<div class="field"><label>标签（逗号分隔）</label><input class="input" id="ceTags"></div>' +
    '<div class="field"><label>类型（逗号分隔）</label><input class="input" id="ceGenres"></div>' +
    '<div class="field"><label>分级</label><input class="input" id="ceRating"></div>' +
    '<div style="display:flex;gap:8px;margin-top:14px">' +
    '<button class="btn btn-primary" id="ceSave">保存</button>' +
    '<button class="btn" id="ceScrape">刮削这一条</button>' +
    '</div>' +
    '<div class="cnhint" style="margin-top:10px">只提交改动过的字段，没碰的不动。' +
    '名称不能为空；发行日期和年份<b>留空表示不修改</b>（不会清掉已有值）。</div>'
  );
  const set = (id, v) => { $(id).value = v == null ? '' : v; };
  set('#ceName', orig.name); set('#ceOrig', orig.original_title);
  set('#ceOv', orig.overview); set('#ceDate', orig.premiere_date);
  set('#ceYear', orig.production_year || ''); set('#ceTags', orig.tags.join(', '));
  set('#ceGenres', orig.genres.join(', ')); set('#ceRating', orig.official_rating);

  $('#ceSave').onclick = async (ev) => {
    const patch = {};
    const name = $('#ceName').value.trim();
    if (name !== orig.name) {
      if (!name) { toast('名称不能为空', 'err'); return; }
      patch.name = name;
    }
    if ($('#ceOrig').value.trim() !== orig.original_title) patch.original_title = $('#ceOrig').value.trim();
    if ($('#ceOv').value.trim() !== orig.overview) patch.overview = $('#ceOv').value.trim();
    if ($('#ceRating').value.trim() !== orig.official_rating) patch.official_rating = $('#ceRating').value.trim();
    const date = $('#ceDate').value.trim();
    if (date !== orig.premiere_date) {
      if (!date) { toast('发行日期留空表示不修改，要清空请直接在 Emby 里改', 'err'); return; }
      patch.premiere_date = date;
    }
    const year = Number($('#ceYear').value) || 0;
    if (year !== orig.production_year) {
      if (year <= 0) { toast('年份留空表示不修改，要清空请直接在 Emby 里改', 'err'); return; }
      patch.production_year = year;
    }
    const tags = cnSplitList($('#ceTags').value);
    if (tags.join('\u0001') !== orig.tags.join('\u0001')) patch.tags = tags;
    const genres = cnSplitList($('#ceGenres').value);
    if (genres.join('\u0001') !== orig.genres.join('\u0001')) patch.genres = genres;

    if (!Object.keys(patch).length) { toast('没有改动', 'info'); return; }
    ev.target.disabled = true;
    try {
      const r = await api('/api/items/update', {
        method: 'POST',
        body: Object.assign({ id: itemId }, patch),
      });
      toast('已保存：' + (r.updated || []).join(' / '), 'ok');
      $('#drawerHost').innerHTML = '';
      loadCnItems(S.cn.start);
    } catch (e) {
      toast(e.message, 'err');
      ev.target.disabled = false;
    }
  };

  $('#ceScrape').onclick = async (ev) => {
    ev.target.disabled = true;
    try {
      const r = await api('/api/cn/scrape', {
        method: 'POST',
        body: {
          id: itemId,
          overwrite_images: $('#cnOverwriteImg').checked,
          overwrite_title: $('#cnOverwriteTitle').checked,
        },
      });
      toast(cnOutcome(r), r.applied ? 'ok' : 'err');
      if (r.applied) { $('#drawerHost').innerHTML = ''; loadCnItems(S.cn.start); }
      else ev.target.disabled = false;
    } catch (e) { toast(e.message, 'err'); ev.target.disabled = false; }
  };
}

// ---------------- 设置 ----------------
function fillSettings() {
  const c = S.cfg || {};
  const sec = c.secrets || {};
  const set = (id, v) => { const el = $(id); if (el) el.value = v == null ? '' : v; };
  set('#stUrl', c.emby_url); set('#stUser', c.username);
  markSecret('#stKey', sec.emby_api_key);
  set('#stMt', c.metatube_url);
  markSecret('#stMtToken', sec.metatube_token);
  set('#stGfTree', c.gfriends_tree_url); set('#stGfCdn', c.gfriends_cdn);
  set('#stJb', c.javbus_url);
  markSecret('#stJbCookie', sec.javbus_cookie);
  set('#stJbInterval', c.javbus_interval_ms); set('#stConc', c.concurrency);
  const cn = c.cn_sites || {};
  set('#stCnXchina', cn.xchina); set('#stCnMadouqu', cn.madouqu);
  set('#stCnMadou', cn.madou); set('#stCn7mmtv', cn['7mmtv']);
  set('#stProxy', c.proxy);
  const oai = c.openai || {};
  set('#stOaiUrl', oai.base_url);
  markSecret('#stOaiKey', sec.openai_api_key);
  set('#stOaiModel', oai.model);
  $('#stOaiOn').checked = !!oai.enabled;
  $('#stInsecure').checked = !!c.insecure_tls;
  $('#stAutoRefresh').checked = !!c.auto_refresh;
  $('#stOverwrite').checked = !!c.overwrite_images;
  set('#stAuthUser', (c.auth || {}).username);
  $('#stPath').textContent = c.config_path || '';
  const av = $('#aboutVersion');
  if (av) av.textContent = c.version || '';
  const ar = $('#aboutRepo');
  if (ar) {
    ar.textContent = c.repo_url || '';
    ar.href = c.repo_url || '#';
  }
}

async function saveSettings() {
  const body = {
    emby_url: $('#stUrl').value.trim(),
    username: $('#stUser').value.trim(),
    password: $('#stPass').value,
    api_key: $('#stKey').value.trim(),
    metatube_url: $('#stMt').value.trim(),
    metatube_token: $('#stMtToken').value.trim(),
    gfriends_tree_url: $('#stGfTree').value.trim(),
    gfriends_cdn: $('#stGfCdn').value.trim(),
    javbus_url: $('#stJb').value.trim(),
    javbus_cookie: $('#stJbCookie').value.trim(),
    javbus_interval_ms: Number($('#stJbInterval').value) || 1500,
    concurrency: Number($('#stConc').value) || 4,
    cn_sites: {
      xchina: $('#stCnXchina').value.trim(),
      madouqu: $('#stCnMadouqu').value.trim(),
      madou: $('#stCnMadou').value.trim(),
      '7mmtv': $('#stCn7mmtv').value.trim(),
    },
    proxy: $('#stProxy').value.trim(),
    insecure_tls: $('#stInsecure').checked,
    auto_refresh: $('#stAutoRefresh').checked,
    overwrite_images: $('#stOverwrite').checked,
    openai: {
      base_url: $('#stOaiUrl').value.trim(),
      api_key: $('#stOaiKey').value,
      model: $('#stOaiModel').value.trim(),
      enabled: $('#stOaiOn').checked,
    },
  };
  try {
    await api('/api/config', { method: 'POST', body });
    await loadConfig();
    toast('设置已保存', 'ok');
  } catch (e) { toast(e.message, 'err'); }
}

// testOpenAI 把当前输入框里的接口地址 / Key / 模型发给后端做连通性探测。
// 即使「启用翻译」没勾也能测（探测不依赖开关）。
async function testOpenAI() {
  const el = $('#oaiTestResult');
  const btn = $('#btnOaiTest');
  el.textContent = '测试中…';
  el.style.color = '';
  btn.disabled = true;
  const body = {
    openai: {
      base_url: $('#stOaiUrl').value.trim(),
      api_key: $('#stOaiKey').value,
      model: $('#stOaiModel').value.trim(),
    },
  };
  try {
    const r = await api('/api/openai/test', { method: 'POST', body });
    el.textContent = r.message;
    el.style.color = r.ok ? 'var(--ok,#2e8b57)' : 'var(--err,#c0392b)';
  } catch (e) {
    el.textContent = '测试请求失败：' + e.message;
    el.style.color = 'var(--err,#c0392b)';
  } finally {
    btn.disabled = false;
  }
}

// ---------------- 配置与启动 ----------------
async function loadConfig() {
  S.cfg = await api('/api/config');
  const c = S.cfg;
  const sec = c.secrets || {};
  $('#lgUrl').value = c.emby_url || '';
  $('#lgUser').value = c.username || '';
  markSecret('#lgKey', sec.emby_api_key);
  $('#lgMt').value = c.metatube_url || '';
  markSecret('#lgMtToken', sec.metatube_token);
  $('#lgGfTree').value = c.gfriends_tree_url || '';
  $('#lgGfCdn').value = c.gfriends_cdn || '';
  $('#lgJb').value = c.javbus_url || '';
  markSecret('#lgJbCookie', sec.javbus_cookie);
  $('#lgProxy').value = c.proxy || '';
  $('#lgConc').value = c.concurrency || 4;
  $('#lgInsecure').checked = !!c.insecure_tls;
  $('#sbServer').textContent = c.user_name || (c.emby_url || '').replace(/^https?:\/\//, '') || '未连接';
}

async function checkStatus() {
  try {
    const st = await api('/api/emby/status');
    $('#sbDot').className = 'dot ' + (st.online ? 'on' : 'off');
    $('#sbState').textContent = st.online ? ('在线' + (st.user ? ' · ' + st.user : '')) : '离线';
    if (st.server && st.server.Version) $('#sbVer').textContent = 'Emby ' + st.server.Version;
    return st;
  } catch (e) {
    $('#sbDot').className = 'dot off';
    $('#sbState').textContent = '离线';
    return { online: false };
  }
}

function enterApp() {
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
  ensureLibs();
  loadStats();
  refreshGfState();
}

async function doLogin() {
  const mode = $('#loginMode button.on').dataset.mode;
  const btn = $('#lgBtn');
  const alertBox = $('#lgAlert');
  btn.disabled = true; btn.innerHTML = '<span class="spin"></span> 连接中…';
  alertBox.classList.add('hidden');
  try {
    const body = {
      url: $('#lgUrl').value.trim(),
      username: $('#lgUser').value.trim(),
      password: $('#lgPass').value,
      api_key: $('#lgKey').value.trim(),
      mode,
    };
    const r = await api('/api/emby/login', { method: 'POST', body });
    // 顺带保存高级配置
    await api('/api/config', {
      method: 'POST',
      body: {
        emby_url: body.url,
        metatube_url: $('#lgMt').value.trim(),
        metatube_token: $('#lgMtToken').value.trim(),
        gfriends_tree_url: $('#lgGfTree').value.trim(),
        gfriends_cdn: $('#lgGfCdn').value.trim(),
        javbus_url: $('#lgJb').value.trim(),
        javbus_cookie: $('#lgJbCookie').value.trim(),
        proxy: $('#lgProxy').value.trim(),
        concurrency: Number($('#lgConc').value) || 4,
        insecure_tls: $('#lgInsecure').checked,
      },
    });
    await loadConfig();
    toast('已连接：' + (r.user_name || 'Emby'), 'ok');
    enterApp();
  } catch (e) {
    alertBox.textContent = e.message;
    alertBox.classList.remove('hidden', 'ok');
  } finally {
    btn.disabled = false; btn.textContent = '连接并进入';
  }
}

// ---------------- 事件绑定 ----------------
function bind() {
  // 访问认证（进程自己的登录）
  $('#agBtn').onclick = doAuthLogin;
  $('#agPass').onkeydown = (e) => { if (e.key === 'Enter') doAuthLogin(); };
  $('#agUser').onkeydown = (e) => { if (e.key === 'Enter') $('#agPass').focus(); };
  $('#sbLogout').onclick = doAuthLogout;
  $('#stAuthSave').onclick = saveAuth;

  $$('#loginMode button').forEach((b) => b.onclick = () => {
    $$('#loginMode button').forEach((x) => x.classList.toggle('on', x === b));
    const k = b.dataset.mode === 'apikey';
    $('#lgKeyBox').classList.toggle('hidden', !k);
    $('#lgPwdBox').classList.toggle('hidden', k);
  });
  $('#lgBtn').onclick = doLogin;
  $('#lgSkip').onclick = () => { enterApp(); toast('已跳过登录，仅可使用 gfriends / javbus 相关功能', 'info'); };
  $('#lgPass').onkeydown = (e) => { if (e.key === 'Enter') doLogin(); };

  $$('#nav button').forEach((b) => b.onclick = () => switchView(b.dataset.view));

  $('#stReload').onclick = loadStats;
  $('#lbSearch').onclick = () => loadItems(0);
  $('#lbQ').onkeydown = (e) => { if (e.key === 'Enter') loadItems(0); };
  $('#lbNoPoster').onchange = () => loadItems(0);
  $('#lbLib').onchange = () => loadItems(0);
  $('#lbGrid').onclick = (e) => {
    const btn = e.target.closest('button[data-act]');
    if (!btn) return;
    const card = btn.closest('.mcard');
    if (btn.dataset.act === 'scrape') scrapeOne(card.dataset.id, btn);
    else openItemDetail(card.dataset.id);
  };
  $('#lbBatch').onclick = async () => {
    try {
      const r = await api('/api/items/scrape-batch', {
        method: 'POST',
        body: {
          parent_id: $('#lbLib').value,
          only_no_poster: $('#lbNoPoster').checked,
          limit: Number($('#lbBatchLimit').value) || 50,
          overwrite_images: $('#lbOverwrite').checked,
          refresh: $('#lbRefresh').checked,
        },
      });
      watchJob(r.job_id, '批量刮削影片', () => loadItems(S.lb.start));
    } catch (e) { toast(e.message, 'err'); }
  };

  $('#psSearch').onclick = () => loadPersons(0);
  $('#psQ').onkeydown = (e) => { if (e.key === 'Enter') loadPersons(0); };
  $('#psMissing').onchange = () => loadPersons(0);
  $('#psLib').onchange = () => loadPersons(0);
  $('#psList').onclick = (e) => {
    const btn = e.target.closest('button[data-act]');
    if (!btn) return;
    const card = btn.closest('.pcard');
    const act = btn.dataset.act;
    if (act === 'av') {
      // 「重写头像」= 强制覆盖（按钮文案已经说明了），「刮削头像」才看工具栏那个开关
      const hasImage = !!btn.closest('.pcard').querySelector('.av img');
      scrapeAvatar(card.dataset.id, card.dataset.name, btn, hasImage ? true : undefined);
    } else if (act === 'pick') {
      pickAvatar(card.dataset.id, card.dataset.name);
    } else if (act === 'prof') {
      openProfilePanel(card.dataset.id, card.dataset.name);
    }
  };
  $('#psBatch').onclick = async () => {
    try {
      const r = await api('/api/persons/avatars', {
        method: 'POST',
        body: {
          mode: 'missing', limit: Number($('#psLimit').value) || 100,
          source: $('#psSource').value, overwrite: $('#psOverwrite').checked,
          parent_id: $('#psLib').value,
        },
      });
      watchJob(r.job_id, '批量刮削演员头像', () => loadPersons(S.ps.start));
    } catch (e) { toast(e.message, 'err'); }
  };
  $('#gfReload').onclick = async () => {
    try {
      const r = await api('/api/gfriends/reload', { method: 'POST' });
      watchJob(r.job_id, '下载 gfriends 索引', refreshGfState);
    } catch (e) { toast(e.message, 'err'); }
  };
  $('#gfSearch').onclick = async () => {
    const q = $('#gfQ').value.trim();
    if (!q) return;
    $('#gfResult').innerHTML = '<div class="empty"><span class="spin"></span> 搜索中…</div>';
    try {
      const hits = await api('/api/gfriends/search?q=' + encodeURIComponent(q));
      if (!hits.length) { $('#gfResult').innerHTML = '<div class="empty">没有找到</div>'; return; }
      $('#gfResult').innerHTML = hits.map((h) => '<div style="margin-bottom:12px"><div style="font-size:12.5px;margin-bottom:6px"><b>' +
        esc(h.name) + '</b> <span style="color:var(--muted)">' + h.entries.length + ' 张</span></div>' +
        '<div style="display:flex;gap:8px;flex-wrap:wrap">' + h.entries.slice(0, 12).map((en) =>
          '<img loading="lazy" src="' + esc(imgSrc(en.f)) + '" title="' + esc(en.gz) + '" style="width:66px;height:90px;object-fit:cover;border-radius:6px;border:1px solid var(--border)">').join('') +
        '</div></div>').join('');
    } catch (e) { $('#gfResult').innerHTML = '<div class="empty">' + esc(e.message) + '</div>'; }
  };
  $('#gfQ').onkeydown = (e) => { if (e.key === 'Enter') $('#gfSearch').click(); };

  $('#pfBatchPage').onclick = () => batchProfile('page');
  $('#pfBatchLimit').onclick = () => batchProfile('limit');
  $('#pfHistory').onclick = openSyncHistory;

  $('#jbScan').onclick = jbScan;
  $('#jbStar').onkeydown = (e) => { if (e.key === 'Enter') jbScan(); };
  $('#jbProbe').onclick = jbProbe;

  $('#cnSearch').onclick = () => loadCnItems(0);
  $('#cnQ').onkeydown = (e) => { if (e.key === 'Enter') loadCnItems(0); };
  // 换库就清空选择 —— 上一个库勾中的 id 在新库里没有意义。
  $('#cnLib').onchange = () => { S.cn.sel.clear(); loadCnItems(0); };
  $('#cnMissing').onchange = () => loadCnItems(0);
  $('#cnSelAll').onchange = (e) => {
    const on = e.target.checked;
    S.cn.items.forEach((it) => { if (on) S.cn.sel.add(it.Id); else S.cn.sel.delete(it.Id); });
    // 只重画选中态，不重新请求：勾选是纯前端状态，走一趟网络纯属浪费。
    $$('#cnGrid .mcard').forEach((card) => {
      const pick = card.querySelector('input[data-act="cnpick"]');
      if (pick) pick.checked = on;
      card.classList.toggle('sel', on);
    });
    cnSyncSelUI();
  };
  $('#cnBatch').onclick = cnScrapeSelected;
  $('#cnGrid').onclick = (e) => {
    const card = e.target.closest('.mcard');
    if (!card) return;
    const pick = e.target.closest('input[data-act="cnpick"]');
    if (pick) {
      if (pick.checked) S.cn.sel.add(card.dataset.id);
      else S.cn.sel.delete(card.dataset.id);
      card.classList.toggle('sel', pick.checked);
      cnSyncSelUI();
      return;
    }
    const btn = e.target.closest('button[data-act]');
    if (!btn || btn.disabled) return;
    if (btn.dataset.act === 'cnscrape') cnScrapeOne(card.dataset.id, btn);
    else openCnEditor(card.dataset.id);
  };

  $('#stSave').onclick = saveSettings;
  $('#stTest').onclick = async () => {
    const st = await checkStatus();
    $('#stMsg').classList.toggle('hidden', false);
    $('#stMsg').className = 'alert' + (st.online ? ' ok' : '');
    $('#stMsg').textContent = st.online
      ? ('连接成功：' + (st.server ? (st.server.ServerName || '') + ' ' + (st.server.Version || '') : '') + (st.user ? '（用户 ' + st.user + '）' : ''))
      : ('连接失败：' + (st.error || '未知错误'));
  };
  $('#stLogin').onclick = async () => {
    const key = $('#stKey').value.trim();
    try {
      const r = await api('/api/emby/login', {
        method: 'POST',
        body: { url: $('#stUrl').value.trim(), username: $('#stUser').value.trim(), password: $('#stPass').value, api_key: key, mode: key ? 'apikey' : 'password' },
      });
      await loadConfig(); await checkStatus();
      $('#stMsg').className = 'alert ok';
      $('#stMsg').classList.remove('hidden');
      $('#stMsg').textContent = '登录成功：' + (r.user_name || '');
    } catch (e) {
      $('#stMsg').className = 'alert';
      $('#stMsg').classList.remove('hidden');
      $('#stMsg').textContent = e.message;
    }
  };
  $('#stLogout').onclick = async () => {
    await api('/api/emby/logout', { method: 'POST' });
    await loadConfig(); await checkStatus();
    toast('已退出登录', 'info');
  };
  $('#stMtTest').onclick = async () => {
    $('#stMtProviders').textContent = '查询中…';
    try {
      await api('/api/config', { method: 'POST', body: { metatube_url: $('#stMt').value.trim(), metatube_token: $('#stMtToken').value.trim() } });
      await loadConfig();
      const list = await api('/api/metatube/providers');
      $('#stMtProviders').textContent = '可用：' + list.join(' / ');
    } catch (e) { $('#stMtProviders').textContent = '失败：' + e.message; }
  };
}

// ---------------- 启动 ----------------
// startApp 是「过了访问认证之后」才走的路：读配置 → 探 Emby → 决定进主界面还是停在连接页。
async function startApp() {
  try {
    await loadConfig();
    const st = await checkStatus();
    if (st.online && S.cfg && S.cfg.logged_in) { enterApp(); return; }
  } catch (e) {
    toast('初始化失败：' + e.message, 'err');
  }
  // 还没连上 Emby（或没登录）→ 停在 Emby 连接页
  $('#authGate').classList.add('hidden');
  $('#login').classList.remove('hidden');
}

(async function boot() {
  bind();
  let st;
  try {
    st = await authStatus();
  } catch (e) {
    showAuthGate('无法获取登录状态：' + e.message);
    return;
  }
  if (!st.authenticated) {
    $('#agUser').value = st.username || 'admin';
    showAuthGate(st.password_is_new
      ? '当前用的还是首次启动自动生成的密码，登录后建议在「设置 → 访问认证」里改掉。'
      : '');
    return;
  }
  await startApp();
})();
