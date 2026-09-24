#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""磁力列表「按番号分页」的渲染层回归。

不开真实 javbus 扫描（慢且依赖外网），而是把已经成型的分组数据直接喂给
renderMagnets()，然后在真实浏览器里断言：

  1. 番号标签个数 = 分组个数，且顺序与数据一致
  2. 同一时刻只有一个 panel 处于 .on（点哪个看哪个）
  3. 无磁力的分组标签带 .bad，面板里显示占位文案
  4. 点击标签真的切换面板（不是只改 class 没换内容）
  5. 「复制当前番号」只复制当前标签的链接
  6. 重新渲染时保持当前选中的番号

需要先起 exe（127.0.0.1:8097）+ 无头 Edge。
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


def main():
    if subprocess.call(["curl", "-s", "--noproxy", "*", "-m", "3", "-o", os.devnull, BASE]) != 0:
        print("8097 没起来，先运行 EmbyMetaEditor.exe")
        return 2

    profile = os.path.join(tempfile.gettempdir(), "emby-magtab-profile")
    # Edge 要求 --user-data-dir 是非默认目录；用 Windows 风格路径
    profile = profile.replace("\\", "/")
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

        # 造三组数据：2 条 / 无 / 3 条
        groups = [
            {"number": "SSNI-989", "title": "SSNI-989 タイトルA",
             "magnets": [{"name": "a1", "link": "magnet:?xt=urn:btih:AAA1", "size": "5.5GB", "date": "2021-03-14"},
                         {"name": "a2", "link": "magnet:?xt=urn:btih:AAA2", "size": "6.1GB", "date": "2021-03-15"}]},
            {"number": "SSNI-963", "title": "SSNI-963 タイトルB", "magnets": [],
             "error": "javbus 没有收录磁力"},
            {"number": "SSNI-001", "title": "SSNI-001 タイトルC",
             "magnets": [{"name": "c1", "link": "magnet:?xt=urn:btih:CCC1", "size": "4GB", "date": ""},
                         {"name": "c2", "link": "magnet:?xt=urn:btih:CCC2", "size": "4.5GB", "date": ""},
                         {"name": "c3", "link": "magnet:?xt=urn:btih:CCC3", "size": "5GB", "date": ""}]},
        ]
        # 切到「番号补全」视图，否则 #jbBody 所在的 .view 是 display:none，截不到图
        cdp.js("switchView('javbus')")
        # 先把磁力卡片所在的容器渲染出来（真实流程里由 renderJbMissing 生成）
        cdp.js("document.querySelector('#jbBody').innerHTML = "
               "'<div class=\"card\" id=\"jbMagCard\" style=\"display:none\"><h3>磁力列表</h3>"
               "<button id=\"jbCopyOne\"></button><button id=\"jbCopyAll\"></button>"
               "<div class=\"maglist\" id=\"jbMagList\"></div></div>';")
        cdp.js("S.jb.magnets = %s; S.jb.magTab = ''; renderMagnets();" % json.dumps(groups, ensure_ascii=False))

        tabs = cdp.js("Array.from(document.querySelectorAll('#jbMagList .magtab')).map(b => b.dataset.num)")
        check("番号标签个数 = 3", tabs == ["SSNI-989", "SSNI-963", "SSNI-001"], tabs)
        check("默认选中第一个番号",
              cdp.js("document.querySelector('#jbMagList .magtab.on').dataset.num") == "SSNI-989")
        check("同时只有一个面板可见",
              cdp.js("document.querySelectorAll('#jbMagList .magpanel.on').length") == 1)
        check("可见面板是第一个番号",
              cdp.js("document.querySelector('#jbMagList .magpanel.on').dataset.num") == "SSNI-989")
        check("标签上带磁力条数角标",
              cdp.js("document.querySelectorAll('#jbMagList .magtab')[0].querySelector('i').textContent") == "2")
        check("无磁力的标签带 .bad",
              cdp.js("document.querySelectorAll('#jbMagList .magtab')[1].classList.contains('bad')") is True)
        check("无磁力的角标显示「无」",
              cdp.js("document.querySelectorAll('#jbMagList .magtab')[1].querySelector('i').textContent") == "无")
        check("无磁力面板显示原因",
              "javbus 没有收录磁力" in (cdp.js("document.querySelectorAll('#jbMagList .magpanel')[1].textContent") or ""))

        # 面板真的按 .on 隐藏（不只是 class）
        check("非选中面板 display:none",
              cdp.js("getComputedStyle(document.querySelectorAll('#jbMagList .magpanel')[1]).display") == "none")

        # 点第三个标签
        cdp.js("document.querySelectorAll('#jbMagList .magtab')[2].click()")
        time.sleep(0.3)
        check("点击后选中第三个番号",
              cdp.js("document.querySelector('#jbMagList .magtab.on').dataset.num") == "SSNI-001")
        check("点击后面板切到第三个",
              cdp.js("document.querySelector('#jbMagList .magpanel.on').dataset.num") == "SSNI-001")
        check("点击后可见面板只有 1 个",
              cdp.js("document.querySelectorAll('#jbMagList .magpanel.on').length") == 1)
        check("切换后旧面板隐藏",
              cdp.js("getComputedStyle(document.querySelectorAll('#jbMagList .magpanel')[0]).display") == "none")
        check("第三个面板有 3 行磁力",
              cdp.js("document.querySelectorAll('#jbMagList .magpanel.on .magrow').length") == 3)

        # 复制当前番号：拦截 clipboard，只应拿到当前标签的 3 条
        cdp.js("window.__cp = null; navigator.clipboard.writeText = (t) => { window.__cp = t; return Promise.resolve(); };")
        cdp.js("document.querySelector('#jbCopyOne').click()")
        time.sleep(0.3)
        cp = cdp.js("window.__cp") or ""
        check("复制当前番号只含当前番号的链接",
              cp.count("magnet:") == 3 and "CCC1" in cp and "AAA1" not in cp, cp[:120])

        cdp.js("window.__cp = null; document.querySelector('#jbCopyAll').click()")
        time.sleep(0.3)
        cp2 = cdp.js("window.__cp") or ""
        check("复制全部含所有番号的链接",
              cp2.count("magnet:") == 5 and "AAA1" in cp2 and "CCC3" in cp2, cp2.count("magnet:"))

        # 重渲染保持当前选中
        cdp.js("renderMagnets()")
        time.sleep(0.2)
        check("重渲染后仍停在第三个番号",
              cdp.js("document.querySelector('#jbMagList .magtab.on').dataset.num") == "SSNI-001")

        # 选中项消失时回到第一个
        cdp.js("S.jb.magnets = S.jb.magnets.slice(0, 2); renderMagnets()")
        time.sleep(0.2)
        check("当前番号消失后回到第一个",
              cdp.js("document.querySelector('#jbMagList .magtab.on').dataset.num") == "SSNI-989")

        # 空数据
        cdp.js("S.jb.magnets = []; renderMagnets()")
        time.sleep(0.2)
        check("空数据时显示占位",
              "没有磁力数据" in (cdp.js("document.querySelector('#jbMagList').textContent") or ""))

        # 截图（视口截图，吞异常）
        try:
            cdp.js("S.jb.magnets = %s; S.jb.magTab=''; renderMagnets();" % json.dumps(groups, ensure_ascii=False))
            # 清掉前面测试留下的 toast，并让卡片滚进视口再截
            cdp.js("document.querySelectorAll('.toast').forEach((t) => t.remove());"
                   "document.querySelector('#jbMagCard').scrollIntoView({block:'start'});")
            time.sleep(0.5)
            shot = cdp.call("Page.captureScreenshot", format="png")
            import base64
            out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "screenshots", "magnet_tabs.png")
            out = os.path.abspath(out)
            with open(out, "wb") as f:
                f.write(base64.b64decode(shot["data"]))
            print("      截图 " + out)
        except Exception as e:
            print("      截图跳过：" + str(e)[:100])
    finally:
        cdp.close()
        proc.kill()

    print("\n结果：%d 通过 / %d 失败" % (ok_n, fail_n))
    return 1 if fail_n else 0


if __name__ == "__main__":
    sys.exit(main())
