#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""条目写入历史 / 回滚的界面回归。

服务端每次写条目元数据（刮削 / 国产传媒 / 手动编辑）之前都会存一份快照，
`/api/items/history` 列出来、`/api/items/rollback` 撤销。这个脚本验的是**界面这一层**：

  1. 切到设置页会自动拉取历史（不点刷新也有内容）
  2. 表格列出时间 / 条目 / 来源 / 改动的字段
  3. 已回滚的行灰掉并显示「已回滚」，**不再给回滚按钮**（不能回滚两次）
  4. 回滚要点两次：第一次只切成确认态，不请求；第二次才真的 POST
  5. POST 的 body 是 {"id": "<record_id>"}
  6. 回滚成功后重新拉列表（界面跟着更新）
  7. 面板上明确写着「图片不可还原」—— 不然用户会以为连旧海报一起找回来了

不开真实 Emby 写入：拦 `window.fetch` 造响应（必须回 {ok:true,data:…} 信封，
app.js 的 api() 取的是 json.data；回裸对象会让面板拿到 undefined 并抛未捕获异常）。

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
    print("缺少 websocket-client，请用 ~/.workbuddy-ai/binaries/envs/default 里的 python")
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
        # 详情一律 str()：这里可能拿到列表，直接 print 会抛 TypeError 把真信息顶掉。
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


RECORDS = [
    {"id": "rec-new", "item_id": "it-1", "name": "SSIS-001 某个片名", "kind": "item",
     "created_at": "2026-09-28T10:11:12Z", "sources": ["MetaTube 刮削"],
     "changed": ["Name", "Overview", "Tags"], "rolled_back": False},
    {"id": "rec-old", "item_id": "cn-9", "name": "91CM-014", "kind": "item",
     "created_at": "2026-09-28T09:00:00Z", "sources": ["国产传媒刮削"],
     "changed": ["Name", "Tags"], "rolled_back": True},
]
HIST_NOTE = "快照只覆盖元数据字段；被覆盖的旧图片无法还原。"


def stub_js(records, rollback_ok=True):
    """拦 fetch：历史接口回造好的数据，回滚接口回成功。"""
    hist = {"ok": True, "data": {"records": records, "total": len(records), "note": HIST_NOTE}}
    roll = {"ok": True, "data": {"item_id": "it-1", "name": "SSIS-001 某个片名",
                                 "record_id": "rec-new",
                                 "message": "已还原到写入前的元数据（图片不在快照范围内，覆盖掉的旧图不会回来）"}}
    return (
        "(() => {"
        "  window.__reqs = [];"
        "  window.fetch = async (p, o) => {"
        "    const path = String(p);"
        "    window.__reqs.push({path: path, body: (o && o.body) || '', method: (o && o.method) || 'GET'});"
        "    const payload = path.indexOf('/api/items/rollback') === 0 ? %s : %s;"
        "    return {ok: true, status: 200, json: async () => payload};"
        "  };"
        "  return true;"
        "})()" % (json.dumps(roll, ensure_ascii=False), json.dumps(hist, ensure_ascii=False))
    )


