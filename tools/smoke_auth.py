"""访问认证冒烟：起一个临时实例，验证登录门真的把所有接口都挡住了。

为什么单独有这么一支脚本：
  登录加进来之后，「哪些接口要拦、哪些要放行」变成了最容易出错的地方 ——
  漏拦一个 `/api/...` 就等于整道门白做（配置里躺着 Emby 的管理员令牌）。
  这支脚本把「未登录必须 401」「登录后拿不到明文密钥」「跨站请求被拒」
  「暴力破解会被退避」这几条固定成可回归的断言。

特点：完全不碰真实配置（数据目录指向临时目录），也不依赖 Emby。
用法： python tools/smoke_auth.py
退出码 0 = 全过。
"""
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = os.path.join(ROOT, "EmbyMetaEditor.exe")

FAILED = []


def check(name, cond, detail=""):
    if cond:
        print("PASS  " + name)
    else:
        print("FAIL  " + name + (("  → " + str(detail)) if detail else ""))
        FAILED.append(name)


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def http(method, url, body=None, cookie=None, headers=None):
    req = urllib.request.Request(url, method=method)
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    if cookie:
        req.add_header("Cookie", cookie)
    try:
        with urllib.request.urlopen(req, data=data, timeout=20) as r:
            return r.status, r.read().decode("utf-8", "replace"), r.headers
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), e.headers


def jbody(raw):
    try:
        return json.loads(raw)
    except Exception:
        return {}


class Instance:
    """一个临时实例：独立数据目录 + 独立端口，退出时清理。"""

    def __init__(self, env_extra=None):
        self.home = tempfile.mkdtemp(prefix="embyme-auth-")
        self.port = free_port()
        self.lines = []
        env = dict(os.environ)
        env["EMBYME_HOME"] = self.home
        # 环境变量会覆盖配置里的密码，先清掉，免得本机的设置影响断言
        env.pop("EMBYME_AUTH_PASSWORD", None)
        env.pop("EMBYME_AUTH_USER", None)
        env.update(env_extra or {})
        self.proc = subprocess.Popen(
            [EXE, "-port", str(self.port), "-open=false"],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env,
        )

        def drain():
            for line in self.proc.stdout:
                self.lines.append(line.decode("utf-8", "replace").rstrip("\r\n"))

        threading.Thread(target=drain, daemon=True).start()
        self.wait_ready()
        self.password = self.parse_password()

    def wait_ready(self, timeout=25):
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                code, _, _ = http("GET", self.url("/api/auth/status"))
                if code == 200:
                    return
            except Exception:
                pass
            time.sleep(0.2)
        raise RuntimeError("实例没起来：\n" + "\n".join(self.lines))

    def parse_password(self):
        """首次启动会把随机密码打到控制台，这里照着日志把它捞出来。"""
        for line in self.lines:
            m = re.search(r"密码\s*:\s*(\S+)", line)
            if m:
                return m.group(1)
        return ""

    def url(self, path):
        return "http://127.0.0.1:%d%s" % (self.port, path)

    def login(self, user, password):
        code, raw, hdr = http("POST", self.url("/api/auth/login"),
                              {"username": user, "password": password})
        cookie = ""
        setc = hdr.get("Set-Cookie") or ""
        m = re.search(r"embyme_session=([^;]+)", setc)
        if m:
            cookie = "embyme_session=" + m.group(1)
        return code, jbody(raw), cookie

    def stop(self):
        try:
            self.proc.terminate()
            self.proc.wait(timeout=10)
        except Exception:
            try:
                self.proc.kill()
            except Exception:
                pass
        shutil.rmtree(self.home, ignore_errors=True)


