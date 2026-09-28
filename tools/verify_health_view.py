#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""媒体库体检页的界面回归。

体检是**只读扫描**：列出「还没被收拾过」的条目，按问题类型分组。这个脚本验界面：

  1. 导航里有「媒体库体检」，点了会切过去
  2. 库下拉被填上（不选库就扫描时只提示，不发请求）
  3. 扫描请求带上 parent / type / limit
  4. 汇总 chip：检查条目（截断时显示「/ 共 N」）/ 有问题 / 干净
  5. 分组按严重程度排（err 在前），每组的计数与提示都在
  6. 明细超出上限时显示「还有 N 条未列出」
  7. 点明细行会按 id 打开条目详情
  8. 页面里**没有任何写入按钮** —— 体检不做「一键自动修复」是刻意的
  9. 全部通过时列出「✓ 这几项全部通过」，而不是一片空白

不开真实 Emby：拦 fetch 造响应（必须回 {ok:true,data:…} 信封）。

需要先起 exe（127.0.0.1:8097）+ 无头 Edge（9333）。
"""
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.request

EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
PORT = 9333
BASE = "http://127.0.0.1:8097/"

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import wbauth  # noqa: E402
try:
    import websocket  # websocket-client
except ImportError:
    print("缺少 websocket-client，请用 ~/.workbuddy-ai/binaries/python/envs/default 里的 python")
    sys.exit(2)

ok_n = 0
fail_n = 0


def check(name, cond, extra=""):
    global ok_n, fail_n
    if cond:
        ok_n += 1
        print("PASS  " + name)
    else:
        fail_n += 1
        print("FAIL  " + name + ("  " + str(extra) if extra else ""))


class CDP:
    def __init__(self, ws_url):
        self.ws = websocket.create_connection(ws_url, timeout=30)
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
        res = r.get("result", {})
        if r.get("exceptionDetails"):
            raise RuntimeError(json.dumps(r["exceptionDetails"])[:400])
        return res.get("value")

    def close(self):
        try:
            self.ws.close()
        except Exception:
            pass


def http_json(path):
    with urllib.request.urlopen("http://127.0.0.1:%d%s" % (PORT, path), timeout=10) as r:
        return json.loads(r.read().decode("utf-8"))


def page_ws():
    try:
        for t in http_json("/json/list"):
            if t.get("type") == "page":
                return t["webSocketDebuggerUrl"]
    except Exception:
        pass
    return None


def hitem(iid, name, number="", year=0, issues=()):
    return {"id": iid, "name": name, "number": number, "year": year,
            "path": "/media/" + iid + ".mp4", "issues": list(issues)}


REPORT = {
    "parent": "lib-1", "scanned": 500, "total": 1200, "truncated": True,
    "clean": 380, "problem": 120,
    "groups": [
        {"key": "no_number", "label": "认不出番号", "level": "err",
         "hint": "刮削要靠番号去搜。", "count": 2, "more": 0,
         "items": [hitem("i1", "日本街头拜金女大测试.mp4", "", 0, ["no_number", "title_is_filename"]),
                   hitem("i2", "某部没有番号的片子", "", 2020, ["no_number"])]},
        {"key": "title_is_filename", "label": "标题像文件名", "level": "err",
         "hint": "标题还是下载时的文件名。", "count": 3, "more": 1,
         "items": [hitem("i1", "日本街头拜金女大测试.mp4", "", 0, ["no_number", "title_is_filename"])]},
        {"key": "missing_poster", "label": "缺海报", "level": "warn",
         "hint": "没有主图（海报）。", "count": 0, "more": 0, "items": []},
        {"key": "no_tags", "label": "无标签", "level": "warn",
         "hint": "标签为空。", "count": 0, "more": 0, "items": []},
    ],
}
CLEAN_REPORT = {
    "parent": "lib-1", "scanned": 42, "total": 42, "truncated": False,
    "clean": 42, "problem": 0,
    "groups": [{"key": "no_number", "label": "认不出番号", "level": "err",
                "hint": "刮削要靠番号去搜。", "count": 0, "more": 0, "items": []},
               {"key": "missing_poster", "label": "缺海报", "level": "warn",
                "hint": "没有主图（海报）。", "count": 0, "more": 0, "items": []}],
}
LIBS = [{"Id": "lib-1", "Name": "电影"}, {"Id": "lib-2", "Name": "国产传媒"}]
DETAIL = {"Id": "i1", "Name": "日本街头拜金女大测试.mp4", "Type": "Movie",
          "Path": "/media/i1.mp4", "ProviderIds": {}, "ImageTags": {}}


def stub_js(report):
    def env(data):
        return json.dumps({"ok": True, "data": data}, ensure_ascii=False)
    return (
        "(() => {"
        "  window.__reqs = [];"
        "  window.fetch = async (p, o) => {"
        "    const path = String(p);"
        "    window.__reqs.push({path: path, method: (o && o.method) || 'GET'});"
        "    let payload = {ok: true, data: {}};"
        "    if (path.indexOf('/api/health') === 0) payload = %s;"
        "    else if (path.indexOf('/api/libraries') === 0) payload = %s;"
        "    else if (path.indexOf('/api/items/detail') === 0) payload = %s;"
        "    return {ok: true, status: 200, json: async () => payload};"
        "  };"
        "  return true;"
        "})()" % (env(report), env(LIBS), env(DETAIL))
    )


def main():
    if subprocess.call(["curl", "-s", "--noproxy", "*", "-m", "3", "-o", os.devnull, BASE]) != 0:
        print("8097 没起来，先运行 EmbyMetaEditor.exe")
        return 2

    proc = None
    ws_url = page_ws()
    if not ws_url:
        profile = os.path.join(tempfile.gettempdir(), "emby-health-profile").replace("\\", "/")
        proc = subprocess.Popen([
            EDGE, "--headless=new", "--disable-gpu", "--no-first-run",
            "--remote-debugging-port=%d" % PORT, "--remote-allow-origins=*",
            "--user-data-dir=" + profile, "about:blank",
        ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(60):
            time.sleep(0.5)
            ws_url = page_ws()
            if ws_url:
                break
    if not ws_url:
        if proc:
            proc.kill()
        print("拿不到 CDP 目标")
        return 2

    cdp = CDP(ws_url)
    try:
        cdp.call("Page.enable")
        cdp.call("Runtime.enable")
        cdp.call("Page.navigate", url=BASE)
        wbauth.login_in_browser(cdp)
        cdp.call("Page.navigate", url=BASE)
        time.sleep(3)

        check("导航里有「媒体库体检」",
              cdp.js("!!document.querySelector('#nav button[data-view=\"health\"]')") is True)
        # 页面启动时 /api/libraries 已经真实跑过一次（下拉里是用户自己的库），
        # 这里清掉缓存再切视图，让 ensureLibs 走我们的 stub，断言才可复现。
        cdp.js("S.libs = [];")
        cdp.js(stub_js(REPORT))
        cdp.js("switchView('health')")
        time.sleep(0.8)
        check("切换后体检视图可见",
              cdp.js("document.querySelector('#view-health').classList.contains('on')") is True)

        opts = cdp.js("Array.from(document.querySelectorAll('#hLib option')).map(o => o.textContent)") or []
        check("库下拉被填上（含占位项 + 两个库）",
              opts[:1] == ["请选择媒体库"] and "电影" in opts and "国产传媒" in opts, opts)
        check("库下拉默认不选任何库（避免误扫整库）",
              cdp.js("document.querySelector('#hLib').value") == "")

        # 不选库就点扫描 → 只提示，不发请求
        cdp.js("window.__reqs = [];")
        cdp.js("(async () => { document.querySelector('#hScan').click();"
               " await new Promise(r => setTimeout(r, 300)); return true; })()")
        check("没选库时不发请求",
              len(cdp.js("window.__reqs") or []) == 0, cdp.js("window.__reqs"))
        toasts = cdp.js("Array.from(document.querySelectorAll('#toasts .toast'))"
                        ".map(t => t.textContent).join('|')") or ""
        check("没选库时给了提示", "先选一个媒体库" in toasts, toasts)

        # 选库后扫描
        cdp.js("document.querySelector('#hLib').value = 'lib-1';"
               " document.querySelector('#hType').value = 'Movie';"
               " document.querySelector('#hLimit').value = '500';"
               " window.__reqs = [];")
        cdp.js("(async () => { document.querySelector('#hScan').click();"
               " await new Promise(r => setTimeout(r, 600)); return true; })()")
        reqs = cdp.js("window.__reqs") or []
        health_reqs = [r for r in reqs if r["path"].startswith("/api/health")]
        check("扫描打到 /api/health", len(health_reqs) == 1, reqs)
        if health_reqs:
            q = health_reqs[0]["path"]
            check("请求带上 parent / type / limit",
                  "parent=lib-1" in q and "type=Movie" in q and "limit=500" in q, q)

        summary = cdp.js("(document.querySelector('#hSummary') || {}).textContent") or ""
        check("汇总显示「检查条目 500 / 共 1,200」",
              "1,200" in summary and "500" in summary, summary[:160])
        check("截断时说明了为什么只扫了一部分", "只检查了" in summary, summary[:200])
        check("汇总显示有问题 / 干净", "有问题" in summary and "干净" in summary and "120" in summary)
        check("汇总列出了「全部通过」的项", "全部通过" in summary and "缺海报" in summary)

        cards = cdp.js("Array.from(document.querySelectorAll('#hBody .card')).map(c => "
                       "c.querySelector('h3').textContent)") or []
        check("只渲染有问题的分组，顺序按严重程度（err 在前）",
              len(cards) == 2 and "认不出番号" in cards[0] and "标题像文件名" in cards[1], cards)
        check("分组标题带条数", cards and "2 条" in cards[0], cards)

        rows = cdp.js("Array.from(document.querySelectorAll('#hBody button.hrow')).map(b => "
                      "({id: b.dataset.id, t: b.textContent}))") or []
        check("明细行渲染出来了", len(rows) == 3, rows)
        check("行上显示了条目名与「无番号」",
              any("日本街头拜金女大测试" in r["t"] and "无番号" in r["t"] for r in rows), rows)

        body = cdp.js("(document.querySelector('#hBody') || {}).textContent") or ""
        check("明细超出上限时给出「还有 N 条未列出」", "还有 1 条未列出" in body, body[-200:])

        # 只读承诺：页面里不能有写入按钮
        btns = cdp.js("Array.from(document.querySelectorAll('#view-health button'))"
                      ".map(b => b.textContent)") or []
        check("体检页没有「自动修复 / 一键写入」这类按钮",
              all(("修复" not in t and "写入" not in t and "刮削" not in t) for t in btns), btns)
        hint = cdp.js("(document.querySelector('#view-health .toolbar') || {}).textContent") or ""
        check("工具栏上写明「只读扫描，不会写入任何东西」", "只读扫描" in hint, hint)

        # 点明细行 → 打开条目详情（按 id 请求）
        cdp.js("window.__reqs = [];")
        cdp.js("document.querySelectorAll('#hBody button.hrow')[0].click()")
        time.sleep(0.6)
        detail_reqs = [r for r in (cdp.js("window.__reqs") or []) if "items/detail" in r["path"]]
        check("点明细行按 id 打开条目详情",
              len(detail_reqs) == 1 and "id=i1" in detail_reqs[0]["path"], detail_reqs)
        check("抽屉打开了",
              cdp.js("!!document.querySelector('#drawerHost .drawer')") is True)
        cdp.js("document.querySelector('#drawerHost').innerHTML = ''")

        # 全部通过的报告
        cdp.js(stub_js(CLEAN_REPORT))
        cdp.js("(async () => { document.querySelector('#hScan').click();"
               " await new Promise(r => setTimeout(r, 600)); return true; })()")
        clean_body = cdp.js("(document.querySelector('#hBody') || {}).textContent") or ""
        check("没有问题时给出明确结论（而不是一片空白）",
              "没有发现问题" in clean_body and "42" in clean_body, clean_body[:200])
        clean_sum = cdp.js("(document.querySelector('#hSummary') || {}).textContent") or ""
        check("没截断时不说「只检查了」", "只检查了" not in clean_sum, clean_sum[:200])
        check("没有问题时不再渲染分组卡",
              cdp.js("document.querySelectorAll('#hBody .card').length") == 1)

        print("\n%d 通过 / %d 失败" % (ok_n, fail_n))
        return 1 if fail_n else 0
    finally:
        cdp.close()
        if proc:
            proc.kill()


if __name__ == "__main__":
    sys.exit(main())