def main():
    if subprocess.call(["curl", "-s", "--noproxy", "*", "-m", "3", "-o", os.devnull, BASE]) != 0:
        print("8097 没起来，先运行 EmbyMetaEditor.exe")
        return 2

    proc = None
    ws_url = page_ws()
    if not ws_url:
        profile = os.path.join(tempfile.gettempdir(), "emby-hist-profile").replace("\\", "/")
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

        cdp.js(stub_js(RECORDS))
        # 切到设置页 —— 历史面板应该自动加载，不需要用户点刷新
        cdp.js("switchView('settings')")
        time.sleep(0.8)

        paths = [r["path"] for r in (cdp.js("window.__reqs") or [])]
        check("切到设置页自动拉取历史（不用点刷新）",
              any(p.startswith("/api/items/history") for p in paths), paths)

        # 重新注入 stub 后直接调 loadItemHistory()（它是全局函数）拿干净的一份数据，
        # 不用来回切视图 —— 那样更容易被别处的加载时序干扰。
        cdp.js(stub_js(RECORDS))
        cdp.js("loadItemHistory()")
        time.sleep(0.8)

        rows = cdp.js("Array.from(document.querySelectorAll('#stHistList table.pf-tbl tbody tr'))"
                      ".map(tr => ({cls: tr.className, tds: Array.from(tr.querySelectorAll('td'))"
                      ".map(td => td.textContent)}))") or []
        check("历史表列出 2 条", len(rows) == 2, rows)
        if len(rows) == 2:
            check("第一行是时间 / 条目 / 来源 / 字段",
                  rows[0]["tds"][0].startswith("2026-09-28") and
                  rows[0]["tds"][1] == "SSIS-001 某个片名" and
                  "MetaTube" in rows[0]["tds"][2] and
                  "Overview" in rows[0]["tds"][3], rows[0])
            check("已回滚的行灰掉并标「已回滚」",
                  "same" in rows[1]["cls"] and "已回滚" in rows[1]["tds"][4], rows[1])

        btns = cdp.js("document.querySelectorAll('#stHistList button[data-rollback]').length")
        check("只有未回滚的那条有回滚按钮（不能回滚两次）", btns == 1, btns)
        check("回滚按钮带的是记录 id",
              cdp.js("(document.querySelector('#stHistList button[data-rollback]') || {}).dataset") is not None and
              cdp.js("document.querySelector('#stHistList button[data-rollback]').dataset.rollback") == "rec-new")

        panel = cdp.js("(document.querySelector('#stHistList').closest('.card') || {}).textContent") or ""
        check("面板上写明「被覆盖的旧图片无法还原」", "图片" in panel and "不会回来" in panel, panel[:200])
        check("面板上说明了为什么要快照（整对象替换 / 没有版本历史）",
              "整对象替换" in panel and "没有版本历史" in panel)

        # 回滚要点两次：第一次只切确认态，不发请求
        cdp.js("window.__reqs = [];")
        cdp.js("document.querySelector('#stHistList button[data-rollback]').click()")
        time.sleep(0.3)
        check("第一次点击不发请求（防误触）",
              len(cdp.js("window.__reqs") or []) == 0, cdp.js("window.__reqs"))
        check("第一次点击把按钮切成确认态",
              cdp.js("document.querySelector('#stHistList button[data-rollback]').textContent") == "确认回滚？",
              cdp.js("document.querySelector('#stHistList button[data-rollback]').textContent"))

        cdp.js("document.querySelector('#stHistList button[data-rollback]').click()")
        time.sleep(0.8)
        reqs = cdp.js("window.__reqs") or []
        roll_reqs = [r for r in reqs if r["path"].startswith("/api/items/rollback")]
        check("第二次点击才真的回滚", len(roll_reqs) == 1, reqs)
        if roll_reqs:
            check("回滚用 POST", roll_reqs[0]["method"] == "POST", roll_reqs[0])
            body = json.loads(roll_reqs[0]["body"] or "{}")
            check("回滚 body 是 {id: 记录ID}", body.get("id") == "rec-new", roll_reqs[0]["body"])
        check("回滚成功后重新拉了一次列表",
              sum(1 for r in reqs if r["path"].startswith("/api/items/history")) >= 1, reqs)
        toasts = cdp.js("Array.from(document.querySelectorAll('#toasts .toast'))"
                        ".map(t => t.textContent).join('|')") or ""
        check("回滚给了可见反馈", "已还原" in toasts, toasts)
        check("回滚提示里也说清了图片不回来", "图片" in toasts, toasts)

        print("\n%d 通过 / %d 失败" % (ok_n, fail_n))
        return 1 if fail_n else 0
    finally:
        cdp.close()
        if proc:
            proc.kill()


if __name__ == "__main__":
    sys.exit(main())