def main():
    if not os.path.isfile(EXE):
        print("找不到 %s，先跑： go build -o EmbyMetaEditor.exe ." % EXE)
        return 1

    # ---------- 第一段：首次启动（自动生成密码）----------
    ins = Instance()
    try:
        print("  [实例 A] 端口 %d，数据目录 %s" % (ins.port, ins.home))
        check("首次启动在控制台打印了随机密码", len(ins.password) == 14, ins.password)

        code, raw, _ = http("GET", ins.url("/api/auth/status"))
        st = jbody(raw).get("data") or {}
        check("未登录时 /api/auth/status 可用", code == 200, code)
        check("未登录时 authenticated=false", st.get("authenticated") is False, st)
        check("首次启动 password_is_new=true", st.get("password_is_new") is True, st)

        # 静态资源放行（登录界面本身活在这里）
        for path in ("/", "/app.js", "/style.css", "/favicon.png"):
            code, _, _ = http("GET", ins.url(path))
            check("静态资源放行 %s" % path, code == 200, code)

        # 未登录时所有数据接口一律 401
        protected = [
            ("GET", "/api/config"), ("GET", "/api/items"), ("GET", "/api/stats"),
            ("GET", "/api/libraries"), ("GET", "/api/persons"), ("GET", "/api/jobs"),
            ("GET", "/api/emby/status"), ("GET", "/api/gfriends/status"),
            ("GET", "/api/metatube/providers"), ("GET", "/api/cn/sites"),
            ("GET", "/api/img?u=https://www.javbus.com/pics/cover/x.jpg"),
            ("POST", "/api/items/update"), ("POST", "/api/config"),
            ("POST", "/api/cn/scrape"), ("POST", "/api/javbus/scan"),
        ]
        for method, path in protected:
            code, _, _ = http(method, ins.url(path), {} if method == "POST" else None)
            check("未登录被拦 %s %s" % (method, path), code == 401, code)

        # 正确凭据
        code, body, cookie = ins.login("admin", ins.password)
        check("正确密码可以登录", code == 200 and bool(cookie), (code, body))
        check("登录响应只回用户名", set((body.get("data") or {}).keys()) == {"username"}, body)

        # 跨站请求被拒（CSRF）
        code, _, _ = http("POST", ins.url("/api/config"), {},
                          cookie=cookie, headers={"Origin": "http://evil.example"})
        check("跨站 Origin 被拒（403）", code == 403, code)
        code, _, _ = http("POST", ins.url("/api/auth/logout"), {},
                          cookie=cookie, headers={"Origin": "http://127.0.0.1:%d" % ins.port})
        check("同源 Origin 放行", code == 200, code)

        # 登录后读配置：数据都在，密钥一个都不在
        code, _, cookie = ins.login("admin", ins.password)
        code, raw, _ = http("GET", ins.url("/api/config"), cookie=cookie)
        cfg = jbody(raw).get("data") or {}
        check("登录后能读配置", code == 200 and bool(cfg), code)
        leaked = {k: cfg.get(k) for k in
                  ("password", "api_key", "token", "metatube_token", "javbus_cookie")
                  if cfg.get(k)}
        leaked.update({k: cfg["openai"][k] for k in ("api_key",)
                       if (cfg.get("openai") or {}).get(k)})
        check("配置里没有任何明文密钥", not leaked and not (cfg.get("auth") or {}).get("password_hash"), leaked)
        sec = cfg.get("secrets") or {}
        check("secrets 标记已保存的项（javbus cookie 有默认值）", sec.get("javbus_cookie") is True, sec)
        check("配置里带上了认证信息", (cfg.get("auth") or {}).get("username") == "admin", cfg.get("auth"))

        # 空值提交不会抹掉已保存的密钥（这是最容易复发的坑）
        before = (json.load(open(os.path.join(ins.home, "config.json"), encoding="utf-8")))
        http("POST", ins.url("/api/config"), {"username": "someone", "javbus_cookie": "",
                                             "api_key": "", "metatube_token": "",
                                             "openai": {"api_key": "", "enabled": False}},
             cookie=cookie)
        after = json.load(open(os.path.join(ins.home, "config.json"), encoding="utf-8"))
        check("密钥留空提交不会清空原有值",
              after.get("javbus_cookie") == before.get("javbus_cookie") != "",
              (before.get("javbus_cookie"), after.get("javbus_cookie")))

        # 改密码：旧密码错会被拒
        code, _, _ = http("POST", ins.url("/api/auth/password"),
                          {"old_password": "nope", "new_password": "brand-new-pass"},
                          cookie=cookie)
        check("改密码要验旧密码", code == 401, code)
        code, _, _ = http("POST", ins.url("/api/auth/password"),
                          {"old_password": ins.password, "new_password": "brand-new-pass"},
                          cookie=cookie)
        check("改密码成功", code == 200, code)
        code, _, _ = ins.login("admin", "brand-new-pass")
        check("新密码可登录", code == 200, code)
        code, _, _ = ins.login("admin", ins.password)
        check("旧密码失效", code == 401, code)

        # 退出登录
        code, _, cookie2 = ins.login("admin", "brand-new-pass")
        code, _, _ = http("POST", ins.url("/api/auth/logout"), {}, cookie=cookie2)
        check("退出登录返回 200", code == 200, code)
        code, _, _ = http("GET", ins.url("/api/config"), cookie=cookie2)
        check("退出后会话立即失效", code == 401, code)

        # 暴力破解退避放最后：它会把本机 IP 关在门外一段时间，
        # 后面的断言再想登录就会被 429 挡住。
        code, _, _ = ins.login("admin", "definitely-wrong")
        check("密码错返回 401", code == 401, code)
        code, _, _ = ins.login("not-admin", "definitely-wrong")
        check("用户名错返回 401", code == 401, code)
        codes = [ins.login("admin", "wrong-%d" % i)[0] for i in range(8)]
        check("连续失败后开始退避（出现 429）", 429 in codes, codes)

        # Emby 图片代取在没有 Emby 令牌时应当明确报错，而不是 200 空图
        code, _, _ = http("GET", ins.url("/api/emby/image?id=12345&type=Primary"),
                          cookie=cookie)
        check("未登录 Emby 时图片代取报 502", code == 502, code)
        code, _, _ = http("GET", ins.url("/api/emby/image?id=../../System/Info"),
                          cookie=cookie)
        check("图片代取拒绝非法 id", code == 400, code)
    finally:
        ins.stop()

    # ---------- 第二段：环境变量指定密码 ----------
    ins2 = Instance({"EMBYME_AUTH_PASSWORD": "from-env-pass", "EMBYME_AUTH_USER": "boss"})
    try:
        print("  [实例 B] 端口 %d，密码来自 EMBYME_AUTH_PASSWORD" % ins2.port)
        check("环境变量指定密码时不再打印密码", ins2.password == "", ins2.lines[-6:])
        code, _, _ = ins2.login("boss", "from-env-pass")
        check("环境变量里的用户名密码可登录", code == 200, code)
        code, _, _ = ins2.login("admin", "from-env-pass")
        check("环境变量里的用户名生效（admin 不再可用）", code == 401, code)
        code, raw, _ = http("GET", ins2.url("/api/auth/status"))
        st = jbody(raw).get("data") or {}
        check("环境变量密码不算「新生成的」", st.get("password_is_new") is False, st)
    finally:
        ins2.stop()

    print("\n%s" % ("全部通过" if not FAILED else "%d 项未通过：%s" % (len(FAILED), "；".join(FAILED))))
    return 1 if FAILED else 0


if __name__ == "__main__":
    sys.exit(main())
