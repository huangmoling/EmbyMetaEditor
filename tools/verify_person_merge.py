#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""人物归并的界面回归。

归并是这个工具里**唯一会批量改写别人数据**的功能：它把一批作品的演职员表
改挂到另一个人名下，再删掉多余的人物条目。所以这个脚本验的重点不是「能不能用」，
而是「**不会误触**」：

  1. 导航里有「人物归并」，切过去是查重页
  2. 查重只发一个 GET（/api/persons/duplicates），一个字节都不写
  3. 候选渲染成卡片：作品多的排第一、默认就是「保留」那一个
  4. **归并按钮默认禁用** —— 没看预演就不能写
  5. 点「预演」发 POST + dry_run:true，结果表格是「预演结果」，且明说还没写进去
  6. 换「保留谁」之后，归并按钮重新禁用、预演结果清空（不能拿 A 的预演去点 B）
  7. 归并要点两次：第一次只切确认态、不发请求；第二次才真的 POST，且不带 dry_run
  8. 服务端报错时错误照原样显示出来（别吞掉）

不开真实 Emby 写入：拦 `window.fetch` 造响应（必须回 {ok:true,data:…} 信封，
app.js 的 api() 取的是 json.data；回裸对象会让面板拿到 undefined 并抛未捕获异常）。

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
# 无头 Edge 的 --user-data-dir **必须指工作区内的路径**：指到 /tmp 会静默起不来
# （9333 不监听、进程直接消失，还不报错）。跑完记得删。
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
        # 详情一律 str()：可能拿到列表，直接 print 会抛 TypeError 把真信息顶掉。
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


DUP = {
    "ok": True,
    "data": {
        "scanned": 10561, "total": 10561, "truncated": False,
        "persons": 4, "min_score": 86, "note": "",
        "groups": [
            {"key": "三上悠亜",
             "reason": "归一化后名字完全相同（大小写 / 全半角 / 空格 / 标点差异都算同一个）",
             "persons": [
                 {"id": "p-big", "name": "三上悠亜", "image_tag": "t1", "works": 82},
                 {"id": "p-small", "name": "三上悠亜 ", "image_tag": "", "works": 3},
                 {"id": "p-tiny", "name": "三上悠亜(中文)", "image_tag": "", "works": 1},
             ]},
            {"key": "深田えいみ",
             "reason": "别名记忆里指向同一个人（深田えいみ）",
             "persons": [
                 {"id": "f-1", "name": "深田えいみ", "image_tag": "", "works": 40},
                 {"id": "f-2", "name": "Eimi Fukada", "image_tag": "", "works": 40},
             ]},
        ],
    },
}

PLAN = {
    "ok": True,
    "data": {
        "keep_id": "p-big", "keep_name": "三上悠亜", "dry_run": True,
        "plan": [
            {"drop_id": "p-small", "drop_name": "三上悠亜 ", "items": 3, "moved": 3,
             "image": False, "deleted": False},
            {"drop_id": "p-tiny", "drop_name": "三上悠亜(中文)", "items": 1, "moved": 1,
             "image": True, "deleted": False},
        ],
        "total_items": 4, "deleted": 0,
    },
}

DONE = {
    "ok": True,
    "data": {
        "keep_id": "p-big", "keep_name": "三上悠亜", "dry_run": False,
        "plan": [
            {"drop_id": "p-small", "drop_name": "三上悠亜 ", "items": 3, "moved": 3,
             "image": False, "deleted": True},
        ],
        "total_items": 3, "deleted": 1,
        "snapshots": ["rec-1", "rec-2"],
        "errors": ["改写作品 XX-001 的演员失败：连接超时"],
    },
}


