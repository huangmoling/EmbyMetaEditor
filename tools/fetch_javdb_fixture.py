#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""抓一次 javdb 真实页面，**逐字节**截出搜索结果与磁力区块，写进 testdata/javdb/。

为什么不用手写夹具：javdb 的磁力列表长这样 ——

    <div class="item odd" data-rank="0" data-size="6480" data-files="1" data-date="20231118">
      <div class="magnet-name">
        <a href="magnet:?xt=urn:btih:…&amp;dn=…">
          <span class="name">SSIS-001-UC.torrent.无码破解</span>
          <span class="meta">6.33GB, 1個文件</span>
        </a>
      </div>
      <div class="date"><span class="time">2023-11-18</span></div>
    </div>

体积既有 `data-size`（MB 整数）又在 `span.meta` 里（"6.33GB, 1個文件"），
日期既有 `data-date`（20231118）又在 `span.time` 里。**哪个才是准的、单位是什么**，
只有真页面能回答 —— 手写夹具必然把「6.33GB」当成体积的权威来源，
而实际排序依据是 data-size。所以夹具一律从真实响应里截，一个字节都不改。

javdb 有 Cloudflare 与 18 岁确认，但匿名请求目前够用；被拦时把浏览器 Cookie
填进 config.json 的 javdb_cookie。用法：

    python tools/fetch_javdb_fixture.py            # 默认 SSIS-001
    python tools/fetch_javdb_fixture.py IPX-777    # 换个番号
"""
import gzip
import io
import json
import os
import re
import sys
import urllib.parse
import urllib.request
import zlib

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "testdata", "javdb")

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
        "Referer": url.split("//")[0] + "//" + urllib.parse.urlsplit(url).netloc + "/",
    }
    if cookie:
        hdr["Cookie"] = cookie
    with urllib.request.urlopen(urllib.request.Request(url, headers=hdr), timeout=40) as r:
        raw = r.read()
        print("HTTP %d  %7d 字节  %s" % (r.status, len(raw), r.geturl()))
    if raw[:2] == b"\x1f\x8b":
        raw = gzip.decompress(raw)
    elif raw[:1] == b"\x78":
        raw = zlib.decompress(raw)
    return raw.decode("utf-8", "replace")


def write(name, frag):
    os.makedirs(OUT, exist_ok=True)
    path = os.path.join(OUT, name)
    with io.open(path, "w", encoding="utf-8", newline="") as f:
        f.write(frag)
    print("写出 %-38s %7d 字节" % (name, len(frag.encode("utf-8"))))


def main():
    number = (sys.argv[1] if len(sys.argv) > 1 else "SSIS-001").strip()
    slug = number.lower().replace("-", "").replace("_", "")
    with io.open(os.path.join(ROOT, "config.json"), encoding="utf-8") as f:
        cfg = json.load(f)
    base = (cfg.get("javdb_url") or "https://javdb.com").rstrip("/")
    cookie = cfg.get("javdb_cookie") or ""

    search = fetch("%s/search?f=all&q=%s" % (base, urllib.parse.quote(number)), cookie)
    marker = 'class="movie-list'
    i = search.find(marker)
    if i < 0:
        raise SystemExit("搜索页里没有 %s —— 可能被 Cloudflare 拦了（看看 config.json 的 javdb_cookie）" % marker)
    frag = slice_element(search, search.rfind("<", 0, i))
    write("search_%s.html" % slug, frag)
    print("   搜索结果 %d 条" % len(re.findall(r'class="box"', frag)))

    # 找精确匹配的那一条（番号写在 div.video-title 的 <strong> 里）
    want = re.sub(r"[^a-z0-9]", "", number.lower())
    hit = None
    for m in re.finditer(r'<a href="(/v/[^"]+)" class="box"[^>]*>(.*?)</a>', frag, re.S):
        s = re.search(r"<strong>([^<]*)</strong>", m.group(2))
        got = re.sub(r"[^a-z0-9]", "", (s.group(1) if s else "").lower())
        if got == want:
            hit = m.group(1)
            break
    if not hit:
        raise SystemExit("搜索结果里没有和 %s 精确匹配的条目，换个番号试试" % number)
    print("   精确匹配：%s" % hit)

    detail = fetch(base + hit, cookie)
    j = detail.find('id="magnets-content"')
    if j < 0:
        raise SystemExit("详情页里没有 #magnets-content —— 这个番号可能没有磁力，或页面结构变了")
    dfrag = slice_element(detail, detail.rfind("<", 0, j))
    write("detail_%s_magnets.html" % slug, dfrag)
    print("   磁力 %d 条，含 data-size %d 个"
          % (len(re.findall(r'href="magnet:', dfrag)), len(re.findall(r"data-size=", dfrag))))


if __name__ == "__main__":
    main()
