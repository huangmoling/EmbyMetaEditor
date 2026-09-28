#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""批量刮削「预演」（dry-run）的界面回归。

背景：服务端的 dry-run 从很早就实现了（`ScrapeOptions.DryRun` / `CNOptions.DryRun`），
但界面从来不传 —— 等于白写。这个功能的价值全在「点之前能看见会改成什么」，
而 Emby 的写入是**不可逆**的（`POST /Items/{id}` 整对象替换，没有历史版本），
所以「预演按钮结果真的写进去了」是最坏的一种 bug。

不开真实刮削（慢、依赖外网 + MetaTube Server），而是：
  - 拦 `window.fetch` 记录请求体（断言 dry_run 真的发出去了）；
  - 换掉 `window.watchJob` 直接把一份造好的 job 结果喂给回调（跳过轮询）；
然后在真实浏览器里断言抽屉渲染。

  1. 「预演」按钮存在，且点了会把 dry_run=true 发出去
  2. 真实批量按钮发的是 dry_run=false（预演不能劫持真写按钮）
  3. 预演抽屉顶部明确写「没有写入任何东西」
  4. 逐字段表格：表头是「字段 / 现在的值 / 会改成」，行数 = 差异条数
  5. 新旧相同的行带 .same（灰掉），且**照样列出来**
  6. 会尝试写入的图片槽位列出来
  7. 抽屉里**没有任何写入按钮**（预演是只读的）
  8. 国产传媒预演：勾选为空时只提示、不发请求；勾选后请求体带 dry_run + ids

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
import wbauth  # noqa: E402  （访问认证辅助，见 wbauth.py）
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
        # 详情一律 str()：Page.errors() 之类返回的是列表，直接 print 会抛 TypeError，
        # 把真正的失败信息顶掉（这个坑踩过两次）。
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


# 造一份预演 job 结果：一条条目、3 个字段（其中 1 个新旧相同）、3 张图。
MOVID = {
    "item_id": "it-1", "item_name": "SSIS-001 某个片名", "number": "SSIS-1",
    "dry_run": True,
    "changes": [
        {"field": "Name", "before": "SSIS-001 某个片名", "after": "SSIS-001 中文标题", "same": False},
        {"field": "Overview", "before": "旧简介", "after": "旧简介", "same": True},
        {"field": "PremiereDate", "before": "", "after": "2021-04-06", "same": False},
    ],
    "will_images": ["Primary", "Thumb", "Backdrop"],
    "message": "试运行：会改动 2 个字段、3 个图片槽位，未写入",
}
CNJOB = {
    "item_id": "it-cn", "item_name": "91CM-014", "number": "91CM-014",
    "dry_run": True,
    "changes": [
        {"field": "Name", "before": "91CM-014", "after": "91CM-014 日本街头拜金女大测试", "same": False},
        {"field": "Tags", "before": "91CM-014", "after": "91CM-014；果冻传媒", "same": False},
    ],
    "will_images": ["封面", "缩略图"],
    "message": "试运行：命中 1 个站点，会改动 2 个字段、2 张图，未写入",
}


def stub_js(job):
    """拦 fetch + 换掉 watchJob 的注入脚本（纯字符串，方便复用）。

    注意 `result` 必须是**数组** —— 批量的 job.Result 是 []*ScrapeResult / []*CNScrape，
    传单个对象进去 showScrapePreview 会判成「没有可预演的条目」（踩过）。
    """
    return (
        "(() => {"
        "  window.__reqs = [];"
        "  window.__jobTitle = '';"
        "  window.fetch = async (p, o) => {"
        "    window.__reqs.push({path: String(p), body: (o && o.body) || ''});"
        "    return {ok: true, status: 200, json: async () => ({ok: true, data: {job_id: 'j-dry'}})};"
        "  };"
        "  window.watchJob = (id, title, cb) => { window.__jobTitle = title; cb({result: [%s]}); };"
        "  return true;"
        "})()" % json.dumps(job, ensure_ascii=False)
    )


