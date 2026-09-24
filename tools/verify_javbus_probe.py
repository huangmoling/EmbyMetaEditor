"""用 CDP 驱动无头 Edge，端到端验证「连通性诊断」按钮的渲染与交互。

真实点击（而不是直接调函数）：点「跳过登录」-> 切到「番号补全」-> 填演员名 -> 点「连通性诊断」。
全程收集 console 报错与未捕获异常。
"""
import base64
import json
import os
import sys
import time

import websocket
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import wbauth  # noqa: E402  （访问认证辅助，见 wbauth.py）

CDP = "http://127.0.0.1:9333"
APP = "http://127.0.0.1:8097/"
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "screenshots")
os.makedirs(OUT, exist_ok=True)

_id = 0


def http_json(path, method="GET"):
    req = urllib.request.Request(CDP + path, method=method)
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.loads(r.read().decode())


class Page:
    def __init__(self, ws_url):
        self.ws = websocket.create_connection(ws_url, timeout=30,
                                              suppress_origin=True)
        self.events = []

    def send(self, method, **params):
        global _id
        _id += 1
        mid = _id
        self.ws.send(json.dumps({"id": mid, "method": method, "params": params}))
        while True:
            msg = json.loads(self.ws.recv())
            if msg.get("id") == mid:
                if "error" in msg:
                    raise RuntimeError(f"{method} -> {msg['error']}")
                return msg.get("result", {})
            self._keep(msg)

    def _keep(self, msg):
        m = msg.get("method")
        if m in ("Runtime.consoleAPICalled", "Runtime.exceptionThrown",
                 "Log.entryAdded"):
            self.events.append(msg)
        elif m == "Network.responseReceived":
            r = msg["params"]["response"]
            if r.get("status", 0) >= 400:
                self.events.append({"method": "__http__", "url": r.get("url"),
                                    "status": r.get("status")})

    def pump(self, seconds):
        """持续读取事件，避免缓冲区堆积。"""
        end = time.time() + seconds
        self.ws.settimeout(0.3)
        while time.time() < end:
            try:
                self._keep(json.loads(self.ws.recv()))
            except Exception:
                pass
        self.ws.settimeout(30)

    def eval(self, expr, await_promise=False):
        r = self.send("Runtime.evaluate", expression=expr, returnByValue=True,
                      awaitPromise=await_promise, userGesture=True)
        if r.get("exceptionDetails"):
            raise RuntimeError("JS 异常: " + json.dumps(
                r["exceptionDetails"], ensure_ascii=False)[:400])
        return r.get("result", {}).get("value")

    def wait_for(self, expr, timeout=20, desc=""):
        end = time.time() + timeout
        while time.time() < end:
            try:
                if self.eval(expr):
                    return True
            except Exception:
                pass
            self.pump(0.25)
        raise TimeoutError(f"等待超时：{desc or expr}")

    def shot(self, name):
        r = self.send("Page.captureScreenshot", format="png", captureBeyondViewport=True)
        path = os.path.join(OUT, name)
        with open(path, "wb") as f:
            f.write(base64.b64decode(r["data"]))
        print(f"  screenshot -> {os.path.abspath(path)} ({os.path.getsize(path)} bytes)")
        return path

    def errors(self):
        out = []
        for e in self.events:
            m = e.get("method")
            if m == "Runtime.exceptionThrown":
                d = e["params"]["exceptionDetails"]
                out.append("未捕获异常: " + (d.get("text") or "") + " " +
                           json.dumps(d.get("exception", {}).get("description", ""))[:200])
            elif m == "Log.entryAdded":
                en = e["params"]["entry"]
                if en.get("level") in ("error", "warning"):
                    out.append(f"[{en['level']}] {en.get('text')} <- {en.get('url','')}")
            elif m == "Runtime.consoleAPICalled":
                if e["params"].get("type") == "error":
                    out.append("console.error: " + json.dumps(
                        e["params"].get("args", []), ensure_ascii=False)[:200])
            elif m == "__http__":
                out.append(f"HTTP {e['status']} <- {e['url']}")
        return out


