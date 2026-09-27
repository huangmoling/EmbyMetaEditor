#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""磁力列表「按番号分页」的渲染层回归。

不开真实 javbus 扫描（慢且依赖外网），而是把已经成型的分组数据直接喂给
renderMagnets()，然后在真实浏览器里断言：

  1. 番号标签个数 = 分组个数，且顺序与数据一致
  2. 同一时刻只有一个 panel 处于 .on（点哪个看哪个）
  3. 无磁力的分组标签带 .bad，面板里显示占位文案
  4. 点击标签真的切换面板（不是只改 class 没换内容）
  5. 每行磁力是**完整** URI 的 <a href>（超长地址不截断，dn/tr 参数都在）
  6. 「复制当前番号」只复制当前标签的链接，且复制出来的是完整地址
  7. 没有 navigator.clipboard 时（Docker / 局域网 http 访问）回退 execCommand 成功
  8. 两条路都不通时必须给出可见的失败提示（不能静默）
  9. 重新渲染时保持当前选中的番号

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
        #
        # 第三组第一条刻意用**超长**磁力地址（200+ 字符，带 dn 与两条 tr）：
        # 以前这一行渲染成纯文本并把地址截到 110 字符，DOM 里根本没有完整 URI，
        # 页面上的磁力工具（以及浏览器自己的磁力处理器）都认不出来，
        # 表现就是「点磁力链接没反应 / 弹出来的预览是空的」。
        LONG_MAG = ("magnet:?xt=urn:btih:CCC3A1B2C3D4E5F60718293A4B5C6D7E8F90A1B2"
                    "&dn=SSNI-001%20%E4%B8%AD%E6%96%87%E5%AD%97%E5%B9%95%E7%89%88"
                    "&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce"
                    "&tr=udp%3A%2F%2Ftracker.torrent.eu.org%3A451%2Fannounce")
        groups = [
            {"number": "SSNI-989", "title": "SSNI-989 タイトルA",
             "magnets": [{"name": "a1", "link": "magnet:?xt=urn:btih:AAA1", "size": "5.5GB", "date": "2021-03-14"},
                         {"name": "a2", "link": "magnet:?xt=urn:btih:AAA2", "size": "6.1GB", "date": "2021-03-15"}]},
            {"number": "SSNI-963", "title": "SSNI-963 タイトルB", "magnets": [],
             "error": "javbus 没有收录磁力"},
            {"number": "SSNI-001", "title": "SSNI-001 タイトルC",
             "magnets": [{"name": "c1", "link": LONG_MAG, "size": "4GB", "date": ""},
                         {"name": "c2", "link": "magnet:?xt=urn:btih:CCC2", "size": "4.5GB", "date": ""},
                         {"name": "c3", "link": "magnet:?xt=urn:btih:CCC4", "size": "5GB", "date": ""}]},
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

        # 磁力地址必须以**完整** URI 落在 DOM 里（<a href>），而不是截断的纯文本。
        # 截断过的地址肉眼看着像链接，实际点不动、磁力工具也识别不了。
        rows = cdp.js(
            "Array.from(document.querySelectorAll('#jbMagList .magpanel.on .magrow')).map(function (r) {"
            "  var a = r.querySelector('a.maglink');"
            "  return {href: a ? a.getAttribute('href') : null, text: a ? a.textContent : null,"
            "          anchors: r.querySelectorAll('a').length,"
            "          full: a ? a.getAttribute('href') === a.textContent : false};})")
        check("每行磁力都是真的 <a> 而不是一段文字",
              bool(rows) and all(r["anchors"] == 1 and r["href"] for r in rows), rows)
        check("超长磁力地址没有被截断",
              bool(rows) and rows[0]["href"] == LONG_MAG, (rows[0]["href"] or "")[:60] if rows else None)
        check("锚点文本也是完整地址（省略交给 CSS）",
              bool(rows) and "…" not in (rows[0]["text"] or "") and rows[0]["text"] == LONG_MAG,
              (rows[0]["text"] or "")[-30:] if rows else None)
        check("地址里的查询参数（dn/tr）都还在",
              bool(rows) and "&dn=" in (rows[0]["href"] or "") and (rows[0]["href"] or "").count("&tr=") == 2,
              (rows[0]["href"] or "")[:60] if rows else None)

        # 复制当前番号：拦截 clipboard，只应拿到当前标签的 3 条
        cdp.js("window.__cp = null; navigator.clipboard.writeText = (t) => { window.__cp = t; return Promise.resolve(); };")
        cdp.js("document.querySelector('#jbCopyOne').click()")
        time.sleep(0.3)
        cp = cdp.js("window.__cp") or ""
        check("复制当前番号只含当前番号的链接",
              cp.count("magnet:") == 3 and "CCC3A1B2" in cp and "AAA1" not in cp, cp[:120])
        check("复制出来的也是完整地址",
              LONG_MAG in cp, [ln[:50] for ln in cp.splitlines()][:1])

        cdp.js("window.__cp = null; document.querySelector('#jbCopyAll').click()")
        time.sleep(0.3)
        cp2 = cdp.js("window.__cp") or ""
        check("复制全部含所有番号的链接",
              cp2.count("magnet:") == 5 and "AAA1" in cp2 and "CCC4" in cp2, cp2.count("magnet:"))

        # 非安全上下文（Docker / NAS 上 http://<局域网IP>:8097）根本没有
        # navigator.clipboard。以前直接调它会抛异常，被 onclick 吞掉 —— 点复制毫无反应。
        # 现在必须：要么回退成功，要么至少给出可见的失败提示。
        #
        # 注意不能用 `delete navigator.clipboard`：它是 Navigator.prototype 上的
        # getter，delete 掉的是实例上并不存在的自有属性，删完照样有值。得在实例上
        # 定义同名的 own property 把原型那个盖住。
        cdp.js("Object.defineProperty(navigator, 'clipboard', "
               "{value: undefined, configurable: true, writable: true});")
        check("把 clipboard 藏起来后确实拿不到它", cdp.js("typeof navigator.clipboard") == "undefined")
        cdp.js("window.__exec = []; document.execCommand = function (c) { window.__exec.push(c); return true; };"
               "document.querySelectorAll('.toast').forEach((t) => t.remove());")
        cdp.js("document.querySelector('#jbCopyOne').click()")
        time.sleep(0.3)
        check("没有 clipboard API 时回退到 execCommand",
              cdp.js("window.__exec && window.__exec[0]") == "copy", cdp.js("window.__exec"))
        check("回退成功照样提示已复制",
              "已复制" in (cdp.js("Array.from(document.querySelectorAll('.toast')).map(t=>t.textContent).join('|')") or ""),
              cdp.js("Array.from(document.querySelectorAll('.toast')).map(t=>t.textContent)"))

        # 两条路都不通时，必须有**看得见**的失败提示，而不是静默
        cdp.js("document.querySelectorAll('.toast').forEach((t) => t.remove());"
               "document.execCommand = function () { return false; };")
        cdp.js("document.querySelector('#jbCopyOne').click()")
        time.sleep(0.3)
        check("回退也失败时给出可见提示（不再静默）",
              "复制失败" in (cdp.js("Array.from(document.querySelectorAll('.toast')).map(t=>t.textContent).join('|')") or ""),
              cdp.js("Array.from(document.querySelectorAll('.toast')).map(t=>t.textContent)"))

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
