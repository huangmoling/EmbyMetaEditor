#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""抓一次 javbus 真实详情页，**逐字节**截出样例图区块，写进 testdata/javbus/。

为什么不用手写夹具：`#sample-waterfall` 里每张样例图都是
`<a class="sample-box" href="dmm 原图"><div class="photo-frame"><img src="/pics/sample/xxx_N.jpg"></div></a>`，
解析要挑「内层 img 的相对路径」而不是「外层 a 的绝对地址」。手写夹具时结构写歪一点
（比如少套一层 div、把 href 写成 img 的 src），解析器照样能过，线上却全空 ——
所以夹具一律从真实响应里截，一个字节都不改。

需要 config.json 里的 javbus_url 与 javbus_cookie（该站有验证页，没 cookie 会
被重定向到 /doc/driver-verify）。用法：

    python tools/fetch_javbus_fixture.py            # 默认 SSIS-001
    python tools/fetch_javbus_fixture.py IPX-777    # 换个番号
"""
import gzip
import io
import json
import os
import re
import sys
import urllib.request
import zlib

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "testdata", "javbus")

VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link",
        "meta", "param", "source", "track", "wbr"}
TAG_RE = re.compile(r"<(/?)([a-zA-Z][a-zA-Z0-9]*)((?:[^>\"']|\"[^\"]*\"|'[^']*')*)>")


def slice_element(html, start_idx):
    """从 start_idx（指向某个开始标签的 '<'）起，截出该元素完整的一段。"""
    depth = 0
    for m in TAG_RE.finditer(html, start_idx):
        tag = m.group(2).lower()
        if tag in VOID:
            continue
        if m.group(1):
            depth -= 1
            if depth == 0:
                return html[start_idx:m.end()]
        else:
            depth += 1
    raise ValueError("标签没有闭合")


def fetch(url, cookie):
    hdr = {
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
                      "(KHTML, like Gecko) Chrome/124.0 Safari/537.36",
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
        "Accept-Language": "zh-CN,zh;q=0.9,ja;q=0.8",
        "Referer": url.rsplit("/", 1)[0] + "/",
    }
    if cookie:
        hdr["Cookie"] = cookie
    with urllib.request.urlopen(urllib.request.Request(url, headers=hdr), timeout=40) as r:
        raw = r.read()
        print("HTTP %d  %d 字节  %s" % (r.status, len(raw), r.geturl()))
    if raw[:2] == b"\x1f\x8b":
        raw = gzip.decompress(raw)
    elif raw[:1] == b"\x78":
        raw = zlib.decompress(raw)
    return raw.decode("utf-8", "replace")


def main():
    number = (sys.argv[1] if len(sys.argv) > 1 else "SSIS-001").strip()
    with io.open(os.path.join(ROOT, "config.json"), encoding="utf-8") as f:
        cfg = json.load(f)
    base = (cfg.get("javbus_url") or "https://www.javbus.com").rstrip("/")
    html = fetch("%s/%s" % (base, number), cfg.get("javbus_cookie") or "")

    marker = 'id="sample-waterfall"'
    i = html.find(marker)
    if i < 0:
        raise SystemExit("页面里没有 %s —— 可能是被重定向到了验证页" % marker)
    frag = slice_element(html, html.rfind("<", 0, i))

    os.makedirs(OUT, exist_ok=True)
    name = "detail_%s_samples.html" % number.lower().replace("-", "")
    path = os.path.join(OUT, name)
    with io.open(path, "w", encoding="utf-8", newline="") as f:
        f.write(frag)
    n = len(re.findall(r"/pics/sample/[^\"']+", frag))
    print("写出 %-34s %6d 字节  样例图 %d 张" % (name, len(frag.encode("utf-8")), n))


if __name__ == "__main__":
    main()