def main():
    # 新建标签页
    tgt = http_json("/json/new?about:blank", method="PUT")
    page = Page(tgt["webSocketDebuggerUrl"])
    page.send("Runtime.enable")
    page.send("Log.enable")
    page.send("Network.enable")
    page.send("Page.enable")
    page.send("Emulation.setDeviceMetricsOverride", width=1440, height=1000,
              deviceScaleFactor=1, mobile=False)

    print("1) 打开应用")
    page.send("Page.navigate", url=APP)
    # 先过访问认证这关（未登录时所有 /api/ 都是 401），再重载让 boot() 带着会话跑
    wbauth.login_in_browser(page)
    page.send("Page.navigate", url=APP)
    page.wait_for("!!document.querySelector('#lgSkip')", desc="登录页出现")
    page.pump(0.6)
    print("   title =", page.eval("document.title"))

    # 上一轮可能把 javbus 地址改成了死端口，这里先复位成模拟站点
    page.eval("""(async () => {
      await fetch('/api/config', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({javbus_url: 'http://127.0.0.1:9500'})
      });
      return true;
    })()""", await_promise=True)
    print("   已复位 javbus 地址 -> http://127.0.0.1:9500")
    page.events.clear()

    print("2) 点「跳过登录」")
    page.eval("document.querySelector('#lgSkip').click()")
    page.wait_for("!!document.querySelector('#view-javbus') && "
                  "getComputedStyle(document.querySelector('#view-javbus')).display !== 'none' "
                  "|| !!document.querySelector('.nav button[data-view=\"javbus\"]')",
                  desc="进入主界面")
    page.pump(0.5)

    print("3) 切到「番号补全」")
    page.eval("""(() => {
      const b = [...document.querySelectorAll('button')].find(x => x.dataset.view === 'javbus');
      if (b) b.click();
      return !!b;
    })()""")
    page.wait_for("getComputedStyle(document.querySelector('#view-javbus')).display !== 'none'",
                  desc="javbus 视图可见")
    page.pump(0.4)

    print("4) 确认诊断按钮存在")
    ok = page.eval("!!document.querySelector('#jbProbe')")
    if not ok:
        raise RuntimeError("找不到 #jbProbe 按钮")
    print("   按钮文案 =", page.eval("document.querySelector('#jbProbe').textContent"))
    page.shot("07-javbus-diagnose-before.png")

    print("5) 填演员名并点「连通性诊断」")
    page.eval("""(() => {
      const i = document.querySelector('#jbStar');
      i.value = '三上悠亜';
      i.dispatchEvent(new Event('input', {bubbles: true}));
      return i.value;
    })()""")
    page.eval("document.querySelector('#jbProbe').click()")
    page.wait_for("document.querySelector('#jbBody').textContent.includes('诊断结果')",
                  timeout=60, desc="诊断结果卡片")
    page.pump(0.8)

    text = page.eval("document.querySelector('#jbBody').innerText")
    print("   ---- 诊断卡片内容 ----")
    for line in text.splitlines():
        if line.strip():
            print("   | " + line.strip())
    page.shot("08-javbus-diagnose-ok.png")

    # 关键断言
    checks = {
        "出现『诊断结果』": "诊断结果" in text,
        "标记为连通": "连通" in text,
        "显示目标地址": "127.0.0.1:9500" in text,
        "显示 HTTP 状态": "200" in text,
        "页面结构正常": "正常" in text,
        "演员搜索接口有结果": "演员搜索接口" in text and "json" in text,
    }
    print("   ---- 断言 ----")
    bad = 0
    for k, v in checks.items():
        print(f"   {'PASS' if v else 'FAIL'}  {k}")
        if not v:
            bad += 1

    print("6) 切到不可达地址再诊断（验证失败路径的提示）")
    page.eval("""(async () => {
      await fetch('/api/config', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({javbus_url: 'http://127.0.0.1:9999'})
      });
      return true;
    })()""")
    page.eval("document.querySelector('#jbProbe').click()")
    page.wait_for("document.querySelector('#jbBody').textContent.includes('无法连接')",
                  timeout=60, desc="不可达提示")
    page.pump(0.6)
    text2 = page.eval("document.querySelector('#jbBody').innerText")
    print("   ---- 失败路径卡片 ----")
    for line in text2.splitlines():
        if line.strip():
            print("   | " + line.strip())
    page.shot("09-javbus-diagnose-fail.png")

    checks2 = {
        "提示『无法连接』": "无法连接" in text2,
        "标记为失败（红色）": "失败" in text2,
    }
    for k, v in checks2.items():
        print(f"   {'PASS' if v else 'FAIL'}  {k}")
        if not v:
            bad += 1

    print("\n7) 页面报错检查")
    # 本测试的 Emby 指向死地址（127.0.0.1:9999），这几个接口报 502 是预期内的，
    # 不算前端缺陷；只关心除此之外的报错。
    EXPECTED = ("/api/libraries", "/api/stats", "/api/emby", "/api/persons", "/favicon")
    errs = page.errors()
    real, expected = [], []
    for e in errs:
        (expected if any(k in e for k in EXPECTED) else real).append(e)
    if expected:
        print("   以下报错来自测试用的死 Emby 地址，预期内，忽略：")
        for e in expected:
            print("   ~ " + e)
    if real:
        for e in real:
            print("   ! " + e)
        bad += len(real)
    else:
        print("   无前端自身报错 / 无未捕获异常")

    print(f"\n结论：{'全部通过' if bad == 0 else str(bad) + ' 项未通过'}")
    return 1 if bad else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as e:
        print("E2E 失败:", e)
        sys.exit(2)
