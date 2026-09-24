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
  if (v === 'persons') { ensureLibs(); loadPersons(0); refreshGfState(); }
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
  return '<div class="pcard" data-id="' + esc(p.Id) + '" data-name="' + esc(p.Name) + '">' +
    '<div class="av">' + img + '</div>' +
    '<div class="info"><b title="' + esc(p.Name) + '">' + esc(p.Name) + '</b>' +
    '<div class="sub">' + (p.has_image ? '<span class="tag tag-green">已有头像</span>'
      : (p.gfriends > 0 ? '<span class="tag tag-blue">gfriends 命中 ' + p.gfriends + '</span>' : '<span class="tag tag-amber">库中无记录</span>')) +
    '</div></div>' +
    '<div class="ops">' +
    '<button class="btn btn-sm btn-primary" data-act="av">刮削</button>' +
    '<button class="btn btn-sm" data-act="pick">选择</button>' +
    '</div></div>';
}

async function scrapeAvatar(personId, name, btn) {
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="spin"></span>'; }
  try {
    const r = await api('/api/persons/avatar', {
      method: 'POST',
      body: {
        person_id: personId, name: name,
        source: $('#psSource').value,
        overwrite: $('#psOverwrite').checked,
      },
    });
    toast(name + '：' + r.message, 'ok');
    loadPersons(S.ps.start);
  } catch (e) {
    toast(name + '：' + e.message, 'err');
    if (btn) { btn.disabled = false; btn.textContent = '刮削'; }
  }
}

// gfFileName 从搜索接口下发的完整 CDN 地址里取回裸文件名，只用于提示文案。
function gfFileName(url) {
  try {
    const p = decodeURIComponent(new URL(url, location.href).pathname);
    return p.slice(p.lastIndexOf('/') + 1);
  } catch (_) { return String(url || ''); }
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
        esc(h.name) + '（' + h.entries.length + ' 张）</div><div style="display:flex;gap:8px;flex-wrap:wrap">' +
        h.entries.map((en, i) => '<img loading="lazy" data-name="' + esc(h.name) + '" data-file="' + esc(en.f) + '" data-group="' + esc(en.g) + '" ' +
          'src="' + esc(imgSrc(en.f)) + '" alt="" title="' + esc(en.gz + ' / ' + gfFileName(en.f)) + '" ' +
          'style="width:76px;height:104px;object-fit:cover;border-radius:6px;border:1px solid var(--border);cursor:pointer">').join('') +
        '</div></div>').join('');
      $$('#pkList img').forEach((im) => im.onclick = async () => {
        im.style.opacity = '.4';
        try {
          const r = await api('/api/persons/avatar', {
            method: 'POST',
            body: { person_id: personId, name: im.dataset.name, source: 'gfriends', group: im.dataset.group, file: im.dataset.file, overwrite: true },
          });
          toast(r.message, 'ok'); $('#drawerHost').innerHTML = ''; loadPersons(S.ps.start);
        } catch (e) { toast(e.message, 'err'); im.style.opacity = '1'; }
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
    '<button class="btn btn-sm" id="jbCopyOne">复制当前番号</button>' +
    '<button class="btn btn-sm" id="jbCopyAll">复制全部</button></h3><div class="maglist" id="jbMagList"></div></div>' +
    (res.local_unmatched && res.local_unmatched.length
      ? '<div class="card"><h3>本地有、javbus 未列出（' + res.local_unmatched.length + '）</h3>' +
        '<div style="max-height:180px;overflow:auto" class="mono">' + res.local_unmatched.map(esc).join('　') + '</div></div>' : '');

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
    if (btn.dataset.act === 'av') scrapeAvatar(card.dataset.id, card.dataset.name, btn);
    else pickAvatar(card.dataset.id, card.dataset.name);
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
