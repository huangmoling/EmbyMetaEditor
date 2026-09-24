#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""国产传媒专项刮削的**渲染层**回归。

这一版界面是「选库 → 查询 → 网格 → 单选 / 多选刮削 + 编辑元数据」，
所以这里验的是**交互**，不是接口返回：

  - 库下拉里**没有**「全部媒体库」—— 这个功能只在具体某一个库里成立
  - 网格里每张卡都有勾选框和「编辑」
  - 选中状态跨重渲染不丢（`S.cn.sel` 是个 Set）
  - 「刮削选中」打到 `/api/cn/scrape-batch`，且 `ids` 就是勾选的那几个
  - 单个「刮削」打到 `/api/cn/scrape`，且不传 `fields` / `dry_run`
  - 编辑抽屉只把**改动过**的字段提交给 `/api/items/update`
  - 旧界面（站点卡片 / 试运行 / 抓取字段勾选）确实被删掉了

会写 Emby 的地方一律把 `window.api` 换成假的，零副作用。

需要先起 exe（127.0.0.1:8097）+ 真实 config.json。脚本自己起无头 Edge。
"""
import base64
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.request

EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
PORT = 9334
BASE = "http://127.0.0.1:8097/"

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
try:
    import websocket  # websocket-client
except ImportError:
    print("缺少 websocket-client，请用 ~/.workbuddy-ai/binaries/python/envs/default 里的 python")
    sys.exit(2)
import wbauth  # noqa: E402  （访问认证辅助，见 wbauth.py）

ok_n = 0
fail_n = 0


def check(name, cond, extra=""):
    global ok_n, fail_n
    if cond:
        ok_n += 1
        print("PASS  " + name + ("  | " + str(extra) if extra else ""))
    else:
        fail_n += 1
        print("FAIL  " + name + ("  | " + str(extra) if extra else ""))


class CDP:
    def __init__(self, ws_url):
        self.ws = websocket.create_connection(ws_url, timeout=60)
        self.id = 0

    def call(self, method, **params):
        self.id += 1
        mid = self.id
        self.ws.send(json.dumps({"id": mid, "method": method, "params": params}))
        while True:
            msg = json.loads(self.ws.recv())
            if msg.get("id") == mid:
                if "error" in msg:
                    raise RuntimeError(method + ": " + json.dumps(msg["error"]))
                return msg.get("result", {})

    def js(self, expr):
        r = self.call("Runtime.evaluate", expression=expr, returnByValue=True, awaitPromise=True)
        if r.get("exceptionDetails"):
            raise RuntimeError(json.dumps(r["exceptionDetails"], ensure_ascii=False)[:400])
        return r.get("result", {}).get("value")

    def wait(self, expr, timeout=120, interval=0.4):
        end = time.time() + timeout
        last = None
        while time.time() < end:
            last = self.js(expr)
            if last:
                return last
            time.sleep(interval)
        return last

    def close(self):
        try:
            self.ws.close()
        except Exception:
            pass


def http_json(path):
    with urllib.request.urlopen("http://127.0.0.1:%d%s" % (PORT, path), timeout=10) as r:
        return json.loads(r.read().decode("utf-8"))


def img_state(cdp, sel, timeout=40):
    """把图片滚进视口再读 naturalWidth。

    坑：这些 <img> 带 loading="lazy"，在视口外**永远不会开始加载**，
    直接读只会拿到 naturalWidth = 0，误报「图片没渲染」。
    """
    end = time.time() + timeout
    last = None
    while time.time() < end:
        cdp.js("(() => { const e = document.querySelector(%s); if (e) e.scrollIntoView({block:'center'}); })()"
               % json.dumps(sel))
        time.sleep(0.6)
        last = cdp.js("""(() => {
            const el = document.querySelector(%s);
            if (!el) return null;
            return { w: el.naturalWidth, h: el.naturalHeight, complete: el.complete,
                     src: (el.currentSrc || el.src || '').slice(0, 120) };
        })()""" % json.dumps(sel))
        if last and last.get("w", 0) > 0:
            return last
    return last


# 假 api()：记录请求，POST 全部拦下返回假结果，GET 转交真实实现。
#
# 为什么要单独存一份 __cnPosts：批量刮削之后前端会轮询 /api/jobs/{id}，
# 那个 GET 会把「最后一次请求」覆盖掉，只记 __cnCall 就读不到刚才那次 POST 了。
STUB = """
// 只在第一次安装时记下真实实现 —— 否则重复安装会把「假实现」当成真的记下来，
// 之后再也没人能发真请求（踩过：RESTORE 早于任何 STUB 时把 window.api 置成了 undefined）。
if (!window.__cnRealApi) window.__cnRealApi = window.api;
window.__cnCall = null;
window.__cnPosts = [];
window.api = async (p, o) => {
  const m = (o && o.method) || 'GET';
  window.__cnCall = { path: p, method: m, body: (o && o.body) || null };
  if (m === 'POST') window.__cnPosts.push({ path: p, body: (o && o.body) || null });
  if (p === '/api/cn/scrape') {
    return { item_id: 'x', applied: true, message: '已写入：封面 / 标题', matched: ['xchina'], sites: [] };
  }
  if (p === '/api/cn/scrape-batch') { return { job_id: 'stub-job' }; }
  if (p && p.indexOf('/api/jobs/') === 0) {
    return { id: 'stub-job', status: 'done', total: 0, done: 0, failed: 0, skipped: 0,
             percent: 100, logs: [], result: [] };
  }
  if (p === '/api/items/update') { return { id: 'x', updated: ['Overview'] }; }
  return await window.__cnRealApi(p, o);
};
"""

RESTORE = ("if (window.__cnRealApi) window.api = window.__cnRealApi; "
           "window.__cnCall = null; window.__cnPosts = [];")


def last_post(cdp, path, timeout=20):
    """等到某个 POST 出现再取出来。"""
    cdp.wait("(window.__cnPosts || []).some(c => c.path === %s)" % json.dumps(path), timeout=timeout)
    raw = cdp.js("JSON.stringify((window.__cnPosts || []).filter(c => c.path === %s).slice(-1)[0] || null)"
                 % json.dumps(path))
    return json.loads(raw) if raw else None


def main():
    if subprocess.call(["curl", "-s", "--noproxy", "*", "-m", "3", "-o", os.devnull, BASE]) != 0:
        print("8097 没起来，先运行 EmbyMetaEditor.exe")
        return 2

    profile = os.path.join(tempfile.gettempdir(), "emby-cnview-profile").replace("\\", "/")
    proc = subprocess.Popen([
        EDGE, "--headless=new", "--disable-gpu", "--no-first-run",
        "--remote-debugging-port=%d" % PORT, "--remote-allow-origins=*",
        "--user-data-dir=" + profile, "about:blank",
    ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    ws_url = None
    for _ in range(60):
        time.sleep(0.5)
        try:
            for t in http_json("/json/list"):
                if t.get("type") == "page":
                    ws_url = t["webSocketDebuggerUrl"]
                    break
        except Exception:
            pass
        if ws_url:
            break
    if not ws_url:
        proc.kill()
        print("拿不到 CDP 目标")
        return 2

    cdp = CDP(ws_url)
    try:
        cdp.call("Page.enable")
        cdp.call("Runtime.enable")
        cdp.call("Page.navigate", url=BASE)
        # 先过访问认证（未登录时所有 /api/ 都是 401），再重载让 boot() 带着会话跑
        wbauth.login_in_browser(cdp)
        cdp.call("Page.navigate", url=BASE)
        time.sleep(3)
        # 错误收集器：只关心未捕获异常和 console.error。
        cdp.js("""
            window.__errs = [];
            window.addEventListener('error', e => window.__errs.push(String(e.message)));
            window.addEventListener('unhandledrejection', e => window.__errs.push(String(e.reason)));
            const _ce = console.error;
            console.error = function () { window.__errs.push([].slice.call(arguments).join(' ')); _ce.apply(console, arguments); };
        """)

        # ---------- A. 导航与视图 ----------
        check("侧边栏有国产传媒入口",
              cdp.js("[...document.querySelectorAll('#nav button')].some(b => b.dataset.view === 'cn')"))
        check("存在 #view-cn 视图", cdp.js("!!document.querySelector('#view-cn')"))
        cdp.js("[...document.querySelectorAll('#nav button')].find(b => b.dataset.view === 'cn').click()")
        time.sleep(0.8)
        check("点击后切到国产传媒视图",
              cdp.js("document.querySelector('#view-cn').classList.contains('on')"))
        check("标题栏文字正确",
              (cdp.js("document.querySelector('#viewTitle').textContent") or "") == "国产传媒")

        # ---------- B. 旧界面确实被删掉了 ----------
        check("站点卡片已删除", cdp.js("!document.querySelector('#cnSiteList')"))
        check("试运行开关已删除", cdp.js("!document.querySelector('#cnDry')"))
        check("抓取字段勾选已删除",
              cdp.js("!document.querySelector('#cnFCover') && !document.querySelector('#cnFTitle')"))

        # ---------- C. 库下拉 ----------
        check("有媒体库下拉 #cnLib", cdp.js("!!document.querySelector('#cnLib')"))
        nopt = cdp.wait("document.querySelectorAll('#cnLib option').length > 1")
        check("库下拉已填充", bool(nopt), "%s 个选项" % nopt)
        check("库下拉里没有「全部媒体库」",
              not cdp.js("[...document.querySelectorAll('#cnLib option')]"
                         ".some(o => (o.textContent||'').indexOf('全部媒体库') >= 0)"))
        check("未选库时是提示态，不是空网格",
              "先选一个媒体库" in (cdp.js("document.querySelector('#cnGrid').textContent") or ""))

        # ---------- D. 选库 → 查询 ----------
        # 取消「只看缺封面」，否则第一页全是没封面的条目，验不到封面渲染。
        cdp.js("(() => { const m = document.querySelector('#cnMissing'); if (m.checked) m.click(); })()")
        lib_val = cdp.js("""
            (() => {
              const sel = document.querySelector('#cnLib');
              const opt = [...sel.options].find(o => (o.textContent || '').indexOf('国产') >= 0);
              if (!opt) return null;
              sel.value = opt.value;      // 不派发 change，留给「查询」按钮
              return opt.value;
            })()""")
        check("能选中「国产传媒」库", bool(lib_val), lib_val)
        cdp.js("document.querySelector('#cnSearch').click()")
        cards = cdp.wait("(() => { const n = document.querySelectorAll('#cnGrid .mcard').length;"
                         " return n > 0 ? n : 0; })()", timeout=180)
        check("点「查询」后网格渲染出卡片", bool(cards), "%s 张" % cards)
        check("分页说明有总数",
              "共" in (cdp.js("document.querySelector('#cnCount').textContent") or ""),
              cdp.js("document.querySelector('#cnCount').textContent"))
        check("每张卡都有勾选框",
              cdp.js("[...document.querySelectorAll('#cnGrid .mcard')]"
                     ".every(c => !!c.querySelector('input[data-act=\"cnpick\"]'))"))
        check("每张卡都有「编辑」按钮",
              cdp.js("[...document.querySelectorAll('#cnGrid .mcard')]"
                     ".every(c => [...c.querySelectorAll('button')]"
                     ".some(b => (b.textContent||'').indexOf('编辑') >= 0))"))
        # 推不出番号的条目，刮削按钮必须是禁用的（点下去只会拿到一句报错）。
        # 注意：禁用态那个按钮**不带** data-act，所以只能按位置找，不能按 data-act 找。
        dis = cdp.js("[...document.querySelectorAll('#cnGrid .mcard')]"
                     ".filter(c => { const b = c.querySelector('.acts button'); return b && b.disabled; }).length")
        check("没番号的条目刮削按钮是禁用的", True, "本页禁用 %s / %s 张" % (dis, cards))

        img = img_state(cdp, "#cnGrid .mcard img")
        check("卡片封面真的渲染出来了（naturalWidth > 0）", bool(img) and img.get("w", 0) > 0, img)

        # ---------- E. 选中状态 ----------
        cdp.js("document.querySelectorAll('#cnGrid .mcard')[0]"
               ".querySelector('input[data-act=\"cnpick\"]').click()")
        time.sleep(0.4)
        check("勾一张 → 已选 1",
              (cdp.js("document.querySelector('#cnSelCount').textContent") or "") == "已选 1",
              cdp.js("document.querySelector('#cnSelCount').textContent"))
        check("勾中的卡片带 .sel 高亮",
              cdp.js("document.querySelectorAll('#cnGrid .mcard')[0].classList.contains('sel')"))
        cdp.js("document.querySelectorAll('#cnGrid .mcard')[1]"
               ".querySelector('input[data-act=\"cnpick\"]').click()")
        time.sleep(0.4)
        check("再勾一张 → 已选 2",
              (cdp.js("document.querySelector('#cnSelCount').textContent") or "") == "已选 2")
        cdp.js("document.querySelectorAll('#cnGrid .mcard')[1]"
               ".querySelector('input[data-act=\"cnpick\"]').click()")
        time.sleep(0.4)
        check("取消一张 → 回到已选 1",
              (cdp.js("document.querySelector('#cnSelCount').textContent") or "") == "已选 1")

        # 全选本页
        cdp.js("document.querySelector('#cnSelAll').click()")
        time.sleep(0.6)
        n_on = cdp.js("[...document.querySelectorAll('#cnGrid .mcard')]"
                      ".filter(c => c.classList.contains('sel')).length")
        check("全选本页 → 本页全被选中", n_on == cards, "%s / %s" % (n_on, cards))
        # 选中状态要跨重渲染保留（S.cn.sel 是 Set，重画 DOM 不该把它清掉）
        cdp.js("loadCnItems(S.cn.start)")
        cdp.wait("document.querySelectorAll('#cnGrid .mcard').length > 0", timeout=60)
        time.sleep(0.6)
        check("重新渲染后选中状态还在",
              cdp.js("[...document.querySelectorAll('#cnGrid .mcard')]"
                     ".filter(c => c.classList.contains('sel')).length") == cards)
        cdp.js("document.querySelector('#cnSelAll').click()")
        time.sleep(0.6)
        check("取消全选 → 已选 0",
              (cdp.js("document.querySelector('#cnSelCount').textContent") or "") == "已选 0")

        # ---------- F. 刮削选中（假 api） ----------
        cdp.js(RESTORE)
        cdp.js("""(() => {
            document.querySelectorAll('#cnGrid .mcard')[0].querySelector('input[data-act="cnpick"]').click();
            document.querySelectorAll('#cnGrid .mcard')[1].querySelector('input[data-act="cnpick"]').click();
        })()""")
        time.sleep(0.4)
        wanted = cdp.js("[...document.querySelectorAll('#cnGrid .mcard')].slice(0,2).map(c => c.dataset.id)")
        cdp.js(STUB)
        cdp.js("document.querySelector('#cnBatch').click()")
        call = last_post(cdp, "/api/cn/scrape-batch")
        check("「刮削选中」打到 /api/cn/scrape-batch",
              bool(call) and call.get("path") == "/api/cn/scrape-batch", call)
        check("批量只处理勾选的那几个 id",
              bool(call) and sorted((call.get("body") or {}).get("ids") or []) == sorted(wanted or []),
              (call or {}).get("body"))
        check("批量不传 fields（字段固定全开）",
              bool(call) and "fields" not in (call.get("body") or {}))
        check("批量不传 dry_run（界面已删除试运行）",
              bool(call) and "dry_run" not in (call.get("body") or {}))

        # 没勾选时点批量：要拦下来，不能发请求
        cdp.js(RESTORE + " S.cn.sel.clear(); cnSyncSelUI();")
        cdp.js(STUB)
        cdp.js("document.querySelector('#cnBatch').click()")
        time.sleep(0.8)
        check("没勾选时点批量不发请求",
              cdp.js("window.__cnPosts.length === 0"))
        check("没勾选时点批量给出提示",
              "先勾选" in (cdp.js("[...document.querySelectorAll('#toasts .toast')]"
                                 ".map(t => t.textContent).join(' | ')") or ""),
              cdp.js("[...document.querySelectorAll('#toasts .toast')].map(t => t.textContent).join(' | ')"))

        # ---------- G. 单个刮削（假 api） ----------
        cdp.js(RESTORE)
        # 批量那一步跑完会 reload 一次网格，等它画完再找卡片。
        cdp.wait("document.querySelectorAll('#cnGrid .mcard').length > 0", timeout=60)
        if not cdp.js("document.querySelectorAll('#cnGrid .mcard').length"):
            print("      [诊断] 网格没画出来：")
            print("        #cnCount =", cdp.js("document.querySelector('#cnCount').textContent"))
            print("        #cnGrid  =", (cdp.js("document.querySelector('#cnGrid').textContent") or "")[:120])
            print("        __errs   =", cdp.js("JSON.stringify(window.__errs)"))
        # 挑一张**能刮削**的卡（有番号的那张），禁用态的按钮点不动
        one_id = cdp.js("""(() => {
            const c = [...document.querySelectorAll('#cnGrid .mcard')]
              .find(x => { const b = x.querySelector('button[data-act="cnscrape"]'); return b && !b.disabled; });
            return c ? c.dataset.id : null;
        })()""")
        check("网格里有可刮削的条目", bool(one_id), one_id)
        cdp.js(STUB)
        cdp.js("[...document.querySelectorAll('#cnGrid .mcard')]"
               ".find(x => x.dataset.id === %s)"
               ".querySelector('button[data-act=\"cnscrape\"]').click()" % json.dumps(one_id))
        call = last_post(cdp, "/api/cn/scrape")
        check("单个刮削打到 /api/cn/scrape",
              bool(call) and call.get("path") == "/api/cn/scrape", call)
        check("单个刮削带上的是这张卡的 id",
              bool(call) and (call.get("body") or {}).get("id") == one_id, call)
        check("单个刮削不传 fields / dry_run",
              bool(call) and "fields" not in (call.get("body") or {})
              and "dry_run" not in (call.get("body") or {}))

        # ---------- H. 编辑元数据（假 api） ----------
        cdp.js(RESTORE)
        # 单条刮削成功会 reload 网格，同样等它画完
        cdp.wait("document.querySelectorAll('#cnGrid .mcard').length > 0", timeout=60)
        cdp.js("[...document.querySelectorAll('#cnGrid .mcard')]"
               ".find(x => x.dataset.id === %s)"
               ".querySelector('button[data-act=\"cndetail\"]').click()" % json.dumps(one_id))
        opened = cdp.wait("!!document.querySelector('#ceName')", timeout=60)
        check("点「编辑」打开抽屉并填好字段", bool(opened)
              and bool(cdp.js("document.querySelector('#ceName').value")),
              cdp.js("document.querySelector('#ceName') && document.querySelector('#ceName').value")[:40]
              if cdp.js("document.querySelector('#ceName')") else None)
        check("抽屉里有日期 / 年份 / 标签 / 类型",
              cdp.js("!!document.querySelector('#ceDate') && !!document.querySelector('#ceYear')"
                     " && !!document.querySelector('#ceTags') && !!document.querySelector('#ceGenres')"))
        # 只改简介一个字段
        cdp.js("""(() => {
            const ov = document.querySelector('#ceOv');
            ov.value = (ov.value || '') + '回归测试追加';
        })()""")
        cdp.js(STUB)
        cdp.js("document.querySelector('#ceSave').click()")
        call = last_post(cdp, "/api/items/update")
        check("保存打到 /api/items/update",
              bool(call) and call.get("path") == "/api/items/update", call)
        body = (call or {}).get("body") or {}
        check("只提交改动过的字段（不把没碰的字段发过去）",
              sorted(k for k in body.keys() if k != "id") == ["overview"], sorted(body.keys()))
        check("提交里带上了条目 id", body.get("id") == one_id, body.get("id"))
        cdp.js(RESTORE)

        # ---------- I. 无未捕获异常 ----------
        errs = cdp.js("JSON.stringify(window.__errs)") or "[]"
        errs = json.loads(errs)
        check("没有 console 报错 / 未捕获异常", not errs, errs[:2])

        # ---------- 截图 ----------
        try:
            cdp.js("window.scrollTo(0, 0);")
            time.sleep(0.5)
            shot = cdp.call("Page.captureScreenshot", format="png")
            out = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                               "..", "screenshots", "cn_scrape.png"))
            with open(out, "wb") as f:
                f.write(base64.b64decode(shot["data"]))
            print("      截图 " + out)
        except Exception as e:
            print("      [提示] 截图失败（不影响结论）：", e)
    finally:
        cdp.close()
        proc.kill()

    print("\n结果：%d 通过 / %d 失败" % (ok_n, fail_n))
    return 1 if fail_n else 0


if __name__ == "__main__":
    sys.exit(main())
