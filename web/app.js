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
  jb: { scan: null, selected: new Set(), magnets: [], targets: [] },
  watching: {},
};

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

function fmtSize(b) {
  if (b == null || b < 0) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0, n = Number(b);
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 2 : 1)) + ' ' + u[i];
}
const num = (n) => (n == null ? '0' : Number(n).toLocaleString('zh-CN'));

// ---------------- Emby 图片地址 ----------------
function embyImg(itemId, tag, h) {
  if (!S.cfg || !S.cfg.emby_url || !tag) return '';
  return S.cfg.emby_url.replace(/\/$/, '') + '/Items/' + itemId + '/Images/Primary?maxHeight=' +
    (h || 300) + '&quality=90&tag=' + encodeURIComponent(tag) + '&api_key=' + encodeURIComponent(S.cfg.token || '');
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
const VIEW_TITLES = { stats: '概览统计', library: '媒体库刮削', persons: '演员头像', javbus: '番号补全', settings: '设置' };

function switchView(v) {
  S.view = v;
  $$('#nav button').forEach((b) => b.classList.toggle('on', b.dataset.view === v));
  $$('.view').forEach((s) => s.classList.toggle('on', s.id === 'view-' + v));
  $('#viewTitle').textContent = VIEW_TITLES[v] || v;
  if (v === 'stats') loadStats();
  if (v === 'library') { ensureLibs(); loadItems(0); }
  if (v === 'persons') { ensureLibs(); loadPersons(0); refreshGfState(); }
  if (v === 'settings') fillSettings();
}

// ---------------- 概览 ----------------
async function loadStats() {
  $('#statGrid').innerHTML = '<div class="empty"><span class="spin"></span> 正在统计…</div>';
  $('#stLibs').innerHTML = '<div class="empty">加载中…</div>';
  const withSize = $('#stSize').checked;
  try {
    const d = await api('/api/stats?size=' + (withSize ? 'true' : 'false'));
    const c = d.counts || {};
    const p = d.persons || {};
    const cards = [
      { k: '电影', v: num(c.MovieCount), c: 'accent' },
      { k: '剧集', v: num(c.SeriesCount), c: '' },
      { k: '集数', v: num(c.EpisodeCount), c: '' },
      { k: '演员总数', v: num(p.total), c: 'violet', s: '已扫描 ' + num(p.scanned) },
      { k: '缺头像演员', v: num(p.missing_image), c: 'warn' },
      { k: '媒体库体积', v: withSize ? fmtSize(d.total_size) : '—', c: 'green', s: withSize ? '' : '勾选左侧选项后统计' },
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
        (withSize ? '<th class="num">体积</th>' : '') + '</tr></thead><tbody>' +
        libs.map((l) => '<tr><td><b>' + esc(l.name) + '</b></td><td><span class="tag">' + esc(l.collection_type || 'mixed') + '</span></td>' +
          '<td class="num">' + num(l.item_count) + '</td><td class="num">' + num(l.movie_count) + '</td>' +
          '<td class="num">' + num(l.series_count) + '</td><td class="num">' + num(l.episode_count) + '</td>' +
          (withSize ? '<td class="num">' + fmtSize(l.total_size) + '</td>' : '') + '</tr>').join('') +
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
        h.entries.map((en, i) => '<img data-name="' + esc(h.name) + '" data-file="' + esc(en.f) + '" data-group="' + esc(en.g) + '" ' +
          'src="' + esc(en.f) + '" title="' + esc(en.gz + ' / ' + en.f) + '" ' +
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
      renderMagnets();
    });
  } catch (e) { toast(e.message, 'err'); }
}

function renderMagnets() {
  const card = $('#jbMagCard');
  if (!card) return;
  card.style.display = 'block';
  const list = $('#jbMagList');
  const all = [];
  list.innerHTML = S.jb.magnets.map((r) => {
    if (r.error && !(r.magnets || []).length) {
      return '<div style="padding:10px;border-bottom:1px solid var(--border)"><b class="mono">' + esc(r.number) + '</b>' +
        ' <span class="tag tag-amber">' + esc(r.error) + '</span></div>';
    }
    const rows = (r.magnets || []).map((m) => {
      all.push(m.link);
      return '<div class="magrow"><div class="n">' + esc(m.name) + '<br><i>' + esc(m.link.slice(0, 110)) + '…</i></div>' +
        '<span class="sz">' + esc(m.size || '') + '</span><span class="dt">' + esc(m.date || '') + '</span>' +
        '<button class="btn btn-sm" data-copy="' + esc(m.link) + '">复制</button></div>';
    }).join('');
    return '<div style="padding:9px 11px 3px;background:var(--surface-2);border-bottom:1px solid var(--border)">' +
      '<b class="mono">' + esc(r.number) + '</b> <span style="color:var(--muted);font-size:12px">' +
      esc(r.title || '') + '</span></div>' + rows;
  }).join('');
  $$('button[data-copy]', list).forEach((b) => b.onclick = () => {
    navigator.clipboard.writeText(b.dataset.copy).then(() => toast('已复制磁力链接', 'ok'), () => toast('复制失败', 'err'));
  });
  const ca = $('#jbCopyAll');
  if (ca) ca.onclick = () => navigator.clipboard.writeText(all.join('\n'))
    .then(() => toast('已复制 ' + all.length + ' 条磁力链接', 'ok'), () => toast('复制失败', 'err'));
}

// ---------------- 设置 ----------------
function fillSettings() {
  const c = S.cfg || {};
  const set = (id, v) => { const el = $(id); if (el) el.value = v == null ? '' : v; };
  set('#stUrl', c.emby_url); set('#stUser', c.username); set('#stKey', c.api_key);
  set('#stMt', c.metatube_url); set('#stMtToken', c.metatube_token);
  set('#stGfTree', c.gfriends_tree_url); set('#stGfCdn', c.gfriends_cdn);
  set('#stJb', c.javbus_url); set('#stJbCookie', c.javbus_cookie);
  set('#stJbInterval', c.javbus_interval_ms); set('#stConc', c.concurrency);
  set('#stProxy', c.proxy);
  $('#stInsecure').checked = !!c.insecure_tls;
  $('#stAutoRefresh').checked = !!c.auto_refresh;
  $('#stOverwrite').checked = !!c.overwrite_images;
  $('#stPath').textContent = c.config_path || '';
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
    proxy: $('#stProxy').value.trim(),
    insecure_tls: $('#stInsecure').checked,
    auto_refresh: $('#stAutoRefresh').checked,
    overwrite_images: $('#stOverwrite').checked,
  };
  try {
    await api('/api/config', { method: 'POST', body });
    await loadConfig();
    toast('设置已保存', 'ok');
  } catch (e) { toast(e.message, 'err'); }
}

// ---------------- 配置与启动 ----------------
async function loadConfig() {
  S.cfg = await api('/api/config');
  const c = S.cfg;
  $('#lgUrl').value = c.emby_url || '';
  $('#lgUser').value = c.username || '';
  $('#lgKey').value = c.api_key || '';
  $('#lgMt').value = c.metatube_url || '';
  $('#lgMtToken').value = c.metatube_token || '';
  $('#lgGfTree').value = c.gfriends_tree_url || '';
  $('#lgGfCdn').value = c.gfriends_cdn || '';
  $('#lgJb').value = c.javbus_url || '';
  $('#lgJbCookie').value = c.javbus_cookie || '';
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
  $('#stSize').onchange = loadStats;
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
(async function boot() {
  bind();
  try {
    await loadConfig();
    const st = await checkStatus();
    if (st.online && S.cfg && S.cfg.logged_in) {
      enterApp();
    } else if (st.online && S.cfg && S.cfg.token) {
      enterApp();
    }
  } catch (e) {
    toast('初始化失败：' + e.message, 'err');
  }
})();
