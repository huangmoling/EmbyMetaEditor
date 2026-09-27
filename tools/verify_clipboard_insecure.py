#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""验证**非安全上下文**下复制依然可用 —— Docker / NAS 的真实访问方式。

背景（这是一个真实报障）：
`navigator.clipboard` 只在**安全上下文**里存在。本机 http://127.0.0.1:8097 算安全
上下文，所以本机跑 exe 一切正常；但 Docker 部署后大家都是从 http://<局域网IP>:8097
打开的，那里 `navigator.clipboard` 直接是 undefined，
`navigator.clipboard.writeText(...)` 会抛 TypeError —— 而且抛在 onclick 里，没人接，
**连「复制失败」的提示都没有**，用户看到的就是「点复制没反应」。

不能拿 127.0.0.1 测这个：它恰好是安全上下文，永远测不出问题。所以本脚本要求先把
应用起在 0.0.0.0 上，再从局域网地址访问：

    EMBYME_AUTH_PASSWORD=test-pass EmbyMetaEditor.exe \\
        -host 0.0.0.0 -port 8098 -dir <临时数据目录> -open=false

然后：

    python tools/verify_clipboard_insecure.py

局域网 IP 默认从 config.json 里的 Emby 地址推出来（同一台机器），
也可以用 EMBYME_LAN_IP / EMBYME_INSECURE_PORT 覆盖。

验证内容：
  1. 这个 origin 确实不是安全上下文（isSecureContext === false）
  2. navigator.clipboard 确实不存在，且裸调它会抛 TypeError（当年的 bug 本体）
  3. 点「复制当前番号」依然复制成功（回退到 execCommand），并给出可见提示
  4. 两条路都不通时给出**可见**的失败提示，而不是静默
  5. 没有未捕获异常 / console 报错