def main():
    if subprocess.call(["curl", "-s", "--noproxy", "*", "-m", "3", "-o", os.devnull, BASE]) != 0:
        print("8097 没起来，先运行 EmbyMetaEditor.exe")
        return 2

    proc = None
    ws_url = page_ws()
    if not ws_url:
        # 复用不了就自己起一个。--user-data-dir 必须是仓库内 / 临时目录下的**真实**路径
        # （指到 /tmp 那种会在部分环境下静默起不来：9333 不监听、进程直接消失）。
        profile = os.path.join(tempfile.gettempdir(), "emby-dryrun-profile").replace("\\", "/")
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

        # ---------- 1. 媒体库批量预演 ----------
        check("index.html 有「预演」按钮",
              cdp.js("!!document.querySelector('#lbDry')") is True)
        check("媒体库预演按钮在批量按钮**左边**（先预演后动手的视觉顺序）",
              cdp.js("document.querySelector('#lbDry').compareDocumentPosition("
                     "document.querySelector('#lbBatch')) === 4") is True)

        cdp.js(stub_js(MOVID))
        r = cdp.js("(async () => { document.querySelector('#lbDry').click();"
                   " await new Promise(r => setTimeout(r, 500)); return true; })()")
        check("点击预演后页面没抛异常", r is True)

        reqs = cdp.js("window.__reqs") or []
        check("预演只发了一个请求", len(reqs) == 1, reqs)
        if reqs:
            check("预演打到批量刮削接口", "/api/items/scrape-batch" in reqs[0]["path"], reqs[0]["path"])
            body = json.loads(reqs[0]["body"] or "{}")
            check("请求体带 dry_run=true（这是整条链最容易断的一环）",
                  body.get("dry_run") is True, reqs[0]["body"])
        check("job 标题标了「预演」", "预演" in (cdp.js("window.__jobTitle") or ""),
              cdp.js("window.__jobTitle"))

        drawer = cdp.js("(document.querySelector('#drawerHost .drawer') || {}).textContent") or ""
        check("抽屉顶部明确写了「没有写入任何东西」", "没有写入任何东西" in drawer, drawer[:120])
        check("汇总了条数与字段数", "共 1 条" in drawer and "合计 2 个字段" in drawer, drawer[:200])
        check("提示了灰行含义", "灰色行" in drawer)
        check("列出会尝试写入的图片槽位",
              "会尝试写入图片" in drawer and "Primary / Thumb / Backdrop" in drawer)
        check("抽屉里**没有**写入按钮（预演是只读的）",
              cdp.js("document.querySelectorAll('#drawerHost .drawer button').length") == 0)
        check("给出「要真写怎么办」的指引", "要真的写入" in drawer)

        heads = cdp.js("Array.from(document.querySelectorAll('#drawerHost table.pf-tbl thead th'))"
                       ".map(th => th.textContent)")
        check("表头是 字段 / 现在的值 / 会改成",
              heads == ["字段", "现在的值", "会改成"], heads)
        rows = cdp.js("Array.from(document.querySelectorAll('#drawerHost table.pf-tbl tbody tr'))"
                      ".map(tr => ({cls: tr.className, tds: Array.from(tr.querySelectorAll('td'))"
                      ".map(td => td.textContent)}))") or []
        check("差异行数 = 3（含 1 行新旧相同）", len(rows) == 3, rows)
        if len(rows) == 3:
            check("第一行是 Name 且带现值和目标值",
                  rows[0]["tds"][0] == "Name" and "SSIS-001 中文标题" in rows[0]["tds"][2], rows[0])
            check("New=旧 的那行带 .same（灰掉）", "same" in rows[1]["cls"], rows[1])
            check("空的现值显示「（空）」而不是留白", "（空）" in rows[2]["tds"][1], rows[2])
        check("每个条目一张卡片，卡片上是条目名与番号",
              cdp.js("(document.querySelector('#drawerHost .card h3') || {}).textContent") ==
              "SSIS-001 某个片名SSIS-1")

        # 真实按钮必须仍然是真写（预演不能把 dry_run 也传给真写）
        cdp.js("document.querySelector('#drawerHost').innerHTML = ''")
        cdp.js("window.__reqs = [];")
        cdp.js("(async () => { document.querySelector('#lbBatch').click();"
               " await new Promise(r => setTimeout(r, 500)); return true; })()")
        reqs2 = cdp.js("window.__reqs") or []
        if reqs2:
            body2 = json.loads(reqs2[0]["body"] or "{}")
            check("真实批量按钮发的是 dry_run=false", body2.get("dry_run") is False, reqs2[0]["body"])
        else:
            check("真实批量按钮发的是 dry_run=false", False, "没有发出请求")
        check("真实批量按钮的 job 标题不带「预演」",
              "预演" not in (cdp.js("window.__jobTitle") or ""), cdp.js("window.__jobTitle"))

        # ---------- 2. 国产传媒预演 ----------
        cdp.js(stub_js(CNJOB))
        cdp.js("S.cn.sel = new Set(); window.__reqs = [];")
        cdp.js("(async () => { document.querySelector('#cnDry').click();"
               " await new Promise(r => setTimeout(r, 300)); return true; })()")
        check("没勾选时不发请求（只提示）",
              len(cdp.js("window.__reqs") or []) == 0, cdp.js("window.__reqs"))
        check("没勾选时弹了可见的提示",
              "先勾选要刮削的条目" in (
                  cdp.js("Array.from(document.querySelectorAll('#toasts .toast'))"
                         ".map(t => t.textContent).join('|')") or ""))

        cdp.js("S.cn.sel = new Set(['it-cn']); window.__reqs = [];")
        cdp.js("(async () => { document.querySelector('#cnDry').click();"
               " await new Promise(r => setTimeout(r, 500)); return true; })()")
        reqs3 = cdp.js("window.__reqs") or []
        if reqs3:
            check("国产传媒预演打到批量接口",
                  "/api/cn/scrape-batch" in reqs3[0]["path"], reqs3[0]["path"])
            body3 = json.loads(reqs3[0]["body"] or "{}")
            check("国产传媒预演请求体带 dry_run=true 与 ids",
                  body3.get("dry_run") is True and body3.get("ids") == ["it-cn"], reqs3[0]["body"])
        else:
            check("国产传媒预演打到批量接口", False, "没有发出请求")
            check("国产传媒预演请求体带 dry_run=true 与 ids", False, "没有发出请求")

        cn_drawer = cdp.js("(document.querySelector('#drawerHost .drawer') || {}).textContent") or ""
        check("国产传媒预演抽屉标题带「预演」", "预演" in cn_drawer, cn_drawer[:120])
        check("国产传媒预演也标了没写入", "没有写入任何东西" in cn_drawer)
        check("国产传媒预演的图片槽位用中文（封面 / 缩略图）",
              "封面 / 缩略图" in cn_drawer, cn_drawer[:200])
        check("国产传媒预演没有写入按钮",
              cdp.js("document.querySelectorAll('#drawerHost .drawer button').length") == 0)

        print("\n%d 通过 / %d 失败" % (ok_n, fail_n))
        return 1 if fail_n else 0
    finally:
        cdp.close()
        if proc:
            proc.kill()


if __name__ == "__main__":
    sys.exit(main())
