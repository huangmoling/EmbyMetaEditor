#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""磁力多源（javbus + javdb）的界面回归。

多源之后有两件事只能靠界面验：

  1. **诊断要分清「谁挂了」**。以前只有 javbus，抓不到就是「抓不到」；
     现在某个站被拦 / 被限频 / 没收录，界面上必须能看出是哪一家，否则
     用户会把「javdb 被 Cloudflare 拦了」当成「这个番号没有种子」。
  2. **合并之后要能看出每条磁力来自哪个站**。同一个种子两个站都有、
     和只有一家有，对用户的意义完全不同（前者可以挑 tracker 多的那条）。

另外还验设置页那张「磁力搜索源」卡片：源开关与 javdb 地址必须真的进
保存请求体 —— 「界面能改、请求里没带」是这个项目最典型的一类静默失效。

不开真实写入：拦 `window.fetch` 造响应，必须回 {ok:true,data:…} 信封
（app.js 的 api() 取的是 json.data；回裸对象会让面板拿到 undefined）。

需要先起 exe（127.0.0.1:8097）+ 无头 Edge（9333）。
"""
import json
import os
import subprocess
import sys
import time
import urllib.request

EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
PORT = 9333
BASE = "http://127.0.0.1:8097/"
PROFILE = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                       ".tmp-edge-profile").replace("\\", "/")

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


PROBE = {"ok": True, "data": {
    "url": "https://www.javbus.com", "ok": True, "status": 200, "size": 65536,
    "elapsed_ms": 812, "looks_like_home": True,
    "markers": {"movie-box": 30, "pics/cover": 30},
    "message": "连通正常（HTTP 200，64 KB，812 ms），页面结构符合预期，可以开始抓取。",
}}

SOURCES = {"ok": True, "data": {"sources": [
    {"key": "javbus", "name": "javbus", "url": "https://www.javbus.com",
     "has_cookie": True, "enabled": True,
     "status": {"key": "javbus", "name": "javbus", "ok": True, "count": 0,
                "elapsed_ms": 830}},
    {"key": "javdb", "name": "javdb", "url": "https://javdb.com",
     "has_cookie": False, "enabled": True,
     "status": {"key": "javdb", "name": "javdb", "ok": False,
                "error": "javdb 提示「操作过于频繁」：已自动放慢重试，若一直如此请把设置里的请求间隔调大"}},
]}}

MAGNETS = [
    {"number": "SSIS-001", "url": "https://www.javbus.com/SSIS-001", "title": "一ヶ月間の禁欲の果てに",
     "note": "javbus 12 条，javdb 26 条",
     "sources": [
         {"key": "javbus", "name": "javbus", "ok": True, "count": 12, "elapsed_ms": 900},
         {"key": "javdb", "name": "javdb", "ok": True, "count": 26, "elapsed_ms": 1500},
     ],
     "magnets": [
         {"link": "magnet:?xt=urn:btih:AAAA1111&dn=SSIS-001-6.33GB", "name": "SSIS-001-UC.torrent.无码破解",
          "size": "6.33GB", "date": "2023-11-18", "source": "javdb"},
         {"link": "magnet:?xt=urn:btih:BBBB2222&dn=SSIS-001-2.57GB", "name": "SSIS-001 高清",
          "size": "2.57GB", "date": "2021-03-01", "source": "javbus"},
     ]},
    {"number": "SSIS-002", "url": "https://www.javbus.com/SSIS-002", "title": "SSIS-002",
     "note": "javbus：访问 javbus 失败：连接超时；javdb 26 条",
     "error": "各磁力源都没有收录这个番号", "sources": [], "magnets": []},
]

SETTINGS_BODY_TAG = "__reqs"


def stub_js():
    return (
        "(() => {"
        "  window.__reqs = [];"
        "  window.fetch = async (p, o) => {"
        "    const path = String(p);"
        "    const body = (o && o.body) || '';"
        "    window.__reqs.push({path: path, body: body, method: (o && o.method) || 'GET'});"
        "    let payload = {ok: true, data: {}};"
        "    if (path.indexOf('/api/magnets/sources') === 0) payload = %s;"
        "    else if (path.indexOf('/api/javbus/probe') === 0) payload = %s;"
        "    return {ok: true, status: 200, json: async () => payload};"
        "  };"
        "  return true;"
        "})()" % (json.dumps(SOURCES, ensure_ascii=False), json.dumps(PROBE, ensure_ascii=False))
    )


def main():
    if subprocess.call(["curl", "-s", "--noproxy", "*", "-m", "3", "-o", os.devnull, BASE]) != 0:
        print("8097 没起来，先运行 EmbyMetaEditor.exe")
        return 2

    proc = None
    ws_url = page_ws()
    if not ws_url:
        proc = subprocess.Popen([
            EDGE, "--headless=new", "--disable-gpu", "--no-first-run",
            "--remote-debugging-port=%d" % PORT, "--remote-allow-origins=*",
            "--user-data-dir=" + PROFILE, "about:blank",
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

        cdp.js(stub_js())
        cdp.js("switchView('javbus')")
        time.sleep(0.4)

        # --- 诊断：一次性把 javbus 细节和磁力源状态都拉出来 ---
        cdp.js("window.__reqs = [];")
        cdp.js("jbProbe()")
        time.sleep(1.2)
        paths = [r["path"] for r in (cdp.js("window.__reqs") or [])]
        check("诊断同时问了 javbus 页面结构和磁力源连通性",
              any(p.startswith("/api/javbus/probe") for p in paths) and
              any(p.startswith("/api/magnets/sources") for p in paths), paths)
        check("磁力源诊断带了 probe=1",
              any("probe=1" in p for p in paths if p.startswith("/api/magnets/sources")), paths)

        txt = cdp.js("document.querySelector('#jbBody').textContent") or ""
        check("诊断结果里列出了磁力源表格", "磁力源" in txt, txt[:200])
        check("javbus 显示为正常", "正常" in txt, txt[:400])
        check("javdb 显示为异常并把原因写出来",
              "javdb" in txt and "操作过于频繁" in txt, txt[:600])
        check("诊断里说明了「多个源一起问、按哈希去重」",
              "去重" in txt and "体积" in txt, txt[:600])
        check("未配置 Cookie 的源标出来了", "未配置" in txt, txt[:400])

        # --- 磁力列表：来源标签 + 各源小结 + 完整链接 ---
        cdp.js("switchView('javbus')")
        cdp.js("renderJbMissing({missing:[{number:'SSIS-001',title:'x',date:'2021-02-19',cover:''},"
               "{number:'SSIS-002',title:'y',date:'2021-02-20',cover:''}]})")
        cdp.js("S.jb.magnets = %s" % json.dumps(MAGNETS, ensure_ascii=False))
        cdp.js("S.jb.magTab = 'SSIS-001'; S.jb.selected = new Set(['SSIS-001','SSIS-002']);")
        cdp.js("renderMagnets()")
        time.sleep(0.4)

        src_tags = cdp.js("Array.from(document.querySelectorAll('#jbMagList .magpanel.on .src'))"
                          ".map(s => s.textContent)")
        check("每条磁力都标了来源", src_tags == ["javdb", "javbus"], src_tags)
        note = cdp.js("document.querySelector('#jbMagList .magpanel.on .cnhint').textContent") or ""
        check("面板上写了各源各给了多少条", "javbus 12 条" in note and "javdb 26 条" in note, note)

        hrefs = cdp.js("Array.from(document.querySelectorAll('#jbMagList .magpanel.on a.maglink'))"
                       ".map(a => a.getAttribute('href'))") or []
        check("磁力地址是完整的（没被截断）",
              hrefs and all(h.startswith("magnet:?xt=urn:btih:") and "&dn=" in h for h in hrefs), hrefs)

        tabs_bad = cdp.js("Array.from(document.querySelectorAll('#jbMagList .magtab.bad'))"
                          ".map(b => b.textContent)")
        check("没有磁力/失败的番号在标签上是红的", len(tabs_bad) == 1, tabs_bad)

        # --- 设置页：源开关必须进请求体 ---
        cdp.js(stub_js())
        cdp.js("switchView('settings')")
        time.sleep(0.5)
        check("设置页有 javbus / javdb 两个源开关",
              cdp.js("!!document.querySelector('#stSrcJb') && !!document.querySelector('#stSrcJdb')"))
        check("设置页有 javdb 地址与 Cookie 输入框",
              cdp.js("!!document.querySelector('#stJdb') && !!document.querySelector('#stJdbCookie')"))

        cdp.js("document.querySelector('#stSrcJb').checked = true;"
               "document.querySelector('#stSrcJdb').checked = false;"
               "document.querySelector('#stJdb').value = 'https://javdb.com';")
        cdp.js("window.__reqs = [];")
        cdp.js("saveSettings()")
        time.sleep(0.8)
        cfg_reqs = [r for r in (cdp.js("window.__reqs") or [])
                    if r["path"].startswith("/api/config") and r["method"] == "POST"]
        check("保存设置发了 POST /api/config", len(cfg_reqs) == 1, cdp.js("window.__reqs"))
        if cfg_reqs:
            body = json.loads(cfg_reqs[0]["body"] or "{}")
            check("magnet_sources 进了请求体（取消 javdb 后只剩 javbus）",
                  body.get("magnet_sources") == ["javbus"], body.get("magnet_sources"))
            check("javdb_url 进了请求体", body.get("javdb_url") == "https://javdb.com",
                  body.get("javdb_url"))

        print("\n%d 通过 / %d 失败" % (ok_n, fail_n))
        return 1 if fail_n else 0
    finally:
        cdp.close()
        if proc:
            proc.kill()


if __name__ == "__main__":
    sys.exit(main())