"""
import json
import os
import re
import socket
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from verify_javbus_probe import Page, http_json  # noqa: E402
import wbauth  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PORT = os.environ.get("EMBYME_INSECURE_PORT", "8098")


def lan_ip():
    """本机的局域网地址。

    优先问操作系统「这个 IP 走哪块网卡出去」（UDP connect 不发包，只做路由选择，
    所以不依赖对端可达）。**不能拿 config.json 里 Emby 的地址当本机地址**：
    Emby 常在另一台机器上（这台机器就是这样，而且是 DHCP，实测已经从 .110 变成 .197）。
    """
    env = os.environ.get("EMBYME_LAN_IP")
    if env:
        return env
    for probe in ("192.168.1.110", "223.5.5.5"):
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            s.settimeout(2)
            s.connect((probe, 80))
            ip = s.getsockname()[0]
            if ip and not ip.startswith("127."):
                return ip
        except Exception:
            pass
        finally:
            s.close()
    try:
        with open(os.path.join(ROOT, "config.json"), encoding="utf-8") as f:
            url = json.load(f).get("emby_url", "")
        m = re.match(r"https?://([^:/]+)", url)
        if m:
            return m.group(1)
    except Exception:
        pass
    return "127.0.0.1"


BASE = "http://%s:%s/" % (lan_ip(), PORT)


def main():
    results = []

    def check(name, ok, detail=""):
        results.append((name, ok, detail))
        print(("PASS  " if ok else "FAIL  ") + name +
              (("  | " + str(detail)) if detail else ""))

    try:
        http_json("/json/version")
    except Exception as e:
        print("连不上无头 Edge（9333）：%s" % e)
        return 2

    print("目标：%s（非安全上下文）" % BASE)
    tgt = http_json("/json/new?about:blank", method="PUT")
    page = Page(tgt["webSocketDebuggerUrl"])
    for m in ("Runtime.enable", "Log.enable", "Network.enable", "Page.enable"):
        page.send(m)

    try:
        page.send("Page.navigate", url=BASE)
        wbauth.login_in_browser(page)
        page.send("Page.navigate", url=BASE)
        page.wait_for("!!document.querySelector('#lgSkip')", desc="登录页出现")
        page.pump(0.6)
        page.eval("document.querySelector('#lgSkip').click()")
        page.wait_for("!!document.querySelector('#jbStar')", timeout=60, desc="进入主界面")
        page.pump(0.5)
    except Exception as e:
        print("打开 %s 失败：%s" % (BASE, e))
        print("（要先按脚本头部说明，把应用起在 -host 0.0.0.0 上）")
        return 2

    # 1) 确认这个 origin 真的不是安全上下文
    check("这个 origin 不是安全上下文（模拟 Docker 的访问方式）",
          page.eval("location.origin === %s && window.isSecureContext === false"
                    % json.dumps(BASE.rstrip("/"))),
          page.eval("location.origin + ' / isSecureContext=' + window.isSecureContext"))

    # 2) 确认裸调 clipboard API 会抛异常 —— 这就是用户遇到的「点了没反应」
    raw = page.eval("""(() => {
      if (typeof navigator.clipboard === 'undefined') return 'undefined';
      try { navigator.clipboard.writeText('x'); return 'no-throw'; }
      catch (e) { return e.constructor.name; }
    })()""")
    check("navigator.clipboard 不存在，裸调会抛异常", raw in ("undefined", "TypeError"), raw)

    # 3) 造一组磁力数据，点「复制当前番号」——必须成功且给提示
    page.eval("""(() => {
      const host = document.querySelector('#jbBody') || document.body;
      host.innerHTML = '<div class="card" id="jbMagCard"><h3>磁力列表</h3>'
        + '<button id="jbCopyOne"></button><button id="jbCopyAll"></button>'
        + '<div class="maglist" id="jbMagList"></div></div>';
      S.jb.magnets = [{number: 'SSNI-989', title: 't',
        magnets: [{name: 'a', link: 'magnet:?xt=urn:btih:AAA1', size: '1GB', date: '2021-01-01'}]}];
      S.jb.magTab = '';
      renderMagnets();
      return true;
    })()""")
    page.eval("window.__exec = [];"
              "document.execCommand = function (c) { window.__exec.push(c); return true; };"
              "document.querySelectorAll('.toast').forEach((t) => t.remove());")
    page.eval("document.querySelector('#jbCopyOne').click()")
    page.pump(0.6)
    toasts = page.eval("Array.from(document.querySelectorAll('.toast')).map(t => t.textContent)")
    check("没有 clipboard API 时回退到 execCommand",
          page.eval("window.__exec && window.__exec[0]") == "copy", page.eval("window.__exec"))
    check("复制成功并给出可见提示", any("已复制" in (t or "") for t in (toasts or [])), toasts)

    # 4) 两条路都不通时必须有可见的失败提示（不能像以前那样静默）
    page.eval("document.querySelectorAll('.toast').forEach((t) => t.remove());"
              "document.execCommand = function () { return false; };")
    page.eval("document.querySelector('#jbCopyOne').click()")
    page.pump(0.6)
    toasts2 = page.eval("Array.from(document.querySelectorAll('.toast')).map(t => t.textContent)")
    check("回退也失败时给出可见提示（不再静默）",
          any("复制失败" in (t or "") for t in (toasts2 or [])), toasts2)

    # 5) 页面错误
    #
    # 已知噪声：以 http://<局域网IP> 访问时，浏览器会说
    # 「Cross-Origin-Opener-Policy header has been ignored, because the URL's origin
    #  was untrustworthy」—— 这是浏览器**必须**这么做的（COOP 只对可信来源生效），
    # 属于明文局域网访问的固有提示，不是应用的问题（上了 TLS 反代就没了）。
    # 只放行这一条，别的报错一律算失败。
    NOISE = ("Cross-Origin-Opener-Policy header has been ignored",)
    errs = []
    for e in page.events:
        m = e.get("method")
        if m == "Runtime.exceptionThrown":
            errs.append("未捕获异常: " + str(e["params"].get("exceptionDetails", {}).get("text")))
        elif m == "Log.entryAdded":
            en = e["params"]["entry"]
            text = str(en.get("text") or "")
            if en.get("level") == "error" and not any(n in text for n in NOISE):
                errs.append("console.error: " + text[:120])
    check("没有未捕获异常 / console 报错（COOP 明文提示除外）", not errs, "；".join(errs[:3]))

    print("\n" + "=" * 70)
    bad = [r for r in results if not r[1]]
    print("共 %d 项，通过 %d，失败 %d" % (len(results), len(results) - len(bad), len(bad)))
    for n, _, d in bad:
        print("  失败：" + n + "  " + str(d))
    print("=" * 70)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
