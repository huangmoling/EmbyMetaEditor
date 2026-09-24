"""验证脚本共用的「访问认证」辅助。

背景：界面从 v1.0.8 起有了**进程自己的**登录（保护「谁能打开这个界面」），
未登录时后端对所有 `/api/` 一律返回 401。于是原来那批脚本会整片报错 ——
不是功能坏了，而是它们没先过这道门。

用法（起 exe 时用同一个密码，两边默认都是 `test-pass`）：

    EMBYME_AUTH_PASSWORD=test-pass EmbyMetaEditor.exe -port 8097 -open=false

- 用 urllib 直接打接口的脚本（`smoke_real.py`）→ `cookie_header(BASE)`
- 用 CDP 驱动浏览器的脚本（`verify_*.py`）→ `login_in_browser(page/cdp)`
"""
import json
import os
import re
import time
import urllib.error
import urllib.request

DEFAULT_USER = "admin"
DEFAULT_PASSWORD = "test-pass"


def user():
    return os.environ.get("EMBYME_AUTH_USER") or DEFAULT_USER


def password():
    return os.environ.get("EMBYME_AUTH_PASSWORD") or DEFAULT_PASSWORD


def howto():
    return ("起 exe 时要带上同一个密码（脚本默认 test-pass）：\n"
            "  EMBYME_AUTH_PASSWORD=%s EmbyMetaEditor.exe -port 8097 -open=false" % password())


def login_js():
    """一段在页面里执行的 JS：用 /api/auth/login 换会话 cookie。

    用相对路径 —— 它跑在应用自己的页面里，origin 就是应用本身。
    """
    return ("(async () => {"
            "  const r = await fetch('/api/auth/login', {method:'POST',"
            "    headers:{'Content-Type':'application/json'},"
            "    body: JSON.stringify({username:%s, password:%s})});"
            "  if (!r.ok) throw new Error('访问认证登录失败 HTTP ' + r.status);"
            "  return true;"
            "})()" % (json.dumps(user()), json.dumps(password())))


def _eval(obj, expr):
    """兼容两个脚本里各自的 CDP 包装类：Page.eval(expr, await_promise=) / CDP.js(expr)。"""
    if hasattr(obj, "js"):
        return obj.js(expr)
    return obj.eval(expr, await_promise=True)


def login_in_browser(obj, timeout=30):
    """在已经 Page.navigate 过的 CDP 会话里过掉访问认证。

    为什么要重试：`Page.navigate` 是异步的，紧接着 evaluate 很可能还跑在
    about:blank 上（那时 fetch 的相对地址无效，会抛异常）。这里就靠这个异常
    当「页面还没落地」的信号，一直重试到真的登录成功为止。

    登录成功后**再导航一次**：前端的 boot() 只在页面加载时跑，不重载会一直停在登录门。
    """
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            _eval(obj, login_js())
            return True
        except Exception as e:  # noqa: BLE001 —— 页面未就绪 / 上下文被导航销毁都走这里
            last = e
            time.sleep(0.3)
    raise RuntimeError("过访问认证失败：%s\n%s" % (last, howto()))


def cookie_header(base, timeout=20):
    """用 urllib 登一次，返回可以直接塞进请求头的 Cookie 串。"""
    base = base.rstrip("/")
    body = json.dumps({"username": user(), "password": password()}).encode("utf-8")
    req = urllib.request.Request(base + "/api/auth/login", data=body, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            set_cookie = r.headers.get("Set-Cookie", "")
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:200]
        raise SystemExit("访问认证登录失败（HTTP %d）：%s\n%s" % (e.code, detail, howto()))
    except urllib.error.URLError as e:
        raise SystemExit("连不上 %s（%s）—— 先把 exe 起起来，见 README 的「验证脚本一览」。"
                         % (base, e.reason))
    m = re.search(r"embyme_session=([^;]+)", set_cookie)
    if not m:
        raise SystemExit("登录成功却没拿到会话 cookie，认证接口可能改过名字。\n" + howto())
    return "embyme_session=" + m.group(1)