def stub_js(dup=DUP, plan=PLAN, done=DONE):
    """拦 fetch：查重回候选，归并回预演/执行两种结果。"""
    return (
        "(() => {"
        "  window.__reqs = [];"
        "  window.fetch = async (p, o) => {"
        "    const path = String(p);"
        "    const body = (o && o.body) || '';"
        "    window.__reqs.push({path: path, body: body, method: (o && o.method) || 'GET'});"
        "    let payload = %s;"
        "    if (path.indexOf('/api/persons/merge') === 0) {"
        "      payload = JSON.parse(body || '{}').dry_run ? %s : %s;"
        "    }"
        "    return {ok: true, status: 200, json: async () => payload};"
        "  };"
        "  return true;"
        "})()" % (json.dumps(dup, ensure_ascii=False),
                  json.dumps(plan, ensure_ascii=False),
                  json.dumps(done, ensure_ascii=False))
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
        cdp.js("switchView('merge')")
        time.sleep(0.5)

        check("导航里有「人物归并」", cdp.js(
            "!!document.querySelector('#nav button[data-view=merge]')"))
        check("查重页是当前视图", cdp.js(
            "document.querySelector('#view-merge').classList.contains('on')"))
        check("标题切成了「人物归并」", cdp.js(
            "document.querySelector('#viewTitle').textContent") == "人物归并",
            cdp.js("document.querySelector('#viewTitle').textContent"))
        check("页面写明「查找只读、归并前必须预演」", "预演" in (cdp.js(
            "document.querySelector('#view-merge .hint').textContent") or ""))

        # --- 查重：只发一个 GET ---
        cdp.js("window.__reqs = [];")
        cdp.js("loadDuplicates()")
        time.sleep(0.8)
        reqs = cdp.js("window.__reqs") or []
        check("查重只发了一个请求", len(reqs) == 1, reqs)
        if reqs:
            check("查重走 GET /api/persons/duplicates", reqs[0]["method"] == "GET" and
                  reqs[0]["path"].startswith("/api/persons/duplicates"), reqs[0])
            check("查重带上了 min_score / limit / alias",
                  "min_score=" in reqs[0]["path"] and "limit=" in reqs[0]["path"] and
                  "alias=" in reqs[0]["path"], reqs[0]["path"])

        # --- 候选卡片 ---
        cards = cdp.js("document.querySelectorAll('#mgBody .mg-card').length")
        check("渲染出 2 个候选分组", cards == 2, cards)
        names = cdp.js("Array.from(document.querySelectorAll('#mgBody .mg-card:first-of-type .mg-p b'))"
                       ".map(b => b.textContent)")
        check("组内按作品数排序（82 部那个在前）", names and names[0] == "三上悠亜", names)
        check("第一个候选默认就是「保留」",
              cdp.js("document.querySelector('#mgBody .mg-card input[type=radio]').checked") is True)
        check("候选卡片显示了作品数",
              "82 部作品" in (cdp.js("document.querySelector('#mgBody .mg-card').textContent") or ""))
        check("无头像的候选有占位块",
              "无头像" in (cdp.js("document.querySelector('#mgBody').textContent") or ""))
        reason = cdp.js("document.querySelector('#mgBody .mg-card .cnhint').textContent") or ""
        check("每组都写了「为什么算重复」", "完全相同" in reason, reason)

        # --- 归并按钮默认禁用 ---
        check("归并按钮默认禁用（没预演不能写）", cdp.js(
            "document.querySelector('#mgBody button[data-act=merge]').disabled") is True)

        # --- 预演 ---
        cdp.js("window.__reqs = [];")
        cdp.js("document.querySelector('#mgBody button[data-act=preview]').click()")
        time.sleep(0.9)
        reqs = cdp.js("window.__reqs") or []
        merge_reqs = [r for r in reqs if r["path"].startswith("/api/persons/merge")]
        check("预演发了一次 POST /api/persons/merge", len(merge_reqs) == 1, reqs)
        pre_body = {}
        if merge_reqs:
            pre_body = json.loads(merge_reqs[0]["body"] or "{}")
            check("预演带 dry_run:true（否则这就是个写入按钮）",
                  pre_body.get("dry_run") is True, pre_body)
            check("预演带 keep_id / drop_ids",
                  pre_body.get("keep_id") == "p-big" and
                  sorted(pre_body.get("drop_ids") or []) == ["p-small", "p-tiny"], pre_body)

        out = cdp.js("document.querySelector('#mgBody .mg-out').textContent") or ""
        check("预演结果标着「预演结果」", "预演结果" in out, out[:200])
        check("预演结果明说还没写进 Emby", "还没有" in out, out[:200])
        check("预演表格列出被并入的条目", "并入的条目" in out, out[:200])
        check("预演里写出了会转移头像", "转移头像" in out, out[:200])
        check("预演之后归并按钮解锁", cdp.js(
            "document.querySelector('#mgBody button[data-act=merge]').disabled") is False)

        # --- 换「保留谁」必须作废预演 ---
        cdp.js("(() => { const r = document.querySelectorAll('#mgBody .mg-card input[type=radio]');"
               " r[1].checked = true; r[1].dispatchEvent(new Event('change')); })()")
        time.sleep(0.3)
        check("换「保留谁」后归并按钮重新禁用", cdp.js(
            "document.querySelector('#mgBody button[data-act=merge]').disabled") is True)
        check("换「保留谁」后预演结果被清空",
              (cdp.js("document.querySelector('#mgBody .mg-out').textContent") or "").strip() == "",
              cdp.js("document.querySelector('#mgBody .mg-out').textContent"))

        # 换回来，继续走归并流程
        cdp.js("(() => { const r = document.querySelectorAll('#mgBody .mg-card input[type=radio]');"
               " r[0].checked = true; r[0].dispatchEvent(new Event('change')); })()")
        cdp.js("document.querySelector('#mgBody button[data-act=preview]').click()")
        time.sleep(0.9)

        # --- 归并要点两次 ---
        cdp.js("window.__reqs = [];")
        cdp.js("document.querySelector('#mgBody button[data-act=merge]').click()")
        time.sleep(0.3)
        check("第一次点归并不发请求（防误触）",
              len([r for r in (cdp.js("window.__reqs") or [])
                   if r["path"].startswith("/api/persons/merge")]) == 0,
              cdp.js("window.__reqs"))
        check("第一次点把按钮切成确认态", cdp.js(
            "document.querySelector('#mgBody button[data-act=merge]').textContent") == "确认归并",
            cdp.js("document.querySelector('#mgBody button[data-act=merge]').textContent"))

        cdp.js("document.querySelector('#mgBody button[data-act=merge]').click()")
        time.sleep(1.0)
        reqs = cdp.js("window.__reqs") or []
        merge_reqs = [r for r in reqs if r["path"].startswith("/api/persons/merge")]
        check("第二次点击才真的归并", len(merge_reqs) == 1, reqs)
        if merge_reqs:
            body = json.loads(merge_reqs[0]["body"] or "{}")
            check("真归并不带 dry_run（或为 false）", not body.get("dry_run"), body)
            check("真归并带 keep_id / drop_ids",
                  body.get("keep_id") == "p-big" and len(body.get("drop_ids") or []) == 2, body)

        out = cdp.js("document.querySelector('#mgBody .mg-out').textContent") or ""
        check("执行结果标着「执行结果」", "执行结果" in out, out[:200])
        check("执行结果说清了删掉几个人物", "删除 1 个" in out, out[:200])
        check("服务端报的错原样显示出来（不吞）", "连接超时" in out, out[:300])
        toasts = cdp.js("Array.from(document.querySelectorAll('#toasts .toast'))"
                        ".map(t => t.textContent).join('|')") or ""
        check("归并给了可见反馈", "归并完成" in toasts, toasts)

        # --- 别名记忆开关真的进请求 ---
        cdp.js(stub_js())
        cdp.js("document.querySelector('#mgAlias').checked = false")
        cdp.js("window.__reqs = [];")
        cdp.js("loadDuplicates()")
        time.sleep(0.8)
        reqs = cdp.js("window.__reqs") or []
        check("取消「参考别名记忆」后请求里带 alias=false",
              reqs and "alias=false" in reqs[0]["path"], reqs)

        check("页面上没有出现未捕获异常", not (cdp.js(
            "window.__errs || []") or []), cdp.js("window.__errs || []"))

        print("\n%d 通过 / %d 失败" % (ok_n, fail_n))
        return 1 if fail_n else 0
    finally:
        cdp.close()
        if proc:
            proc.kill()


if __name__ == "__main__":
    sys.exit(main())
