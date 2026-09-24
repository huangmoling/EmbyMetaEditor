"""验证 Emby 图片代取链路：/api/emby/image 能不能从真实 Emby 取到海报。

背景：图片原来是浏览器直接连 Emby（把令牌拼在 <img src> 的 api_key 里），
现在改成服务端带令牌代取 —— 好处是令牌不再进 DOM，代价是多了一跳。
「页面上所有海报变成空白」正是这次改动最可能出的回归，而它偏偏是单元测试
照不到的地方：要真实 Emby、真实条目、真实图片字节。

只读：全程只 GET，不写 Emby 任何东西。用的是 config.json 的**副本**
（复制到临时目录再改数据目录），不会动到真实配置。

用法： python tools/verify_emby_image.py
退出码 0 = 通过。
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
PASSWORD = "verify-image-pass"
FAILED = []


def check(name, cond, detail=""):
    print(("PASS  " if cond else "FAIL  ") + name + (("" if cond else "  → " + str(detail)) if detail else ""))
    if not cond:
        FAILED.append(name)


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def get(url, cookie=None, headers=None):
    req = urllib.request.Request(url)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    if cookie:
        req.add_header("Cookie", cookie)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read(), dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)


def main():
    if not os.path.isfile(EXE):
        print("找不到 %s，先跑： go build -o EmbyMetaEditor.exe ." % EXE)
        return 1
    cfg_path = os.path.join(ROOT, "config.json")
    if not os.path.isfile(cfg_path):
        print("找不到 config.json（需要里面已配好 Emby 地址与令牌）")
        return 1
    real = json.load(open(cfg_path, encoding="utf-8"))
    if not real.get("emby_url") or not real.get("token"):
        print("config.json 里没有 emby_url / token，跳过")
        return 0

    home = tempfile.mkdtemp(prefix="embyme-img-")
    shutil.copy(cfg_path, os.path.join(home, "config.json"))
    port = free_port()
    base = "http://127.0.0.1:%d" % port
    env = dict(os.environ)
    env["EMBYME_HOME"] = home
    env["EMBYME_AUTH_PASSWORD"] = PASSWORD
    proc = subprocess.Popen([EXE, "-port", str(port), "-open=false"],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env)
    lines = []
    threading.Thread(target=lambda: [lines.append(l.decode("utf-8", "replace"))
                                     for l in proc.stdout], daemon=True).start()
    try:
        deadline = time.time() + 25
        while time.time() < deadline:
            try:
                if get(base + "/api/auth/status")[0] == 200:
                    break
            except Exception:
                pass
            time.sleep(0.2)
        else:
            print("实例没起来：\n" + "".join(lines))
            return 1

        # 登录拿会话
        req = urllib.request.Request(
            base + "/api/auth/login",
            data=json.dumps({"username": "admin", "password": PASSWORD}).encode(),
            method="POST", headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=20) as r:
            cookie = re.search(r"embyme_session=([^;]+)", r.headers.get("Set-Cookie", "")).group(1)
        cookie = "embyme_session=" + cookie
        print("  已登录，开始找带海报的条目…")

        # 找一个真的有 Primary 图的条目，顺便拿到它的 tag（缓存键要用）
        code, body, _ = get(base + "/api/items?limit=60", cookie=cookie)
        data = json.loads(body.decode("utf-8")).get("data") or {}
        items = data.get("items") or []
        target = None
        for it in items:
            tag = ((it.get("ImageTags") or {}).get("Primary"))
            if tag:
                target = (it.get("Id"), tag, it.get("Name"))
                break
        if not target:
            print("  条目里没有带 Primary 海报的，跳过（共 %d 条）" % len(items))
            return 0
        item_id, tag, name = target
        print("  目标条目：%s（%s）" % (name, item_id))

        url = "%s/api/emby/image?id=%s&type=Primary&h=300&tag=%s" % (base, item_id, tag)
        code, img, hdr = get(url, cookie=cookie)
        ctype = hdr.get("Content-Type", "")
        check("代取返回 200", code == 200, (code, img[:200]))
        check("返回的是图片（%s）" % ctype, ctype.startswith("image/"), ctype)
        check("图片不是空壳（%d 字节）" % len(img), len(img) > 1000, len(img))
        check("不是 HTML 错误页", not img[:15].lower().startswith(b"<!doctype"))

        # 和「直连 Emby」比一下字节数：代取应当拿到同一张图
        direct = urllib.request.Request(
            "%s/Items/%s/Images/Primary?maxHeight=300&quality=90&tag=%s"
            % (real["emby_url"].rstrip("/"), item_id, tag))
        direct.add_header("X-Emby-Token", real["token"])
        try:
            with urllib.request.urlopen(direct, timeout=60) as r:
                ref = r.read()
            check("与直连 Emby 拿到的图完全一致（%d 字节）" % len(ref), ref == img, (len(ref), len(img)))
        except Exception as e:
            print("  提示：直连 Emby 对照失败（%s），跳过一致性比对" % e)

        # 不带 tag 也要能出图（前端在有些地方拿不到 tag）
        code, _, _ = get("%s/api/emby/image?id=%s&type=Primary&h=120" % (base, item_id), cookie=cookie)
        check("不带 tag 也能代取", code == 200, code)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        shutil.rmtree(home, ignore_errors=True)

    print("\n%s" % ("全部通过" if not FAILED else "%d 项未通过：%s" % (len(FAILED), "；".join(FAILED))))
    return 1 if FAILED else 0


if __name__ == "__main__":
    sys.exit(main())
